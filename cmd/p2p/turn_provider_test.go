package p2p

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

type fakeTURNServer struct {
	conn               net.Conn
	wire               *turnWire
	realm              string
	nonce              string
	username           string
	password           string
	peer               *net.UDPAddr
	relay              *net.UDPAddr
	mu                 sync.Mutex
	allocates          int
	permissions        int
	refreshes          int
	sends              int
	channels           int
	channel            uint16
	sendWrongChannel   bool
	rejectChannel      bool
	stalePermission    bool
	staleSent          bool
	allocationLifetime uint32
	err                error
	done               chan struct{}
}

type blockingTURNWriteConn struct {
	entered   chan struct{}
	closed    chan struct{}
	enterOnce sync.Once
	closeOnce sync.Once
}

func (c *blockingTURNWriteConn) Read([]byte) (int, error) { <-c.closed; return 0, net.ErrClosed }
func (c *blockingTURNWriteConn) Write([]byte) (int, error) {
	c.enterOnce.Do(func() { close(c.entered) })
	<-c.closed
	return 0, net.ErrClosed
}
func (c *blockingTURNWriteConn) Close() error                     { c.closeOnce.Do(func() { close(c.closed) }); return nil }
func (c *blockingTURNWriteConn) LocalAddr() net.Addr              { return testAddr("local") }
func (c *blockingTURNWriteConn) RemoteAddr() net.Addr             { return testAddr("remote") }
func (c *blockingTURNWriteConn) SetDeadline(time.Time) error      { return nil }
func (c *blockingTURNWriteConn) SetReadDeadline(time.Time) error  { return nil }
func (c *blockingTURNWriteConn) SetWriteDeadline(time.Time) error { return nil }

func newFakeTURNServer(conn net.Conn, peer *net.UDPAddr) *fakeTURNServer {
	return &fakeTURNServer{
		conn: conn, wire: &turnWire{conn: conn}, realm: "xrok.test", nonce: "nonce-1", username: "alice", password: "secret",
		peer: peer, relay: &net.UDPAddr{IP: net.ParseIP("192.0.2.50"), Port: 51000}, allocationLifetime: 60, done: make(chan struct{}),
	}
}

func newFakeTURNStreamServer(conn net.Conn, peer *net.UDPAddr) *fakeTURNServer {
	s := newFakeTURNServer(conn, peer)
	s.wire.stream = true
	return s
}

func (s *fakeTURNServer) run() {
	defer close(s.done)
	defer s.conn.Close()
	key := turnLongTermKey(s.username, s.realm, s.password)
	for {
		message, err := s.wire.read()
		if err != nil {
			if err != io.EOF && !isClosedNetworkError(err) {
				s.fail(err)
			}
			return
		}
		if len(message) >= 4 && binary.BigEndian.Uint16(message[:2])&0xc000 == 0x4000 {
			channel := binary.BigEndian.Uint16(message[:2])
			if channel != s.channel {
				s.fail(fmt.Errorf("unexpected ChannelData channel %#x", channel))
				return
			}
			payload := append([]byte(nil), message[4:]...)
			s.mu.Lock()
			s.channels++
			wrongChannel := s.sendWrongChannel
			s.mu.Unlock()
			if wrongChannel {
				s.write(channelFrame(channel+1, []byte("wrong-channel")))
			}
			s.write(channelFrame(channel, append([]byte("echo:"), payload...)))
			continue
		}
		method, class := turnDecodeType(binary.BigEndian.Uint16(message[:2]))
		var tx [12]byte
		copy(tx[:], message[8:20])
		switch {
		case method == turnMethodAllocate && class == turnClassRequest:
			s.mu.Lock()
			s.allocates++
			attempt := s.allocates
			s.mu.Unlock()
			if attempt == 1 {
				errorCode := []byte{0, 0, 4, 1}
				s.write(turnMessage(turnMethodAllocate, turnClassError, tx, []turnAttribute{
					{typ: turnAttrErrorCode, value: errorCode},
					{typ: turnAttrRealm, value: []byte(s.realm)},
					{typ: turnAttrNonce, value: []byte(s.nonce)},
				}))
				continue
			}
			if !turnVerifyIntegrity(message, key) {
				s.fail(fmt.Errorf("allocate request has invalid MESSAGE-INTEGRITY"))
				return
			}
			relay, _ := turnEncodeXORAddress(s.relay, tx)
			lifetime := make([]byte, 4)
			binary.BigEndian.PutUint32(lifetime, s.allocationLifetime)
			s.write(turnAuthenticatedMessage(turnMethodAllocate, turnClassSuccess, tx, []turnAttribute{
				{typ: turnAttrXORRelayedAddress, value: relay}, {typ: turnAttrLifetime, value: lifetime},
			}, s.username, s.realm, s.nonce, key))

		case method == turnMethodCreatePermission && class == turnClassRequest:
			if !turnVerifyIntegrity(message, key) {
				s.fail(fmt.Errorf("permission request has invalid MESSAGE-INTEGRITY"))
				return
			}
			s.mu.Lock()
			stale := s.stalePermission && !s.staleSent
			if stale {
				s.staleSent = true
			}
			s.mu.Unlock()
			if stale {
				oldKey := key
				s.realm, s.nonce = "xrok.rotated", "nonce-2"
				key = turnLongTermKey(s.username, s.realm, s.password)
				s.write(turnIntegrityMessage(turnMethodCreatePermission, turnClassError, tx, []turnAttribute{
					{typ: turnAttrErrorCode, value: []byte{0, 0, 4, 38}},
					{typ: turnAttrRealm, value: []byte(s.realm)}, {typ: turnAttrNonce, value: []byte(s.nonce)},
				}, oldKey))
				continue
			}
			gotPeer, err := turnAddressAttribute(message, turnAttrXORPeerAddress)
			if err != nil || !sameUDPAddr(gotPeer, s.peer) {
				s.fail(fmt.Errorf("permission peer=%v err=%v", gotPeer, err))
				return
			}
			s.mu.Lock()
			s.permissions++
			s.mu.Unlock()
			s.write(turnAuthenticatedMessage(turnMethodCreatePermission, turnClassSuccess, tx, nil, s.username, s.realm, s.nonce, key))

		case method == turnMethodChannelBind && class == turnClassRequest:
			if !turnVerifyIntegrity(message, key) {
				s.fail(fmt.Errorf("channel bind request has invalid MESSAGE-INTEGRITY"))
				return
			}
			gotPeer, err := turnAddressAttribute(message, turnAttrXORPeerAddress)
			var channel uint16
			_ = turnAttributes(message, func(typ uint16, value []byte, _ int) bool {
				if typ == turnAttrChannelNumber && len(value) == 4 {
					channel = binary.BigEndian.Uint16(value)
				}
				return true
			})
			if err != nil || !sameUDPAddr(gotPeer, s.peer) || channel < 0x4000 || channel > 0x7fff {
				s.fail(fmt.Errorf("channel bind peer=%v channel=%#x err=%v", gotPeer, channel, err))
				return
			}
			s.mu.Lock()
			reject := s.rejectChannel
			s.channel = channel
			s.mu.Unlock()
			if reject {
				s.write(turnIntegrityMessage(turnMethodChannelBind, turnClassError, tx, []turnAttribute{
					{typ: turnAttrErrorCode, value: []byte{0, 0, 4, 0}},
				}, key))
				continue
			}
			s.write(turnAuthenticatedMessage(turnMethodChannelBind, turnClassSuccess, tx, nil, s.username, s.realm, s.nonce, key))

		case method == turnMethodRefresh && class == turnClassRequest:
			if !turnVerifyIntegrity(message, key) {
				s.fail(fmt.Errorf("refresh request has invalid MESSAGE-INTEGRITY"))
				return
			}
			s.mu.Lock()
			s.refreshes++
			s.mu.Unlock()
			s.write(turnAuthenticatedMessage(turnMethodRefresh, turnClassSuccess, tx, nil, s.username, s.realm, s.nonce, key))

		case method == turnMethodSend && class == turnClassIndication:
			gotPeer, err := turnAddressAttribute(message, turnAttrXORPeerAddress)
			if err != nil || !sameUDPAddr(gotPeer, s.peer) {
				s.fail(fmt.Errorf("send peer=%v err=%v", gotPeer, err))
				return
			}
			var data []byte
			_ = turnAttributes(message, func(typ uint16, value []byte, _ int) bool {
				if typ == turnAttrData {
					data = append([]byte(nil), value...)
				}
				return true
			})
			s.mu.Lock()
			s.sends++
			s.mu.Unlock()
			peer, _ := turnEncodeXORAddress(s.peer, tx)
			s.write(turnMessage(turnMethodData, turnClassIndication, tx, []turnAttribute{
				{typ: turnAttrXORPeerAddress, value: peer}, {typ: turnAttrData, value: append([]byte("echo:"), data...)},
			}))
		default:
			s.fail(fmt.Errorf("unexpected TURN message method=%#x class=%d", method, class))
			return
		}
	}
}

func (s *fakeTURNServer) write(message []byte) {
	if err := s.wire.write(message); err != nil && !isClosedNetworkError(err) {
		s.fail(err)
	}
}

func channelFrame(channel uint16, payload []byte) []byte {
	frame := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint16(frame, channel)
	binary.BigEndian.PutUint16(frame[2:], uint16(len(payload)))
	copy(frame[4:], payload)
	return frame
}

func turnIntegrityMessage(method, class uint16, tx [12]byte, attrs []turnAttribute, key []byte) []byte {
	message := turnMessage(method, class, tx, attrs)
	binary.BigEndian.PutUint16(message[2:], uint16(len(message)-turnHeaderSize+24))
	mac := hmac.New(sha1.New, key)
	_, _ = mac.Write(message)
	return turnAppendAttribute(message, turnAttrMessageIntegrity, mac.Sum(nil))
}

func (s *fakeTURNServer) fail(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
}

func (s *fakeTURNServer) stats() (int, int, int, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.allocates, s.permissions, s.refreshes, s.sends, s.err
}

func readTURNFrame(r io.Reader) ([]byte, error) {
	header := make([]byte, turnHeaderSize)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	length := int(binary.BigEndian.Uint16(header[2:4]))
	if length > turnMaxPacket-turnHeaderSize || length%4 != 0 {
		return nil, fmt.Errorf("invalid fake TURN frame length %d", length)
	}
	message := append([]byte(nil), header...)
	message = append(message, make([]byte, length)...)
	_, err := io.ReadFull(r, message[turnHeaderSize:])
	return message, err
}

func sameUDPAddr(a, b *net.UDPAddr) bool {
	return a != nil && b != nil && a.Port == b.Port && a.IP.Equal(b.IP)
}

func isClosedNetworkError(err error) bool {
	return err == net.ErrClosed || (err != nil && (err.Error() == "io: read/write on closed pipe" || err.Error() == "use of closed network connection"))
}

func TestTURNRelayProviderAllocationPermissionAndData(t *testing.T) {
	client, server := net.Pipe()
	peer := &net.UDPAddr{IP: net.ParseIP("198.51.100.7"), Port: 4242}
	fake := newFakeTURNServer(server, peer)
	go fake.run()

	provider, err := NewTURNRelayProvider(TURNConfig{
		Server: "turn.test:3478", Username: "alice", Password: "secret", Timeout: time.Second,
		PeerResolver: func(context.Context, string) (*net.UDPAddr, error) { return peer, nil },
		Dial:         func(context.Context, string, string) (net.Conn, error) { return client, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	conn, candidate, err := provider.Allocate(context.Background(), "peer-a")
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Type != CandidateRelay || candidate.Addr != "192.0.2.50:51000" {
		t.Fatalf("candidate=%#v", candidate)
	}
	if !sameUDPAddr(conn.LocalAddr().(*net.UDPAddr), fake.relay) || !sameUDPAddr(conn.RemoteAddr().(*net.UDPAddr), peer) {
		t.Fatalf("addresses local=%v remote=%v", conn.LocalAddr(), conn.RemoteAddr())
	}
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buf[:n]); got != "echo:hello" {
		t.Fatalf("read=%q", got)
	}
	_ = conn.Close()
	<-fake.done
	allocates, permissions, refreshes, sends, serverErr := fake.stats()
	if serverErr != nil || allocates != 2 || permissions != 1 || refreshes != 1 || sends != 1 {
		t.Fatalf("allocates=%d permissions=%d refreshes=%d sends=%d err=%v", allocates, permissions, refreshes, sends, serverErr)
	}
}

func TestTURNRelayProviderRefreshesAllocationAndPermission(t *testing.T) {
	client, server := net.Pipe()
	peer := &net.UDPAddr{IP: net.ParseIP("203.0.113.4"), Port: 9000}
	fake := newFakeTURNServer(server, peer)
	go fake.run()
	provider, err := NewTURNRelayProvider(TURNConfig{
		Server: "turn.test:3478", Username: "alice", Password: "secret", Timeout: time.Second, RefreshInterval: 15 * time.Millisecond,
		PeerResolver: func(context.Context, string) (*net.UDPAddr, error) { return peer, nil },
		Dial:         func(context.Context, string, string) (net.Conn, error) { return client, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	conn, _, err := provider.Allocate(context.Background(), "peer-b")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2500 * time.Millisecond)
	for time.Now().Before(deadline) {
		_, permissions, refreshes, _, serverErr := fake.stats()
		if serverErr != nil {
			t.Fatal(serverErr)
		}
		if refreshes >= 1 && permissions >= 2 {
			_ = conn.Close()
			<-fake.done
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	_ = conn.Close()
	t.Fatal("TURN allocation was not refreshed")
}

func TestTURNAllocateCandidateDefersPermissionUntilConnect(t *testing.T) {
	client, server := net.Pipe()
	peer := &net.UDPAddr{IP: net.ParseIP("198.51.100.90"), Port: 7400}
	fake := newFakeTURNServer(server, peer)
	go fake.run()
	provider, err := NewTURNRelayProvider(TURNConfig{
		Server: "turn.test:3478", Username: "alice", Password: "secret", Timeout: time.Second,
		Dial: func(context.Context, string, string) (net.Conn, error) { return client, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	pending, candidate, err := provider.AllocateCandidate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, permissions, _, _, serverErr := fake.stats()
	if serverErr != nil || permissions != 0 || candidate.Type != CandidateRelay {
		t.Fatalf("candidate=%#v permissions=%d err=%v", candidate, permissions, serverErr)
	}
	conn, err := pending.Connect(context.Background(), peer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pending.Connect(context.Background(), peer); err == nil {
		t.Fatal("pending TURN allocation connected twice")
	}
	if err := pending.Close(); err != nil {
		t.Fatalf("Close after ownership transfer: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	<-fake.done
	_, permissions, refreshes, _, serverErr := fake.stats()
	if serverErr != nil || permissions != 1 || refreshes != 1 {
		t.Fatalf("permissions=%d deallocations=%d err=%v", permissions, refreshes, serverErr)
	}
}

func TestTURNPendingAllocationCloseBeforeConnectDeallocates(t *testing.T) {
	client, server := net.Pipe()
	peer := &net.UDPAddr{IP: net.ParseIP("198.51.100.91"), Port: 7401}
	fake := newFakeTURNServer(server, peer)
	go fake.run()
	provider, err := NewTURNRelayProvider(TURNConfig{
		Server: "turn.test:3478", Username: "alice", Password: "secret", Timeout: time.Second,
		Dial: func(context.Context, string, string) (net.Conn, error) { return client, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	pending, _, err := provider.AllocateCandidate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := pending.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pending.Close(); err != nil {
		t.Fatalf("idempotent close: %v", err)
	}
	<-fake.done
	_, permissions, refreshes, _, serverErr := fake.stats()
	if serverErr != nil || permissions != 0 || refreshes != 1 {
		t.Fatalf("permissions=%d deallocations=%d err=%v", permissions, refreshes, serverErr)
	}
	if _, err := pending.Connect(context.Background(), peer); err == nil {
		t.Fatal("closed pending allocation connected")
	}
}

func TestTURNSafeRefreshIntervalAndPayloadDefault(t *testing.T) {
	provider, err := NewTURNRelayProvider(TURNConfig{Server: "turn.test:3478", Username: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if provider.cfg.MaxDatagramSize < 65046 {
		t.Fatalf("default max datagram=%d", provider.cfg.MaxDatagramSize)
	}
	if got := safeTURNRefreshInterval(time.Hour, 2*time.Minute); got != time.Minute {
		t.Fatalf("long refresh interval=%s", got)
	}
	if got := safeTURNRefreshInterval(time.Millisecond, 10*time.Minute); got != time.Second {
		t.Fatalf("short refresh interval=%s", got)
	}
	if got := safeTURNRefreshInterval(0, 20*time.Minute); got != 4*time.Minute {
		t.Fatalf("default refresh interval=%s", got)
	}
}

func TestTURNIntegrityBoundaryIgnoresTrailingAttributes(t *testing.T) {
	key := turnLongTermKey("user", "realm", "password")
	var tx [12]byte
	message := turnAuthenticatedMessage(turnMethodRefresh, turnClassSuccess, tx, nil, "user", "realm", "nonce", key)
	message = turnAppendAttribute(message, turnAttrLifetime, []byte{0, 0, 0, 1})
	binary.BigEndian.PutUint16(message[2:4], uint16(len(message)-turnHeaderSize))
	if !turnVerifyIntegrity(message, key) {
		t.Fatal("valid integrity prefix rejected")
	}
	if got := turnLifetime(message); got != 10*time.Minute {
		t.Fatalf("unauthenticated trailing lifetime accepted: %s", got)
	}
}

func TestTURNRefreshReturnsToRequestedPolicyAfterShortLifetime(t *testing.T) {
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()
	c := newTURNConn(&turnWire{conn: local}, &net.UDPAddr{}, &net.UDPAddr{}, TURNConfig{}, "u", "p", "r", "n", nil, 2*time.Second)
	if c.cfg.RefreshInterval != time.Second {
		t.Fatalf("initial refresh=%s", c.cfg.RefreshInterval)
	}
	if got := safeTURNRefreshInterval(c.requestedRefreshInterval, 10*time.Minute); got != 4*time.Minute {
		t.Fatalf("updated lifetime retained short refresh flood: %s", got)
	}
	if got := turnPendingAllocationTTL(10 * time.Minute); got != 30*time.Second {
		t.Fatalf("pending allocation ttl=%s", got)
	}
}

func TestTURNCloseInterruptsConcurrentBlockedWriter(t *testing.T) {
	raw := &blockingTURNWriteConn{entered: make(chan struct{}), closed: make(chan struct{})}
	c := newTURNConn(&turnWire{conn: raw, stream: true}, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}, &net.UDPAddr{}, TURNConfig{Timeout: time.Second, MaxDatagramSize: 1024}, "u", "p", "r", "n", []byte("key"), time.Minute)
	c.channel, c.channelBound = 0x4001, true
	writeDone := make(chan struct{})
	go func() { _, _ = c.Write([]byte("blocked")); close(writeDone) }()
	<-raw.entered
	started := time.Now()
	_ = c.Close()
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("Close remained blocked behind writer for %s", elapsed)
	}
	select {
	case <-writeDone:
	case <-time.After(time.Second):
		t.Fatal("blocked writer was not interrupted")
	}
}

func TestTURNAllocateCandidateRejectsExpiredLifetime(t *testing.T) {
	client, server := net.Pipe()
	peer := &net.UDPAddr{IP: net.ParseIP("198.51.100.92"), Port: 7402}
	fake := newFakeTURNServer(server, peer)
	fake.allocationLifetime = 0
	go fake.run()
	provider, err := NewTURNRelayProvider(TURNConfig{
		Server: "turn.test:3478", Username: "alice", Password: "secret", Timeout: time.Second,
		Dial: func(context.Context, string, string) (net.Conn, error) { return client, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := provider.AllocateCandidate(context.Background()); err == nil {
		t.Fatal("expired TURN allocation accepted")
	}
	<-fake.done
}

func TestTURNRelayProviderTCPChannelDataRejectsUnexpectedChannel(t *testing.T) {
	client, server := net.Pipe()
	peer := &net.UDPAddr{IP: net.ParseIP("198.51.100.33"), Port: 7000}
	fake := newFakeTURNStreamServer(server, peer)
	fake.sendWrongChannel = true
	go fake.run()
	provider, err := NewTURNRelayProvider(TURNConfig{
		Server: "turn.test:3478", Username: "alice", Password: "secret", Timeout: time.Second,
		Transport: TURNTransportTCP, EnableChannelBind: true, ChannelNumber: 0x4123,
		PeerResolver: func(context.Context, string) (*net.UDPAddr, error) { return peer, nil },
		Dial:         func(context.Context, string, string) (net.Conn, error) { return client, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	conn, _, err := provider.Allocate(context.Background(), "peer-channel")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("channel")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buf[:n]); got != "echo:channel" {
		t.Fatalf("unexpected or invalid ChannelData accepted: %q", got)
	}
	_ = conn.Close()
	<-fake.done
	fake.mu.Lock()
	channels, serverErr := fake.channels, fake.err
	fake.mu.Unlock()
	if serverErr != nil || channels != 1 {
		t.Fatalf("channels=%d err=%v", channels, serverErr)
	}
}

func TestTURNRelayProviderChannelBindFallsBackToIndications(t *testing.T) {
	client, server := net.Pipe()
	peer := &net.UDPAddr{IP: net.ParseIP("198.51.100.44"), Port: 7100}
	fake := newFakeTURNStreamServer(server, peer)
	fake.rejectChannel = true
	go fake.run()
	provider, err := NewTURNRelayProvider(TURNConfig{
		Server: "turn.test:3478", Username: "alice", Password: "secret", Timeout: time.Second,
		Transport: TURNTransportTCP, EnableChannelBind: true, ChannelNumber: 0x4333,
		PeerResolver: func(context.Context, string) (*net.UDPAddr, error) { return peer, nil },
		Dial:         func(context.Context, string, string) (net.Conn, error) { return client, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	conn, _, err := provider.Allocate(context.Background(), "peer-fallback")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("fallback")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil || string(buf[:n]) != "echo:fallback" {
		t.Fatalf("indication fallback read=%q err=%v", buf[:n], err)
	}
	_ = conn.Close()
	<-fake.done
	_, _, _, sends, serverErr := fake.stats()
	if serverErr != nil || sends != 1 {
		t.Fatalf("fallback sends=%d err=%v", sends, serverErr)
	}
}

func TestTURNRelayProviderRetriesStaleNonceForPermission(t *testing.T) {
	client, server := net.Pipe()
	peer := &net.UDPAddr{IP: net.ParseIP("203.0.113.88"), Port: 7200}
	fake := newFakeTURNServer(server, peer)
	fake.stalePermission = true
	go fake.run()
	provider, err := NewTURNRelayProvider(TURNConfig{
		Server: "turn.test:3478", Username: "alice", Password: "secret", Timeout: time.Second,
		PeerResolver: func(context.Context, string) (*net.UDPAddr, error) { return peer, nil },
		Dial:         func(context.Context, string, string) (net.Conn, error) { return client, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	conn, _, err := provider.Allocate(context.Background(), "peer-stale")
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	<-fake.done
	_, permissions, _, _, serverErr := fake.stats()
	if serverErr != nil || permissions != 1 || !fake.staleSent {
		t.Fatalf("permissions=%d stale=%v err=%v", permissions, fake.staleSent, serverErr)
	}
}

func TestTURNStreamWireSplitAndCoalescedFrames(t *testing.T) {
	client, server := net.Pipe()
	wire := &turnWire{conn: client, stream: true}
	tx, _ := turnTransactionID()
	stun := turnMessage(turnMethodData, turnClassIndication, tx, []turnAttribute{{typ: turnAttrData, value: []byte("one")}})
	channel := channelFrame(0x4001, []byte("two"))
	channelPadded := append(append([]byte(nil), channel...), 0)
	go func() {
		defer server.Close()
		// Force a split STUN header, then coalesce its tail and the next frame.
		_, _ = server.Write(stun[:3])
		_, _ = server.Write(append(append([]byte(nil), stun[3:]...), channelPadded...))
	}()
	first, err := wire.read()
	if err != nil {
		t.Fatal(err)
	}
	second, err := wire.read()
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(stun) || string(second) != string(channel) {
		t.Fatalf("stream frames were not preserved: first=%d second=%q", len(first), second)
	}
	_ = client.Close()
}

func TestTURNRelayProviderTLSVerifiedTransport(t *testing.T) {
	certificate, roots := testTURNCertificate(t, "turn.test")
	rawClient, rawServer := net.Pipe()
	serverTLS := tls.Server(rawServer, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
	peer := &net.UDPAddr{IP: net.ParseIP("198.51.100.66"), Port: 7300}
	fake := newFakeTURNStreamServer(serverTLS, peer)
	serverReady := make(chan error, 1)
	go func() {
		if err := serverTLS.Handshake(); err != nil {
			serverReady <- err
			return
		}
		serverReady <- nil
		fake.run()
	}()
	provider, err := NewTURNRelayProvider(TURNConfig{
		Server: "turn.test:5349", Username: "alice", Password: "secret", Timeout: time.Second,
		Transport: TURNTransportTLS, TLSConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		PeerResolver: func(context.Context, string) (*net.UDPAddr, error) { return peer, nil },
		Dial:         func(context.Context, string, string) (net.Conn, error) { return rawClient, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	conn, candidate, err := provider.Allocate(context.Background(), "peer-tls")
	if err != nil {
		t.Fatal(err)
	}
	if err := <-serverReady; err != nil {
		t.Fatal(err)
	}
	if candidate.Type != CandidateRelay {
		t.Fatalf("candidate=%#v", candidate)
	}
	_ = conn.Close()
	<-fake.done
}

func TestTURNRelayProviderTLSRejectsUntrustedServerByDefault(t *testing.T) {
	certificate, _ := testTURNCertificate(t, "turn.test")
	rawClient, rawServer := net.Pipe()
	serverTLS := tls.Server(rawServer, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- serverTLS.Handshake()
		_ = serverTLS.Close()
	}()
	peer := &net.UDPAddr{IP: net.ParseIP("198.51.100.67"), Port: 7301}
	provider, err := NewTURNRelayProvider(TURNConfig{
		Server: "turn.test:5349", Username: "alice", Password: "secret", Timeout: time.Second,
		Transport:    TURNTransportTLS,
		PeerResolver: func(context.Context, string) (*net.UDPAddr, error) { return peer, nil },
		Dial:         func(context.Context, string, string) (net.Conn, error) { return rawClient, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := provider.Allocate(context.Background(), "peer-untrusted"); err == nil {
		t.Fatal("untrusted TURN TLS certificate was accepted")
	}
	if err := <-serverDone; err == nil {
		t.Fatal("server TLS handshake unexpectedly succeeded")
	}
}

func testTURNCertificate(t *testing.T, dnsName string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: dnsName}, DNSNames: []string{dnsName},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyBytes, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certPEM) {
		t.Fatal("append TURN test root")
	}
	return certificate, roots
}

func TestTURNRelayProviderValidation(t *testing.T) {
	valid := TURNConfig{
		Server: "turn.test:3478", Username: "u", Password: "p",
		PeerResolver: func(context.Context, string) (*net.UDPAddr, error) { return nil, nil },
	}
	for name, mutate := range map[string]func(*TURNConfig){
		"server":      func(c *TURNConfig) { c.Server = "" },
		"server_port": func(c *TURNConfig) { c.Server = "turn.test" },
		"bad_port":    func(c *TURNConfig) { c.Server = "turn.test:70000" },
		"username":    func(c *TURNConfig) { c.Username = "" },
		"password":    func(c *TURNConfig) { c.Password = "" },
		"datagram":    func(c *TURNConfig) { c.MaxDatagramSize = turnMaxPacket },
		"transport":   func(c *TURNConfig) { c.Transport = "sctp" },
		"channel":     func(c *TURNConfig) { c.ChannelNumber = 0x2000 },
		"retransmits": func(c *TURNConfig) { c.Retransmits = 9 },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := valid
			mutate(&cfg)
			if _, err := NewTURNRelayProvider(cfg); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestTURNExchangeRetransmitsDroppedRequest(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	var tx [12]byte
	copy(tx[:], []byte("retransmit01"))
	request := turnMessage(turnMethodAllocate, turnClassRequest, tx, nil)
	response := turnMessage(turnMethodAllocate, turnClassSuccess, tx, nil)
	serverErr := make(chan error, 1)
	go func() {
		first, err := readTURNFrame(server)
		if err == nil && string(first) != string(request) {
			err = fmt.Errorf("first request differs from original")
		}
		if err == nil {
			var second []byte
			second, err = readTURNFrame(server)
			if err == nil && string(second) != string(request) {
				err = fmt.Errorf("retransmission differs from original")
			}
		}
		if err == nil {
			_, err = server.Write(response)
		}
		serverErr <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := turnExchangeRetry(ctx, &turnWire{conn: client}, request, tx, 80*time.Millisecond, 1)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(response) {
		t.Fatal("unexpected response after retransmission")
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestTURNAuthenticatedTransactionRefreshesStaleNonce(t *testing.T) {
	client, server := net.Pipe()
	oldRealm, newRealm := "old.realm", "new.realm"
	oldNonce, newNonce := "old-nonce", "new-nonce"
	username, password := "alice", "secret"
	oldKey := turnLongTermKey(username, oldRealm, password)
	newKey := turnLongTermKey(username, newRealm, password)
	cfg := TURNConfig{Timeout: 200 * time.Millisecond, Retransmits: 1}
	c := newTURNConn(&turnWire{conn: client}, &net.UDPAddr{IP: net.ParseIP("198.51.100.2"), Port: 9}, &net.UDPAddr{IP: net.ParseIP("192.0.2.2"), Port: 50000}, cfg, username, password, oldRealm, oldNonce, oldKey, time.Minute)
	go c.readLoop()
	serverErr := make(chan error, 1)
	go func() {
		first, err := readTURNFrame(server)
		if err != nil {
			serverErr <- err
			return
		}
		var firstTx [12]byte
		copy(firstTx[:], first[8:20])
		stale := turnAuthenticatedMessage(turnMethodRefresh, turnClassError, firstTx,
			[]turnAttribute{{typ: turnAttrErrorCode, value: []byte{0, 0, 4, 38}}},
			username, newRealm, newNonce, oldKey)
		if _, err = server.Write(stale); err != nil {
			serverErr <- err
			return
		}
		second, err := readTURNFrame(server)
		if err != nil {
			serverErr <- err
			return
		}
		if !turnVerifyIntegrity(second, newKey) || turnStringAttribute(second, turnAttrNonce) != newNonce {
			serverErr <- fmt.Errorf("stale nonce retry did not use refreshed credentials")
			return
		}
		var secondTx [12]byte
		copy(secondTx[:], second[8:20])
		_, err = server.Write(turnAuthenticatedMessage(turnMethodRefresh, turnClassSuccess, secondTx, nil, username, newRealm, newNonce, newKey))
		serverErr <- err
	}()
	response, err := c.authTransaction(context.Background(), turnMethodRefresh, func([12]byte) ([]turnAttribute, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, class := turnDecodeType(binary.BigEndian.Uint16(response[:2])); class != turnClassSuccess {
		t.Fatalf("unexpected response class %d", class)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
	c.authMu.RLock()
	gotRealm, gotNonce := c.realm, c.nonce
	c.authMu.RUnlock()
	if gotRealm != newRealm || gotNonce != newNonce {
		t.Fatalf("credentials realm=%q nonce=%q", gotRealm, gotNonce)
	}
	_ = c.Close()
	_ = server.Close()
}

func turnStringAttribute(message []byte, wanted uint16) string {
	var result string
	_ = turnAttributes(message, func(typ uint16, value []byte, _ int) bool {
		if typ == wanted {
			result = string(value)
		}
		return true
	})
	return result
}

// Set XROK_COTURN_ADDR (plus optional credentials) to exercise a bilateral
// relay data path through real coturn. This remains opt-in so unit tests never
// depend on Docker or an external network.
func TestTURNRelayProviderCoturnBilateralDataPathIntegration(t *testing.T) {
	server := os.Getenv("XROK_COTURN_ADDR")
	if server == "" {
		t.Skip("set XROK_COTURN_ADDR to run coturn integration")
	}
	username := os.Getenv("XROK_COTURN_USERNAME")
	password := os.Getenv("XROK_COTURN_PASSWORD")
	if username == "" {
		username = "matrix"
	}
	if password == "" {
		password = "matrix-pass"
	}
	transport := TURNTransport(os.Getenv("XROK_COTURN_TRANSPORT"))
	if transport == "" {
		transport = TURNTransportUDP
	}
	var tlsConfig *tls.Config
	if transport == TURNTransportTLS {
		roots := x509.NewCertPool()
		caPath := os.Getenv("XROK_COTURN_TLS_CA")
		if caPath == "" {
			t.Fatal("XROK_COTURN_TLS_CA is required for verified coturn TLS integration")
		}
		pemBytes, readErr := os.ReadFile(caPath)
		if readErr != nil || !roots.AppendCertsFromPEM(pemBytes) {
			t.Fatalf("load XROK_COTURN_TLS_CA: %v", readErr)
		}
		tlsConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}
	provider, err := NewTURNRelayProvider(TURNConfig{
		Server: server, Username: username, Password: password, Timeout: 3 * time.Second,
		Transport: transport, TLSConfig: tlsConfig, TLSServerName: os.Getenv("XROK_COTURN_TLS_SERVER_NAME"),
		EnableChannelBind: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pendingA, candidateA, err := provider.AllocateCandidate(ctx)
	if err != nil {
		t.Fatalf("allocate candidate A: %v", err)
	}
	defer pendingA.Close()
	pendingB, candidateB, err := provider.AllocateCandidate(ctx)
	if err != nil {
		t.Fatalf("allocate candidate B: %v", err)
	}
	defer pendingB.Close()
	peerA := coturnRelayAddr(t, candidateA)
	peerB := coturnRelayAddr(t, candidateB)
	if sameUDPAddr(peerA, peerB) {
		t.Fatalf("coturn returned duplicate relay candidates A=%v B=%v", peerA, peerB)
	}

	connA, err := pendingA.Connect(ctx, peerB)
	if err != nil {
		t.Fatalf("connect allocation A to B relay: %v", err)
	}
	defer connA.Close()
	connB, err := pendingB.Connect(ctx, peerA)
	if err != nil {
		t.Fatalf("connect allocation B to A relay: %v", err)
	}
	defer connB.Close()
	assertTURNChannelBound(t, "A", connA)
	assertTURNChannelBound(t, "B", connB)

	assertTURNRelayPayload(t, connA, connB, []byte("coturn-A-to-B-"+string(transport)))
	assertTURNRelayPayload(t, connB, connA, []byte("coturn-B-to-A-"+string(transport)))

	closedAt := time.Now()
	if err := connA.Close(); err != nil {
		t.Fatalf("deallocate A: %v", err)
	}
	if err := connB.Close(); err != nil {
		t.Fatalf("deallocate B: %v", err)
	}
	if elapsed := time.Since(closedAt); elapsed > time.Second {
		t.Fatalf("coturn deallocation exceeded bound: %v", elapsed)
	}
	if _, err := connA.Write([]byte("after-close")); err == nil {
		t.Fatal("allocation A remained writable after deallocation")
	}
	if _, err := connB.Write([]byte("after-close")); err == nil {
		t.Fatal("allocation B remained writable after deallocation")
	}
}

func coturnRelayAddr(t *testing.T, candidate Candidate) *net.UDPAddr {
	t.Helper()
	if candidate.Type != CandidateRelay {
		t.Fatalf("invalid coturn relay candidate %#v", candidate)
	}
	address, err := net.ResolveUDPAddr("udp", candidate.Addr)
	if err != nil || address.IP == nil || address.Port < 1 {
		t.Fatalf("resolve coturn relay candidate %#v: %v", candidate, err)
	}
	return address
}

func assertTURNChannelBound(t *testing.T, label string, conn net.Conn) {
	t.Helper()
	turnConnection, ok := conn.(*turnConn)
	if !ok {
		t.Fatalf("allocation %s has unexpected connection type %T", label, conn)
	}
	turnConnection.channelMu.RLock()
	bound := turnConnection.channelBound
	turnConnection.channelMu.RUnlock()
	if !bound {
		t.Fatalf("coturn allocation %s did not complete ChannelBind", label)
	}
}

func assertTURNRelayPayload(t *testing.T, sender, receiver net.Conn, payload []byte) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	if err := sender.SetWriteDeadline(deadline); err != nil {
		t.Fatalf("set TURN write deadline: %v", err)
	}
	if err := receiver.SetReadDeadline(deadline); err != nil {
		t.Fatalf("set TURN read deadline: %v", err)
	}
	n, err := sender.Write(payload)
	if err != nil || n != len(payload) {
		t.Fatalf("write TURN relay payload: n=%d err=%v", n, err)
	}
	buffer := make([]byte, len(payload)+1)
	n, err = receiver.Read(buffer)
	if err != nil {
		t.Fatalf("read TURN relay payload: %v", err)
	}
	if string(buffer[:n]) != string(payload) {
		t.Fatalf("TURN relay payload mismatch: got=%q want=%q", buffer[:n], payload)
	}
}
