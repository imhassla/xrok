package p2p

import (
	"net"
	"testing"
)

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
