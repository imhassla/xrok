package p2p

import (
	"encoding/binary"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// Fault tests intentionally use net.Pipe: they are deterministic and do not
// require a socket, NAT, or an external relay.
func TestFaultMalformedFrameIsIgnored(t *testing.T) {
	a, b := net.Pipe()
	tun := NewP2PTunnel(a, "fault-peer", "")
	defer tun.Close()
	stream, err := tun.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	// Length claims a payload larger than the frame. The reader must discard it
	// and keep the tunnel alive rather than panic or deliver partial bytes.
	frame := make([]byte, p2pHeaderSize+1)
	frame[0] = msgTypeData
	binary.BigEndian.PutUint32(frame[1:5], stream.id)
	binary.BigEndian.PutUint32(frame[5:9], 1024)
	go func() { _, _ = b.Write(frame); time.Sleep(10 * time.Millisecond); _ = b.Close() }()
	time.Sleep(20 * time.Millisecond)
	if stream.readBuf.Len() != 0 {
		t.Fatal("malformed payload was delivered")
	}
}

func TestFaultConnectionLossClosesTunnel(t *testing.T) {
	a, b := net.Pipe()
	tun := NewP2PTunnel(a, "fault-peer", "")
	_, err := tun.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	_ = b.Close()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt32(&tun.closed) == 0 {
		time.Sleep(time.Millisecond)
	}
	if atomic.LoadInt32(&tun.closed) == 0 {
		t.Fatal("tunnel did not close after peer disconnect")
	}
	// Shutdown is idempotent after a transport fault.
	tun.Close()
}

func TestFaultWriteAfterCloseReturnsError(t *testing.T) {
	a, b := net.Pipe()
	tun := NewP2PTunnel(a, "fault-peer", "")
	stream, err := tun.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	tun.Close()
	_ = b.Close()
	if _, err := stream.Write([]byte("reconnect")); err == nil {
		t.Fatal("write succeeded on closed tunnel")
	}
}
