package cmd

import (
	"context"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"xrok/cmd/config"
)

func routeTestPipe(c net.Conn, reader io.Reader, up net.Conn) {
	done := make(chan struct{}, 2)
	go func() { io.Copy(up, reader); done <- struct{}{} }()
	go func() { io.Copy(c, up); done <- struct{}{} }()
	<-done
	c.Close()
	up.Close()
	<-done
}

func connectFixture(t *testing.T, secure, auth bool, observe func(*http.Request)) *httptest.Server {
	t.Helper()
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if observe != nil {
			observe(r)
		}
		if auth && r.Header.Get("Proxy-Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("lab-user:lab-password")) {
			http.Error(w, "auth required", 407)
			return
		}
		if r.Method != "CONNECT" {
			http.Error(w, "expected CONNECT", 405)
			return
		}
		up, err := net.DialTimeout("tcp", r.Host, time.Second)
		if err != nil {
			http.Error(w, "target unavailable", 502)
			return
		}
		c, br, err := w.(http.Hijacker).Hijack()
		if err != nil {
			up.Close()
			return
		}
		fmt.Fprint(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
		routeTestPipe(c, br, up)
	}))
	if secure {
		s.StartTLS()
	} else {
		s.Start()
	}
	t.Cleanup(s.Close)
	return s
}

func routeOrigin(t *testing.T) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{}
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Lab-Session") != "" || r.Header.Get("X-Proxy-Session") != "" || r.Header.Get("Proxy-Authorization") != "" {
			t.Error("proxy credentials or marker reached origin")
		}
		if r.URL.Path == "/ws" {
			c, e := upgrader.Upgrade(w, r, nil)
			if e != nil {
				return
			}
			defer c.Close()
			kind, p, e := c.ReadMessage()
			if e == nil {
				c.WriteMessage(kind, p)
			}
			return
		}
		if r.Method == "POST" {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			w.Write(body)
		} else {
			io.WriteString(w, "origin-ok")
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func TestProxyHTTPAndWebSocketRouteMatrix(t *testing.T) {
	for _, secure := range []bool{false, true} {
		for _, auth := range []bool{false, true} {
			t.Run(fmt.Sprintf("https-proxy=%v/auth=%v", secure, auth), func(t *testing.T) {
				origin := routeOrigin(t)
				var requests atomic.Int64
				proxy := connectFixture(t, secure, auth, func(r *http.Request) {
					if r.Header.Get("X-Lab-Session") != "same-session" {
						t.Error("missing affinity marker")
					}
					requests.Add(1)
				})
				u, _ := url.Parse(proxy.URL)
				if auth {
					u.User = url.UserPassword("lab-user", "lab-password")
				}
				state := &clientState{HTTPProxy: u.String(), ProxySession: "same-session", ProxySessionHeader: "X-Lab-Session", ConnectTimeout: time.Second, TLSConfig: origin.Client().Transport.(*http.Transport).TLSClientConfig.Clone()}
				if secure {
					state.ProxyTLSConfig = proxy.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
				}
				tr := state.routeTransport()
				defer tr.CloseIdleConnections()
				client := &http.Client{Transport: tr, Timeout: 2 * time.Second}
				for _, method := range []string{"GET", "POST"} {
					payload := strings.Repeat("x", 68796)
					req, _ := http.NewRequest(method, origin.URL, strings.NewReader(payload))
					resp, e := client.Do(req)
					if e != nil {
						t.Fatal(e)
					}
					b, e := io.ReadAll(resp.Body)
					resp.Body.Close()
					want := "origin-ok"
					if method == "POST" {
						want = payload
					}
					if e != nil || string(b) != want {
						t.Fatalf("origin body mismatch: method=%s status=%d got=%d want=%d error=%v preview=%q", method, resp.StatusCode, len(b), len(want), e, string(b[:min(len(b), 80)]))
					}
				}
				c, _, e := state.routeWebSocketDialer().DialContext(context.Background(), "wss"+strings.TrimPrefix(origin.URL, "https")+"/ws", nil)
				if e != nil {
					t.Fatal(e)
				}
				defer c.Close()
				c.WriteMessage(websocket.TextMessage, []byte("ws-echo"))
				_, b, e := c.ReadMessage()
				if e != nil || string(b) != "ws-echo" {
					t.Fatal(string(b), e)
				}
				if requests.Load() < 2 {
					t.Fatal("HTTP or WebSocket bypassed proxy")
				}
			})
		}
	}
}

func TestProxyAndTargetTLSAreVerifiedIndependently(t *testing.T) {
	origin := routeOrigin(t)
	proxy := connectFixture(t, true, false, nil)
	state := &clientState{HTTPProxy: proxy.URL, ConnectTimeout: time.Second, TLSConfig: origin.Client().Transport.(*http.Transport).TLSClientConfig.Clone()}
	tr := state.routeTransport()
	defer tr.CloseIdleConnections()
	if resp, e := (&http.Client{Transport: tr}).Get(origin.URL); e == nil {
		resp.Body.Close()
		t.Fatal("untrusted HTTPS proxy accepted")
	}
	state.ProxyTLSConfig = proxy.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	state.TLSConfig = &tls.Config{}
	tr2 := state.routeTransport()
	defer tr2.CloseIdleConnections()
	if resp, e := (&http.Client{Transport: tr2}).Get(origin.URL); e == nil {
		resp.Body.Close()
		t.Fatal("untrusted target accepted")
	}
}

func TestProxyCONNECTCancellationBudgetAndErrorBody(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(fmt.Sprint(reject), func(t *testing.T) {
			entered := make(chan struct{})
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, _, e := w.(http.Hijacker).Hijack()
				if e != nil {
					return
				}
				defer c.Close()
				close(entered)
				if reject {
					io.WriteString(c, "HTTP/1.1 407 Proxy Authentication Required\r\nContent-Length: 1000000\r\n\r\n")
				}
				io.Copy(io.Discard, c)
			}))
			defer proxy.Close()
			state := &clientState{HTTPProxy: proxy.URL, ConnectTimeout: time.Minute}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				c, e := state.dialRoute(ctx, "tcp", "target:443")
				if c != nil {
					c.Close()
				}
				done <- e
			}()
			<-entered
			if !reject {
				cancel()
			}
			select {
			case e := <-done:
				if e == nil {
					t.Fatal("unexpected success")
				}
			case <-time.After(time.Second):
				t.Fatal("blocked CONNECT did not terminate")
			}
		})
	}
}

func TestProxyBufferedBytesAndDeadlineClearing(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, br, e := w.(http.Hijacker).Hijack()
		if e != nil {
			return
		}
		defer c.Close()
		io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\nhello")
		io.Copy(c, br)
	}))
	defer proxy.Close()
	state := &clientState{HTTPProxy: proxy.URL, ConnectTimeout: 200 * time.Millisecond}
	c, e := state.dialRoute(context.Background(), "tcp", "target:443")
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	var p [5]byte
	if _, e = io.ReadFull(c, p[:]); e != nil || string(p[:]) != "hello" {
		t.Fatal(p, e)
	}
	time.Sleep(250 * time.Millisecond)
	c.SetDeadline(time.Now().Add(time.Second))
	c.Write([]byte("world"))
	if _, e = io.ReadFull(c, p[:]); e != nil || string(p[:]) != "world" {
		t.Fatal(p, e)
	}
}

func TestProxyRedactionAndAuthorityValidation(t *testing.T) {
	if s := redactedProxy("http://lab-user:lab-password@127.0.0.1:8080"); strings.Contains(s, "lab-user") || strings.Contains(s, "lab-password") {
		t.Fatal(s)
	}
	for _, raw := range []string{"http://user:password@[bad", "http://user:password@host:0", "ftp://user:password@host"} {
		_, e := parseProxyRoute(raw)
		if e == nil || strings.Contains(e.Error(), "password") {
			t.Fatal(e)
		}
	}
	u, e := parseProxyRoute("https://[::1]")
	if e != nil || u.Host != "[::1]:443" {
		t.Fatal(u, e)
	}
}

func TestQUICProxyRejectedBeforeRuntimeDial(t *testing.T) {
	if e := config.ValidateProxySettings("http://proxy:8080", "", "", time.Second, true, false); e == nil {
		t.Fatal("QUIC proxy combination accepted")
	}
	// Runtime callers cannot bypass the CLI guard with manually constructed state.
	setupClientLogging()
	disconnected := make(chan struct{}, 1)
	connectQUICWithDisconnect(context.Background(), &clientState{HTTPProxy: "http://proxy.invalid:8080", UseQUIC: true}, disconnected)
	select {
	case <-disconnected:
	default:
		t.Fatal("rejected QUIC path did not signal disconnect")
	}
}

// Minimal controlled SOCKS5 fixture, including RFC 1929 auth. Target DNS is simulated
// at the proxy so the remote-DNS test never queries an external resolver.
func socksRouteFixture(t *testing.T, auth bool, observe func(byte, string)) string {
	t.Helper()
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, e := ln.Accept()
			if e != nil {
				return
			}
			go func() {
				defer c.Close()
				c.SetDeadline(time.Now().Add(3 * time.Second))
				var h [2]byte
				if _, e := io.ReadFull(c, h[:]); e != nil {
					return
				}
				methods := make([]byte, int(h[1]))
				if _, e := io.ReadFull(c, methods); e != nil {
					return
				}
				if auth {
					c.Write([]byte{5, 2})
					if _, e := io.ReadFull(c, h[:]); e != nil {
						return
					}
					user := make([]byte, int(h[1]))
					io.ReadFull(c, user)
					var length [1]byte
					io.ReadFull(c, length[:])
					password := make([]byte, int(length[0]))
					io.ReadFull(c, password)
					if string(user) != "lab-user" || string(password) != "lab-password" {
						c.Write([]byte{1, 1})
						return
					}
					c.Write([]byte{1, 0})
				} else {
					c.Write([]byte{5, 0})
				}
				var req [4]byte
				if _, e := io.ReadFull(c, req[:]); e != nil {
					return
				}
				var host string
				switch req[3] {
				case 1:
					b := make([]byte, 4)
					io.ReadFull(c, b)
					host = net.IP(b).String()
				case 4:
					b := make([]byte, 16)
					io.ReadFull(c, b)
					host = net.IP(b).String()
				case 3:
					var n [1]byte
					io.ReadFull(c, n[:])
					b := make([]byte, int(n[0]))
					io.ReadFull(c, b)
					host = string(b)
				default:
					return
				}
				var port [2]byte
				io.ReadFull(c, port[:])
				if observe != nil {
					observe(req[3], host)
				}
				if host == "origin.invalid" || host == "::1" {
					host = "127.0.0.1"
				}
				up, e := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port[:])))), time.Second)
				if e != nil {
					c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
					return
				}
				c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
				c.SetDeadline(time.Time{})
				routeTestPipe(c, c, up)
			}()
		}
	}()
	return ln.Addr().String()
}

func TestSOCKSProxyRoutesAuthenticationAndDNS(t *testing.T) {
	for _, auth := range []bool{false, true} {
		t.Run(fmt.Sprint(auth), func(t *testing.T) {
			origin := routeOrigin(t)
			proxy := socksRouteFixture(t, auth, nil)
			u := &url.URL{Scheme: "socks5h", Host: proxy}
			if auth {
				u.User = url.UserPassword("lab-user", "lab-password")
			}
			state := &clientState{HTTPProxy: u.String(), ConnectTimeout: time.Second, TLSConfig: origin.Client().Transport.(*http.Transport).TLSClientConfig.Clone()}
			tr := state.routeTransport()
			defer tr.CloseIdleConnections()
			resp, e := (&http.Client{Transport: tr}).Get(origin.URL)
			if e != nil {
				t.Fatal(e)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(b) != "origin-ok" {
				t.Fatal(string(b))
			}
			ws, _, e := state.routeWebSocketDialer().DialContext(context.Background(), "wss"+strings.TrimPrefix(origin.URL, "https")+"/ws", nil)
			if e != nil {
				t.Fatal(e)
			}
			ws.WriteMessage(1, []byte("socks-echo"))
			_, b, e = ws.ReadMessage()
			ws.Close()
			if e != nil || string(b) != "socks-echo" {
				t.Fatal(string(b), e)
			}
		})
	}
	var atyp byte
	var hostname string
	var mu sync.Mutex
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer origin.Close()
	u, _ := url.Parse(origin.URL)
	proxy := socksRouteFixture(t, false, func(a byte, h string) { mu.Lock(); atyp = a; hostname = h; mu.Unlock() })
	for _, scheme := range []string{"socks5", "socks5h"} {
		host := "localhost"
		if scheme == "socks5h" {
			host = "origin.invalid"
		}
		state := &clientState{HTTPProxy: scheme + "://" + proxy, ConnectTimeout: time.Second}
		c, e := state.dialRoute(context.Background(), "tcp", net.JoinHostPort(host, u.Port()))
		if e != nil {
			t.Fatal(e)
		}
		c.Close()
		mu.Lock()
		if scheme == "socks5h" && (atyp != 3 || hostname != "origin.invalid") {
			t.Errorf("remote DNS changed target: %d %s", atyp, hostname)
		}
		if scheme == "socks5" && atyp == 3 {
			t.Error("local DNS sent hostname")
		}
		mu.Unlock()
	}
}

func TestRouteErrorsPreserveCause(t *testing.T) {
	if !errors.Is(&routeError{Stage: "dial", Err: context.Canceled}, context.Canceled) {
		t.Fatal("cause lost")
	}
}

func TestRouteTLSConfigurationIsNotMutated(t *testing.T) {
	origin := routeOrigin(t)
	cfg := origin.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	state := &clientState{TLSConfig: cfg, ConnectTimeout: time.Second}
	target := strings.TrimPrefix(origin.URL, "https://")
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, e := state.dialRouteTLS(context.Background(), "tcp", target)
			if e != nil {
				t.Error(e)
				return
			}
			c.Close()
		}()
	}
	wg.Wait()
	if cfg.ServerName != "" {
		t.Fatal("shared SNI mutated", cfg.ServerName)
	}
}
func TestRouteSecretRedaction(t *testing.T) {
	state := &clientState{HTTPProxy: "http://lab-user:lab-password@proxy:8080", ProxySession: "opaque-session", APIToken: "api-secret", SessionToken: "server-session"}
	text := state.redactRouteSecrets("http://lab-user:lab-password@proxy:8080 api-secret server-session opaque-session lab-password")
	for _, secret := range []string{"lab-user", "lab-password", "opaque-session", "api-secret", "server-session"} {
		if strings.Contains(text, secret) {
			t.Fatal("secret leaked", secret)
		}
	}
}

func TestTLSProxyAndSOCKSHandshakesHonorCancellation(t *testing.T) {
	for _, scheme := range []string{"https", "socks5h"} {
		t.Run(scheme, func(t *testing.T) {
			ln, e := net.Listen("tcp", "127.0.0.1:0")
			if e != nil {
				t.Fatal(e)
			}
			defer ln.Close()
			accepted := make(chan struct{})
			go func() {
				c, e := ln.Accept()
				if e != nil {
					return
				}
				defer c.Close()
				close(accepted)
				io.Copy(io.Discard, c)
			}()
			state := &clientState{HTTPProxy: scheme + "://" + ln.Addr().String(), ConnectTimeout: time.Minute}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				c, e := state.dialRoute(ctx, "tcp", "origin:443")
				if c != nil {
					c.Close()
				}
				done <- e
			}()
			<-accepted
			cancel()
			select {
			case e := <-done:
				if e == nil {
					t.Fatal("canceled handshake succeeded")
				}
			case <-time.After(time.Second):
				t.Fatal("handshake ignored cancellation")
			}
		})
	}
}

func TestFramedUpgradeBytesSurviveHTTPResponseReadAhead(t *testing.T) {
	proxy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _, e := w.(http.Hijacker).Hijack()
		if e != nil {
			return
		}
		defer c.Close()
		// Compute a valid upgrade response and pipeline a complete binary WebSocket greeting.
		key := r.Header.Get("Sec-WebSocket-Key")
		hash := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		fmt.Fprintf(c, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: %s\r\n\r\n\x82\x0craw-greeting", base64.StdEncoding.EncodeToString(hash[:]))
		io.Copy(c, c)
	}))
	defer proxy.Close()
	state := &clientState{TLSConfig: proxy.Client().Transport.(*http.Transport).TLSClientConfig.Clone(), ConnectTimeout: time.Second}
	ws, _, e := state.dialWebSocket(context.Background(), "wss"+strings.TrimPrefix(proxy.URL, "https"), nil)
	if e != nil {
		t.Fatal(e)
	}
	defer ws.Close()
	ws.SetReadDeadline(time.Now().Add(time.Second))
	kind, p, e := ws.ReadMessage()
	if e != nil || kind != websocket.BinaryMessage || string(p) != "raw-greeting" {
		t.Fatal("upgrade frame lost", string(p), e)
	}
}
