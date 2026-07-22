package p2p

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"
)

// EstablishICE integrates the authenticated P2P punch exchange with the ICE
// checklist and TURN fallback. Each connectivity check uses the candidate
// selected by the agent; the punch response authenticates the remote peer.
func (m *P2PManager) EstablishICE(ctx context.Context, agent *ICEAgent, peer *PeerInfo) (*ICESession, error) {
	return m.EstablishICECandidates(ctx, agent, peer, peerCandidates(peer))
}

// EstablishICECandidates uses candidates exchanged by the authenticated
// offer/answer flow instead of inferring the remote checklist from PeerInfo.
func (m *P2PManager) EstablishICECandidates(ctx context.Context, agent *ICEAgent, peer *PeerInfo, remote []Candidate) (*ICESession, error) {
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
	return m.EstablishICENegotiated(ctx, agent, peer, local, remote)
}

// EstablishICENegotiated connects the exact local and remote candidate sets
// bound by an ICE offer/answer exchange.
func (m *P2PManager) EstablishICENegotiated(ctx context.Context, agent *ICEAgent, peer *PeerInfo, local, remote []Candidate) (*ICESession, error) {
	return m.establishICENegotiated(ctx, agent, peer, "", local, remote)
}

// EstablishPreparedICE performs checks through the negotiation-owned socket
// whose exact port was advertised in the offer/answer candidate set.
func (m *P2PManager) EstablishPreparedICE(ctx context.Context, agent *ICEAgent, peer *PeerInfo, negotiationID string, local, remote []Candidate) (*ICESession, error) {
	if negotiationID == "" {
		return nil, fmt.Errorf("ICE negotiation session is required")
	}
	return m.establishICENegotiated(ctx, agent, peer, negotiationID, local, remote)
}

func (m *P2PManager) establishICENegotiated(ctx context.Context, agent *ICEAgent, peer *PeerInfo, negotiationID string, local, remote []Candidate) (*ICESession, error) {
	if agent == nil {
		if negotiationID != "" {
			m.CancelPreparedICE(negotiationID)
		}
		return nil, fmt.Errorf("ICE agent is required")
	}
	if peer == nil || peer.PeerID == "" {
		if negotiationID != "" {
			m.CancelPreparedICE(negotiationID)
		}
		return nil, fmt.Errorf("P2P peer information is required")
	}
	if len(remote) == 0 && negotiationID == "" && agent.cfg.Relay == nil {
		return nil, fmt.Errorf("peer %q has no usable ICE candidates", peer.PeerID)
	}

	cfg := agent.cfg
	cfg.ConsentKey, cfg.ConsentSession = m.consentMaterial(peer, negotiationID)
	if len(cfg.ConsentKey) != 0 {
		cfg.ConsentProbe = RuntimeConsentProbe{}
		cfg.consentSendLabel = m.peerID
		cfg.consentReceiveLabel = peer.PeerID
	}
	if negotiationID != "" {
		// Bilateral relay candidates are handled from the negotiation-owned
		// pending allocation below. Never invoke the legacy peer-ID resolver.
		cfg.Relay = nil
		prepared, ok := m.claimPrepared(negotiationID)
		if !ok {
			return nil, fmt.Errorf("prepared ICE socket not found or expired")
		}
		cfg.Checker = ConnectivityCheckFunc(func(checkCtx context.Context, _, candidate Candidate) (net.Conn, error) {
			return m.punchPrepared(checkCtx, prepared, peer, candidate)
		})
		// Every advertised host/srflx address shares this one prepared UDP
		// socket, and the checker uses only the remote endpoint. Repeating the
		// remote checklist once per local interface only burns the global budget.
		local = onePreparedChecklistCandidate(local)
	} else {
		cfg.Checker = ConnectivityCheckFunc(func(checkCtx context.Context, _, candidate Candidate) (netConn net.Conn, err error) {
			candidatePeer := *peer
			candidatePeer.PublicAddr = candidate.Addr
			connection, err := m.PunchHole(checkCtx, &candidatePeer)
			if err != nil {
				return nil, err
			}
			return connection.Conn, nil
		})
	}
	runtimeAgent := NewICEAgent(cfg)
	session, directErr := runtimeAgent.Connect(ctx, peer.PeerID, local, remote)
	if directErr != nil && negotiationID != "" {
		session, directErr = m.connectPreparedRelay(ctx, runtimeAgent, peer, negotiationID, remote, directErr)
	}
	if directErr != nil {
		if negotiationID != "" {
			m.CancelPreparedICE(negotiationID)
		}
		return nil, directErr
	}
	if negotiationID != "" {
		if !session.ViaRelay() {
			if udp, ok := unwrapUDPConn(session.Conn()); ok {
				m.transferPreparedSocket(negotiationID, udp)
			}
		}
	}
	m.mu.Lock()
	m.connections[peer.PeerID] = &P2PConnection{PeerID: peer.PeerID, Conn: session.Conn(), LocalAddr: session.LocalAddr().String(), RemoteAddr: session.RemoteAddr().String(), Established: time.Now()}
	m.mu.Unlock()
	go m.removeClosedICESession(peer.PeerID, session)
	return session, nil
}

func onePreparedChecklistCandidate(candidates []Candidate) []Candidate {
	for _, candidate := range candidates {
		if candidate.Type != CandidateRelay {
			return []Candidate{candidate}
		}
	}
	return nil
}

func (m *P2PManager) connectPreparedRelay(ctx context.Context, agent *ICEAgent, peer *PeerInfo, negotiationID string, remote []Candidate, directErr error) (*ICESession, error) {
	relayCtx, cancelRelay := context.WithTimeout(ctx, agent.cfg.ChecklistTimeout)
	defer cancelRelay()
	pending, localRelay, ok := m.preparedRelay(negotiationID)
	if !ok {
		return nil, directErr
	}
	var remoteRelay Candidate
	for _, candidate := range remote {
		if candidate.Type == CandidateRelay {
			remoteRelay = candidate
			break
		}
	}
	if remoteRelay.Addr == "" {
		return nil, errors.Join(directErr, fmt.Errorf("peer has no negotiated relay candidate"))
	}
	remoteAddr, err := net.ResolveUDPAddr("udp", remoteRelay.Addr)
	if err != nil || remoteAddr.IP == nil {
		return nil, errors.Join(directErr, fmt.Errorf("invalid remote relay candidate"))
	}
	relayConn, err := pending.Connect(relayCtx, remoteAddr)
	if err != nil {
		return nil, errors.Join(directErr, fmt.Errorf("connect TURN relay pair: %w", err))
	}
	checked, err := m.punchConnected(relayCtx, relayConn, peer)
	if err != nil {
		_ = relayConn.Close()
		return nil, errors.Join(directErr, fmt.Errorf("authenticate TURN relay pair: %w", err))
	}
	pair := CandidatePair{Local: localRelay, Remote: remoteRelay, Priority: pairPriority(localRelay, remoteRelay), State: CandidatePairNominated}
	// The nominated session inherits the original runtime context; only the
	// connect/authentication phase is constrained by the relay budget.
	session := agent.newSession(ctx, pair, checked, true)
	m.transferPreparedRelay(negotiationID, pending)
	return session, nil
}

func (m *P2PManager) punchConnected(ctx context.Context, conn net.Conn, peer *PeerInfo) (net.Conn, error) {
	probe, err := authenticatedPunchMessage(peer.AuthToken, m.peerID, peer.SessionID, time.Now())
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(probe); err != nil {
		return nil, err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	buf := make([]byte, 2048)
	for {
		deadline := time.Now().Add(100 * time.Millisecond)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		if err := conn.SetReadDeadline(deadline); err != nil {
			return nil, err
		}
		n, readErr := conn.Read(buf)
		if readErr == nil && validateAuthenticatedPunch(string(buf[:n]), m.authToken, peer.PeerID, m.sessionID, time.Now(), defaultPunchMaxSkew, m.replay) {
			_ = conn.SetReadDeadline(time.Time{})
			return conn, nil
		}
		select {
		case <-ctx.Done():
			_ = conn.SetReadDeadline(time.Time{})
			return nil, ctx.Err()
		case <-ticker.C:
			_, _ = conn.Write(probe)
		default:
		}
	}
}

func unwrapUDPConn(conn net.Conn) (*net.UDPConn, bool) {
	if wrapped, ok := conn.(*consentConn); ok {
		conn = wrapped.Conn
	}
	udp, ok := conn.(*UDPConn)
	if !ok {
		return nil, false
	}
	return udp.conn, true
}

func (m *P2PManager) consentMaterial(peer *PeerInfo, negotiationID string) ([]byte, string) {
	if peer == nil || m.peerID == "" || m.authToken == "" || m.sessionID == "" || peer.PeerID == "" || peer.AuthToken == "" || peer.SessionID == "" {
		return nil, ""
	}
	endpoints := []string{
		strings.Join([]string{m.peerID, m.sessionID, m.authToken}, "\x00"),
		strings.Join([]string{peer.PeerID, peer.SessionID, peer.AuthToken}, "\x00"),
	}
	sort.Strings(endpoints)
	sum := sha256.Sum256([]byte(negotiationID + "\x02" + strings.Join(endpoints, "\x01")))
	session := base64.RawURLEncoding.EncodeToString(sum[:18])
	return sum[:], session
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
