package config

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ValidateProxySettings deliberately omits raw URLs and values from errors because
// proxy URLs can contain credentials and session markers are relay-private.
func ValidateProxySettings(raw, session, header string, budget time.Duration, quic, p2p bool) error {
	if budget < 0 {
		return fmt.Errorf("connect_timeout must not be negative")
	}
	if header == "" {
		header = "X-Proxy-Session"
	}
	if !validProxyHeaderName(header) {
		return fmt.Errorf("invalid proxy session header name")
	}
	switch strings.ToLower(header) {
	case "host", "authorization", "proxy-authorization", "proxy-authenticate", "connection", "proxy-connection", "upgrade", "cookie", "set-cookie", "content-length", "transfer-encoding", "x-api-token", "x-xrok-session":
		return fmt.Errorf("reserved proxy session header name")
	}
	if len(session) > 256 || !validProxySession(session) {
		return fmt.Errorf("invalid proxy session marker")
	}
	if raw == "" {
		if session != "" {
			return fmt.Errorf("proxy_session requires an upstream proxy")
		}
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return fmt.Errorf("invalid upstream proxy URL")
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return fmt.Errorf("proxy scheme must be http, https, socks5, or socks5h")
	}
	if port := u.Port(); port != "" {
		n, e := strconv.Atoi(port)
		if e != nil || n < 1 || n > 65535 {
			return fmt.Errorf("invalid proxy port")
		}
	}
	if strings.ContainsAny(u.Hostname(), " \t\r\n") {
		return fmt.Errorf("invalid proxy host")
	}
	if strings.HasPrefix(u.Scheme, "socks5") && session != "" {
		return fmt.Errorf("proxy_session requires an HTTP CONNECT relay")
	}
	if quic {
		return fmt.Errorf("QUIC cannot use this TCP proxy; select the TLS transport or remove the proxy explicitly")
	}
	if p2p {
		return fmt.Errorf("P2P discovery and data use direct paths; disable P2P when requiring an upstream proxy")
	}
	return nil
}

func validProxyHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for _, b := range []byte(s) {
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || strings.IndexByte("!#$%&'*+-.^_`|~", b) >= 0 {
			continue
		}
		return false
	}
	return true
}
func validProxySession(s string) bool {
	if s != strings.TrimSpace(s) {
		return false
	}
	for _, b := range []byte(s) {
		if b < 0x20 || b > 0x7e {
			return false
		}
	}
	return true
}
