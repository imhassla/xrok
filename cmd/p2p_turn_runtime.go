package cmd

import (
	"crypto/tls"
	"fmt"

	"xrok/cmd/config"
	"xrok/cmd/p2p"
)

// newClientTURNSplitAllocator constructs the peer-independent side of a
// bilateral TURN negotiation. AllocateCandidate runs before signaling; the
// remote relay IP:port is learned only from the authenticated answer/offer.
func newClientTURNSplitAllocator(turn config.TURNConfig) (p2p.RelayCandidateAllocator, error) {
	if !turn.Enabled {
		return nil, nil
	}
	cfg, err := clientTURNProviderConfig(turn)
	if err != nil {
		return nil, err
	}
	provider, err := p2p.NewTURNRelayProvider(cfg)
	if err != nil {
		return nil, err
	}
	return p2p.TURNRelayCandidateAllocator(provider), nil
}

func clientTURNProviderConfig(turn config.TURNConfig) (p2p.TURNConfig, error) {
	var tlsConfig *tls.Config
	if turn.Transport == "tls" && turn.CACert != "" {
		roots, err := loadRootCAs(turn.CACert)
		if err != nil {
			return p2p.TURNConfig{}, fmt.Errorf("load TURN CA certificate: %w", err)
		}
		tlsConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}
	return p2p.TURNConfig{
		Server: turn.Server, Username: turn.Username, Password: turn.Password,
		Transport: p2p.TURNTransport(turn.Transport), TLSConfig: tlsConfig,
		TLSServerName: turn.ServerName, TLSInsecureSkipVerify: turn.Insecure,
		EnableChannelBind: turn.ChannelBind, Timeout: turn.Timeout,
		RefreshInterval: turn.RefreshInterval, MaxDatagramSize: turn.MaxDatagramSize,
		Retransmits: turn.Retransmits,
	}, nil
}
