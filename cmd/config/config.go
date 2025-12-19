package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// ClientConfig represents client configuration
type ClientConfig struct {
	Server    string         `yaml:"server"`
	Token     string         `yaml:"token,omitempty"`
	TLS       bool           `yaml:"tls"`
	Debug     bool           `yaml:"debug"`
	Quiet     bool           `yaml:"quiet"`
	Verbose   bool           `yaml:"verbose"`
	Tunnels   []TunnelConfig `yaml:"tunnels,omitempty"`
	Socks5    string         `yaml:"socks5,omitempty"`
	UDP       []UDPConfig    `yaml:"udp,omitempty"`
	Reverse   []ReverseConfig `yaml:"reverse,omitempty"`
	Reconnect ReconnectConfig `yaml:"reconnect"`
	Headers   map[string]string `yaml:"headers,omitempty"`
	Compress  bool           `yaml:"compress"`
	LogFile   string         `yaml:"log_file,omitempty"`
	// Transport protocol: "websocket" (default), "quic"
	Transport string         `yaml:"transport,omitempty"`
	// P2P configuration
	P2P P2PConfig `yaml:"p2p,omitempty"`
	// HTTP CONNECT proxy for corporate environments
	HTTPProxy  string `yaml:"http_proxy,omitempty"`
	// mTLS client certificates
	ClientCert string `yaml:"client_cert,omitempty"`
	ClientKey  string `yaml:"client_key,omitempty"`
}

// TunnelConfig represents a tunnel configuration
type TunnelConfig struct {
	Name      string `yaml:"name"`
	Target    string `yaml:"target"`
	BasicAuth string `yaml:"basic_auth,omitempty"` // user:password
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
	Domain       string        `yaml:"domain"`
	TLSCertPath  string        `yaml:"tls_cert_path,omitempty"`
	TLSKeyPath   string        `yaml:"tls_key_path,omitempty"`
	Port         int           `yaml:"port"`
	ProxyPort    int           `yaml:"proxy_port"`
	MetricsPort  int           `yaml:"metrics_port"`
	Debug        bool          `yaml:"debug"`
	Quiet        bool          `yaml:"quiet"`
	Verbose      bool          `yaml:"verbose"`
	APIToken     string        `yaml:"api_token,omitempty"`
	RateLimit    int           `yaml:"rate_limit"`
	LogFile      string        `yaml:"log_file,omitempty"`
	StateFile    string        `yaml:"state_file,omitempty"`
	IPWhitelist  []string      `yaml:"ip_whitelist,omitempty"`
	RequestLog   bool          `yaml:"request_log"`
	Compress     bool          `yaml:"compress"`
	HealthCheck  HealthCheckConfig `yaml:"health_check"`
	// mTLS: CA certificate for verifying client certificates
	ClientCAPath string `yaml:"client_ca_path,omitempty"`
}

// HealthCheckConfig holds health check settings
type HealthCheckConfig struct {
	Enabled  bool          `yaml:"enabled"`
	Interval time.Duration `yaml:"interval"`
	Timeout  time.Duration `yaml:"timeout"`
}

// DefaultClientConfig returns default client configuration
func DefaultClientConfig() *ClientConfig {
	return &ClientConfig{
		TLS:     true,
		Debug:   false,
		Quiet:   false,
		Verbose: false,
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
		Port:        7645,
		ProxyPort:   444,
		MetricsPort: 9090,
		Debug:       false,
		Quiet:       false,
		Verbose:     false,
		RateLimit:   0, // unlimited
		RequestLog:  false,
		Compress:    false,
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

	return cfg, nil
}

// SaveClientConfig saves client configuration to file
func SaveClientConfig(cfg *ClientConfig, path string) error {
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

# Compression
compress: false

# Logging
debug: false
quiet: false
verbose: false
log_file: /var/log/xrok/server.log
`
}
