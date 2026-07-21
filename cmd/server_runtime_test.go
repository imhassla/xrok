package cmd

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

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
	c.PostgresDSN = "postgres://user:pass@localhost:5432/db?sslmode=disable"
	r, err = NewServerRuntimeWithDeps(c, ServerRuntimeDeps{StoreFactory: StoreFactoryDeps{OpenSQL: func(_, _ string) (*sql.DB, error) { return nil, context.Canceled }}})
	if err != context.Canceled {
		t.Fatalf("err=%v", err)
	}
}

func TestPostgresBackendRequiresDriverContract(t *testing.T) {
	c := config.ServerConfig{Domain: "test.local", StateBackend: "postgres", PostgresDSN: "postgres://user:pass@localhost:5432/db?sslmode=disable"}
	_, err := NewServerRuntime(c)
	if err == nil || !strings.Contains(err.Error(), "OpenSQL") {
		t.Fatalf("err=%v, want explicit driver availability error", err)
	}
}

func TestServerRuntimeUsesConfigBackendAndInjectedStore(t *testing.T) {
	// A runtime embedded in a process must honor its explicit config even when
	// the caller's environment requests another HA backend.
	t.Setenv("XROK_HA_BACKEND", "redis")
	store := NewInMemoryStateStore(8)
	r, err := NewServerRuntimeWithDeps(config.ServerConfig{Domain: "test.local", StateBackend: "memory"}, ServerRuntimeDeps{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	if r.StateStore() != store {
		t.Fatalf("store=%T, want injected store", r.StateStore())
	}
	if r.HAConfig().Backend != "memory" {
		t.Fatalf("backend=%q, want memory", r.HAConfig().Backend)
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

func TestServerRuntimeContextRunnerIsIsolatedAndCancellable(t *testing.T) {
	legacyShutdown := serverShutdownChan
	started := make(chan struct{})
	r, err := NewServerRuntimeWithDeps(config.ServerConfig{Domain: "test.local"}, ServerRuntimeDeps{
		RunContext: func(ctx context.Context, _ []string) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-started
	r.Shutdown()
	if err := r.WaitErr(); err != context.Canceled {
		t.Fatalf("WaitErr=%v, want context.Canceled", err)
	}
	if serverShutdownChan != legacyShutdown {
		t.Fatal("injected runtime mutated legacy shutdown channel")
	}
	select {
	case <-legacyShutdown:
		t.Fatal("injected runtime closed legacy shutdown channel")
	default:
	}
}

func TestServerRuntimeShutdownBeforeStartDoesNotCloseGlobal(t *testing.T) {
	legacyShutdown := serverShutdownChan
	r, err := NewServerRuntimeWithDeps(config.ServerConfig{Domain: "test.local"}, ServerRuntimeDeps{
		RunContext: func(context.Context, []string) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	r.Shutdown()
	select {
	case <-legacyShutdown:
		t.Fatal("unstarted runtime closed process-global shutdown channel")
	default:
	}
}

func TestServerRuntimeWaitContextIsBounded(t *testing.T) {
	r, err := NewServerRuntimeWithDeps(config.ServerConfig{Domain: "test.local"}, ServerRuntimeDeps{
		RunContext: func(ctx context.Context, _ []string) error {
			<-ctx.Done()
			return ctx.Err()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := r.WaitContext(waitCtx); err != context.DeadlineExceeded {
		t.Fatalf("WaitContext=%v, want deadline", err)
	}
	r.Shutdown()
	if err := r.WaitContext(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestServerRuntimeRejectsConcurrentLegacyOwners(t *testing.T) {
	r1, err := NewServerRuntime(config.ServerConfig{Domain: "test.local"})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := NewServerRuntime(config.ServerConfig{Domain: "test.local"})
	if err != nil {
		t.Fatal(err)
	}
	r1.run = func([]string) { <-r1.shutdown }
	r2.run = func([]string) { <-r2.shutdown }
	if err := r1.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r2.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "already active") {
		t.Fatalf("err=%v, want active legacy runtime error", err)
	}
	r1.Shutdown()
	r1.Wait()
	if err := r2.Start(context.Background()); err != nil {
		t.Fatalf("start after owner release: %v", err)
	}
	r2.Shutdown()
	r2.Wait()
}
