package p2p

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"
)

type fakePendingRelay struct {
	mu        sync.Mutex
	conn      net.Conn
	wantPeer  string
	connected bool
	closed    bool
}

func (p *fakePendingRelay) Connect(ctx context.Context, peer *net.UDPAddr) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.connected {
		return nil, net.ErrClosed
	}
	if peer == nil || peer.String() != p.wantPeer {
		return nil, fmt.Errorf("unexpected relay peer %v, want %s", peer, p.wantPeer)
	}
	p.connected = true
	return p.conn, nil
}

func (p *fakePendingRelay) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.connected && !p.closed {
		p.closed = true
		return p.conn.Close()
	}
	return nil
}

func TestBilateralRelayCandidatesAuthenticateBeforeNomination(t *testing.T) {
	rawAlice, rawBob := messagePipe()
	alicePending := &fakePendingRelay{conn: rawAlice, wantPeer: "192.0.2.20:52000"}
	bobPending := &fakePendingRelay{conn: rawBob, wantPeer: "192.0.2.10:51000"}
	aliceRelay := Candidate{ID: "relay-alice", Type: CandidateRelay, Addr: "192.0.2.10:51000", Priority: 1}
	bobRelay := Candidate{ID: "relay-bob", Type: CandidateRelay, Addr: "192.0.2.20:52000", Priority: 1}
	alice := NewP2PManagerWithToken("alice", "", "alice-relay-token")
	bob := NewP2PManagerWithToken("bob", "", "bob-relay-token")
	negotiationID := "bilateral-relay-session"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	aliceCandidates, err := alice.PrepareICEWithRelay(ctx, negotiationID, "", func(context.Context) (PendingRelayAllocation, Candidate, error) {
		return alicePending, aliceRelay, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	bobCandidates, err := bob.PrepareICEWithRelay(ctx, negotiationID, "", func(context.Context) (PendingRelayAllocation, Candidate, error) {
		return bobPending, bobRelay, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	aliceCandidates = relayOnly(aliceCandidates)
	bobCandidates = relayOnly(bobCandidates)
	type result struct {
		session *ICESession
		err     error
	}
	aliceResult, bobResult := make(chan result, 1), make(chan result, 1)
	go func() {
		session, connectErr := alice.EstablishPreparedICE(ctx, NewICEAgent(ICEConfig{CheckTimeout: time.Second}), bob.SignalingPeerInfo(), negotiationID, aliceCandidates, bobCandidates)
		aliceResult <- result{session, connectErr}
	}()
	go func() {
		session, connectErr := bob.EstablishPreparedICE(ctx, NewICEAgent(ICEConfig{CheckTimeout: time.Second}), alice.SignalingPeerInfo(), negotiationID, bobCandidates, aliceCandidates)
		bobResult <- result{session, connectErr}
	}()
	a, b := <-aliceResult, <-bobResult
	if a.err != nil || b.err != nil {
		t.Fatalf("bilateral relay failed: alice=%v bob=%v", a.err, b.err)
	}
	defer a.session.Close()
	defer b.session.Close()
	if !a.session.ViaRelay() || !b.session.ViaRelay() || a.session.Pair().Local.Type != CandidateRelay || a.session.Pair().Remote.Type != CandidateRelay {
		t.Fatalf("non-relay pair nominated: alice=%#v bob=%#v", a.session.Pair(), b.session.Pair())
	}
	if !alicePending.connected || !bobPending.connected || alicePending.closed || bobPending.closed {
		t.Fatalf("pending ownership not transferred: alice=%#v bob=%#v", alicePending, bobPending)
	}
}

func TestPreparedRelayCleanupOnDirectTransferAndError(t *testing.T) {
	for _, mode := range []string{"direct", "error", "manager-close"} {
		t.Run(mode, func(t *testing.T) {
			raw, peer := messagePipe()
			defer peer.Close()
			pending := &fakePendingRelay{conn: raw, wantPeer: "192.0.2.20:52000"}
			manager := NewP2PManagerWithToken("alice", "", "token")
			ctx := context.Background()
			candidates, err := manager.PrepareICEWithRelay(ctx, mode, "", func(context.Context) (PendingRelayAllocation, Candidate, error) {
				return pending, Candidate{ID: "relay", Type: CandidateRelay, Addr: "192.0.2.10:51000"}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if mode == "direct" {
				conn, ok := manager.preparedSocket(mode)
				if !ok {
					t.Fatal("prepared direct socket missing")
				}
				manager.transferPreparedSocket(mode, conn)
			} else if mode == "error" {
				_, err = manager.EstablishPreparedICE(ctx, NewICEAgent(ICEConfig{}), &PeerInfo{PeerID: "bob", AuthToken: "b", SessionID: "s"}, mode, relayOnly(candidates), nil)
				if err == nil {
					t.Fatal("missing remote relay candidate accepted")
				}
			} else if err := manager.Close(); err != nil {
				t.Fatal(err)
			}
			if !pending.closed || pending.connected {
				t.Fatalf("pending allocation leaked: %#v", pending)
			}
		})
	}
}

func TestConsentMaterialBindsNegotiationSession(t *testing.T) {
	manager := NewP2PManagerWithToken("alice", "", "alice-token")
	peer := NewP2PManagerWithToken("bob", "", "bob-token").SignalingPeerInfo()
	keyA, sessionA := manager.consentMaterial(peer, "negotiation-a")
	keyB, sessionB := manager.consentMaterial(peer, "negotiation-b")
	if string(keyA) == string(keyB) || sessionA == sessionB {
		t.Fatal("consent material is not negotiation-bound")
	}
}

func TestClaimedPreparedSessionIgnoresTTLUntilLifecycleCleanup(t *testing.T) {
	manager := NewP2PManagerWithToken("alice", "", "token")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := manager.PrepareICE(ctx, "claimed", ""); err != nil {
		t.Fatal(err)
	}
	conn, ok := manager.claimPrepared("claimed")
	if !ok || conn == nil {
		t.Fatal("prepared session could not be claimed")
	}
	if _, second := manager.claimPrepared("claimed"); second {
		t.Fatal("prepared session was claimed twice")
	}
	manager.mu.Lock()
	manager.preparedICE["claimed"].expires = time.Now().Add(-time.Hour)
	manager.expirePreparedLocked(time.Now())
	_, retained := manager.preparedICE["claimed"]
	manager.mu.Unlock()
	if !retained {
		t.Fatal("TTL expired lifecycle-owned prepared session")
	}
	manager.CancelPreparedICE("claimed")
}

func TestRelayPhaseTimeoutCleansClaimedPrepared(t *testing.T) {
	raw, silentPeer := messagePipe()
	defer silentPeer.Close()
	pending := &fakePendingRelay{conn: raw, wantPeer: "192.0.2.20:52000"}
	manager := NewP2PManagerWithToken("alice", "", "alice-token")
	peer := NewP2PManagerWithToken("bob", "", "bob-token").SignalingPeerInfo()
	local, err := manager.PrepareICEWithRelay(context.Background(), "timeout", "", func(context.Context) (PendingRelayAllocation, Candidate, error) {
		return pending, Candidate{ID: "local-relay", Type: CandidateRelay, Addr: "192.0.2.10:51000"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	remote := []Candidate{{ID: "remote-relay", Type: CandidateRelay, Addr: "192.0.2.20:52000"}}
	started := time.Now()
	_, err = manager.EstablishPreparedICE(context.Background(), NewICEAgent(ICEConfig{ChecklistTimeout: 30 * time.Millisecond}), peer, "timeout", relayOnly(local), remote)
	if err == nil {
		t.Fatal("silent relay peer unexpectedly nominated")
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("relay phase exceeded bounded timeout: %s", elapsed)
	}
	manager.mu.RLock()
	_, retained := manager.preparedICE["timeout"]
	manager.mu.RUnlock()
	if retained {
		t.Fatal("timed-out claimed prepared allocation retained")
	}
	select {
	case <-raw.closed:
	default:
		t.Fatal("connected relay transport not closed after authentication timeout")
	}
}

func relayOnly(candidates []Candidate) []Candidate {
	var out []Candidate
	for _, candidate := range candidates {
		if candidate.Type == CandidateRelay {
			out = append(out, candidate)
		}
	}
	return out
}
