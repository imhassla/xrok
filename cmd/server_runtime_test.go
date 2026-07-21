package cmd

import (
	"context"
	"database/sql"
	"testing"

	"xrok/cmd/config"
)

func TestRuntimeStateBackendSelection(t *testing.T) {
	c := config.ServerConfig{Domain: "test.local", StateBackend: "redis", RedisAddr: "127.0.0.1:6379"}
	r, err := NewServerRuntime(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.StateStore().(*RedisStateStore); !ok {
		t.Fatalf("store=%T", r.StateStore())
	}
	c.StateBackend = "postgres"
	c.PostgresDSN = "dsn"
	r, err = NewServerRuntimeWithDeps(c, ServerRuntimeDeps{StoreFactory: StoreFactoryDeps{OpenSQL: func(_, _ string) (*sql.DB, error) { return nil, context.Canceled }}})
	if err != context.Canceled {
		t.Fatalf("err=%v", err)
	}
}

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

func TestServerRuntimeInjectableRunner(t *testing.T) {
	called := make(chan []string, 1)
	r, err := NewServerRuntimeWithDeps(config.ServerConfig{Domain: "test.local", ProxyPort: 0}, ServerRuntimeDeps{
		Run: func(args []string) { called <- args },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	select {
	case args := <-called:
		if len(args) == 0 || args[0] != "--domain" {
			t.Fatalf("unexpected args: %v", args)
		}
	default:
		t.Fatal("injected runner was not called")
	}
}
