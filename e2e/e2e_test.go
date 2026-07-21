//go:build e2e

package e2e

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
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
	configData := fmt.Sprintf("server: localhost\ntls: true\ntunnels:\n  - name: protected\n    target: %s\n    basic_auth: user:secret\nheaders:\n  X-Injected: e2e\nreconnect:\n  enabled: true\n  initial_wait: 50ms\n  max_wait: 250ms\n", target)
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
		return resp.StatusCode == http.StatusUnauthorized
	})

	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("https://localhost:%d/", port), nil)
	req.Host = "protected.localhost"
	credentials := base64.StdEncoding.EncodeToString([]byte("user:secret"))
	req.Header.Set("Authorization", "Basic "+credentials)
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
