package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// FuzzLoadServerConfigBytes exercises YAML parsing with arbitrary input. The
// parser must return an error (or a valid config) without panicking or
// allocating unbounded process state.
func FuzzLoadServerConfigBytes(f *testing.F) {
	for _, seed := range []string{
		"domain: example.test\nport: 7645\n",
		"domain: example.test\nhealth_check:\n  interval: 1s\n",
		"{}", "\x00\xff",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		cfg := DefaultServerConfig()
		// Unmarshal directly keeps this fuzz target in-memory and avoids file
		// system effects while covering the same YAML decoder as LoadServerConfig.
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return
		}
		_ = cfg.Validate()
	})
}

func FuzzLoadClientConfigBytes(f *testing.F) {
	f.Add([]byte("server: example.test\ntls: true\n"))
	f.Add([]byte("tunnels:\n  - name: web\n    target: localhost:8080\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		cfg := DefaultClientConfig()
		_ = yaml.Unmarshal(data, cfg)
	})
}
