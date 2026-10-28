package cmd

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	masque "github.com/quic-go/masque-go"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	webtransport "github.com/quic-go/webtransport-go"
	"github.com/yosida95/uritemplate/v3"
)

var serverMASQUE bool
var serverWebTransport bool
var serverHTTP3Slots = make(chan struct{}, 64)

type http3Services struct {
	web      *webtransport.Server
	sessions chan struct{}
	server   *http3.Server
	proxy    masque.Proxy
	flows    chan struct{}
}

func newHTTP3Services(cfg *tls.Config) *http3Services {
	cfg = cfg.Clone()
	cfg.NextProtos = []string{http3.NextProtoH3}
	s := &http3Services{flows: make(chan struct{}, 128)}
	s.server = &http3.Server{TLSConfig: cfg, EnableDatagrams: true, QUICConfig: &quic.Config{EnableDatagrams: true, InitialPacketSize: 1350, MaxIdleTimeout: 45 * time.Second}, MaxHeaderBytes: 16 << 10, Handler: http.HandlerFunc(s.serve)}
	s.sessions = make(chan struct{}, 32)
	if serverWebTransport {
		s.server.QUICConfig.EnableStreamResetPartialDelivery = true
		webtransport.ConfigureHTTP3Server(s.server)
		// Origin policy is route-specific and checked before Upgrade in
		// serveWebTransport; the library's single global callback cannot do it.
		s.web = &webtransport.Server{H3: s.server, CheckOrigin: func(*http.Request) bool { return true }}
	}
	return s
}
func (s *http3Services) Close() {
	if s.web != nil {
		_ = s.web.Close()
	}
	_ = s.proxy.Close()
	_ = s.server.Close()
}

func (s *http3Services) ServeQUICConn(conn *quic.Conn) error {
	if s.web != nil {
		return s.web.ServeQUICConn(conn)
	}
	return s.server.ServeQUICConn(conn)
}

func (s *http3Services) serve(w http.ResponseWriter, r *http.Request) {
	if s.web != nil && r.URL.Path == "/.well-known/xrok/webtransport" {
		s.serveWebTransport(w, r)
		return
	}
	if !serverMASQUE || !strings.HasPrefix(r.URL.Path, "/.well-known/masque/udp/") {
		http.NotFound(w, r)
		return
	}
	if !controlClientCertAccepted(r) {
		http.Error(w, "client certificate required", 403)
		return
	}
	// Opt-in must not turn an anonymously configured server into an open UDP relay.
	if serverApiToken == "" && len(serverClientIdentityPolicy.credentials) == 0 {
		http.Error(w, "authenticated UDP proxy credentials required", 503)
		return
	}
	serverAuthScopeMiddleware("proxy:udp", s.serveMASQUE)(w, r)
}

func (s *http3Services) serveMASQUE(w http.ResponseWriter, r *http.Request) {
	select {
	case s.flows <- struct{}{}:
		defer func() { <-s.flows }()
	default:
		http.Error(w, "UDP flow limit reached", 429)
		return
	}
	template, err := uritemplate.New("https://" + r.Host + "/.well-known/masque/udp/{target_host}/{target_port}/")
	if err != nil {
		http.Error(w, "invalid authority", 400)
		return
	}
	req, err := masque.ParseRequest(r, template)
	if err != nil {
		code := 400
		if p, ok := err.(*masque.RequestParseError); ok {
			code = p.HTTPStatus
		}
		http.Error(w, "invalid CONNECT-UDP request", code)
		return
	}
	conn, err := dialEgress("udp", req.Target, 10*time.Second)
	if err != nil {
		http.Error(w, "UDP destination unavailable or denied", 403)
		return
	}
	udp, ok := conn.(*net.UDPConn)
	if !ok {
		conn.Close()
		http.Error(w, "UDP socket unavailable", 502)
		return
	}
	// Bound resource lifetime even for a peer that never sends another datagram.
	deadline := time.Now().Add(5 * time.Minute)
	_ = udp.SetDeadline(deadline)
	_, ok = w.(http3.HTTPStreamer)
	if !ok {
		udp.Close()
		http.Error(w, "HTTP/3 required", 400)
		return
	}
	// The library owns the stream after takeover; a deadline also bounds its
	// capsule reader, not only the target UDP socket.
	_ = http.NewResponseController(w).SetReadDeadline(deadline)
	_ = s.proxy.ProxyConnectedSocket(w, req, udp)
}

func (s *http3Services) String() string {
	return fmt.Sprintf("HTTP/3 services (masque=%t)", serverMASQUE)
}
