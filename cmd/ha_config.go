package cmd

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"xrok/cmd/config"
)

// HAConfig describes optional control-plane backends. Keeping this separate
// from transport configuration makes the local lab explicit and testable.
// Adapters can consume these endpoints without changing ServerRuntime APIs.
type HAConfig struct {
	Backend       string
	RedisAddr     string
	RedisPassword string
	PostgresDSN   string
}

func LoadHAConfig(getenv func(string) string) (HAConfig, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	c := HAConfig{Backend: getenv("XROK_HA_BACKEND"), RedisAddr: getenv("XROK_HA_REDIS_ADDR"), RedisPassword: getenv("XROK_HA_REDIS_PASSWORD"), PostgresDSN: getenv("XROK_HA_POSTGRES_DSN")}
	if c.Backend == "" {
		c.Backend = "memory"
	}
	if c.Backend != "memory" && c.Backend != "redis" && c.Backend != "postgres" {
		return HAConfig{}, fmt.Errorf("invalid XROK_HA_BACKEND %q", c.Backend)
	}
	if c.RedisAddr != "" && !validRedisAddr(c.RedisAddr) {
		return HAConfig{}, fmt.Errorf("invalid XROK_HA_REDIS_ADDR %q", c.RedisAddr)
	}
	if c.PostgresDSN != "" {
		u, err := url.Parse(c.PostgresDSN)
		if err != nil || u.Scheme != "postgres" || u.Host == "" {
			return HAConfig{}, fmt.Errorf("invalid XROK_HA_POSTGRES_DSN")
		}
	}
	return c, nil
}

// HAConfigFromServerConfig maps versioned configuration into runtime backend
// settings. Environment variables remain a compatibility fallback for the CLI.
func HAConfigFromServerConfig(c config.ServerConfig) (HAConfig, error) {
	b := c.StateBackend
	if b == "" {
		b = "memory"
	}
	h := HAConfig{Backend: b, RedisAddr: c.RedisAddr, RedisPassword: c.RedisPassword, PostgresDSN: c.PostgresDSN}
	if b == "redis" && !validRedisAddr(h.RedisAddr) {
		return HAConfig{}, fmt.Errorf("invalid redis_addr")
	}
	if b == "postgres" {
		u, err := url.Parse(h.PostgresDSN)
		if err != nil || u.Scheme != "postgres" || u.Host == "" {
			return HAConfig{}, fmt.Errorf("invalid postgres_dsn")
		}
	}
	return h, nil
}

func validRedisAddr(v string) bool {
	host, port, err := net.SplitHostPort(v)
	if err != nil || host == "" || port == "" {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && n > 0 && n <= 65535
}
