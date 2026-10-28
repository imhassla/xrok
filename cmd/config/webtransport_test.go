package config

import "testing"

func TestWebTransportRouteConfigPolicy(t *testing.T) {
	for _, origin := range []string{"https://ui.example", "http://localhost:3000", "https://ui.example:8443/"} {
		c := &ClientConfig{Tunnels: []TunnelConfig{{WebTransport: true, Datagrams: true, WebTransportOrigins: []string{origin}}}}
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, tunnel := range []TunnelConfig{{Datagrams: true}, {WebTransport: true, WebTransportOrigins: []string{"null"}}, {WebTransport: true, WebTransportOrigins: []string{"https://user:secret@ui.example"}}, {WebTransport: true, WebTransportOrigins: []string{"https://ui.example/path"}}} {
		c := &ClientConfig{Tunnels: []TunnelConfig{tunnel}}
		if err := c.Validate(); err == nil {
			t.Fatal("invalid WebTransport route accepted")
		}
	}
}
