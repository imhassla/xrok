package cmd

import (
	"fmt"
	"net/url"
	"os"
)

// HAConfig describes optional control-plane backends. Keeping this separate
// from transport configuration makes the local lab explicit and testable.
// Adapters can consume these endpoints without changing ServerRuntime APIs.
type HAConfig struct {
	RedisAddr   string
	PostgresDSN string
}

func LoadHAConfig(getenv func(string) string) (HAConfig, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	c := HAConfig{RedisAddr: getenv("XROK_HA_REDIS_ADDR"), PostgresDSN: getenv("XROK_HA_POSTGRES_DSN")}
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

func validRedisAddr(v string) bool {
	u, err := url.Parse("redis://" + v)
	return err == nil && u.Host != ""
}
