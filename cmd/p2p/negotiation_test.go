package p2p

import (
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestICEOfferAnswerAndCandidateRoundTrip(t *testing.T) {
	candidate := Candidate{ID: "host-a", Type: CandidateHost, Addr: "127.0.0.1:41000", Priority: 126}
	for _, signal := range []*SignalMessage{
		NewICEOfferSignal("alice", "bob", "session-a", []Candidate{candidate}),
		NewICEAnswerSignal("bob", "alice", "session-a", []Candidate{candidate}),
		NewICECandidateSignal("alice", "bob", "session-a", candidate),
	} {
		data, err := json.Marshal(signal)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodeSignal(data)
		if err != nil {
			t.Fatalf("decode %s: %v", signal.Type, err)
		}
		if decoded.SessionID != "session-a" || len(decoded.Candidates) != 1 {
			t.Fatalf("signal binding lost: %#v", decoded)
		}
	}
}

func TestNegotiationCheckCASRejectsAnswerReplay(t *testing.T) {
	store := NewNegotiationStore(4, time.Minute)
	candidate := Candidate{ID: "host-a", Type: CandidateHost, Addr: "127.0.0.1:41000"}
	if _, err := store.Start("bob", NegotiationControlling, "session-a", []Candidate{candidate}, []Candidate{candidate}); err != nil {
		t.Fatal(err)
	}
	var winners atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := store.BeginCheck("bob", "session-a"); ok {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("connectivity check owners=%d, want 1", winners.Load())
	}
	if !store.Complete("bob", "session-a") {
		t.Fatal("checking negotiation did not complete")
	}
	replayed, err := store.Start("bob", NegotiationControlling, "session-a", []Candidate{candidate}, nil)
	if err != nil || replayed.State != NegotiationCompleted {
		t.Fatalf("completed replay changed state: %#v err=%v", replayed, err)
	}
	if _, ok := store.BeginCheck("bob", "session-a"); ok {
		t.Fatal("replayed answer started a second connectivity check")
	}
	if _, err := store.AddRemote("bob", "session-a", []Candidate{candidate}); err == nil {
		t.Fatal("completed session accepted replayed candidates")
	}
}

func TestICEOfferGlareHasSingleDeterministicController(t *testing.T) {
	aliceControls := ShouldInitiateICE("alice", "bob")
	bobControls := ShouldInitiateICE("bob", "alice")
	if aliceControls == bobControls {
		t.Fatalf("offer glare unresolved: alice=%t bob=%t", aliceControls, bobControls)
	}
	if !aliceControls {
		t.Fatal("stable lexical controller changed")
	}
}

func TestNegotiationStoreBindsSessionAndExpires(t *testing.T) {
	now := time.Unix(100, 0)
	store := NewNegotiationStore(1, time.Second)
	store.now = func() time.Time { return now }
	candidate := Candidate{ID: "host-a", Type: CandidateHost, Addr: "127.0.0.1:41000"}
	if _, err := store.Start("bob", NegotiationControlling, "session-a", []Candidate{candidate}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddRemote("bob", "session-b", []Candidate{candidate}); err == nil {
		t.Fatal("cross-session candidate accepted")
	}
	if _, err := store.Start("carol", NegotiationControlling, "session-c", []Candidate{candidate}, nil); err == nil {
		t.Fatal("bounded store accepted excess negotiation")
	}
	now = now.Add(2 * time.Second)
	if store.Len() != 0 {
		t.Fatal("expired negotiation retained")
	}
}

func TestCheckingNegotiationIsLifecycleOwnedNotTTLExpired(t *testing.T) {
	now := time.Unix(100, 0)
	store := NewNegotiationStore(1, time.Second)
	store.now = func() time.Time { return now }
	candidate := Candidate{ID: "host", Type: CandidateHost, Addr: "127.0.0.1:41000"}
	if _, err := store.Start("bob", NegotiationControlling, "session", []Candidate{candidate}, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.BeginCheck("bob", "session"); !ok {
		t.Fatal("checking ownership not acquired")
	}
	now = now.Add(time.Hour)
	if store.Len() != 1 {
		t.Fatal("active checking negotiation expired by TTL")
	}
	if !store.Complete("bob", "session") {
		t.Fatal("checking negotiation did not complete")
	}
	now = now.Add(2 * time.Second)
	if store.Len() != 0 {
		t.Fatal("completed negotiation tombstone did not expire")
	}
}

func TestICESignalRejectsUnboundedAndInvalidCandidates(t *testing.T) {
	invalid := NewICEOfferSignal("alice", "bob", "session", []Candidate{{ID: "bad", Type: CandidateHost, Addr: "not an address"}})
	data, _ := json.Marshal(invalid)
	if _, err := DecodeSignal(data); err == nil {
		t.Fatal("invalid candidate address accepted")
	}
	invalid.Candidates[0] = Candidate{ID: strings.Repeat("x", 129), Type: CandidateHost, Addr: "127.0.0.1"}
	data, _ = json.Marshal(invalid)
	if _, err := DecodeSignal(data); err == nil {
		t.Fatal("oversized candidate ID accepted")
	}
	for _, address := range []string{"127.0.0.1", "relay.example.test:3478"} {
		invalid.Candidates[0] = Candidate{ID: "candidate", Type: CandidateHost, Addr: address}
		data, _ = json.Marshal(invalid)
		if _, err := DecodeSignal(data); err == nil {
			t.Fatalf("non-literal IP:port candidate accepted: %q", address)
		}
	}
}

func TestICESignalRequiresCompleteCandidateBatches(t *testing.T) {
	for _, signal := range []*SignalMessage{
		NewICEOfferSignal("alice", "bob", "session", nil),
		NewICEAnswerSignal("bob", "alice", "session", nil),
		{Type: SignalTypeICECandidate, FromPeer: "alice", ToPeer: "bob", SessionID: "session"},
		{Type: SignalTypeICECandidate, FromPeer: "alice", ToPeer: "bob", SessionID: "session", Candidates: []Candidate{
			{ID: "one", Type: CandidateHost, Addr: "127.0.0.1:4001"},
			{ID: "two", Type: CandidateHost, Addr: "127.0.0.1:4002"},
		}},
	} {
		data, err := json.Marshal(signal)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeSignal(data); err == nil {
			t.Fatalf("incomplete ICE signal accepted: %#v", signal)
		}
	}
}
