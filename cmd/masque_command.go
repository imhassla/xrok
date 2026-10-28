package cmd

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"xrok/cmd/config"
	"xrok/cmd/transport"
)

// RunMASQUE exposes a bounded local UDP forwarder through a fixed-target
// authenticated HTTP/3 CONNECT-UDP proxy. It is independent of TCP registration.
func RunMASQUE(args []string) error {
	fs := flag.NewFlagSet("masque", flag.ContinueOnError)
	profile := fs.String("config", defaultClientConfigPath(), "Existing xrok client profile for credentials and TLS")
	proxy := fs.String("proxy", "", "HTTPS MASQUE proxy authority or URI template (default configured xrok server)")
	target := fs.String("target", "", "Fixed UDP target host:port")
	listen := fs.String("listen", "127.0.0.1:5353", "Local UDP listen address")
	token := fs.String("token", os.Getenv("XROK_TOKEN"), "Proxy credential (prefer a protected profile or XROK_TOKEN)")
	ca := fs.String("ca-cert", "", "Proxy CA bundle")
	cert := fs.String("client-cert", "", "Proxy mTLS client certificate")
	key := fs.String("client-key", "", "Proxy mTLS client key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *profile != "" {
		cfg, err := config.LoadClientConfig(*profile)
		if err != nil {
			return err
		}
		if *token == "" {
			*token = cfg.Token
		}
		if *ca == "" {
			*ca = cfg.CACert
		}
		if *cert == "" {
			*cert = cfg.ClientCert
		}
		if *key == "" {
			*key = cfg.ClientKey
		}
		if *proxy == "" {
			host := cfg.Server
			port := cfg.ServerPort
			if h, p, err := net.SplitHostPort(host); err == nil {
				host = h
				if port == 0 {
					port, _ = strconv.Atoi(p)
				}
			}
			if port == 0 {
				port = 443
			}
			*proxy = "https://" + net.JoinHostPort(host, strconv.Itoa(port))
		}
	}
	if _, _, err := net.SplitHostPort(*target); err != nil {
		return fmt.Errorf("target must be host:port")
	}
	if *proxy == "" || *token == "" {
		return fmt.Errorf("a MASQUE proxy and authenticated token are required")
	}
	roots, err := loadRootCAs(*ca)
	if err != nil {
		return err
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots}
	if *cert != "" || *key != "" {
		c, err := tls.LoadX509KeyPair(*cert, *key)
		if err != nil {
			return err
		}
		cfg.Certificates = []tls.Certificate{c}
	}
	addr, err := net.ResolveUDPAddr("udp", *listen)
	if err != nil {
		return err
	}
	ln, err := net.ListenUDP("udp", addr)
	if err != nil {
		return err
	}
	defer ln.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serveMASQUELocal(ctx, ln, transport.MASQUEOptions{ProxyURL: *proxy, Target: *target, Token: *token, TLSConfig: cfg, Timeout: 15 * time.Second})
}

type localUDPFlow struct {
	packets chan []byte
	last    atomic.Int64
}

func serveMASQUELocal(ctx context.Context, ln *net.UDPConn, options transport.MASQUEOptions) error {
	var mu sync.Mutex
	flows := make(map[string]*localUDPFlow)
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	closeListener := context.AfterFunc(ctx, func() { ln.Close() })
	defer closeListener()
	defer func() { cancel(); wg.Wait() }()
	buf := make([]byte, 65536)
	for {
		n, peer, err := ln.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		mu.Lock()
		flow := flows[peer.String()]
		if flow == nil && len(flows) < 32 {
			flow = &localUDPFlow{packets: make(chan []byte, 16)}
			flow.last.Store(time.Now().UnixNano())
			flows[peer.String()] = flow
			wg.Add(1)
			go func(flow *localUDPFlow, peer *net.UDPAddr) {
				defer wg.Done()
				defer func() { mu.Lock(); delete(flows, peer.String()); mu.Unlock() }()
				conn, err := transport.DialMASQUE(ctx, options)
				if err != nil {
					log.Printf("MASQUE flow failed: %s", strings.ReplaceAll(err.Error(), options.Token, "[redacted]"))
					return
				}
				readDone := make(chan struct{})
				defer func() { conn.Close(); <-readDone }()
				go func() {
					defer close(readDone)
					b := make([]byte, 65536)
					for {
						n, _, err := conn.ReadFrom(b)
						if err != nil {
							return
						}
						if _, err = ln.WriteToUDP(b[:n], peer); err != nil {
							return
						}
					}
				}()
				ticker := time.NewTicker(time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-readDone:
						return
					case <-ticker.C:
						if time.Since(time.Unix(0, flow.last.Load())) > 30*time.Second {
							return
						}
					case payload := <-flow.packets:
						if _, err := conn.WriteTo(payload, conn.RemoteAddr()); err != nil {
							return
						}
					}
				}
			}(flow, peer)
		}
		if flow != nil {
			flow.last.Store(time.Now().UnixNano())
			select {
			case flow.packets <- append([]byte(nil), buf[:n]...):
			default:
			}
		}
		mu.Unlock()
	}
}
