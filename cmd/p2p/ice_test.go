package p2p

import (
	"context"
	"testing"
)

func TestICEGatherCandidatesHasHost(t *testing.T) {
	a := NewICEAgent(ICEConfig{})
	cs, err := a.GatherCandidates(context.Background())
	if err != nil || len(cs) == 0 {
		t.Fatalf("gather: %v %#v", err, cs)
	}
	found := false
	for _, c := range cs {
		if c.Type == CandidateHost {
			found = true
		}
	}
	if !found {
		t.Fatal("missing host candidate")
	}
}

func TestICESelectPrefersRelay(t *testing.T) {
	a := NewICEAgent(ICEConfig{})
	l, r, err := a.Select([]Candidate{{Type: CandidateHost, Addr: "a"}, {Type: CandidateRelay, Addr: "ra"}}, []Candidate{{Type: CandidateHost, Addr: "b"}, {Type: CandidateRelay, Addr: "rb"}})
	if err != nil || l.Type != CandidateRelay || r.Type != CandidateRelay {
		t.Fatalf("selection=%v,%v err=%v", l, r, err)
	}
}

func TestICESelectNoPair(t *testing.T) {
	if _, _, err := NewICEAgent(ICEConfig{}).Select([]Candidate{{Type: CandidateHost}}, []Candidate{{Type: CandidateSrflx}}); err == nil {
		t.Fatal("expected no compatible candidates")
	}
}
