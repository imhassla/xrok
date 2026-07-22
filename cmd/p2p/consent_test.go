package p2p

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

type messagePipeConn struct {
	in, out      chan []byte
	closed       chan struct{}
	closeOnce    sync.Once
	mu           sync.Mutex
	readDeadline time.Time
	writeHook    func([]byte, chan []byte) bool
}

func messagePipe() (*messagePipeConn, *messagePipeConn) {
	ab, ba := make(chan []byte, 32), make(chan []byte, 32)
	return &messagePipeConn{in: ba, out: ab, closed: make(chan struct{})}, &messagePipeConn{in: ab, out: ba, closed: make(chan struct{})}
}

func (c *messagePipeConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	deadline := c.readDeadline
	c.mu.Unlock()
	var timer <-chan time.Time
	if !deadline.IsZero() {
		d := time.Until(deadline)
		if d <= 0 {
			return 0, timeoutErr{}
		}
		t := time.NewTimer(d)
		defer t.Stop()
		timer = t.C
	}
	select {
	case msg := <-c.in:
		if len(msg) > len(p) {
			copy(p, msg[:len(p)])
			return len(p), io.ErrShortBuffer
		}
		return copy(p, msg), nil
	case <-timer:
		return 0, timeoutErr{}
	case <-c.closed:
		return 0, net.ErrClosed
	}
}

func (c *messagePipeConn) Write(p []byte) (int, error) {
	copyOfP := append([]byte(nil), p...)
	c.mu.Lock()
	hook := c.writeHook
	c.mu.Unlock()
	if hook != nil && hook(copyOfP, c.out) {
		return len(p), nil
	}
	select {
	case c.out <- copyOfP:
		return len(p), nil
	case <-c.closed:
		return 0, net.ErrClosed
	}
}

func (c *messagePipeConn) Close() error         { c.closeOnce.Do(func() { close(c.closed) }); return nil }
func (c *messagePipeConn) LocalAddr() net.Addr  { return testAddr("local") }
func (c *messagePipeConn) RemoteAddr() net.Addr { return testAddr("remote") }
func (c *messagePipeConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	c.readDeadline = t
	c.mu.Unlock()
	return nil
}
func (c *messagePipeConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.readDeadline = t
	c.mu.Unlock()
	return nil
}
func (c *messagePipeConn) SetWriteDeadline(time.Time) error { return nil }

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

type testAddr string

func (a testAddr) Network() string { return "test" }
func (a testAddr) String() string  { return string(a) }

func TestRuntimeConsentPreservesConcurrentLargePayload(t *testing.T) {
	rawA, rawB := messagePipe()
	a := newDirectionalConsentConn(rawA, []byte("shared-key"), "bound-session", "alice", "bob")
	b := newDirectionalConsentConn(rawB, []byte("shared-key"), "bound-session", "bob", "alice")
	// Exercise the largest legal P2PTunnel frame through authenticated
	// encryption; nonce, tag and record framing still fit one UDP datagram.
	payload := bytes.Repeat([]byte{'x'}, p2pMaxPayload+p2pHeaderSize+1)

	readA := make(chan error, 1)
	go func() {
		buf := make([]byte, maxConsentRecord)
		for {
			n, err := a.Read(buf)
			if err != nil {
				readA <- err
				return
			}
			if n != 0 {
				readA <- errors.New("unexpected application data on consent initiator")
				return
			}
		}
	}()
	received := make(chan []byte, 1)
	stopB := make(chan struct{})
	go func() {
		buf := make([]byte, maxConsentRecord)
		for {
			n, err := b.Read(buf)
			if err != nil {
				return
			}
			if n != 0 {
				select {
				case received <- append([]byte(nil), buf[:n]...):
				default:
				}
			}
			select {
			case <-stopB:
				return
			default:
			}
		}
	}()
	probeDone := make(chan error, 1)
	go func() { probeDone <- (RuntimeConsentProbe{}).Probe(context.Background(), a) }()
	if _, err := a.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := <-probeDone; err != nil {
		t.Fatalf("consent probe failed: %v", err)
	}
	if got := <-received; !bytes.Equal(got, payload) {
		t.Fatalf("payload corrupted: got=%d want=%d", len(got), len(payload))
	}
	close(stopB)
	_ = a.Close()
	_ = b.Close()
	select {
	case <-readA:
	case <-time.After(time.Second):
		t.Fatal("consent read owner did not stop")
	}
}

func TestDirectionalConsentRejectsReflectionAndOldReplay(t *testing.T) {
	rawA, rawB := messagePipe()
	defer rawA.Close()
	defer rawB.Close()
	a := newDirectionalConsentConn(rawA, []byte("shared-key"), "session", "alice", "bob").(*consentConn)
	b := newDirectionalConsentConn(rawB, []byte("shared-key"), "session", "bob", "alice").(*consentConn)
	reflectedAck := a.dataAckPayload(1, make([]byte, consentDataNonceSize))
	if _, ok := a.verifyDataAck(reflectedAck); ok {
		t.Fatal("reflected opposite-direction ACK authenticated")
	}
	first, _, err := a.sealData([]byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.openData(first); err == nil {
		t.Fatal("reflected outbound ciphertext authenticated as inbound data")
	}
	if got, _, err := b.openData(first); err != nil || string(got) != "first" {
		t.Fatalf("peer failed to authenticate first record: %q err=%v", got, err)
	}
	for i := 0; i < 1100; i++ {
		record, _, err := a.sealData([]byte("advance"))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := b.openData(record); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := b.openData(first); !errors.Is(err, errConsentReplay) {
		t.Fatalf("old record accepted after replay window advanced: %v", err)
	}
}

func TestReliableConsentClosesAfterRetryExhaustion(t *testing.T) {
	rawA, rawB := messagePipe()
	a := newDirectionalConsentConn(rawA, []byte("shared-key"), "session", "alice", "bob")
	defer rawB.Close()
	rawA.mu.Lock()
	rawA.writeHook = func(record []byte, _ chan []byte) bool {
		return len(record) >= 9 && record[4] == consentData
	}
	rawA.mu.Unlock()
	started := time.Now()
	if _, err := a.Write([]byte("never-acked")); err == nil {
		t.Fatal("retry exhaustion reported success")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("retry exhaustion was not bounded: %s", elapsed)
	}
	if _, err := a.Write([]byte("after-close")); err == nil {
		t.Fatal("reliability failure did not terminate transport")
	}
}

func TestConsentDataRetransmitsDroppedRecord(t *testing.T) {
	rawA, rawB := messagePipe()
	a := newDirectionalConsentConn(rawA, []byte("shared-key"), "bound-session", "alice", "bob")
	b := newDirectionalConsentConn(rawB, []byte("shared-key"), "bound-session", "bob", "alice")
	dropped := false
	rawA.mu.Lock()
	rawA.writeHook = func(record []byte, _ chan []byte) bool {
		if !dropped && len(record) >= 9 && record[4] == consentData {
			dropped = true
			return true
		}
		return false
	}
	rawA.mu.Unlock()
	stop := make(chan struct{})
	go func() {
		buf := make([]byte, maxConsentRecord)
		for {
			if _, err := a.Read(buf); err != nil {
				return
			}
		}
	}()
	received := make(chan string, 1)
	go func() {
		buf := make([]byte, maxConsentRecord)
		for {
			n, err := b.Read(buf)
			if err != nil {
				return
			}
			if n > 0 {
				received <- string(buf[:n])
				return
			}
			select {
			case <-stop:
				return
			default:
			}
		}
	}()
	if _, err := a.Write([]byte("survives-loss")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-received:
		if got != "survives-loss" || !dropped {
			t.Fatalf("received=%q dropped=%t", got, dropped)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dropped data record was not retransmitted")
	}
	close(stop)
	_ = a.Close()
	_ = b.Close()
}

func TestRuntimeConsentIgnoresStaleReorderedResponse(t *testing.T) {
	rawA, rawB := messagePipe()
	a := newConsentConn(rawA, []byte("shared-key"), "bound-session")
	b := newConsentConn(rawB, []byte("shared-key"), "bound-session")
	var held []byte
	responseCount := 0
	rawB.mu.Lock()
	rawB.writeHook = func(record []byte, out chan []byte) bool {
		if len(record) < 9 || record[4] != consentResponse {
			return false
		}
		responseCount++
		if responseCount == 1 {
			held = append([]byte(nil), record...)
			return true
		}
		if responseCount == 2 {
			out <- append([]byte(nil), held...)
			out <- record
			return true
		}
		return false
	}
	rawB.mu.Unlock()
	stop := make(chan struct{})
	for _, conn := range []net.Conn{a, b} {
		go func(conn net.Conn) {
			buf := make([]byte, maxConsentRecord)
			for {
				select {
				case <-stop:
					return
				default:
					_, _ = conn.Read(buf)
				}
			}
		}(conn)
	}
	firstCtx, firstCancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	if err := (RuntimeConsentProbe{}).Probe(firstCtx, a); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("dropped response unexpectedly accepted: %v", err)
	}
	firstCancel()
	secondCtx, secondCancel := context.WithTimeout(context.Background(), time.Second)
	defer secondCancel()
	if err := (RuntimeConsentProbe{}).Probe(secondCtx, a); err != nil {
		t.Fatalf("fresh response rejected after stale replay: %v", err)
	}
	close(stop)
	_ = a.Close()
	_ = b.Close()
}

func TestConsentDropsLatePunchWithoutClosingTunnelFraming(t *testing.T) {
	rawA, rawB := messagePipe()
	a := newConsentConn(rawA, []byte("shared-key"), "bound-session")
	b := newConsentConn(rawB, []byte("shared-key"), "bound-session")
	late := []byte("XROK-P2P-PUNCH:v2:peer:session:123:nonce:authenticated-mac")
	if _, err := rawA.Write(late); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, maxConsentRecord)
	if n, err := b.Read(buf); err != nil || n != 0 {
		t.Fatalf("late punch reached/closed tunnel: n=%d err=%v", n, err)
	}
	payload := []byte("valid-tunnel-frame-after-late-punch")
	if _, err := a.Write(payload); err != nil {
		t.Fatal(err)
	}
	n, err := b.Read(buf)
	if err != nil || !bytes.Equal(buf[:n], payload) {
		t.Fatalf("tunnel did not recover after late punch: n=%d err=%v", n, err)
	}
	_ = a.Close()
	_ = b.Close()
}

func TestConsentDataRejectsTamperAndDropsReplay(t *testing.T) {
	t.Run("tamper", func(t *testing.T) {
		rawA, rawB := messagePipe()
		a := newConsentConn(rawA, []byte("shared-key"), "bound-session")
		b := newConsentConn(rawB, []byte("shared-key"), "bound-session")
		rawA.mu.Lock()
		rawA.writeHook = func(record []byte, out chan []byte) bool {
			if len(record) >= 10 && record[4] == consentData {
				record[len(record)-1] ^= 0x80
				out <- record
				return true
			}
			return false
		}
		rawA.mu.Unlock()
		if _, err := a.Write([]byte("authenticated payload")); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Read(make([]byte, 128)); err == nil || !strings.Contains(err.Error(), "authenticate P2P data") {
			t.Fatalf("tampered record error=%v", err)
		}
		_ = a.Close()
		_ = b.Close()
	})

	t.Run("replay", func(t *testing.T) {
		rawA, rawB := messagePipe()
		a := newConsentConn(rawA, []byte("shared-key"), "bound-session")
		b := newConsentConn(rawB, []byte("shared-key"), "bound-session")
		rawA.mu.Lock()
		rawA.writeHook = func(record []byte, out chan []byte) bool {
			if len(record) >= 10 && record[4] == consentData {
				out <- append([]byte(nil), record...)
				out <- append([]byte(nil), record...)
				return true
			}
			return false
		}
		rawA.mu.Unlock()
		payload := []byte("one delivery")
		if _, err := a.Write(payload); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 128)
		if n, err := b.Read(buf); err != nil || !bytes.Equal(buf[:n], payload) {
			t.Fatalf("first delivery n=%d err=%v", n, err)
		}
		if n, err := b.Read(buf); err != nil || n != 0 {
			t.Fatalf("replay was not dropped: n=%d err=%v", n, err)
		}
		_ = a.Close()
		_ = b.Close()
	})
}

func TestConsentControlRenewsDispatcherDeadline(t *testing.T) {
	rawA, rawB := messagePipe()
	a := newConsentConn(rawA, []byte("shared-key"), "bound-session")
	b := newConsentConn(rawB, []byte("shared-key"), "bound-session")
	readErr := make(chan error, 2)
	for _, conn := range []net.Conn{a, b} {
		go func(conn net.Conn) {
			buf := make([]byte, maxConsentRecord)
			for i := 0; i < 5; i++ {
				_ = conn.SetReadDeadline(time.Now().Add(35 * time.Millisecond))
				n, err := conn.Read(buf)
				if err != nil || n != 0 {
					readErr <- err
					return
				}
			}
			readErr <- nil
		}(conn)
	}
	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		if err := (RuntimeConsentProbe{}).Probe(ctx, a); err != nil {
			cancel()
			t.Fatal(err)
		}
		cancel()
		time.Sleep(10 * time.Millisecond)
	}
	// The responder consumes five requests and the initiator five responses;
	// neither side may retain the deadline from its first hidden control frame.
	for i := 0; i < 2; i++ {
		if err := <-readErr; err != nil {
			t.Fatalf("dispatcher deadline was not renewed: %v", err)
		}
	}
	_ = a.Close()
	_ = b.Close()
}
