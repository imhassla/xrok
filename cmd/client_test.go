package cmd

import (
	"testing"
	"time"
)

func TestTunnelFlag(t *testing.T) {
	var tunnels tunnelFlag
	for _, value := range []string{"web:127.0.0.1:8080", "127.0.0.1:9000"} {
		if err := tunnels.Set(value); err != nil {
			t.Fatal(err)
		}
	}
	if tunnels[0].Name != "web" || tunnels[0].Target != "127.0.0.1:8080" {
		t.Fatalf("unexpected named tunnel: %+v", tunnels[0])
	}
}

func TestUDPAndReverseFlags(t *testing.T) {
	var udp udpFlag
	if err := udp.Set(":5353:127.0.0.1:53"); err != nil {
		t.Fatal(err)
	}
	if udp[0].LocalAddr != ":5353" || udp[0].RemoteAddr != "127.0.0.1:53" {
		t.Fatalf("unexpected UDP config: %+v", udp[0])
	}
	var reverse reverseFlag
	if err := reverse.Set("2222:127.0.0.1:22"); err != nil {
		t.Fatal(err)
	}
	if reverse[0].Remote != ":2222" || reverse[0].Local != "127.0.0.1:22" {
		t.Fatalf("unexpected reverse config: %+v", reverse[0])
	}
}

func TestHeaderFlagRejectsInjection(t *testing.T) {
	headers := headerFlag{}
	if err := headers.Set("X-Test: value"); err != nil {
		t.Fatal(err)
	}
	if headers["X-Test"] != "value" {
		t.Fatalf("unexpected headers: %#v", headers)
	}
	if err := headers.Set("X-Test: ok\r\nInjected: true"); err == nil {
		t.Fatal("expected CRLF injection to be rejected")
	}
}

func TestReconnectBackoffBounds(t *testing.T) {
	cfg := ReconnectConfig{InitialWait: time.Second, MaxWait: 4 * time.Second, Multiplier: 2}
	if got := cfg.Backoff(0); got != time.Second {
		t.Fatalf("initial backoff = %s", got)
	}
	if got := cfg.Backoff(10); got < 4*time.Second || got > 5*time.Second {
		t.Fatalf("capped jittered backoff out of range: %s", got)
	}
}
