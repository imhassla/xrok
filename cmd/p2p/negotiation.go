package p2p

import (
	"fmt"
	"sync"
	"time"
)

type NegotiationRole string
type NegotiationState string

const (
	NegotiationControlling NegotiationRole  = "controlling"
	NegotiationControlled  NegotiationRole  = "controlled"
	NegotiationPending     NegotiationState = "pending"
	NegotiationChecking    NegotiationState = "checking"
	NegotiationCompleted   NegotiationState = "completed"
)

type Negotiation struct {
	PeerID           string
	SessionID        string
	Role             NegotiationRole
	State            NegotiationState
	LocalCandidates  []Candidate
	RemoteCandidates []Candidate
	ExpiresAt        time.Time
}

// NegotiationStore bounds concurrent ICE negotiations and rejects messages
// that are stale or not bound to the peer/session that created the exchange.
type NegotiationStore struct {
	mu       sync.Mutex
	entries  map[string]*Negotiation
	capacity int
	ttl      time.Duration
	now      func() time.Time
}

func NewNegotiationStore(capacity int, ttl time.Duration) *NegotiationStore {
	if capacity <= 0 {
		capacity = 128
	}
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &NegotiationStore{entries: make(map[string]*Negotiation), capacity: capacity, ttl: ttl, now: time.Now}
}

func (s *NegotiationStore) Start(peerID string, role NegotiationRole, sessionID string, local, remote []Candidate) (*Negotiation, error) {
	if peerID == "" || sessionID == "" {
		return nil, fmt.Errorf("ICE peer and session are required")
	}
	if role != NegotiationControlling && role != NegotiationControlled {
		return nil, fmt.Errorf("invalid ICE negotiation role %q", role)
	}
	if len(local) > MaxICECandidates || len(remote) > MaxICECandidates {
		return nil, fmt.Errorf("ICE candidate limit exceeded")
	}
	for _, candidate := range append(cloneCandidates(local), remote...) {
		if err := ValidateCandidate(candidate); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.expireLocked(now)
	if existing, ok := s.entries[peerID]; ok {
		if existing.SessionID != sessionID {
			return nil, fmt.Errorf("ICE negotiation already active for peer %q", peerID)
		}
		// Replayed offer/start is idempotent and must never roll a checking or
		// completed session back to pending.
		return cloneNegotiation(existing), nil
	}
	if _, ok := s.entries[peerID]; !ok && len(s.entries) >= s.capacity {
		return nil, fmt.Errorf("ICE negotiation capacity reached")
	}
	n := &Negotiation{PeerID: peerID, SessionID: sessionID, Role: role, State: NegotiationPending, LocalCandidates: cloneCandidates(local), RemoteCandidates: cloneCandidates(remote), ExpiresAt: now.Add(s.ttl)}
	s.entries[peerID] = n
	return cloneNegotiation(n), nil
}

func (s *NegotiationStore) AddRemote(peerID, sessionID string, candidates []Candidate) (*Negotiation, error) {
	if len(candidates) == 0 || len(candidates) > MaxICECandidates {
		return nil, fmt.Errorf("invalid ICE candidate batch")
	}
	for _, candidate := range candidates {
		if err := ValidateCandidate(candidate); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.expireLocked(now)
	n, ok := s.entries[peerID]
	if !ok || n.SessionID != sessionID {
		return nil, fmt.Errorf("unknown or stale ICE negotiation")
	}
	if n.State != NegotiationPending {
		return nil, fmt.Errorf("ICE negotiation is not accepting candidates")
	}
	for _, candidate := range candidates {
		duplicate := false
		for _, existing := range n.RemoteCandidates {
			if existing.ID == candidate.ID && existing.Addr == candidate.Addr {
				duplicate = true
				break
			}
		}
		if !duplicate {
			if len(n.RemoteCandidates) >= MaxICECandidates {
				return nil, fmt.Errorf("ICE candidate limit exceeded")
			}
			n.RemoteCandidates = append(n.RemoteCandidates, candidate)
		}
	}
	n.ExpiresAt = now.Add(s.ttl)
	return cloneNegotiation(n), nil
}

// BeginCheck atomically grants exactly one goroutine ownership of connectivity
// checks for a peer/session. Replayed answers and offers observe false.
func (s *NegotiationStore) BeginCheck(peerID, sessionID string) (*Negotiation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.expireLocked(now)
	n, ok := s.entries[peerID]
	if !ok || n.SessionID != sessionID || n.State != NegotiationPending {
		return nil, false
	}
	n.State = NegotiationChecking
	n.ExpiresAt = now.Add(s.ttl)
	return cloneNegotiation(n), true
}

func (s *NegotiationStore) Complete(peerID, sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.entries[peerID]
	if !ok || n.SessionID != sessionID || n.State != NegotiationChecking {
		return false
	}
	n.State = NegotiationCompleted
	n.ExpiresAt = s.now().Add(s.ttl)
	return true
}

func (s *NegotiationStore) Fail(peerID, sessionID string) {
	s.Delete(peerID, sessionID)
}

func (s *NegotiationStore) Get(peerID, sessionID string) (*Negotiation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(s.now())
	n, ok := s.entries[peerID]
	if !ok || n.SessionID != sessionID {
		return nil, false
	}
	return cloneNegotiation(n), true
}

func (s *NegotiationStore) Delete(peerID, sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n, ok := s.entries[peerID]; ok && n.SessionID == sessionID {
		delete(s.entries, peerID)
	}
}

func (s *NegotiationStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(s.now())
	return len(s.entries)
}

func (s *NegotiationStore) expireLocked(now time.Time) {
	for peer, n := range s.entries {
		if n.State == NegotiationChecking {
			continue
		}
		if !n.ExpiresAt.After(now) {
			delete(s.entries, peer)
		}
	}
}

func cloneCandidates(in []Candidate) []Candidate { return append([]Candidate(nil), in...) }

func cloneNegotiation(n *Negotiation) *Negotiation {
	out := *n
	out.LocalCandidates = cloneCandidates(n.LocalCandidates)
	out.RemoteCandidates = cloneCandidates(n.RemoteCandidates)
	return &out
}
