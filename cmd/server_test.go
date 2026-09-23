package cmd

import (
	"crypto/x509"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVerifyBasicAuth(t *testing.T) {
	credential := base64.StdEncoding.EncodeToString([]byte("user:secret"))
	request := []byte("GET / HTTP/1.1\r\nAuthorization: Basic " + credential + "\r\n\r\n")
	if !verifyBasicAuth(request, "user:secret") {
		t.Fatal("valid basic auth rejected")
	}
	if verifyBasicAuth(request, "user:other") {
		t.Fatal("invalid basic auth accepted")
	}
}

func TestInjectHeadersReplacesExistingValue(t *testing.T) {
	request := []byte("GET / HTTP/1.1\r\nX-Env: old\r\nHost: app.localhost\r\n\r\n")
	result := string(injectHeaders(request, map[string]string{"X-Env": "test", "X-New": "yes"}))
	if strings.Count(strings.ToLower(result), "x-env:") != 1 || !strings.Contains(result, "X-Env: test") {
		t.Fatalf("header not replaced safely:\n%s", result)
	}
	if !strings.Contains(result, "X-New: yes") {
		t.Fatalf("new header missing:\n%s", result)
	}
}

func TestInjectHeadersDoesNotRewriteBody(t *testing.T) {
	request := []byte("POST / HTTP/1.1\r\nX-Env: old\r\nHost: app.localhost\r\nContent-Length: 11\r\n\r\nX-Env: old")
	result := string(injectHeaders(request, map[string]string{"X-Env": "test"}))
	if !strings.HasSuffix(result, "\r\n\r\nX-Env: old") {
		t.Fatalf("body was modified:\n%s", result)
	}
}

func BenchmarkInjectHeaders(b *testing.B) {
	request := []byte("POST /benchmark HTTP/1.1\r\nHost: app.localhost\r\nContent-Length: 7\r\n\r\npayload")
	headers := map[string]string{"X-Trace-ID": "bench", "X-Environment": "test"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = injectHeaders(request, headers)
	}
}

func TestRestoredClientWithoutSessionTokenCannotAttach(t *testing.T) {
	restored := &serverClientInfo{}
	if restored.validSessionToken("") {
		t.Fatal("empty presented token matched a client restored without a session token")
	}
	live := &serverClientInfo{sessionToken: "s3cret"}
	if !live.validSessionToken("s3cret") || live.validSessionToken("") || live.validSessionToken("other") {
		t.Fatal("session token comparison is incorrect for a live client")
	}
}

func TestServerConfigPathAcceptsBothFlagForms(t *testing.T) {
	cases := map[string][]string{
		"/a.yaml": {"--config", "/a.yaml"},
		"/b.yaml": {"--config=/b.yaml"},
		"/c.yaml": {"--domain", "x", "-config=/c.yaml"},
		"/d.yaml": {"-config", "/d.yaml", "--debug"},
	}
	for want, args := range cases {
		if got, ok := serverConfigPath(args); !ok || got != want {
			t.Fatalf("serverConfigPath(%q) = %q, %v; want %q", args, got, ok, want)
		}
	}
	if _, ok := serverConfigPath([]string{"--", "--config=/e.yaml"}); ok {
		t.Fatal("config after the -- terminator must be ignored")
	}
}

func TestIsUpgradeRequestPreservesWebSocketHandshake(t *testing.T) {
	ws := httptest.NewRequest(http.MethodGet, "/ws", nil)
	ws.Header.Set("Connection", "keep-alive, Upgrade")
	ws.Header.Set("Upgrade", "websocket")
	if !isUpgradeRequest(ws) {
		t.Fatal("WebSocket handshake not recognised as an upgrade")
	}
	plain := httptest.NewRequest(http.MethodGet, "/", nil)
	plain.Header.Set("Connection", "keep-alive")
	if isUpgradeRequest(plain) {
		t.Fatal("ordinary request treated as an upgrade")
	}
	bare := httptest.NewRequest(http.MethodGet, "/", nil)
	bare.Header.Set("Upgrade", "websocket")
	if isUpgradeRequest(bare) {
		t.Fatal("Upgrade without Connection: upgrade must stay single-use")
	}
}

func TestControlClientCertRequiredOnlyForControlPlane(t *testing.T) {
	old := serverClientCA
	t.Cleanup(func() { serverClientCA = old })

	serverClientCA = ""
	if !controlClientCertAccepted(httptest.NewRequest(http.MethodGet, "/register_client", nil)) {
		t.Fatal("control plane rejected while mTLS is disabled")
	}

	serverClientCA = "/ca.pem"
	noCert := httptest.NewRequest(http.MethodGet, "https://example.com/register_client", nil)
	if controlClientCertAccepted(noCert) {
		t.Fatal("control plane accepted without a verified client certificate")
	}
	for _, probe := range []string{"/health", "/ready"} {
		if !controlClientCertAccepted(httptest.NewRequest(http.MethodGet, "https://example.com"+probe, nil)) {
			t.Fatalf("%s probe required a client certificate", probe)
		}
	}
	verified := httptest.NewRequest(http.MethodGet, "https://example.com/register_client", nil)
	verified.TLS.VerifiedChains = [][]*x509.Certificate{{{}}}
	if !controlClientCertAccepted(verified) {
		t.Fatal("verified client certificate rejected")
	}
}

func TestStaleSessionTeardownKeepsReRegisteredClient(t *testing.T) {
	const id = "reconnecting-client"
	t.Cleanup(func() {
		legacyClientRegistry.Delete(id)
		legacyRouteRegistry.Delete("app")
		releaseServerPorts(41001, 41002)
	})
	tunnels := []ServerTunnelConfig{{Name: "app"}}
	stale := &serverClientInfo{serializableClientInfo: serializableClientInfo{PortNoTLS: 41001, ClientPortTLS: 41002}, tunnels: tunnels}
	fresh := &serverClientInfo{serializableClientInfo: serializableClientInfo{PortNoTLS: 41001, ClientPortTLS: 41002}, tunnels: tunnels}

	// The successor registered under the same ID and was handed the ports
	// the stale session had released.
	putServerClient(id, fresh)
	legacyRouteRegistry.Put("app", &TunnelRoute{ClientID: id, TunnelName: "app"})
	serverPortMutex.Lock()
	serverUsedPorts[41001], serverUsedPorts[41002] = true, true
	serverPortMutex.Unlock()

	if forgetServerClient(id, stale) {
		t.Fatal("stale session reported removing its successor")
	}
	if current, ok := legacyClientRegistry.Get(id); !ok || current != fresh {
		t.Fatal("stale teardown removed the re-registered client")
	}
	if _, ok := legacyRouteRegistry.Get("app"); !ok {
		t.Fatal("stale teardown removed the successor's route")
	}
	serverPortMutex.Lock()
	inUse := serverUsedPorts[41001] && serverUsedPorts[41002]
	serverPortMutex.Unlock()
	if !inUse {
		t.Fatal("stale teardown released ports now owned by the successor")
	}

	if !forgetServerClient(id, fresh) {
		t.Fatal("current client was not removed")
	}
	if _, ok := legacyRouteRegistry.Get("app"); ok {
		t.Fatal("current client's route survived its teardown")
	}
}
