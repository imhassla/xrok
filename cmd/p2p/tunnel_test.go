package p2p

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"net"
	"testing"
	"time"
)

func TestPunchProofDeterministicAndSecret(t *testing.T) {
	a := punchProof("secret", "peer-a")
	b := punchProof("secret", "peer-a")
	if a != b || len(a) != 64 {
		t.Fatalf("unexpected proof: %q %q", a, b)
	}
	h := hmac.New(sha256.New, []byte("secret"))
	h.Write([]byte("peer-a"))
	if !hmac.Equal([]byte(a), []byte(fmt.Sprintf("%x", h.Sum(nil)))) {
		t.Fatal("proof is not HMAC-SHA256")
	}
	if punchProof("other", "peer-a") == a {
		t.Fatal("proof reused across tokens")
	}
}

func TestP2PManagerCreatesAuthenticatedCapability(t *testing.T) {
	m := NewP2PManager("peer-a", "stun.invalid:3478")
	if m.authToken == "" || len(m.authToken) < 32 {
		t.Fatalf("missing strong P2P capability: %q", m.authToken)
	}
	if !validPunchMessage(string(punchMessage(m.authToken, "peer-b")), m.authToken, "peer-b") {
		t.Fatal("valid authenticated punch rejected")
	}
}

func TestPunchMessageFailsClosed(t *testing.T) {
	valid := string(punchMessage("target-capability", "peer-a"))
	tests := []struct {
		name, message, token, peer string
	}{
		{"legacy", "XROK-P2P-PUNCH:peer-a", "target-capability", "peer-a"},
		{"empty token", valid, "", "peer-a"},
		{"wrong token", valid, "other-capability", "peer-a"},
		{"wrong peer", valid, "target-capability", "peer-b"},
		{"extra field", valid + ":extra", "target-capability", "peer-a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if validPunchMessage(tt.message, tt.token, tt.peer) {
				t.Fatal("unauthenticated punch accepted")
			}
		})
	}
}

func TestPunchHoleRequiresPeerCapability(t *testing.T) {
	m := NewP2PManagerWithToken("local", "stun.invalid:3478", "local-capability")
	if _, err := m.PunchHole(t.Context(), &PeerInfo{PeerID: "remote", PublicAddr: "127.0.0.1:9"}); err == nil {
		t.Fatal("missing peer capability accepted")
	}
}

func TestPunchHoleHonorsCancelledContext(t *testing.T) {
	m := NewP2PManagerWithToken("local", "stun.invalid:3478", "local-capability")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := m.PunchHole(ctx, &PeerInfo{PeerID: "remote", PublicAddr: "127.0.0.1:9", AuthToken: "remote-capability"})
	if err == nil {
		t.Fatal("cancelled hole punch succeeded")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cancelled hole punch blocked for %v", elapsed)
	}
}

func BenchmarkPunchProof(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = punchProof("benchmark-session-token", "peer-benchmark")
	}
}

func TestP2PFrameRejectsOversizedPayload(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	tun := NewP2PTunnel(a, "peer", "")
	stream, err := tun.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	// Exercise the framing guard directly; callers using Write are fragmented.
	if err := stream.writeFrame(msgTypeData, make([]byte, p2pMaxPayload)); err == nil {
		t.Fatal("expected oversized frame to be rejected")
	}
	tun.Close()
}

func TestP2PWriteFragmentsPayload(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	tun := NewP2PTunnel(a, "peer", "")
	stream, err := tun.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 2*(p2pMaxPayload-p2pHeaderSize-1)+7)
	readDone := make(chan int)
	go func() {
		n := 0
		buf := make([]byte, p2pMaxPayload+16)
		for n < len(payload)+2*(p2pHeaderSize+1) {
			m, err := b.Read(buf)
			if err != nil {
				break
			}
			n += m
		}
		readDone <- n
	}()
	if n, err := stream.Write(payload); err != nil || n != len(payload) {
		t.Fatalf("Write=%d err=%v", n, err)
	}
	if got := <-readDone; got <= len(payload) {
		t.Fatalf("expected framed bytes, got %d", got)
	}
	tun.Close()
}
