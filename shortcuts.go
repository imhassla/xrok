package main

import (
	"fmt"
	"os"
	"os/user"
	"regexp"
	"strconv"
	"strings"

	"xrok/cmd"
)

// Shortcut commands expand to `xrok client ...` and rely on the profile in
// ~/.xrok/config.yaml (server, server_port, token) for everything else.
// Remaining arguments are passed through as client flags.

var nonLabel = regexp.MustCompile(`[^a-z0-9-]+`)

func splitPositional(args []string, max int) (pos, rest []string) {
	for len(args) > 0 && len(pos) < max && !strings.HasPrefix(args[0], "-") {
		pos, args = append(pos, args[0]), args[1:]
	}
	return pos, args
}

func localTarget(s string) string {
	if _, err := strconv.Atoi(s); err == nil {
		return "localhost:" + s
	}
	return s
}

func defaultTunnelName(target string) string {
	name := "app"
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	port := target[strings.LastIndex(target, ":")+1:]
	name = strings.Trim(nonLabel.ReplaceAllString(strings.ToLower(name+"-"+port), "-"), "-")
	return name
}

func shortcutUsage(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(2)
}

// runHTTP: xrok http <port|host:port> [name] [client flags]
func runHTTP(args []string) {
	pos, rest := splitPositional(args, 2)
	if len(pos) == 0 {
		shortcutUsage("usage: xrok http <port|host:port> [name] [client flags]")
	}
	target := localTarget(pos[0])
	name := defaultTunnelName(target)
	if len(pos) > 1 {
		name = pos[1]
	}
	cmd.RunClient(append([]string{"--tunnel", name + ":" + target}, rest...))
}

// runTCP: xrok tcp <local port|host:port> <remote port> [client flags]
func runTCP(args []string) {
	pos, rest := splitPositional(args, 2)
	if len(pos) != 2 {
		shortcutUsage("usage: xrok tcp <local port|host:port> <remote port> [client flags]")
	}
	cmd.RunClient(append([]string{"-R", pos[1] + ":" + localTarget(pos[0])}, rest...))
}

// runSOCKS: xrok socks [listen addr] [client flags]
func runSOCKS(args []string) {
	pos, rest := splitPositional(args, 1)
	addr := "127.0.0.1:1080"
	if len(pos) == 1 {
		addr = pos[0]
		if _, err := strconv.Atoi(addr); err == nil {
			addr = "127.0.0.1:" + addr
		}
	}
	cmd.RunClient(append([]string{"--socks5", addr}, rest...))
}
