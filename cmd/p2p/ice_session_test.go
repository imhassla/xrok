package p2p

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordingChecker struct {
	mu       sync.Mutex
	attempts []CandidatePair
	succeed  string
	peers    []net.Conn
}

func (c *recordingChecker) Check(_ context.Context, local, remote Candidate) (net.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attempts = append(c.attempts, CandidatePair{Local: local, Remote: remote})
	if remote.ID != c.succeed {
		return nil, errors.New("unreachable")
	}
	client, peer := net.Pipe()
	c.peers = append(c.peers, peer)
	return client, nil
}

func (c *recordingChecker) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, peer := range c.peers {
		_ = peer.Close()
	}
}

type recordingRelay struct {
	mu       sync.Mutex
	calls    int
	peerID   string
	fail     bool
	peerConn net.Conn
}

func (r *recordingRelay) Allocate(_ context.Context, peerID string) (net.Conn, Candidate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.peerID = peerID
	if r.fail {
		return nil, Candidate{}, errors.New("allocation rejected")
	}
	client, peer := net.Pipe()
	r.peerConn = peer
	return client, Candidate{ID: "turn-1", Type: CandidateRelay, Addr: "192.0.2.20:50000", Priority: 1}, nil
}

func (r *recordingRelay) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.peerConn != nil {
		_ = r.peerConn.Close()
	}
}

func TestICEChecklistIsPriorityOrderedAndExcludesRelay(t *testing.T) {
	agent := NewICEAgent(ICEConfig{})
	local := []Candidate{
		{ID: "srflx", Type: CandidateSrflx, Addr: "198.51.100.1:1", Priority: 100},
		{ID: "host", Type: CandidateHost, Addr: "10.0.0.1:1", Priority: 126},
		{ID: "relay", Type: CandidateRelay, Addr: "192.0.2.1:1", Priority: 1},
	}
	remote := []Candidate{
		{ID: "remote-low", Type: CandidateSrflx, Addr: "198.51.100.2:2", Priority: 90},
		{ID: "remote-high", Type: CandidateHost, Addr: "10.0.0.2:2", Priority: 126},
	}
	pairs := agent.Checklist(local, remote)
	if len(pairs) != 4 {
		t.Fatalf("pairs=%d, want 4", len(pairs))
	}
	if pairs[0].Local.ID != "host" || pairs[0].Remote.ID != "remote-high" {
		t.Fatalf("first pair=%s/%s", pairs[0].Local.ID, pairs[0].Remote.ID)
	}
	for i, pair := range pairs {
		if pair.State != CandidatePairWaiting {
			t.Fatalf("pair %d state=%s", i, pair.State)
		}
		if pair.Local.Type == CandidateRelay || pair.Remote.Type == CandidateRelay {
			t.Fatal("relay must be allocated lazily")
		}
		if i > 0 && pairs[i-1].Priority < pair.Priority {
			t.Fatal("checklist is not priority ordered")
		}
	}
}

func TestICEConnectNominatesFirstSuccessfulPairWithoutTURN(t *testing.T) {
	checker := &recordingChecker{succeed: "remote-low"}
	defer checker.close()
	relay := &recordingRelay{}
	defer relay.close()
	agent := NewICEAgent(ICEConfig{Checker: checker, Relay: relay})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session, err := agent.Connect(ctx, "peer-b",
		[]Candidate{{ID: "local", Type: CandidateHost, Addr: "10.0.0.1:1", Priority: 126}},
		[]Candidate{
			{ID: "remote-high", Type: CandidateHost, Addr: "10.0.0.2:2", Priority: 126},
			{ID: "remote-low", Type: CandidateSrflx, Addr: "198.51.100.2:2", Priority: 100},
		})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if session.ViaRelay() || session.Pair().State != CandidatePairNominated || session.Pair().Remote.ID != "remote-low" {
		t.Fatalf("unexpected nomination: %#v relay=%v", session.Pair(), session.ViaRelay())
	}
	if relay.calls != 0 {
		t.Fatalf("TURN allocated despite successful direct check: %d", relay.calls)
	}
	if len(checker.attempts) != 2 {
		t.Fatalf("attempts=%d, want 2", len(checker.attempts))
	}
}

func TestICEConnectAllocatesTURNOnlyAfterDirectFailure(t *testing.T) {
	checker := &recordingChecker{succeed: "never"}
	relay := &recordingRelay{}
	defer relay.close()
	agent := NewICEAgent(ICEConfig{Checker: checker, Relay: relay})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session, err := agent.Connect(ctx, "peer-turn",
		[]Candidate{{ID: "local", Type: CandidateHost, Addr: "10.0.0.1:1"}},
		[]Candidate{{ID: "remote", Type: CandidateHost, Addr: "10.0.0.2:2"}})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if !session.ViaRelay() || session.Pair().Local.Type != CandidateRelay {
		t.Fatalf("expected nominated TURN pair, got %#v", session.Pair())
	}
	if relay.calls != 1 || relay.peerID != "peer-turn" {
		t.Fatalf("TURN calls=%d peer=%q", relay.calls, relay.peerID)
	}
	if len(checker.attempts) != 1 {
		t.Fatalf("direct checks=%d", len(checker.attempts))
	}
}

func TestICEConnectReportsDirectAndTURNFailures(t *testing.T) {
	checker := &recordingChecker{succeed: "never"}
	relay := &recordingRelay{fail: true}
	agent := NewICEAgent(ICEConfig{Checker: checker, Relay: relay})
	_, err := agent.Connect(context.Background(), "peer-fail",
		[]Candidate{{ID: "local", Type: CandidateHost, Addr: "10.0.0.1:1"}},
		[]Candidate{{ID: "remote", Type: CandidateHost, Addr: "10.0.0.2:2"}})
	if err == nil || !strings.Contains(err.Error(), "unreachable") || !strings.Contains(err.Error(), "allocation rejected") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestICEConsentFreshnessClosesAfterConsecutiveFailures(t *testing.T) {
	checker := &recordingChecker{succeed: "remote"}
	defer checker.close()
	ticks := make(chan time.Time, 4)
	probeResults := make(chan error, 4)
	agent := NewICEAgent(ICEConfig{
		Checker: checker, ConsentFailures: 2,
		TickSource:   func(context.Context, time.Duration) <-chan time.Time { return ticks },
		ConsentProbe: ConsentProbeFunc(func(context.Context, net.Conn) error { return <-probeResults }),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session, err := agent.Connect(ctx, "peer",
		[]Candidate{{ID: "local", Type: CandidateHost, Addr: "10.0.0.1:1"}},
		[]Candidate{{ID: "remote", Type: CandidateHost, Addr: "10.0.0.2:2"}})
	if err != nil {
		t.Fatal(err)
	}

	// A successful probe resets the consecutive failure counter.
	for _, result := range []error{errors.New("lost"), nil, errors.New("lost"), errors.New("lost again")} {
		probeResults <- result
		ticks <- time.Now()
	}
	select {
	case <-session.Done():
	case <-time.After(time.Second):
		t.Fatal("consent expiry did not close session")
	}
	if err := session.Err(); err == nil || !strings.Contains(err.Error(), "consent expired") {
		t.Fatalf("session error=%v", err)
	}
}

func TestICESessionClosesWhenParentContextEnds(t *testing.T) {
	checker := &recordingChecker{succeed: "remote"}
	defer checker.close()
	agent := NewICEAgent(ICEConfig{Checker: checker})
	ctx, cancel := context.WithCancel(context.Background())
	session, err := agent.Connect(ctx, "peer",
		[]Candidate{{ID: "local", Type: CandidateHost, Addr: "10.0.0.1:1"}},
		[]Candidate{{ID: "remote", Type: CandidateHost, Addr: "10.0.0.2:2"}})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-session.Done():
	case <-time.After(time.Second):
		t.Fatal("parent cancellation did not close ICE session")
	}
	if !errors.Is(session.Err(), context.Canceled) {
		t.Fatalf("session error=%v", session.Err())
	}
}

func TestPeerCandidatesAvoidsDuplicates(t *testing.T) {
	candidates := peerCandidates(&PeerInfo{PeerID: "p", PublicAddr: "127.0.0.1:1", LocalAddr: "127.0.0.1:1"})
	if len(candidates) != 1 || candidates[0].Type != CandidateSrflx {
		t.Fatalf("candidates=%#v", candidates)
	}
}
