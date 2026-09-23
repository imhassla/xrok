package cmd

import (
	"errors"
	"net"
	"testing"
	"time"
)

func TestEgressPolicyBlocksInternalDestinations(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.8.9.10", "::1", "10.1.2.3", "172.25.0.2", "192.168.1.1",
		"169.254.169.254", "100.64.0.1", "0.0.0.0", "::", "fe80::1", "fd00::1",
		"224.0.0.1", "::ffff:127.0.0.1", "64:ff9b::7f00:1",
	}
	for _, s := range blocked {
		if !isBlockedEgressIP(net.ParseIP(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111"} {
		if isBlockedEgressIP(net.ParseIP(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
}

func TestDialEgressRefusesLoopbackEvenViaHostname(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	prev := serverAllowPrivateEgress
	defer func() { serverAllowPrivateEgress = prev }()

	serverAllowPrivateEgress = false
	if _, err := dialEgress("tcp", net.JoinHostPort("localhost", port), time.Second); !errors.Is(err, errEgressBlocked) {
		t.Fatalf("expected egress block for localhost, got %v", err)
	}
	serverAllowPrivateEgress = true
	c, err := dialEgress("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("opt-in dial failed: %v", err)
	}
	c.Close()
}

func TestReversePolicy(t *testing.T) {
	prevMin, prevMax, prevCap, prevPort := serverReversePortMin, serverReversePortMax, serverReverseMaxPerCli, serverPort
	defer func() {
		serverReversePortMin, serverReversePortMax, serverReverseMaxPerCli, serverPort = prevMin, prevMax, prevCap, prevPort
	}()
	serverReversePortMin, serverReversePortMax, serverReverseMaxPerCli, serverPort = 9000, 9019, 2, 8443

	for in, want := range map[string]string{"9000": ":9000", ":9019": ":9019", "0.0.0.0:9005": ":9005", "[::]:9001": ":9001"} {
		got, err := normalizeReverseRemote(in)
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"8999", "9020", "22", "5432", "127.0.0.1:9000", "8443", "abc", "1.2.3.4:9001"} {
		if _, err := normalizeReverseRemote(in); err == nil {
			t.Errorf("%q should be rejected", in)
		}
	}
	if _, err := validateReverseConfigs(make([]ServerReverseConfig, 3)); err == nil {
		t.Error("per-client cap not enforced")
	}
}
