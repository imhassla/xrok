package p2p

// This file implements the RFC 5766 UDP-relay subset needed by ICE fallback:
// authenticated Allocate/Refresh/CreatePermission/ChannelBind transactions,
// Send/Data indications, and ChannelData. The control connection can use UDP,
// TCP, or TLS; TURN TCP allocations and RFC 6062 Connect are intentionally out
// of scope. Callers receive a connected net.Conn abstraction to one UDP peer.

import (
	"context"
	"crypto/hmac"
	"crypto/md5" // RFC 5389 long-term credential key derivation requires MD5.
	"crypto/rand"
	"crypto/sha1" // RFC 5389 MESSAGE-INTEGRITY requires HMAC-SHA1.
	"crypto/subtle"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	turnMagicCookie = 0x2112a442
	turnHeaderSize  = 20
	turnMaxPacket   = 64 * 1024

	turnMethodAllocate         = 0x003
	turnMethodRefresh          = 0x004
	turnMethodSend             = 0x006
	turnMethodData             = 0x007
	turnMethodCreatePermission = 0x008
	turnMethodChannelBind      = 0x009

	turnClassRequest    = 0
	turnClassIndication = 1
	turnClassSuccess    = 2
	turnClassError      = 3

	turnAttrUsername           = 0x0006
	turnAttrMessageIntegrity   = 0x0008
	turnAttrErrorCode          = 0x0009
	turnAttrChannelNumber      = 0x000c
	turnAttrLifetime           = 0x000d
	turnAttrXORPeerAddress     = 0x0012
	turnAttrData               = 0x0013
	turnAttrRealm              = 0x0014
	turnAttrNonce              = 0x0015
	turnAttrXORRelayedAddress  = 0x0016
	turnAttrRequestedTransport = 0x0019
)

// TURNPacketDialer allows deterministic protocol tests without a live TURN
// server. It dials the raw UDP or TCP control transport; TLS wrapping and
// verification remain owned by TURNRelayProvider.
type TURNPacketDialer func(context.Context, string, string) (net.Conn, error)

// TURNPeerResolver maps an authenticated signalling peer ID to the peer's UDP
// address. TURN permissions are address-bound, so silently treating peerID as
// an address would cross the signalling/security boundary.
type TURNPeerResolver func(context.Context, string) (*net.UDPAddr, error)

type TURNTransport string

const (
	TURNTransportUDP TURNTransport = "udp"
	TURNTransportTCP TURNTransport = "tcp"
	TURNTransportTLS TURNTransport = "tls"
)

type TURNConfig struct {
	Server        string
	Username      string
	Password      string
	PeerResolver  TURNPeerResolver
	Transport     TURNTransport
	TLSConfig     *tls.Config
	TLSServerName string
	// TLSInsecureSkipVerify is deliberately explicit and should only be used
	// by isolated local labs. TLS verifies certificates by default.
	TLSInsecureSkipVerify bool
	Dial                  TURNPacketDialer
	Timeout               time.Duration
	RefreshInterval       time.Duration
	MaxDatagramSize       int
	// Retransmits is the number of bounded UDP retransmissions after the
	// initial send. TURN transactions are otherwise vulnerable to a single
	// dropped packet. The default is three.
	Retransmits int
	// EnableChannelBind upgrades payloads to ChannelData after a successful
	// bind. A rejected bind transparently retains Send/Data indications.
	EnableChannelBind bool
	// ChannelNumber is primarily a deterministic-test hook. Zero selects a
	// cryptographically random number in RFC 5766's 0x4000-0x7fff range.
	ChannelNumber uint16
}

// TURNRelayProvider is a concrete provider for UDP relay allocations over
// UDP/TCP/TLS control transports. ChannelBind is optional and falls back to
// Send/Data indications; it does not claim TURN TCP allocations.
type TURNRelayProvider struct{ cfg TURNConfig }

// TURNPendingAllocation is an authenticated allocation that has not yet been
// bound to a peer. It is intentionally single-use and contains no read-loop
// goroutine until Connect succeeds.
type TURNPendingAllocation struct {
	mu        sync.Mutex
	wire      *turnWire
	cfg       TURNConfig
	username  string
	password  string
	realm     string
	nonce     string
	key       []byte
	relay     *net.UDPAddr
	lifetime  time.Duration
	connected bool
	closed    bool
	closeErr  error
	timer     *time.Timer
}

// Connect installs the peer permission, optionally binds a channel, and
// transfers socket ownership to the returned connection.
func (a *TURNPendingAllocation) Connect(ctx context.Context, peer *net.UDPAddr) (net.Conn, error) {
	if a == nil {
		return nil, fmt.Errorf("TURN pending allocation is nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("TURN connect context is required")
	}
	if peer == nil || peer.IP == nil || peer.Port < 1 || peer.Port > 65535 {
		return nil, fmt.Errorf("TURN peer UDP address is invalid")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil, net.ErrClosed
	}
	if a.connected || a.wire == nil {
		return nil, fmt.Errorf("TURN pending allocation is already connected")
	}
	if a.timer != nil {
		a.timer.Stop()
	}
	peerCopy := *peer
	peerCopy.IP = append(net.IP(nil), peer.IP...)
	relayCopy := *a.relay
	relayCopy.IP = append(net.IP(nil), a.relay.IP...)
	tc := newTURNConn(a.wire, &peerCopy, &relayCopy, a.cfg, a.username, a.password, a.realm, a.nonce, a.key, a.lifetime)
	if err := tc.start(ctx); err != nil {
		_ = tc.Close()
		a.closed = true
		a.wire = nil
		a.clearCredentials()
		return nil, err
	}
	a.connected = true
	a.wire = nil
	a.clearCredentials()
	return tc, nil
}

// Close deallocates an unconnected allocation with a short authenticated
// Refresh(LIFETIME=0). The socket is always closed even if the server is gone.
func (a *TURNPendingAllocation) Close() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.connected {
		return nil // ownership was transferred to turnConn
	}
	if a.closed {
		return a.closeErr
	}
	a.closed = true
	if a.timer != nil {
		a.timer.Stop()
	}
	if a.wire == nil {
		return nil
	}
	closeTimeout := a.cfg.Timeout
	if closeTimeout <= 0 || closeTimeout > 250*time.Millisecond {
		closeTimeout = 250 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	tx, err := turnTransactionID()
	if err == nil {
		request := turnAuthenticatedMessage(turnMethodRefresh, turnClassRequest, tx,
			[]turnAttribute{{typ: turnAttrLifetime, value: []byte{0, 0, 0, 0}}},
			a.username, a.realm, a.nonce, a.key)
		response, exchangeErr := turnExchangeRetry(ctx, a.wire, request, tx, closeTimeout, a.cfg.Retransmits)
		err = exchangeErr
		if err == nil {
			method, class := turnDecodeType(binary.BigEndian.Uint16(response[:2]))
			if method != turnMethodRefresh || class != turnClassSuccess || !turnVerifyIntegrity(response, a.key) {
				err = fmt.Errorf("TURN pending deallocation response failed authentication")
			}
		}
	}
	a.closeErr = err
	_ = a.wire.conn.Close()
	a.wire = nil
	a.clearCredentials()
	return a.closeErr
}

func (a *TURNPendingAllocation) clearCredentials() {
	for i := range a.key {
		a.key[i] = 0
	}
	a.key = nil
	a.password = ""
}

func NewTURNRelayProvider(cfg TURNConfig) (*TURNRelayProvider, error) {
	if strings.TrimSpace(cfg.Server) == "" {
		return nil, fmt.Errorf("TURN server is required")
	}
	serverHost, serverPort, err := net.SplitHostPort(cfg.Server)
	if err != nil || strings.TrimSpace(serverHost) == "" {
		return nil, fmt.Errorf("TURN server must be a valid host:port: %q", cfg.Server)
	}
	port, err := strconv.Atoi(serverPort)
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("TURN server port is invalid: %q", serverPort)
	}
	if cfg.Username == "" || cfg.Password == "" {
		return nil, fmt.Errorf("TURN long-term username and password are required")
	}
	if cfg.Transport == "" {
		cfg.Transport = TURNTransportUDP
	}
	if cfg.Transport != TURNTransportUDP && cfg.Transport != TURNTransportTCP && cfg.Transport != TURNTransportTLS {
		return nil, fmt.Errorf("unsupported TURN transport %q", cfg.Transport)
	}
	if cfg.Transport == TURNTransportTLS {
		serverName := cfg.TLSServerName
		if serverName == "" && cfg.TLSConfig != nil {
			serverName = cfg.TLSConfig.ServerName
		}
		if serverName == "" {
			serverName = serverHost
		}
		if strings.TrimSpace(serverName) == "" || strings.ContainsAny(serverName, " \t\r\n") {
			return nil, fmt.Errorf("TURN TLS server name is invalid")
		}
	}
	if cfg.ChannelNumber != 0 && (cfg.ChannelNumber < 0x4000 || cfg.ChannelNumber > 0x7fff) {
		return nil, fmt.Errorf("TURN channel number must be between 0x4000 and 0x7fff")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.MaxDatagramSize <= 0 {
		cfg.MaxDatagramSize = 65046
	}
	if cfg.MaxDatagramSize > turnMaxPacket-128 {
		return nil, fmt.Errorf("TURN max datagram size exceeds protocol limit")
	}
	if cfg.Retransmits < 0 {
		return nil, fmt.Errorf("TURN retransmits cannot be negative")
	}
	if cfg.Retransmits == 0 {
		cfg.Retransmits = 3
	}
	if cfg.Retransmits > 8 {
		return nil, fmt.Errorf("TURN retransmits exceeds bounded limit 8")
	}
	if cfg.Dial == nil {
		cfg.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, address)
		}
	}
	return &TURNRelayProvider{cfg: cfg}, nil
}

// Allocate preserves the peer-ID RelayProvider contract through the split
// allocation API. New signalling flows can advertise the relay Candidate from
// AllocateCandidate before binding it to a peer with Connect.
func (p *TURNRelayProvider) Allocate(ctx context.Context, peerID string) (net.Conn, Candidate, error) {
	if ctx == nil {
		return nil, Candidate{}, fmt.Errorf("TURN context is required")
	}
	if strings.TrimSpace(peerID) == "" {
		return nil, Candidate{}, fmt.Errorf("TURN peer ID is required")
	}
	if p.cfg.PeerResolver == nil {
		return nil, Candidate{}, fmt.Errorf("TURN peer resolver is required for peer-ID allocation")
	}
	peer, err := p.cfg.PeerResolver(ctx, peerID)
	if err != nil {
		return nil, Candidate{}, fmt.Errorf("resolve TURN peer %q: %w", peerID, err)
	}
	if peer == nil || peer.IP == nil || peer.Port <= 0 {
		return nil, Candidate{}, fmt.Errorf("resolve TURN peer %q: invalid UDP address", peerID)
	}
	pending, candidate, err := p.AllocateCandidate(ctx)
	if err != nil {
		return nil, Candidate{}, err
	}
	conn, err := pending.Connect(ctx, peer)
	if err != nil {
		_ = pending.Close()
		return nil, Candidate{}, err
	}
	return conn, candidate, nil
}

// AllocateCandidate creates an authenticated UDP relay allocation without a
// peer permission. The returned bounded pending allocation owns the control
// socket until Connect transfers ownership to a turnConn or Close releases it.
func (p *TURNRelayProvider) AllocateCandidate(ctx context.Context) (*TURNPendingAllocation, Candidate, error) {
	if ctx == nil {
		return nil, Candidate{}, fmt.Errorf("TURN context is required")
	}

	dialCtx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()
	network := "udp"
	if p.cfg.Transport != TURNTransportUDP {
		network = "tcp"
	}
	conn, err := p.cfg.Dial(dialCtx, network, p.cfg.Server)
	if err != nil {
		return nil, Candidate{}, fmt.Errorf("dial TURN server: %w", err)
	}
	if p.cfg.Transport == TURNTransportTLS {
		tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
		if p.cfg.TLSConfig != nil {
			tlsConfig = p.cfg.TLSConfig.Clone()
			if tlsConfig.MinVersion < tls.VersionTLS12 {
				tlsConfig.MinVersion = tls.VersionTLS12
			}
		}
		if p.cfg.TLSServerName != "" {
			tlsConfig.ServerName = p.cfg.TLSServerName
		}
		if tlsConfig.ServerName == "" {
			tlsConfig.ServerName, _, _ = net.SplitHostPort(p.cfg.Server)
		}
		if p.cfg.TLSInsecureSkipVerify {
			tlsConfig.InsecureSkipVerify = true // explicit local-lab opt-out
		}
		tlsConn := tls.Client(conn, tlsConfig)
		if err := tlsConn.HandshakeContext(dialCtx); err != nil {
			_ = conn.Close()
			return nil, Candidate{}, fmt.Errorf("TURN TLS handshake: %w", err)
		}
		conn = tlsConn
	}
	wire := &turnWire{conn: conn, stream: p.cfg.Transport != TURNTransportUDP}
	fail := func(err error) (*TURNPendingAllocation, Candidate, error) {
		_ = conn.Close()
		return nil, Candidate{}, err
	}

	deadline := time.Now().Add(p.cfg.Timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return fail(fmt.Errorf("set TURN allocation deadline: %w", err))
	}

	tx, err := turnTransactionID()
	if err != nil {
		return fail(err)
	}
	request := turnMessage(turnMethodAllocate, turnClassRequest, tx, []turnAttribute{
		{typ: turnAttrRequestedTransport, value: []byte{17, 0, 0, 0}},
	})
	response, err := turnExchangeRetry(ctx, wire, request, tx, p.cfg.Timeout, p.cfg.Retransmits)
	if err != nil {
		return fail(fmt.Errorf("TURN unauthenticated allocate: %w", err))
	}
	code, realm, nonce, err := turnChallenge(response)
	if err != nil || code != 401 {
		if err == nil {
			err = fmt.Errorf("expected 401 challenge, got %d", code)
		}
		return fail(fmt.Errorf("TURN authentication challenge: %w", err))
	}

	key := turnLongTermKey(p.cfg.Username, realm, p.cfg.Password)
	var relay *net.UDPAddr
	var lifetime time.Duration
	for attempts := 0; attempts < 2; attempts++ {
		tx, err = turnTransactionID()
		if err != nil {
			return fail(err)
		}
		request = turnAuthenticatedMessage(turnMethodAllocate, turnClassRequest, tx, []turnAttribute{
			{typ: turnAttrRequestedTransport, value: []byte{17, 0, 0, 0}},
		}, p.cfg.Username, realm, nonce, key)
		response, err = turnExchangeRetry(ctx, wire, request, tx, p.cfg.Timeout, p.cfg.Retransmits)
		if err != nil {
			return fail(fmt.Errorf("TURN authenticated allocate: %w", err))
		}
		method, class := turnDecodeType(binary.BigEndian.Uint16(response[:2]))
		if method != turnMethodAllocate {
			return fail(fmt.Errorf("TURN allocate returned unexpected method %#x", method))
		}
		if class == turnClassError {
			code, newRealm, newNonce, challengeErr := turnChallenge(response)
			if challengeErr == nil && code == 438 && attempts == 0 && turnVerifyIntegrity(response, key) {
				if newRealm != "" {
					realm = newRealm
					key = turnLongTermKey(p.cfg.Username, realm, p.cfg.Password)
				}
				nonce = newNonce
				continue
			}
			return fail(fmt.Errorf("TURN allocate rejected with code %d", code))
		}
		if class != turnClassSuccess || !turnVerifyIntegrity(response, key) {
			return fail(fmt.Errorf("TURN allocate response failed authentication"))
		}
		relay, err = turnAddressAttribute(response, turnAttrXORRelayedAddress)
		if err != nil {
			return fail(fmt.Errorf("TURN relayed address: %w", err))
		}
		lifetime = turnLifetime(response)
		if lifetime <= 0 {
			return fail(fmt.Errorf("TURN allocation returned an expired lifetime"))
		}
		break
	}
	if relay == nil {
		return fail(fmt.Errorf("TURN allocation did not return a relay address"))
	}
	_ = conn.SetDeadline(time.Time{})

	candidate := Candidate{ID: "turn-" + relay.String(), Type: CandidateRelay, Addr: relay.String(), Priority: 1}
	pending := &TURNPendingAllocation{
		wire: wire, cfg: p.cfg, username: p.cfg.Username, password: p.cfg.Password,
		realm: realm, nonce: nonce, key: append([]byte(nil), key...), relay: relay, lifetime: lifetime,
	}
	pending.timer = time.AfterFunc(turnPendingAllocationTTL(lifetime), func() { _ = pending.Close() })
	return pending, candidate, nil
}

func turnPendingAllocationTTL(lifetime time.Duration) time.Duration {
	ttl := 30 * time.Second
	if half := lifetime / 2; half > 0 && half < ttl {
		ttl = half
	}
	if ttl < time.Second {
		ttl = time.Second
	}
	return ttl
}

type turnAttribute struct {
	typ   uint16
	value []byte
}

func turnType(method, class uint16) uint16 {
	return (method & 0x000f) | ((method & 0x0070) << 1) | ((method & 0x0f80) << 2) |
		((class & 1) << 4) | ((class & 2) << 7)
}

func turnDecodeType(typ uint16) (uint16, uint16) {
	method := (typ & 0x000f) | ((typ >> 1) & 0x0070) | ((typ >> 2) & 0x0f80)
	class := ((typ >> 4) & 1) | ((typ >> 7) & 2)
	return method, class
}

func turnTransactionID() ([12]byte, error) {
	var tx [12]byte
	if _, err := rand.Read(tx[:]); err != nil {
		return tx, fmt.Errorf("generate TURN transaction ID: %w", err)
	}
	return tx, nil
}

func turnMessage(method, class uint16, tx [12]byte, attrs []turnAttribute) []byte {
	b := make([]byte, turnHeaderSize)
	binary.BigEndian.PutUint16(b, turnType(method, class))
	binary.BigEndian.PutUint32(b[4:], turnMagicCookie)
	copy(b[8:], tx[:])
	for _, attr := range attrs {
		b = turnAppendAttribute(b, attr.typ, attr.value)
	}
	binary.BigEndian.PutUint16(b[2:], uint16(len(b)-turnHeaderSize))
	return b
}

func turnAuthenticatedMessage(method, class uint16, tx [12]byte, attrs []turnAttribute, username, realm, nonce string, key []byte) []byte {
	attrs = append(attrs,
		turnAttribute{typ: turnAttrUsername, value: []byte(username)},
		turnAttribute{typ: turnAttrRealm, value: []byte(realm)},
		turnAttribute{typ: turnAttrNonce, value: []byte(nonce)},
	)
	b := turnMessage(method, class, tx, attrs)
	binary.BigEndian.PutUint16(b[2:], uint16(len(b)-turnHeaderSize+24))
	mac := hmac.New(sha1.New, key)
	_, _ = mac.Write(b)
	return turnAppendAttribute(b, turnAttrMessageIntegrity, mac.Sum(nil))
}

func turnAppendAttribute(b []byte, typ uint16, value []byte) []byte {
	start := len(b)
	padding := (4 - len(value)%4) % 4
	b = append(b, make([]byte, 4+len(value)+padding)...)
	binary.BigEndian.PutUint16(b[start:], typ)
	binary.BigEndian.PutUint16(b[start+2:], uint16(len(value)))
	copy(b[start+4:], value)
	return b
}

func turnLongTermKey(username, realm, password string) []byte {
	sum := md5.Sum([]byte(username + ":" + realm + ":" + password))
	return sum[:]
}

type turnWire struct {
	conn   net.Conn
	stream bool
	readMu sync.Mutex
}

func (w *turnWire) write(message []byte) error {
	if !w.stream {
		n, err := w.conn.Write(message)
		if err == nil && n != len(message) {
			return io.ErrShortWrite
		}
		return err
	}
	// STUN frames are already padded to four bytes. ChannelData on a stream
	// carries its unpadded length and is followed by zero padding.
	frame := message
	if len(message) >= 4 && binary.BigEndian.Uint16(message[:2])&0xc000 == 0x4000 {
		length := int(binary.BigEndian.Uint16(message[2:4]))
		if length > len(message)-4 {
			return fmt.Errorf("truncated TURN ChannelData write")
		}
		padding := (4 - length%4) % 4
		frame = append(append([]byte(nil), message[:4+length]...), make([]byte, padding)...)
	}
	for len(frame) > 0 {
		n, err := w.conn.Write(frame)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		frame = frame[n:]
	}
	return nil
}

func (w *turnWire) read() ([]byte, error) {
	w.readMu.Lock()
	defer w.readMu.Unlock()
	if !w.stream {
		buf := make([]byte, turnMaxPacket)
		n, err := w.conn.Read(buf)
		if err != nil {
			return nil, err
		}
		return append([]byte(nil), buf[:n]...), nil
	}
	first := make([]byte, 4)
	if _, err := io.ReadFull(w.conn, first); err != nil {
		return nil, err
	}
	switch first[0] & 0xc0 {
	case 0x00: // STUN message: 20-byte header plus its bounded attribute length.
		header := append([]byte(nil), first...)
		header = append(header, make([]byte, turnHeaderSize-4)...)
		if _, err := io.ReadFull(w.conn, header[4:]); err != nil {
			return nil, err
		}
		length := int(binary.BigEndian.Uint16(header[2:4]))
		if length%4 != 0 || length > turnMaxPacket-turnHeaderSize {
			return nil, fmt.Errorf("invalid TURN stream STUN length %d", length)
		}
		message := append(header, make([]byte, length)...)
		_, err := io.ReadFull(w.conn, message[turnHeaderSize:])
		return message, err
	case 0x40: // ChannelData: channel + length + data + stream padding.
		channel := binary.BigEndian.Uint16(first[:2])
		if channel < 0x4000 || channel > 0x7fff {
			return nil, fmt.Errorf("invalid TURN channel %#x", channel)
		}
		length := int(binary.BigEndian.Uint16(first[2:4]))
		if length > turnMaxPacket-4 {
			return nil, fmt.Errorf("TURN ChannelData exceeds protocol limit")
		}
		padded := (length + 3) &^ 3
		body := make([]byte, padded)
		if _, err := io.ReadFull(w.conn, body); err != nil {
			return nil, err
		}
		return append(first, body[:length]...), nil
	default:
		return nil, fmt.Errorf("invalid TURN stream frame prefix %#x", first[0])
	}
}

func turnExchangeRetry(ctx context.Context, wire *turnWire, request []byte, tx [12]byte, timeout time.Duration, retransmits int) ([]byte, error) {
	// Reliable stream transports use their own delivery/retry semantics.
	attempts := 1
	if !wire.stream {
		attempts = retransmits + 1
	}
	perAttempt := timeout / time.Duration(attempts)
	if perAttempt < 20*time.Millisecond {
		perAttempt = 20 * time.Millisecond
	}
	started := time.Now()
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		deadline := time.Now().Add(perAttempt)
		if total := started.Add(timeout); deadline.After(total) {
			deadline = total
		}
		if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
			deadline = contextDeadline
		}
		if err := wire.conn.SetDeadline(deadline); err != nil {
			return nil, err
		}
		if err := wire.write(request); err != nil {
			lastErr = err
			continue
		}
		for {
			response, err := wire.read()
			if err != nil {
				lastErr = err
				break
			}
			if err := turnValidateMessage(response); err != nil {
				lastErr = err
				continue
			}
			// Delayed responses to a retransmitted prior transaction may be
			// queued on the socket. Ignore them instead of poisoning this one.
			if !hmac.Equal(response[8:20], tx[:]) {
				lastErr = fmt.Errorf("TURN transaction ID mismatch")
				continue
			}
			return response, nil
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("TURN transaction timeout")
	}
	return nil, lastErr
}

func turnValidateMessage(message []byte) error {
	if len(message) < turnHeaderSize || len(message) > turnMaxPacket {
		return fmt.Errorf("invalid TURN message size %d", len(message))
	}
	if binary.BigEndian.Uint32(message[4:8]) != turnMagicCookie {
		return fmt.Errorf("invalid TURN magic cookie")
	}
	length := int(binary.BigEndian.Uint16(message[2:4]))
	if length%4 != 0 || length != len(message)-turnHeaderSize {
		return fmt.Errorf("invalid TURN message length")
	}
	return nil
}

func turnAttributes(message []byte, visit func(uint16, []byte, int) bool) error {
	if err := turnValidateMessage(message); err != nil {
		return err
	}
	for off := turnHeaderSize; off < len(message); {
		if off+4 > len(message) {
			return fmt.Errorf("truncated TURN attribute header")
		}
		typ := binary.BigEndian.Uint16(message[off:])
		length := int(binary.BigEndian.Uint16(message[off+2:]))
		valueStart := off + 4
		if valueStart+length > len(message) {
			return fmt.Errorf("truncated TURN attribute %#x", typ)
		}
		if !visit(typ, message[valueStart:valueStart+length], off) {
			return nil
		}
		// Only FINGERPRINT may legally follow MESSAGE-INTEGRITY, and none of
		// the semantic consumers need it. Stop here so unauthenticated trailing
		// attributes can never override lifetime, realm, or nonce.
		if typ == turnAttrMessageIntegrity {
			return nil
		}
		off = valueStart + ((length + 3) &^ 3)
	}
	return nil
}

func turnChallenge(message []byte) (int, string, string, error) {
	code, realm, nonce := 0, "", ""
	err := turnAttributes(message, func(typ uint16, value []byte, _ int) bool {
		switch typ {
		case turnAttrErrorCode:
			if len(value) >= 4 {
				code = int(value[2]&7)*100 + int(value[3])
			}
		case turnAttrRealm:
			realm = string(value)
		case turnAttrNonce:
			nonce = string(value)
		}
		return true
	})
	if err != nil {
		return 0, "", "", err
	}
	if code == 0 || realm == "" || nonce == "" {
		return code, realm, nonce, fmt.Errorf("incomplete TURN authentication challenge")
	}
	return code, realm, nonce, nil
}

func turnErrorCode(message []byte) int {
	code := 0
	_ = turnAttributes(message, func(typ uint16, value []byte, _ int) bool {
		if typ == turnAttrErrorCode && len(value) >= 4 {
			code = int(value[2]&7)*100 + int(value[3])
			return false
		}
		return true
	})
	return code
}

func turnVerifyIntegrity(message, key []byte) bool {
	valid := false
	_ = turnAttributes(message, func(typ uint16, value []byte, off int) bool {
		if typ != turnAttrMessageIntegrity || len(value) != sha1.Size {
			return true
		}
		prefix := append([]byte(nil), message[:off]...)
		binary.BigEndian.PutUint16(prefix[2:], uint16(off+24-turnHeaderSize))
		mac := hmac.New(sha1.New, key)
		_, _ = mac.Write(prefix)
		valid = subtle.ConstantTimeCompare(value, mac.Sum(nil)) == 1
		return false
	})
	return valid
}

func turnAddressAttribute(message []byte, wanted uint16) (*net.UDPAddr, error) {
	var result *net.UDPAddr
	var parseErr error
	var tx [12]byte
	copy(tx[:], message[8:20])
	err := turnAttributes(message, func(typ uint16, value []byte, _ int) bool {
		if typ != wanted {
			return true
		}
		result, parseErr = turnDecodeXORAddress(value, tx)
		return false
	})
	if err != nil {
		return nil, err
	}
	if parseErr != nil {
		return nil, parseErr
	}
	if result == nil {
		return nil, fmt.Errorf("TURN address attribute %#x is missing", wanted)
	}
	return result, nil
}

func turnDecodeXORAddress(value []byte, tx [12]byte) (*net.UDPAddr, error) {
	if len(value) < 8 {
		return nil, fmt.Errorf("short XOR address")
	}
	port := int(binary.BigEndian.Uint16(value[2:4]) ^ uint16(turnMagicCookie>>16))
	mask := make([]byte, 16)
	binary.BigEndian.PutUint32(mask, turnMagicCookie)
	copy(mask[4:], tx[:])
	switch value[1] {
	case 1:
		ip := make(net.IP, 4)
		for i := range ip {
			ip[i] = value[4+i] ^ mask[i]
		}
		return &net.UDPAddr{IP: ip, Port: port}, nil
	case 2:
		if len(value) < 20 {
			return nil, fmt.Errorf("short IPv6 XOR address")
		}
		ip := make(net.IP, 16)
		for i := range ip {
			ip[i] = value[4+i] ^ mask[i]
		}
		return &net.UDPAddr{IP: ip, Port: port}, nil
	default:
		return nil, fmt.Errorf("unsupported TURN address family %d", value[1])
	}
}

func turnEncodeXORAddress(addr *net.UDPAddr, tx [12]byte) ([]byte, error) {
	if addr == nil || addr.Port <= 0 {
		return nil, fmt.Errorf("invalid TURN UDP address")
	}
	mask := make([]byte, 16)
	binary.BigEndian.PutUint32(mask, turnMagicCookie)
	copy(mask[4:], tx[:])
	if ip := addr.IP.To4(); ip != nil {
		value := make([]byte, 8)
		value[1] = 1
		binary.BigEndian.PutUint16(value[2:], uint16(addr.Port)^uint16(turnMagicCookie>>16))
		for i := range ip {
			value[4+i] = ip[i] ^ mask[i]
		}
		return value, nil
	}
	ip := addr.IP.To16()
	if ip == nil {
		return nil, fmt.Errorf("invalid TURN peer IP")
	}
	value := make([]byte, 20)
	value[1] = 2
	binary.BigEndian.PutUint16(value[2:], uint16(addr.Port)^uint16(turnMagicCookie>>16))
	for i := range ip {
		value[4+i] = ip[i] ^ mask[i]
	}
	return value, nil
}

func turnLifetime(message []byte) time.Duration {
	lifetime := 10 * time.Minute
	_ = turnAttributes(message, func(typ uint16, value []byte, _ int) bool {
		if typ == turnAttrLifetime && len(value) == 4 {
			seconds := binary.BigEndian.Uint32(value)
			lifetime = time.Duration(seconds) * time.Second
		}
		return true
	})
	return lifetime
}

type turnResponse struct {
	message []byte
	err     error
}

type turnConn struct {
	conn     net.Conn
	wire     *turnWire
	peer     *net.UDPAddr
	relay    *net.UDPAddr
	cfg      TURNConfig
	username string
	password string
	key      []byte

	authMu sync.RWMutex
	realm  string
	nonce  string

	writeMu                  sync.Mutex
	txMu                     sync.Mutex
	pending                  map[[12]byte]chan turnResponse
	inbound                  chan []byte
	done                     chan struct{}
	once                     sync.Once
	errMu                    sync.RWMutex
	err                      error
	closeOnce                sync.Once
	closeErr                 error
	channelMu                sync.RWMutex
	channel                  uint16
	channelBound             bool
	requestedRefreshInterval time.Duration

	deadlineMu    sync.RWMutex
	readDeadline  time.Time
	writeDeadline time.Time
}

func newTURNConn(wire *turnWire, peer, relay *net.UDPAddr, cfg TURNConfig, username, password, realm, nonce string, key []byte, lifetime time.Duration) *turnConn {
	requestedRefreshInterval := cfg.RefreshInterval
	cfg.RefreshInterval = safeTURNRefreshInterval(cfg.RefreshInterval, lifetime)
	channel := cfg.ChannelNumber
	if cfg.EnableChannelBind && channel == 0 {
		var random [2]byte
		if _, err := rand.Read(random[:]); err == nil {
			channel = 0x4000 | (binary.BigEndian.Uint16(random[:]) & 0x3fff)
		} else {
			channel = 0x4000
		}
	}
	return &turnConn{
		conn: wire.conn, wire: wire, peer: peer, relay: relay, cfg: cfg, username: username, password: password,
		realm: realm, nonce: nonce, key: append([]byte(nil), key...),
		channel: channel, requestedRefreshInterval: requestedRefreshInterval,
		pending: make(map[[12]byte]chan turnResponse), inbound: make(chan []byte, 32), done: make(chan struct{}),
	}
}

func safeTURNRefreshInterval(requested, lifetime time.Duration) time.Duration {
	if lifetime <= 0 {
		return time.Second
	}
	maximum := lifetime / 2
	if maximum > 4*time.Minute {
		maximum = 4 * time.Minute
	}
	if maximum <= 0 {
		maximum = lifetime
	}
	minimum := time.Second
	if maximum < minimum {
		minimum = maximum
	}
	if requested <= 0 || requested > maximum {
		return maximum
	}
	if requested < minimum {
		return minimum
	}
	return requested
}

func (c *turnConn) start(ctx context.Context) error {
	go c.readLoop()
	if err := c.createPermission(ctx); err != nil {
		return fmt.Errorf("create TURN permission: %w", err)
	}
	if c.cfg.EnableChannelBind {
		// ChannelBind failure is a negotiated optimization failure, not an
		// allocation failure. Send/Data indications remain fully usable.
		if err := c.bindChannel(ctx); err != nil {
			var rejected *turnChannelBindRejectedError
			if !errors.As(err, &rejected) {
				return fmt.Errorf("bind TURN channel: %w", err)
			}
		}
	}
	go c.refreshLoop()
	return nil
}

type turnChannelBindRejectedError struct{ code int }

func (e *turnChannelBindRejectedError) Error() string {
	return fmt.Sprintf("TURN channel bind rejected with code %d", e.code)
}

func (c *turnConn) bindChannel(ctx context.Context) error {
	c.channelMu.RLock()
	channel := c.channel
	c.channelMu.RUnlock()
	response, err := c.authTransaction(ctx, turnMethodChannelBind, func(tx [12]byte) ([]turnAttribute, error) {
		peer, err := turnEncodeXORAddress(c.peer, tx)
		if err != nil {
			return nil, err
		}
		number := make([]byte, 4)
		binary.BigEndian.PutUint16(number, channel)
		return []turnAttribute{{typ: turnAttrChannelNumber, value: number}, {typ: turnAttrXORPeerAddress, value: peer}}, nil
	})
	if err != nil {
		return err
	}
	method, class := turnDecodeType(binary.BigEndian.Uint16(response[:2]))
	if method != turnMethodChannelBind || !c.verifyIntegrity(response) {
		return fmt.Errorf("TURN channel bind response failed authentication")
	}
	if class == turnClassError {
		code := turnErrorCode(response)
		if code == 0 {
			return fmt.Errorf("TURN channel bind returned malformed error")
		}
		return &turnChannelBindRejectedError{code: code}
	}
	if class != turnClassSuccess {
		return fmt.Errorf("TURN channel bind returned invalid class %d", class)
	}
	c.channelMu.Lock()
	c.channelBound = true
	c.channelMu.Unlock()
	return nil
}

func (c *turnConn) createPermission(ctx context.Context) error {
	response, err := c.authTransaction(ctx, turnMethodCreatePermission, func(tx [12]byte) ([]turnAttribute, error) {
		value, err := turnEncodeXORAddress(c.peer, tx)
		return []turnAttribute{{typ: turnAttrXORPeerAddress, value: value}}, err
	})
	if err != nil {
		return err
	}
	method, class := turnDecodeType(binary.BigEndian.Uint16(response[:2]))
	if method != turnMethodCreatePermission || class != turnClassSuccess || !c.verifyIntegrity(response) {
		return fmt.Errorf("TURN permission response failed authentication")
	}
	return nil
}

func (c *turnConn) refresh(ctx context.Context) (time.Duration, error) {
	response, err := c.authTransaction(ctx, turnMethodRefresh, func([12]byte) ([]turnAttribute, error) {
		lifetime := make([]byte, 4)
		binary.BigEndian.PutUint32(lifetime, 600)
		return []turnAttribute{{typ: turnAttrLifetime, value: lifetime}}, nil
	})
	if err != nil {
		return 0, err
	}
	method, class := turnDecodeType(binary.BigEndian.Uint16(response[:2]))
	if method != turnMethodRefresh || class != turnClassSuccess || !c.verifyIntegrity(response) {
		return 0, fmt.Errorf("TURN refresh response failed authentication")
	}
	lifetime := turnLifetime(response)
	if lifetime <= 0 {
		return 0, fmt.Errorf("TURN refresh returned an expired lifetime")
	}
	c.channelMu.RLock()
	bound := c.channelBound
	c.channelMu.RUnlock()
	if bound {
		if err := c.bindChannel(ctx); err == nil {
			return lifetime, nil
		} else {
			var rejected *turnChannelBindRejectedError
			if !errors.As(err, &rejected) {
				return 0, err
			}
		}
		c.channelMu.Lock()
		c.channelBound = false
		c.channelMu.Unlock()
	}
	if err := c.createPermission(ctx); err != nil {
		return 0, err
	}
	return lifetime, nil
}

func (c *turnConn) verifyIntegrity(message []byte) bool {
	c.authMu.RLock()
	key := append([]byte(nil), c.key...)
	c.authMu.RUnlock()
	return turnVerifyIntegrity(message, key)
}

func (c *turnConn) authTransaction(ctx context.Context, method uint16, attrs func([12]byte) ([]turnAttribute, error)) ([]byte, error) {
	for attempt := 0; attempt < 2; attempt++ {
		tx, err := turnTransactionID()
		if err != nil {
			return nil, err
		}
		requestAttrs, err := attrs(tx)
		if err != nil {
			return nil, err
		}
		c.authMu.RLock()
		realm, nonce, key := c.realm, c.nonce, append([]byte(nil), c.key...)
		c.authMu.RUnlock()
		request := turnAuthenticatedMessage(method, turnClassRequest, tx, requestAttrs, c.username, realm, nonce, key)
		response, err := c.transaction(ctx, tx, request)
		if err != nil {
			return nil, err
		}
		responseMethod, class := turnDecodeType(binary.BigEndian.Uint16(response[:2]))
		if responseMethod != method {
			return nil, fmt.Errorf("TURN transaction returned unexpected method %#x", responseMethod)
		}
		if class != turnClassError {
			return response, nil
		}
		code, newRealm, newNonce, challengeErr := turnChallenge(response)
		if attempt != 0 || challengeErr != nil || code != 438 || !turnVerifyIntegrity(response, key) {
			return response, nil
		}
		newKey := turnLongTermKey(c.username, newRealm, c.password)
		c.authMu.Lock()
		c.realm, c.nonce, c.key = newRealm, newNonce, newKey
		c.authMu.Unlock()
	}
	return nil, fmt.Errorf("TURN stale nonce retry exhausted")
}

func (c *turnConn) transaction(ctx context.Context, tx [12]byte, request []byte) ([]byte, error) {
	if ctx == nil {
		return nil, fmt.Errorf("TURN transaction context is required")
	}
	wait := make(chan turnResponse, 1)
	c.txMu.Lock()
	if _, exists := c.pending[tx]; exists {
		c.txMu.Unlock()
		return nil, fmt.Errorf("duplicate TURN transaction")
	}
	c.pending[tx] = wait
	c.txMu.Unlock()
	defer func() {
		c.txMu.Lock()
		delete(c.pending, tx)
		c.txMu.Unlock()
	}()
	if err := c.writePacket(request); err != nil {
		return nil, err
	}
	total := time.NewTimer(c.cfg.Timeout)
	defer total.Stop()
	retransmitEvery := c.cfg.Timeout / time.Duration(c.cfg.Retransmits+1)
	if retransmitEvery < 20*time.Millisecond {
		retransmitEvery = 20 * time.Millisecond
	}
	retransmit := time.NewTicker(retransmitEvery)
	defer retransmit.Stop()
	retransmits := 0
	for {
		select {
		case result := <-wait:
			return result.message, result.err
		case <-retransmit.C:
			if !c.wire.stream && retransmits < c.cfg.Retransmits {
				retransmits++
				if err := c.writePacket(request); err != nil {
					return nil, err
				}
			}
		case <-total.C:
			return nil, fmt.Errorf("TURN transaction timeout after %d retransmits", retransmits)
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.done:
			return nil, c.closeError()
		}
	}
}

func (c *turnConn) readLoop() {
	for {
		message, err := c.wire.read()
		if err != nil {
			c.terminate(err)
			return
		}
		if len(message) >= 4 && binary.BigEndian.Uint16(message[:2])&0xc000 == 0x4000 {
			channel := binary.BigEndian.Uint16(message[:2])
			length := int(binary.BigEndian.Uint16(message[2:4]))
			c.channelMu.RLock()
			bound, expected := c.channelBound, c.channel
			c.channelMu.RUnlock()
			available := len(message) - 4
			padding := available - length
			if !bound || channel != expected || length > available || padding < 0 || padding > 3 || length > c.cfg.MaxDatagramSize {
				continue // reject unbound, unexpected-channel and malformed frames
			}
			data := append([]byte(nil), message[4:4+length]...)
			select {
			case c.inbound <- data:
			case <-c.done:
				return
			default:
			}
			continue
		}
		if err := turnValidateMessage(message); err != nil {
			continue
		}
		method, class := turnDecodeType(binary.BigEndian.Uint16(message[:2]))
		if method == turnMethodData && class == turnClassIndication {
			peer, err := turnAddressAttribute(message, turnAttrXORPeerAddress)
			if err != nil || !peer.IP.Equal(c.peer.IP) || peer.Port != c.peer.Port {
				continue
			}
			var data []byte
			_ = turnAttributes(message, func(typ uint16, value []byte, _ int) bool {
				if typ == turnAttrData {
					data = append([]byte(nil), value...)
					return false
				}
				return true
			})
			if data != nil && len(data) <= c.cfg.MaxDatagramSize {
				select {
				case c.inbound <- data:
				case <-c.done:
					return
				default: // Bounded queue: drop rather than deadlock control traffic.
				}
			}
			continue
		}
		var tx [12]byte
		copy(tx[:], message[8:20])
		c.txMu.Lock()
		wait := c.pending[tx]
		c.txMu.Unlock()
		if wait != nil {
			select {
			case wait <- turnResponse{message: message}:
			default:
			}
		}
	}
}

func (c *turnConn) refreshLoop() {
	timer := time.NewTimer(c.cfg.RefreshInterval)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			ctx, cancel := context.WithTimeout(context.Background(), c.cfg.Timeout)
			lifetime, err := c.refresh(ctx)
			cancel()
			if err != nil {
				c.terminate(fmt.Errorf("TURN allocation refresh: %w", err))
				return
			}
			c.cfg.RefreshInterval = safeTURNRefreshInterval(c.requestedRefreshInterval, lifetime)
			timer.Reset(c.cfg.RefreshInterval)
		case <-c.done:
			return
		}
	}
}

func (c *turnConn) Read(p []byte) (int, error) {
	deadline := c.deadline(true)
	var timer <-chan time.Time
	if !deadline.IsZero() {
		d := time.Until(deadline)
		if d <= 0 {
			return 0, timeoutError{"TURN read timeout"}
		}
		t := time.NewTimer(d)
		defer t.Stop()
		timer = t.C
	}
	select {
	case data := <-c.inbound:
		return copy(p, data), nil
	case <-c.done:
		return 0, c.closeError()
	case <-timer:
		return 0, timeoutError{"TURN read timeout"}
	}
}

func (c *turnConn) Write(p []byte) (int, error) {
	if len(p) > c.cfg.MaxDatagramSize {
		return 0, fmt.Errorf("TURN datagram exceeds %d bytes", c.cfg.MaxDatagramSize)
	}
	select {
	case <-c.done:
		return 0, c.closeError()
	default:
	}
	c.channelMu.RLock()
	bound, channel := c.channelBound, c.channel
	c.channelMu.RUnlock()
	if bound {
		message := make([]byte, 4+len(p))
		binary.BigEndian.PutUint16(message[:2], channel)
		binary.BigEndian.PutUint16(message[2:4], uint16(len(p)))
		copy(message[4:], p)
		if err := c.writePacket(message); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	tx, err := turnTransactionID()
	if err != nil {
		return 0, err
	}
	peer, err := turnEncodeXORAddress(c.peer, tx)
	if err != nil {
		return 0, err
	}
	message := turnMessage(turnMethodSend, turnClassIndication, tx, []turnAttribute{
		{typ: turnAttrXORPeerAddress, value: peer}, {typ: turnAttrData, value: append([]byte(nil), p...)},
	})
	if err := c.writePacket(message); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *turnConn) writePacket(message []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	deadline := c.deadline(false)
	if deadline.IsZero() {
		deadline = time.Now().Add(c.cfg.Timeout)
	}
	if err := c.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	return c.wire.write(message)
}

func (c *turnConn) Close() error {
	c.closeOnce.Do(func() {
		select {
		case <-c.done:
			return
		default:
		}
		// Best-effort deallocation bounds server-side resource retention. Close
		// remains bounded even when the TURN server has disappeared.
		closeTimeout := c.cfg.Timeout
		if closeTimeout > 250*time.Millisecond {
			closeTimeout = 250 * time.Millisecond
		}
		_ = c.SetWriteDeadline(time.Now().Add(closeTimeout))
		// Interrupt a concurrent TCP/TLS writer before waiting for writeMu in
		// authTransaction; its previous user deadline may be arbitrarily far out.
		_ = c.conn.SetWriteDeadline(time.Now().Add(closeTimeout))
		// A writer racing the line above may reinstall its old far deadline after
		// Close. Force-closing the control socket at the same bounded deadline is
		// the final ordering-independent escape hatch; successful deallocation
		// stops this timer before it fires.
		forceClose := time.AfterFunc(closeTimeout, func() { _ = c.conn.Close() })
		defer forceClose.Stop()
		ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		response, err := c.authTransaction(ctx, turnMethodRefresh, func([12]byte) ([]turnAttribute, error) {
			return []turnAttribute{{typ: turnAttrLifetime, value: []byte{0, 0, 0, 0}}}, nil
		})
		cancel()
		if err == nil {
			method, class := turnDecodeType(binary.BigEndian.Uint16(response[:2]))
			if method != turnMethodRefresh || class != turnClassSuccess || !c.verifyIntegrity(response) {
				err = fmt.Errorf("TURN deallocation response failed authentication")
			}
		}
		c.closeErr = err
		c.terminate(net.ErrClosed)
	})
	return c.closeErr
}

func (c *turnConn) terminate(err error) {
	c.once.Do(func() {
		c.errMu.Lock()
		c.err = err
		c.errMu.Unlock()
		close(c.done)
		_ = c.conn.Close()
	})
}

func (c *turnConn) closeError() error {
	c.errMu.RLock()
	err := c.err
	c.errMu.RUnlock()
	if err == nil {
		return net.ErrClosed
	}
	return err
}

func (c *turnConn) LocalAddr() net.Addr  { return c.relay }
func (c *turnConn) RemoteAddr() net.Addr { return c.peer }

func (c *turnConn) SetDeadline(t time.Time) error {
	c.deadlineMu.Lock()
	c.readDeadline, c.writeDeadline = t, t
	c.deadlineMu.Unlock()
	return nil
}

func (c *turnConn) SetReadDeadline(t time.Time) error {
	c.deadlineMu.Lock()
	c.readDeadline = t
	c.deadlineMu.Unlock()
	return nil
}

func (c *turnConn) SetWriteDeadline(t time.Time) error {
	c.deadlineMu.Lock()
	c.writeDeadline = t
	c.deadlineMu.Unlock()
	return nil
}

func (c *turnConn) deadline(read bool) time.Time {
	c.deadlineMu.RLock()
	defer c.deadlineMu.RUnlock()
	if read {
		return c.readDeadline
	}
	return c.writeDeadline
}

type timeoutError struct{ message string }

func (e timeoutError) Error() string   { return e.message }
func (e timeoutError) Timeout() bool   { return true }
func (e timeoutError) Temporary() bool { return true }

var _ RelayProvider = (*TURNRelayProvider)(nil)
var _ net.Conn = (*turnConn)(nil)

// Keep io imported as a compile-time assertion that net.Conn retains the
// stream interfaces expected by ICE even though TURN payloads are datagrams.
var _ io.ReadWriteCloser = (*turnConn)(nil)
