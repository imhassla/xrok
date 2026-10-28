package cmd

import (
	"testing"

	"xrok/cmd/config"
)

func TestAdvertisedPortIsSeparateFromListenPort(t *testing.T) {
	oldListen, oldPublic := serverPort, serverPublicPort
	defer func() { serverPort, serverPublicPort = oldListen, oldPublic }()
	oldLegacy := serverLegacyListeners
	defer func() { serverLegacyListeners = oldLegacy }()
	serverLegacyListeners = false
	serverPort = 8443
	for _, tc := range []struct {
		port int
		want string
	}{{0, "https://app.xrok.example.com:8443"}, {443, "https://app.xrok.example.com"}, {9443, "https://app.xrok.example.com:9443"}} {
		serverPublicPort = tc.port
		if got := serverPublicURL("app", "xrok.example.com"); got != tc.want {
			t.Fatal(got, tc.want)
		}
		if serverClientRoutingPort(0) != serverAdvertisedPort() {
			t.Fatal("registration exposed backend port")
		}
		if serverPort != 8443 {
			t.Fatal("advertisement changed listener")
		}
	}
	c := config.DefaultServerConfig()
	c.Domain = "xrok.example.com"
	c.PublicPort = 65536
	if c.Validate() == nil {
		t.Fatal("invalid public port accepted")
	}
}
