package cmd

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
	"golang.org/x/net/http2"
)

func standardCarrierFixture(t *testing.T) (*httptest.Server, *serverClientInfo, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	id := "standard-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	info := &serverClientInfo{serializableClientInfo: serializableClientInfo{UDID: id}, sessionToken: "private-session", ctx: ctx, cancelFunc: cancel, tunnelMap: map[string]string{"app": "unused"}}
	info.decrementOnce.Do(func() {})
	legacyClientRegistry.Put(id, info)
	t.Cleanup(func() { legacyClientRegistry.Delete(id) })
	done := make(chan struct{})
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer close(done); serverStandardTunnelHandler(w, r) }))
	ts.EnableHTTP2 = true
	if err := configureIngressHTTP2(ts.Config); err != nil {
		t.Fatal(err)
	}
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts, info, done
}

func carrierState(t *testing.T, ts *httptest.Server, info *serverClientInfo) *clientState {
	t.Helper()
	u, _ := url.Parse(ts.URL)
	host, portText, _ := net.SplitHostPort(u.Host)
	port, _ := strconv.Atoi(portText)
	return &clientState{ServerAddr: host, ServerPort: port, ClientID: info.UDID, SessionToken: info.sessionToken, UseTLS: true, TLSConfig: ts.Client().Transport.(*http.Transport).TLSClientConfig.Clone(), ConnectTimeout: 2 * time.Second, SupportedTransports: []string{"http2", "wss"}}
}

func exerciseStandardCarrier(t *testing.T, s *clientState, info *serverClientInfo, mode string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	carrier, err := s.dialStandardCarrier(ctx, mode)
	if err != nil {
		t.Fatal(err)
	}
	defer carrier.Close()
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	peer, err := yamux.Client(carrier, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	deadline := time.Now().Add(2 * time.Second)
	var server *yamux.Session
	for time.Now().Before(deadline) {
		info.muxSessionMutex.Lock()
		server = info.muxSession
		info.muxSessionMutex.Unlock()
		if server != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if server == nil {
		t.Fatal("server mux did not attach")
	}
	stream, err := server.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	_ = stream.SetDeadline(time.Now().Add(5 * time.Second))
	accepted := make(chan struct{})
	go func() {
		c, err := peer.Accept()
		if err != nil {
			close(accepted)
			return
		}
		defer c.Close()
		close(accepted)
		_, _ = io.Copy(c, c)
	}()
	<-accepted
	payload := bytes.Repeat([]byte("framed-and-multiplexed"), 100000)
	written := make(chan error, 1)
	go func() { _, err := stream.Write(payload); written <- err }()
	response := make([]byte, len(payload))
	if _, err = io.ReadFull(stream, response); err != nil {
		t.Fatal(err)
	}
	if err = <-written; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(response, payload) {
		t.Fatal("carrier payload changed")
	}
}

func TestStandardCarriersThroughProxyRoutes(t *testing.T) {
	for _, mode := range []string{"http2", "wss"} {
		for _, route := range []string{"direct", "http", "https", "socks5h"} {
			t.Run(mode+"/"+route, func(t *testing.T) {
				ts, info, done := standardCarrierFixture(t)
				state := carrierState(t, ts, info)
				var count atomic.Int64
				if route == "socks5h" {
					state.HTTPProxy = "socks5h://" + socksRouteFixture(t, false, func(byte, string) { count.Add(1) })
				} else if route != "direct" {
					proxy := connectFixture(t, route == "https", true, func(r *http.Request) {
						count.Add(1)
						if r.Header.Get("X-Proxy-Session") != "opaque" {
							t.Error("missing proxy affinity marker")
						}
					})
					u, _ := url.Parse(proxy.URL)
					u.User = url.UserPassword("lab-user", "lab-password")
					state.HTTPProxy = u.String()
					state.ProxySession = "opaque"
					state.ProxyTLSConfig = ts.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
				}
				exerciseStandardCarrier(t, state, info, mode)
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Fatal("server carrier did not close")
				}
				if route != "direct" && count.Load() != 1 {
					t.Fatalf("expected one proxied connection, got %d", count.Load())
				}
			})
		}
	}
}

func TestStandardCarriersThroughHTTPAwareIntermediary(t *testing.T) {
	for _, mode := range []string{"http2", "wss"} {
		t.Run(mode, func(t *testing.T) {
			origin, info, done := standardCarrierFixture(t)
			u, _ := url.Parse(origin.URL)
			var handler http.Handler
			var binaryFrames atomic.Int64
			if mode == "http2" {
				rp := httputil.NewSingleHostReverseProxy(u)
				tr := &http2.Transport{TLSClientConfig: origin.Client().Transport.(*http.Transport).TLSClientConfig}
				defer tr.CloseIdleConnections()
				rp.Transport = tr
				rp.FlushInterval = -1
				handler = rp
			} else {
				handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					d := websocket.Dialer{TLSClientConfig: origin.Client().Transport.(*http.Transport).TLSClientConfig, Subprotocols: []string{tunnelProtocol}}
					h := make(http.Header)
					h.Set("X-Xrok-Session", r.Header.Get("X-Xrok-Session"))
					h.Set("X-Xrok-Tunnel-Version", "2")
					up, _, err := d.Dial("wss"+strings.TrimPrefix(origin.URL, "https")+r.URL.RequestURI(), h)
					if err != nil {
						http.Error(w, "upstream failed", 502)
						return
					}
					defer up.Close()
					ws, err := (&websocket.Upgrader{Subprotocols: []string{tunnelProtocol}}).Upgrade(w, r, nil)
					if err != nil {
						return
					}
					defer ws.Close()
					copyFrames := func(dst, src *websocket.Conn) {
						for {
							kind, p, err := src.ReadMessage()
							if err != nil {
								return
							}
							if kind != websocket.BinaryMessage {
								t.Error("non-binary carrier frame")
								return
							}
							binaryFrames.Add(1)
							if err = dst.WriteMessage(kind, p); err != nil {
								return
							}
						}
					}
					finished := make(chan struct{})
					go func() { copyFrames(up, ws); up.Close(); close(finished) }()
					copyFrames(ws, up)
					ws.Close()
					<-finished
				})
			}
			middle := httptest.NewUnstartedServer(handler)
			middle.EnableHTTP2 = true
			middle.StartTLS()
			defer middle.Close()
			state := carrierState(t, middle, info)
			exerciseStandardCarrier(t, state, info, mode)
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("origin session did not close")
			}
			if mode == "wss" && binaryFrames.Load() == 0 {
				t.Fatal("did not traverse a frame-validating WebSocket proxy")
			}
		})
	}
}

func TestStandardTransportNegotiationAndAuthentication(t *testing.T) {
	state := &clientState{UseTLS: true}
	if _, err := state.dialStandardCarrier(context.Background(), "http2"); err == nil {
		t.Fatal("unadvertised transport accepted")
	}
	for _, version := range []string{"", "1", "2"} {
		r := httptest.NewRequest("POST", "https://server/v2/tunnel?clientID=missing", nil)
		r.Header.Set("X-Xrok-Tunnel-Version", version)
		w := httptest.NewRecorder()
		serverStandardTunnelHandler(w, r)
		if w.Code != 400 && w.Code != 401 {
			t.Fatalf("invalid tunnel accepted: %d", w.Code)
		}
	}
}
