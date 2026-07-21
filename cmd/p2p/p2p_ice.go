package p2p

import (
	"context"
	"fmt"
	"net"
)

// EstablishICE integrates the authenticated P2P punch exchange with the ICE
// checklist and TURN fallback. Each connectivity check uses the candidate
// selected by the agent; the punch response authenticates the remote peer.
func (m *P2PManager) EstablishICE(ctx context.Context, agent *ICEAgent, peer *PeerInfo) (*ICESession, error) {
	if agent == nil {
		return nil, fmt.Errorf("ICE agent is required")
	}
	if peer == nil || peer.PeerID == "" {
		return nil, fmt.Errorf("P2P peer information is required")
	}
	local, err := agent.GatherCandidates(ctx)
	if err != nil {
		return nil, fmt.Errorf("gather local ICE candidates: %w", err)
	}
	remote := peerCandidates(peer)
	if len(remote) == 0 && agent.cfg.Relay == nil {
		return nil, fmt.Errorf("peer %q has no usable ICE candidates", peer.PeerID)
	}

	cfg := agent.cfg
	cfg.Checker = ConnectivityCheckFunc(func(checkCtx context.Context, _, candidate Candidate) (netConn net.Conn, err error) {
		candidatePeer := *peer
		candidatePeer.PublicAddr = candidate.Addr
		connection, err := m.PunchHole(checkCtx, &candidatePeer)
		if err != nil {
			return nil, err
		}
		return connection.Conn, nil
	})
	runtimeAgent := NewICEAgent(cfg)
	session, err := runtimeAgent.Connect(ctx, peer.PeerID, local, remote)
	if err != nil {
		return nil, err
	}
	go m.removeClosedICESession(peer.PeerID, session)
	return session, nil
}

func peerCandidates(peer *PeerInfo) []Candidate {
	var candidates []Candidate
	if peer.PublicAddr != "" {
		candidates = append(candidates, Candidate{
			ID: "peer-srflx-" + peer.PeerID, Type: CandidateSrflx, Addr: peer.PublicAddr, Priority: 100,
		})
	}
	if peer.LocalAddr != "" && peer.LocalAddr != peer.PublicAddr {
		candidates = append(candidates, Candidate{
			ID: "peer-host-" + peer.PeerID, Type: CandidateHost, Addr: peer.LocalAddr, Priority: 126,
		})
	}
	return candidates
}

func (m *P2PManager) removeClosedICESession(peerID string, session *ICESession) {
	<-session.Done()
	m.mu.Lock()
	defer m.mu.Unlock()
	if connection, ok := m.connections[peerID]; ok && connection.Conn == session.Conn() {
		delete(m.connections, peerID)
	}
}
