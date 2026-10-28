package cmd

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	webtransport "github.com/quic-go/webtransport-go"
)

func webTransportOriginAllowed(r *http.Request, origins []string) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || (u.Scheme != "https" && u.Scheme != "http") {
		return false
	}
	if u.Scheme == "https" && strings.EqualFold(u.Host, r.Host) {
		return true
	}
	for _, allowed := range origins {
		if strings.EqualFold(strings.TrimSuffix(allowed, "/"), strings.TrimSuffix(origin, "/")) {
			return true
		}
	}
	return false
}

func webTransportCapability(r *http.Request, route *TunnelRoute) (time.Time, bool) {
	expires := time.Now().Add(5 * time.Minute)
	if route.Capability == "" {
		return expires, false
	}
	if route.BasicAuth != "" {
		u, p, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(u+":"+p), []byte(route.BasicAuth)) != 1 {
			return expires, false
		}
	}
	header := r.Header.Get("X-Xrok-Capability")
	if header == "" {
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			header = strings.TrimPrefix(auth, "Bearer ")
		}
	}
	if header != "" && subtle.ConstantTimeCompare([]byte(header), []byte(route.Capability)) == 1 {
		return expires, true
	}
	token := header
	if token == "" {
		token = r.URL.Query().Get("cap")
	}
	if !verifyJWTCapability(token, route.Capability, route.CapabilityAudience, time.Now()) {
		return expires, false
	}
	parts := strings.Split(token, ".")
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Exp int64 `json:"exp"`
	}
	_ = json.Unmarshal(payload, &claims)
	if t := time.Unix(claims.Exp, 0); t.Before(expires) {
		expires = t
	}
	return expires, true
}

func (s *http3Services) serveWebTransport(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	client, route, name := resolveServerTunnel(serverDepsFromRequest(r), extractServerClientIDFromHost(host))
	if client == nil || route == nil || !route.WebTransport {
		http.Error(w, "WebTransport tunnel unavailable", 404)
		return
	}
	if !webTransportOriginAllowed(r, route.WebTransportOrigins) {
		http.Error(w, "origin not allowed", 403)
		return
	}
	expires, ok := webTransportCapability(r, route)
	if !ok {
		http.Error(w, "tunnel capability required", 403)
		return
	}
	select {
	case s.sessions <- struct{}{}:
		defer func() { <-s.sessions }()
	default:
		http.Error(w, "session limit reached", 429)
		return
	}
	session, err := s.web.Upgrade(w, r)
	if err != nil {
		return
	}
	defer session.CloseWithError(0, "")
	ctx, cancel := context.WithDeadline(session.Context(), expires)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { session.CloseWithError(0, "") })
	defer stop()
	if client.ctx != nil {
		stopClient := context.AfterFunc(client.ctx, cancel)
		defer stopClient()
	}
	var wg sync.WaitGroup
	defer func() { cancel(); _ = session.CloseWithError(0, ""); wg.Wait() }()
	wg.Add(1)
	go func() { defer wg.Done(); bridgeWebTransportDatagrams(ctx, cancel, session, client, route, name) }()
	slots := make(chan struct{}, 32)
	for {
		stream, err := session.AcceptStream(ctx)
		if err != nil {
			return
		}
		select {
		case slots <- struct{}{}:
		default:
			stream.CancelRead(1)
			stream.CancelWrite(1)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			defer stream.Close()
			up, err := openIngressStream(ctx, client, name)
			if err != nil {
				stream.CancelRead(2)
				stream.CancelWrite(2)
				return
			}
			defer up.Close()
			relayLocalApplication(ctx, stream, up)
		}()
	}
}

func bridgeWebTransportDatagrams(ctx context.Context, cancel context.CancelFunc, session *webtransport.Session, client *serverClientInfo, route *TunnelRoute, name string) {
	var up net.Conn
	defer func() {
		if up != nil {
			_ = up.SetDeadline(time.Now())
			up.Close()
		}
	}()
	var readerDone chan struct{}
	defer func() {
		if readerDone != nil {
			_ = up.SetDeadline(time.Now())
			up.Close()
			<-readerDone
		}
	}()
	for {
		p, err := session.ReceiveDatagram(ctx)
		if err != nil {
			return
		}
		if !route.Datagrams {
			session.CloseWithError(3, "datagrams disabled")
			cancel()
			return
		}
		if len(p) > 1200 {
			continue
		}
		if up == nil {
			up, err = openVisitorUDPStream(ctx, client, name)
			if err != nil {
				cancel()
				return
			}
			readerDone = make(chan struct{})
			go func() {
				defer close(readerDone)
				for {
					p, err := readUDPFrame(up)
					if err != nil {
						cancel()
						return
					}
					if err = session.SendDatagram(p); err != nil {
						cancel()
						return
					}
				}
			}()
		}
		if err = writeUDPFrame(up, p); err != nil {
			cancel()
			return
		}
	}
}

var _ io.ReadWriteCloser = (*webtransport.Stream)(nil)
