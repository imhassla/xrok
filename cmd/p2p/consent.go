package p2p

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var consentMagic = [4]byte{'X', 'R', 'C', '1'}
var errLatePunch = errors.New("late authenticated punch datagram")

const (
	consentData byte = iota
	consentRequest
	consentResponse
	consentDataAck
	maxConsentApplicationRecord = p2pMaxPayload + p2pHeaderSize + 1
	consentDataNonceSize        = 12
	consentDataTagSize          = 16
	maxConsentRecord            = maxConsentApplicationRecord + consentDataNonceSize + consentDataTagSize
	maxConsentPending           = 16
	maxConsentDataPending       = 64
	consentReplayWindowBits     = 64
	consentDataRetries          = 5
	consentDataRetryInterval    = 250 * time.Millisecond
)

var errConsentReplay = errors.New("replayed P2P data record")

// RuntimeConsentProbe performs consent through the connection's framing
// layer. It never reads the transport itself, so the P2PTunnel dispatcher
// remains the only socket read owner.
type RuntimeConsentProbe struct{}

func (RuntimeConsentProbe) Probe(ctx context.Context, conn net.Conn) error {
	prober, ok := conn.(interface{ probeConsent(context.Context) error })
	if !ok {
		return fmt.Errorf("connection does not support runtime consent")
	}
	return prober.probeConsent(ctx)
}

// consentConn multiplexes AEAD-protected application records and authenticated
// consent controls below the P2P tunnel framing. The wrapped net.Conn must be message-oriented: one
// Write is one complete message and one Read returns exactly one message. This
// includes direct UDPConn and the TURN relay connection used by this runtime;
// arbitrary TCP stream net.Conn values are intentionally unsupported.
type consentConn struct {
	net.Conn
	key        []byte
	session    string
	sendAEAD   cipher.AEAD
	recvAEAD   cipher.AEAD
	sendAAD    []byte
	recvAAD    []byte
	sendAckKey []byte
	recvAckKey []byte
	reliable   bool
	sendSalt   [4]byte
	sendSeq    atomic.Uint64

	writeMu     sync.Mutex
	sendMu      sync.Mutex
	closeOnce   sync.Once
	done        chan struct{}
	readBuf     []byte
	mu          sync.Mutex
	pending     map[string]chan struct{}
	pendingData map[uint64]chan struct{}
	replayMu    sync.Mutex
	recvSalt    [4]byte
	recvSaltSet bool
	recvHighest uint64
	recvWindow  uint64
	recvNext    uint64
	recvPending map[uint64][]byte
}

func newConsentConn(conn net.Conn, key []byte, session string) net.Conn {
	return newDirectionalConsentConn(conn, key, session, "shared", "shared")
}

func newDirectionalConsentConn(conn net.Conn, key []byte, session, sendLabel, receiveLabel string) net.Conn {
	if conn == nil || len(key) == 0 || session == "" {
		return conn
	}
	controlKey := deriveConsentKey("control", key, session)
	sendKey := deriveConsentKey("data/from/"+sendLabel, key, session)
	receiveKey := deriveConsentKey("data/from/"+receiveLabel, key, session)
	sendBlock, err := aes.NewCipher(sendKey)
	if err != nil {
		return conn // SHA-256 always yields a valid AES-256 key.
	}
	receiveBlock, err := aes.NewCipher(receiveKey)
	if err != nil {
		return conn
	}
	sendAEAD, err := cipher.NewGCM(sendBlock)
	if err != nil {
		return conn
	}
	receiveAEAD, err := cipher.NewGCM(receiveBlock)
	if err != nil {
		return conn
	}
	c := &consentConn{
		Conn: conn, key: controlKey, session: session, sendAEAD: sendAEAD, recvAEAD: receiveAEAD,
		sendAAD:     []byte("xrok-p2p-data-v2\x00" + session + "\x00from=" + sendLabel),
		recvAAD:     []byte("xrok-p2p-data-v2\x00" + session + "\x00from=" + receiveLabel),
		sendAckKey:  deriveConsentKey("ack/for/"+sendLabel, key, session),
		recvAckKey:  deriveConsentKey("ack/for/"+receiveLabel, key, session),
		reliable:    sendLabel != receiveLabel,
		pending:     make(map[string]chan struct{}),
		pendingData: make(map[uint64]chan struct{}), recvNext: 1, recvPending: make(map[uint64][]byte),
		done: make(chan struct{}),
	}
	if _, err := rand.Read(c.sendSalt[:]); err != nil {
		return conn
	}
	return c
}

func deriveConsentKey(label string, key []byte, session string) []byte {
	h := sha256.New()
	_, _ = h.Write([]byte("xrok-p2p/" + label + "/v1\x00"))
	_, _ = h.Write(key)
	_, _ = h.Write([]byte("\x00" + session))
	return h.Sum(nil)
}

func (c *consentConn) Write(p []byte) (int, error) {
	if len(p) > maxConsentApplicationRecord {
		return 0, fmt.Errorf("P2P record too large: %d", len(p))
	}
	if c.reliable {
		c.sendMu.Lock()
		defer c.sendMu.Unlock()
	}
	payload, sequence, err := c.sealData(p)
	if err != nil {
		return 0, err
	}
	waiter := make(chan struct{})
	c.mu.Lock()
	if len(c.pendingData) >= maxConsentDataPending {
		c.mu.Unlock()
		return 0, fmt.Errorf("too many pending P2P data records")
	}
	c.pendingData[sequence] = waiter
	c.mu.Unlock()
	if c.reliable {
		return c.writeReliableData(p, sequence, payload, waiter)
	}
	if err := c.writeRecord(consentData, payload); err != nil {
		c.mu.Lock()
		delete(c.pendingData, sequence)
		c.mu.Unlock()
		return 0, err
	}
	go c.retransmitData(sequence, payload, waiter)
	return len(p), nil
}

func (c *consentConn) writeReliableData(plaintext []byte, sequence uint64, payload []byte, waiter <-chan struct{}) (int, error) {
	defer func() {
		c.mu.Lock()
		delete(c.pendingData, sequence)
		c.mu.Unlock()
	}()
	for attempt := 0; attempt <= consentDataRetries; attempt++ {
		if err := c.writeRecord(consentData, payload); err != nil {
			_ = c.Close()
			return 0, err
		}
		timer := time.NewTimer(consentDataRetryInterval)
		select {
		case <-waiter:
			timer.Stop()
			return len(plaintext), nil
		case <-c.done:
			timer.Stop()
			return 0, net.ErrClosed
		case <-timer.C:
		}
	}
	err := timeoutError{"P2P data acknowledgement timeout"}
	_ = c.Close()
	return 0, err
}

func (c *consentConn) retransmitData(sequence uint64, payload []byte, waiter <-chan struct{}) {
	ticker := time.NewTicker(consentDataRetryInterval)
	defer ticker.Stop()
	defer func() {
		c.mu.Lock()
		delete(c.pendingData, sequence)
		c.mu.Unlock()
	}()
	for attempt := 0; attempt < consentDataRetries; attempt++ {
		select {
		case <-waiter:
			return
		case <-c.done:
			return
		case <-ticker.C:
			if err := c.writeRecord(consentData, payload); err != nil {
				return
			}
		}
	}
}

func (c *consentConn) Close() error {
	c.closeOnce.Do(func() { close(c.done) })
	return c.Conn.Close()
}

func (c *consentConn) Read(p []byte) (int, error) {
	if len(c.readBuf) != 0 {
		n := copy(p, c.readBuf)
		c.readBuf = c.readBuf[n:]
		return n, nil
	}
	c.replayMu.Lock()
	if ready := c.recvPending[c.recvNext]; ready != nil {
		delete(c.recvPending, c.recvNext)
		c.recvNext++
		c.readBuf = ready
	}
	c.replayMu.Unlock()
	if len(c.readBuf) != 0 {
		n := copy(p, c.readBuf)
		c.readBuf = c.readBuf[n:]
		return n, nil
	}
	kind, payload, err := c.readRecord()
	if err != nil {
		if errors.Is(err, errLatePunch) || errors.Is(err, errConsentReplay) {
			// A retransmit scheduled before nomination can arrive after the socket
			// has moved to tunnel framing. It is control-plane residue, not a
			// malformed application frame.
			return 0, nil
		}
		return 0, err
	}
	switch kind {
	case consentData:
		plaintext, sequence, err := c.openData(payload)
		if err != nil {
			if errors.Is(err, errConsentReplay) {
				_ = c.writeRecord(consentDataAck, c.dataAckPayload(sequence, payload[:consentDataNonceSize]))
				return 0, nil
			}
			return 0, err
		}
		ack := false
		c.replayMu.Lock()
		if sequence == c.recvNext {
			c.recvNext++
			c.readBuf = plaintext
			ack = true
		} else if sequence > c.recvNext && sequence-c.recvNext < consentReplayWindowBits {
			c.recvPending[sequence] = plaintext
			ack = true
		}
		c.replayMu.Unlock()
		if ack {
			_ = c.writeRecord(consentDataAck, c.dataAckPayload(sequence, payload[:consentDataNonceSize]))
		}
		if len(c.readBuf) == 0 {
			return 0, nil
		}
		n := copy(p, c.readBuf)
		c.readBuf = c.readBuf[n:]
		return n, nil
	case consentRequest:
		nonce, ok := c.verifyControl(consentRequest, payload)
		if ok {
			_ = c.writeRecord(consentResponse, c.controlPayload(consentResponse, nonce))
		}
		// Let the tunnel dispatcher renew its read deadline after consuming a
		// control datagram that is intentionally hidden from application framing.
		return 0, nil
	case consentResponse:
		nonce, ok := c.verifyControl(consentResponse, payload)
		if ok {
			c.mu.Lock()
			waiter := c.pending[nonce]
			if waiter != nil {
				delete(c.pending, nonce)
				close(waiter)
			}
			c.mu.Unlock()
		}
		return 0, nil
	case consentDataAck:
		sequence, ok := c.verifyDataAck(payload)
		if ok {
			c.mu.Lock()
			waiter := c.pendingData[sequence]
			if waiter != nil {
				delete(c.pendingData, sequence)
				close(waiter)
			}
			c.mu.Unlock()
		}
		return 0, nil
	default:
		return 0, fmt.Errorf("unknown P2P consent record %d", kind)
	}
}

func (c *consentConn) dataAckPayload(sequence uint64, nonce []byte) []byte {
	token := fmt.Sprintf("%x:%d", nonce[:4], sequence)
	return controlPayloadWithKey(c.recvAckKey, c.session, consentDataAck, token)
}

func (c *consentConn) verifyDataAck(payload []byte) (uint64, bool) {
	token, ok := verifyControlWithKey(c.sendAckKey, c.session, consentDataAck, payload)
	if !ok {
		return 0, false
	}
	separator := strings.LastIndexByte(token, ':')
	if separator < 0 {
		return 0, false
	}
	sequence, err := strconv.ParseUint(token[separator+1:], 10, 64)
	return sequence, err == nil
}

func (c *consentConn) sealData(plaintext []byte) ([]byte, uint64, error) {
	nonce := make([]byte, c.sendAEAD.NonceSize())
	copy(nonce[:4], c.sendSalt[:])
	sequence := c.sendSeq.Add(1)
	binary.BigEndian.PutUint64(nonce[4:], sequence)
	return c.sendAEAD.Seal(nonce, nonce, plaintext, c.sendAAD), sequence, nil
}

func (c *consentConn) openData(payload []byte) ([]byte, uint64, error) {
	nonceSize := c.recvAEAD.NonceSize()
	if len(payload) < nonceSize+c.recvAEAD.Overhead() {
		return nil, 0, fmt.Errorf("truncated encrypted P2P data record")
	}
	nonce := payload[:nonceSize]
	plaintext, err := c.recvAEAD.Open(nil, nonce, payload[nonceSize:], c.recvAAD)
	if err != nil {
		return nil, 0, fmt.Errorf("authenticate P2P data record: %w", err)
	}
	sequence := binary.BigEndian.Uint64(nonce[4:])
	c.replayMu.Lock()
	defer c.replayMu.Unlock()
	var salt [4]byte
	copy(salt[:], nonce[:4])
	if !c.recvSaltSet {
		c.recvSalt, c.recvSaltSet = salt, true
	} else if salt != c.recvSalt {
		return nil, sequence, errConsentReplay
	}
	if sequence > c.recvHighest {
		shift := sequence - c.recvHighest
		if shift >= consentReplayWindowBits {
			c.recvWindow = 0
		} else {
			c.recvWindow <<= shift
		}
		c.recvHighest = sequence
		c.recvWindow |= 1
	} else {
		delta := c.recvHighest - sequence
		if delta >= consentReplayWindowBits || c.recvWindow&(uint64(1)<<delta) != 0 {
			return nil, sequence, errConsentReplay
		}
		c.recvWindow |= uint64(1) << delta
	}
	return plaintext, sequence, nil
}

func (c *consentConn) probeConsent(ctx context.Context) error {
	nonceBytes := make([]byte, 18)
	if _, err := rand.Read(nonceBytes); err != nil {
		return fmt.Errorf("generate consent nonce: %w", err)
	}
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)
	waiter := make(chan struct{})
	c.mu.Lock()
	if len(c.pending) >= maxConsentPending {
		c.mu.Unlock()
		return fmt.Errorf("too many pending consent probes")
	}
	c.pending[nonce] = waiter
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, nonce)
		c.mu.Unlock()
	}()
	if err := c.writeRecord(consentRequest, c.controlPayload(consentRequest, nonce)); err != nil {
		return err
	}
	select {
	case <-waiter:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *consentConn) readRecord() (byte, []byte, error) {
	record := make([]byte, maxConsentRecord+9)
	n, err := c.Conn.Read(record)
	if err != nil {
		return 0, nil, err
	}
	if n < 9 {
		return 0, nil, fmt.Errorf("truncated P2P consent record")
	}
	if bytes.HasPrefix(record[:n], []byte("XROK-P2P-PUNCH:")) {
		return 0, nil, errLatePunch
	}
	header := record[:9]
	if !hmac.Equal(header[:4], consentMagic[:]) {
		return 0, nil, fmt.Errorf("invalid P2P consent framing")
	}
	length := binary.BigEndian.Uint32(header[5:])
	if length > maxConsentRecord {
		return 0, nil, fmt.Errorf("P2P consent record too large: %d", length)
	}
	if n != 9+int(length) {
		return 0, nil, fmt.Errorf("invalid P2P consent record length: got %d want %d", n, 9+length)
	}
	payload := append([]byte(nil), record[9:n]...)
	return header[4], payload, nil
}

func (c *consentConn) writeRecord(kind byte, payload []byte) error {
	if len(payload) > maxConsentRecord {
		return fmt.Errorf("P2P consent record too large: %d", len(payload))
	}
	record := make([]byte, 9+len(payload))
	copy(record, consentMagic[:])
	record[4] = kind
	binary.BigEndian.PutUint32(record[5:9], uint32(len(payload)))
	copy(record[9:], payload)
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	n, err := c.Conn.Write(record)
	if err != nil {
		return err
	}
	if n != len(record) {
		return fmt.Errorf("short P2P datagram write: %d/%d", n, len(record))
	}
	return nil
}

func (c *consentConn) controlPayload(kind byte, nonce string) []byte {
	return controlPayloadWithKey(c.key, c.session, kind, nonce)
}

func (c *consentConn) verifyControl(kind byte, payload []byte) (string, bool) {
	for i := 0; i < len(payload); i++ {
		if payload[i] != ':' {
			continue
		}
		nonce := string(payload[:i])
		return nonce, nonce != "" && hmac.Equal(payload, c.controlPayload(kind, nonce))
	}
	return "", false
}

func controlPayloadWithKey(key []byte, session string, kind byte, token string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = fmt.Fprintf(mac, "%s:%d:%s", session, kind, token)
	return []byte(token + ":" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)))
}

func verifyControlWithKey(key []byte, session string, kind byte, payload []byte) (string, bool) {
	separator := bytes.LastIndexByte(payload, ':')
	if separator <= 0 {
		return "", false
	}
	token := string(payload[:separator])
	return token, hmac.Equal(payload, controlPayloadWithKey(key, session, kind, token))
}

// Keep compile-time assurance that deadlines remain delegated to the wrapped
// message transport, including the runtime's TURN relay connection.
var _ net.Conn = (*consentConn)(nil)
var _ ConsentProbe = RuntimeConsentProbe{}
