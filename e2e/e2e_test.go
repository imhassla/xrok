//go:build e2e

package e2e

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestHTTPRelayEndToEnd(t *testing.T) {
	t.Parallel()
	root := repositoryRoot(t)
	tmp := t.TempDir()
	caFile, certFile, keyFile := generateTestPKI(t, tmp)
	binary := filepath.Join(tmp, "xrok")
	run(t, root, nil, "go", "build", "-o", binary, ".")

	upstream := newIPv4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream", "xrok-e2e")
		_, _ = fmt.Fprintf(w, "method=%s path=%s body=", r.Method, r.URL.Path)
		_, _ = io.Copy(w, r.Body)
	}))
	defer upstream.Close()
	target := strings.TrimPrefix(upstream.URL, "http://")
	port := freePort(t)
	token := "e2e-control-token"

	server := startProcess(t, tmp, []string{"XROK_API_TOKEN=" + token}, binary,
		"server", "--domain", "localhost", "--port", fmt.Sprint(port),
		"--tls-cert", certFile, "--tls-key", keyFile,
		"--state-file", filepath.Join(tmp, "state.dat"),
		"--log-file", filepath.Join(tmp, "server.log"))
	defer stopProcess(t, server)

	tlsClient := newTLSClient(t, caFile)
	waitFor(t, 10*time.Second, func() bool {
		resp, err := tlsClient.Get(fmt.Sprintf("https://localhost:%d/health", port))
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})

	client := startProcess(t, tmp, nil, binary,
		"client", "--server", "localhost", "--rp", fmt.Sprint(port),
		"--ca-cert", caFile, "--token", token, "--id", "e2e-client",
		"--tunnel", "app:"+target, "--retry-interval", "50ms", "--max-interval", "250ms")
	defer stopProcess(t, client)

	var responseBody string
	waitFor(t, 15*time.Second, func() bool {
		req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("https://localhost:%d/hello", port), strings.NewReader("payload"))
		req.Host = "app.localhost"
		resp, err := tlsClient.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		responseBody = string(data)
		return resp.StatusCode == http.StatusOK && resp.Header.Get("X-Upstream") == "xrok-e2e"
	})
	if responseBody != "method=POST path=/hello body=payload" {
		t.Fatalf("unexpected tunneled response: %q", responseBody)
	}
}

func TestPerClientIdentityPolicyEndToEnd(t *testing.T) {
	root := repositoryRoot(t)
	tmp := t.TempDir()
	caFile, certFile, keyFile := generateTestPKI(t, tmp)
	binary := filepath.Join(tmp, "xrok")
	run(t, root, nil, "go", "build", "-o", binary, ".")

	upstream := newIPv4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "identity-ok")
	}))
	target := strings.TrimPrefix(upstream.URL, "http://")
	port := freePort(t)
	configPath := filepath.Join(tmp, "server.yaml")
	configData := fmt.Sprintf(`domain: localhost
proxy_port: %d
tls_cert_path: %s
tls_key_path: %s
state_file: %s
log_file: %s
client_identities:
  - id: alice
    token: alice-secret
    scopes: [client, "p2p:signal"]
  - id: bob
    token: bob-secret
    scopes: [client]
  - id: auditor
    token: auditor-secret
    scopes: ["p2p:read"]
`, port, certFile, keyFile, filepath.Join(tmp, "state.dat"), filepath.Join(tmp, "server.log"))
	if err := os.WriteFile(configPath, []byte(configData), 0o600); err != nil {
		t.Fatal(err)
	}

	server := startProcess(t, tmp, nil, binary, "server", "--config", configPath)
	defer stopProcess(t, server)
	tlsClient := newTLSClient(t, caFile)
	waitFor(t, 10*time.Second, func() bool {
		resp, err := tlsClient.Get(fmt.Sprintf("https://localhost:%d/health", port))
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})

	client := startProcess(t, tmp, nil, binary,
		"client", "--server", "localhost", "--rp", fmt.Sprint(port),
		"--ca-cert", caFile, "--token", "alice-secret", "--id", "alice",
		"--tunnel", "identity:"+target, "--retry-interval", "50ms", "--max-interval", "250ms")
	defer stopProcess(t, client)
	waitFor(t, 15*time.Second, func() bool {
		req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("https://localhost:%d/", port), nil)
		req.Host = "identity.localhost"
		resp, err := tlsClient.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode == http.StatusOK && string(body) == "identity-ok"
	})

	registration := map[string]interface{}{
		"connection_type": "tls",
		"client_id":       "mallory",
	}
	assertControlStatus(t, tlsClient, port, "/register_client", "alice-secret", registration, http.StatusForbidden)

	discoverAsBob := map[string]interface{}{
		"type":      "p2p_discover",
		"from_peer": "bob",
		"to_peer":   "alice",
	}
	assertControlStatus(t, tlsClient, port, "/p2p/signal", "bob-secret", discoverAsBob, http.StatusForbidden)

	impersonateBob := map[string]interface{}{
		"type":      "p2p_discover",
		"from_peer": "bob",
		"to_peer":   "alice",
	}
	assertControlStatus(t, tlsClient, port, "/p2p/signal", "alice-secret", impersonateBob, http.StatusForbidden)

	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("https://localhost:%d/p2p/clients", port), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-API-Token", "auditor-secret")
	resp, err := tlsClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("scoped auditor request failed: status=%s body=%q", resp.Status, body)
	}
}

func TestP2PReplayRejectedAcrossProcess(t *testing.T) {
	root := repositoryRoot(t)
	tmp := t.TempDir()
	helper := filepath.Join(tmp, "p2p-replay-e2e")
	run(t, root, nil, "go", "build", "-o", helper, "./e2e/testdata/p2p_replay_helper.go")
	run(t, root, nil, helper)
}

func assertControlStatus(t *testing.T, client *http.Client, port int, path, token string, payload interface{}, want int) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("https://localhost:%d%s", port, path), strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Token", token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		responseBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s: status=%s, want=%d body=%q", path, resp.Status, want, responseBody)
	}
}

func TestQUICHTTPRelayEndToEnd(t *testing.T) {
	root := repositoryRoot(t)
	tmp := t.TempDir()
	caFile, certFile, keyFile := generateTestPKI(t, tmp)
	binary := filepath.Join(tmp, "xrok")
	run(t, root, nil, "go", "build", "-o", binary, ".")

	upstream := newIPv4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Transport", "quic")
		_, _ = fmt.Fprintf(w, "quic method=%s body=", r.Method)
		_, _ = io.Copy(w, r.Body)
	}))
	target := strings.TrimPrefix(upstream.URL, "http://")
	port := freePort(t)
	token := "e2e-quic-token"
	server := startProcess(t, tmp, []string{"XROK_API_TOKEN=" + token}, binary,
		"server", "--domain", "localhost", "--port", fmt.Sprint(port),
		"--tls-cert", certFile, "--tls-key", keyFile,
		"--state-file", filepath.Join(tmp, "state.dat"), "--log-file", filepath.Join(tmp, "server.log"))
	defer stopProcess(t, server)
	tlsClient := newTLSClient(t, caFile)
	waitFor(t, 10*time.Second, func() bool {
		resp, err := tlsClient.Get(fmt.Sprintf("https://localhost:%d/health", port))
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	client := startProcess(t, tmp, nil, binary,
		"client", "--server", "localhost", "--rp", fmt.Sprint(port), "--quic",
		"--ca-cert", caFile, "--token", token, "--id", "quic-client",
		"--tunnel", "app:"+target, "--retry-interval", "50ms", "--max-interval", "250ms")
	defer stopProcess(t, client)
	var body string
	waitFor(t, 20*time.Second, func() bool {
		req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("https://localhost:%d/quic", port), strings.NewReader("payload"))
		req.Host = "app.localhost"
		resp, err := tlsClient.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		body = string(data)
		return resp.StatusCode == http.StatusOK && resp.Header.Get("X-Transport") == "quic"
	})
	if body != "quic method=POST body=payload" {
		t.Fatalf("unexpected QUIC response: %q", body)
	}
}

func TestHTTPAuthAndHeadersEndToEnd(t *testing.T) {
	root := repositoryRoot(t)
	tmp := t.TempDir()
	caFile, certFile, keyFile := generateTestPKI(t, tmp)
	binary := filepath.Join(tmp, "xrok")
	run(t, root, nil, "go", "build", "-o", binary, ".")

	upstream := newIPv4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Seen-By-Upstream", r.Header.Get("X-Injected"))
		_, _ = io.WriteString(w, "protected")
	}))
	target := strings.TrimPrefix(upstream.URL, "http://")
	port := freePort(t)
	token := "e2e-auth-token"
	configPath := filepath.Join(tmp, "client.yaml")
	configData := fmt.Sprintf("server: localhost\ntls: true\ntunnels:\n  - name: protected\n    target: %s\n    basic_auth: user:secret\n    capability: e2e-jwt-secret\n    capability_audience: protected\nheaders:\n  X-Injected: e2e\nreconnect:\n  enabled: true\n  initial_wait: 50ms\n  max_wait: 250ms\n", target)
	if err := os.WriteFile(configPath, []byte(configData), 0o600); err != nil {
		t.Fatal(err)
	}
	server := startProcess(t, tmp, []string{"XROK_API_TOKEN=" + token}, binary,
		"server", "--domain", "localhost", "--port", fmt.Sprint(port),
		"--tls-cert", certFile, "--tls-key", keyFile,
		"--state-file", filepath.Join(tmp, "state.dat"),
		"--log-file", filepath.Join(tmp, "server.log"))
	defer stopProcess(t, server)
	tlsClient := newTLSClient(t, caFile)
	waitFor(t, 10*time.Second, func() bool {
		resp, err := tlsClient.Get(fmt.Sprintf("https://localhost:%d/health", port))
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})

	client := startProcess(t, tmp, nil, binary,
		"client", "--config", configPath, "--rp", fmt.Sprint(port),
		"--ca-cert", caFile, "--token", token, "--id", "auth-client",
		"--retry-interval", "50ms", "--max-interval", "250ms")
	defer stopProcess(t, client)

	waitFor(t, 15*time.Second, func() bool {
		req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("https://localhost:%d/", port), nil)
		req.Host = "protected.localhost"
		resp, err := tlsClient.Do(req)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusForbidden
	})

	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("https://localhost:%d/", port), nil)
	req.Host = "protected.localhost"
	credentials := base64.StdEncoding.EncodeToString([]byte("user:secret"))
	req.Header.Set("Authorization", "Basic "+credentials)
	req.Header.Set("X-Xrok-Capability", makeTestJWT("e2e-jwt-secret", "protected"))
	resp, err := tlsClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "protected" {
		t.Fatalf("protected request failed: status=%s body=%q", resp.Status, body)
	}
}

func makeTestJWT(secret, audience string) string {
	enc := func(v []byte) string { return base64.RawURLEncoding.EncodeToString(v) }
	header := enc([]byte(`{"alg":"HS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]interface{}{"aud": audience, "exp": time.Now().Add(time.Hour).Unix()})
	body := enc(claims)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(header + "." + body))
	return header + "." + body + "." + enc(mac.Sum(nil))
}

func TestReverseTCPRelayEndToEnd(t *testing.T) {
	root := repositoryRoot(t)
	tmp := t.TempDir()
	caFile, certFile, keyFile := generateTestPKI(t, tmp)
	binary := filepath.Join(tmp, "xrok")
	run(t, root, nil, "go", "build", "-o", binary, ".")

	local, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	go func() {
		conn, acceptErr := local.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, conn)
	}()
	serverPort := freePort(t)
	remotePort := freePort(t)
	token := "e2e-reverse-token"
	server := startProcess(t, tmp, []string{"XROK_API_TOKEN=" + token}, binary,
		"server", "--domain", "localhost", "--port", fmt.Sprint(serverPort),
		"--tls-cert", certFile, "--tls-key", keyFile,
		"--state-file", filepath.Join(tmp, "state.dat"), "--log-file", filepath.Join(tmp, "server.log"))
	defer stopProcess(t, server)
	tlsClient := newTLSClient(t, caFile)
	waitFor(t, 10*time.Second, func() bool {
		resp, err := tlsClient.Get(fmt.Sprintf("https://localhost:%d/health", serverPort))
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	client := startProcess(t, tmp, nil, binary,
		"client", "--server", "localhost", "--rp", fmt.Sprint(serverPort), "--ca-cert", caFile,
		"--token", token, "--id", "reverse-client", "-R", fmt.Sprintf("%d:%s", remotePort, local.Addr()),
		"--retry-interval", "50ms", "--max-interval", "250ms")
	defer stopProcess(t, client)

	var conn net.Conn
	waitFor(t, 15*time.Second, func() bool {
		candidate, dialErr := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", remotePort), 200*time.Millisecond)
		if dialErr != nil {
			return false
		}
		conn = candidate
		return true
	})
	defer conn.Close()
	if _, err := conn.Write([]byte("reverse-payload")); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(io.LimitReader(conn, int64(len("reverse-payload"))))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "reverse-payload" {
		t.Fatalf("unexpected reverse response: %q", got)
	}
}

// TestUDPTunnelEndToEnd exercises the real UDP data path with a deterministic
// loss pattern. The first datagram is intentionally dropped by the upstream
// echo server; subsequent packets (including a large near-MTU-safe payload) must relay.
func TestUDPTunnelEndToEnd(t *testing.T) {
	root := repositoryRoot(t)
	tmp := t.TempDir()
	caFile, certFile, keyFile := generateTestPKI(t, tmp)
	binary := filepath.Join(tmp, "xrok")
	run(t, root, nil, "go", "build", "-o", binary, ".")

	upstream, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	// Deterministic fault injection: drop exactly the first packet.
	go func() {
		buf := make([]byte, 65535)
		dropped := false
		for {
			n, addr, readErr := upstream.ReadFromUDP(buf)
			if readErr != nil {
				return
			}
			if !dropped {
				dropped = true
				continue
			}
			_, _ = upstream.WriteToUDP(buf[:n], addr)
		}
	}()

	serverPort := freePort(t)
	localPort := freeUDPPort(t)
	token := "e2e-udp-token"
	server := startProcess(t, tmp, []string{"XROK_API_TOKEN=" + token}, binary,
		"server", "--domain", "localhost", "--port", fmt.Sprint(serverPort),
		"--tls-cert", certFile, "--tls-key", keyFile,
		"--state-file", filepath.Join(tmp, "state.dat"), "--log-file", filepath.Join(tmp, "server.log"))
	defer stopProcess(t, server)
	tlsClient := newTLSClient(t, caFile)
	waitFor(t, 10*time.Second, func() bool {
		resp, getErr := tlsClient.Get(fmt.Sprintf("https://localhost:%d/health", serverPort))
		if getErr != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})

	remote := upstream.LocalAddr().(*net.UDPAddr)
	client := startProcess(t, tmp, nil, binary,
		"client", "--server", "localhost", "--rp", fmt.Sprint(serverPort), "--ca-cert", caFile,
		"--token", token, "--id", "udp-client", "--udp", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", localPort, remote.Port),
		"--retry-interval", "50ms", "--max-interval", "250ms")
	defer stopProcess(t, client)

	conn, dialErr := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: localPort})
	if dialErr != nil {
		t.Fatal(dialErr)
	}
	defer conn.Close()
	// Allow registration and the local UDP listener to become ready before the
	// deliberate loss probe; otherwise the first packet can be lost before the
	// tunnel exists at all.
	time.Sleep(1500 * time.Millisecond)
	// First packet is dropped; timeout is expected and must not tear down tunnel.
	conn.SetReadDeadline(time.Now().Add(700 * time.Millisecond))
	if _, writeErr := conn.Write([]byte("drop-me")); writeErr != nil {
		t.Fatal(writeErr)
	}
	buf := make([]byte, 65535)
	if _, readErr := conn.Read(buf); readErr == nil {
		t.Fatal("expected deterministic first-packet loss")
	}

	// Keep the process E2E portable across hosts whose loopback UDP MTU is
	// smaller than the internet maximum; framing limits are covered separately
	// by unit tests.
	payload := make([]byte, 8000)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	if _, writeErr := conn.Write(payload); writeErr != nil {
		t.Fatal(writeErr)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, _, readErr := conn.ReadFromUDP(buf)
	if readErr != nil {
		t.Fatalf("UDP relay read: %v", readErr)
	}
	if n != len(payload) || string(buf[:n]) != string(payload) {
		t.Fatalf("payload mismatch: got %d bytes, want %d", n, len(payload))
	}
}

func newIPv4TestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	return server
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot determine repository root")
	}
	return filepath.Dir(filepath.Dir(file))
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func freeUDPPort(t *testing.T) int {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	conn.Close()
	return port
}

func run(t *testing.T, dir string, env []string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, output)
	}
}

func startProcess(t *testing.T, dir string, env []string, name string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	return cmd
}

func stopProcess(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(os.Interrupt)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Signal(syscall.SIGKILL)
		<-done
	}
}

func waitFor(t *testing.T, timeout time.Duration, check func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if check() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("condition not met within %s", timeout)
		case <-ticker.C:
		}
	}
}

func newTLSClient(t *testing.T, caFile string) *http.Client {
	t.Helper()
	pemData, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemData) {
		t.Fatal("failed to load test CA")
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig(pool)}, Timeout: 2 * time.Second}
}

func tlsConfig(pool *x509.CertPool) *tls.Config {
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
}

func generateTestPKI(t *testing.T, dir string) (string, string, string) {
	t.Helper()
	now := time.Now()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "xrok test CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	serverKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		DNSNames: []string{"localhost", "*.localhost"}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caTemplate, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caFile := filepath.Join(dir, "ca.pem")
	certFile := filepath.Join(dir, "server.pem")
	keyFile := filepath.Join(dir, "server-key.pem")
	writePEM(t, caFile, "CERTIFICATE", caDER)
	writePEM(t, certFile, "CERTIFICATE", serverDER)
	writePEM(t, keyFile, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(serverKey))
	return caFile, certFile, keyFile
}

func writePEM(t *testing.T, path, blockType string, data []byte) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := pem.Encode(file, &pem.Block{Type: blockType, Bytes: data}); err != nil {
		t.Fatal(err)
	}
}
