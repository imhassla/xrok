package cmd

import (
	"context"
	"errors"
	"testing"

	"xrok/cmd/config"
	"xrok/cmd/p2p"
)

func TestPrepareClientICEFallsBackToDirectWhenTURNUnavailable(t *testing.T) {
	state := NewClientRuntime(nil).State()
	state.P2PManager = p2p.NewP2PManagerWithToken("client", "", "token")
	allocator := func(context.Context) (p2p.PendingRelayAllocation, p2p.Candidate, error) {
		return nil, p2p.Candidate{}, errors.New("coturn unavailable")
	}
	candidates, err := prepareClientICEWithAllocator(context.Background(), state, "session", allocator)
	if err != nil {
		t.Fatal(err)
	}
	defer state.P2PManager.CancelPreparedICE("session")
	if len(candidates) == 0 {
		t.Fatal("TURN outage suppressed direct candidates")
	}
	for _, candidate := range candidates {
		if candidate.Type == p2p.CandidateRelay {
			t.Fatal("failed TURN allocation advertised a relay candidate")
		}
	}
}

func TestClientTURNSplitConfigHasNoPeerResolver(t *testing.T) {
	cfg, err := clientTURNProviderConfig(config.TURNConfig{
		Enabled: true, Server: "turn.test:3478", Username: "u", Password: "p",
		Transport: "tcp", ChannelBind: true, Retransmits: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Transport != p2p.TURNTransportTCP || !cfg.EnableChannelBind || cfg.Retransmits != 4 {
		t.Fatalf("provider config=%#v", cfg)
	}
	if cfg.PeerResolver != nil {
		t.Fatal("bilateral TURN base provider unexpectedly has a PeerResolver")
	}
}

func TestClientTURNSplitAllocatorDisabled(t *testing.T) {
	allocator, err := newClientTURNSplitAllocator(config.TURNConfig{})
	if err != nil || allocator != nil {
		t.Fatalf("allocator=%v err=%v", allocator, err)
	}
}

func TestClientTURNSplitAllocatorValidatesBaseProvider(t *testing.T) {
	_, err := newClientTURNSplitAllocator(config.TURNConfig{Enabled: true, Server: "turn.test:3478"})
	if err == nil {
		t.Fatal("TURN allocator accepted missing long-term credentials")
	}
}
