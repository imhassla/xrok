package config

import "testing"

func TestTunnelTransportCompatibilityAndValidation(t *testing.T) {
	for _, s := range []string{"", "legacy", "tcp", "tls", "websocket"} {
		got, err := TunnelTransport(s, false)
		if err != nil || got != "legacy" {
			t.Fatalf("legacy alias %q: %q %v", s, got, err)
		}
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
	for _, s := range []string{"grpc", "http3", "unknown"} {
		if _, err := TunnelTransport(s, false); err == nil {
			t.Fatal("accepted", s)
		}
	}
	if _, err := TunnelTransport("wss", true); err == nil {
		t.Fatal("accepted conflicting flags")
	}
}
