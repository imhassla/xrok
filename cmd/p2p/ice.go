package p2p

// Lightweight ICE-style candidate negotiation.  The transport remains
// dependency-free, while deployments can plug in a real ICE/TURN provider.
// This makes the signalling contract testable locally and avoids pretending
// that UDP punching alone is a complete NAT traversal implementation.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
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
	// DialUDP allows deterministic tests and custom transports. When nil,
	// net.DialUDP is used.
	DialUDP func(network string, laddr, raddr *net.UDPAddr) (udpConn, error)
}

type udpConn interface {
	Write([]byte) (int, error)
	Read([]byte) (int, error)
	SetReadDeadline(time.Time) error
	Close() error
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
	deadline, cancel := context.WithTimeout(ctx, a.cfg.GatherTimeout)
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
	if a.cfg.STUNServer != "" {
		if c, err := a.gatherSrflx(deadline); err == nil {
			out = append(out, c)
		}
	}
	return out, nil
}

// GatherRelay obtains a TURN allocation through the injected provider. TURN
// failures are returned to the caller so deployments can choose whether to
// fall back to host/srflx candidates.
func (a *ICEAgent) GatherRelay(ctx context.Context, peerID string) (net.Conn, Candidate, error) {
	if a.cfg.Relay == nil {
		return nil, Candidate{}, fmt.Errorf("TURN relay provider is not configured")
	}
	return a.cfg.Relay.Allocate(ctx, peerID)
}

func (a *ICEAgent) gatherSrflx(ctx context.Context) (Candidate, error) {
	addr, err := net.ResolveUDPAddr("udp", a.cfg.STUNServer)
	if err != nil {
		// Deterministic/in-memory dialers do not need DNS resolution. Allow
		// them to exercise STUN parsing with synthetic hostnames in tests.
		if a.cfg.DialUDP == nil {
			return Candidate{}, err
		}
		addr = &net.UDPAddr{IP: net.IPv4zero, Port: 3478}
	}
	dial := a.cfg.DialUDP
	if dial == nil {
		dial = func(n string, l, r *net.UDPAddr) (udpConn, error) { return net.DialUDP(n, l, r) }
	}
	c, err := dial("udp", nil, addr)
	if err != nil {
		return Candidate{}, err
	}
	defer c.Close()
	var tx [12]byte
	if _, err := rand.Read(tx[:]); err != nil {
		return Candidate{}, fmt.Errorf("generate STUN transaction ID: %w", err)
	}
	p := make([]byte, 20)
	binary.BigEndian.PutUint16(p, 0x0001)
	binary.BigEndian.PutUint16(p[2:], 0)
	binary.BigEndian.PutUint32(p[4:], 0x2112A442)
	copy(p[8:], tx[:])
	if _, err = c.Write(p); err != nil {
		return Candidate{}, err
	}
	readDeadline := time.Now().Add(a.cfg.GatherTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(readDeadline) {
		readDeadline = ctxDeadline
	}
	if err := c.SetReadDeadline(readDeadline); err != nil {
		return Candidate{}, err
	}
	b := make([]byte, 1500)
	n, err := c.Read(b)
	if err != nil {
		return Candidate{}, err
	}
	if n < 20 || binary.BigEndian.Uint16(b) != 0x0101 {
		return Candidate{}, fmt.Errorf("invalid STUN response")
	}
	msgLen := int(binary.BigEndian.Uint16(b[2:4]))
	if msgLen%4 != 0 || msgLen > n-20 || binary.BigEndian.Uint32(b[4:8]) != 0x2112A442 || !bytes.Equal(b[8:20], tx[:]) {
		return Candidate{}, fmt.Errorf("invalid STUN response transaction")
	}
	end := 20 + msgLen
	for off := 20; off+4 <= end; {
		typ, ln := binary.BigEndian.Uint16(b[off:]), int(binary.BigEndian.Uint16(b[off+2:]))
		off += 4
		if off+ln > end {
			return Candidate{}, fmt.Errorf("truncated STUN attribute")
		}
		if typ == 0x0020 && ln >= 8 && b[off+1] == 0x01 {
			port := binary.BigEndian.Uint16(b[off+2:]) ^ 0x2112
			ip := net.IPv4(b[off+4]^0x21, b[off+5]^0x12, b[off+6]^0xA4, b[off+7]^0x42)
			return Candidate{ID: "srflx-" + ip.String(), Type: CandidateSrflx, Addr: net.JoinHostPort(ip.String(), fmt.Sprint(port)), Priority: 100}, nil
		}
		off += (ln + 3) &^ 3
	}
	return Candidate{}, fmt.Errorf("STUN mapped address not found")
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
