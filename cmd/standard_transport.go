package cmd

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
	"golang.org/x/net/http2"
	"xrok/cmd/transport"
)

const tunnelProtocol = "xrok.v2"

type streamCarrier struct {
	reader  io.ReadCloser
	writer  io.Writer
	closeFn func()
	once    sync.Once
	closed  atomic.Bool
	writeMu sync.Mutex
	remote  net.Addr
}

func (c *streamCarrier) Read(p []byte) (int, error) { return c.reader.Read(p) }
func (c *streamCarrier) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed.Load() {
		return 0, net.ErrClosed
	}
	return c.writer.Write(p)
}
func (c *streamCarrier) Close() error {
	c.once.Do(func() {
		c.closed.Store(true)
		c.closeFn() // Interrupt blocked I/O before joining an in-flight write.
		c.writeMu.Lock()
		c.writeMu.Unlock()
	})
	return nil
}
func (c *streamCarrier) LocalAddr() net.Addr { return &net.TCPAddr{} }
func (c *streamCarrier) RemoteAddr() net.Addr {
	if c.remote != nil {
		return c.remote
	}
	return &net.TCPAddr{}
}
func (c *streamCarrier) SetDeadline(time.Time) error      { return errors.ErrUnsupported }
func (c *streamCarrier) SetReadDeadline(time.Time) error  { return errors.ErrUnsupported }
func (c *streamCarrier) SetWriteDeadline(time.Time) error { return errors.ErrUnsupported }

type flushWriter struct{ w http.ResponseWriter }

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err == nil {
		err = http.NewResponseController(f.w).Flush()
	}
	return n, err
}

func standardTunnelClient(w http.ResponseWriter, r *http.Request) (*serverClientInfo, string, bool) {
	if r.Header.Get("X-Xrok-Tunnel-Version") != "2" {
		http.Error(w, "tunnel version 2 required", 400)
		return nil, "", false
	}
	id := r.URL.Query().Get("clientID")
	c, ok := serverDepsFromRequest(r).Clients.Get(id)
	if !ok || !c.validSessionToken(r.Header.Get("X-Xrok-Session")) {
		http.Error(w, "unauthorized tunnel", 401)
		return nil, "", false
	}
	return c, id, true
}

func serverStandardTunnelHandler(w http.ResponseWriter, r *http.Request) {
	c, id, ok := standardTunnelClient(w, r)
	if !ok {
		return
	}
	if r.URL.Path == "/v2/tunnel/ws" {
		if !containsTransport(websocket.Subprotocols(r), tunnelProtocol) {
			http.Error(w, "xrok.v2 subprotocol required", 400)
			return
		}
		u := websocket.Upgrader{Subprotocols: []string{tunnelProtocol}, ReadBufferSize: 32 << 10, WriteBufferSize: 32 << 10}
		ws, err := u.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		srvWsUpgrades.Inc()
		carrier := transport.NewWebSocketConn(ws)
		defer carrier.Close()
		serveServerMuxCarrier(carrier, c, id)
		return
	}
	if r.Method != "POST" || r.ProtoMajor != 2 {
		http.Error(w, "HTTP/2 POST required", 400)
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Xrok-Tunnel-Version", "2")
	w.WriteHeader(200)
	if err := rc.Flush(); err != nil {
		return
	}
	carrier := &streamCarrier{reader: r.Body, writer: flushWriter{w}, closeFn: func() { _ = rc.SetWriteDeadline(time.Now()); _ = r.Body.Close() }}
	defer carrier.Close()
	serveServerMuxCarrier(carrier, c, id)
}

func containsTransport(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func (s *clientState) dialStandardCarrier(ctx context.Context, mode string) (net.Conn, error) {
	if !containsTransport(s.SupportedTransports, mode) {
		return nil, fmt.Errorf("server does not advertise %s", mode)
	}
	if !s.UseTLS {
		return nil, fmt.Errorf("standard tunnel carriers require TLS")
	}
	port := s.ServerPort
	if port == 0 {
		port = clientRp
	}
	u := url.URL{Scheme: "https", Host: serverAuthority(s.ServerAddr, port), Path: "/v2/tunnel", RawQuery: url.Values{"clientID": {s.ClientID}}.Encode()}
	h := make(http.Header)
	h.Set("X-Xrok-Session", s.SessionToken)
	h.Set("X-Xrok-Tunnel-Version", "2")
	if mode == "wss" {
		u.Scheme = "wss"
		u.Path += "/ws"
		d := s.routeWebSocketDialer()
		d.Subprotocols = []string{tunnelProtocol}
		ws, resp, err := d.DialContext(ctx, u.String(), h)
		if err != nil {
			if resp != nil && resp.Body != nil {
				resp.Body.Close()
			}
			return nil, err
		}
		if ws.Subprotocol() != tunnelProtocol {
			ws.Close()
			return nil, fmt.Errorf("server did not negotiate xrok.v2")
		}
		return transport.NewWebSocketConn(ws), nil
	}
	if mode != "http2" {
		return nil, fmt.Errorf("unsupported standard transport %s", mode)
	}
	lifetime, cancel := context.WithCancel(ctx)
	tr := &http2.Transport{DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
		// Keep h2 ALPN local to the HTTP/2 carrier.
		cfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if s.TLSConfig != nil {
			cfg = s.TLSConfig.Clone()
		}
		cfg.NextProtos = []string{"h2"}
		route := &clientState{TLSConfig: cfg, HTTPProxy: s.HTTPProxy, ConnectTimeout: s.ConnectTimeout, ProxySession: s.ProxySession, ProxySessionHeader: s.ProxySessionHeader, ProxyTLSConfig: s.ProxyTLSConfig}
		conn, err := route.dialRouteTLS(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		if conn.(*tls.Conn).ConnectionState().NegotiatedProtocol != "h2" {
			conn.Close()
			return nil, fmt.Errorf("server did not negotiate h2")
		}
		return conn, nil
	}}
	pr, pw := io.Pipe()
	request, err := http.NewRequestWithContext(lifetime, "POST", u.String(), pr)
	if err != nil {
		cancel()
		return nil, err
	}
	request.Header = h
	request.Header.Set("Content-Type", "application/octet-stream")
	timer := time.AfterFunc(s.connectionBudget(), cancel)
	response, err := tr.RoundTrip(request)
	timer.Stop()
	if err != nil {
		cancel()
		pr.Close()
		pw.Close()
		tr.CloseIdleConnections()
		return nil, err
	}
	if response.StatusCode != 200 || response.ProtoMajor != 2 || response.Header.Get("X-Xrok-Tunnel-Version") != "2" {
		response.Body.Close()
		cancel()
		pr.Close()
		pw.Close()
		tr.CloseIdleConnections()
		return nil, fmt.Errorf("HTTP/2 tunnel refused (status %d)", response.StatusCode)
	}
	return &streamCarrier{reader: response.Body, writer: pw, closeFn: func() { cancel(); _ = pw.Close(); _ = pr.Close(); _ = response.Body.Close(); tr.CloseIdleConnections() }}, nil
}

func runClientMuxCarrier(ctx context.Context, state *clientState, carrier net.Conn) {
	defer carrier.Close()
	cfg := yamux.DefaultConfig()
	cfg.AcceptBacklog = 256
	cfg.EnableKeepAlive = true
	cfg.KeepAliveInterval = 30 * time.Second
	session, err := yamux.Client(carrier, cfg)
	if err != nil {
		logCarrierError(state, err)
		return
	}
	defer session.Close()
	clientStateMutex.Lock()
	state.MuxSession = session
	clientStateMutex.Unlock()
	stop := context.AfterFunc(ctx, func() { _ = session.Close() })
	defer stop()
	defer func() {
		clientStateMutex.Lock()
		if state.MuxSession == session {
			state.MuxSession = nil
		}
		clientStateMutex.Unlock()
	}()
	for {
		stream, err := session.Accept()
		if err != nil {
			return
		}
		go handleMuxStream(ctx, stream, state)
	}
}

func logCarrierError(state *clientState, err error) {
	if clientLog != nil {
		clientLog.Warn("Tunnel carrier: %s", state.redactRouteSecrets(err.Error()))
	}
}
