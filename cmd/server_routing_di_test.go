package cmd

import "testing"

func TestResolveServerTunnelInjectedRoute(t *testing.T) {
	clients := NewClientRegistry()
	routes := NewRouteRegistry()
	c := &serverClientInfo{}
	clients.Put("client-a", c)
	routes.Put("app", &TunnelRoute{ClientID: "client-a", TunnelName: "web"})
	got, route, name := resolveServerTunnel(&ServerDeps{Clients: clients, Routes: routes}, "app")
	if got != c || route == nil || name != "web" {
		t.Fatalf("unexpected injected route resolution: client=%p route=%v name=%q", got, route, name)
	}
}

func TestResolveServerTunnelFallbackClient(t *testing.T) {
	clients := NewClientRegistry()
	clients.Put("direct", &serverClientInfo{})
	got, route, name := resolveServerTunnel(&ServerDeps{Clients: clients, Routes: NewRouteRegistry()}, "direct")
	if got == nil || route != nil || name != "direct" {
		t.Fatalf("unexpected direct resolution: client=%p route=%v name=%q", got, route, name)
	}
}
