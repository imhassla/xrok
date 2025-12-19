package p2p

import (
	"encoding/json"
	"fmt"
)

// SignalType represents P2P signaling message types
type SignalType string

const (
	SignalTypeDiscover    SignalType = "p2p_discover"     // Request peer info
	SignalTypePeerInfo    SignalType = "p2p_peer_info"    // Response with peer info
	SignalTypePunch       SignalType = "p2p_punch"        // Start hole punching
	SignalTypeReady       SignalType = "p2p_ready"        // P2P connection ready
	SignalTypeFallback    SignalType = "p2p_fallback"     // Fallback to relay
	SignalTypeTunnelReq   SignalType = "p2p_tunnel_req"   // P2P tunnel request
	SignalTypeTunnelResp  SignalType = "p2p_tunnel_resp"  // P2P tunnel response
	SignalTypeTunnelData  SignalType = "p2p_tunnel_data"  // P2P tunnel data chunk
	SignalTypeTunnelClose SignalType = "p2p_tunnel_close" // P2P tunnel close
)

// SignalMessage represents a P2P signaling message
type SignalMessage struct {
	Type      SignalType `json:"type"`
	FromPeer  string     `json:"from_peer"`
	ToPeer    string     `json:"to_peer,omitempty"`
	PeerInfo  *PeerInfo  `json:"peer_info,omitempty"`
	SessionID string     `json:"session_id,omitempty"`
	Error     string     `json:"error,omitempty"`
}

// EncodeSignal encodes a signal message to JSON
func EncodeSignal(msg *SignalMessage) ([]byte, error) {
	return json.Marshal(msg)
}

// DecodeSignal decodes a JSON signal message
func DecodeSignal(data []byte) (*SignalMessage, error) {
	var msg SignalMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, fmt.Errorf("failed to decode signal: %w", err)
	}
	return &msg, nil
}

// NewDiscoverSignal creates a peer discovery request
func NewDiscoverSignal(fromPeer, toPeer string) *SignalMessage {
	return &SignalMessage{
		Type:     SignalTypeDiscover,
		FromPeer: fromPeer,
		ToPeer:   toPeer,
	}
}

// NewPeerInfoSignal creates a peer info response
func NewPeerInfoSignal(fromPeer string, info *PeerInfo) *SignalMessage {
	return &SignalMessage{
		Type:     SignalTypePeerInfo,
		FromPeer: fromPeer,
		PeerInfo: info,
	}
}

// NewPunchSignal creates a hole punch coordination message
func NewPunchSignal(fromPeer, toPeer, sessionID string) *SignalMessage {
	return &SignalMessage{
		Type:      SignalTypePunch,
		FromPeer:  fromPeer,
		ToPeer:    toPeer,
		SessionID: sessionID,
	}
}

// NewReadySignal indicates P2P connection is established
func NewReadySignal(fromPeer, toPeer, sessionID string) *SignalMessage {
	return &SignalMessage{
		Type:      SignalTypeReady,
		FromPeer:  fromPeer,
		ToPeer:    toPeer,
		SessionID: sessionID,
	}
}

// NewFallbackSignal indicates P2P failed, use relay
func NewFallbackSignal(fromPeer, toPeer, sessionID, reason string) *SignalMessage {
	return &SignalMessage{
		Type:      SignalTypeFallback,
		FromPeer:  fromPeer,
		ToPeer:    toPeer,
		SessionID: sessionID,
		Error:     reason,
	}
}
