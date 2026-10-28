package cmd

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/quic-go/quic-go"
	"golang.org/x/net/http2"
)

func configureIngressHTTP2(s *http.Server) error {
	if s.TLSConfig == nil {
		s.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	s.TLSConfig.NextProtos = []string{"h2", "http/1.1"}
	return http2.ConfigureServer(s, &http2.Server{MaxConcurrentStreams: 256, MaxReadFrameSize: 1 << 20})
}

type ingressQUICStream struct {
	*quic.Stream
	local, remote net.Addr
}

func (s *ingressQUICStream) LocalAddr() net.Addr  { return s.local }
func (s *ingressQUICStream) RemoteAddr() net.Addr { return s.remote }
func (s *ingressQUICStream) CloseWrite() error    { return s.Stream.Close() }
func (s *ingressQUICStream) Close() error         { s.CancelRead(0); return s.Stream.Close() }

// openIngressStream retains the existing named-stream wire protocol, so HTTP/2
// ingress also works with older clients and either established tunnel carrier.
func openClientTransportStream(ctx context.Context, client *serverClientInfo) (net.Conn, error) {
	client.muxSessionMutex.Lock()
	session := client.muxSession
	client.muxSessionMutex.Unlock()
	var conn net.Conn
	if session != nil && !session.IsClosed() {
		stream, err := openYamuxIngress(ctx, client, session)
		if err != nil {
			return nil, err
		}
		conn = stream
	} else {
		client.connMutex.Lock()
		qc := client.quicConn
		client.connMutex.Unlock()
		if qc == nil {
			return nil, fmt.Errorf("no multiplexed tunnel connection")
		}
		stream, err := qc.Connection().OpenStreamSync(ctx)
		if err != nil {
			return nil, err
		}
		conn = &ingressQUICStream{stream, qc.Connection().LocalAddr(), qc.Connection().RemoteAddr()}
	}
	return conn, nil
}

// yamux.Open has no context API. Bound in-flight opens and close a late result
// when its HTTP/2/WebTransport request was canceled, without killing other RPCs.
func openYamuxIngress(ctx context.Context, client *serverClientInfo, session *yamux.Session) (net.Conn, error) {
	client.ingressOpenMu.Lock()
	if client.ingressOpening == nil {
		client.ingressOpening = make(chan struct{}, 128)
	}
	slots := client.ingressOpening
	client.ingressOpenMu.Unlock()
	select {
	case slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	type result struct {
		conn net.Conn
		err  error
	}
	ready := make(chan result)
	go func() {
		defer func() { <-slots }()
		conn, err := session.Open()
		select {
		case ready <- result{conn, err}:
		case <-ctx.Done():
			if conn != nil {
				_ = conn.Close()
			}
		}
	}()
	select {
	case r := <-ready:
		if err := ctx.Err(); err != nil {
			if r.conn != nil {
				_ = r.conn.Close()
			}
			return nil, err
		}
		return r.conn, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func openIngressStream(ctx context.Context, client *serverClientInfo, name string) (net.Conn, error) {
	conn, err := openClientTransportStream(ctx, client)
	if err != nil {
		return nil, err
	}
	// Always emit a routing byte. Legacy unnamed clients consume a zero marker.
	if len(name) > 255 {
		_ = conn.Close()
		return nil, fmt.Errorf("tunnel name too long")
	}
	if len(client.tunnelMap) == 0 {
		name = ""
	}
	header := append([]byte{byte(len(name))}, []byte(name)...)
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err = conn.Write(header)
	_ = conn.SetWriteDeadline(time.Time{})
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func isGRPCRequest(r *http.Request) bool {
	ct := strings.TrimSpace(strings.Split(strings.ToLower(r.Header.Get("Content-Type")), ";")[0])
	return ct == "application/grpc" || strings.HasPrefix(ct, "application/grpc+")
}

func serveHTTP2Tunnel(w http.ResponseWriter, r *http.Request, client *serverClientInfo, name, basicAuth string, headers map[string]string) {
	if basicAuth != "" {
		u, p, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(u+":"+p), []byte(basicAuth)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="Protected"`)
			http.Error(w, "Unauthorized", 401)
			return
		}
	}
	client.SetLastActivity(time.Now())
	// Long-lived RPCs have their own context/deadline, not the listener's
	// generic five-minute socket deadline. HTTP/2 cancellation stays stream-local.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) { return openIngressStream(ctx, client, name) }
	var rt http.RoundTripper
	var closeIdle func()
	if isGRPCRequest(r) {
		t := &http2.Transport{AllowHTTP: true, DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return dial(ctx, network, addr)
		}}
		rt = t
		closeIdle = t.CloseIdleConnections
	} else {
		t := &http.Transport{Proxy: nil, DialContext: dial, DisableKeepAlives: true, MaxResponseHeaderBytes: 1 << 20}
		rt = t
		closeIdle = t.CloseIdleConnections
	}
	defer closeIdle()
	proxy := &httputil.ReverseProxy{Transport: rt, FlushInterval: -1, Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(&url.URL{Scheme: "http", Host: r.Host})
		pr.Out.Host = r.Host
		pr.SetXForwarded()
		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			ip = r.RemoteAddr
		}
		pr.Out.Header.Set("X-Real-IP", ip)
		for k, v := range headers {
			pr.Out.Header.Set(k, v)
		}
	}, ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
		if r.Context().Err() != nil {
			return
		}
		if isGRPCRequest(r) {
			w.Header().Set("Content-Type", "application/grpc")
			w.Header().Set("Trailer", "Grpc-Status, Grpc-Message")
			w.WriteHeader(200)
			w.Header().Set("Grpc-Status", "14")
			w.Header().Set("Grpc-Message", "tunnel unavailable")
			return
		}
		http.Error(w, "Tunnel unavailable", 502)
	}}
	proxy.ServeHTTP(w, r)
}
