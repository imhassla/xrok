package cmd

import "testing"

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
