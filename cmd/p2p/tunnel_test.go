package p2p

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestP2PManagerCreatesAuthenticatedCapability(t *testing.T) {
	m := NewP2PManager("peer-a", "stun.invalid:3478")
	if m.authToken == "" || len(m.authToken) < 32 {
		t.Fatalf("missing strong P2P capability: %q", m.authToken)
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

func TestP2PTunnelRoleStreamIDsAreDisjoint(t *testing.T) {
	controllingConn, controllingPeer := net.Pipe()
	defer controllingPeer.Close()
	controlledConn, controlledPeer := net.Pipe()
	defer controlledPeer.Close()

	controlling := NewP2PTunnelWithRole(controllingConn, "controlled", "", true)
	defer controlling.Close()
	controlled := NewP2PTunnelWithRole(controlledConn, "controlling", "", false)
	defer controlled.Close()

	even, err := controlling.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	odd, err := controlled.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if even.id%2 != 0 || odd.id%2 != 1 || even.id == odd.id {
		t.Fatalf("role stream IDs collide: controlling=%d controlled=%d", even.id, odd.id)
	}
}
