//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Exercise the actual ProxyCommand stdout, not just the carrier adapters.
func TestDefaultAutoStdioAndRemovedControlEndpoints(t *testing.T) {
	root := repositoryRoot(t)
	tmp := t.TempDir()
	ca, cert, key := generateTestPKI(t, tmp)
	binary := filepath.Join(tmp, "xrok")
	run(t, root, nil, "go", "build", "-o", binary, ".")
	port := freePort(t)
	token := "stdio-test-token"
	server := startProcess(t, tmp, []string{"XROK_API_TOKEN=" + token}, binary, "server", "--domain", "localhost", "--port", fmt.Sprint(port), "--tls-cert", cert, "--tls-key", key, "--allow-private-egress", "--state-file", filepath.Join(tmp, "state.dat"), "--log-file", filepath.Join(tmp, "server.log"))
	defer stopProcess(t, server)
	hc := newTLSClient(t, ca)
	waitFor(t, 20*time.Second, func() bool {
		r, e := hc.Get(fmt.Sprintf("https://localhost:%d/health", port))
		if e != nil {
			return false
		}
		r.Body.Close()
		return r.StatusCode == 200
	})
	for _, path := range []string{"/mux", "/ws", "/client_ws"} {
		r, e := hc.Get(fmt.Sprintf("https://localhost:%d%s", port, path))
		if e != nil {
			t.Fatal(e)
		}
		r.Body.Close()
		if r.StatusCode != 410 {
			t.Fatalf("retired endpoint %s: %d", path, r.StatusCode)
		}
	}
	target, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		c, e := target.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		b, e := io.ReadAll(c)
		if e == nil {
			c.Write(append([]byte("reply:"), b...))
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "client", "--server", "localhost", "--rp", fmt.Sprint(port), "--ca-cert", ca, "--token", token, "--stdio", "--stdio-target", target.Addr().String())
	command.Dir = tmp
	input := bytes.Repeat([]byte("ssh-proxy-payload"), 20000)
	command.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("stdio process failed: %v\n%s", err, stderr.String())
	}
	if !bytes.Equal(output, append([]byte("reply:"), input...)) {
		t.Fatalf("stdio stdout corrupted or truncated: %d bytes\nstderr: %s", len(output), stderr.String())
	}
}
