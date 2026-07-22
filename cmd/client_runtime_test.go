package cmd

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"xrok/cmd/p2p"
)

func TestClientRuntimeOwnsIndependentState(t *testing.T) {
	a := NewClientRuntime(nil)
	b := NewClientRuntime(nil)
	a.Update(func(s *clientState) { s.ServerAddr = "a"; s.TunnelMap["x"] = "127.0.0.1:1" })
	if b.State().ServerAddr != "" || len(b.State().TunnelMap) != 0 {
		t.Fatal("runtime state leaked between instances")
	}
	if a.State().ServerAddr != "a" {
		t.Fatal("state update was not retained")
	}
}

type trackedClientConn struct{ closed atomic.Bool }

func (c *trackedClientConn) Read([]byte) (int, error)         { return 0, net.ErrClosed }
func (c *trackedClientConn) Write(p []byte) (int, error)      { return len(p), nil }
func (c *trackedClientConn) Close() error                     { c.closed.Store(true); return nil }
func (c *trackedClientConn) LocalAddr() net.Addr              { return nil }
func (c *trackedClientConn) RemoteAddr() net.Addr             { return nil }
func (c *trackedClientConn) SetDeadline(time.Time) error      { return nil }
func (c *trackedClientConn) SetReadDeadline(time.Time) error  { return nil }
func (c *trackedClientConn) SetWriteDeadline(time.Time) error { return nil }

func TestP2PSessionSwapAndStaleFallback(t *testing.T) {
	state := &clientState{
		P2PConnections:    make(map[string]net.Conn),
		P2PTunnels:        make(map[string]*p2p.P2PTunnel),
		P2PActiveSessions: make(map[string]string),
	}
	first, second := &trackedClientConn{}, &trackedClientConn{}
	installP2PSession(state, "peer", "session-old", first, nil)
	installP2PSession(state, "peer", "session-current", second, nil)
	if !first.closed.Load() || second.closed.Load() {
		t.Fatalf("session swap cleanup incorrect: old=%t current=%t", first.closed.Load(), second.closed.Load())
	}
	if removeP2PSessionIfMatch(state, "peer", "session-old") {
		t.Fatal("stale fallback removed current P2P session")
	}
	if second.closed.Load() || state.P2PConnections["peer"] != second {
		t.Fatal("stale fallback closed/replaced current connection")
	}
	if !removeP2PSessionIfMatch(state, "peer", "session-current") || !second.closed.Load() {
		t.Fatal("current fallback did not close active session")
	}
}

func TestP2PSessionMonitorRemovesOnlyMatchingSession(t *testing.T) {
	state := NewClientRuntime(nil).State()
	oldLocal, oldRemote := net.Pipe()
	defer oldRemote.Close()
	newLocal, newRemote := net.Pipe()
	defer newRemote.Close()
	installP2PSession(state, "peer", "old", oldLocal, nil)

	oldDone := make(chan struct{})
	monitorReturned := make(chan struct{})
	go func() {
		monitorP2PSession(state, state.P2PNegotiations, "peer", "old", oldDone)
		close(monitorReturned)
	}()
	installP2PSession(state, "peer", "new", newLocal, nil)
	close(oldDone)
	<-monitorReturned
	if got := getP2PConnection(state, "peer"); got != newLocal {
		t.Fatal("stale consent monitor removed the replacement session")
	}

	newDone := make(chan struct{})
	go monitorP2PSession(state, state.P2PNegotiations, "peer", "new", newDone)
	close(newDone)
	deadline := time.Now().Add(time.Second)
	for getP2PConnection(state, "peer") != nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := getP2PConnection(state, "peer"); got != nil {
		t.Fatal("matching consent monitor retained a dead session")
	}
}

func TestClientRuntimeCloseDrainsP2PState(t *testing.T) {
	conn := &trackedClientConn{}
	state := &clientState{
		P2PConnections:    map[string]net.Conn{"peer": conn},
		P2PTunnels:        make(map[string]*p2p.P2PTunnel),
		P2PPeers:          map[string]*p2p.PeerInfo{"peer": {PeerID: "peer"}},
		P2PActiveSessions: map[string]string{"peer": "session"},
		P2PNegotiations:   p2p.NewNegotiationStore(4, time.Minute),
		P2PManager:        p2p.NewP2PManagerWithToken("local", "", "token"),
	}
	runtime := &ClientRuntime{state: state}
	runtime.Close()
	if !conn.closed.Load() || len(state.P2PConnections) != 0 || len(state.P2PActiveSessions) != 0 || len(state.P2PPeers) != 0 {
		t.Fatalf("P2P runtime state leaked after Close: conn=%t connections=%d sessions=%d peers=%d", conn.closed.Load(), len(state.P2PConnections), len(state.P2PActiveSessions), len(state.P2PPeers))
	}
}

func TestClientRuntimeIdentityOverridesProcessGlobal(t *testing.T) {
	old := clientID
	clientID = "legacy-global"
	t.Cleanup(func() { clientID = old })
	if got := clientRuntimeID(&clientState{ClientID: "runtime-client"}); got != "runtime-client" {
		t.Fatalf("clientRuntimeID=%q", got)
	}
}

func TestActivateP2PManagerAllowsHostOnlyICE(t *testing.T) {
	state := &clientState{}
	manager := p2p.NewP2PManagerWithToken("offline", "stun.invalid:9", "capability")
	info := manager.SignalingPeerInfo()
	if info.PublicAddr != "" {
		t.Fatalf("test requires host-only peer info, got %q", info.PublicAddr)
	}
	activateP2PManager(state, manager, info)
	if !state.P2PEnabled || state.P2PManager != manager || state.P2PInfo.PeerID != "offline" {
		t.Fatalf("host-only P2P manager was not activated: %#v", state.P2PInfo)
	}
}

func TestClientRuntimeContextAndClose(t *testing.T) {
	r := NewClientRuntime(nil)
	ctx := r.WithContext(context.Background())
	select {
	case <-ctx.Done():
		t.Fatal("context canceled too early")
	default:
	}
	r.Close()
	select {
	case <-ctx.Done():
	default:
		t.Fatal("Close did not cancel runtime context")
	}
	// Close is intentionally idempotent.
	r.Close()
}

func TestCurrentClientStatePrefersInjectedRuntime(t *testing.T) {
	oldRuntime, oldState := clientRuntime_, clientState_
	defer func() { clientRuntime_, clientState_ = oldRuntime, oldState }()
	r := NewClientRuntime(nil)
	r.Update(func(s *clientState) { s.ServerAddr = "runtime" })
	clientRuntime_ = r
	clientState_ = &clientState{ServerAddr: "legacy"}
	if got := currentClientState().ServerAddr; got != "runtime" {
		t.Fatalf("currentClientState() = %q, want runtime state", got)
	}
}
