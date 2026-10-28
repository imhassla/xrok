package config

import "fmt"

func TunnelTransport(value string, quicFlag bool) (string, error) {
	if quicFlag {
		if value != "" && value != "quic" {
			return "", fmt.Errorf("--quic conflicts with transport %q", value)
		}
		return "quic", nil
	}
	switch value {
	case "":
		return "auto", nil
	case "legacy", "tcp", "tls", "non-tls", "websocket":
		return "", fmt.Errorf("transport %q was removed; use auto, http2, wss, or quic and update both client and server", value)
	case "http2", "wss", "quic", "auto":
		return value, nil
	default:
		return "", fmt.Errorf("unsupported transport %q", value)
	}
}
