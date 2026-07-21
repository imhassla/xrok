package p2p

import (
	"context"
	"net"
	"testing"
	"time"
)

type fakeUDP struct{ out []byte }

func (f *fakeUDP) Write(p []byte) (int, error)     { return len(p), nil }
func (f *fakeUDP) Read(p []byte) (int, error)      { copy(p, f.out); return len(f.out), nil }
func (f *fakeUDP) SetReadDeadline(time.Time) error { return nil }
func (f *fakeUDP) Close() error                    { return nil }

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

func TestICEGatherSrflxViaInjectedDialer(t *testing.T) {
	resp := make([]byte, 32)
	resp[0], resp[1] = 1, 1
	resp[2], resp[3] = 0, 12
	// XOR-MAPPED-ADDRESS: family IPv4, port 3478^0x2112, 192.0.2.1^magic.
	resp[20], resp[21] = 0x00, 0x20
	resp[22], resp[23] = 0, 8
	resp[24] = 0
	resp[25] = 0x04
	p := uint16(3478) ^ 0x2112
	resp[26], resp[27] = byte(p>>8), byte(p)
	resp[28], resp[29], resp[30], resp[31] = 0xE1, 0x12, 0xA6, 0x43
	a := NewICEAgent(ICEConfig{STUNServer: "stun.test:3478", DialUDP: func(string, *net.UDPAddr, *net.UDPAddr) (udpConn, error) { return &fakeUDP{out: resp}, nil }})
	cs, err := a.GatherCandidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		if c.Type == CandidateSrflx {
			if c.Addr != "192.0.2.1:3478" {
				t.Fatalf("addr=%s", c.Addr)
			}
			return
		}
	}
	t.Fatal("missing srflx")
}

type fakeRelay struct{}

func (fakeRelay) Allocate(context.Context, string) (net.Conn, Candidate, error) {
	return nil, Candidate{Type: CandidateRelay, Addr: "turn:1"}, nil
}
func TestICEGatherRelayProvider(t *testing.T) {
	a := NewICEAgent(ICEConfig{Relay: fakeRelay{}})
	_, c, e := a.GatherRelay(context.Background(), "peer")
	if e != nil || c.Type != CandidateRelay {
		t.Fatalf("%v %v", c, e)
	}
}
