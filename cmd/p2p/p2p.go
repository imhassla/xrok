package p2p

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/ccding/go-stun/stun"
)

// PeerInfo contains information about a peer
type PeerInfo struct {
	PeerID     string `json:"peer_id"`
	PublicAddr string `json:"public_addr"` // Public IP:port discovered via STUN
	LocalAddr  string `json:"local_addr"`  // Local IP:port
	NATType    string `json:"nat_type"`    // NAT type detected
}

// P2PManager handles P2P connections
type P2PManager struct {
	peerID      string
	stunServer  string
	publicAddr  string
	localAddr   string
	natType     stun.NATType
	connections map[string]*P2PConnection
	mu          sync.RWMutex
}

// P2PConnection represents a direct P2P connection
type P2PConnection struct {
	PeerID      string
	Conn        net.Conn
	LocalAddr   string
	RemoteAddr  string
	Established time.Time
}

// NewP2PManager creates a new P2P manager
func NewP2PManager(peerID, stunServer string) *P2PManager {
	if stunServer == "" {
		stunServer = "stun.l.google.com:19302"
	}
	return &P2PManager{
		peerID:      peerID,
		stunServer:  stunServer,
		connections: make(map[string]*P2PConnection),
	}
}

// DiscoverPublicAddr uses STUN to discover public address
func (m *P2PManager) DiscoverPublicAddr() (*PeerInfo, error) {
	// First try simple binding request (works with most STUN servers)
	publicAddr, localAddr, err := m.simpleSTUNBinding()
	if err != nil {
		// Fallback to full discovery
		client := stun.NewClient()
		client.SetServerAddr(m.stunServer)

		natType, host, discoverErr := client.Discover()
		if discoverErr != nil {
			return nil, fmt.Errorf("STUN discovery failed: %w (simple binding also failed: %v)", discoverErr, err)
		}

		m.natType = natType
		m.publicAddr = fmt.Sprintf("%s:%d", host.IP(), host.Port())

		// Get local address
		conn, err := net.Dial("udp", m.stunServer)
		if err == nil {
			m.localAddr = conn.LocalAddr().String()
			conn.Close()
		}

		log.Printf("P2P: Discovered public address %s (NAT type: %s)", m.publicAddr, natType)

		return &PeerInfo{
			PeerID:     m.peerID,
			PublicAddr: m.publicAddr,
			LocalAddr:  m.localAddr,
			NATType:    natType.String(),
		}, nil
	}

	m.publicAddr = publicAddr
	m.localAddr = localAddr
	m.natType = stun.NATUnknown

	log.Printf("P2P: Discovered public address %s (via simple binding)", m.publicAddr)

	return &PeerInfo{
		PeerID:     m.peerID,
		PublicAddr: m.publicAddr,
		LocalAddr:  m.localAddr,
		NATType:    "unknown",
	}, nil
}

// simpleSTUNBinding performs a simple STUN binding request
func (m *P2PManager) simpleSTUNBinding() (publicAddr, localAddr string, err error) {
	// Resolve STUN server
	serverAddr, err := net.ResolveUDPAddr("udp", m.stunServer)
	if err != nil {
		return "", "", fmt.Errorf("failed to resolve STUN server: %w", err)
	}

	// Create UDP connection
	conn, err := net.DialUDP("udp", nil, serverAddr)
	if err != nil {
		return "", "", fmt.Errorf("failed to connect to STUN server: %w", err)
	}
	defer conn.Close()

	localAddr = conn.LocalAddr().String()

	// Build STUN Binding Request (RFC 5389)
	// Header: 20 bytes
	// - 2 bytes: Message Type (0x0001 = Binding Request)
	// - 2 bytes: Message Length (0 for basic request)
	// - 4 bytes: Magic Cookie (0x2112A442)
	// - 12 bytes: Transaction ID (random)
	request := make([]byte, 20)
	request[0] = 0x00
	request[1] = 0x01 // Binding Request
	request[2] = 0x00
	request[3] = 0x00 // Length = 0
	// Magic Cookie
	request[4] = 0x21
	request[5] = 0x12
	request[6] = 0xa4
	request[7] = 0x42
	// Transaction ID (12 random bytes)
	for i := 8; i < 20; i++ {
		request[i] = byte(time.Now().UnixNano() >> (i * 4))
	}

	// Send request
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write(request); err != nil {
		return "", "", fmt.Errorf("failed to send STUN request: %w", err)
	}

	// Read response
	response := make([]byte, 1024)
	n, err := conn.Read(response)
	if err != nil {
		return "", "", fmt.Errorf("failed to read STUN response: %w", err)
	}

	if n < 20 {
		return "", "", fmt.Errorf("STUN response too short")
	}

	// Check message type (0x0101 = Binding Response)
	if response[0] != 0x01 || response[1] != 0x01 {
		return "", "", fmt.Errorf("unexpected STUN response type")
	}

	// Parse attributes to find XOR-MAPPED-ADDRESS (0x0020) or MAPPED-ADDRESS (0x0001)
	offset := 20
	msgLen := int(response[2])<<8 | int(response[3])

	for offset < 20+msgLen && offset+4 <= n {
		attrType := int(response[offset])<<8 | int(response[offset+1])
		attrLen := int(response[offset+2])<<8 | int(response[offset+3])
		offset += 4

		if offset+attrLen > n {
			break
		}

		if attrType == 0x0020 { // XOR-MAPPED-ADDRESS
			if attrLen >= 8 {
				family := response[offset+1]
				if family == 0x01 { // IPv4
					// XOR with magic cookie
					port := (int(response[offset+2])<<8 | int(response[offset+3])) ^ 0x2112
					ip := net.IPv4(
						response[offset+4]^0x21,
						response[offset+5]^0x12,
						response[offset+6]^0xa4,
						response[offset+7]^0x42,
					)
					return fmt.Sprintf("%s:%d", ip.String(), port), localAddr, nil
				}
			}
		} else if attrType == 0x0001 { // MAPPED-ADDRESS (fallback)
			if attrLen >= 8 {
				family := response[offset+1]
				if family == 0x01 { // IPv4
					port := int(response[offset+2])<<8 | int(response[offset+3])
					ip := net.IPv4(
						response[offset+4],
						response[offset+5],
						response[offset+6],
						response[offset+7],
					)
					return fmt.Sprintf("%s:%d", ip.String(), port), localAddr, nil
				}
			}
		}

		// Move to next attribute (4-byte aligned)
		offset += (attrLen + 3) &^ 3
	}

	return "", "", fmt.Errorf("no mapped address in STUN response")
}

// PunchHole attempts UDP hole punching to peer
func (m *P2PManager) PunchHole(ctx context.Context, peerInfo *PeerInfo) (*P2PConnection, error) {
	// Resolve peer address
	peerAddr, err := net.ResolveUDPAddr("udp", peerInfo.PublicAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve peer address: %w", err)
	}

	// Create local UDP socket
	localAddr, err := net.ResolveUDPAddr("udp", ":0")
	if err != nil {
		return nil, fmt.Errorf("failed to resolve local address: %w", err)
	}

	conn, err := net.ListenUDP("udp", localAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to create UDP socket: %w", err)
	}

	// Start hole punching
	punchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Send punch packets
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()

		punchMsg := []byte(fmt.Sprintf("XROK-P2P-PUNCH:%s", m.peerID))

		for {
			select {
			case <-punchCtx.Done():
				return
			case <-ticker.C:
				conn.WriteToUDP(punchMsg, peerAddr)
			}
		}
	}()

	// Wait for response
	buf := make([]byte, 1024)
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	for {
		select {
		case <-punchCtx.Done():
			conn.Close()
			return nil, fmt.Errorf("hole punching timed out")
		default:
			n, remoteAddr, err := conn.ReadFromUDP(buf)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					conn.Close()
					return nil, fmt.Errorf("hole punching timed out")
				}
				continue
			}

			msg := string(buf[:n])
			if len(msg) > 15 && msg[:15] == "XROK-P2P-PUNCH:" {
				// Received punch packet from peer - connection established!
				conn.SetReadDeadline(time.Time{})

				p2pConn := &P2PConnection{
					PeerID:      peerInfo.PeerID,
					Conn:        &UDPConn{conn: conn, remoteAddr: remoteAddr},
					LocalAddr:   conn.LocalAddr().String(),
					RemoteAddr:  remoteAddr.String(),
					Established: time.Now(),
				}

				m.mu.Lock()
				m.connections[peerInfo.PeerID] = p2pConn
				m.mu.Unlock()

				log.Printf("P2P: Hole punch successful to %s at %s", peerInfo.PeerID, remoteAddr)
				return p2pConn, nil
			}
		}
	}
}

// UDPConn wraps UDP connection to implement net.Conn
type UDPConn struct {
	conn       *net.UDPConn
	remoteAddr *net.UDPAddr
	mu         sync.Mutex
}

func (c *UDPConn) Read(b []byte) (n int, err error) {
	n, _, err = c.conn.ReadFromUDP(b)
	return
}

func (c *UDPConn) Write(b []byte) (n int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.WriteToUDP(b, c.remoteAddr)
}

func (c *UDPConn) Close() error {
	return c.conn.Close()
}

func (c *UDPConn) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

func (c *UDPConn) RemoteAddr() net.Addr {
	return c.remoteAddr
}

func (c *UDPConn) SetDeadline(t time.Time) error {
	return c.conn.SetDeadline(t)
}

func (c *UDPConn) SetReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}

func (c *UDPConn) SetWriteDeadline(t time.Time) error {
	return c.conn.SetWriteDeadline(t)
}

// GetConnection returns an existing P2P connection
func (m *P2PManager) GetConnection(peerID string) *P2PConnection {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.connections[peerID]
}

// CloseConnection closes a P2P connection
func (m *P2PManager) CloseConnection(peerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if conn, exists := m.connections[peerID]; exists {
		conn.Conn.Close()
		delete(m.connections, peerID)
	}
}

// GetPublicAddr returns the discovered public address
func (m *P2PManager) GetPublicAddr() string {
	return m.publicAddr
}

// GetNATType returns the detected NAT type
func (m *P2PManager) GetNATType() stun.NATType {
	return m.natType
}
