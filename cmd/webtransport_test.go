package cmd

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	webtransport "github.com/quic-go/webtransport-go"
)

type wtFixture struct {
	url     string
	tls     *tls.Config
	route   *TunnelRoute
	service *http3Services
}

func webTransportFixture(t *testing.T, datagrams bool) *wtFixture {
	return webTransportTLSFixture(t, datagrams, nil)
}

func webTransportTLSFixture(t *testing.T, datagrams bool, leaf *tls.Certificate) *wtFixture {
	t.Helper()
	old := serverWebTransport
	serverWebTransport = true
	t.Cleanup(func() { serverWebTransport = old })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tcp.Close() })
	go func() {
		for {
			c, err := tcp.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				b, err := io.ReadAll(c)
				if err != nil {
					return
				}
				_, _ = c.Write(append([]byte("reply:"), b...))
			}()
		}
	}()
	udp, err := net.ListenPacket("udp", tcp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { udp.Close() })
	go func() {
		b := make([]byte, 1500)
		for {
			n, peer, err := udp.ReadFrom(b)
			if err != nil {
				return
			}
			_, _ = udp.WriteTo(append([]byte("udp:"), b[:n]...), peer)
		}
	}()
	a, b := net.Pipe()
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	server, err := yamux.Server(a, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	peer, err := yamux.Client(b, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { peer.Close() })
	state := &clientState{TunnelMap: map[string]string{"app": tcp.Addr().String()}, Tunnels: []TunnelConfig{{Name: "app", Target: tcp.Addr().String(), WebTransport: true, Datagrams: datagrams}}}
	go func() {
		for {
			c, err := peer.Accept()
			if err != nil {
				return
			}
			go handleMuxStream(ctx, c, state)
		}
	}()
	info := &serverClientInfo{muxSession: server, tunnelMap: state.TunnelMap, ctx: ctx}
	deps := NewServerDeps("example.test", time.Now(), nil)
	deps.Clients.Put("owner", info)
	route := &TunnelRoute{ClientID: "owner", TunnelName: "app", WebTransport: true, Datagrams: datagrams, Capability: "cap-secret", CapabilityAudience: "browser", WebTransportOrigins: []string{"http://localhost:3000"}}
	// Numeric host keeps the test entirely local without DNS overrides.
	deps.Routes.Put("127", route)
	certs := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(certs.Close)
	serverTLS := certs.TLS.Clone()
	clientTLS := certs.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	if leaf != nil {
		serverTLS.Certificates = []tls.Certificate{*leaf}
		roots := x509.NewCertPool()
		cert, err := x509.ParseCertificate(leaf.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		roots.AddCert(cert)
		clientTLS = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}
	}
	s := newHTTP3Services(serverTLS)
	base := s.server.Handler
	s.server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base.ServeHTTP(w, r.WithContext(WithServerDeps(r.Context(), deps)))
	})
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = s.web.Serve(packet) }()
	t.Cleanup(func() {
		s.Close()
		packet.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("WebTransport server did not stop")
		}
	})
	return &wtFixture{url: "https://" + packet.LocalAddr().String() + "/.well-known/xrok/webtransport", tls: clientTLS, route: route, service: s}
}

func dialWebTransport(t *testing.T, f *wtFixture, headers http.Header, query string) (*webtransport.Session, *http.Response, error) {
	t.Helper()
	d := &webtransport.Dialer{TLSClientConfig: f.tls}
	t.Cleanup(func() { d.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	response, session, err := d.Dial(ctx, f.url+query, headers)
	return session, response, err
}

func TestWebTransportStreamsDatagramsHalfCloseAndCancellation(t *testing.T) {
	f := webTransportFixture(t, true)
	token := testJWT(f.route.Capability, time.Now().Add(time.Minute).Unix(), "browser")
	s, _, err := dialWebTransport(t, f, http.Header{"Origin": {"http://localhost:3000"}}, "?cap="+url.QueryEscape(token))
	if err != nil {
		t.Fatal(err)
	}
	defer s.CloseWithError(0, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := 0; i < 3; i++ {
		stream, err := s.OpenStreamSync(ctx)
		if err != nil {
			t.Fatal(err)
		}
		payload := bytes.Repeat([]byte(strconv.Itoa(i)), 100000)
		if _, err = stream.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err = stream.Close(); err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(stream)
		if err != nil || !bytes.Equal(body, append([]byte("reply:"), payload...)) {
			t.Fatalf("stream/half-close integrity: %v len=%d", err, len(body))
		}
	}
	if err = s.SendDatagram([]byte("owned")); err != nil {
		t.Fatal(err)
	}
	reply, err := s.ReceiveDatagram(ctx)
	if err != nil || string(reply) != "udp:owned" {
		t.Fatalf("visitor UDP: %v %q", err, reply)
	}
	stream, err := s.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = stream.Write([]byte("open"))
	s.CloseWithError(0, "")
	done := make(chan error, 1)
	go func() { _, err := stream.Read(make([]byte, 1)); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("closed session kept stream alive")
		}
	case <-time.After(time.Second):
		t.Fatal("session cancellation did not close stream")
	}
}

func TestWebTransportRejectsOriginCapabilitiesAndDisabledRoutes(t *testing.T) {
	for _, tc := range []struct {
		name, origin, query string
		disabled            bool
	}{{"no-capability", "http://localhost:3000", "", false}, {"expired", "http://localhost:3000", "?cap=" + testJWT("cap-secret", time.Now().Add(-time.Minute).Unix(), "browser"), false}, {"wrong-audience", "http://localhost:3000", "?cap=" + testJWT("cap-secret", time.Now().Add(time.Minute).Unix(), "other"), false}, {"origin", "https://evil.test", "?cap=" + testJWT("cap-secret", time.Now().Add(time.Minute).Unix(), "browser"), false}, {"disabled", "http://localhost:3000", "", true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := webTransportFixture(t, false)
			if tc.disabled {
				f.route.WebTransport = false
			}
			s, response, err := dialWebTransport(t, f, http.Header{"Origin": {tc.origin}}, tc.query)
			if err == nil {
				s.CloseWithError(0, "")
				t.Fatal("unauthorized WebTransport accepted")
			}
			want := http.StatusForbidden
			if tc.disabled {
				want = http.StatusNotFound
			}
			if response == nil || response.StatusCode != want {
				t.Fatalf("expected policy status %d, got response=%v err=%v", want, response, err)
			}
			if response != nil && response.Body != nil {
				response.Body.Close()
			}
		})
	}
}

func TestWebTransportDatagramsRequireExplicitOptIn(t *testing.T) {
	f := webTransportFixture(t, false)
	s, _, err := dialWebTransport(t, f, http.Header{"X-Xrok-Capability": {f.route.Capability}}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.CloseWithError(0, "")
	if err = s.SendDatagram([]byte("denied")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Context().Done():
	case <-time.After(2 * time.Second):
		t.Fatal("disabled UDP was silently accepted")
	}
}

func TestVisitorUDPRejectsUnconfiguredTarget(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleVisitorUDPStream(ctx, a, &clientState{TunnelMap: map[string]string{"app": "127.0.0.1:9"}})
	}()
	_, _ = b.Write(append([]byte{3}, []byte("app")...))
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("unconfigured UDP permission did not reject")
	}
}

func TestIngressOpenCancellationDoesNotWaitForStalledCarrier(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	s, err := yamux.Server(a, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	client := &serverClientInfo{muxSession: s}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	if conn, err := openClientTransportStream(ctx, client); !errors.Is(err, context.DeadlineExceeded) {
		if conn != nil {
			conn.Close()
		}
		t.Fatalf("stalled open ignored context: %v", err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("open cancellation waited on carrier")
	}
	s.Close()
	b.Close()
	deadline := time.Now().Add(time.Second)
	for len(client.ingressOpening) > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(client.ingressOpening) != 0 {
		t.Fatal("stalled open worker did not drain")
	}
}

func TestWebTransportQueryRequiresJWTAndPreservesBasicAuth(t *testing.T) {
	route := &TunnelRoute{Capability: "private-signing-secret", CapabilityAudience: "browser", BasicAuth: "u:p"}
	r := httptest.NewRequest("CONNECT", "https://app.example/.well-known/xrok/webtransport?cap=private-signing-secret", nil)
	r.SetBasicAuth("u", "p")
	if _, ok := webTransportCapability(r, route); ok {
		t.Fatal("long-lived signing secret accepted in query")
	}
	token := testJWT(route.Capability, time.Now().Add(time.Minute).Unix(), "browser")
	r.URL.RawQuery = "cap=" + url.QueryEscape(token)
	if _, ok := webTransportCapability(r, route); !ok {
		t.Fatal("valid JWT plus Basic Auth rejected")
	}
	r.SetBasicAuth("u", "wrong")
	if _, ok := webTransportCapability(r, route); ok {
		t.Fatal("JWT bypassed Basic Auth")
	}
}
