package p2p

// Lightweight ICE-style candidate negotiation.  The transport remains
// dependency-free, while deployments can plug in a real ICE/TURN provider.
// This makes the signalling contract testable locally and avoids pretending
// that UDP punching alone is a complete NAT traversal implementation.

import (
	"context"
	"fmt"
	"net"
	"time"
)

type CandidateType string

const (
	CandidateHost  CandidateType = "host"
	CandidateSrflx CandidateType = "srflx"
	CandidateRelay CandidateType = "relay"
)

type Candidate struct {
	ID       string        `json:"id"`
	Type     CandidateType `json:"type"`
	Addr     string        `json:"addr"`
	Priority uint32        `json:"priority"`
}

// RelayProvider is implemented by TURN adapters.  Implementations must
// return an already authenticated, connected net.Conn.
type RelayProvider interface {
	Allocate(ctx context.Context, peerID string) (net.Conn, Candidate, error)
}

type ICEConfig struct {
	STUNServer    string
	Relay         RelayProvider
	GatherTimeout time.Duration
}

type ICEAgent struct{ cfg ICEConfig }

func NewICEAgent(cfg ICEConfig) *ICEAgent {
	if cfg.GatherTimeout <= 0 {
		cfg.GatherTimeout = 3 * time.Second
	}
	return &ICEAgent{cfg: cfg}
}

// GatherCandidates always returns host candidates and optionally a server
// reflexive candidate. Failure to reach STUN is non-fatal.
func (a *ICEAgent) GatherCandidates(ctx context.Context) ([]Candidate, error) {
	_, cancel := context.WithTimeout(ctx, a.cfg.GatherTimeout)
	defer cancel()
	var out []Candidate
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for _, in := range ifaces {
		addrs, _ := in.Addrs()
		for _, x := range addrs {
			host, _, e := net.ParseCIDR(x.String())
			if e == nil && host != nil {
				out = append(out, Candidate{ID: fmt.Sprintf("host-%s", host), Type: CandidateHost, Addr: host.String(), Priority: 126})
			}
		}
	}
	if len(out) == 0 {
		out = append(out, Candidate{ID: "host-loopback", Type: CandidateHost, Addr: "127.0.0.1", Priority: 126})
	}
	return out, nil
}

func (a *ICEAgent) Select(local, remote []Candidate) (Candidate, Candidate, error) {
	order := []CandidateType{CandidateRelay, CandidateSrflx, CandidateHost}
	for _, typ := range order {
		for _, l := range local {
			if l.Type != typ {
				continue
			}
			for _, r := range remote {
				if r.Type == typ {
					return l, r, nil
				}
			}
		}
	}
	// A relay can still connect to any candidate when the peer has no relay
	// allocation; prefer this only after same-type pairs were exhausted.
	for _, l := range local {
		for _, r := range remote {
			if l.Type == CandidateRelay || r.Type == CandidateRelay {
				return l, r, nil
			}
		}
	}
	return Candidate{}, Candidate{}, fmt.Errorf("no compatible ICE candidates")
}
