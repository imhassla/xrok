package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// ClientConfig represents client configuration
type ClientConfig struct {
	// Version is the schema version. Zero denotes the legacy unversioned format.
	Version   int               `yaml:"version,omitempty"`
	Server    string            `yaml:"server"`
	ID        string            `yaml:"id,omitempty"`
	Token     string            `yaml:"token,omitempty"`
	TLS       bool              `yaml:"tls"`
	Debug     bool              `yaml:"debug"`
	Quiet     bool              `yaml:"quiet"`
	Verbose   bool              `yaml:"verbose"`
	Tunnels   []TunnelConfig    `yaml:"tunnels,omitempty"`
	Socks5    string            `yaml:"socks5,omitempty"`
	UDP       []UDPConfig       `yaml:"udp,omitempty"`
	Reverse   []ReverseConfig   `yaml:"reverse,omitempty"`
	Reconnect ReconnectConfig   `yaml:"reconnect"`
	Headers   map[string]string `yaml:"headers,omitempty"`
	Compress  bool              `yaml:"compress"`
	LogFile   string            `yaml:"log_file,omitempty"`
	// Transport protocol: "websocket" (default), "quic"
	Transport string `yaml:"transport,omitempty"`
	// ConnectionType is a deprecated alias retained for older examples.
	ConnectionType string `yaml:"connection_type,omitempty"`
	// P2P configuration
	P2P P2PConfig `yaml:"p2p,omitempty"`
	// HTTP CONNECT proxy for corporate environments
	HTTPProxy string `yaml:"http_proxy,omitempty"`
	// mTLS client certificates
	ClientCert string `yaml:"client_cert,omitempty"`
	ClientKey  string `yaml:"client_key,omitempty"`
	CACert     string `yaml:"ca_cert,omitempty"`
	Insecure   bool   `yaml:"insecure,omitempty"`
}

// TunnelConfig represents a tunnel configuration
type TunnelConfig struct {
	Name               string `yaml:"name"`
	Target             string `yaml:"target"`
	BasicAuth          string `yaml:"basic_auth,omitempty"` // user:password
	Capability         string `yaml:"capability,omitempty"`
	CapabilityAudience string `yaml:"capability_audience,omitempty"`
}

// UDPConfig represents UDP tunnel configuration
type UDPConfig struct {
	Local  string `yaml:"local"`
	Remote string `yaml:"remote"`
}

// ReverseConfig represents reverse port forwarding
type ReverseConfig struct {
	Remote string `yaml:"remote"` // Remote port to listen on
	Local  string `yaml:"local"`  // Local target to forward to
}

// ReconnectConfig holds reconnection settings
type ReconnectConfig struct {
	Enabled     bool          `yaml:"enabled"`
	MaxRetries  int           `yaml:"max_retries"`
	InitialWait time.Duration `yaml:"initial_wait"`
	MaxWait     time.Duration `yaml:"max_wait"`
}

// P2PConfig holds P2P settings
type P2PConfig struct {
	Enabled    bool   `yaml:"enabled"`
	StunServer string `yaml:"stun_server,omitempty"`
	Fallback   bool   `yaml:"fallback"` // Fallback to relay if P2P fails
}

// ServerConfig represents server configuration
type ServerConfig struct {
	Version     int    `yaml:"version,omitempty"`
	Domain      string `yaml:"domain"`
	TLSCertPath string `yaml:"tls_cert_path,omitempty"`
	TLSKeyPath  string `yaml:"tls_key_path,omitempty"`
	Port        int    `yaml:"port"`
	ProxyPort   int    `yaml:"proxy_port"`
	MetricsPort int    `yaml:"metrics_port"`
	Debug       bool   `yaml:"debug"`
	Quiet       bool   `yaml:"quiet"`
	Verbose     bool   `yaml:"verbose"`
	APIToken    string `yaml:"api_token,omitempty"`
	// ClientIdentities enables per-client control-plane credentials. APIToken is
	// retained as an administrator/legacy fallback during migration.
	ClientIdentities []ClientIdentityConfig `yaml:"client_identities,omitempty"`
	RateLimit        int                    `yaml:"rate_limit"`
	LogFile          string                 `yaml:"log_file,omitempty"`
	StateFile        string                 `yaml:"state_file,omitempty"`
	IPWhitelist      []string               `yaml:"ip_whitelist,omitempty"`
	RequestLog       bool                   `yaml:"request_log"`
	Compress         bool                   `yaml:"compress"`
	HealthCheck      HealthCheckConfig      `yaml:"health_check"`
	// mTLS: CA certificate for verifying client certificates
	ClientCAPath string `yaml:"client_ca_path,omitempty"`
	Insecure     bool   `yaml:"insecure,omitempty"`
	// StateBackend selects the runtime control-plane store: memory, redis, or postgres.
	StateBackend  string `yaml:"state_backend,omitempty"`
	RedisAddr     string `yaml:"redis_addr,omitempty"`
	RedisPassword string `yaml:"redis_password,omitempty"`
	PostgresDSN   string `yaml:"postgres_dsn,omitempty"`
	// Telemetry configures bounded asynchronous OTLP/HTTP trace export. An
	// empty endpoint keeps export disabled.
	Telemetry TelemetryConfig `yaml:"telemetry,omitempty"`
}

// TelemetryConfig is the versioned server-side trace export contract.
// Durations use Go/YAML duration syntax, for example "250ms" or "5s".
type TelemetryConfig struct {
	Endpoint        string        `yaml:"endpoint,omitempty"`
	QueueSize       int           `yaml:"queue_size,omitempty"`
	BatchSize       int           `yaml:"batch_size,omitempty"`
	FlushInterval   time.Duration `yaml:"flush_interval,omitempty"`
	ExportTimeout   time.Duration `yaml:"export_timeout,omitempty"`
	MaxRetries      int           `yaml:"max_retries,omitempty"`
	RetryBackoff    time.Duration `yaml:"retry_backoff,omitempty"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout,omitempty"`
}

// ClientIdentityConfig binds a credential to one stable client ID and an
// explicit set of control-plane scopes (for example client or p2p).
type ClientIdentityConfig struct {
	ID     string   `yaml:"id"`
	Token  string   `yaml:"token"`
	Scopes []string `yaml:"scopes,omitempty"`
}

// CurrentVersion is the on-disk configuration schema version.
const CurrentVersion = 2

func defaultTelemetryConfig() TelemetryConfig {
	return TelemetryConfig{
		QueueSize: 2048, BatchSize: 128, FlushInterval: time.Second,
		ExportTimeout: 5 * time.Second, MaxRetries: 3,
		RetryBackoff: 100 * time.Millisecond, ShutdownTimeout: 10 * time.Second,
	}
}

func migrateTelemetryConfig(c *TelemetryConfig) {
	d := defaultTelemetryConfig()
	if c.QueueSize == 0 {
		c.QueueSize = d.QueueSize
	}
	if c.BatchSize == 0 {
		c.BatchSize = d.BatchSize
	}
	if c.FlushInterval == 0 {
		c.FlushInterval = d.FlushInterval
	}
	if c.ExportTimeout == 0 {
		c.ExportTimeout = d.ExportTimeout
	}
	if c.MaxRetries == 0 {
		c.MaxRetries = d.MaxRetries
	}
	if c.RetryBackoff == 0 {
		c.RetryBackoff = d.RetryBackoff
	}
	if c.ShutdownTimeout == 0 {
		c.ShutdownTimeout = d.ShutdownTimeout
	}
}

// MigrateClientConfig upgrades legacy configs in memory and is idempotent.
func MigrateClientConfig(c *ClientConfig) {
	if c.Version < CurrentVersion {
		if c.Transport == "" && c.ConnectionType != "" {
			c.Transport = c.ConnectionType
		}
		c.Version = CurrentVersion
	}
}

// MigrateServerConfig upgrades legacy configs in memory and is idempotent.
func MigrateServerConfig(c *ServerConfig) {
	if c.Version > CurrentVersion {
		return
	}
	if c.Version < 1 {
		if c.StateBackend == "" {
			c.StateBackend = "memory"
		}
	}
	if c.Version < 2 {
		migrateTelemetryConfig(&c.Telemetry)
	}
	c.Version = CurrentVersion
	if c.StateBackend == "" {
		c.StateBackend = "memory"
	}
}

// HealthCheckConfig holds health check settings
type HealthCheckConfig struct {
	Enabled  bool          `yaml:"enabled"`
	Interval time.Duration `yaml:"interval"`
	Timeout  time.Duration `yaml:"timeout"`
}

// Validate checks settings that would otherwise fail only after the runtime
// has started goroutines and opened partial listeners.
func (c *ServerConfig) Validate() error {
	if c.Version > CurrentVersion {
		return fmt.Errorf("unsupported server config version %d (current %d)", c.Version, CurrentVersion)
	}
	if c.Domain == "" {
		return fmt.Errorf("server domain is required")
	}
	if c.ProxyPort < 0 || c.ProxyPort > 65535 {
		return fmt.Errorf("proxy_port must be between 0 and 65535")
	}
	if c.Port < 0 || c.Port > 65535 {
		return fmt.Errorf("port must be between 0 and 65535")
	}
	if c.MetricsPort < 0 || c.MetricsPort > 65535 {
		return fmt.Errorf("metrics_port must be between 0 and 65535")
	}
	switch c.StateBackend {
	case "", "memory":
	case "redis":
		if c.RedisAddr == "" {
			return fmt.Errorf("redis state backend requires redis_addr")
		}
	case "postgres":
		if c.PostgresDSN == "" {
			return fmt.Errorf("postgres state backend requires postgres_dsn")
		}
	default:
		return fmt.Errorf("unsupported state_backend %q (want memory, redis, or postgres)", c.StateBackend)
	}
	ids := make(map[string]struct{}, len(c.ClientIdentities))
	tokens := make(map[string]struct{}, len(c.ClientIdentities))
	for i, identity := range c.ClientIdentities {
		if identity.ID == "" || identity.Token == "" {
			return fmt.Errorf("client_identities[%d] requires id and token", i)
		}
		if _, exists := ids[identity.ID]; exists {
			return fmt.Errorf("duplicate client identity %q", identity.ID)
		}
		if _, exists := tokens[identity.Token]; exists {
			return fmt.Errorf("credential token is reused by multiple client identities")
		}
		ids[identity.ID] = struct{}{}
		tokens[identity.Token] = struct{}{}
		for _, scope := range identity.Scopes {
			if scope == "" {
				return fmt.Errorf("client identity %q contains an empty scope", identity.ID)
			}
		}
	}
	if t := c.Telemetry; t.Endpoint != "" {
		u, err := url.Parse(t.Endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			return fmt.Errorf("telemetry endpoint must be an http(s) URL with a host and no userinfo")
		}
		if t.QueueSize <= 0 {
			return fmt.Errorf("telemetry queue_size must be positive")
		}
		if t.BatchSize <= 0 || t.BatchSize > t.QueueSize {
			return fmt.Errorf("telemetry batch_size must be between 1 and queue_size")
		}
		if t.FlushInterval <= 0 || t.ExportTimeout <= 0 || t.RetryBackoff <= 0 || t.ShutdownTimeout <= 0 {
			return fmt.Errorf("telemetry durations must be positive")
		}
		if t.MaxRetries < 0 {
			return fmt.Errorf("telemetry max_retries must not be negative")
		}
	}
	// Empty certificate paths are valid: the server derives the Let's Encrypt
	// paths from domain, or the CLI can supply them after loading this config.
	return nil
}

// DefaultClientConfig returns default client configuration
func DefaultClientConfig() *ClientConfig {
	return &ClientConfig{
		TLS:      true,
		Debug:    false,
		Quiet:    false,
		Verbose:  false,
		Compress: false,
		Reconnect: ReconnectConfig{
			Enabled:     true,
			MaxRetries:  0, // unlimited
			InitialWait: 1 * time.Second,
			MaxWait:     5 * time.Minute,
		},
	}
}

// DefaultServerConfig returns default server configuration
func DefaultServerConfig() *ServerConfig {
	return &ServerConfig{
		Port:         7645,
		ProxyPort:    443,
		MetricsPort:  9090,
		Debug:        false,
		Quiet:        false,
		Verbose:      false,
		RateLimit:    0, // unlimited
		RequestLog:   false,
		Compress:     false,
		StateBackend: "memory",
		Telemetry:    defaultTelemetryConfig(),
		HealthCheck: HealthCheckConfig{
			Enabled:  true,
			Interval: 30 * time.Second,
			Timeout:  10 * time.Second,
		},
	}
}

// LoadClientConfig loads client configuration from file
func LoadClientConfig(path string) (*ClientConfig, error) {
	cfg := DefaultClientConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}
	MigrateClientConfig(cfg)
	if cfg.Version > CurrentVersion {
		return nil, fmt.Errorf("unsupported client config version %d (current %d)", cfg.Version, CurrentVersion)
	}

	return cfg, nil
}

// LoadServerConfig loads server configuration from file
func LoadServerConfig(path string) (*ServerConfig, error) {
	cfg := DefaultServerConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}
	MigrateServerConfig(cfg)
	if cfg.Version > CurrentVersion {
		return nil, fmt.Errorf("unsupported server config version %d (current %d)", cfg.Version, CurrentVersion)
	}

	return cfg, nil
}

// SaveClientConfig saves client configuration to file
func SaveClientConfig(cfg *ClientConfig, path string) error {
	MigrateClientConfig(cfg)
	if cfg.Version > CurrentVersion {
		return fmt.Errorf("unsupported client config version %d (current %d)", cfg.Version, CurrentVersion)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
}

// SaveServerConfig saves server configuration to file
func SaveServerConfig(cfg *ServerConfig, path string) error {
	MigrateServerConfig(cfg)
	if cfg.Version > CurrentVersion {
		return fmt.Errorf("unsupported server config version %d (current %d)", cfg.Version, CurrentVersion)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
}

// FindConfigFile looks for config file in standard locations
func FindConfigFile(name string) string {
	// Check current directory
	if _, err := os.Stat(name); err == nil {
		return name
	}

	// Check home directory
	home, err := os.UserHomeDir()
	if err == nil {
		path := filepath.Join(home, ".xrok", name)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}

	// Check /etc/xrok
	path := filepath.Join("/etc/xrok", name)
	if _, err := os.Stat(path); err == nil {
		return path
	}

	return ""
}

// GenerateExampleClientConfig creates an example client config
func GenerateExampleClientConfig() string {
	return `# xrok client configuration
server: xrok.example.com
id: client-a
token: your-api-token
tls: true

# Tunnels
tunnels:
  - name: web
    target: localhost:8080
  - name: api
    target: localhost:3000
    basic_auth: user:password

# SOCKS5 proxy
socks5: ":1080"

# UDP tunnels
udp:
  - local: ":5353"
    remote: "8.8.8.8:53"

# Reverse forwarding
reverse:
  - remote: ":9000"
    local: "localhost:9000"

# Reconnect settings
reconnect:
  enabled: true
  max_retries: 0  # 0 = unlimited
  initial_wait: 1s
  max_wait: 5m

# Custom headers
headers:
  X-Custom-Header: value

# Compression
compress: false

# Logging
debug: false
quiet: false
verbose: false
log_file: ""
`
}

// GenerateExampleServerConfig creates an example server config
func GenerateExampleServerConfig() string {
	return `# xrok server configuration
domain: xrok.example.com

# TLS certificates (auto-detected from /etc/letsencrypt if not specified)
# tls_cert_path: /etc/letsencrypt/live/xrok.example.com/fullchain.pem
# tls_key_path: /etc/letsencrypt/live/xrok.example.com/privkey.pem

# Ports
port: 7645
proxy_port: 444
metrics_port: 9090

# Authentication
api_token: your-secret-token

# Prefer per-client identities for least-privilege control-plane access.
client_identities:
  - id: client-a
    token: replace-with-a-unique-secret
    scopes: [client, p2p]

# Rate limiting (requests per second, 0 = unlimited)
rate_limit: 100

# IP whitelist (empty = allow all)
ip_whitelist: []
#  - 192.168.1.0/24
#  - 10.0.0.0/8

# Health checks
health_check:
  enabled: true
  interval: 30s
  timeout: 10s

# State persistence
state_file: /var/lib/xrok/state.json

# Request logging
request_log: false

# Bounded asynchronous OTLP/HTTP traces (disabled when endpoint is empty)
telemetry:
  endpoint: "" # e.g. http://otel-collector:4318; empty disables export
  queue_size: 2048
  batch_size: 128
  flush_interval: 1s
  export_timeout: 5s
  max_retries: 3
  retry_backoff: 100ms
  shutdown_timeout: 10s

# Compression
compress: false

# Logging
debug: false
quiet: false
verbose: false
log_file: /var/log/xrok/server.log
`
}
