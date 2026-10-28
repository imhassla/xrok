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
	case "", "legacy", "tcp", "tls", "websocket":
		return "legacy", nil
	case "http2", "wss", "quic", "auto":
		return value, nil
	default:
		return "", fmt.Errorf("unsupported transport %q", value)
	}
}
