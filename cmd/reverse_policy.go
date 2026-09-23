package cmd

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Reverse (-R) listener policy. Without it any client can bind any free port
// on every interface of the host, including ports other co-hosted services
// briefly release during a restart.
var (
	serverReversePortMin   = 1024
	serverReversePortMax   = 65535
	serverReverseMaxPerCli = 10
)

// normalizeReverseRemote validates a client-requested reverse listener
// address and returns the address to bind. Only a port (optionally with an
// empty/wildcard host) is accepted, and it must fall inside the configured
// range and differ from the server's own listen port.
func normalizeReverseRemote(remote string) (string, error) {
	host, portStr := "", remote
	if strings.Contains(remote, ":") {
		var err error
		host, portStr, err = net.SplitHostPort(remote)
		if err != nil {
			return "", fmt.Errorf("invalid reverse address %q", remote)
		}
	}
	if host != "" && host != "0.0.0.0" && host != "::" {
		return "", fmt.Errorf("reverse address %q: bind host is not allowed", remote)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", fmt.Errorf("invalid reverse port %q", portStr)
	}
	if port < serverReversePortMin || port > serverReversePortMax || port == serverPort {
		return "", fmt.Errorf("reverse port %d outside allowed range %d-%d", port, serverReversePortMin, serverReversePortMax)
	}
	return ":" + strconv.Itoa(port), nil
}

func validateReverseConfigs(configs []ServerReverseConfig) ([]ServerReverseConfig, error) {
	if len(configs) > serverReverseMaxPerCli {
		return nil, fmt.Errorf("too many reverse listeners: %d (max %d)", len(configs), serverReverseMaxPerCli)
	}
	out := make([]ServerReverseConfig, 0, len(configs))
	for _, c := range configs {
		addr, err := normalizeReverseRemote(c.Remote)
		if err != nil {
			return nil, err
		}
		out = append(out, ServerReverseConfig{Remote: addr, Local: c.Local})
	}
	return out, nil
}
