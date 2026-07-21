package p2p

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	punchProtocolVersion = "v2"
	defaultPunchMaxSkew  = 30 * time.Second
	defaultReplayEntries = 4096
)

// PunchReplayCache provides bounded, session-aware replay protection for UDP
// connectivity probes. Entries expire after maxSkew and the cache evicts the
// oldest entry when its fixed capacity is reached.
type PunchReplayCache struct {
	mu       sync.Mutex
	seen     map[string]time.Time
	capacity int
}

func NewPunchReplayCache(capacity int) *PunchReplayCache {
	if capacity <= 0 {
		capacity = defaultReplayEntries
	}
	return &PunchReplayCache{seen: make(map[string]time.Time), capacity: capacity}
}

func (c *PunchReplayCache) consume(key string, now time.Time, ttl time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, expires := range c.seen {
		if !expires.After(now) {
			delete(c.seen, k)
		}
	}
	if expires, ok := c.seen[key]; ok && expires.After(now) {
		return false
	}
	if len(c.seen) >= c.capacity {
		var oldestKey string
		var oldest time.Time
		for k, expires := range c.seen {
			if oldestKey == "" || expires.Before(oldest) {
				oldestKey, oldest = k, expires
			}
		}
		delete(c.seen, oldestKey)
	}
	c.seen[key] = now.Add(ttl)
	return true
}

func newPunchSessionID() string {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func newPunchNonce() string {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func punchMACInput(peerID, sessionID string, timestamp int64, nonce string) string {
	return strings.Join([]string{punchProtocolVersion, peerID, sessionID, strconv.FormatInt(timestamp, 10), nonce}, ":")
}

func authenticatedPunchMessage(token, peerID, sessionID string, now time.Time) ([]byte, error) {
	if token == "" || peerID == "" || sessionID == "" {
		return nil, fmt.Errorf("P2P punch token, peer and session are required")
	}
	nonce := newPunchNonce()
	if nonce == "" {
		return nil, fmt.Errorf("failed to generate P2P punch nonce")
	}
	ts := now.UnixMilli()
	input := punchMACInput(peerID, sessionID, ts, nonce)
	h := hmac.New(sha256.New, []byte(token))
	h.Write([]byte(input))
	mac := base64.RawURLEncoding.EncodeToString(h.Sum(nil))
	return []byte("XROK-P2P-PUNCH:" + input + ":" + mac), nil
}

func validateAuthenticatedPunch(msg, token, expectedPeer, expectedSession string, now time.Time, maxSkew time.Duration, replay *PunchReplayCache) bool {
	if token == "" || expectedPeer == "" || expectedSession == "" || replay == nil || maxSkew <= 0 {
		return false
	}
	parts := strings.Split(msg, ":")
	if len(parts) != 7 || parts[0] != "XROK-P2P-PUNCH" || parts[1] != punchProtocolVersion ||
		parts[2] != expectedPeer || parts[3] != expectedSession || parts[5] == "" {
		return false
	}
	ts, err := strconv.ParseInt(parts[4], 10, 64)
	if err != nil {
		return false
	}
	issued := time.UnixMilli(ts)
	if issued.Before(now.Add(-maxSkew)) || issued.After(now.Add(maxSkew)) {
		return false
	}
	input := punchMACInput(parts[2], parts[3], ts, parts[5])
	h := hmac.New(sha256.New, []byte(token))
	h.Write([]byte(input))
	want := base64.RawURLEncoding.EncodeToString(h.Sum(nil))
	if !hmac.Equal([]byte(parts[6]), []byte(want)) {
		return false
	}
	// Bind the replay key to the authenticated session and sender. The MAC is
	// included so distinct valid messages cannot alias through crafted fields.
	return replay.consume(expectedPeer+"\x00"+expectedSession+"\x00"+parts[5]+"\x00"+parts[6], now, maxSkew)
}
