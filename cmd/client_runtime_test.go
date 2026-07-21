package cmd

import (
	"context"
	"testing"
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
