package p2p

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/netip"
)

const (
	MaxSignalMessageSize = 64 << 10
	MaxICECandidates     = 32
)

// SignalType represents P2P signaling message types
type SignalType string

const (
	SignalTypeDiscover     SignalType = "p2p_discover"  // Request peer info
	SignalTypePeerInfo     SignalType = "p2p_peer_info" // Response with peer info
	SignalTypePunch        SignalType = "p2p_punch"     // Start hole punching
	SignalTypeReady        SignalType = "p2p_ready"     // P2P connection ready
	SignalTypeFallback     SignalType = "p2p_fallback"  // Fallback to relay
	SignalTypeICEOffer     SignalType = "ice_offer"
	SignalTypeICEAnswer    SignalType = "ice_answer"
	SignalTypeICECandidate SignalType = "ice_candidate"
	SignalTypeTunnelReq    SignalType = "p2p_tunnel_req"   // P2P tunnel request
	SignalTypeTunnelResp   SignalType = "p2p_tunnel_resp"  // P2P tunnel response
	SignalTypeTunnelData   SignalType = "p2p_tunnel_data"  // P2P tunnel data chunk
	SignalTypeTunnelClose  SignalType = "p2p_tunnel_close" // P2P tunnel close
)

// SignalMessage represents a P2P signaling message
type SignalMessage struct {
	Type       SignalType  `json:"type"`
	FromPeer   string      `json:"from_peer"`
	ToPeer     string      `json:"to_peer,omitempty"`
	PeerInfo   *PeerInfo   `json:"peer_info,omitempty"`
	SessionID  string      `json:"session_id,omitempty"`
	Candidates []Candidate `json:"candidates,omitempty"`
	Error      string      `json:"error,omitempty"`
}

// EncodeSignal encodes a signal message to JSON
func EncodeSignal(msg *SignalMessage) ([]byte, error) {
	return json.Marshal(msg)
}

// DecodeSignal decodes a JSON signal message
func DecodeSignal(data []byte) (*SignalMessage, error) {
	if len(data) == 0 || len(data) > MaxSignalMessageSize {
		return nil, fmt.Errorf("invalid signal size: %d", len(data))
	}
	var msg SignalMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, fmt.Errorf("failed to decode signal: %w", err)
	}
	if !validSignalType(msg.Type) {
		return nil, fmt.Errorf("unknown signal type %q", msg.Type)
	}
	if len(msg.Candidates) > MaxICECandidates {
		return nil, fmt.Errorf("too many ICE candidates: %d", len(msg.Candidates))
	}
	if isICESignal(msg.Type) {
		if msg.FromPeer == "" || msg.ToPeer == "" || msg.SessionID == "" {
			return nil, fmt.Errorf("ICE signal requires peers and session")
		}
		if len(msg.FromPeer) > 128 || len(msg.ToPeer) > 128 || len(msg.SessionID) > 128 {
			return nil, fmt.Errorf("ICE signaling field too long")
		}
		for _, candidate := range msg.Candidates {
			if err := ValidateCandidate(candidate); err != nil {
				return nil, err
			}
		}
		switch msg.Type {
		case SignalTypeICEOffer, SignalTypeICEAnswer:
			if len(msg.Candidates) == 0 {
				return nil, fmt.Errorf("ICE offer/answer requires at least one candidate")
			}
		case SignalTypeICECandidate:
			if len(msg.Candidates) != 1 {
				return nil, fmt.Errorf("ICE candidate signal requires exactly one candidate")
			}
		}
	}
	return &msg, nil
}

func validSignalType(t SignalType) bool {
	switch t {
	case SignalTypeDiscover, SignalTypePeerInfo, SignalTypePunch, SignalTypeReady, SignalTypeFallback,
		SignalTypeICEOffer, SignalTypeICEAnswer, SignalTypeICECandidate,
		SignalTypeTunnelReq, SignalTypeTunnelResp, SignalTypeTunnelData, SignalTypeTunnelClose:
		return true
	default:
		return false
	}
}

func isICESignal(t SignalType) bool {
	return t == SignalTypeICEOffer || t == SignalTypeICEAnswer || t == SignalTypeICECandidate
}

func validCandidateType(t CandidateType) bool {
	return t == CandidateHost || t == CandidateSrflx || t == CandidateRelay
}

func ValidateCandidate(candidate Candidate) error {
	if candidate.ID == "" || len(candidate.ID) > 128 || candidate.Addr == "" || len(candidate.Addr) > 256 || !validCandidateType(candidate.Type) {
		return fmt.Errorf("invalid ICE candidate")
	}
	if _, err := netip.ParseAddrPort(candidate.Addr); err == nil {
		return nil
	}
	return fmt.Errorf("ICE candidate must be a literal IP:port")
}

// ShouldInitiateICE deterministically resolves offer glare. Exactly one side
// becomes controlling for a pair of stable authenticated peer IDs.
func ShouldInitiateICE(localPeer, remotePeer string) bool {
	return localPeer != "" && remotePeer != "" && localPeer < remotePeer
}

func NewICESessionID() string {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func NewICEOfferSignal(fromPeer, toPeer, sessionID string, candidates []Candidate) *SignalMessage {
	return &SignalMessage{Type: SignalTypeICEOffer, FromPeer: fromPeer, ToPeer: toPeer, SessionID: sessionID, Candidates: candidates}
}

func NewICEAnswerSignal(fromPeer, toPeer, sessionID string, candidates []Candidate) *SignalMessage {
	return &SignalMessage{Type: SignalTypeICEAnswer, FromPeer: fromPeer, ToPeer: toPeer, SessionID: sessionID, Candidates: candidates}
}

func NewICECandidateSignal(fromPeer, toPeer, sessionID string, candidate Candidate) *SignalMessage {
	return &SignalMessage{Type: SignalTypeICECandidate, FromPeer: fromPeer, ToPeer: toPeer, SessionID: sessionID, Candidates: []Candidate{candidate}}
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
