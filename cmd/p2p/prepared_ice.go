package p2p

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"time"
)

const (
	maxPreparedICE = 128
	preparedICETTL = 30 * time.Second
)

type preparedICE struct {
	conn        *net.UDPConn
	candidates  []Candidate
	relay       PendingRelayAllocation
	expires     time.Time
	timer       *time.Timer
	stopContext func() bool
	claimed     bool
}

// PendingRelayAllocation is the negotiation-owned half of a split TURN
// allocation. Connect binds it to the relay candidate authenticated by the
// remote offer/answer and transfers ownership to the returned message conn.
type PendingRelayAllocation interface {
	Connect(context.Context, *net.UDPAddr) (net.Conn, error)
	Close() error
}

type RelayCandidateAllocator func(context.Context) (PendingRelayAllocation, Candidate, error)

func TURNRelayCandidateAllocator(provider *TURNRelayProvider) RelayCandidateAllocator {
	if provider == nil {
		return nil
	}
	return func(ctx context.Context) (PendingRelayAllocation, Candidate, error) {
		return provider.AllocateCandidate(ctx)
	}
}

// SignalingPeerInfo returns the per-manager capability that may only be sent
// over the authenticated signaling channel.
func (m *P2PManager) SignalingPeerInfo() *PeerInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return &PeerInfo{PeerID: m.localID(), PublicAddr: m.publicAddr, LocalAddr: m.localAddr, NATType: m.natType.String(), AuthToken: m.authToken, SessionID: m.sessionID}
}

// PrepareICE binds the negotiation socket before advertising candidates. The
// exact same socket is later used for connectivity checks and transferred to
// the nominated ICESession, avoiding transient STUN/host port mismatches.
func (m *P2PManager) PrepareICE(ctx context.Context, negotiationID, stunServer string) ([]Candidate, error) {
	if ctx == nil || negotiationID == "" {
		return nil, fmt.Errorf("ICE context and negotiation session are required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.preparedICE == nil {
		m.preparedICE = make(map[string]*preparedICE)
	}
	if m.preparingICE == nil {
		m.preparingICE = make(map[string]struct{})
	}
	m.expirePreparedLocked(time.Now())
	if existing := m.preparedICE[negotiationID]; existing != nil {
		out := cloneCandidates(existing.candidates)
		m.mu.Unlock()
		return out, nil
	}
	if _, preparing := m.preparingICE[negotiationID]; preparing {
		m.mu.Unlock()
		return nil, fmt.Errorf("ICE session is already being prepared")
	}
	if len(m.preparedICE)+len(m.preparingICE) >= maxPreparedICE {
		m.mu.Unlock()
		return nil, fmt.Errorf("prepared ICE socket capacity reached")
	}
	m.preparingICE[negotiationID] = struct{}{}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.preparingICE, negotiationID)
		m.mu.Unlock()
	}()

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("bind ICE negotiation socket: %w", err)
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	candidates := hostCandidatesForPort(port)
	if stunServer != "" {
		if mapped, err := stunBindingOnSocket(ctx, conn, stunServer); err == nil {
			candidates = append(candidates, Candidate{ID: "srflx-" + mapped, Type: CandidateSrflx, Addr: mapped, Priority: 100})
		}
	}
	if len(candidates) == 0 {
		_ = conn.Close()
		return nil, fmt.Errorf("no usable ICE host candidates")
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	m.mu.Lock()
	m.expirePreparedLocked(time.Now())
	if existing := m.preparedICE[negotiationID]; existing != nil {
		m.mu.Unlock()
		_ = conn.Close()
		return cloneCandidates(existing.candidates), nil
	}
	if len(m.preparedICE) >= maxPreparedICE {
		m.mu.Unlock()
		_ = conn.Close()
		return nil, fmt.Errorf("prepared ICE socket capacity reached")
	}
	prepared := &preparedICE{conn: conn, candidates: cloneCandidates(candidates), expires: time.Now().Add(preparedICETTL)}
	prepared.timer = time.AfterFunc(preparedICETTL, func() { m.expirePreparedSession(negotiationID, conn) })
	prepared.stopContext = context.AfterFunc(ctx, func() { m.CancelPreparedICE(negotiationID) })
	m.preparedICE[negotiationID] = prepared
	m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		m.CancelPreparedICE(negotiationID)
		return nil, err
	}
	return candidates, nil
}

// PrepareICEWithRelay allocates the local TURN candidate before signaling and
// retains the unbound allocation with the same bounded negotiation lifecycle.
func (m *P2PManager) PrepareICEWithRelay(ctx context.Context, negotiationID, stunServer string, allocate RelayCandidateAllocator) ([]Candidate, error) {
	candidates, err := m.PrepareICE(ctx, negotiationID, stunServer)
	if err != nil || allocate == nil {
		return candidates, err
	}
	m.mu.Lock()
	prepared := m.preparedICE[negotiationID]
	if prepared != nil && prepared.relay != nil {
		out := cloneCandidates(prepared.candidates)
		m.mu.Unlock()
		return out, nil
	}
	m.mu.Unlock()
	pending, candidate, err := allocate(ctx)
	if err != nil {
		m.CancelPreparedICE(negotiationID)
		return nil, fmt.Errorf("allocate negotiation TURN candidate: %w", err)
	}
	if pending == nil || candidate.Type != CandidateRelay {
		if pending != nil {
			_ = pending.Close()
		}
		m.CancelPreparedICE(negotiationID)
		return nil, fmt.Errorf("TURN allocator returned an invalid relay candidate")
	}
	if err := ValidateCandidate(candidate); err != nil {
		_ = pending.Close()
		m.CancelPreparedICE(negotiationID)
		return nil, err
	}
	m.mu.Lock()
	prepared = m.preparedICE[negotiationID]
	if prepared == nil || prepared.relay != nil {
		m.mu.Unlock()
		_ = pending.Close()
		if prepared == nil {
			return nil, fmt.Errorf("prepared ICE session expired during TURN allocation")
		}
		return cloneCandidates(prepared.candidates), nil
	}
	prepared.relay = pending
	prepared.candidates = append(prepared.candidates, candidate)
	out := cloneCandidates(prepared.candidates)
	m.mu.Unlock()
	return out, nil
}

func hostCandidatesForPort(port int) []Candidate {
	seen := make(map[string]struct{})
	var out []Candidate
	ifaces, _ := net.Interfaces()
	for _, iface := range ifaces {
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			ip, _, err := net.ParseCIDR(addr.String())
			if err != nil || ip.To4() == nil {
				continue
			}
			endpoint := net.JoinHostPort(ip.String(), fmt.Sprint(port))
			if _, ok := seen[endpoint]; ok {
				continue
			}
			seen[endpoint] = struct{}{}
			out = append(out, Candidate{ID: "host-" + endpoint, Type: CandidateHost, Addr: endpoint, Priority: 126})
		}
	}
	loopback := net.JoinHostPort("127.0.0.1", fmt.Sprint(port))
	if _, ok := seen[loopback]; !ok {
		out = append(out, Candidate{ID: "host-" + loopback, Type: CandidateHost, Addr: loopback, Priority: 126})
	}
	return out
}

func stunBindingOnSocket(ctx context.Context, conn *net.UDPConn, server string) (string, error) {
	serverAddr, err := net.ResolveUDPAddr("udp", server)
	if err != nil {
		return "", err
	}
	request := make([]byte, 20)
	request[1] = 0x01
	copy(request[4:8], []byte{0x21, 0x12, 0xa4, 0x42})
	if _, err := rand.Read(request[8:]); err != nil {
		return "", err
	}
	deadline := time.Now().Add(3 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return "", err
	}
	if _, err := conn.WriteToUDP(request, serverAddr); err != nil {
		return "", err
	}
	response := make([]byte, 1024)
	n, source, err := conn.ReadFromUDP(response)
	if err != nil {
		return "", err
	}
	if !source.IP.Equal(serverAddr.IP) || source.Port != serverAddr.Port || n < 20 || response[0] != 0x01 || response[1] != 0x01 || !hmac.Equal(response[4:20], request[4:20]) {
		return "", fmt.Errorf("invalid STUN response")
	}
	msgLen := int(binary.BigEndian.Uint16(response[2:4]))
	if msgLen > n-20 || msgLen%4 != 0 {
		return "", fmt.Errorf("invalid STUN response length")
	}
	for offset := 20; offset+4 <= 20+msgLen; {
		typ := binary.BigEndian.Uint16(response[offset : offset+2])
		length := int(binary.BigEndian.Uint16(response[offset+2 : offset+4]))
		offset += 4
		if offset+length > n {
			return "", fmt.Errorf("truncated STUN attribute")
		}
		if length >= 8 && response[offset+1] == 0x01 && (typ == 0x0020 || typ == 0x0001) {
			port := binary.BigEndian.Uint16(response[offset+2 : offset+4])
			ip := append(net.IP(nil), response[offset+4:offset+8]...)
			if typ == 0x0020 {
				port ^= 0x2112
				for i, b := range []byte{0x21, 0x12, 0xa4, 0x42} {
					ip[i] ^= b
				}
			}
			return net.JoinHostPort(ip.String(), fmt.Sprint(port)), nil
		}
		offset += (length + 3) &^ 3
	}
	return "", fmt.Errorf("STUN mapped address missing")
}

func (m *P2PManager) preparedSocket(sessionID string) (*net.UDPConn, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expirePreparedLocked(time.Now())
	prepared := m.preparedICE[sessionID]
	if prepared == nil {
		return nil, false
	}
	prepared.expires = time.Now().Add(preparedICETTL)
	if prepared.timer != nil {
		prepared.timer.Reset(preparedICETTL)
	}
	return prepared.conn, true
}

func (m *P2PManager) claimPrepared(sessionID string) (*net.UDPConn, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expirePreparedLocked(time.Now())
	prepared := m.preparedICE[sessionID]
	if prepared == nil || prepared.claimed {
		return nil, false
	}
	prepared.claimed = true
	if prepared.timer != nil {
		prepared.timer.Stop()
	}
	return prepared.conn, true
}

func (m *P2PManager) preparedRelay(sessionID string) (PendingRelayAllocation, Candidate, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expirePreparedLocked(time.Now())
	prepared := m.preparedICE[sessionID]
	if prepared == nil || prepared.relay == nil {
		return nil, Candidate{}, false
	}
	for _, candidate := range prepared.candidates {
		if candidate.Type == CandidateRelay {
			if !prepared.claimed {
				prepared.expires = time.Now().Add(preparedICETTL)
			}
			if prepared.timer != nil && !prepared.claimed {
				prepared.timer.Reset(preparedICETTL)
			}
			return prepared.relay, candidate, true
		}
	}
	return nil, Candidate{}, false
}

func (m *P2PManager) transferPreparedSocket(sessionID string, conn *net.UDPConn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if prepared := m.preparedICE[sessionID]; prepared != nil && prepared.conn == conn {
		if prepared.timer != nil {
			prepared.timer.Stop()
		}
		if prepared.stopContext != nil {
			prepared.stopContext()
		}
		if prepared.relay != nil {
			_ = prepared.relay.Close()
		}
		delete(m.preparedICE, sessionID)
	}
}

func (m *P2PManager) transferPreparedRelay(sessionID string, allocation PendingRelayAllocation) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prepared := m.preparedICE[sessionID]
	if prepared == nil || prepared.relay != allocation {
		return
	}
	if prepared.timer != nil {
		prepared.timer.Stop()
	}
	if prepared.stopContext != nil {
		prepared.stopContext()
	}
	_ = prepared.conn.Close()
	delete(m.preparedICE, sessionID)
}

func (m *P2PManager) CancelPreparedICE(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if prepared := m.preparedICE[sessionID]; prepared != nil {
		if prepared.timer != nil {
			prepared.timer.Stop()
		}
		if prepared.stopContext != nil {
			prepared.stopContext()
		}
		if prepared.relay != nil {
			_ = prepared.relay.Close()
		}
		_ = prepared.conn.Close()
		delete(m.preparedICE, sessionID)
	}
}

// Close releases active connections and every unconnected TURN allocation.
// The manager identity remains immutable, but no transport ownership survives.
func (m *P2PManager) Close() error {
	m.mu.Lock()
	prepared := make([]*preparedICE, 0, len(m.preparedICE))
	for _, item := range m.preparedICE {
		if item.timer != nil {
			item.timer.Stop()
		}
		if item.stopContext != nil {
			item.stopContext()
		}
		prepared = append(prepared, item)
	}
	m.preparedICE = make(map[string]*preparedICE)
	m.preparingICE = make(map[string]struct{})
	connections := make([]net.Conn, 0, len(m.connections))
	for _, connection := range m.connections {
		if connection != nil && connection.Conn != nil {
			connections = append(connections, connection.Conn)
		}
	}
	m.connections = make(map[string]*P2PConnection)
	m.mu.Unlock()
	for _, item := range prepared {
		_ = item.conn.Close()
		if item.relay != nil {
			_ = item.relay.Close()
		}
	}
	for _, conn := range connections {
		_ = conn.Close()
	}
	return nil
}

func (m *P2PManager) expirePreparedLocked(now time.Time) {
	for session, prepared := range m.preparedICE {
		if prepared.claimed {
			continue
		}
		if !prepared.expires.After(now) {
			if prepared.timer != nil {
				prepared.timer.Stop()
			}
			if prepared.stopContext != nil {
				prepared.stopContext()
			}
			if prepared.relay != nil {
				_ = prepared.relay.Close()
			}
			_ = prepared.conn.Close()
			delete(m.preparedICE, session)
		}
	}
}

func (m *P2PManager) expirePreparedSession(sessionID string, conn *net.UDPConn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prepared := m.preparedICE[sessionID]
	if prepared == nil || prepared.conn != conn || prepared.claimed || prepared.expires.After(time.Now()) {
		return
	}
	_ = prepared.conn.Close()
	if prepared.relay != nil {
		_ = prepared.relay.Close()
	}
	if prepared.stopContext != nil {
		prepared.stopContext()
	}
	delete(m.preparedICE, sessionID)
}

// punchConfirmations is how many probes are sent after the peer's probe
// arrives. Its probe proves the peer's NAT now admits us, but our earlier
// probes may have been dropped before that mapping existed; without these the
// peer can keep waiting and fail even though our side succeeded. The peer's
// consent layer discards the surplus as late punches.
const punchConfirmations = 3

func (m *P2PManager) punchPrepared(ctx context.Context, conn *net.UDPConn, peer *PeerInfo, candidate Candidate) (net.Conn, error) {
	remote, err := net.ResolveUDPAddr("udp", candidate.Addr)
	if err != nil {
		return nil, fmt.Errorf("resolve ICE candidate: %w", err)
	}
	probe, err := authenticatedPunchMessage(peer.AuthToken, m.localID(), peer.SessionID, time.Now())
	if err != nil {
		return nil, err
	}
	if _, err := conn.WriteToUDP(probe, remote); err != nil {
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
		n, source, readErr := conn.ReadFromUDP(buf)
		if readErr == nil {
			if source.IP.Equal(remote.IP) && source.Port == remote.Port && validateAuthenticatedPunch(string(buf[:n]), m.authToken, peer.PeerID, m.sessionID, time.Now(), defaultPunchMaxSkew, m.replay) {
				_ = conn.SetReadDeadline(time.Time{})
				for i := 0; i < punchConfirmations; i++ {
					_, _ = conn.WriteToUDP(probe, remote)
				}
				return &UDPConn{conn: conn, remoteAddr: source}, nil
			}
		}
		select {
		case <-ctx.Done():
			_ = conn.SetReadDeadline(time.Time{})
			return nil, ctx.Err()
		case <-ticker.C:
			_, _ = conn.WriteToUDP(probe, remote)
		default:
		}
	}
}
