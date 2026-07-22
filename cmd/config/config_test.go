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

func TestClientTURNConfigRoundTripAndMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client-turn.yaml")
	want := DefaultClientConfig()
	want.Server = "xrok.test"
	want.P2P.Enabled = true
	want.P2P.TURN = TURNConfig{
		Enabled: true, Server: "turn.test:5349", Username: "client-a", Password: "secret",
		Transport: "tls", CACert: "/run/secrets/turn-ca.pem", ServerName: "turn.test",
		ChannelBind: true, Timeout: 4 * time.Second, RefreshInterval: 2 * time.Minute,
		MaxDatagramSize: 32 * 1024, Retransmits: 4,
	}
	if err := SaveClientConfig(want, path); err != nil {
		t.Fatal(err)
	}
	got, err := LoadClientConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.P2P.TURN.Transport != "tls" || got.P2P.TURN.CACert != want.P2P.TURN.CACert || !got.P2P.TURN.ChannelBind || got.P2P.TURN.RefreshInterval != 2*time.Minute {
		t.Fatalf("TURN round trip mismatch: %#v", got.P2P.TURN)
	}
	legacy := &ClientConfig{P2P: P2PConfig{Enabled: true, TURN: TURNConfig{Enabled: true}}}
	MigrateClientConfig(legacy)
	if legacy.P2P.TURN.Transport != "udp" {
		t.Fatalf("legacy TURN transport=%q", legacy.P2P.TURN.Transport)
	}
}

func TestClientTURNConfigValidation(t *testing.T) {
	valid := DefaultClientConfig()
	valid.P2P.Enabled = true
	valid.P2P.TURN = TURNConfig{Enabled: true, Server: "turn.test:3478", Username: "u", Password: "p", Transport: "udp"}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*ClientConfig){
		"p2p_disabled": func(c *ClientConfig) { c.P2P.Enabled = false },
		"server":       func(c *ClientConfig) { c.P2P.TURN.Server = "turn.test" },
		"credentials":  func(c *ClientConfig) { c.P2P.TURN.Password = "" },
		"transport":    func(c *ClientConfig) { c.P2P.TURN.Transport = "sctp" },
		"server_name":  func(c *ClientConfig) { c.P2P.TURN.ServerName = "bad name" },
		"duration":     func(c *ClientConfig) { c.P2P.TURN.Timeout = -time.Second },
		"datagram":     func(c *ClientConfig) { c.P2P.TURN.MaxDatagramSize = 65047 },
		"retransmits":  func(c *ClientConfig) { c.P2P.TURN.Retransmits = 9 },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := *valid
			if valid.P2P.TURN.CACert != "" {
				t.Fatal("unexpected shared test state")
			}
			mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("invalid TURN config accepted")
			}
		})
	}
}

func TestSavedConfigsContainingCredentialsAreOwnerOnly(t *testing.T) {
	clientPath := filepath.Join(t.TempDir(), "client.yaml")
	client := DefaultClientConfig()
	client.Server = "xrok.test"
	client.Token = "client-secret"
	if err := SaveClientConfig(client, clientPath); err != nil {
		t.Fatal(err)
	}
	serverPath := filepath.Join(t.TempDir(), "server.yaml")
	server := DefaultServerConfig()
	server.Domain = "xrok.test"
	server.APIToken = "server-secret"
	if err := SaveServerConfig(server, serverPath); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{clientPath, serverPath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0600 {
			t.Fatalf("%s mode=%#o, want 0600", path, got)
		}
	}
}

func TestSaveConfigAtomicallyReplacesSymlinkWithoutTouchingTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("do-not-overwrite"), 0644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "client.yaml")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultClientConfig()
	cfg.Server = "xrok.test"
	if err := SaveClientConfig(cfg, path); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "do-not-overwrite" {
		t.Fatalf("symlink target changed: %q err=%v", got, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0600 {
		t.Fatalf("saved config mode=%v", info.Mode())
	}
}

func TestServerTelemetryResourceBounds(t *testing.T) {
	base := DefaultServerConfig()
	base.Domain = "xrok.test"
	base.Telemetry.Endpoint = "http://collector:4318"
	for name, mutate := range map[string]func(*ServerConfig){
		"queue":   func(c *ServerConfig) { c.Telemetry.QueueSize = 65537 },
		"batch":   func(c *ServerConfig) { c.Telemetry.BatchSize = 4097 },
		"retries": func(c *ServerConfig) { c.Telemetry.MaxRetries = 9 },
		"backoff": func(c *ServerConfig) { c.Telemetry.RetryBackoff = time.Hour },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := *base
			mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("unsafe telemetry config accepted")
			}
		})
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

func TestServerTelemetryConfigMigrationAndRoundTrip(t *testing.T) {
	cfg := &ServerConfig{Domain: "localhost", Version: 1}
	MigrateServerConfig(cfg)
	if cfg.Version != 2 || cfg.Telemetry.QueueSize != 2048 || cfg.Telemetry.ShutdownTimeout != 10*time.Second {
		t.Fatalf("telemetry migration failed: %#v", cfg.Telemetry)
	}
	path := filepath.Join(t.TempDir(), "server.yaml")
	cfg.Telemetry.Endpoint = "http://collector:4318"
	cfg.Telemetry.BatchSize = 32
	if err := SaveServerConfig(cfg, path); err != nil {
		t.Fatal(err)
	}
	got, err := LoadServerConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Telemetry.Endpoint != cfg.Telemetry.Endpoint || got.Telemetry.BatchSize != 32 {
		t.Fatalf("telemetry round trip mismatch: %#v", got.Telemetry)
	}
}

func TestServerTelemetryConfigValidation(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.Domain = "localhost"
	cfg.Telemetry.Endpoint = "http://collector:4318"
	cfg.Telemetry.BatchSize = cfg.Telemetry.QueueSize + 1
	if err := cfg.Validate(); err == nil {
		t.Fatal("oversized telemetry batch accepted")
	}
	cfg.Telemetry.BatchSize = 1
	cfg.Telemetry.ShutdownTimeout = -time.Second
	if err := cfg.Validate(); err == nil {
		t.Fatal("negative telemetry shutdown timeout accepted")
	}
}

func TestGeneratedServerConfigKeepsTelemetryDisabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.yaml")
	if err := os.WriteFile(path, []byte(GenerateExampleServerConfig()), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadServerConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telemetry.Endpoint != "" {
		t.Fatalf("generated config unexpectedly enables telemetry at %q", cfg.Telemetry.Endpoint)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestServerTelemetryEndpointValidation(t *testing.T) {
	for _, endpoint := range []string{"collector:4318", "ftp://collector:4318", "http:///traces", "http://user:pass@collector:4318"} {
		cfg := DefaultServerConfig()
		cfg.Domain = "localhost"
		cfg.Telemetry.Endpoint = endpoint
		if err := cfg.Validate(); err == nil {
			t.Fatalf("invalid telemetry endpoint accepted: %q", endpoint)
		}
	}
}

func TestFutureServerConfigVersionIsNeverDowngraded(t *testing.T) {
	future := CurrentVersion + 10
	cfg := &ServerConfig{Version: future, Domain: "localhost"}
	MigrateServerConfig(cfg)
	if cfg.Version != future {
		t.Fatalf("future version downgraded to %d", cfg.Version)
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("future config version accepted")
	}
	if err := SaveServerConfig(cfg, filepath.Join(t.TempDir(), "saved-future.yaml")); err == nil {
		t.Fatal("future server config version saved without error")
	}
	path := filepath.Join(t.TempDir(), "future.yaml")
	if err := os.WriteFile(path, []byte("version: 12\ndomain: localhost\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadServerConfig(path); err == nil {
		t.Fatal("future config version loaded without error")
	}
}

func TestFutureClientConfigVersionIsRejected(t *testing.T) {
	future := CurrentVersion + 10
	cfg := &ClientConfig{Version: future}
	MigrateClientConfig(cfg)
	if cfg.Version != future {
		t.Fatalf("future client version downgraded to %d", cfg.Version)
	}
	if err := SaveClientConfig(cfg, filepath.Join(t.TempDir(), "saved-future.yaml")); err == nil {
		t.Fatal("future client config version saved without error")
	}
	path := filepath.Join(t.TempDir(), "future.yaml")
	if err := os.WriteFile(path, []byte("version: 12\nserver: localhost\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadClientConfig(path); err == nil {
		t.Fatal("future client config version loaded without error")
	}
}
