package p2p

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

type fakeTURNServer struct {
	conn        net.Conn
	realm       string
	nonce       string
	username    string
	password    string
	peer        *net.UDPAddr
	relay       *net.UDPAddr
	mu          sync.Mutex
	allocates   int
	permissions int
	refreshes   int
	sends       int
	err         error
	done        chan struct{}
}

func newFakeTURNServer(conn net.Conn, peer *net.UDPAddr) *fakeTURNServer {
	return &fakeTURNServer{
		conn: conn, realm: "xrok.test", nonce: "nonce-1", username: "alice", password: "secret",
		peer: peer, relay: &net.UDPAddr{IP: net.ParseIP("192.0.2.50"), Port: 51000}, done: make(chan struct{}),
	}
}

func (s *fakeTURNServer) run() {
	defer close(s.done)
	defer s.conn.Close()
	key := turnLongTermKey(s.username, s.realm, s.password)
	for {
		message, err := readTURNFrame(s.conn)
		if err != nil {
			if err != io.EOF && !isClosedNetworkError(err) {
				s.fail(err)
			}
			return
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
			binary.BigEndian.PutUint32(lifetime, 60)
			s.write(turnAuthenticatedMessage(turnMethodAllocate, turnClassSuccess, tx, []turnAttribute{
				{typ: turnAttrXORRelayedAddress, value: relay}, {typ: turnAttrLifetime, value: lifetime},
			}, s.username, s.realm, s.nonce, key))

		case method == turnMethodCreatePermission && class == turnClassRequest:
			if !turnVerifyIntegrity(message, key) {
				s.fail(fmt.Errorf("permission request has invalid MESSAGE-INTEGRITY"))
				return
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
	if _, err := s.conn.Write(message); err != nil && !isClosedNetworkError(err) {
		s.fail(err)
	}
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
	allocates, permissions, _, sends, serverErr := fake.stats()
	if serverErr != nil || allocates != 2 || permissions != 1 || sends != 1 {
		t.Fatalf("allocates=%d permissions=%d sends=%d err=%v", allocates, permissions, sends, serverErr)
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
	deadline := time.Now().Add(time.Second)
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

func TestTURNRelayProviderValidation(t *testing.T) {
	valid := TURNConfig{
		Server: "turn.test:3478", Username: "u", Password: "p",
		PeerResolver: func(context.Context, string) (*net.UDPAddr, error) { return nil, nil },
	}
	for name, mutate := range map[string]func(*TURNConfig){
		"server":   func(c *TURNConfig) { c.Server = "" },
		"username": func(c *TURNConfig) { c.Username = "" },
		"password": func(c *TURNConfig) { c.Password = "" },
		"resolver": func(c *TURNConfig) { c.PeerResolver = nil },
		"datagram": func(c *TURNConfig) { c.MaxDatagramSize = turnMaxPacket },
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
	got, err := turnExchangeRetry(ctx, client, request, tx, 80*time.Millisecond, 1)
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
	c := newTURNConn(client, &net.UDPAddr{IP: net.ParseIP("198.51.100.2"), Port: 9}, &net.UDPAddr{IP: net.ParseIP("192.0.2.2"), Port: 50000}, cfg, username, password, oldRealm, oldNonce, oldKey, time.Minute)
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

// Set XROK_COTURN_ADDR (plus optional credentials) to exercise a real coturn
// allocation in the local Docker lab. This remains opt-in so unit tests never
// depend on Docker or an external network.
func TestTURNRelayProviderCoturnIntegration(t *testing.T) {
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
	// Coturn rejects loopback peers by default. A TEST-NET address is sufficient
	// for exercising a real authenticated allocation and permission without
	// depending on an external echo service.
	peer := &net.UDPAddr{IP: net.ParseIP("198.51.100.7"), Port: 9}
	provider, err := NewTURNRelayProvider(TURNConfig{
		Server: server, Username: username, Password: password, Timeout: 3 * time.Second,
		PeerResolver: func(context.Context, string) (*net.UDPAddr, error) { return peer, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, candidate, err := provider.Allocate(ctx, "coturn-smoke")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if candidate.Type != CandidateRelay || net.ParseIP(hostOf(candidate.Addr)) == nil {
		t.Fatalf("invalid coturn relay candidate %#v", candidate)
	}
}

func hostOf(address string) string {
	host, _, _ := net.SplitHostPort(address)
	return host
}
