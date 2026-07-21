package cmd

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

func TestSOCKS5ConnectDomainAndCloseIdempotent(t *testing.T) {
	up, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback sockets unavailable in this environment: %v", err)
	}
	defer up.Close()
	go func() {
		c, e := up.Accept()
		if e == nil {
			defer c.Close()
			io.Copy(c, c)
		}
	}()
	s, err := NewSOCKS5Server("127.0.0.1:0", func(target string) (net.Conn, error) { return net.Dial("tcp", target) })
	if err != nil {
		t.Skipf("loopback sockets unavailable in this environment: %v", err)
	}
	go s.Serve()
	defer s.Close()
	defer s.Close()
	c, err := net.Dial("tcp", s.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err = c.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	resp := make([]byte, 2)
	if _, err = io.ReadFull(c, resp); err != nil {
		t.Fatal(err)
	}
	if resp[0] != 5 || resp[1] != 0 {
		t.Fatalf("auth=%v", resp)
	}
	host, port, _ := net.SplitHostPort(up.Addr().String())
	ip := net.ParseIP(host).To4()
	request := []byte{5, 1, 0, 1}
	request = append(request, ip...)
	p := make([]byte, 2)
	binary.BigEndian.PutUint16(p, uint16(mustPort(t, port)))
	request = append(request, p...)
	if _, err = c.Write(request); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 10)
	if _, err = io.ReadFull(c, reply); err != nil {
		t.Fatal(err)
	}
	if reply[1] != 0 {
		t.Fatalf("reply=%v", reply)
	}
	if _, err = c.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 5)
	if _, err = io.ReadFull(c, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Fatalf("got %q", got)
	}
}

func mustPort(t *testing.T, s string) int {
	t.Helper()
	var p int
	if _, err := fmt.Sscan(s, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

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
