package p2p

// This file implements the RFC 5766 UDP subset needed by the ICE relay
// fallback: authenticated Allocate/Refresh/CreatePermission transactions and
// Send/Data indications. TURN channels and TCP allocations are intentionally
// outside this adapter; callers still get a connected net.Conn abstraction.

import (
	"context"
	"crypto/hmac"
	"crypto/md5" // RFC 5389 long-term credential key derivation requires MD5.
	"crypto/rand"
	"crypto/sha1" // RFC 5389 MESSAGE-INTEGRITY requires HMAC-SHA1.
	"crypto/subtle"
	"encoding/binary"
	"fmt"
	"io"
	"net"
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

	turnClassRequest    = 0
	turnClassIndication = 1
	turnClassSuccess    = 2
	turnClassError      = 3

	turnAttrUsername           = 0x0006
	turnAttrMessageIntegrity   = 0x0008
	turnAttrErrorCode          = 0x0009
	turnAttrLifetime           = 0x000d
	turnAttrXORPeerAddress     = 0x0012
	turnAttrData               = 0x0013
	turnAttrRealm              = 0x0014
	turnAttrNonce              = 0x0015
	turnAttrXORRelayedAddress  = 0x0016
	turnAttrRequestedTransport = 0x0019
)

// TURNPacketDialer allows deterministic protocol tests without a live TURN
// server. The production implementation uses a connected UDP socket.
type TURNPacketDialer func(context.Context, string, string) (net.Conn, error)

// TURNPeerResolver maps an authenticated signalling peer ID to the peer's UDP
// address. TURN permissions are address-bound, so silently treating peerID as
// an address would cross the signalling/security boundary.
type TURNPeerResolver func(context.Context, string) (*net.UDPAddr, error)

type TURNConfig struct {
	Server          string
	Username        string
	Password        string
	PeerResolver    TURNPeerResolver
	Dial            TURNPacketDialer
	Timeout         time.Duration
	RefreshInterval time.Duration
	MaxDatagramSize int
	// Retransmits is the number of bounded UDP retransmissions after the
	// initial send. TURN transactions are otherwise vulnerable to a single
	// dropped packet. The default is three.
	Retransmits int
}

// TURNRelayProvider is a concrete RelayProvider for TURN-over-UDP. It uses
// Send/Data indications and does not claim support for ChannelBind or TURN/TCP.
type TURNRelayProvider struct{ cfg TURNConfig }

func NewTURNRelayProvider(cfg TURNConfig) (*TURNRelayProvider, error) {
	if strings.TrimSpace(cfg.Server) == "" {
		return nil, fmt.Errorf("TURN server is required")
	}
	if cfg.Username == "" || cfg.Password == "" {
		return nil, fmt.Errorf("TURN long-term username and password are required")
	}
	if cfg.PeerResolver == nil {
		return nil, fmt.Errorf("TURN peer resolver is required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.MaxDatagramSize <= 0 {
		cfg.MaxDatagramSize = 60 * 1024
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

func (p *TURNRelayProvider) Allocate(ctx context.Context, peerID string) (net.Conn, Candidate, error) {
	if ctx == nil {
		return nil, Candidate{}, fmt.Errorf("TURN context is required")
	}
	if strings.TrimSpace(peerID) == "" {
		return nil, Candidate{}, fmt.Errorf("TURN peer ID is required")
	}
	peer, err := p.cfg.PeerResolver(ctx, peerID)
	if err != nil {
		return nil, Candidate{}, fmt.Errorf("resolve TURN peer %q: %w", peerID, err)
	}
	if peer == nil || peer.IP == nil || peer.Port <= 0 {
		return nil, Candidate{}, fmt.Errorf("resolve TURN peer %q: invalid UDP address", peerID)
	}

	dialCtx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()
	conn, err := p.cfg.Dial(dialCtx, "udp", p.cfg.Server)
	if err != nil {
		return nil, Candidate{}, fmt.Errorf("dial TURN server: %w", err)
	}
	fail := func(err error) (net.Conn, Candidate, error) {
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
	response, err := turnExchangeRetry(ctx, conn, request, tx, p.cfg.Timeout, p.cfg.Retransmits)
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
		response, err = turnExchangeRetry(ctx, conn, request, tx, p.cfg.Timeout, p.cfg.Retransmits)
		if err != nil {
			return fail(fmt.Errorf("TURN authenticated allocate: %w", err))
		}
		method, class := turnDecodeType(binary.BigEndian.Uint16(response[:2]))
		if method != turnMethodAllocate {
			return fail(fmt.Errorf("TURN allocate returned unexpected method %#x", method))
		}
		if class == turnClassError {
			code, newRealm, newNonce, challengeErr := turnChallenge(response)
			if challengeErr == nil && code == 438 && attempts == 0 {
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
		break
	}
	if relay == nil {
		return fail(fmt.Errorf("TURN allocation did not return a relay address"))
	}
	_ = conn.SetDeadline(time.Time{})

	tc := newTURNConn(conn, peer, relay, p.cfg, p.cfg.Username, p.cfg.Password, realm, nonce, key, lifetime)
	if err := tc.start(ctx); err != nil {
		_ = tc.Close()
		return nil, Candidate{}, err
	}
	candidate := Candidate{ID: "turn-" + relay.String(), Type: CandidateRelay, Addr: relay.String(), Priority: 1}
	return tc, candidate, nil
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

func turnExchangeRetry(ctx context.Context, conn net.Conn, request []byte, tx [12]byte, timeout time.Duration, retransmits int) ([]byte, error) {
	attempts := retransmits + 1
	perAttempt := timeout / time.Duration(attempts)
	if perAttempt < 20*time.Millisecond {
		perAttempt = 20 * time.Millisecond
	}
	started := time.Now()
	buf := make([]byte, turnMaxPacket)
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
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, err
		}
		if _, err := conn.Write(request); err != nil {
			lastErr = err
			continue
		}
		for {
			n, err := conn.Read(buf)
			if err != nil {
				lastErr = err
				break
			}
			response := append([]byte(nil), buf[:n]...)
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
			if seconds > 0 {
				lifetime = time.Duration(seconds) * time.Second
			}
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
	peer     *net.UDPAddr
	relay    *net.UDPAddr
	cfg      TURNConfig
	username string
	password string
	key      []byte

	authMu sync.RWMutex
	realm  string
	nonce  string

	writeMu sync.Mutex
	txMu    sync.Mutex
	pending map[[12]byte]chan turnResponse
	inbound chan []byte
	done    chan struct{}
	once    sync.Once
	errMu   sync.RWMutex
	err     error

	deadlineMu    sync.RWMutex
	readDeadline  time.Time
	writeDeadline time.Time
}

func newTURNConn(conn net.Conn, peer, relay *net.UDPAddr, cfg TURNConfig, username, password, realm, nonce string, key []byte, lifetime time.Duration) *turnConn {
	if cfg.RefreshInterval <= 0 {
		cfg.RefreshInterval = lifetime / 2
		if cfg.RefreshInterval > 4*time.Minute {
			cfg.RefreshInterval = 4 * time.Minute
		}
		if cfg.RefreshInterval < time.Second {
			cfg.RefreshInterval = time.Second
		}
	}
	return &turnConn{
		conn: conn, peer: peer, relay: relay, cfg: cfg, username: username, password: password,
		realm: realm, nonce: nonce, key: append([]byte(nil), key...),
		pending: make(map[[12]byte]chan turnResponse), inbound: make(chan []byte, 32), done: make(chan struct{}),
	}
}

func (c *turnConn) start(ctx context.Context) error {
	go c.readLoop()
	if err := c.createPermission(ctx); err != nil {
		return fmt.Errorf("create TURN permission: %w", err)
	}
	go c.refreshLoop()
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

func (c *turnConn) refresh(ctx context.Context) error {
	response, err := c.authTransaction(ctx, turnMethodRefresh, func([12]byte) ([]turnAttribute, error) {
		lifetime := make([]byte, 4)
		binary.BigEndian.PutUint32(lifetime, 600)
		return []turnAttribute{{typ: turnAttrLifetime, value: lifetime}}, nil
	})
	if err != nil {
		return err
	}
	method, class := turnDecodeType(binary.BigEndian.Uint16(response[:2]))
	if method != turnMethodRefresh || class != turnClassSuccess || !c.verifyIntegrity(response) {
		return fmt.Errorf("TURN refresh response failed authentication")
	}
	return c.createPermission(ctx)
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
			if retransmits < c.cfg.Retransmits {
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
	buf := make([]byte, turnMaxPacket)
	for {
		n, err := c.conn.Read(buf)
		if err != nil {
			c.terminate(err)
			return
		}
		message := append([]byte(nil), buf[:n]...)
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
	ticker := time.NewTicker(c.cfg.RefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), c.cfg.Timeout)
			err := c.refresh(ctx)
			cancel()
			if err != nil {
				c.terminate(fmt.Errorf("TURN allocation refresh: %w", err))
				return
			}
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
	_, err := c.conn.Write(message)
	return err
}

func (c *turnConn) Close() error {
	c.terminate(net.ErrClosed)
	return nil
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
