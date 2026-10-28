package config

import (
	"strings"
	"testing"
	"time"
)

func TestProxyConfigurationValidation(t *testing.T) {
	for _, raw := range []string{"http://proxy:8080", "https://proxy", "socks5://user:pass@proxy:1080", "socks5h://[::1]:1080"} {
		if e := ValidateProxySettings(raw, "", "", time.Second, false, false); e != nil {
			t.Fatal(raw, e)
		}
	}
	for _, tc := range []struct {
		proxy, session, header string
		quic, p2p              bool
	}{
		{"ftp://proxy:1", "", "", false, false},
		{"http://secret:password@[bad", "", "", false, false},
		{"http://proxy:0", "", "", false, false},
		{"http://proxy:8080/path", "", "", false, false},
		{"http://proxy:8080", "marker", "Authorization", false, false},
		{"http://proxy:8080", "marker\r\nInjected: yes", "X-Session", false, false},
		{"socks5h://proxy:1080", "marker", "X-Session", false, false},
		{"", "marker", "X-Session", false, false},
		{"http://proxy:8080", "", "", true, false},
		{"http://proxy:8080", "", "", false, true},
	} {
		e := ValidateProxySettings(tc.proxy, tc.session, tc.header, time.Second, tc.quic, tc.p2p)
		if e == nil || strings.Contains(e.Error(), "password") {
			t.Fatalf("invalid settings accepted or leaked: %v", e)
		}
	}
	c := DefaultClientConfig()
	c.HTTPProxy = "http://proxy:8080"
	c.Transport = "quic"
	if c.Validate() == nil {
		t.Fatal("config file QUIC/proxy combination accepted")
	}
}

func TestProxySessionCanBeCompletedByCLI(t *testing.T) {
	c := DefaultClientConfig()
	c.ProxySession = "private-profile-session"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := ValidateProxySettings("", c.ProxySession, c.ProxySessionHeader, c.ConnectTimeout, false, false); err == nil {
		t.Fatal("unresolved runtime proxy accepted")
	}
}
