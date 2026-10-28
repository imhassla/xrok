package cmd

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"xrok/cmd/config"
	"xrok/cmd/transport"
)

func masqueFixture(t *testing.T) (string, *tls.Config) {
	t.Helper()
	oldOn, oldPrivate, oldAuth, oldToken, oldPolicy, oldCA := serverMASQUE, serverAllowPrivateEgress, serverAuthEnabled, serverApiToken, serverClientIdentityPolicy, serverClientCA
	serverMASQUE = true
	serverAllowPrivateEgress = true
	serverAuthEnabled = true
	serverApiToken = "masque-secret"
	serverClientIdentityPolicy = newControlIdentityPolicy(nil)
	serverClientCA = ""
	t.Cleanup(func() {
		serverMASQUE, serverAllowPrivateEgress, serverAuthEnabled, serverApiToken, serverClientIdentityPolicy, serverClientCA = oldOn, oldPrivate, oldAuth, oldToken, oldPolicy, oldCA
	})
	certs := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(certs.Close)
	s := newHTTP3Services(certs.TLS)
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = s.server.Serve(udp) }()
	t.Cleanup(func() {
		s.Close()
		udp.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("HTTP/3 service did not stop")
		}
	})
	return "https://" + udp.LocalAddr().String(), certs.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
}

func udpEchoFixture(t *testing.T) string {
	t.Helper()
	ln, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		b := make([]byte, 65536)
		for {
			n, peer, err := ln.ReadFrom(b)
			if err != nil {
				return
			}
			_, _ = ln.WriteTo(b[:n], peer)
		}
	}()
	return ln.LocalAddr().String()
}

func TestMASQUEUDPEchoDeadlinesAndNoDirectFallback(t *testing.T) {
	proxy, cfg := masqueFixture(t)
	target := udpEchoFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, err := transport.DialMASQUE(ctx, transport.MASQUEOptions{ProxyURL: proxy, Target: target, Token: "masque-secret", TLSConfig: cfg, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = c.SetReadDeadline(time.Now().Add(40 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 1500)
	if _, _, err = c.ReadFrom(b); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("read deadline: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	for _, payload := range [][]byte{[]byte("udp-owned-echo"), bytes.Repeat([]byte{0xa5}, 1200)} {
		if _, err = c.WriteTo(payload, c.RemoteAddr()); err != nil {
			t.Fatal(err)
		}
		n, _, err := c.ReadFrom(b)
		if err != nil || !bytes.Equal(b[:n], payload) {
			t.Fatalf("UDP integrity: %v n=%d", err, n)
		}
	}
	if _, err = c.WriteTo([]byte("no"), &net.UDPAddr{IP: net.IPv4(8, 8, 8, 8), Port: 53}); err == nil {
		t.Fatal("flow allowed changing target")
	}
	_ = c.SetWriteDeadline(time.Now().Add(-time.Second))
	if _, err = c.WriteTo([]byte("no"), c.RemoteAddr()); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Time{})
	blocked := make(chan error, 1)
	go func() { _, _, err := c.ReadFrom(b); blocked <- err }()
	cancel()
	select {
	case err := <-blocked:
		if err == nil {
			t.Fatal("closed read succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not unblock UDP read")
	}
}

func TestMASQUERejectsCredentialsTLSAndPrivateEgress(t *testing.T) {
	proxy, cfg := masqueFixture(t)
	target := udpEchoFixture(t)
	for _, token := range []string{"", "wrong"} {
		if conn, err := transport.DialMASQUE(context.Background(), transport.MASQUEOptions{ProxyURL: proxy, Target: target, Token: token, TLSConfig: cfg, Timeout: time.Second}); err == nil {
			conn.Close()
			t.Fatal("invalid token accepted")
		}
	}
	if conn, err := transport.DialMASQUE(context.Background(), transport.MASQUEOptions{ProxyURL: proxy, Target: target, Token: "masque-secret", Timeout: time.Second}); err == nil {
		conn.Close()
		t.Fatal("untrusted proxy TLS accepted")
	}
	serverAllowPrivateEgress = false
	if conn, err := transport.DialMASQUE(context.Background(), transport.MASQUEOptions{ProxyURL: proxy, Target: target, Token: "masque-secret", TLSConfig: cfg, Timeout: time.Second}); err == nil {
		conn.Close()
		t.Fatal("loopback egress accepted")
	}
}

func TestMASQUEScopedCredentialsAndAnonymousServerPolicy(t *testing.T) {
	proxy, cfg := masqueFixture(t)
	target := udpEchoFixture(t)
	serverClientIdentityPolicy = newControlIdentityPolicy([]config.ClientIdentityConfig{{ID: "client-only", Token: "client-token", Scopes: []string{"client"}}, {ID: "udp", Token: "udp-token", Scopes: []string{"proxy:udp"}}})
	if c, err := transport.DialMASQUE(context.Background(), transport.MASQUEOptions{ProxyURL: proxy, Target: target, Token: "client-token", TLSConfig: cfg, Timeout: time.Second}); err == nil {
		c.Close()
		t.Fatal("client-only credential gained UDP proxy scope")
	}
	c, err := transport.DialMASQUE(context.Background(), transport.MASQUEOptions{ProxyURL: proxy, Target: target, Token: "udp-token", TLSConfig: cfg, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	serverClientIdentityPolicy = newControlIdentityPolicy(nil)
	serverApiToken = ""
	serverAuthEnabled = false
	if c, err := transport.DialMASQUE(context.Background(), transport.MASQUEOptions{ProxyURL: proxy, Target: target, Token: "anything", TLSConfig: cfg, Timeout: time.Second}); err == nil {
		c.Close()
		t.Fatal("anonymous server became an open UDP proxy")
	}
}

func TestMASQUELocalMultipleClientsAndShutdown(t *testing.T) {
	proxy, cfg := masqueFixture(t)
	target := udpEchoFixture(t)
	ln, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- serveMASQUELocal(ctx, ln, transport.MASQUEOptions{ProxyURL: proxy, Target: target, Token: "masque-secret", TLSConfig: cfg, Timeout: time.Second})
	}()
	for _, body := range []string{"client-one", "client-two"} {
		c, err := net.DialUDP("udp", nil, ln.LocalAddr().(*net.UDPAddr))
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err = c.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
		b := make([]byte, 64)
		n, err := c.Read(b)
		c.Close()
		if err != nil || string(b[:n]) != body {
			t.Fatalf("local UDP forwarding: %v %q", err, b[:n])
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("local UDP forwarder did not stop")
	}
}

func TestQUICThroughMASQUE(t *testing.T) {
	proxy, cfg := masqueFixture(t)
	certs := httptest.NewTLSServer(http.NotFoundHandler())
	defer certs.Close()
	serverTLS := certs.TLS.Clone()
	serverTLS.NextProtos = []string{"inner-quic"}
	ln, err := quic.ListenAddr("127.0.0.1:0", serverTLS, &quic.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		c, err := ln.Accept(ctx)
		if err != nil {
			return
		}
		defer c.CloseWithError(0, "")
		s, err := c.AcceptStream(ctx)
		if err != nil {
			return
		}
		_, _ = io.Copy(s, s)
		_ = s.Close()
		<-c.Context().Done()
	}()
	pc, err := transport.DialMASQUE(ctx, transport.MASQUEOptions{ProxyURL: proxy, Target: ln.Addr().String(), Token: "masque-secret", TLSConfig: cfg, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	innerTLS := certs.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	innerTLS.NextProtos = []string{"inner-quic"}
	innerTLS.ServerName = "127.0.0.1"
	tr := &quic.Transport{Conn: pc}
	defer tr.Close()
	conn, err := tr.Dial(ctx, pc.RemoteAddr(), innerTLS, &quic.Config{InitialPacketSize: 1200, DisablePathMTUDiscovery: true})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "")
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("inner"), 1000)
	if _, err = stream.Write(payload); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, len(payload))
	if _, err = io.ReadFull(stream, b); err != nil || !bytes.Equal(b, payload) {
		t.Fatalf("inner QUIC integrity: %v", err)
	}
}
