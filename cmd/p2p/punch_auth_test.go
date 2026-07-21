package p2p

import (
	"strings"
	"testing"
	"time"
)

func TestAuthenticatedPunchIsSessionBoundAndSingleUse(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	msg, err := authenticatedPunchMessage("secret", "peer-a", "session-a", now)
	if err != nil {
		t.Fatal(err)
	}
	cache := NewPunchReplayCache(8)
	if !validateAuthenticatedPunch(string(msg), "secret", "peer-a", "session-a", now, time.Minute, cache) {
		t.Fatal("valid punch rejected")
	}
	if validateAuthenticatedPunch(string(msg), "secret", "peer-a", "session-a", now, time.Minute, cache) {
		t.Fatal("replayed punch accepted")
	}
	if validateAuthenticatedPunch(string(msg), "secret", "peer-a", "other-session", now, time.Minute, NewPunchReplayCache(8)) {
		t.Fatal("cross-session replay accepted")
	}
}

func TestAuthenticatedPunchRejectsTamperingAndStaleTimestamp(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	msg, err := authenticatedPunchMessage("secret", "peer-a", "session-a", now)
	if err != nil {
		t.Fatal(err)
	}
	badMAC := append([]byte(nil), msg...)
	if badMAC[len(badMAC)-1] == 'A' {
		badMAC[len(badMAC)-1] = 'B'
	} else {
		badMAC[len(badMAC)-1] = 'A'
	}
	tests := []struct {
		name string
		msg  string
		now  time.Time
	}{
		{"stale", string(msg), now.Add(2 * time.Minute)},
		{"future", string(msg), now.Add(-2 * time.Minute)},
		{"wrong peer", string(msg), now},
		{"bad mac", string(badMAC), now},
		{"extra field", string(msg) + ":extra", now},
		{"legacy", "XROK-P2P-PUNCH:v1:peer-a:proof", now},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			peer := "peer-a"
			if strings.Contains(tt.name, "wrong peer") {
				peer = "peer-b"
			}
			if validateAuthenticatedPunch(tt.msg, "secret", peer, "session-a", tt.now, time.Minute, NewPunchReplayCache(8)) {
				t.Fatal("invalid punch accepted")
			}
		})
	}
}

func TestPunchReplayCacheIsBounded(t *testing.T) {
	c := NewPunchReplayCache(2)
	now := time.Now()
	if !c.consume("one", now, time.Minute) || !c.consume("two", now, time.Minute) || !c.consume("three", now, time.Minute) {
		t.Fatal("fresh values rejected")
	}
	if len(c.seen) != 2 {
		t.Fatalf("cache size=%d, want 2", len(c.seen))
	}
}
