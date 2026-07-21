package cmd

import (
	"context"
	"testing"
)

func TestServerDepsRegistriesAreIsolated(t *testing.T) {
	a := &ServerDeps{Clients: NewClientRegistry(), Routes: NewRouteRegistry()}
	b := &ServerDeps{Clients: NewClientRegistry(), Routes: NewRouteRegistry()}
	a.Clients.Put("one", &serverClientInfo{})
	if b.Clients.Len() != 0 {
		t.Fatal("client registries must not share state")
	}
	r := &TunnelRoute{ClientID: "one"}
	a.Routes.Put("route", r)
	if got, ok := b.Routes.Get("route"); ok || got != nil {
		t.Fatal("route registries must not share state")
	}
}

func TestServerDepsContext(t *testing.T) {
	deps := &ServerDeps{Clients: NewClientRegistry(), Routes: NewRouteRegistry()}
	ctx := WithServerDeps(context.Background(), deps)
	got, ok := ServerDepsFromContext(ctx)
	if !ok || got != deps {
		t.Fatal("server dependencies were not propagated")
	}
	if _, ok := ServerDepsFromContext(context.Background()); ok {
		t.Fatal("unexpected dependencies in empty context")
	}
}

func TestServerDepsGlobalBridgeIsStable(t *testing.T) {

	a := serverDepsFromGlobals()
	b := serverDepsFromGlobals()
	if a.Clients != b.Clients || a.Routes != b.Routes {
		t.Fatal("legacy bridge must reuse registry instances")
	}
	if a.Clients == nil || a.Routes == nil {
		t.Fatal("legacy bridge returned nil registries")
	}
}
