package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestConfigMigrationIsIdempotent(t *testing.T) {
	c := &ClientConfig{ConnectionType: "quic"}
	MigrateClientConfig(c)
	if c.Version != CurrentVersion || c.Transport != "quic" {
		t.Fatalf("migration failed: %+v", c)
	}
	MigrateClientConfig(c)
	if c.Version != CurrentVersion || c.Transport != "quic" {
		t.Fatalf("migration not idempotent: %+v", c)
	}
	s := &ServerConfig{}
	MigrateServerConfig(s)
	if s.Version != CurrentVersion {
		t.Fatalf("server version=%d", s.Version)
	}
}

func TestClientConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.yaml")
	want := DefaultClientConfig()
	want.Server = "localhost"
	want.ID = "client-a"
	want.CACert = "/tmp/test-ca.pem"
	want.Transport = "quic"
	want.Tunnels = []TunnelConfig{{Name: "app", Target: "127.0.0.1:8080"}}
	if err := SaveClientConfig(want, path); err != nil {
		t.Fatal(err)
	}
	got, err := LoadClientConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Server != want.Server || got.ID != want.ID || got.CACert != want.CACert || got.Transport != "quic" || got.Reconnect.MaxWait != 5*time.Minute {
		t.Fatalf("round trip mismatch: %#v", got)
	}
}

func TestServerClientIdentitiesValidationAndRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.yaml")
	want := DefaultServerConfig()
	want.Domain = "localhost"
	want.ClientIdentities = []ClientIdentityConfig{{ID: "alice", Token: "alice-secret", Scopes: []string{"client", "p2p"}}}
	if err := SaveServerConfig(want, path); err != nil {
		t.Fatal(err)
	}
	got, err := LoadServerConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ClientIdentities) != 1 || got.ClientIdentities[0].ID != "alice" || len(got.ClientIdentities[0].Scopes) != 2 {
		t.Fatalf("identity round trip mismatch: %#v", got.ClientIdentities)
	}
	got.ClientIdentities = append(got.ClientIdentities, got.ClientIdentities[0])
	if err := got.Validate(); err == nil {
		t.Fatal("duplicate client identity accepted")
	}
}

func TestLoadServerConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.yaml")
	data := []byte("domain: localhost\nproxy_port: 8443\ntls_cert_path: cert.pem\ntls_key_path: key.pem\nrate_limit: 42\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadServerConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Domain != "localhost" || cfg.ProxyPort != 8443 || cfg.RateLimit != 42 {
		t.Fatalf("unexpected config: %#v", cfg)
	}
}

func TestServerConfigValidate(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.Domain = "localhost"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.ProxyPort = 70000
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected invalid proxy port")
	}
}
