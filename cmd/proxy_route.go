package cmd

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	xproxy "golang.org/x/net/proxy"
	"xrok/cmd/config"
)

type routeError struct {
	Stage string
	Err   error
}

func (e *routeError) Error() string { return e.Stage + ": " + e.Err.Error() }
func (e *routeError) Unwrap() error { return e.Err }

func (s *clientState) connectionBudget() time.Duration {
	if s.ConnectTimeout > 0 {
		return s.ConnectTimeout
	}
	return 30 * time.Second
}

func (s *clientState) proxyHeaders() http.Header {
	h := make(http.Header)
	if s.ProxySession != "" {
		name := s.ProxySessionHeader
		if name == "" {
			name = "X-Proxy-Session"
		}
		h.Set(name, s.ProxySession)
	}
	return h
}

func redactedProxy(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "[invalid proxy]"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func parseProxyRoute(raw string) (*url.URL, error) {
	if err := config.ValidateProxySettings(raw, "", "", 0, false, false); err != nil {
		return nil, err
	}
	if raw == "" {
		return nil, nil
	}
	u, _ := url.Parse(raw)
	if u.Port() == "" {
		port := "80"
		if u.Scheme == "https" {
			port = "443"
		}
		if strings.HasPrefix(u.Scheme, "socks5") {
			port = "1080"
		}
		u.Host = net.JoinHostPort(u.Hostname(), port)
	}
	return u, nil
}

func handshakeCancellation(ctx context.Context, conn net.Conn) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Now()); close(done) })
	var once sync.Once
	return func() {
		once.Do(func() {
			if !stop() {
				<-done
			}
		})
	}
}

type bufferedRouteConn struct {
	net.Conn
	r io.Reader
}

func (c *bufferedRouteConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// dialRoute always returns a plain target byte stream, even when TLS protects the
// proxy hop. The caller's HTTP/WebSocket transport supplies target TLS separately.
func (s *clientState) dialRoute(parent context.Context, network, target string) (conn net.Conn, err error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("proxy route requires TCP")
	}
	host, portText, e := net.SplitHostPort(target)
	port, e2 := strconv.Atoi(portText)
	if e != nil || e2 != nil || host == "" || port < 1 || port > 65535 || strings.ContainsAny(host, "\r\n\t ") {
		return nil, fmt.Errorf("invalid target authority")
	}
	u, e := parseProxyRoute(s.HTTPProxy)
	if e != nil {
		return nil, e
	}
	ctx, cancel := context.WithTimeout(parent, s.connectionBudget())
	defer cancel()
	dialer := &net.Dialer{KeepAlive: 30 * time.Second}
	if u == nil {
		c, e := dialer.DialContext(ctx, network, target)
		if e != nil {
			return nil, &routeError{"dial", e}
		}
		return c, nil
	}
	if strings.HasPrefix(u.Scheme, "socks5") {
		var auth *xproxy.Auth
		if u.User != nil {
			password, _ := u.User.Password()
			auth = &xproxy.Auth{User: u.User.Username(), Password: password}
		}
		socks, e := xproxy.SOCKS5(network, u.Host, auth, dialer)
		if e != nil {
			return nil, &routeError{"proxy_handshake", e}
		}
		targets := []string{target}
		if u.Scheme == "socks5" && net.ParseIP(host) == nil {
			ips, e := net.DefaultResolver.LookupIPAddr(ctx, host)
			if e != nil {
				return nil, &routeError{"target_dns", e}
			}
			targets = nil
			for _, ip := range ips {
				if network == "tcp4" && ip.IP.To4() == nil {
					continue
				}
				if network == "tcp6" && ip.IP.To4() != nil {
					continue
				}
				targets = append(targets, net.JoinHostPort(ip.String(), portText))
			}
			if len(targets) == 0 {
				return nil, &routeError{"target_dns", fmt.Errorf("no target addresses")}
			}
		}
		var last error
		for _, address := range targets {
			c, e := socks.(xproxy.ContextDialer).DialContext(ctx, network, address)
			if e == nil {
				return c, nil
			}
			last = e
			if ctx.Err() != nil {
				break
			}
		}
		return nil, &routeError{"proxy_handshake", last}
	}
	conn, e = dialer.DialContext(ctx, network, u.Host)
	if e != nil {
		return nil, &routeError{"proxy_dial", e}
	}
	rawConn := conn
	defer func() {
		if err != nil {
			rawConn.Close()
		}
	}()
	release := handshakeCancellation(ctx, conn)
	defer release()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	if u.Scheme == "https" {
		cfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if s.ProxyTLSConfig != nil {
			cfg = s.ProxyTLSConfig.Clone()
		}
		cfg.ServerName = u.Hostname()
		tc := tls.Client(conn, cfg)
		if e = tc.HandshakeContext(ctx); e != nil {
			return nil, &routeError{"proxy_tls", e}
		}
		conn = tc
	}
	headers := s.proxyHeaders()
	if u.User != nil {
		password, _ := u.User.Password()
		headers.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(u.User.Username()+":"+password)))
	}
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: target}, Host: target, Header: headers}
	if e = req.Write(conn); e != nil {
		return nil, &routeError{"proxy_connect", e}
	}
	// Cap untrusted CONNECT headers and retain any pipelined target bytes. Once
	// this limited reader is exhausted, MultiReader resumes on the raw connection.
	br := bufio.NewReader(io.LimitReader(conn, 64<<10))
	resp, e := http.ReadResponse(br, req)
	if e != nil {
		var networkErr net.Error
		if !errors.As(e, &networkErr) && !errors.Is(e, io.EOF) {
			e = fmt.Errorf("invalid CONNECT response")
		}
		return nil, &routeError{"proxy_connect", e}
	}
	if resp.StatusCode != http.StatusOK {
		stage := "proxy_connect"
		if resp.StatusCode == 407 {
			stage = "proxy_auth"
		}
		return nil, &routeError{stage, fmt.Errorf("CONNECT status %d", resp.StatusCode)}
	}
	// A CONNECT error body is never drained and a successful tunnel body is never
	// closed here. Both can block or consume bytes that belong to the target.
	release()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	conn.SetDeadline(time.Time{})
	return &bufferedRouteConn{Conn: conn, r: io.MultiReader(br, conn)}, nil
}

func (s *clientState) routeTransport() *http.Transport {
	return &http.Transport{
		DialContext:         s.dialRoute,
		DialTLSContext:      s.dialRouteTLS,
		TLSClientConfig:     s.TLSConfig,
		TLSHandshakeTimeout: s.connectionBudget(),
		MaxIdleConns:        1000, MaxIdleConnsPerHost: 1000, IdleConnTimeout: 90 * time.Second,
	}
}

func (s *clientState) routeWebSocketDialer() *websocket.Dialer {
	return &websocket.Dialer{NetDialContext: s.dialRoute, NetDialTLSContext: s.dialRouteTLS, TLSClientConfig: s.TLSConfig, HandshakeTimeout: s.connectionBudget(), EnableCompression: s.Compress}
}

func waitRouteRetry(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (s *clientState) dialRouteTLS(parent context.Context, network, target string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(parent, s.connectionBudget())
	defer cancel()
	conn, err := s.dialRoute(ctx, network, target)
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if s.TLSConfig != nil {
		cfg = s.TLSConfig.Clone()
	}
	if cfg.ServerName == "" {
		host, _, err := net.SplitHostPort(target)
		if err != nil {
			conn.Close()
			return nil, err
		}
		cfg.ServerName = host
	}
	tc := tls.Client(conn, cfg)
	if err := tc.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, &routeError{"target_tls", err}
	}
	return tc, nil
}

func (s *clientState) dialWebSocket(ctx context.Context, rawURL string, headers http.Header) (*websocket.Conn, *http.Response, error) {
	conn, resp, err := s.routeWebSocketDialer().DialContext(ctx, rawURL, headers)
	if err != nil {
		var staged *routeError
		if !errors.As(err, &staged) {
			stage := "upgrade"
			if ctx.Err() != nil {
				stage = "canceled"
			}
			err = &routeError{stage, err}
		}
	}
	return conn, resp, err
}

func (s *clientState) redactRouteSecrets(text string) string {
	if s.HTTPProxy != "" {
		text = strings.ReplaceAll(text, s.HTTPProxy, redactedProxy(s.HTTPProxy))
	}
	secrets := []string{s.APIToken, s.SessionToken, s.ProxySession}
	if u, e := url.Parse(s.HTTPProxy); e == nil && u.User != nil {
		password, _ := u.User.Password()
		secrets = append(secrets, u.User.Username(), password)
	}
	for _, secret := range secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[redacted]")
		}
	}
	return text
}

// raw mux and legacy data use the byte stream after the WebSocket HTTP upgrade.
