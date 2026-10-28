package cmd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
	"github.com/quic-go/quic-go"
	"xrok/cmd/config"
	"xrok/cmd/logger"
	"xrok/cmd/p2p"
	"xrok/cmd/transport"
)

// TunnelConfig represents a single tunnel configuration
type TunnelConfig struct {
	Name               string `json:"name"`                 // Subdomain name (e.g., "app", "api")
	Target             string `json:"target"`               // Local target (e.g., "localhost:8080")
	BasicAuth          string `json:"basic_auth,omitempty"` // HTTP Basic Auth (user:password)
	Capability         string `json:"capability,omitempty"` // optional capability required by the public route
	CapabilityAudience string `json:"capability_audience,omitempty"`
}

// tunnelFlag implements flag.Value for multiple --tunnel flags
type tunnelFlag []TunnelConfig

type headerFlag map[string]string

func (h headerFlag) String() string { return fmt.Sprintf("%v", map[string]string(h)) }

func (h headerFlag) Set(value string) error {
	name, headerValue, ok := strings.Cut(value, ":")
	name, headerValue = strings.TrimSpace(name), strings.TrimSpace(headerValue)
	if !ok || name == "" || strings.ContainsAny(name+headerValue, "\r\n") {
		return fmt.Errorf("invalid header %q, expected Name: Value", value)
	}
	h[http.CanonicalHeaderKey(name)] = headerValue
	return nil
}

func (t *tunnelFlag) String() string {
	return fmt.Sprintf("%v", *t)
}

func (t *tunnelFlag) Set(value string) error {
	parts := strings.SplitN(value, ":", 2)
	if len(parts) == 1 {
		// No name specified, use auto-generated
		*t = append(*t, TunnelConfig{Name: "", Target: parts[0]})
	} else {
		// name:target format
		name := parts[0]
		target := parts[1]
		// Handle case like "app:localhost:8080" (name:host:port)
		if strings.Count(value, ":") >= 2 {
			idx := strings.Index(value, ":")
			name = value[:idx]
			target = value[idx+1:]
		}
		*t = append(*t, TunnelConfig{Name: name, Target: target})
	}
	return nil
}

// UDPTunnelConfig represents a UDP tunnel configuration
type UDPTunnelConfig struct {
	LocalAddr  string // Local address to listen on (e.g., ":5353")
	RemoteAddr string // Remote UDP target (e.g., "8.8.8.8:53")
}

// ReverseConfig represents reverse port forwarding configuration
type ReverseConfig struct {
	Remote string // Remote port to listen on server (e.g., ":9000")
	Local  string // Local target to forward to (e.g., "localhost:9000")
}

// udpFlag implements flag.Value for multiple --udp flags
type udpFlag []UDPTunnelConfig

func (u *udpFlag) String() string {
	return fmt.Sprintf("%v", *u)
}

func (u *udpFlag) Set(value string) error {
	// Format: local:remote (e.g., ":5353:8.8.8.8:53" or "127.0.0.1:5353:8.8.8.8:53")
	// We need to split at the third colon for IPv4 or handle IPv6 specially
	parts := strings.Split(value, ":")
	if len(parts) < 3 {
		return fmt.Errorf("invalid UDP tunnel format, expected local:remote (e.g., :5353:8.8.8.8:53)")
	}

	// Try to find the split point between local and remote
	// Format could be: :5353:host:port or host:port:host:port
	var localAddr, remoteAddr string
	if parts[0] == "" {
		// Format: :localport:remotehost:remoteport
		localAddr = ":" + parts[1]
		remoteAddr = strings.Join(parts[2:], ":")
	} else if len(parts) == 4 {
		// Format: localhost:localport:remotehost:remoteport
		localAddr = parts[0] + ":" + parts[1]
		remoteAddr = parts[2] + ":" + parts[3]
	} else {
		return fmt.Errorf("invalid UDP tunnel format: %s", value)
	}

	*u = append(*u, UDPTunnelConfig{LocalAddr: localAddr, RemoteAddr: remoteAddr})
	return nil
}

// reverseFlag implements flag.Value for multiple --reverse flags
type reverseFlag []ReverseConfig

func (r *reverseFlag) String() string {
	return fmt.Sprintf("%v", *r)
}

func (r *reverseFlag) Set(value string) error {
	// Format: remote:local (e.g., ":9000:localhost:9000" or "9000:localhost:9000")
	parts := strings.Split(value, ":")
	if len(parts) < 3 {
		return fmt.Errorf("invalid reverse format, expected remote:local (e.g., :9000:localhost:9000)")
	}

	var remoteAddr, localAddr string
	if parts[0] == "" {
		// Format: :remoteport:localhost:localport
		remoteAddr = ":" + parts[1]
		localAddr = strings.Join(parts[2:], ":")
	} else if len(parts) == 4 {
		// Format: remotehost:remoteport:localhost:localport
		remoteAddr = parts[0] + ":" + parts[1]
		localAddr = parts[2] + ":" + parts[3]
	} else if len(parts) == 3 {
		// Format: remoteport:localhost:localport
		remoteAddr = ":" + parts[0]
		localAddr = parts[1] + ":" + parts[2]
	} else {
		return fmt.Errorf("invalid reverse format: %s", value)
	}

	*r = append(*r, ReverseConfig{Remote: remoteAddr, Local: localAddr})
	return nil
}

// ReconnectConfig holds configuration for auto-reconnect with exponential backoff
type ReconnectConfig struct {
	Enabled     bool
	InitialWait time.Duration
	MaxWait     time.Duration
	MaxRetries  int // 0 = unlimited
	Multiplier  float64
}

// Backoff calculates the next wait duration with jitter
func (r *ReconnectConfig) Backoff(attempt int) time.Duration {
	if attempt <= 0 {
		return r.InitialWait
	}

	wait := float64(r.InitialWait) * math.Pow(r.Multiplier, float64(attempt-1))

	if wait > float64(r.MaxWait) {
		wait = float64(r.MaxWait)
	}

	jitter := wait * 0.25 * rand.Float64()
	return time.Duration(wait + jitter)
}

type activeConnection struct {
	WebSocketConn *websocket.Conn
	LocalConn     net.Conn
}

// TunnelInfo contains info about a registered tunnel
type TunnelInfo struct {
	Name   string `json:"name"`
	Target string `json:"target"`
	URL    string `json:"url"`
}

type clientState struct {
	ClientID            string
	ProxyURL            string
	ClientPort          string
	ServerAddr          string
	ServerPort          int    // Registration/QUIC server port
	LocalPort           string // Legacy single tunnel
	Tunnels             []TunnelConfig
	TunnelMap           map[string]string // name -> target mapping
	UDPTunnels          []UDPTunnelConfig // UDP tunnel configurations
	ReverseTunnels      []ReverseConfig   // Reverse forwarding configurations
	ReverseMap          map[string]string // remote -> local mapping
	UseTLS              bool
	UseProxy            bool
	Socks5Addr          string // SOCKS5 proxy address
	ConnectionID        string
	MuxSession          *yamux.Session
	QUICConn            *transport.QUICConn // QUIC connection
	UseQUIC             bool                // Use QUIC transport
	Transport           string
	SupportedTransports []string
	Headers             map[string]string // Custom headers to inject
	Compress            bool              // Enable compression
	ConnectTimeout      time.Duration
	ProxySession        string
	ProxySessionHeader  string
	ProxyTLSConfig      *tls.Config
	Multiplexed         bool
	HTTPProxy           string      // HTTP CONNECT proxy URL
	ClientCert          string      // Path to client certificate for mTLS
	ClientKey           string      // Path to client private key for mTLS
	TLSConfig           *tls.Config // TLS config with mTLS if configured
	SessionToken        string      // Credential returned by registration
	APIToken            string      // Control-plane credential
	StdioMode           bool        // Stdio tunneling mode
	StdioTarget         string      // Target for stdio mode
	P2PManager          *p2p.P2PManager
	P2PEnabled          bool
	P2PInfo             *p2p.PeerInfo
	P2PSignalConn       *websocket.Conn           // WebSocket for P2P signaling
	P2PSignalMutex      sync.Mutex                // Mutex for P2P signaling
	P2PConnections      map[string]net.Conn       // Active P2P connections (peerID -> conn)
	P2PConnMutex        sync.RWMutex              // Mutex for P2P connections
	P2PTunnels          map[string]*p2p.P2PTunnel // P2P tunnels for HTTP traffic (peerID -> tunnel)
	P2PTunnelsMutex     sync.RWMutex              // Mutex for P2P tunnels
	P2PNegotiations     *p2p.NegotiationStore
	P2PPeers            map[string]*p2p.PeerInfo
	P2PStateMutex       sync.RWMutex
	P2PStunServer       string
	P2PTURN             config.TURNConfig
	P2PActiveSessions   map[string]string

	// p2pLookupClient is shared by FetchViaP2P owner lookups.
	p2pLookupOnce   sync.Once
	p2pLookupClient *http.Client
}

// ClientRuntime owns client lifecycle state and synchronization.  The CLI keeps
// a compatibility pointer for legacy handlers, while new code and tests can
// inject an independent runtime instance without sharing process globals.
type ClientRuntime struct {
	mu    sync.RWMutex
	state *clientState
	log   *logger.Logger
	stop  context.CancelFunc
}

func NewClientRuntime(l *logger.Logger) *ClientRuntime {
	if l == nil {
		l = logger.New()
	}
	return &ClientRuntime{state: &clientState{TunnelMap: make(map[string]string), ReverseMap: make(map[string]string), P2PConnections: make(map[string]net.Conn), P2PTunnels: make(map[string]*p2p.P2PTunnel), P2PNegotiations: p2p.NewNegotiationStore(128, 30*time.Second), P2PPeers: make(map[string]*p2p.PeerInfo), P2PActiveSessions: make(map[string]string)}, log: l}
}

// CurrentClientRuntime exposes the active CLI runtime for embedding and
// diagnostics. It returns nil until RunClient has initialized the client.
func CurrentClientRuntime() *ClientRuntime { return clientRuntime_ }

// currentClientState resolves the active state through the injected runtime.
// The global pointer is retained only as a compatibility fallback for legacy
// callers; new handlers should use this accessor instead of clientState_.
func currentClientState() *clientState {
	if r := clientRuntime_; r != nil {
		if s := r.State(); s != nil {
			return s
		}
	}
	clientStateMutex.Lock()
	defer clientStateMutex.Unlock()
	return clientState_
}

// State returns a snapshot pointer for read-only inspection. Mutations should
// use Update to preserve lifecycle synchronization.
func (r *ClientRuntime) State() *clientState { r.mu.RLock(); defer r.mu.RUnlock(); return r.state }

func (r *ClientRuntime) Update(fn func(*clientState)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(r.state)
}

func (r *ClientRuntime) replaceState(s *clientState) {
	r.mu.Lock()
	r.state = s
	r.mu.Unlock()
}

func (r *ClientRuntime) Logger() *logger.Logger { return r.log }

func (r *ClientRuntime) WithContext(parent context.Context) context.Context {
	ctx, cancel := context.WithCancel(parent)
	r.mu.Lock()
	r.stop = cancel
	r.mu.Unlock()
	return ctx
}

func (r *ClientRuntime) Close() {
	r.mu.Lock()
	if r.stop != nil {
		r.stop()
		r.stop = nil
	}
	s := r.state
	r.mu.Unlock()
	if s != nil && s.MuxSession != nil {
		_ = s.MuxSession.Close()
	}
	closeP2PState(s)
}

type registerResponse struct {
	Transports    []string     `json:"transports,omitempty"`
	UDID          string       `json:"udid"`
	ProxyURLTLS   string       `json:"proxy_url_tls"`
	ProxyURLNoTLS string       `json:"proxy_url_notls"`
	ClientPortTLS string       `json:"client_port_tls"`
	Tunnels       []TunnelInfo `json:"tunnels,omitempty"`
	SessionToken  string       `json:"session_token"`
	Multiplexed   bool         `json:"multiplexed"`
}

var (
	clientRp          int
	clientDebug       bool
	clientID          string
	reconnectConfig   *ReconnectConfig
	clientState_      *clientState
	clientStateMutex  sync.Mutex
	clientRuntime_    *ClientRuntime
	isConnected       atomic.Bool
	reconnectCount    atomic.Int64
	clientLog         *logger.Logger
	activeConnections = struct {
		sync.RWMutex
		connections map[string]*activeConnection
	}{connections: make(map[string]*activeConnection)}
)

// RunClient runs the xrok client with the given arguments
func RunClient(args []string) {
	fs := flag.NewFlagSet("client", flag.ExitOnError)

	// Config file
	configFile := fs.String("config", "", "Path to YAML config file")

	localPort := fs.String("port", "", "address:port of the existing application to forward to (legacy, use --tunnel instead)")
	folderPath := fs.String("folder", "", "path to a folder to serve files from")
	fs.IntVar(&clientRp, "rp", 443, "Port for client registration")
	serverAddr := fs.String("server", "", "server name (required)")
	useProxy := fs.Bool("proxy", false, "use local HTTP proxy")
	proxyAddr := fs.String("proxy-addr", "127.0.0.1:8080", "Local HTTP proxy listen address")
	useTLS := fs.Bool("tls", true, "use TLS for connections")
	fs.BoolVar(&clientDebug, "debug", false, "Enable debug logging")
	fs.StringVar(&clientID, "id", "", "Custom client ID for registration")
	apiToken := fs.String("token", "", "API token for authentication")

	// Quiet and verbose modes
	quietMode := fs.Bool("q", false, "Quiet mode (only errors)")
	verboseMode := fs.Bool("v", false, "Verbose mode (debug output)")

	// Multiple tunnels support
	var tunnels tunnelFlag
	fs.Var(&tunnels, "tunnel", "Tunnel specification: name:target (can be used multiple times, e.g., --tunnel app:localhost:8080 --tunnel api:localhost:3000)")
	headers := headerFlag{}
	fs.Var(headers, "header", "Custom upstream header Name:Value (repeatable)")

	// UDP tunnels support
	var udpTunnels udpFlag
	fs.Var(&udpTunnels, "udp", "UDP tunnel: local:remote (e.g., --udp :5353:8.8.8.8:53 forwards local UDP port 5353 to 8.8.8.8:53)")

	// Reverse forwarding support
	var reverseTunnels reverseFlag
	fs.Var(&reverseTunnels, "R", "Reverse forwarding: remote:local (e.g., -R 9000:localhost:9000 listens on server port 9000 and forwards to local port)")

	// SOCKS5 proxy support
	socks5Addr := fs.String("socks5", "", "Start SOCKS5 proxy on this address (e.g., :1080 or 127.0.0.1:1080)")

	// Reconnect configuration
	reconnectEnabled := fs.Bool("reconnect", true, "Enable auto-reconnect on connection loss")
	maxRetries := fs.Int("max-retries", 0, "Maximum retry attempts, 0=unlimited")
	retryInterval := fs.Duration("retry-interval", 1*time.Second, "Initial retry interval")
	maxInterval := fs.Duration("max-interval", 5*time.Minute, "Maximum retry interval")

	// Compression
	enableCompress := fs.Bool("compress", false, "Enable compression for tunnel data")

	// Transport selection
	useQUIC := fs.Bool("quic", false, "Use QUIC transport instead of WebSocket")
	transportMode := fs.String("transport", "", "Tunnel transport: legacy|auto|http2|wss|quic")

	// HTTP CONNECT proxy (for corporate proxies)
	httpProxy := fs.String("http-proxy", "", "Upstream proxy URL: http, https, socks5 (local DNS), or socks5h (proxy DNS)")

	connectTimeout := fs.Duration("connect-timeout", 30*time.Second, "Total connection establishment budget")
	proxySession := fs.String("proxy-session", "", "Opaque relay affinity marker for every HTTP CONNECT")
	proxySessionHeader := fs.String("proxy-session-header", "X-Proxy-Session", "Relay affinity header name")
	proxyCACert := fs.String("proxy-ca-cert", "", "CA bundle for HTTPS proxy TLS, separate from target TLS")

	// mTLS client certificates
	clientCert := fs.String("client-cert", "", "Path to client certificate for mTLS")
	clientKey := fs.String("client-key", "", "Path to client private key for mTLS")
	caCert := fs.String("ca-cert", "", "Path to CA certificate for server verification")
	insecureTLS := fs.Bool("insecure", false, "Skip TLS verification (development only)")

	// Stdio tunneling mode
	stdioMode := fs.Bool("stdio", false, "Run in stdio mode for SSH ProxyCommand")
	stdioTarget := fs.String("stdio-target", "", "Target for stdio mode (e.g., internal-host:22)")

	// P2P options
	enableP2P := fs.Bool("p2p", false, "Enable P2P direct connections")
	p2pStunServer := fs.String("stun-server", "stun.l.google.com:19302", "STUN server for P2P NAT discovery")
	p2pFallback := fs.Bool("p2p-fallback", true, "Fallback to relay if P2P fails")
	turnServer := fs.String("turn-server", "", "TURN server host:port (enables authenticated ICE relay fallback)")
	turnUsername := fs.String("turn-username", "", "TURN long-term credential username")
	turnPassword := fs.String("turn-password", os.Getenv("XROK_TURN_PASSWORD"), "TURN password (prefer XROK_TURN_PASSWORD or protected config)")
	turnTransport := fs.String("turn-transport", "", "TURN control transport: udp, tcp, or tls")
	turnCACert := fs.String("turn-ca-cert", "", "CA certificate for TURN TLS verification")
	turnServerName := fs.String("turn-server-name", "", "TURN TLS certificate server name")
	turnInsecure := fs.Bool("turn-insecure", false, "Skip TURN TLS verification (isolated development only)")
	turnChannelBind := fs.Bool("turn-channel-bind", false, "Use TURN ChannelBind/ChannelData with indication fallback")
	turnTimeout := fs.Duration("turn-timeout", 0, "TURN transaction timeout")
	turnRefreshInterval := fs.Duration("turn-refresh-interval", 0, "TURN allocation/permission refresh interval")
	turnMaxDatagram := fs.Int("turn-max-datagram", 0, "Maximum TURN payload datagram bytes")
	turnRetransmits := fs.Int("turn-retransmits", 0, "TURN UDP retransmits (0 uses bounded default)")

	fs.Parse(args)
	rpSet := false
	setFlags := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) {
		setFlags[f.Name] = true
		if f.Name == "rp" {
			rpSet = true
		}
	})
	// Without --config, fall back to $XROK_CONFIG or ~/.xrok/config.yaml so
	// a profile holding server/port/token makes `xrok client --tunnel ...`
	// work without repeating them.
	if *configFile == "" {
		*configFile = defaultClientConfigPath()
	}

	// Setup logging
	setupClientLogging()
	// Keep lifecycle state behind an injectable runtime. Legacy handlers still
	// access clientState_ during the CLI migration.
	clientRuntime_ = NewClientRuntime(clientLog)

	// Configure logger based on flags (will be adjusted after config load)
	if *quietMode {
		clientLog.SetQuiet(true)
	}
	if *verboseMode || clientDebug {
		clientDebug = true
		clientLog.SetVerbose(true)
	}

	// Load config file if specified
	if *configFile != "" {
		cfg, err := loadClientConfigFile(*configFile)
		if err != nil {
			fmt.Printf("Failed to load config: %v\n", err)
			os.Exit(1)
		}
		// Apply config values (CLI flags override config)
		if *serverAddr == "" {
			*serverAddr = cfg.Server
		}
		if cfg.ServerPort > 0 && !rpSet {
			clientRp = cfg.ServerPort
		}
		if *apiToken == "" {
			*apiToken = cfg.Token
		}
		if clientID == "" {
			clientID = cfg.ID
		}
		if len(tunnels) == 0 && len(cfg.Tunnels) > 0 {
			for _, t := range cfg.Tunnels {
				tunnels = append(tunnels, TunnelConfig{Name: t.Name, Target: t.Target, BasicAuth: t.BasicAuth, Capability: t.Capability, CapabilityAudience: t.CapabilityAudience})
			}
		}
		if len(udpTunnels) == 0 && len(cfg.UDP) > 0 {
			for _, u := range cfg.UDP {
				udpTunnels = append(udpTunnels, UDPTunnelConfig{LocalAddr: u.Local, RemoteAddr: u.Remote})
			}
		}
		if *socks5Addr == "" {
			*socks5Addr = cfg.Socks5
		}
		if len(reverseTunnels) == 0 && len(cfg.Reverse) > 0 {
			for _, r := range cfg.Reverse {
				reverseTunnels = append(reverseTunnels, ReverseConfig{Remote: r.Remote, Local: r.Local})
			}
		}
		// Apply compression from config
		if cfg.Compress && !*enableCompress {
			*enableCompress = cfg.Compress
		}
		// Apply transport from config
		if *transportMode == "" && !*useQUIC {
			*transportMode = cfg.Transport
			if *transportMode == "" {
				*transportMode = cfg.ConnectionType
			}
		}
		// Apply TLS setting from config (CLI can override)
		if !*useTLS && cfg.TLS {
			*useTLS = cfg.TLS
		}
		// Apply debug/quiet/verbose from config
		if cfg.Debug {
			clientDebug = true
		}
		if cfg.Quiet {
			*quietMode = true
		}
		if cfg.Verbose {
			*verboseMode = true
		}
		// Apply reconnect settings from config
		if cfg.Reconnect.InitialWait > 0 {
			*retryInterval = cfg.Reconnect.InitialWait
		}
		if cfg.Reconnect.MaxWait > 0 {
			*maxInterval = cfg.Reconnect.MaxWait
		}
		if cfg.Reconnect.MaxRetries > 0 {
			*maxRetries = cfg.Reconnect.MaxRetries
		}
		*reconnectEnabled = cfg.Reconnect.Enabled

		// Apply HTTP proxy from config
		if !setFlags["http-proxy"] {
			*httpProxy = cfg.HTTPProxy
		}
		if !setFlags["connect-timeout"] && cfg.ConnectTimeout > 0 {
			*connectTimeout = cfg.ConnectTimeout
		}
		if !setFlags["proxy-session"] {
			*proxySession = cfg.ProxySession
		}
		if !setFlags["proxy-session-header"] && cfg.ProxySessionHeader != "" {
			*proxySessionHeader = cfg.ProxySessionHeader
		}
		if !setFlags["proxy-ca-cert"] {
			*proxyCACert = cfg.ProxyCACert
		}
		// Apply mTLS certificates from config
		if *clientCert == "" && cfg.ClientCert != "" {
			*clientCert = cfg.ClientCert
		}
		if *clientKey == "" && cfg.ClientKey != "" {
			*clientKey = cfg.ClientKey
		}
		if *caCert == "" && cfg.CACert != "" {
			*caCert = cfg.CACert
		}
		if cfg.Insecure {
			*insecureTLS = true
		}

		// Apply P2P config
		if cfg != nil && cfg.P2P.Enabled && !*enableP2P {
			*enableP2P = true
		}
		if cfg != nil && cfg.P2P.StunServer != "" {
			*p2pStunServer = cfg.P2P.StunServer
		}
		if cfg != nil && !cfg.P2P.Fallback {
			*p2pFallback = cfg.P2P.Fallback
		}
		if cfg != nil && cfg.P2P.TURN.Enabled {
			if *turnServer == "" {
				*turnServer = cfg.P2P.TURN.Server
			}
			if *turnUsername == "" {
				*turnUsername = cfg.P2P.TURN.Username
			}
			if *turnPassword == "" {
				*turnPassword = cfg.P2P.TURN.Password
			}
			if *turnTransport == "" {
				*turnTransport = cfg.P2P.TURN.Transport
			}
			if *turnCACert == "" {
				*turnCACert = cfg.P2P.TURN.CACert
			}
			if *turnServerName == "" {
				*turnServerName = cfg.P2P.TURN.ServerName
			}
			if cfg.P2P.TURN.Insecure {
				*turnInsecure = true
			}
			if cfg.P2P.TURN.ChannelBind {
				*turnChannelBind = true
			}
			if *turnTimeout == 0 {
				*turnTimeout = cfg.P2P.TURN.Timeout
			}
			if *turnRefreshInterval == 0 {
				*turnRefreshInterval = cfg.P2P.TURN.RefreshInterval
			}
			if *turnMaxDatagram == 0 {
				*turnMaxDatagram = cfg.P2P.TURN.MaxDatagramSize
			}
			if *turnRetransmits == 0 {
				*turnRetransmits = cfg.P2P.TURN.Retransmits
			}
		}

		// Reconfigure logger after loading config
		if *quietMode {
			clientLog.SetQuiet(true)
		}
		if *verboseMode || clientDebug {
			clientLog.SetVerbose(true)
		}
	}

	if *serverAddr == "" {
		clientLog.Error("The --server flag or 'server' in config file is required.")
		os.Exit(1)
	}
	mode, modeErr := config.TunnelTransport(*transportMode, *useQUIC)
	if modeErr != nil {
		clientLog.Error("%v", modeErr)
		os.Exit(1)
	}
	*useQUIC = mode == "quic"
	if mode != "legacy" && !*useTLS {
		clientLog.Error("standard transports require TLS")
		os.Exit(1)
	}
	// Accept --server host:port; every URL is built as host:rp.
	if host, port, err := net.SplitHostPort(*serverAddr); err == nil {
		p, perr := strconv.Atoi(port)
		if perr != nil || p <= 0 || p > 65535 {
			clientLog.Error("Invalid port in --server %q", *serverAddr)
			os.Exit(1)
		}
		*serverAddr = host
		if !rpSet {
			clientRp = p
		}
	}
	if *apiToken == "" {
		*apiToken = os.Getenv("XROK_TOKEN")
	}

	if *connectTimeout <= 0 {
		clientLog.Error("connect-timeout must be positive")
		os.Exit(1)
	}
	if err := config.ValidateProxySettings(*httpProxy, *proxySession, *proxySessionHeader, *connectTimeout, *useQUIC, *enableP2P); err != nil {
		clientLog.Error("Invalid proxy configuration: %v", err)
		os.Exit(1)
	}
	turnRuntimeConfig := config.TURNConfig{
		Enabled: *turnServer != "", Server: *turnServer, Username: *turnUsername, Password: *turnPassword,
		Transport: *turnTransport, CACert: *turnCACert, ServerName: *turnServerName,
		Insecure: *turnInsecure, ChannelBind: *turnChannelBind, Timeout: *turnTimeout,
		RefreshInterval: *turnRefreshInterval, MaxDatagramSize: *turnMaxDatagram, Retransmits: *turnRetransmits,
	}
	if turnRuntimeConfig.Enabled {
		if turnRuntimeConfig.Transport == "" {
			turnRuntimeConfig.Transport = "udp"
		}
		validation := &config.ClientConfig{Version: config.CurrentVersion, P2P: config.P2PConfig{Enabled: *enableP2P, TURN: turnRuntimeConfig}}
		if err := validation.Validate(); err != nil {
			clientLog.Error("Invalid TURN configuration: %v", err)
			os.Exit(1)
		}
	}

	// Build tunnel list from flags
	var tunnelConfigs []TunnelConfig

	// Support legacy --port flag
	if *localPort != "" {
		tunnelConfigs = append(tunnelConfigs, TunnelConfig{Name: clientID, Target: *localPort})
	}

	// Add tunnels from --tunnel flags
	tunnelConfigs = append(tunnelConfigs, tunnels...)

	// File sharing owns exactly one tunnel and replaces its target with the local file server.
	if *folderPath != "" {
		if len(tunnelConfigs) > 1 {
			clientLog.Error("--folder supports exactly one tunnel")
			os.Exit(1)
		}
		if clientID == "" {
			clientID = fmt.Sprintf("files-%08x", rand.Uint32())
		}
		if len(tunnelConfigs) == 0 {
			tunnelConfigs = append(tunnelConfigs, TunnelConfig{Name: clientID, Target: "folder-placeholder"})
		} else {
			tunnelConfigs[0].Target = "folder-placeholder"
		}
	}

	if len(tunnelConfigs) == 0 && *folderPath == "" && !*useProxy && *socks5Addr == "" && len(udpTunnels) == 0 && len(reverseTunnels) == 0 {
		clientLog.Error("Specify --tunnel, --port, --folder, --socks5, or --udp to expose a service.")
		clientLog.Info("Examples:")
		clientLog.Info("  --tunnel app:localhost:8080")
		clientLog.Info("  --tunnel app:localhost:8080 --tunnel api:localhost:3000")
		clientLog.Info("  --port localhost:8080")
		clientLog.Info("  --socks5 :1080")
		clientLog.Info("  --udp :5353:8.8.8.8:53")
		os.Exit(1)
	}

	reconnectConfig = &ReconnectConfig{
		Enabled:     *reconnectEnabled,
		InitialWait: *retryInterval,
		MaxWait:     *maxInterval,
		MaxRetries:  *maxRetries,
		Multiplier:  2.0,
	}

	// Build tunnel map for quick lookup
	tunnelMap := make(map[string]string)
	for _, t := range tunnelConfigs {
		if t.Name != "" {
			tunnelMap[t.Name] = t.Target
		}
	}

	// Build reverse map for quick lookup
	reverseMap := make(map[string]string)
	for _, r := range reverseTunnels {
		reverseMap[r.Remote] = r.Local
	}

	// Get headers from config
	var customHeaders map[string]string
	if *configFile != "" {
		cfg, _ := loadClientConfigFile(*configFile)
		if cfg != nil {
			customHeaders = make(map[string]string, len(cfg.Headers)+len(headers))
			for key, value := range cfg.Headers {
				customHeaders[http.CanonicalHeaderKey(key)] = value
			}
		}
	}
	if customHeaders == nil {
		customHeaders = make(map[string]string, len(headers))
	}
	for key, value := range headers {
		customHeaders[key] = value
	}

	clientStateMutex.Lock()
	clientState_ = &clientState{
		ClientID:           clientID,
		ServerAddr:         *serverAddr,
		ServerPort:         clientRp,
		UseTLS:             *useTLS,
		UseProxy:           *useProxy,
		Tunnels:            tunnelConfigs,
		TunnelMap:          tunnelMap,
		UDPTunnels:         udpTunnels,
		ReverseTunnels:     reverseTunnels,
		ReverseMap:         reverseMap,
		Socks5Addr:         *socks5Addr,
		UseQUIC:            *useQUIC,
		Transport:          mode,
		Headers:            customHeaders,
		Compress:           *enableCompress,
		HTTPProxy:          *httpProxy,
		ConnectTimeout:     *connectTimeout,
		ProxySession:       *proxySession,
		ProxySessionHeader: *proxySessionHeader,
		ClientCert:         *clientCert,
		ClientKey:          *clientKey,
		APIToken:           *apiToken,
		StdioMode:          *stdioMode,
		StdioTarget:        *stdioTarget,
		P2PConnections:     make(map[string]net.Conn),
		P2PTunnels:         make(map[string]*p2p.P2PTunnel),
		P2PNegotiations:    p2p.NewNegotiationStore(128, 30*time.Second),
		P2PPeers:           make(map[string]*p2p.PeerInfo),
		P2PStunServer:      *p2pStunServer,
		P2PTURN:            turnRuntimeConfig,
		P2PActiveSessions:  make(map[string]string),
	}
	clientRuntime_.replaceState(clientState_)
	state := clientRuntime_.State()
	// For backwards compatibility with single tunnel
	if len(tunnelConfigs) == 1 {
		state.LocalPort = tunnelConfigs[0].Target
	}

	rootCAs, err := loadRootCAs(*caCert)
	if err != nil {
		clientLog.Error("Failed to load CA certificate: %v", err)
		os.Exit(1)
	}
	proxyRoots, err := loadRootCAs(*proxyCACert)
	if err != nil {
		clientLog.Error("Failed to load proxy CA certificate: %v", err)
		os.Exit(1)
	}
	state.ProxyTLSConfig = &tls.Config{RootCAs: proxyRoots, MinVersion: tls.VersionTLS12}
	baseTLSConfig := &tls.Config{RootCAs: rootCAs, InsecureSkipVerify: *insecureTLS, MinVersion: tls.VersionTLS12}
	if *insecureTLS {
		clientLog.Warn("TLS certificate verification is disabled")
	}

	// Initialize mTLS if client certificates are provided
	if *clientCert != "" && *clientKey != "" {
		cert, err := tls.LoadX509KeyPair(*clientCert, *clientKey)
		if err != nil {
			clientLog.Error("Failed to load client certificate: %v", err)
			os.Exit(1)
		}
		baseTLSConfig.Certificates = []tls.Certificate{cert}
		state.TLSConfig = baseTLSConfig
		clientLog.Info("mTLS enabled with client certificate")
	} else {
		state.TLSConfig = baseTLSConfig
	}

	// Log HTTP proxy if configured
	if *httpProxy != "" {
		clientLog.Info("Using upstream proxy: %s", redactedProxy(*httpProxy))
	}
	clientStateMutex.Unlock()

	// Initialize P2P if enabled
	if *enableP2P && state.APIToken == "" {
		clientLog.Warn("P2P requires authenticated signaling; configure an API token to enable it")
		*enableP2P = false
	}
	if *enableP2P {
		// Without --id the server assigns the identity (the principal of a
		// scoped credential, or a fresh ID for the legacy token). The manager
		// then adopts it after the first registration; see registerClient.
		p2pMgr := p2p.NewP2PManager(clientID, *p2pStunServer)
		peerInfo, err := p2pMgr.DiscoverPublicAddr()
		if err != nil {
			clientLog.Warn("P2P public discovery failed: %v; continuing with host/TURN ICE candidates", err)
			peerInfo = p2pMgr.SignalingPeerInfo()
		}
		clientStateMutex.Lock()
		activateP2PManager(state, p2pMgr, peerInfo)
		clientStateMutex.Unlock()
		if peerInfo.PublicAddr != "" {
			clientLog.Info("P2P enabled: %s (NAT: %s)", peerInfo.PublicAddr, peerInfo.NATType)
		} else {
			clientLog.Info("P2P enabled with host/TURN ICE candidates")
		}
	}

	// Handle stdio mode
	if *stdioMode {
		if *stdioTarget == "" {
			clientLog.Error("--stdio-target is required when using --stdio mode")
			os.Exit(1)
		}
		runStdioMode(state, *apiToken)
		return
	}

	if *enableCompress {
		clientLog.Info("Compression enabled (permessage-deflate)")
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-stop
		clientLog.Warn("Shutting down gracefully...")
		cancel()
	}()

	if *folderPath != "" {
		port, err := getRandomFreePort(8000, 9000)
		if err != nil {
			log.Fatalf("Failed to find a free port: %v", err)
		}
		folderAddr := fmt.Sprintf("127.0.0.1:%d", port)
		clientStateMutex.Lock()
		state.LocalPort = folderAddr
		// Update TunnelMap with actual folder server port
		for i := range state.Tunnels {
			if state.Tunnels[i].Target == "folder-placeholder" {
				state.Tunnels[i].Target = folderAddr
				state.TunnelMap[state.Tunnels[i].Name] = folderAddr
			}
		}
		clientStateMutex.Unlock()
		wg.Add(1)
		go serveFolderLocally(ctx, *folderPath, folderAddr, &wg)
	} else {
		clientStateMutex.Lock()
		state.LocalPort = *localPort
		clientStateMutex.Unlock()
	}

	if *useProxy {
		wg.Add(1)
		go startLocalProxy(ctx, *proxyAddr, &wg)
		clientStateMutex.Lock()
		state.LocalPort = *proxyAddr
		clientStateMutex.Unlock()
	}

	// Start SOCKS5 proxy server if requested
	var socks5Server *SOCKS5Server
	if *socks5Addr != "" {
		// Dial function that routes through the tunnel
		dialFunc := func(target string) (net.Conn, error) {
			return openDynamicStream(target)
		}

		var err error
		socks5Server, err = NewSOCKS5Server(*socks5Addr, dialFunc)
		if err != nil {
			log.Fatalf("Failed to start SOCKS5 server: %v", err)
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			go func() {
				<-ctx.Done()
				socks5Server.Close()
			}()
			if err := socks5Server.Serve(); err != nil {
				log.Printf("SOCKS5 server error: %v", err)
			}
		}()

		clientLog.Success("SOCKS5 proxy listening on %s", *socks5Addr)
	}

	// Start UDP tunnel listeners
	for _, udpTunnel := range udpTunnels {
		tunnel := udpTunnel // capture for closure
		wg.Add(1)
		go func() {
			defer wg.Done()
			runUDPTunnel(ctx, tunnel.LocalAddr, tunnel.RemoteAddr)
		}()
		clientLog.Success("UDP tunnel listening on %s -> %s", tunnel.LocalAddr, tunnel.RemoteAddr)
	}

	wg.Add(1)
	go runWithReconnect(ctx, state, *apiToken, &wg)

	wg.Wait()

	// Close the data connection explicitly before exiting: the session
	// goroutines that would close it may not run before the process ends,
	// and without a QUIC CONNECTION_CLOSE the server keeps this client's
	// subdomains until its idle timeout.
	clientStateMutex.Lock()
	if state.QUICConn != nil {
		state.QUICConn.Close()
	}
	clientState_ = nil
	clientStateMutex.Unlock()

	clientLog.Info("Client has shut down.")
}

// adoptAssignedP2PIdentity binds a P2P manager started without an ID to the
// client ID the server assigned, so punch and signaling use the same identity
// the server bound to this registration.
func adoptAssignedP2PIdentity(state *clientState, assigned string) {
	if state.P2PManager == nil || !state.P2PManager.AdoptPeerID(assigned) {
		return
	}
	if state.P2PInfo != nil {
		state.P2PInfo.PeerID = assigned
	}
}

func activateP2PManager(state *clientState, manager *p2p.P2PManager, info *p2p.PeerInfo) {
	state.P2PManager = manager
	state.P2PEnabled = manager != nil && info != nil
	state.P2PInfo = info
}

func setupClientLogging() {
	clientLog = logger.New()
	clientLog.SetPrefix("client")

	// Also log to file
	logFile := "client.log"
	if err := clientLog.SetLogFile(logFile); err != nil {
		fmt.Printf("Warning: Failed to open log file %s: %v\n", logFile, err)
	}

	// Set up standard log for library logging
	file, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err == nil {
		log.SetOutput(file)
		log.SetFlags(log.LstdFlags | log.Lshortfile)
	}
}

// loadClientConfigFile loads client configuration from a YAML file
// defaultClientConfigPath returns $XROK_CONFIG, else ~/.xrok/config.yaml when
// it exists, else "".
// serverAuthority is host:port for URLs, bracketing IPv6 literals.
func serverAuthority(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func defaultClientConfigPath() string {
	if p := os.Getenv("XROK_CONFIG"); p != "" {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".xrok", "config.yaml")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func loadClientConfigFile(path string) (*config.ClientConfig, error) {
	return config.LoadClientConfig(path)
}

func loadRootCAs(path string) (*x509.CertPool, error) {
	if path == "" {
		return nil, nil
	}
	pemData, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pemData) {
		return nil, fmt.Errorf("no certificates found in %s", path)
	}
	return pool, nil
}

func getRandomFreePort(min, max int) (int, error) {
	for i := 0; i < 100; i++ {
		port := rand.Intn(max-min+1) + min
		address := fmt.Sprintf(":%d", port)
		l, err := net.Listen("tcp", address)
		if err == nil {
			l.Close()
			return port, nil
		}
	}
	return 0, fmt.Errorf("could not find a free port in the range %d-%d", min, max)
}

func serveFolderLocally(ctx context.Context, folderPath string, localPort string, wg *sync.WaitGroup) {
	defer wg.Done()

	absPath, err := filepath.Abs(folderPath)
	if err != nil {
		log.Fatalf("Invalid folder path: %s", folderPath)
	}

	if _, err := os.Stat(absPath); os.IsNotExist(err) {
		log.Fatalf("Directory %s does not exist", absPath)
	}

	fileServer := http.FileServer(http.Dir(absPath))
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		filePath := absPath + r.URL.Path

		fileInfo, err := os.Stat(filePath)
		if err != nil {
			http.NotFound(w, r)
			return
		}

		if fileInfo.Size() > 100*1024*1024*1024 {
			http.Error(w, "File is too large", http.StatusForbidden)
			return
		}

		if !fileInfo.IsDir() {
			w.Header().Set("Content-Disposition", "attachment; filename="+url.QueryEscape(fileInfo.Name()))
			w.Header().Set("Cache-Control", "public, max-age=86400")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Connection", "keep-alive")
			w.Header().Set("X-XSS-Protection", "1; mode=block")
		}

		fileServer.ServeHTTP(w, r)
	})

	server := &http.Server{
		Addr:    localPort,
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		server.Shutdown(context.Background())
	}()

	log.Printf("Serving folder %s on %s", absPath, localPort)

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Failed to start local file server: %v", err)
	}
}

func openDynamicStream(target string) (net.Conn, error) {
	if len(target) == 0 || len(target) > 255 {
		return nil, fmt.Errorf("invalid dynamic target length")
	}
	state := currentClientState()
	var session *yamux.Session
	if state != nil {
		session = state.MuxSession
	}
	if session == nil {
		return nil, fmt.Errorf("no tunnel connection available")
	}
	stream, err := session.Open()
	if err != nil {
		return nil, fmt.Errorf("failed to open mux stream: %w", err)
	}
	header := make([]byte, 2+len(target))
	header[0], header[1] = 0xFF, byte(len(target))
	copy(header[2:], target)
	if _, err := stream.Write(header); err != nil {
		stream.Close()
		return nil, fmt.Errorf("failed to send dynamic target: %w", err)
	}
	return stream, nil
}

func startLocalProxy(ctx context.Context, localAddr string, wg *sync.WaitGroup) {
	defer wg.Done()

	// No overall client timeout: it would cut downloads longer than it.
	// Only waiting for response headers is bounded.
	client := &http.Client{
		Transport: &http.Transport{
			DisableKeepAlives:     false,
			MaxIdleConns:          1000,
			MaxIdleConnsPerHost:   100,
			IdleConnTimeout:       120 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return openDynamicStream(addr)
			},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	// A plain handler, not http.ServeMux: ServeMux cannot route CONNECT
	// requests (their path is empty) and answered 404, so HTTPS through the
	// proxy never worked.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			proxyConn, err := openDynamicStream(r.Host)
			if err != nil {
				http.Error(w, "Failed to connect", http.StatusBadGateway)
				return
			}
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				proxyConn.Close()
				http.Error(w, "Hijacking unsupported", http.StatusInternalServerError)
				return
			}
			clientConn, clientBuf, err := hijacker.Hijack()
			if err != nil {
				proxyConn.Close()
				return
			}
			_, _ = clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
			go func() {
				defer clientConn.Close()
				defer proxyConn.Close()
				done := make(chan struct{}, 2)
				// Read through the hijack buffer: bytes sent right after the
				// CONNECT headers may already sit in it.
				go func() { _, _ = io.Copy(proxyConn, clientBuf); done <- struct{}{} }()
				go func() { _, _ = io.Copy(clientConn, proxyConn); done <- struct{}{} }()
				<-done
			}()
			return
		}
		if clientDebug {
			log.Printf("Proxying request: %s %s", r.Method, r.URL.String())
		}

		if r.URL.Host == "" {
			r.URL.Host = r.Host
		}
		r.URL.Scheme = "http"
		r.Host = r.URL.Host
		r.RequestURI = ""

		r.Header.Del("Connection")
		r.Header.Del("Proxy-Connection")
		r.Header.Del("Transfer-Encoding")
		r.Header.Del("Content-Length")

		resp, err := client.Do(r)
		if err != nil {
			http.Error(w, "Failed to forward request", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")

		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	})

	server := &http.Server{
		Addr:              localAddr,
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
	}

	go func() {
		<-ctx.Done()
		server.Shutdown(context.Background())
	}()

	if clientDebug {
		log.Printf("Starting local HTTP proxy on %s", localAddr)
	}

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Failed to start local HTTP proxy: %v", err)
	}
}

// ReverseConfigJSON is the JSON format for reverse forwarding
type ReverseConfigJSON struct {
	Remote string `json:"remote"`
	Local  string `json:"local"`
}

// registrationRequest is sent to the server during registration
type registrationRequest struct {
	ConnectionType string              `json:"connection_type"`
	ClientID       string              `json:"client_id,omitempty"`
	Tunnels        []TunnelConfig      `json:"tunnels,omitempty"`
	Reverse        []ReverseConfigJSON `json:"reverse,omitempty"`
	Headers        map[string]string   `json:"headers,omitempty"`
	Compress       bool                `json:"compress,omitempty"`
	P2PInfo        *p2p.PeerInfo       `json:"p2p_info,omitempty"`
}

func registerClient(ctx context.Context, state *clientState, token string) bool {
	if err := config.ValidateProxySettings(state.HTTPProxy, state.ProxySession, state.ProxySessionHeader, state.ConnectTimeout, state.UseQUIC, state.P2PEnabled); err != nil {
		clientLog.Error("Invalid proxy configuration: %v", err)
		return false
	}
	const maxRetries = 3
	const backoffFactor = 500 * time.Millisecond

	connectionType := "tls"
	if state.UseQUIC {
		connectionType = "quic"
	} else if !state.UseTLS {
		connectionType = "non-tls"
	}

	log.Printf("Registering client at %s with connection type %s, %d tunnel(s)", state.ServerAddr, connectionType, len(state.Tunnels))

	fullURL := "https://" + serverAuthority(state.ServerAddr, clientRp) + "/register_client"

	// Build reverse configs for JSON
	var reverseConfigs []ReverseConfigJSON
	for _, r := range state.ReverseTunnels {
		reverseConfigs = append(reverseConfigs, ReverseConfigJSON{
			Remote: r.Remote,
			Local:  r.Local,
		})
	}

	// Build registration request with tunnel info
	regReq := registrationRequest{
		ConnectionType: connectionType,
		ClientID:       clientID,
		Tunnels:        state.Tunnels,
		Reverse:        reverseConfigs,
		Headers:        state.Headers,
		Compress:       state.Compress,
		P2PInfo:        state.P2PInfo,
	}

	tr := state.routeTransport()
	defer tr.CloseIdleConnections()
	client := &http.Client{Timeout: state.connectionBudget(), Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	var res registerResponse

	for attempt := 1; attempt <= maxRetries; attempt++ {
		// Encode request body
		reqBody, err := json.Marshal(regReq)
		if err != nil {
			log.Printf("Attempt %d: Failed to marshal request: %v", attempt, err)
			if !waitRouteRetry(ctx, backoffFactor*time.Duration(attempt)) {
				return false
			}
			continue
		}

		clientLog.Debug("Registering %d tunnels and %d reverse mappings", len(regReq.Tunnels), len(regReq.Reverse))

		req, err := http.NewRequestWithContext(ctx, "POST", fullURL, strings.NewReader(string(reqBody)))
		if err != nil {
			log.Printf("Attempt %d: Failed to create request: %v", attempt, err)
			if !waitRouteRetry(ctx, backoffFactor*time.Duration(attempt)) {
				return false
			}
			continue
		}

		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("X-API-Token", token)
		}

		response, err := client.Do(req)
		if err != nil {
			clientLog.Debug("Attempt %d: Registration failed: %v", attempt, err)
			if !waitRouteRetry(ctx, backoffFactor*time.Duration(attempt)) {
				return false
			}
			continue
		}
		if response.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
			response.Body.Close()
			// A rejected registration (bad token, forbidden -R port, bad
			// request) will not succeed on retry; say why and stop.
			switch response.StatusCode {
			case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
				clientLog.Error("Registration rejected (%d): %s", response.StatusCode, state.redactRouteSecrets(strings.TrimSpace(string(body))))
				os.Exit(1)
			}
			if response.StatusCode == http.StatusConflict {
				clientLog.Warn("Registration conflict: %s (retrying)", state.redactRouteSecrets(strings.TrimSpace(string(body))))
			}
			clientLog.Debug("Attempt %d: Server error %d: %s", attempt, response.StatusCode, state.redactRouteSecrets(string(body)))
			if !waitRouteRetry(ctx, backoffFactor*time.Duration(attempt)) {
				return false
			}
			continue
		}

		err = json.NewDecoder(response.Body).Decode(&res)
		response.Body.Close()
		if err != nil {
			clientLog.Debug("Attempt %d: Failed to decode response: %v", attempt, err)
			if !waitRouteRetry(ctx, backoffFactor*time.Duration(attempt)) {
				return false
			}
			continue
		}

		clientID = res.UDID
		state.ClientID = res.UDID
		adoptAssignedP2PIdentity(state, res.UDID)
		state.SessionToken = res.SessionToken
		state.Multiplexed = res.Multiplexed
		state.SupportedTransports = res.Transports
		state.ClientPort = res.ClientPortTLS

		if state.UseTLS {
			state.ProxyURL = res.ProxyURLTLS
		} else {
			state.ProxyURL = res.ProxyURLNoTLS
		}

		if state.ClientPort == "" {
			log.Printf("Attempt %d: Received empty client port", attempt)
			if !waitRouteRetry(ctx, backoffFactor*time.Duration(attempt)) {
				return false
			}
			continue
		}

		// Update tunnel map with server-assigned names
		if len(res.Tunnels) > 0 {
			clientLog.Info("Registered tunnels:")
			for _, t := range res.Tunnels {
				state.TunnelMap[t.Name] = t.Target
				clientLog.Tunnel(t.Name, t.URL)
			}
		} else if state.ProxyURL != "" {
			// Legacy single tunnel
			clientLog.Connected(fmt.Sprintf("Proxy URL: %s", state.ProxyURL))
		}
		return true
	}

	log.Printf("Registration failed after %d attempts", maxRetries)
	return false
}

func runWithReconnect(ctx context.Context, state *clientState, token string, wg *sync.WaitGroup) {
	defer wg.Done()

	attempt := 0
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if reconnectConfig.MaxRetries > 0 && attempt >= reconnectConfig.MaxRetries {
			clientLog.Error("Max reconnect attempts (%d) reached. Exiting.", reconnectConfig.MaxRetries)
			return
		}

		if !registerClient(ctx, state, token) {
			attempt++
			if reconnectConfig.Enabled {
				wait := reconnectConfig.Backoff(attempt)
				clientLog.Reconnecting(attempt, wait)
				select {
				case <-ctx.Done():
					return
				case <-time.After(wait):
					continue
				}
			} else {
				clientLog.Error("Registration failed and reconnect is disabled. Exiting.")
				os.Exit(1)
			}
		}

		attempt = 0
		isConnected.Store(true)
		reconnectCount.Add(1)

		if reconnectCount.Load() > 1 {
			clientLog.Connected(fmt.Sprintf("Reconnected successfully (reconnect #%d)", reconnectCount.Load()-1))
		}

		sessionCtx, sessionCancel := context.WithCancel(ctx)
		// Buffered: the session goroutines signal with a non-blocking send,
		// which was lost if it fired before this loop reached its receive
		// (e.g. an immediate QUIC failure), leaving the client stuck.
		disconnected := make(chan struct{}, 1)

		if state.Transport == "auto" {
			go connectAutoWithDisconnect(sessionCtx, state, disconnected)
		} else if state.UseQUIC {
			go func() {
				connectQUICWithDisconnect(sessionCtx, state, disconnected)
			}()
		} else {
			go func() {
				connectMuxWithDisconnect(sessionCtx, state, disconnected)
			}()
		}

		if !state.Multiplexed {
			go connectAndForwardSession(sessionCtx, state)
		}

		// Connect P2P signaling if P2P is enabled
		if state.P2PEnabled {
			go connectP2PSignaling(sessionCtx, state)
		}

		select {
		case <-ctx.Done():
			sessionCancel()
			closeP2PState(state)
			return
		case <-disconnected:
			isConnected.Store(false)
			sessionCancel()

			clientStateMutex.Lock()
			if state.MuxSession != nil {
				state.MuxSession.Close()
				state.MuxSession = nil
			}
			if state.QUICConn != nil {
				state.QUICConn.Close()
				state.QUICConn = nil
			}
			closeP2PState(state)
			clientStateMutex.Unlock()

			if reconnectConfig.Enabled {
				attempt++
				wait := reconnectConfig.Backoff(attempt)
				clientLog.Disconnected("Connection lost")
				clientLog.Reconnecting(attempt, wait)
				select {
				case <-ctx.Done():
					return
				case <-time.After(wait):
					continue
				}
			} else {
				clientLog.Error("Connection lost and reconnect is disabled. Exiting.")
				return
			}
		}
	}
}

// connectQUICWithDisconnect establishes QUIC connection to server
func connectQUICWithDisconnect(ctx context.Context, state *clientState, disconnected chan<- struct{}) {
	defer func() {
		select {
		case disconnected <- struct{}{}:
		default:
		}
	}()

	if state.HTTPProxy != "" {
		clientLog.Error("QUIC cannot bypass a configured upstream proxy")
		return
	}
	port := state.ServerPort
	if port == 0 {
		port = 443
	}
	addr := net.JoinHostPort(state.ServerAddr, fmt.Sprint(port))

	tlsConfig := state.TLSConfig.Clone()
	tlsConfig.NextProtos = []string{"xrok-quic"}

	quicTransport := transport.NewQUICTransport(tlsConfig)
	handshake, finishHandshake := context.WithTimeout(ctx, state.connectionBudget())
	conn, err := quicTransport.Dial(handshake, addr)
	finishHandshake()
	if err != nil {
		// UDP is often filtered (cloud firewalls, corporate networks). Fall
		// back to the TCP/TLS mux for the rest of this process; the reconnect
		// loop re-registers with connection type "tls". The write happens
		// before the deferred disconnect signal, which orders it for the loop.
		if state.Transport == "quic" {
			clientLog.Warn("QUIC unavailable: %s", state.redactRouteSecrets(err.Error()))
			return
		}
		clientLog.Warn("QUIC unavailable (%v); falling back to TCP/TLS", err)
		state.UseQUIC = false
		return
	}

	quicConn := conn.(*transport.QUICConn)

	// Send registration message on first stream
	regStream, err := quicConn.OpenStream()
	if err != nil {
		clientLog.Error("Failed to open QUIC registration stream: %v", err)
		conn.Close()
		return
	}

	// Send client ID for association
	regMsg := []byte(fmt.Sprintf("XROK:%s:%s", clientID, state.SessionToken))
	if _, err := regStream.Write(regMsg); err != nil {
		clientLog.Error("Failed to send QUIC registration: %v", err)
		conn.Close()
		return
	}

	clientStateMutex.Lock()
	state.QUICConn = quicConn
	clientStateMutex.Unlock()

	clientLog.Info("QUIC connection established")

	// Accept streams from server
	for {
		select {
		case <-ctx.Done():
			conn.Close()
			return
		default:
			stream, err := quicConn.AcceptStream(ctx)
			if err != nil {
				clientLog.Debug("QUIC accept error: %v", err)
				// Send CONNECTION_CLOSE so the server frees this registration
				// now instead of after its idle timeout (the ctx.Done case
				// above is rarely reached: AcceptStream returns first).
				conn.Close()
				return
			}
			go handleQUICStream(ctx, stream, state)
		}
	}
}

// handleQUICStream handles incoming QUIC stream similar to mux stream
func handleQUICStream(ctx context.Context, stream *quic.Stream, state *clientState) {
	defer (*stream).Close()

	// Read first byte for tunnel routing (same protocol as yamux)
	lenBuf := make([]byte, 1)
	if _, err := io.ReadFull(stream, lenBuf); err != nil {
		clientLog.Debug("Failed to read QUIC stream header: %v", err)
		return
	}

	// Check for reverse forwarding marker
	if lenBuf[0] == 0xFD {
		handleQUICReverseForward(ctx, stream, state)
		return
	}

	// Normal tunnel handling
	clientStateMutex.Lock()
	target := state.LocalPort
	tunnelMap := state.TunnelMap
	clientStateMutex.Unlock()

	nameLen := int(lenBuf[0])

	if len(tunnelMap) > 0 && nameLen > 0 && nameLen < 256 {
		nameBuf := make([]byte, nameLen)
		if _, err := io.ReadFull(stream, nameBuf); err != nil {
			clientLog.Debug("Failed to read tunnel name: %v", err)
			return
		}

		tunnelName := string(nameBuf)
		if t, ok := tunnelMap[tunnelName]; ok {
			target = t
		} else {
			clientLog.Debug("Unknown tunnel: %s", tunnelName)
			return
		}
	}

	if target == "" {
		return
	}

	localConn, err := net.Dial("tcp", target)
	if err != nil {
		clientLog.Debug("Failed to connect to local app %s: %v", target, err)
		return
	}
	defer localConn.Close()

	done := make(chan struct{}, 2)

	go func() {
		io.Copy(localConn, stream)
		done <- struct{}{}
	}()

	go func() {
		io.Copy(stream, localConn)
		done <- struct{}{}
	}()

	select {
	case <-done:
	case <-ctx.Done():
	}
}

func handleQUICReverseForward(ctx context.Context, stream *quic.Stream, state *clientState) {
	// Read local target length
	targetLenBuf := make([]byte, 1)
	if _, err := io.ReadFull(stream, targetLenBuf); err != nil {
		return
	}

	targetLen := int(targetLenBuf[0])
	targetBuf := make([]byte, targetLen)
	if _, err := io.ReadFull(stream, targetBuf); err != nil {
		return
	}

	localTarget := string(targetBuf)
	localConn, err := net.DialTimeout("tcp", localTarget, 30*time.Second)
	if err != nil {
		return
	}
	defer localConn.Close()

	done := make(chan struct{}, 2)

	go func() {
		io.Copy(localConn, stream)
		done <- struct{}{}
	}()

	go func() {
		io.Copy(stream, localConn)
		done <- struct{}{}
	}()

	select {
	case <-done:
	case <-ctx.Done():
	}
}

func connectMuxWithDisconnect(ctx context.Context, state *clientState, disconnected chan<- struct{}) {
	if state.Transport == "http2" || state.Transport == "wss" {
		defer func() {
			select {
			case disconnected <- struct{}{}:
			default:
			}
		}()
		carrier, err := state.dialStandardCarrier(ctx, state.Transport)
		if err != nil {
			logCarrierError(state, err)
			return
		}
		runClientMuxCarrier(ctx, state, carrier)
		return
	}
	defer func() {
		select {
		case disconnected <- struct{}{}:
		default:
		}
	}()

	scheme := "wss"
	u := url.URL{
		Scheme:   scheme,
		Host:     serverAuthority(state.ServerAddr, clientRp),
		Path:     "/mux",
		RawQuery: url.Values{"clientID": {clientID}}.Encode(),
	}

	headers := http.Header{}
	headers.Set("X-Xrok-Session", state.SessionToken)
	conn, _, err := state.dialWebSocket(ctx, u.String(), headers)
	if err != nil {
		log.Printf("Failed to establish mux connection: %v", err)
		return
	}

	config := yamux.DefaultConfig()
	config.AcceptBacklog = 256
	config.EnableKeepAlive = true
	config.KeepAliveInterval = 30 * time.Second

	session, err := yamux.Client(conn.UnderlyingConn(), config)
	if err != nil {
		log.Printf("Failed to create yamux client session: %v", err)
		conn.Close()
		return
	}

	clientStateMutex.Lock()
	state.MuxSession = session
	clientStateMutex.Unlock()

	log.Printf("Yamux multiplexed connection established")

	for {
		select {
		case <-ctx.Done():
			session.Close()
			return
		default:
			stream, err := session.Accept()
			if err != nil {
				if err != io.EOF {
					log.Printf("Yamux session error: %v", err)
				}
				return
			}
			go handleMuxStream(ctx, stream, state)
		}
	}
}

// handleServerInitiatedStream handles streams opened by the server (e.g., reverse forwarding)
func handleServerInitiatedStream(ctx context.Context, stream net.Conn, state *clientState) {
	defer stream.Close()

	// Read first byte to determine stream type
	header := make([]byte, 2)
	if _, err := io.ReadFull(stream, header); err != nil {
		log.Printf("Failed to read server stream header: %v", err)
		return
	}

	// Check for reverse forwarding marker
	if header[0] == 0xFD {
		// Reverse forwarding: header[1] is local target length
		targetLen := int(header[1])
		if targetLen == 0 || targetLen > 255 {
			log.Printf("Invalid reverse forwarding target length: %d", targetLen)
			return
		}

		targetBuf := make([]byte, targetLen)
		if _, err := io.ReadFull(stream, targetBuf); err != nil {
			log.Printf("Failed to read reverse target: %v", err)
			return
		}

		localTarget := string(targetBuf)
		if clientDebug {
			log.Printf("Reverse forwarding connection to: %s", localTarget)
		}

		// Connect to local target
		localConn, err := net.DialTimeout("tcp", localTarget, 30*time.Second)
		if err != nil {
			log.Printf("Failed to connect to local target %s: %v", localTarget, err)
			return
		}
		defer localConn.Close()

		// Bridge the connections
		done := make(chan struct{}, 2)

		go func() {
			io.Copy(localConn, stream)
			done <- struct{}{}
		}()

		go func() {
			io.Copy(stream, localConn)
			done <- struct{}{}
		}()

		select {
		case <-done:
		case <-ctx.Done():
		}
		return
	}

	// Unknown stream type
	log.Printf("Unknown server stream type: 0x%02X", header[0])
}

func handleMuxStream(ctx context.Context, stream net.Conn, state *clientState) {
	defer stream.Close()

	// Read first byte to check for special markers
	lenBuf := make([]byte, 1)
	if _, err := io.ReadFull(stream, lenBuf); err != nil {
		log.Printf("Failed to read stream header: %v", err)
		return
	}

	// Check for reverse forwarding marker
	if lenBuf[0] == 0xFD {
		// Read local target length
		targetLenBuf := make([]byte, 1)
		if _, err := io.ReadFull(stream, targetLenBuf); err != nil {
			log.Printf("Failed to read reverse target length: %v", err)
			return
		}

		targetLen := int(targetLenBuf[0])
		if targetLen == 0 || targetLen > 255 {
			log.Printf("Invalid reverse target length: %d", targetLen)
			return
		}

		targetBuf := make([]byte, targetLen)
		if _, err := io.ReadFull(stream, targetBuf); err != nil {
			log.Printf("Failed to read reverse target: %v", err)
			return
		}

		localTarget := string(targetBuf)
		if clientDebug {
			log.Printf("Reverse forwarding to: %s", localTarget)
		}

		// Connect to local target
		localConn, err := net.DialTimeout("tcp", localTarget, 30*time.Second)
		if err != nil {
			log.Printf("Failed to connect to local target %s: %v", localTarget, err)
			return
		}
		defer localConn.Close()

		// Bridge the connections
		done := make(chan struct{}, 2)

		go func() {
			io.Copy(localConn, stream)
			done <- struct{}{}
		}()

		go func() {
			io.Copy(stream, localConn)
			done <- struct{}{}
		}()

		select {
		case <-done:
		case <-ctx.Done():
		}
		return
	}

	// Normal tunnel handling
	target := state.LocalPort
	nameLen := int(lenBuf[0])

	if len(state.TunnelMap) > 0 && nameLen > 0 && nameLen < 256 {
		nameBuf := make([]byte, nameLen)
		if _, err := io.ReadFull(stream, nameBuf); err != nil {
			log.Printf("Failed to read tunnel name: %v", err)
			return
		}

		tunnelName := string(nameBuf)
		if t, ok := state.TunnelMap[tunnelName]; ok {
			target = t
			if clientDebug {
				log.Printf("Routing to tunnel %s -> %s", tunnelName, target)
			}
		} else {
			log.Printf("Unknown tunnel: %s", tunnelName)
			return
		}
	}

	if target == "" {
		log.Printf("No target configured for connection")
		return
	}

	localConn, err := net.Dial("tcp", target)
	if err != nil {
		log.Printf("Failed to connect to local app %s via mux: %v", target, err)
		return
	}
	defer localConn.Close()

	done := make(chan struct{}, 2)

	go func() {
		defer func() { done <- struct{}{} }()
		io.Copy(localConn, stream)
	}()

	go func() {
		defer func() { done <- struct{}{} }()
		io.Copy(stream, localConn)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		stream.Close()
		localConn.Close()
	}
}

func connectAndForwardSession(ctx context.Context, state *clientState) {
	const (
		connReadLimit = 1024 * 1024
	)

	scheme := "wss"
	u := url.URL{Scheme: scheme, Host: serverAuthority(state.ServerAddr, clientRp), Path: "/client_ws"}

	select {
	case <-ctx.Done():
		return
	default:
	}

	headers := http.Header{}
	if state.APIToken != "" {
		headers.Set("X-API-Token", state.APIToken)
	}
	conn, resp, err := state.dialWebSocket(ctx, u.String(), headers)
	if err != nil {
		if resp != nil {
			log.Printf("WebSocket connection failed with status: %d %s", resp.StatusCode, resp.Status)
		} else {
			log.Printf("WebSocket connection failed: %v", err)
		}
		return
	}

	conn.SetReadLimit(connReadLimit)
	if clientDebug {
		log.Printf("Control connection established")
	}

	handleWebSocketConnection(ctx, conn, state)
}

func handleWebSocketConnection(ctx context.Context, conn *websocket.Conn, state *clientState) {
	defer conn.Close()

	const writeWait = 10 * time.Second
	conn.SetPingHandler(func(appData string) error {
		return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(writeWait))
	})

	go func() {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				log.Printf("Error reading from WebSocket: %v", err)
				return
			}

			var controlMsg map[string]string
			if err := json.Unmarshal(msg, &controlMsg); err != nil {
				log.Printf("Invalid control message: %v", err)
				continue
			}

			if controlMsg["type"] == "new_connection" {
				connectionID := controlMsg["connectionID"]
				useTLSStr := controlMsg["useTLS"]
				useTLSConn := useTLSStr == "true"

				go establishNewWebSocketConnection(ctx, state, connectionID, useTLSConn)
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		default:
			if err := conn.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
				log.Printf("Lost control connection, attempting to reconnect...")
				return
			}
			time.Sleep(5 * time.Second)
		}
	}
}

func establishNewWebSocketConnection(ctx context.Context, state *clientState, connectionID string, useTLS bool) {
	scheme := "wss"

	wsURL := url.URL{
		Scheme: scheme,
		Host:   net.JoinHostPort(state.ServerAddr, state.ClientPort),
		Path:   "/client_ws",
		RawQuery: url.Values{
			"connectionID": {connectionID},
			"clientID":     {clientID},
		}.Encode(),
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
			headers := http.Header{}
			if state.APIToken != "" {
				headers.Set("X-API-Token", state.APIToken)
			}
			wsConn, _, err := state.dialWebSocket(ctx, wsURL.String(), headers)
			if err != nil {
				log.Printf("Failed to establish WebSocket connection for connectionID %s: %v", connectionID, err)
				if !waitRouteRetry(ctx, 2*time.Second) {
					return
				}
				continue
			}

			localConn, err := net.Dial("tcp", state.LocalPort)
			if err != nil {
				log.Printf("Failed to connect to local application for connectionID %s: %v", connectionID, err)
				wsConn.Close()
				return
			}

			activeConnections.Lock()
			activeConnections.connections[connectionID] = &activeConnection{
				WebSocketConn: wsConn,
				LocalConn:     localConn,
			}
			activeConnections.Unlock()

			go handleDataForwarding(ctx, wsConn, localConn, connectionID)
			return
		}
	}
}

func handleDataForwarding(ctx context.Context, wsConn *websocket.Conn, localConn net.Conn, connectionID string) {
	defer func() {
		wsConn.Close()
		localConn.Close()
		activeConnections.Lock()
		delete(activeConnections.connections, connectionID)
		activeConnections.Unlock()
		log.Printf("Connection with ID %s closed", connectionID)
	}()

	done := make(chan struct{}, 2)

	go func() {
		defer func() { done <- struct{}{} }()
		if _, err := io.Copy(localConn, wsConn.UnderlyingConn()); err != nil && !isClosedNetworkError(err) {
			log.Printf("Error during WebSocket to local copy for connectionID %s: %v", connectionID, err)
		}
	}()

	go func() {
		defer func() { done <- struct{}{} }()
		if _, err := io.Copy(wsConn.UnderlyingConn(), localConn); err != nil && !isClosedNetworkError(err) {
			log.Printf("Error during local to WebSocket copy for connectionID %s: %v", connectionID, err)
		}
	}()

	select {
	case <-done:
		log.Printf("Data forwarding completed for connectionID %s", connectionID)
	case <-ctx.Done():
		log.Printf("Context canceled for connectionID %s", connectionID)
	}
}

func isClosedNetworkError(err error) bool {
	if err == nil {
		return false
	}
	if err == io.EOF {
		return true
	}
	netErr, ok := err.(*net.OpError)
	return ok && !netErr.Temporary()
}

// runStdioMode runs the client in stdio mode for SSH ProxyCommand
func runStdioMode(state *clientState, token string) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// Register with server first
	if !registerClient(ctx, state, token) {
		clientLog.Error("Failed to register with server")
		os.Exit(1)
	}

	// Establish mux connection
	scheme := "wss"
	if !state.UseTLS {
		scheme = "ws"
	}

	u := url.URL{
		Scheme:   scheme,
		Host:     serverAuthority(state.ServerAddr, clientRp),
		Path:     "/mux",
		RawQuery: url.Values{"clientID": {clientID}}.Encode(),
	}

	headers := http.Header{}
	headers.Set("X-Xrok-Session", state.SessionToken)
	conn, _, err := state.dialWebSocket(ctx, u.String(), headers)
	if err != nil {
		clientLog.Error("Failed to establish mux connection: %v", err)
		os.Exit(1)
	}

	config := yamux.DefaultConfig()
	config.AcceptBacklog = 256
	config.EnableKeepAlive = true
	config.KeepAliveInterval = 30 * time.Second

	session, err := yamux.Client(conn.UnderlyingConn(), config)
	if err != nil {
		clientLog.Error("Failed to create yamux session: %v", err)
		os.Exit(1)
	}

	// Open stream for stdio target
	stream, err := session.Open()
	if err != nil {
		clientLog.Error("Failed to open stream: %v", err)
		os.Exit(1)
	}

	// Send target header (same format as SOCKS5 dynamic connection)
	target := state.StdioTarget
	header := make([]byte, 2+len(target))
	header[0] = 0xFF // Dynamic connection marker
	header[1] = byte(len(target))
	copy(header[2:], target)

	if _, err := stream.Write(header); err != nil {
		clientLog.Error("Failed to send target header: %v", err)
		os.Exit(1)
	}

	// Bridge stdin/stdout to stream
	done := make(chan struct{}, 2)

	go func() {
		io.Copy(stream, os.Stdin)
		done <- struct{}{}
	}()

	go func() {
		io.Copy(os.Stdout, stream)
		done <- struct{}{}
	}()

	<-done
	stream.Close()
	session.Close()
}

// runUDPTunnel handles UDP tunneling through the mux session
func runUDPTunnel(ctx context.Context, localAddr, remoteAddr string) {
	// Listen on local UDP address
	udpAddr, err := net.ResolveUDPAddr("udp", localAddr)
	if err != nil {
		log.Printf("Failed to resolve UDP address %s: %v", localAddr, err)
		return
	}

	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		log.Printf("Failed to listen on UDP %s: %v", localAddr, err)
		return
	}
	defer conn.Close()

	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	log.Printf("UDP tunnel listening on %s -> %s", localAddr, remoteAddr)

	// Track client addresses for sending responses back
	clients := make(map[string]*net.UDPAddr)
	clientsMutex := sync.RWMutex{}

	buf := make([]byte, 65535) // Max UDP packet size

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, clientAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			log.Printf("UDP read error: %v", err)
			continue
		}

		// Get mux session
		state := currentClientState()
		var session *yamux.Session
		if state != nil {
			session = state.MuxSession
		}

		if session == nil {
			log.Printf("UDP: No tunnel connection available")
			continue
		}

		// Open mux stream for this UDP exchange
		stream, err := session.Open()
		if err != nil {
			log.Printf("UDP: Failed to open mux stream: %v", err)
			continue
		}

		// Track client for response routing
		clientKey := clientAddr.String()
		clientsMutex.Lock()
		clients[clientKey] = clientAddr
		clientsMutex.Unlock()

		// Copy the datagram before handing it to a goroutine. The receive buffer
		// is reused by the next ReadFromUDP; sharing it races and corrupts
		// concurrent packets (especially under bursty UDP traffic).
		packet := append([]byte(nil), buf[:n]...)
		go handleUDPPacket(ctx, stream, conn, clientAddr, remoteAddr, packet, &clientsMutex)
	}
}

// handleUDPPacket sends a UDP packet through the mux stream and handles response
func handleUDPPacket(ctx context.Context, stream net.Conn, localConn *net.UDPConn, clientAddr *net.UDPAddr, remoteAddr string, data []byte, mutex *sync.RWMutex) {
	defer stream.Close()

	// Protocol:
	// 1. Send header: 0xFE (UDP marker) + remoteAddr length (1 byte) + remoteAddr + packet length (2 bytes big-endian) + packet data
	// 2. Receive: packet length (2 bytes big-endian) + response data

	remoteAddrBytes := []byte(remoteAddr)
	if len(remoteAddrBytes) > 255 {
		log.Printf("UDP: Remote address too long")
		return
	}

	// Build header
	header := make([]byte, 1+1+len(remoteAddrBytes)+2+len(data))
	header[0] = 0xFE // UDP marker
	header[1] = byte(len(remoteAddrBytes))
	copy(header[2:2+len(remoteAddrBytes)], remoteAddrBytes)
	// Packet length (big-endian)
	header[2+len(remoteAddrBytes)] = byte(len(data) >> 8)
	header[2+len(remoteAddrBytes)+1] = byte(len(data))
	copy(header[2+len(remoteAddrBytes)+2:], data)

	// Send packet
	stream.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := stream.Write(header); err != nil {
		log.Printf("UDP: Failed to send packet: %v", err)
		return
	}

	// Read response
	respHeader := make([]byte, 2)
	stream.SetReadDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.ReadFull(stream, respHeader); err != nil {
		if err != io.EOF {
			if clientDebug {
				log.Printf("UDP: No response or error: %v", err)
			}
		}
		return
	}

	respLen := int(respHeader[0])<<8 | int(respHeader[1])
	if respLen > 65535 {
		log.Printf("UDP: Response too large: %d", respLen)
		return
	}

	respData := make([]byte, respLen)
	if _, err := io.ReadFull(stream, respData); err != nil {
		log.Printf("UDP: Failed to read response: %v", err)
		return
	}

	// Send response back to client
	if _, err := localConn.WriteToUDP(respData, clientAddr); err != nil {
		log.Printf("UDP: Failed to send response to client: %v", err)
	}
}

// connectP2PSignaling establishes WebSocket connection for P2P signaling
func connectP2PSignaling(ctx context.Context, state *clientState) {
	if !state.P2PEnabled {
		return
	}

	wsURL := fmt.Sprintf("wss://%s/p2p/ws?client_id=%s", serverAuthority(state.ServerAddr, clientRp), url.QueryEscape(clientID))

	headers := http.Header{}
	if state.APIToken != "" {
		headers.Set("X-API-Token", state.APIToken)
	}
	conn, _, err := state.dialWebSocket(ctx, wsURL, headers)
	if err != nil {
		clientLog.Warn("P2P signaling connection failed: %v", err)
		return
	}
	conn.SetReadLimit(p2p.MaxSignalMessageSize)

	state.P2PSignalMutex.Lock()
	state.P2PSignalConn = conn
	state.P2PSignalMutex.Unlock()

	clientLog.Debug("P2P signaling connected")

	// Handle incoming signals
	go handleP2PSignals(ctx, conn, state)
}

// handleP2PSignals processes incoming P2P signaling messages
func handleP2PSignals(ctx context.Context, conn *websocket.Conn, state *clientState) {
	defer func() {
		state.P2PSignalMutex.Lock()
		if state.P2PSignalConn == conn {
			state.P2PSignalConn = nil
		}
		state.P2PSignalMutex.Unlock()
		conn.Close()
	}()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		_, message, err := conn.ReadMessage()
		if err != nil {
			clientLog.Debug("P2P signaling read error: %v", err)
			return
		}

		signal, err := p2p.DecodeSignal(message)
		if err != nil {
			clientLog.Debug("P2P signal decode error: %v", err)
			continue
		}

		handleP2PSignal(ctx, state, signal)
	}
}

// handleP2PSignal processes a single P2P signal
func handleP2PSignal(ctx context.Context, state *clientState, signal *p2p.SignalMessage) {
	localID := clientRuntimeID(state)
	negotiations := clientNegotiationStore(state)
	switch signal.Type {
	case p2p.SignalTypePeerInfo:
		if signal.PeerInfo == nil {
			return
		}
		peer := *signal.PeerInfo
		peerID := peer.PeerID
		if peerID == "" {
			peerID = signal.FromPeer
			peer.PeerID = peerID
		}
		state.P2PStateMutex.Lock()
		state.P2PPeers[peerID] = &peer
		state.P2PStateMutex.Unlock()
		clientLog.Info("P2P: Received peer info from %s at %s", peerID, peer.PublicAddr)
		if p2p.ShouldInitiateICE(localID, peerID) {
			go startICEOffer(ctx, state, &peer)
		}

	case p2p.SignalTypeICEOffer:
		if signal.ToPeer != localID || p2p.ShouldInitiateICE(localID, signal.FromPeer) {
			clientLog.Debug("P2P: rejected ICE offer glare/session mismatch from %s", signal.FromPeer)
			return
		}
		peer := p2pPeer(state, signal.FromPeer)
		if peer == nil {
			return
		}
		if existing, ok := negotiations.Get(signal.FromPeer, signal.SessionID); ok {
			// Idempotently answer an offer replay from the original candidate
			// snapshot. Never bind a second socket or restart connectivity checks.
			sendP2PSignal(state, p2p.NewICEAnswerSignal(localID, signal.FromPeer, signal.SessionID, existing.LocalCandidates))
			return
		}
		local, err := prepareClientICE(ctx, state, signal.SessionID)
		if err != nil {
			sendP2PSignal(state, p2p.NewFallbackSignal(localID, signal.FromPeer, signal.SessionID, err.Error()))
			return
		}
		_, err = negotiations.Start(signal.FromPeer, p2p.NegotiationControlled, signal.SessionID, local, signal.Candidates)
		if err != nil {
			state.P2PManager.CancelPreparedICE(signal.SessionID)
			return
		}
		sendP2PSignal(state, p2p.NewICEAnswerSignal(localID, signal.FromPeer, signal.SessionID, local))
		if checking, won := negotiations.BeginCheck(signal.FromPeer, signal.SessionID); won {
			go establishNegotiatedICE(ctx, state, peer, checking)
		}

	case p2p.SignalTypeICEAnswer:
		if signal.ToPeer != localID || !p2p.ShouldInitiateICE(localID, signal.FromPeer) {
			return
		}
		peer := p2pPeer(state, signal.FromPeer)
		if peer == nil {
			return
		}
		negotiation, err := negotiations.AddRemote(signal.FromPeer, signal.SessionID, signal.Candidates)
		if err != nil || negotiation.Role != p2p.NegotiationControlling {
			return
		}
		if checking, won := negotiations.BeginCheck(signal.FromPeer, signal.SessionID); won {
			go establishNegotiatedICE(ctx, state, peer, checking)
		}

	case p2p.SignalTypeICECandidate:
		if signal.ToPeer == localID {
			_, _ = negotiations.AddRemote(signal.FromPeer, signal.SessionID, signal.Candidates)
		}

	case p2p.SignalTypePunch:
		// Peer is trying to punch - we should already be punching too
		clientLog.Debug("P2P: Received punch signal from %s", signal.FromPeer)

	case p2p.SignalTypeReady:
		// P2P connection is ready on the other side
		clientLog.Info("P2P: Peer %s reports connection ready", signal.FromPeer)

	case p2p.SignalTypeFallback:
		// P2P failed, will use relay
		clientLog.Debug("P2P: Fallback to relay for %s: %s", signal.FromPeer, signal.Error)

		removeP2PSessionIfMatch(state, signal.FromPeer, signal.SessionID)
		negotiations.Fail(signal.FromPeer, signal.SessionID)
		state.P2PManager.CancelPreparedICE(signal.SessionID)
	}
}

func startICEOffer(ctx context.Context, state *clientState, peer *p2p.PeerInfo) {
	negotiations := clientNegotiationStore(state)
	sessionID := p2p.NewICESessionID()
	if sessionID == "" {
		return
	}
	local, err := prepareClientICE(ctx, state, sessionID)
	if err != nil {
		return
	}
	if _, err := negotiations.Start(peer.PeerID, p2p.NegotiationControlling, sessionID, local, nil); err != nil {
		state.P2PManager.CancelPreparedICE(sessionID)
		return
	}
	sendP2PSignal(state, p2p.NewICEOfferSignal(clientRuntimeID(state), peer.PeerID, sessionID, local))
}

func p2pPeer(state *clientState, peerID string) *p2p.PeerInfo {
	state.P2PStateMutex.RLock()
	defer state.P2PStateMutex.RUnlock()
	peer := state.P2PPeers[peerID]
	if peer == nil {
		return nil
	}
	copy := *peer
	return &copy
}

func prepareClientICE(ctx context.Context, state *clientState, sessionID string) ([]p2p.Candidate, error) {
	allocator, err := newClientTURNSplitAllocator(state.P2PTURN)
	if err != nil {
		return nil, err
	}
	return prepareClientICEWithAllocator(ctx, state, sessionID, allocator)
}

func prepareClientICEWithAllocator(ctx context.Context, state *clientState, sessionID string, allocator p2p.RelayCandidateAllocator) ([]p2p.Candidate, error) {
	candidates, err := state.P2PManager.PrepareICEWithRelay(ctx, sessionID, state.P2PStunServer, allocator)
	if err == nil || allocator == nil || ctx.Err() != nil {
		return candidates, err
	}
	// TURN is an availability fallback, not a prerequisite for direct ICE. A
	// failed pre-allocation must not discard an otherwise usable host/srflx path.
	if clientLog != nil {
		clientLog.Warn("P2P: TURN candidate unavailable, continuing direct-only: %v", err)
	}
	return state.P2PManager.PrepareICE(ctx, sessionID, state.P2PStunServer)
}

func establishNegotiatedICE(ctx context.Context, state *clientState, peer *p2p.PeerInfo, negotiation *p2p.Negotiation) {
	negotiations := clientNegotiationStore(state)
	iceConfig := p2p.ICEConfig{STUNServer: state.P2PStunServer}
	agent := p2p.NewICEAgent(iceConfig)
	session, err := state.P2PManager.EstablishPreparedICE(ctx, agent, peer, negotiation.SessionID, negotiation.LocalCandidates, negotiation.RemoteCandidates)
	if err != nil {
		clientLog.Debug("P2P: ICE to %s failed: %v", peer.PeerID, err)
		sendP2PSignal(state, p2p.NewFallbackSignal(clientRuntimeID(state), peer.PeerID, negotiation.SessionID, err.Error()))
		negotiations.Fail(peer.PeerID, negotiation.SessionID)
		state.P2PManager.CancelPreparedICE(negotiation.SessionID)
		return
	}
	localTarget := ""
	if len(state.Tunnels) > 0 {
		localTarget = state.Tunnels[0].Target
	}
	tunnel := p2p.NewP2PTunnelWithRole(session.Conn(), peer.PeerID, localTarget, p2p.ShouldInitiateICE(clientRuntimeID(state), peer.PeerID))
	if !negotiations.Complete(peer.PeerID, negotiation.SessionID) {
		_ = tunnel.Close()
		return
	}
	installP2PSession(state, peer.PeerID, negotiation.SessionID, session.Conn(), tunnel)
	go monitorP2PSession(state, negotiations, peer.PeerID, negotiation.SessionID, session.Done())
	sendP2PSignal(state, p2p.NewReadySignal(clientRuntimeID(state), peer.PeerID, negotiation.SessionID))
}

func clientRuntimeID(state *clientState) string {
	if state != nil && state.ClientID != "" {
		return state.ClientID
	}
	return clientID
}

func installP2PSession(state *clientState, peerID, sessionID string, conn net.Conn, tunnel *p2p.P2PTunnel) {
	state.P2PStateMutex.Lock()
	if state.P2PActiveSessions == nil {
		state.P2PActiveSessions = make(map[string]string)
	}
	state.P2PActiveSessions[peerID] = sessionID
	state.P2PConnMutex.Lock()
	oldConn := state.P2PConnections[peerID]
	state.P2PConnections[peerID] = conn
	state.P2PConnMutex.Unlock()
	state.P2PTunnelsMutex.Lock()
	oldTunnel := state.P2PTunnels[peerID]
	state.P2PTunnels[peerID] = tunnel
	state.P2PTunnelsMutex.Unlock()
	state.P2PStateMutex.Unlock()
	if oldTunnel != nil && oldTunnel != tunnel {
		_ = oldTunnel.Close()
	}
	if oldConn != nil && oldConn != conn {
		_ = oldConn.Close()
	}
}

func monitorP2PSession(state *clientState, negotiations *p2p.NegotiationStore, peerID, sessionID string, done <-chan struct{}) {
	if state == nil || negotiations == nil || done == nil {
		return
	}
	<-done
	if removeP2PSessionIfMatch(state, peerID, sessionID) {
		negotiations.Delete(peerID, sessionID)
	}
}

func clientNegotiationStore(state *clientState) *p2p.NegotiationStore {
	state.P2PStateMutex.RLock()
	defer state.P2PStateMutex.RUnlock()
	return state.P2PNegotiations
}

func removeP2PSessionIfMatch(state *clientState, peerID, sessionID string) bool {
	state.P2PStateMutex.Lock()
	active := state.P2PActiveSessions[peerID]
	if active == "" || active != sessionID {
		state.P2PStateMutex.Unlock()
		return false
	}
	delete(state.P2PActiveSessions, peerID)
	state.P2PConnMutex.Lock()
	conn := state.P2PConnections[peerID]
	delete(state.P2PConnections, peerID)
	state.P2PConnMutex.Unlock()
	state.P2PTunnelsMutex.Lock()
	tunnel := state.P2PTunnels[peerID]
	delete(state.P2PTunnels, peerID)
	state.P2PTunnelsMutex.Unlock()
	state.P2PStateMutex.Unlock()
	if tunnel != nil {
		_ = tunnel.Close()
	}
	if conn != nil {
		_ = conn.Close()
	}
	return true
}

func closeP2PState(state *clientState) {
	if state == nil {
		return
	}
	state.P2PSignalMutex.Lock()
	signalConn := state.P2PSignalConn
	state.P2PSignalConn = nil
	state.P2PSignalMutex.Unlock()
	if signalConn != nil {
		_ = signalConn.Close()
	}

	state.P2PStateMutex.Lock()
	state.P2PActiveSessions = make(map[string]string)
	state.P2PPeers = make(map[string]*p2p.PeerInfo)
	state.P2PNegotiations = p2p.NewNegotiationStore(128, 30*time.Second)
	state.P2PTunnelsMutex.Lock()
	tunnels := state.P2PTunnels
	state.P2PTunnels = make(map[string]*p2p.P2PTunnel)
	state.P2PTunnelsMutex.Unlock()
	state.P2PConnMutex.Lock()
	connections := state.P2PConnections
	state.P2PConnections = make(map[string]net.Conn)
	state.P2PConnMutex.Unlock()
	state.P2PStateMutex.Unlock()
	for _, tunnel := range tunnels {
		if tunnel != nil {
			_ = tunnel.Close()
		}
	}
	for _, conn := range connections {
		if conn != nil {
			_ = conn.Close()
		}
	}
	if state.P2PManager != nil {
		_ = state.P2PManager.Close()
	}
}

// sendP2PSignal sends a signal via the P2P WebSocket
func sendP2PSignal(state *clientState, signal *p2p.SignalMessage) {
	state.P2PSignalMutex.Lock()
	defer state.P2PSignalMutex.Unlock()

	if state.P2PSignalConn == nil {
		return
	}

	data, err := json.Marshal(signal)
	if err != nil {
		return
	}

	if err := state.P2PSignalConn.WriteMessage(websocket.TextMessage, data); err != nil {
		clientLog.Debug("Failed to send P2P signal: %v", err)
	}
}

// getP2PConnection returns a P2P connection to a peer if available
func getP2PConnection(state *clientState, peerID string) net.Conn {
	state.P2PConnMutex.RLock()
	defer state.P2PConnMutex.RUnlock()
	return state.P2PConnections[peerID]
}

// getP2PTunnel returns a P2P tunnel to a peer if available
func getP2PTunnel(state *clientState, peerID string) *p2p.P2PTunnel {
	state.P2PTunnelsMutex.RLock()
	defer state.P2PTunnelsMutex.RUnlock()
	return state.P2PTunnels[peerID]
}

// hasP2PTunnel checks if we have an active P2P tunnel to a peer
func hasP2PTunnel(state *clientState, peerID string) bool {
	state.P2PTunnelsMutex.RLock()
	defer state.P2PTunnelsMutex.RUnlock()
	_, exists := state.P2PTunnels[peerID]
	return exists
}

// requestP2PConnection requests a P2P connection to a peer via signaling
func requestP2PConnection(state *clientState, peerID string) {
	if !state.P2PEnabled || state.P2PInfo == nil {
		return
	}

	// Send discover signal to find peer
	sendP2PSignal(state, p2p.NewDiscoverSignal(clientRuntimeID(state), peerID))
}

// p2pOwnerLookupClient returns one keep-alive client per state; a transport
// per request would leak its idle connections and goroutines.
func (state *clientState) p2pOwnerLookupClient() *http.Client {
	state.p2pLookupOnce.Do(func() {
		state.p2pLookupClient = &http.Client{Timeout: 10 * time.Second, Transport: state.routeTransport()}
	})
	return state.p2pLookupClient
}

// FetchViaP2P attempts to fetch from a tunnel using P2P if available
// Returns the response, whether P2P was used, and any error
func FetchViaP2P(state *clientState, tunnelURL string, req *http.Request) (*http.Response, bool, error) {
	if !state.P2PEnabled {
		return nil, false, nil // P2P not enabled, caller should use normal path
	}

	// Extract tunnel name from URL
	// URL format: https://<tunnel>.domain.com/path
	u, err := url.Parse(tunnelURL)
	if err != nil {
		return nil, false, nil
	}

	parts := strings.Split(u.Host, ".")
	if len(parts) < 3 {
		return nil, false, nil
	}
	tunnelName := parts[0]

	// Query server for tunnel owner
	lookupURL := "https://" + serverAuthority(state.ServerAddr, state.ServerPort) + "/p2p/tunnel-owner?tunnel=" + url.QueryEscape(tunnelName)
	lookupReq, err := http.NewRequestWithContext(req.Context(), http.MethodGet, lookupURL, nil)
	if err != nil {
		return nil, false, nil
	}
	if state.APIToken != "" {
		lookupReq.Header.Set("X-API-Token", state.APIToken)
	}
	lookupResp, err := state.p2pOwnerLookupClient().Do(lookupReq)
	if err != nil {
		return nil, false, nil
	}
	defer lookupResp.Body.Close()

	if lookupResp.StatusCode != http.StatusOK {
		return nil, false, nil
	}

	var ownerInfo struct {
		ClientID   string        `json:"client_id"`
		P2PEnabled bool          `json:"p2p_enabled"`
		P2PInfo    *p2p.PeerInfo `json:"p2p_info"`
	}
	if err := json.NewDecoder(lookupResp.Body).Decode(&ownerInfo); err != nil {
		return nil, false, nil
	}

	if !ownerInfo.P2PEnabled {
		return nil, false, nil
	}

	// Check if we have P2P tunnel to this client
	tunnel := getP2PTunnel(state, ownerInfo.ClientID)
	if tunnel == nil {
		// Try to establish P2P connection
		requestP2PConnection(state, ownerInfo.ClientID)
		// For now, fall back to relay - P2P will be available for next request
		return nil, false, nil
	}

	// Use P2P tunnel
	resp, err := tunnel.DoHTTPRequest(req)
	if err != nil {
		clientLog.Debug("P2P fetch failed, falling back to relay: %v", err)
		return nil, false, err
	}

	clientLog.Info("P2P: Fetched %s via P2P to %s", tunnelURL, ownerInfo.ClientID)
	return resp, true, nil
}
