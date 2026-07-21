package cmd

import (
	"testing"
	"xrok/cmd/config"
)

func TestLoadHAConfig(t *testing.T) {
	vals := map[string]string{"XROK_HA_REDIS_ADDR": "redis:6379", "XROK_HA_POSTGRES_DSN": "postgres://xrok:pw@postgres:5432/xrok?sslmode=disable"}
	c, err := LoadHAConfig(func(k string) string { return vals[k] })
	if err != nil || c.RedisAddr != "redis:6379" || c.PostgresDSN == "" {
		t.Fatalf("config=%+v err=%v", c, err)
	}
}

func TestLoadHAConfigRejectsInvalid(t *testing.T) {
	_, err := LoadHAConfig(func(k string) string {
		if k == "XROK_HA_REDIS_ADDR" {
			return "bad address"
		}
		return ""
	})
	if err == nil {
		t.Fatal("expected invalid redis address")
	}
}

func TestHAConfigFromServerConfig(t *testing.T) {
	c := config.ServerConfig{StateBackend: "redis", RedisAddr: "redis:6379"}
	h, err := HAConfigFromServerConfig(c)
	if err != nil || h.Backend != "redis" || h.RedisAddr != "redis:6379" {
		t.Fatalf("unexpected config: %#v %v", h, err)
	}
	if _, err := HAConfigFromServerConfig(config.ServerConfig{StateBackend: "postgres", PostgresDSN: "bad"}); err == nil {
		t.Fatal("expected invalid postgres dsn")
	}
}
