package cmd

import (
	"context"
	"testing"

	"xrok/cmd/config"
)

func TestServerRuntimeValidationAndArgs(t *testing.T) {
	if _, err := NewServerRuntime(config.ServerConfig{}); err == nil {
		t.Fatal("expected validation error")
	}
	r, err := NewServerRuntime(config.ServerConfig{Domain: "test.local", ProxyPort: 8443, TLSCertPath: "cert.pem", TLSKeyPath: "key.pem", Debug: true})
	if err != nil {
		t.Fatal(err)
	}
	a := r.args()
	if len(a) < 6 || a[0] != "--domain" {
		t.Fatalf("unexpected args: %#v", a)
	}
}

func TestServerRuntimeStartOnce(t *testing.T) {
	r, err := NewServerRuntime(config.ServerConfig{Domain: "test.local", ProxyPort: 0, TLSCertPath: "", TLSKeyPath: ""})
	if err != nil {
		t.Fatal(err)
	}
	// Do not launch listeners in unit tests; verify lifecycle guard directly.
	r.mu.Lock()
	r.start = true
	r.mu.Unlock()
	if err := r.Start(context.Background()); err == nil {
		t.Fatal("expected start-once error")
	}
}
