package config

import "testing"

func TestTunnelTransportCompatibilityAndValidation(t *testing.T) {
	if got, err := TunnelTransport("", false); err != nil || got != "auto" {
		t.Fatal(got, err)
	}
	for _, s := range []string{"http2", "wss", "quic", "auto"} {
		got, err := TunnelTransport(s, false)
		if err != nil || got != s {
			t.Fatal(s, got, err)
		}
	}
	if got, err := TunnelTransport("", true); err != nil || got != "quic" {
		t.Fatal(got, err)
	}
	for _, s := range []string{"grpc", "http3", "unknown", "legacy", "tcp", "tls", "non-tls", "websocket"} {
		if _, err := TunnelTransport(s, false); err == nil {
			t.Fatal("accepted", s)
		}
	}
	if _, err := TunnelTransport("wss", true); err == nil {
		t.Fatal("accepted conflicting flags")
	}
}

func TestTLSConnectionTypeMigratesToAuto(t *testing.T) {
	c := &ClientConfig{ConnectionType: "tls"}
	MigrateClientConfig(c)
	if c.Transport != "auto" {
		t.Fatal(c.Transport)
	}
	c.Server = "example.test"
	c.TLS = true
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	on := true
	server := DefaultServerConfig()
	server.Domain = "example.test"
	server.LegacyListeners = &on
	if err := server.Validate(); err == nil {
		t.Fatal("retired listener enabled")
	}
}
