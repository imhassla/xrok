package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"xrok/cmd/p2p"
)

type result struct {
	session *p2p.ICESession
	err     error
}

func main() {
	if err := runICE(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runICE() error {
	alice := p2p.NewP2PManagerWithToken("alice", "", "alice-process-capability")
	bob := p2p.NewP2PManagerWithToken("bob", "", "bob-process-capability")
	negotiationID := p2p.NewICESessionID()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	aliceCandidates, err := alice.PrepareICE(ctx, negotiationID, "")
	if err != nil {
		return err
	}
	bobCandidates, err := bob.PrepareICE(ctx, negotiationID, "")
	if err != nil {
		return err
	}
	aliceCandidates = loopbackCandidates(aliceCandidates)
	bobCandidates = loopbackCandidates(bobCandidates)
	if len(aliceCandidates) != 1 || len(bobCandidates) != 1 {
		return fmt.Errorf("expected exact loopback candidates: alice=%v bob=%v", aliceCandidates, bobCandidates)
	}

	aliceResult, bobResult := make(chan result, 1), make(chan result, 1)
	go connect(ctx, alice, bob.SignalingPeerInfo(), negotiationID, aliceCandidates, bobCandidates, aliceResult)
	go connect(ctx, bob, alice.SignalingPeerInfo(), negotiationID, bobCandidates, aliceCandidates, bobResult)
	a, b := <-aliceResult, <-bobResult
	if a.err != nil || b.err != nil {
		return fmt.Errorf("real ICE negotiation failed: alice=%v bob=%v", a.err, b.err)
	}
	defer a.session.Close()
	defer b.session.Close()

	// Keep the process test portable across macOS/Linux loopback UDP limits;
	// the 60KB framing boundary is covered by the in-memory consent unit test.
	payload := bytes.Repeat([]byte("ice-runtime-payload"), 400)
	readAlice := make(chan error, 1)
	go drainConsent(a.session.Conn(), readAlice)
	bobPayload := make(chan []byte, 1)
	stopBob := make(chan struct{})
	go func() {
		buf := make([]byte, 65000)
		for {
			n, readErr := b.session.Conn().Read(buf)
			if readErr != nil {
				return
			}
			if n > 0 {
				select {
				case bobPayload <- append([]byte(nil), buf[:n]...):
				default:
				}
			}
			select {
			case <-stopBob:
				return
			default:
			}
		}
	}()
	probe := make(chan error, 1)
	go func() {
		probeCtx, probeCancel := context.WithTimeout(ctx, time.Second)
		defer probeCancel()
		probe <- (p2p.RuntimeConsentProbe{}).Probe(probeCtx, a.session.Conn())
	}()
	if _, err := a.session.Conn().Write(payload); err != nil {
		return err
	}
	if err := <-probe; err != nil {
		return fmt.Errorf("runtime consent failed: %w", err)
	}
	if got := <-bobPayload; !bytes.Equal(got, payload) {
		return fmt.Errorf("concurrent payload corrupted: got=%d want=%d", len(got), len(payload))
	}
	close(stopBob)
	_ = a.session.Close()
	select {
	case <-readAlice:
	case <-time.After(time.Second):
		return fmt.Errorf("consent dispatcher did not stop")
	}
	return nil
}

func connect(ctx context.Context, manager *p2p.P2PManager, peer *p2p.PeerInfo, negotiationID string, local, remote []p2p.Candidate, out chan<- result) {
	agent := p2p.NewICEAgent(p2p.ICEConfig{CheckTimeout: 2 * time.Second})
	session, err := manager.EstablishPreparedICE(ctx, agent, peer, negotiationID, local, remote)
	out <- result{session: session, err: err}
}

func loopbackCandidates(in []p2p.Candidate) []p2p.Candidate {
	for _, candidate := range in {
		if strings.HasPrefix(candidate.Addr, "127.0.0.1:") {
			return []p2p.Candidate{candidate}
		}
	}
	return nil
}

func drainConsent(conn net.Conn, done chan<- error) {
	buf := make([]byte, 65000)
	for {
		n, err := conn.Read(buf)
		if err != nil || n > 0 {
			done <- err
			return
		}
	}
}
