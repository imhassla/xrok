package cmd

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/quic-go/quic-go"
	"golang.org/x/time/rate"
	"xrok/cmd/config"
	"xrok/cmd/p2p"
	"xrok/cmd/telemetry"
	"xrok/cmd/transport"
)

// Server state types
type serializableClientInfo struct {
	UDID            string
	PortNoTLS       int
	ClientPortTLS   int
	ClientPortNoTLS int
	lastActivity    atomic.Int64
	ConnectionType  string
	ConnectionID    string
	Registered      bool
}

func (s *serializableClientInfo) GetLastActivity() time.Time {
	return time.Unix(0, s.lastActivity.Load())
}

func (s *serializableClientInfo) SetLastActivity(t time.Time) {
	s.lastActivity.Store(t.UnixNano())
}

type serializableClientInfoForSave struct {
	UDID            string
	PortNoTLS       int
	ClientPortTLS   int
	ClientPortNoTLS int
	LastActivity    time.Time
	ConnectionType  string
	ConnectionID    string
	Registered      bool
}

// ServerTunnelConfig represents a tunnel configuration on the server
type ServerTunnelConfig struct {
	Name               string `json:"name"`
	Target             string `json:"target"`
	BasicAuth          string `json:"basic_auth,omitempty"` // user:password for HTTP Basic Auth
	Capability         string `json:"capability,omitempty"`
	CapabilityAudience string `json:"capability_audience,omitempty"`
}

// ServerTunnelInfo contains server-side tunnel info
type ServerTunnelInfo struct {
	Name   string `json:"name"`
	Target string `json:"target"`
	URL    string `json:"url"`
}

// ServerReverseConfig represents reverse forwarding config from client
type ServerReverseConfig struct {
	Remote string `json:"remote"` // Port to listen on server
	Local  string `json:"local"`  // Local target on client
}

type serverClientInfo struct {
	serializableClientInfo
	conn                *websocket.Conn
	connWriteMutex      sync.Mutex
	muxSession          *yamux.Session
	muxSessionMutex     sync.Mutex
	quicConn            *transport.QUICConn // QUIC connection
	listenerTLS         net.Listener
	listenerNoTLS       net.Listener
	clientListenerTLS   *http.Server
	clientListenerNoTLS *http.Server
	cancelFunc          context.CancelFunc
	ctx                 context.Context // registration lifetime
	attachMu            sync.Mutex      // orders data-connection attach against the unattached reaper
	attached            bool
	reaped              bool
	connMutex           sync.Mutex
	wsConnChans         map[string]chan *websocket.Conn
	tunnels             []ServerTunnelConfig  // Multiple tunnels per client
	tunnelMap           map[string]string     // name -> target
	reverseConfigs      []ServerReverseConfig // Reverse forwarding configs
	reverseListeners    []net.Listener        // Reverse forwarding listeners
	customHeaders       map[string]string     // Custom headers to inject
	compress            bool                  // Enable compression
	p2pInfo             *p2p.PeerInfo         // P2P peer information
	p2pSignalConn       *websocket.Conn       // WebSocket for P2P signaling
	p2pSignalMutex      sync.Mutex            // Mutex for P2P signaling
	p2pConnections      map[string]net.Conn   // Active P2P connections (peerID -> conn)
	p2pConnMutex        sync.RWMutex          // Mutex for P2P connections
	sessionToken        string                // Credential for data-plane connections
	decrementOnce       sync.Once             // Ensure active clients counter only decremented once
}

// validSessionToken reports whether presented matches the client's data-plane
// credential. Clients restored from state carry no credential and must
// re-register before a data-plane session can attach.
func (c *serverClientInfo) validSessionToken(presented string) bool {
	return c.sessionToken != "" && subtle.ConstantTimeCompare([]byte(presented), []byte(c.sessionToken)) == 1
}

// TunnelRoute maps a subdomain to client and tunnel info
type TunnelRoute struct {
	ClientID           string
	TunnelName         string
	Target             string
	BasicAuth          string            // user:password for HTTP Basic Auth
	Headers            map[string]string // Custom headers to inject
	Capability         string            // Optional capability token required by route
	CapabilityAudience string            // JWT audience when capability is an HS256 secret
}

// Server globals
var (
	serverDebug      bool
	serverClientCA   string
	serverRequestLog bool
	serverDomain     string
	serverCertFile   string
	serverKeyFile    string
	serverPort       = 443
	serverPublicPort = 0
	serverUpgrader   = websocket.Upgrader{
		CheckOrigin:       func(r *http.Request) bool { return true },
		EnableCompression: true,
	}
	serverMaxConcurrentConnections = 5000
	serverConnLimiter              = make(chan struct{}, 5000)
	serverUsedPorts                = make(map[int]bool)
	serverPortMutex                sync.Mutex
	serverInactivityTimeout        = 60 * time.Minute
	serverMaxClientStreams         = 1024
	// serverLegacyListeners binds the per-client port (7000+) and allows
	// non-tls registrations (4000+). Current clients use /mux on the main
	// port; disable on internet-facing hosts to keep the host's port space
	// out of client reach.
	serverLegacyListeners = true
	// Unauthenticated QUIC connections allowed at once, and how long one
	// may take to present its registration.
	serverQUICPendingSlots = make(chan struct{}, 256)
	serverQUICAuthTimeout  = 10 * time.Second
	serverStateSaveMu      sync.Mutex
	serverStartTime        = time.Now()

	serverBasePortNoTLS     = 4000
	serverBaseClientPortTLS = 7000

	serverStateFile = "state.dat"
	serverLogFile   = "server.log"

	serverShutdownChan = make(chan struct{})
	serverShutdownMu   sync.Mutex
	serverWg           sync.WaitGroup

	serverBufferPool = sync.Pool{
		New: func() interface{} {
			buf := make([]byte, 32*1024)
			return &buf
		},
	}
)

// registry mutation helpers keep legacy map access behind one synchronization
// boundary while allowing injected handlers to observe the same registry.
func putServerClient(id string, c *serverClientInfo) {
	legacyClientRegistry.Put(id, c)
}

// Auth globals
var (
	serverApiToken             string
	serverAuthEnabled          bool
	serverAllowAnonymous       bool
	serverClientIdentityPolicy = newControlIdentityPolicy(nil)

	serverRateLimiters     = make(map[string]*rate.Limiter)
	serverRateLimiterMutex sync.Mutex

	serverRateLimit   = rate.Limit(10)
	serverRateBurst   = 20
	serverRegLimit    = rate.Limit(1)
	serverRegBurst    = 5
	serverRegLimiters = make(map[string]*rate.Limiter)
)

// Metrics
var (
	srvActiveClients = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "xrok_active_clients",
		Help: "Number of currently connected clients",
	})

	srvActiveProxyConnections = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "xrok_active_connections",
		Help: "Number of active proxy connections",
	})

	srvTotalConnections = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "xrok_total_connections",
		Help: "Total number of proxy connections handled",
	})

	srvBytesReceived = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "xrok_bytes_received_total",
		Help: "Total bytes received from clients",
	})

	srvBytesSent = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "xrok_bytes_sent_total",
		Help: "Total bytes sent to clients",
	})

	srvConnectionDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "xrok_connection_duration_seconds",
		Help:    "Duration of proxy connections",
		Buckets: prometheus.ExponentialBuckets(0.1, 2, 10),
	})

	srvConnectionErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "xrok_connection_errors_total",
		Help: "Total connection errors by type",
	}, []string{"type"})

	srvRegistrationAttempts = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "xrok_registration_attempts_total",
		Help: "Total client registration attempts",
	})

	srvRegistrationSuccess = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "xrok_registration_success_total",
		Help: "Successful client registrations",
	})

	srvWsUpgrades = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "xrok_websocket_upgrades_total",
		Help: "Total WebSocket upgrade attempts",
	})

	srvWsPingPongLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "xrok_ws_ping_latency_seconds",
		Help:    "WebSocket ping-pong latency",
		Buckets: prometheus.ExponentialBuckets(0.001, 2, 10),
	})
	srvCapabilityDenied = prometheus.NewCounter(prometheus.CounterOpts{Name: "xrok_capability_denied_total", Help: "Requests rejected by tunnel capability policy"})
)

type srvConnectionTimer struct {
	start time.Time
}

func newSrvConnectionTimer() *srvConnectionTimer {
	return &srvConnectionTimer{start: time.Now()}
}

func (t *srvConnectionTimer) observe() {
	srvConnectionDuration.Observe(time.Since(t.start).Seconds())
}

// RunServer runs the xrok server with the given arguments
func RunServer(args []string) {
	runServer(args, nil)
}

func runServer(args []string, injectedExporter telemetry.Exporter) {
	runServerWithConfig(args, nil, injectedExporter)
}

// serverConfigPath returns the --config value in either "--config path" or
// "--config=path" form. Parsing stops at the "--" terminator like FlagSet.
func serverConfigPath(args []string) (string, bool) {
	for i, arg := range args {
		if arg == "--" {
			break
		}
		for _, name := range []string{"-config", "--config"} {
			if arg == name && i+1 < len(args) {
				return args[i+1], true
			}
			if value, ok := strings.CutPrefix(arg, name+"="); ok {
				return value, true
			}
		}
	}
	return "", false
}

// runServerWithConfig serves with preset applied before CLI flags. Embedded
// runtimes pass their validated config here so authentication, identities
// and rate limits are not lost in the flag translation.
func runServerWithConfig(args []string, preset *config.ServerConfig, injectedExporter telemetry.Exporter) {
	serverPublicPort = 0
	loadedConfig := preset
	if path, ok := serverConfigPath(args); ok {
		cfg, err := config.LoadServerConfig(path)
		if err != nil {
			fmt.Printf("Failed to load server config: %v\n", err)
			os.Exit(1)
		}
		if err := cfg.Validate(); err != nil {
			fmt.Printf("Invalid server config: %v\n", err)
			os.Exit(1)
		}
		loadedConfig = cfg
	}
	if loadedConfig != nil {
		serverDomain = loadedConfig.Domain
		serverPort = loadedConfig.ProxyPort
		serverPublicPort = loadedConfig.PublicPort
		serverDebug = loadedConfig.Debug
		serverRequestLog = loadedConfig.RequestLog
		serverClientCA = loadedConfig.ClientCAPath
		serverCertFile = loadedConfig.TLSCertPath
		serverKeyFile = loadedConfig.TLSKeyPath
		if loadedConfig.StateFile != "" {
			serverStateFile = loadedConfig.StateFile
		}
		if loadedConfig.LogFile != "" {
			serverLogFile = loadedConfig.LogFile
		}
		if loadedConfig.RateLimit > 0 {
			serverRateLimit = rate.Limit(loadedConfig.RateLimit)
			serverRateBurst = loadedConfig.RateLimit * 2
		}
		serverApiToken = loadedConfig.APIToken
		serverClientIdentityPolicy = newControlIdentityPolicy(loadedConfig.ClientIdentities)
		serverAllowAnonymous = loadedConfig.AllowAnonymous
		serverAllowPrivateEgress = loadedConfig.AllowPrivateEgress
		if loadedConfig.LegacyListeners != nil {
			serverLegacyListeners = *loadedConfig.LegacyListeners
		}
		if loadedConfig.ReversePortMin > 0 {
			serverReversePortMin = loadedConfig.ReversePortMin
		}
		if loadedConfig.ReversePortMax > 0 {
			serverReversePortMax = loadedConfig.ReversePortMax
		}
		if loadedConfig.ReverseMaxPerClient > 0 {
			serverReverseMaxPerCli = loadedConfig.ReverseMaxPerClient
		}
	}

	fs := flag.NewFlagSet("server", flag.ExitOnError)
	fs.String("config", "", "Path to YAML config file")
	// Preserve a domain loaded from --config unless the CLI explicitly
	// overrides it. Registering the flag with an empty default would reset the
	// already-loaded value during FlagSet construction.
	fs.StringVar(&serverDomain, "domain", serverDomain, "Domain to use in URLs (required)")
	fs.IntVar(&serverPublicPort, "public-port", serverPublicPort, "Advertised HTTPS port behind ingress (0 = listen port)")
	fs.IntVar(&serverPort, "port", serverPort, "Unified HTTPS and QUIC port")
	fs.StringVar(&serverCertFile, "tls-cert", serverCertFile, "Path to TLS certificate")
	fs.StringVar(&serverKeyFile, "tls-key", serverKeyFile, "Path to TLS private key")
	fs.StringVar(&serverStateFile, "state-file", serverStateFile, "Path to persisted server state")
	fs.StringVar(&serverLogFile, "log-file", serverLogFile, "Path to server log")
	fs.BoolVar(&serverDebug, "debug", serverDebug, "Enable debug logging")
	fs.BoolVar(&serverRequestLog, "request-log", serverRequestLog, "Enable HTTP request logging")
	fs.StringVar(&serverClientCA, "client-ca", serverClientCA, "Path to CA certificate for mTLS client verification")
	fs.StringVar(&serverPprofAddr, "pprof", serverPprofAddr, "Serve net/http/pprof on this loopback address (e.g. 127.0.0.1:6060)")
	fs.BoolVar(&serverTrustProxyHeaders, "trust-proxy-headers", serverTrustProxyHeaders, "Key rate limits on X-Forwarded-For/X-Real-IP (only behind a trusted proxy)")
	fs.BoolVar(&serverLegacyListeners, "legacy-listeners", serverLegacyListeners, "Bind legacy per-client ports (7000+) and allow non-tls registrations (4000+)")
	fs.BoolVar(&serverAllowAnonymous, "allow-anonymous", serverAllowAnonymous, "Start without authentication (anyone can register and proxy)")
	fs.BoolVar(&serverAllowPrivateEgress, "allow-private-egress", serverAllowPrivateEgress, "Let SOCKS/UDP streams reach loopback, private and link-local addresses")
	fs.IntVar(&serverReversePortMin, "reverse-port-min", serverReversePortMin, "Lowest port a client may bind with -R")
	fs.IntVar(&serverReversePortMax, "reverse-port-max", serverReversePortMax, "Highest port a client may bind with -R")
	fs.Parse(args)
	if serverPublicPort < 0 || serverPublicPort > 65535 {
		fmt.Println("Invalid public-port")
		return
	}
	telemetryConfig := config.DefaultServerConfig().Telemetry
	if loadedConfig != nil {
		telemetryConfig = loadedConfig.Telemetry
	}
	telemetryConfig, err := telemetryConfigForServer(telemetryConfig, injectedExporter, os.Getenv)
	if err != nil {
		fmt.Printf("Invalid OTLP configuration: %v\n", err)
		return
	}
	telemetryRuntime := runtimeTelemetry{exporter: injectedExporter, timeout: telemetryConfig.ShutdownTimeout}
	if injectedExporter == nil {
		telemetryRuntime, err = newRuntimeTelemetry(telemetryConfig, nil)
		if err != nil {
			fmt.Printf("Invalid OTLP configuration: %v\n", err)
			return
		}
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), telemetryConfig.ShutdownTimeout)
			_ = telemetryRuntime.drain(ctx)
			cancel()
		}()
	}
	setupServerLogging()

	if serverDomain == "" {
		fmt.Println("Flag -domain is required.")
		os.Exit(1)
	}

	initServerCertificates()
	initServerAuth()

	serverWg.Add(1)
	go cleanupServerRateLimiters(serverShutdownChan, &serverWg)

	initServerMetrics()
	startServerPprof(serverPprofAddr)

	loadServerState()

	go gracefulServerShutdown(serverShutdownChan)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		<-serverShutdownChan
		cancel()
	}()

	serverWg.Add(1)
	go func() {
		defer serverWg.Done()
		waitForProxyConnectionTLS(ctx, serverPort, telemetryRuntime.exporter)
	}()

	// Start QUIC listener on port 443 (UDP)
	serverWg.Add(1)
	go func() {
		defer serverWg.Done()
		waitForQUICConnections(ctx, serverPort)
	}()

	defer func() {
		if r := recover(); r != nil {
			log.Printf("Recovered from panic: %v", r)
			saveServerState()
		}
	}()

	// Block until shutdown
	<-serverShutdownChan
	for clientID, client := range legacyClientRegistry.Snapshot() {
		log.Printf("Closing client %s", clientID)
		if client.cancelFunc != nil {
			client.cancelFunc()
		}
	}
	serverWg.Wait()
	saveServerState()
	log.Println("Server shutdown complete")
}

func setupServerLogging() {
	file, err := os.OpenFile(serverLogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640)
	if err != nil {
		fmt.Printf("Failed to open log file %s: %v", serverLogFile, err)
		os.Exit(1)
	}
	log.SetOutput(file)
	log.SetFlags(log.LstdFlags | log.Lshortfile)
}

func initServerCertificates() {
	if serverCertFile == "" {
		serverCertFile = fmt.Sprintf("/etc/letsencrypt/live/%s/fullchain.pem", serverDomain)
	}
	if serverKeyFile == "" {
		serverKeyFile = fmt.Sprintf("/etc/letsencrypt/live/%s/privkey.pem", serverDomain)
	}
}

func initServerAuth() {
	serverAuthEnabled = false
	if token := os.Getenv("XROK_API_TOKEN"); token != "" {
		serverApiToken = token
	}
	if configuredLimit := os.Getenv("XROK_RATE_LIMIT"); configuredLimit != "" {
		if n, err := strconv.Atoi(configuredLimit); err == nil && n > 0 {
			serverRateLimit = rate.Limit(n)
			serverRateBurst = n * 2
		}
	}
	if serverApiToken != "" || len(serverClientIdentityPolicy.credentials) > 0 {
		serverAuthEnabled = true
		log.Printf("API authentication enabled (client identities: %d)", len(serverClientIdentityPolicy.credentials))
	} else if serverAllowAnonymous {
		log.Printf("Warning: XROK_API_TOKEN not set, authentication disabled")
	} else {
		fmt.Println("Refusing to start without authentication: set XROK_API_TOKEN, api_token or client_identities (or pass -allow-anonymous).")
		os.Exit(1)
	}
}

func generateServerToken() string {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		log.Fatalf("Failed to generate token: %v", err)
	}
	return hex.EncodeToString(bytes)
}

func getServerRateLimiter(ip string, limiters map[string]*rate.Limiter, r rate.Limit, b int) *rate.Limiter {
	serverRateLimiterMutex.Lock()
	defer serverRateLimiterMutex.Unlock()

	limiter, exists := limiters[ip]
	if !exists {
		limiter = rate.NewLimiter(r, b)
		limiters[ip] = limiter
	}
	return limiter
}

// serverTrustProxyHeaders keys rate limits on X-Forwarded-For/X-Real-IP.
// Only safe behind a proxy that overwrites them; otherwise any caller could
// pick a fresh key per request and bypass the limiter.
var serverTrustProxyHeaders bool

// serverPublicURL is the visitor URL of a tunnel; a non-default listen port
// must be part of it or the printed link is unreachable.
func serverAdvertisedPort() int {
	if serverPublicPort > 0 {
		return serverPublicPort
	}
	return serverPort
}
func serverClientRoutingPort(legacyPort int) int {
	if serverLegacyListeners {
		return legacyPort
	}
	return serverAdvertisedPort()
}
func serverPublicURL(name, domain string) string {
	port := serverAdvertisedPort()
	if port == 443 {
		return fmt.Sprintf("https://%s.%s", name, domain)
	}
	return fmt.Sprintf("https://%s.%s:%d", name, domain, port)
}

func getServerClientIP(r *http.Request) string {
	if !serverTrustProxyHeaders {
		if ip, _, err := net.SplitHostPort(r.RemoteAddr); err == nil && ip != "" {
			return ip
		}
		return r.RemoteAddr
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff != "" {
		parts := strings.Split(xff, ",")
		if len(parts) > 0 {
			return strings.TrimSpace(parts[0])
		}
	}

	xri := r.Header.Get("X-Real-IP")
	if xri != "" {
		return xri
	}

	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && ip != "" {
		return ip
	}
	return r.RemoteAddr
}

func serverRateLimitMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := getServerClientIP(r)
		limiter := getServerRateLimiter(ip, serverRateLimiters, serverRateLimit, serverRateBurst)

		if !limiter.Allow() {
			http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
			log.Printf("Rate limit exceeded for IP: %s", ip)
			return
		}

		next(w, r)
	}
}

func serverRegRateLimitMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := getServerClientIP(r)
		limiter := getServerRateLimiter(ip, serverRegLimiters, serverRegLimit, serverRegBurst)

		if !limiter.Allow() {
			http.Error(w, "Registration rate limit exceeded", http.StatusTooManyRequests)
			log.Printf("Registration rate limit exceeded for IP: %s", ip)
			return
		}

		next(w, r)
	}
}

func serverAuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return serverAuthScopeMiddleware(scopeAdmin, next)
}

// logRequest logs HTTP request details
func logRequest(requestData []byte, clientIP string, subdomain string) {
	if !serverRequestLog {
		return
	}

	lines := strings.Split(string(requestData), "\r\n")
	if len(lines) == 0 {
		return
	}

	// Parse request line
	parts := strings.Split(lines[0], " ")
	if len(parts) < 2 {
		return
	}

	method := parts[0]
	path := parts[1]

	timestamp := time.Now().Format("2006-01-02 15:04:05")
	log.Printf("[REQUEST] %s %s %s %s %s.%s", timestamp, clientIP, method, path, subdomain, serverDomain)
}

// verifyBasicAuth checks if request has valid Basic Auth
func verifyBasicAuth(requestData []byte, expectedAuth string) bool {
	if expectedAuth == "" {
		return true
	}

	lines := strings.Split(string(requestData), "\r\n")
	for _, line := range lines {
		if strings.HasPrefix(strings.ToLower(line), "authorization:") {
			authHeader := strings.TrimSpace(line[14:])
			if !strings.HasPrefix(authHeader, "Basic ") {
				return false
			}
			provided := authHeader[6:]
			decoded, err := base64.StdEncoding.DecodeString(provided)
			if err != nil {
				return false
			}
			return string(decoded) == expectedAuth
		}
	}
	return false
}

// injectHeaders adds custom headers to an HTTP request
func injectHeaders(requestData []byte, headers map[string]string) []byte {
	if len(headers) == 0 {
		return requestData
	}

	// Find end of first line (request line)
	requestStr := string(requestData)
	firstLineEnd := strings.Index(requestStr, "\r\n")
	if firstLineEnd == -1 {
		return requestData
	}

	// Remove existing values first so a policy header cannot be bypassed by
	// sending a second value with different casing.
	lines := strings.Split(requestStr, "\r\n")
	blocked := make(map[string]struct{}, len(headers))
	for key := range headers {
		blocked[strings.ToLower(strings.TrimSpace(key))] = struct{}{}
	}
	filtered := make([]string, 0, len(lines))
	inHeaders := true
	for i, line := range lines {
		if i == 0 || !inHeaders {
			filtered = append(filtered, line)
			continue
		}
		if line == "" {
			inHeaders = false
			filtered = append(filtered, line)
			continue
		}
		name, _, ok := strings.Cut(line, ":")
		if ok {
			if _, remove := blocked[strings.ToLower(strings.TrimSpace(name))]; remove {
				continue
			}
		}
		filtered = append(filtered, line)
	}

	// Build headers string
	var headerStr strings.Builder
	for key, value := range headers {
		headerStr.WriteString(key)
		headerStr.WriteString(": ")
		headerStr.WriteString(value)
		headerStr.WriteString("\r\n")
	}

	// Insert headers after the first line. The body remains untouched because
	// only header lines before the first blank line are filtered.
	filteredRequest := strings.Join(filtered, "\r\n")
	firstLineEnd = strings.Index(filteredRequest, "\r\n")
	return []byte(filteredRequest[:firstLineEnd+2] + headerStr.String() + filteredRequest[firstLineEnd+2:])
}

// checkBasicAuth verifies HTTP Basic Auth credentials
// Returns true if auth is valid or not required, false otherwise
func checkBasicAuth(conn net.Conn, expectedAuth string) (bool, []byte, error) {
	if expectedAuth == "" {
		return true, nil, nil
	}

	// Read HTTP request to get Authorization header
	// We need to peek at the request without consuming it
	buf := make([]byte, 4096)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := conn.Read(buf)
	if err != nil {
		return false, nil, err
	}
	conn.SetReadDeadline(time.Time{})

	requestData := buf[:n]

	// Parse the request to find Authorization header
	lines := strings.Split(string(requestData), "\r\n")
	if len(lines) == 0 {
		return false, requestData, nil
	}

	var authHeader string
	for _, line := range lines {
		if strings.HasPrefix(strings.ToLower(line), "authorization:") {
			authHeader = strings.TrimSpace(line[14:])
			break
		}
	}

	if authHeader == "" {
		// No auth provided, send 401
		response := "HTTP/1.1 401 Unauthorized\r\n" +
			"WWW-Authenticate: Basic realm=\"Protected\"\r\n" +
			"Content-Type: text/plain\r\n" +
			"Content-Length: 12\r\n" +
			"\r\n" +
			"Unauthorized"
		conn.Write([]byte(response))
		return false, nil, nil
	}

	// Check if it's Basic auth
	if !strings.HasPrefix(authHeader, "Basic ") {
		return false, requestData, nil
	}

	// Decode and compare credentials
	provided := authHeader[6:] // Remove "Basic " prefix
	decoded, err := base64.StdEncoding.DecodeString(provided)
	if err != nil {
		return false, requestData, nil
	}

	if string(decoded) == expectedAuth {
		return true, requestData, nil
	}

	// Wrong credentials, send 401
	response := "HTTP/1.1 401 Unauthorized\r\n" +
		"WWW-Authenticate: Basic realm=\"Protected\"\r\n" +
		"Content-Type: text/plain\r\n" +
		"Content-Length: 12\r\n" +
		"\r\n" +
		"Unauthorized"
	conn.Write([]byte(response))
	return false, nil, nil
}

func cleanupServerRateLimiters(shutdown chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-shutdown:
			if serverDebug {
				log.Printf("Rate limiter cleanup goroutine shutting down")
			}
			return
		case <-ticker.C:
			serverRateLimiterMutex.Lock()
			serverRateLimiters = make(map[string]*rate.Limiter)
			serverRegLimiters = make(map[string]*rate.Limiter)
			serverRateLimiterMutex.Unlock()
			if serverDebug {
				log.Printf("Rate limiters cleaned up")
			}
		}
	}
}

func initServerMetrics() {
	prometheus.MustRegister(
		srvActiveClients,
		srvActiveProxyConnections,
		srvTotalConnections,
		srvBytesReceived,
		srvBytesSent,
		srvConnectionDuration,
		srvConnectionErrors,
		srvRegistrationAttempts,
		srvRegistrationSuccess,
		srvWsUpgrades,
		srvWsPingPongLatency,
		srvCapabilityDenied,
	)
}

// serverLegacyPortSpan bounds each legacy per-client port range so the
// allocator can never walk into ports of other services on the host.
const serverLegacyPortSpan = 1000

var errServerPortsExhausted = errors.New("per-client port range exhausted")

func generateServerPorts() (int, int, error) {
	serverPortMutex.Lock()
	defer serverPortMutex.Unlock()

	portNoTLS := findServerAvailablePort(serverBasePortNoTLS, 1)
	clientPortTLS := findServerAvailablePort(serverBaseClientPortTLS, 1)
	if portNoTLS < 0 || clientPortTLS < 0 {
		return 0, 0, errServerPortsExhausted
	}

	serverUsedPorts[portNoTLS] = true
	serverUsedPorts[clientPortTLS] = true

	return portNoTLS, clientPortTLS, nil
}

func findServerAvailablePort(basePort, step int) int {
	for port := basePort; port < basePort+serverLegacyPortSpan; port += step {
		if !serverUsedPorts[port] {
			return port
		}
	}
	return -1
}

func releaseServerPorts(ports ...int) {
	serverPortMutex.Lock()
	defer serverPortMutex.Unlock()
	for _, port := range ports {
		delete(serverUsedPorts, port)
	}
}

// serverClientLifecycleMu serialises registration with teardown so a stale
// session cannot interleave with its successor's registration.
var serverClientLifecycleMu sync.Mutex

// forgetServerClient removes client's registry entry and routes, but only
// while it is still the registered instance for clientID, and reports whether
// it was. After a re-registration the old session closes late; its successor
// owns the ID, routes and possibly the same ports, so a stale teardown must
// not touch them. Callers release the ports only when this returns true, and
// only after the listeners bound to them are closed.
func forgetServerClient(clientID string, client *serverClientInfo) bool {
	serverClientLifecycleMu.Lock()
	defer serverClientLifecycleMu.Unlock()
	if current, ok := legacyClientRegistry.Get(clientID); !ok || current != client {
		return false
	}
	for _, t := range client.tunnels {
		legacyRouteRegistry.DeleteOwned(t.Name, clientID)
	}
	legacyClientRegistry.Delete(clientID)
	return true
}

func closeServerClientConnections(clientID string, client *serverClientInfo) {
	// Ensure counter is decremented exactly once when client is fully cleaned up
	defer client.decrementOnce.Do(func() {
		srvActiveClients.Dec()
	})

	current := forgetServerClient(clientID, client)

	client.muxSessionMutex.Lock()
	if client.muxSession != nil {
		client.muxSession.Close()
		client.muxSession = nil
	}
	client.muxSessionMutex.Unlock()

	if client.listenerTLS != nil {
		client.listenerTLS.Close()
	}
	if client.listenerNoTLS != nil {
		client.listenerNoTLS.Close()
	}

	if client.clientListenerTLS != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.clientListenerTLS.Shutdown(ctx); err != nil {
			log.Printf("Error shutting down TLS client listener for client %s: %v", clientID, err)
		}
	}
	if client.clientListenerNoTLS != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.clientListenerNoTLS.Shutdown(ctx); err != nil {
			log.Printf("Error shutting down non-TLS client listener for client %s: %v", clientID, err)
		}
	}

	if client.cancelFunc != nil {
		client.cancelFunc()
	}

	if current {
		releaseServerPorts(client.PortNoTLS, client.ClientPortTLS, client.ClientPortNoTLS)
	}
	if serverDebug {
		log.Printf("Resources released for client %s", clientID)
	}
}

// serverAttachTimeout bounds how long a registration may hold its
// subdomains without the client opening its mux (tls) or QUIC connection.
var serverAttachTimeout = 30 * time.Second

func reapUnattachedRegistration(ctx context.Context, clientID string, client *serverClientInfo) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(serverAttachTimeout):
	}
	client.attachMu.Lock()
	reap := !client.attached
	// Monotonic: never un-expire a registration a teardown already expired.
	client.reaped = client.reaped || reap
	client.attachMu.Unlock()
	if reap {
		log.Printf("Client %s registered but never connected within %s, releasing", clientID, serverAttachTimeout)
		closeServerClientConnections(clientID, client)
	}
}

// claimServerAttach publishes a new data connection for a registration under
// attachMu, the lock that also orders the unattached reaper and connection
// teardown (lock order: attachMu, then muxSessionMutex/connMutex). It fails
// once the registration was expired, so a late connection cannot revive one
// whose routes are gone. publish runs under the lock and returns the
// connection it displaced, which the caller closes outside the lock.
func claimServerAttach(client *serverClientInfo, publish func() io.Closer) (io.Closer, bool) {
	client.attachMu.Lock()
	defer client.attachMu.Unlock()
	if client.reaped || (client.ctx != nil && client.ctx.Err() != nil) {
		return nil, false
	}
	client.attached = true
	return publish(), true
}

// releaseServerAttach runs when a data connection ends. Under attachMu it
// checks whether that connection is still the published one and, if so,
// unpublishes it and marks the registration expired, so a successor cannot
// attach between this decision and the teardown that follows.
func releaseServerAttach(client *serverClientInfo, unpublishIfCurrent func() bool) bool {
	client.attachMu.Lock()
	defer client.attachMu.Unlock()
	if !unpublishIfCurrent() {
		return false
	}
	client.reaped = true
	return true
}

// monitorServerClientActivity reaps registrations without traffic. It exits
// with the registration context so shutdown does not wait on its ticker, and
// never reaps a client whose mux session is still up: yamux keepalives prove
// it alive even when its tunnels are idle.
func monitorServerClientActivity(ctx context.Context, clientID string, client *serverClientInfo) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		client.muxSessionMutex.Lock()
		muxAlive := client.muxSession != nil && !client.muxSession.IsClosed()
		client.muxSessionMutex.Unlock()
		if muxAlive {
			continue
		}
		if time.Since(client.GetLastActivity()) > serverInactivityTimeout {
			if serverDebug {
				log.Printf("Client %s inactive, closing connections", clientID)
			}

			if client.cancelFunc != nil {
				client.cancelFunc()
			}

			go func() {
				closeServerClientConnections(clientID, client)
			}()
			return
		}
	}
}

// registrationRequest from client
type serverRegistrationRequest struct {
	ConnectionType string                `json:"connection_type"`
	ClientID       string                `json:"client_id"`
	Tunnels        []ServerTunnelConfig  `json:"tunnels"`
	Reverse        []ServerReverseConfig `json:"reverse,omitempty"`
	Headers        map[string]string     `json:"headers,omitempty"`
	Compress       bool                  `json:"compress,omitempty"`
	P2PInfo        *p2p.PeerInfo         `json:"p2p_info,omitempty"`
}

func registerServerClientHandler(w http.ResponseWriter, r *http.Request) {
	srvRegistrationAttempts.Inc()

	// Parse request - support both JSON body and query params for backwards compatibility
	var regReq serverRegistrationRequest

	if r.Header.Get("Content-Type") == "application/json" {
		if err := json.NewDecoder(r.Body).Decode(&regReq); err != nil {
			log.Printf("Failed to decode registration request: %v", err)
			http.Error(w, "Invalid JSON body", http.StatusBadRequest)
			return
		}
	} else {
		// Legacy query param support
		regReq.ConnectionType = r.URL.Query().Get("connection_type")
		regReq.ClientID = r.URL.Query().Get("id")
	}

	if regReq.ConnectionType == "" {
		regReq.ConnectionType = "tls" // default
	}
	if regReq.ConnectionType != "tls" && regReq.ConnectionType != "quic" && regReq.ConnectionType != "non-tls" {
		http.Error(w, "Invalid connection_type parameter", http.StatusBadRequest)
		return
	}

	clientID, allowed := requirePrincipalClient(w, r, regReq.ClientID)
	if !allowed {
		return
	}
	if clientID == "" {
		clientID = uuid.New().String()
	}
	if err := bindRegistrationP2PIdentity(clientID, regReq.P2PInfo); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	reverse, err := validateReverseConfigs(regReq.Reverse)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	regReq.Reverse = reverse

	if regReq.ConnectionType == "non-tls" && !serverLegacyListeners {
		http.Error(w, "non-tls connections are disabled on this server", http.StatusForbidden)
		return
	}
	// Legacy ports are only reserved when legacy listeners exist; otherwise
	// they would cap registrations at the range size for nothing. Clients
	// only require a non-empty client_port_tls, which "0" satisfies.
	var portNoTLS, clientPortTLS int
	if serverLegacyListeners {
		var err error
		if portNoTLS, clientPortTLS, err = generateServerPorts(); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
	}
	ctx, cancel := context.WithCancel(context.Background())

	serverClientLifecycleMu.Lock()
	// Clean up existing client if reconnecting (stateless behavior)
	if existing, exists := legacyClientRegistry.Get(clientID); exists {
		// Clean up old tunnel routes
		for _, t := range existing.tunnels {
			legacyRouteRegistry.DeleteOwned(t.Name, clientID)
		}

		// Cancel old client context
		if existing.cancelFunc != nil {
			existing.cancelFunc()
		}

		// Close old connections
		existing.muxSessionMutex.Lock()
		oldMux := existing.muxSession
		existing.muxSessionMutex.Unlock()
		if oldMux != nil {
			oldMux.Close()
		}

		// Release old ports
		releaseServerPorts(existing.PortNoTLS, existing.ClientPortTLS, existing.ClientPortNoTLS)
		legacyClientRegistry.Delete(clientID)
		existing.decrementOnce.Do(func() {
			srvActiveClients.Dec()
		})
		log.Printf("Cleaned up existing client %s for reconnection", clientID)
	}

	// Process tunnels
	var tunnels []ServerTunnelConfig
	var tunnelInfos []ServerTunnelInfo
	tunnelMap := make(map[string]string)

	if len(regReq.Tunnels) > 0 {
		for _, t := range regReq.Tunnels {
			name := t.Name
			if name == "" {
				name = uuid.New().String()[:8] // Generate short ID
			}

			// Check for duplicate subdomain (from different client)
			existingRoute, routeExists := legacyRouteRegistry.Get(name)
			if routeExists && existingRoute.ClientID != clientID {
				serverClientLifecycleMu.Unlock()
				http.Error(w, fmt.Sprintf("Subdomain '%s' already in use by another client", name), http.StatusConflict)
				cancel()
				// Without this every refused registration leaked its two
				// port numbers and walked the allocator forward.
				releaseServerPorts(portNoTLS, clientPortTLS)
				return
			}

			tunnels = append(tunnels, ServerTunnelConfig{Name: name, Target: t.Target, BasicAuth: t.BasicAuth, Capability: t.Capability, CapabilityAudience: t.CapabilityAudience})
			tunnelMap[name] = t.Target

			url := serverPublicURL(name, serverDomain)
			tunnelInfos = append(tunnelInfos, ServerTunnelInfo{
				Name:   name,
				Target: t.Target,
				URL:    url,
			})
		}
	} else {
		// Legacy single tunnel - use clientID as subdomain
		tunnels = append(tunnels, ServerTunnelConfig{Name: clientID, Target: ""})
		tunnelMap[clientID] = ""
	}

	client := &serverClientInfo{
		serializableClientInfo: serializableClientInfo{
			UDID:           clientID,
			PortNoTLS:      portNoTLS,
			ClientPortTLS:  clientPortTLS,
			ConnectionType: regReq.ConnectionType,
			Registered:     true,
		},
		cancelFunc:     cancel,
		ctx:            ctx,
		wsConnChans:    make(map[string]chan *websocket.Conn),
		tunnels:        tunnels,
		tunnelMap:      tunnelMap,
		reverseConfigs: regReq.Reverse,
		customHeaders:  regReq.Headers,
		compress:       regReq.Compress,
		p2pInfo:        regReq.P2PInfo,
		p2pConnections: make(map[string]net.Conn),
		sessionToken:   generateServerToken(),
	}
	client.SetLastActivity(time.Now())

	putServerClient(clientID, client)

	// Register tunnel routes
	for _, t := range tunnels {
		log.Printf("Registering tunnel route: name=%s, target=%s, auth_configured=%t, header_count=%d", t.Name, t.Target, t.BasicAuth != "", len(regReq.Headers))
		legacyRouteRegistry.Put(t.Name, &TunnelRoute{
			ClientID:           clientID,
			TunnelName:         t.Name,
			Target:             t.Target,
			BasicAuth:          t.BasicAuth,
			Headers:            regReq.Headers,
			Capability:         t.Capability,
			CapabilityAudience: t.CapabilityAudience,
		})
	}
	serverClientLifecycleMu.Unlock()

	// Start reverse forwarding listeners
	for _, revConfig := range regReq.Reverse {
		rc := revConfig // capture for closure
		serverWg.Add(1)
		go func() {
			defer serverWg.Done()
			startReverseListener(ctx, client, rc.Remote, rc.Local)
		}()
		log.Printf("Started reverse listener on %s -> client local %s for client %s", rc.Remote, rc.Local, clientID)
	}

	if serverLegacyListeners {
		serverWg.Add(1)
		go func() {
			defer serverWg.Done()
			waitForServerClientConnection(ctx, client, client.ClientPortTLS, true, clientID)
		}()
	}

	switch regReq.ConnectionType {
	case "tls":
	case "quic":
		// QUIC connections are handled by the global QUIC listener
		// Client will connect via QUIC after registration
	case "non-tls":
		serverWg.Add(1)
		go func() {
			defer serverWg.Done()
			waitForProxyConnectionNoTLS(ctx, client, client.PortNoTLS)
		}()
	}

	serverWg.Add(1)
	go func() {
		defer serverWg.Done()
		monitorServerClientActivity(ctx, clientID, client)
	}()
	// With legacy listeners a "tls" client may attach through the legacy
	// /ws path, which the reaper cannot see.
	if !serverLegacyListeners {
		go reapUnattachedRegistration(ctx, clientID, client)
	}
	saveServerState()

	srvRegistrationSuccess.Inc()
	srvActiveClients.Inc()

	// Build response
	type regResponse struct {
		UDID          string             `json:"udid"`
		ClientPortTLS string             `json:"client_port_tls"`
		ProxyURLNoTLS string             `json:"proxy_url_notls"`
		ProxyURLTLS   string             `json:"proxy_url_tls,omitempty"`
		Tunnels       []ServerTunnelInfo `json:"tunnels,omitempty"`
		SessionToken  string             `json:"session_token"`
		Multiplexed   bool               `json:"multiplexed"`
	}

	response := regResponse{
		UDID:          clientID,
		ClientPortTLS: strconv.Itoa(serverClientRoutingPort(clientPortTLS)),
		ProxyURLNoTLS: fmt.Sprintf("%s:%d", serverDomain, portNoTLS),
		Tunnels:       tunnelInfos,
		SessionToken:  client.sessionToken,
		Multiplexed:   regReq.ConnectionType == "tls" || regReq.ConnectionType == "quic",
	}

	if regReq.ConnectionType == "tls" {
		response.ProxyURLTLS = serverPublicURL(clientID, serverDomain)
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("Failed to encode response: %v", err)
	}
}

func bindRegistrationP2PIdentity(clientID string, info *p2p.PeerInfo) error {
	if info == nil {
		return nil
	}
	if info.PeerID == "" {
		info.PeerID = clientID
		return nil
	}
	if info.PeerID != clientID {
		return fmt.Errorf("P2P peer identity does not match authenticated client")
	}
	return nil
}

func extractServerClientIDFromHost(host string) string {
	parts := strings.Split(host, ".")
	if len(parts) < 2 {
		return ""
	}
	return parts[0]
}

// resolveServerTunnel resolves a public subdomain through injected registries.
// It is intentionally side-effect free so embedded runtimes can test routing
// without opening sockets.
func resolveServerTunnel(deps *ServerDeps, subdomain string) (*serverClientInfo, *TunnelRoute, string) {
	if deps == nil || deps.Clients == nil || deps.Routes == nil {
		return nil, nil, ""
	}
	route, ok := deps.Routes.Get(subdomain)
	if ok {
		client, _ := deps.Clients.Get(route.ClientID)
		return client, route, route.TunnelName
	}
	client, _ := deps.Clients.Get(subdomain)
	return client, nil, subdomain
}

func waitForProxyConnectionTLS(ctx context.Context, proxyPort int, exporter telemetry.Exporter) {
	certs, err := serverCertReloader()
	if err != nil {
		log.Printf("Failed to load certificate for port %d: %v", proxyPort, err)
		return
	}

	// Create unified HTTP handler for both registration and proxy
	deps := serverDepsFromGlobals()
	deps.Exporter = exporter
	handler := telemetry.HTTPMiddlewareWithExporter(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Propagate runtime-owned dependencies through the request context. This
		// keeps migrated handlers isolated while legacy endpoints are transitioned.
		if _, ok := ServerDepsFromContext(r.Context()); !ok {
			r = r.WithContext(WithServerDeps(r.Context(), deps))
		}
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}

		// Check if this is a request to the main domain (registration/control).
		// A bare IP can never name a tunnel, so it is served as the main
		// domain too (clients connecting by IP, e.g. through an SSH forward).
		if host == serverDomain || net.ParseIP(strings.Trim(host, "[]")) != nil {
			if !controlClientCertAccepted(r) {
				http.Error(w, "Client certificate required", http.StatusForbidden)
				return
			}
			// Handle registration and control endpoints
			switch r.URL.Path {
			case "/register_client":
				serverAuthScopeMiddleware(scopeClient, serverRegRateLimitMiddleware(registerServerClientHandler))(w, r)
				return
			case "/client_ws":
				serverAuthScopeMiddleware(scopeClient, serverRateLimitMiddleware(serverClientWebSocketHandler))(w, r)
				return
			case "/mux":
				serverRateLimitMiddleware(serverMuxWebSocketHandler)(w, r)
				return
			case "/p2p/signal":
				serverAuthScopeMiddleware(scopeP2PSignal, handleP2PSignaling)(w, r)
				return
			case "/p2p/ws":
				serverAuthScopeMiddleware(scopeP2PSignal, handleP2PWebSocket)(w, r)
				return
			case "/p2p/clients":
				serverAuthScopeMiddleware(scopeP2PRead, handleP2PClients)(w, r)
				return
			case "/p2p/discover":
				serverAuthScopeMiddleware(scopeP2PAdmin, handleP2PDiscoverTrigger)(w, r)
				return
			case "/p2p/tunnel-owner":
				serverAuthScopeMiddleware(scopeP2PRead, handleP2PTunnelOwner)(w, r)
				return
			case "/health":
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"status":"healthy"}`))
				return
			case "/ready":
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				clientCount := legacyClientRegistry.Len()
				w.Write([]byte(fmt.Sprintf(`{"status":"ready","clients":%d}`, clientCount)))
				return
			case "/stats":
				clientCount := legacyClientRegistry.Len()
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("xrok Server Stats\n"))
				w.Write([]byte("=================\n"))
				w.Write([]byte(fmt.Sprintf("Active Clients: %d\n", clientCount)))
				return
			case "/status":
				serverAuthMiddleware(serverStatusHandler)(w, r)
				return
			case "/metrics":
				serverAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
					promhttp.Handler().ServeHTTP(w, r)
				})(w, r)
				return
			default:
				http.Error(w, "Not Found", http.StatusNotFound)
				return
			}
		}

		// Extract subdomain for proxy requests
		subdomain := extractServerClientIDFromHost(host)
		if subdomain == "" {
			http.Error(w, "Invalid subdomain", http.StatusBadRequest)
			return
		}

		// Look up tunnel route
		requestDeps := serverDepsFromRequest(r)
		client, route, tunnelName := resolveServerTunnel(requestDeps, subdomain)
		routeExists := route != nil

		if client == nil {
			http.Error(w, "Tunnel not found", http.StatusBadGateway)
			return
		}

		// Get BasicAuth and Headers from route
		var basicAuth string
		var customHeaders map[string]string
		if routeExists {
			basicAuth = route.BasicAuth
			customHeaders = route.Headers
			if route.Capability != "" {
				provided := r.Header.Get("X-Xrok-Capability")
				if provided == "" {
					provided = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				}
				valid := subtle.ConstantTimeCompare([]byte(provided), []byte(route.Capability)) == 1
				// A capability value containing a JWT-like token is treated as the
				// HS256 signing secret, enabling short-lived signed bearer tokens.
				if !valid && strings.Count(provided, ".") == 2 {
					valid = verifyJWTCapability(provided, route.Capability, route.CapabilityAudience, time.Now())
				}
				if !valid {
					srvCapabilityDenied.Inc()
					http.Error(w, "tunnel capability required", http.StatusForbidden)
					return
				}
			}
		}

		// Hijack connection for raw proxying
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
			return
		}

		conn, buf, err := hijacker.Hijack()
		if err != nil {
			log.Printf("Failed to hijack connection: %v", err)
			return
		}
		// The server's Read/WriteTimeout deadlines stay armed on a hijacked
		// conn and would cut WebSocket, SSE and long downloads at 5 minutes.
		// Dead visitors are still reaped by TCP keepalive.
		conn.SetDeadline(time.Time{})

		// Handle the proxy connection with the buffered data
		go handleHijackedProxyConnection(conn, buf, r, client, tunnelName, basicAuth, customHeaders)
	}), deps.Exporter)

	tlsConfig := &tls.Config{
		GetCertificate: certs.GetCertificate,
		NextProtos:     []string{"http/1.1"}, // Disable HTTP/2 for hijacking support
		MinVersion:     tls.VersionTLS12,
	}

	// Configure mTLS if client CA is specified
	// The listener also serves public tunnel visitors, who have no client
	// certificate. Verify certificates when presented and let the main-domain
	// router require a verified chain for control-plane endpoints only.
	if serverClientCA != "" {
		caCert, err := os.ReadFile(serverClientCA)
		if err != nil {
			log.Printf("Failed to load client CA certificate: %v", err)
			return
		}
		caCertPool := x509.NewCertPool()
		if !caCertPool.AppendCertsFromPEM(caCert) {
			log.Printf("Failed to parse client CA certificate %s", serverClientCA)
			return
		}
		tlsConfig.ClientCAs = caCertPool
		tlsConfig.ClientAuth = tls.VerifyClientCertIfGiven
		log.Printf("mTLS enabled: control-plane clients must present a certificate signed by %s", serverClientCA)
	}

	server := &http.Server{
		Addr:         fmt.Sprintf(":%d", proxyPort),
		Handler:      handler,
		TLSConfig:    tlsConfig,
		ReadTimeout:  5 * time.Minute,
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  5 * time.Minute,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		server.Shutdown(shutdownCtx)
	}()

	log.Printf("Unified HTTPS server listening on port %d (registration + proxy)", proxyPort)
	if err := server.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
		log.Printf("HTTPS server error: %v", err)
	}
}

// controlClientCertAccepted enforces --client-ca on main-domain requests.
// Liveness and readiness probes stay reachable without a certificate.
func controlClientCertAccepted(r *http.Request) bool {
	if serverClientCA == "" || r.URL.Path == "/health" || r.URL.Path == "/ready" {
		return true
	}
	return r.TLS != nil && len(r.TLS.VerifiedChains) > 0
}

func serverStatusHandler(w http.ResponseWriter, r *http.Request) {
	deps := serverDepsFromRequest(r)
	clients := deps.Clients.Len()
	tunnels := make([]TunnelStatus, 0)
	for _, route := range deps.Routes.Snapshot() {
		tunnels = append(tunnels, TunnelStatus{
			Name:     route.TunnelName,
			Target:   route.Target,
			URL:      serverPublicURL(route.TunnelName, deps.Domain),
			ClientID: route.ClientID,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ServerStatus{
		Status:  "healthy",
		Uptime:  time.Since(deps.StartTime).Round(time.Second).String(),
		Version: "2.0.0",
		Clients: clients,
		Tunnels: tunnels,
	})
}

// waitForQUICConnections handles QUIC connections
func waitForQUICConnections(ctx context.Context, port int) {
	certs, err := serverCertReloader()
	if err != nil {
		log.Printf("Failed to load certificate for QUIC: %v", err)
		return
	}

	tlsConfig := &tls.Config{
		GetCertificate: certs.GetCertificate,
		NextProtos:     []string{"xrok-quic"},
	}

	quicTransport := transport.NewQUICTransport(tlsConfig)
	listener, err := quicTransport.Listen(fmt.Sprintf(":%d", port))
	if err != nil {
		log.Printf("Failed to start QUIC listener: %v", err)
		return
	}
	defer listener.Close()

	go func() {
		<-ctx.Done()
		listener.Close()
	}()

	log.Printf("QUIC server listening on port %d (UDP)", port)

	var backoff acceptBackoff
	for {
		conn, err := listener.Accept()
		if err != nil {
			if !backoff.retry(ctx, err) {
				return
			}
			continue
		}
		backoff.reset()
		select {
		case serverQUICPendingSlots <- struct{}{}:
		default:
			log.Printf("QUIC: too many unauthenticated connections, dropping %s", conn.RemoteAddr())
			conn.Close()
			continue
		}
		go handleQUICServerConnection(ctx, conn.(*transport.QUICConn))
	}
}

func handleQUICServerConnection(ctx context.Context, conn *transport.QUICConn) {
	// The slot taken by the accept loop bounds unauthenticated connections;
	// it is returned once the peer authenticated or was dropped.
	pending := true
	releasePending := func() {
		if pending {
			pending = false
			<-serverQUICPendingSlots
		}
	}
	defer releasePending()

	// Accept first stream for registration. Keepalives keep an idle peer
	// alive forever, so the wait for its first stream must be bounded.
	authDeadline := time.Now().Add(serverQUICAuthTimeout)
	authCtx, authCancel := context.WithDeadline(ctx, authDeadline)
	stream, err := conn.AcceptStream(authCtx)
	authCancel()
	if err != nil {
		conn.Close()
		return
	}

	// Read registration message; a peer that never sends one must not pin
	// the connection.
	regBuf := make([]byte, 256)
	stream.SetReadDeadline(authDeadline)
	n, err := stream.Read(regBuf)
	stream.SetReadDeadline(time.Time{})
	if err != nil {
		conn.Close()
		return
	}

	regMsg := string(regBuf[:n])
	parts := strings.SplitN(regMsg, ":", 3)
	if len(parts) != 3 || parts[0] != "XROK" {
		log.Printf("QUIC: Invalid registration message")
		conn.Close()
		return
	}

	clientID, sessionToken := parts[1], parts[2]

	client, exists := legacyClientRegistry.Get(clientID)

	if !exists || !client.validSessionToken(sessionToken) {
		log.Printf("QUIC: Unknown client %s", clientID)
		conn.Close()
		return
	}

	displaced, ok := claimServerAttach(client, func() io.Closer {
		client.connMutex.Lock()
		defer client.connMutex.Unlock()
		old := client.quicConn
		client.quicConn = conn
		if old != nil && old != conn {
			return old
		}
		return nil
	})
	if !ok {
		log.Printf("QUIC: registration for client %s already expired", clientID)
		conn.Close()
		return
	}
	releasePending()
	if displaced != nil {
		displaced.Close() // its handler sees it is no longer current
	}

	log.Printf("QUIC connection established for client %s", clientID)

	// Hold until the server stops, the peer goes away or the registration
	// is replaced/released; waiting on the server context alone leaked one
	// goroutine and connection per reconnect.
	var clientDone <-chan struct{}
	if client.ctx != nil {
		clientDone = client.ctx.Done()
	}
	peerGone := false
	select {
	case <-ctx.Done():
	case <-conn.Connection().Context().Done():
		peerGone = true
	case <-clientDone:
	}
	conn.Close()

	current := releaseServerAttach(client, func() bool {
		client.connMutex.Lock()
		defer client.connMutex.Unlock()
		if client.quicConn != conn {
			return false
		}
		client.quicConn = nil
		return true
	})

	// Like a closed mux session: free the subdomains and ports right away
	// instead of holding them for the inactivity timeout (a reconnecting
	// client without a fixed --id was refused with 409 meanwhile). A
	// connection already replaced by a newer one must not tear down the
	// registration its successor is using.
	if peerGone && current {
		closeServerClientConnections(clientID, client)
		log.Printf("QUIC connection closed for client %s, resources released", clientID)
	}
}

// isUpgradeRequest reports whether r asks to switch protocols, i.e. carries
// an Upgrade header and lists "upgrade" among its Connection tokens.
func isUpgradeRequest(r *http.Request) bool {
	// A handshake carries no body (RFC 6455 4.1); anything after its head is
	// held back until the upgrade is confirmed, so a body cannot be relayed.
	if r.Header.Get("Upgrade") == "" || r.ContentLength != 0 || len(r.TransferEncoding) > 0 {
		return false
	}
	for _, value := range r.Header.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
				return true
			}
		}
	}
	return false
}

// upgradeHandshakeTimeout bounds the wait for upstream's answer to an
// upgrade; the visitor relay that would notice a disconnect starts only after.
var upgradeHandshakeTimeout = 30 * time.Second

// maxInformationalResponses bounds the 1xx responses relayed before the final
// answer to an upgrade request.
const maxInformationalResponses = 8

// relayUpgradeResponse forwards the upstream answer to an upgrade request and
// reports whether the socket may become a raw bidirectional pipe. Only a 101
// leaves HTTP; any other final answer is relayed with Connection: close,
// because a reusable socket would carry later requests past Basic Auth,
// capability checks and header injection. Informational responses such as
// 103 Early Hints are relayed while waiting for the final answer.
func relayUpgradeResponse(conn net.Conn, upstream io.Reader, r *http.Request) bool {
	if d, ok := upstream.(interface{ SetReadDeadline(time.Time) error }); ok {
		_ = d.SetReadDeadline(time.Now().Add(upgradeHandshakeTimeout))
		defer d.SetReadDeadline(time.Time{})
	}
	br := bufio.NewReader(upstream)
	for i := 0; i <= maxInformationalResponses; i++ {
		resp, err := http.ReadResponse(br, r)
		if err != nil {
			return false
		}
		if resp.StatusCode == http.StatusSwitchingProtocols {
			if writeResponseHead(conn, resp) != nil {
				return false
			}
			// Frames upstream sent right behind the 101 are already buffered;
			// later bytes are copied straight from upstream.
			early, _ := br.Peek(br.Buffered())
			_, err := conn.Write(early)
			return err == nil
		}
		if resp.StatusCode >= 100 && resp.StatusCode < 200 {
			if writeResponseHead(conn, resp) != nil {
				return false
			}
			continue
		}
		defer resp.Body.Close()
		resp.Close = true
		_ = resp.Write(conn)
		return false
	}
	return false
}

// writeResponseHead writes a bodiless response's status line and headers.
func writeResponseHead(w io.Writer, resp *http.Response) error {
	var head bytes.Buffer
	fmt.Fprintf(&head, "HTTP/%d.%d %s\r\n", resp.ProtoMajor, resp.ProtoMinor, resp.Status)
	_ = resp.Header.Write(&head)
	head.WriteString("\r\n")
	_, err := w.Write(head.Bytes())
	return err
}

// completeUpgrade relays the upstream answer and, once it is 101, forwards
// the bytes the visitor sent after the handshake.
func completeUpgrade(conn net.Conn, upstream io.ReadWriter, r *http.Request, afterUpgrade []byte) bool {
	if !relayUpgradeResponse(conn, upstream, r) {
		return false
	}
	if len(afterUpgrade) > 0 {
		if _, err := upstream.Write(afterUpgrade); err != nil {
			return false
		}
	}
	return true
}

// handleHijackedProxyConnection handles a hijacked HTTP connection for proxying
func handleHijackedProxyConnection(conn net.Conn, buf *bufio.ReadWriter, r *http.Request, client *serverClientInfo, tunnelName string, basicAuth string, customHeaders map[string]string) {
	defer conn.Close()

	client.SetLastActivity(time.Now())

	// Get client IP for logging
	clientIP := conn.RemoteAddr().String()
	if host, _, err := net.SplitHostPort(clientIP); err == nil {
		clientIP = host
	}

	upgrade := isUpgradeRequest(r)

	// Reconstruct the HTTP request
	var reqBuf bytes.Buffer
	reqBuf.WriteString(fmt.Sprintf("%s %s %s\r\n", r.Method, r.URL.RequestURI(), r.Proto))
	// Add Host header (stored in r.Host, not r.Header in Go)
	if r.Host != "" {
		reqBuf.WriteString(fmt.Sprintf("Host: %s\r\n", r.Host))
	}
	for key, values := range r.Header {
		// A hijacked socket is bound to the tunnel selected by the first Host.
		// Reusing it for another request could bypass the main-domain router and
		// its control-plane policy, so every HTTP proxy exchange is single-use.
		if strings.EqualFold(key, "Connection") || strings.EqualFold(key, "Proxy-Connection") {
			continue
		}
		// Forwarding headers are set by the server below; visitor-supplied
		// values must not be passed off as the server's.
		switch http.CanonicalHeaderKey(key) {
		case "X-Forwarded-For", "X-Real-Ip", "X-Forwarded-Proto", "X-Forwarded-Host":
			continue
		}
		for _, value := range values {
			reqBuf.WriteString(fmt.Sprintf("%s: %s\r\n", key, value))
		}
	}
	// Tell the app who the visitor is. The visitor IP is appended to any
	// existing X-Forwarded-For chain; only its last element is trustworthy.
	xff := clientIP
	if prior := r.Header.Values("X-Forwarded-For"); len(prior) > 0 {
		xff = strings.Join(prior, ", ") + ", " + clientIP
	}
	reqBuf.WriteString("X-Forwarded-For: " + xff + "\r\n")
	reqBuf.WriteString("X-Real-IP: " + clientIP + "\r\n")
	reqBuf.WriteString("X-Forwarded-Proto: https\r\n")
	if r.Host != "" {
		reqBuf.WriteString("X-Forwarded-Host: " + r.Host + "\r\n")
	}
	// Protocol upgrades (WebSocket) leave HTTP after the 101 response, so no
	// further request can reuse the socket; keep their Connection token.
	if upgrade {
		reqBuf.WriteString("Connection: Upgrade\r\n")
	} else {
		reqBuf.WriteString("Connection: close\r\n")
	}
	reqBuf.WriteString("\r\n")

	// Read any buffered body data. For an upgrade, bytes after the handshake
	// are withheld until upstream answers 101: a pipelined request sent with
	// it would otherwise reach upstream unchecked.
	var afterUpgrade []byte
	if buf.Reader.Buffered() > 0 {
		buffered := make([]byte, buf.Reader.Buffered())
		buf.Read(buffered)
		if upgrade {
			afterUpgrade = buffered
		} else {
			reqBuf.Write(buffered)
		}
	}

	initialData := reqBuf.Bytes()

	// Log the request
	logRequest(initialData, clientIP, tunnelName)

	// Check Basic Auth if required
	if basicAuth != "" {
		authOK := verifyBasicAuth(initialData, basicAuth)
		if !authOK {
			log.Printf("Basic Auth failed for tunnel %s from %s", tunnelName, clientIP)
			response := "HTTP/1.1 401 Unauthorized\r\n" +
				"WWW-Authenticate: Basic realm=\"Protected\"\r\n" +
				"Content-Type: text/plain\r\n" +
				"Content-Length: 12\r\n" +
				"Connection: close\r\n" +
				"\r\n" +
				"Unauthorized"
			conn.Write([]byte(response))
			return
		}
	}

	// Inject custom headers
	if len(customHeaders) > 0 {
		initialData = injectHeaders(initialData, customHeaders)
	}

	client.muxSessionMutex.Lock()
	session := client.muxSession
	client.muxSessionMutex.Unlock()

	if session != nil && !session.IsClosed() {
		stream, err := session.Open()
		if err != nil {
			log.Printf("Failed to open yamux stream: %v", err)
			return
		}

		// Send tunnel name header if multi-tunnel mode
		if len(client.tunnelMap) > 0 && tunnelName != "" {
			nameBytes := []byte(tunnelName)
			if len(nameBytes) > 255 {
				nameBytes = nameBytes[:255]
			}
			header := make([]byte, 1+len(nameBytes))
			header[0] = byte(len(nameBytes))
			copy(header[1:], nameBytes)
			if _, err := stream.Write(header); err != nil {
				log.Printf("Failed to write tunnel header: %v", err)
				stream.Close()
				return
			}
			srvBytesSent.Add(float64(len(header)))
		}

		// Write initial request data
		if len(initialData) > 0 {
			if _, err := stream.Write(initialData); err != nil {
				log.Printf("Failed to write initial data: %v", err)
				stream.Close()
				return
			}
			srvBytesSent.Add(float64(len(initialData)))
		}

		if upgrade && !completeUpgrade(conn, stream, r, afterUpgrade) {
			stream.Close()
			return
		}
		handleMuxProxyConnection(conn, stream, client)
		return
	}

	// Try QUIC connection if mux session not available
	client.connMutex.Lock()
	quicConn := client.quicConn
	client.connMutex.Unlock()

	if quicConn != nil {
		stream, err := quicConn.OpenStream()
		if err != nil {
			log.Printf("Failed to open QUIC stream: %v", err)
			return
		}

		// Send tunnel name header if multi-tunnel mode
		if len(client.tunnelMap) > 0 && tunnelName != "" {
			nameBytes := []byte(tunnelName)
			if len(nameBytes) > 255 {
				nameBytes = nameBytes[:255]
			}
			header := make([]byte, 1+len(nameBytes))
			header[0] = byte(len(nameBytes))
			copy(header[1:], nameBytes)
			if _, err := (*stream).Write(header); err != nil {
				log.Printf("Failed to write tunnel header to QUIC: %v", err)
				(*stream).Close()
				return
			}
			srvBytesSent.Add(float64(len(header)))
		}

		// Write initial request data
		if len(initialData) > 0 {
			if _, err := (*stream).Write(initialData); err != nil {
				log.Printf("Failed to write initial data to QUIC: %v", err)
				(*stream).Close()
				return
			}
			srvBytesSent.Add(float64(len(initialData)))
		}

		if upgrade && !completeUpgrade(conn, stream, r, afterUpgrade) {
			(*stream).Close()
			return
		}
		handleQUICProxyConnection(conn, stream, client)
		return
	}

	log.Printf("No mux or QUIC session available for client %s", client.UDID)
}

// startReverseListener starts a listener for reverse port forwarding
func startReverseListener(ctx context.Context, client *serverClientInfo, remoteAddr, localTarget string) {
	// Normalize address: if it's just a port number, add the colon prefix
	if !strings.Contains(remoteAddr, ":") {
		remoteAddr = ":" + remoteAddr
	}

	// The previous registration of a reconnecting client releases this port
	// asynchronously; retry briefly instead of failing the bind for good.
	var listener net.Listener
	var err error
	for attempt := 0; ; attempt++ {
		listener, err = net.Listen("tcp", remoteAddr)
		if err == nil || !errors.Is(err, syscall.EADDRINUSE) || attempt >= 50 {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	if err != nil {
		log.Printf("Failed to start reverse listener on %s: %v", remoteAddr, err)
		return
	}

	// Store listener for cleanup
	client.connMutex.Lock()
	client.reverseListeners = append(client.reverseListeners, listener)
	client.connMutex.Unlock()

	defer func() {
		listener.Close()
		// Remove from list
		client.connMutex.Lock()
		for i, l := range client.reverseListeners {
			if l == listener {
				client.reverseListeners = append(client.reverseListeners[:i], client.reverseListeners[i+1:]...)
				break
			}
		}
		client.connMutex.Unlock()
	}()

	go func() {
		<-ctx.Done()
		listener.Close()
	}()

	log.Printf("Reverse listener started on %s for client %s", remoteAddr, client.UDID)

	var backoff acceptBackoff
	for {
		conn, err := listener.Accept()
		if err != nil {
			if !backoff.retry(ctx, err) {
				return
			}
			log.Printf("Reverse listener accept error: %v", err)
			continue
		}
		backoff.reset()

		go handleReverseConnection(ctx, conn, client, localTarget)
	}
}

// handleReverseConnection handles a single reverse forwarding connection
func handleReverseConnection(ctx context.Context, conn net.Conn, client *serverClientInfo, localTarget string) {
	defer conn.Close()

	client.muxSessionMutex.Lock()
	session := client.muxSession
	client.muxSessionMutex.Unlock()

	if session == nil || session.IsClosed() {
		log.Printf("No mux session available for reverse forwarding")
		return
	}

	// Open stream to client
	stream, err := session.Open()
	if err != nil {
		log.Printf("Failed to open mux stream for reverse forwarding: %v", err)
		return
	}
	defer stream.Close()

	// Send reverse forwarding header: 0xFD (marker) + length + local target
	targetBytes := []byte(localTarget)
	if len(targetBytes) > 255 {
		targetBytes = targetBytes[:255]
	}
	header := make([]byte, 2+len(targetBytes))
	header[0] = 0xFD // Reverse forwarding marker
	header[1] = byte(len(targetBytes))
	copy(header[2:], targetBytes)

	if _, err := stream.Write(header); err != nil {
		log.Printf("Failed to write reverse forwarding header: %v", err)
		return
	}

	client.SetLastActivity(time.Now())

	// Bridge the connections
	done := make(chan struct{}, 2)

	go func() {
		io.Copy(stream, conn)
		done <- struct{}{}
	}()

	go func() {
		io.Copy(conn, stream)
		done <- struct{}{}
	}()

	select {
	case <-done:
	case <-ctx.Done():
	}
}

func waitForProxyConnectionNoTLS(ctx context.Context, client *serverClientInfo, proxyPort int) {
	log.Printf("Initializing non-TLS proxy listener on port %d for client %s", proxyPort, client.UDID)

	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", proxyPort))
	if err != nil {
		log.Printf("Error initializing non-TLS listener on port %d: %v", proxyPort, err)
		return
	}

	client.listenerNoTLS = listener
	defer func() {
		if err := listener.Close(); err != nil {
			log.Printf("Error closing non-TLS listener on port %d: %v", proxyPort, err)
		}
	}()

	log.Printf("Non-TLS proxy server listening on port %d for client %s", proxyPort, client.UDID)

	acceptServerConnections(ctx, listener, client, handleNewProxyConnectionNoTLS)
}

func acceptServerConnections(ctx context.Context, listener net.Listener, client *serverClientInfo, handleFunc func(net.Conn, *serverClientInfo)) {
	log.Printf("Starting to accept connections on port %d", listener.Addr().(*net.TCPAddr).Port)
	var wg sync.WaitGroup
	var backoff acceptBackoff
	// Accept blocks, so the ctx check below never runs while idle; close the
	// listener on cancel so the port is actually unbound.
	go func() {
		<-ctx.Done()
		listener.Close()
	}()

	for {
		select {
		case <-ctx.Done():
			log.Printf("Shutting down listener on port %d", listener.Addr().(*net.TCPAddr).Port)
			if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				log.Printf("Error while closing listener: %v", err)
			}
			wg.Wait()
			return

		default:
			proxyConn, err := listener.Accept()
			if err != nil {
				if !backoff.retry(ctx, err) {
					return
				}
				continue
			}
			backoff.reset()

			log.Printf("Accepted connection for client %s", client.UDID)
			wg.Add(1)
			go func() {
				defer wg.Done()
				handleFunc(proxyConn, client)
			}()
		}
	}
}

func handleNewProxyConnectionTLS(proxyConn net.Conn, client *serverClientInfo) {
	handleNewProxyConnectionTLSWithTunnel(proxyConn, client, "", "", nil)
}

func handleNewProxyConnectionTLSWithTunnel(proxyConn net.Conn, client *serverClientInfo, tunnelName string, basicAuth string, customHeaders map[string]string) {
	defer proxyConn.Close()

	client.SetLastActivity(time.Now())

	// Get client IP for logging
	clientIP := proxyConn.RemoteAddr().String()
	if host, _, err := net.SplitHostPort(clientIP); err == nil {
		clientIP = host
	}

	// Check Basic Auth if configured (this reads the request)
	var initialData []byte
	needReadRequest := basicAuth != "" || len(customHeaders) > 0 || serverRequestLog

	if needReadRequest {
		// Read the HTTP request for auth check, header injection, and logging
		buf := make([]byte, 4096)
		proxyConn.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := proxyConn.Read(buf)
		if err != nil {
			log.Printf("Failed to read request: %v", err)
			return
		}
		proxyConn.SetReadDeadline(time.Time{})
		initialData = buf[:n]

		// Log the request
		logRequest(initialData, clientIP, tunnelName)

		// Check Basic Auth if required
		if basicAuth != "" {
			authOK := verifyBasicAuth(initialData, basicAuth)
			if !authOK {
				log.Printf("Basic Auth failed for tunnel %s from %s", tunnelName, clientIP)
				// Send 401 response
				response := "HTTP/1.1 401 Unauthorized\r\n" +
					"WWW-Authenticate: Basic realm=\"Protected\"\r\n" +
					"Content-Type: text/plain\r\n" +
					"Content-Length: 12\r\n" +
					"Connection: close\r\n" +
					"\r\n" +
					"Unauthorized"
				n, err := proxyConn.Write([]byte(response))
				log.Printf("Sent 401 response: %d bytes, err: %v", n, err)
				return
			}
		}

		// Inject custom headers
		if len(customHeaders) > 0 {
			initialData = injectHeaders(initialData, customHeaders)
		}
	}

	client.muxSessionMutex.Lock()
	session := client.muxSession
	client.muxSessionMutex.Unlock()

	if session != nil && !session.IsClosed() {
		stream, err := session.Open()
		if err != nil {
			log.Printf("Failed to open yamux stream: %v", err)
		} else {
			// Send tunnel name header if multi-tunnel mode
			if len(client.tunnelMap) > 0 && tunnelName != "" {
				// Write tunnel name as: 1 byte length + name bytes
				nameBytes := []byte(tunnelName)
				if len(nameBytes) > 255 {
					nameBytes = nameBytes[:255]
				}
				header := make([]byte, 1+len(nameBytes))
				header[0] = byte(len(nameBytes))
				copy(header[1:], nameBytes)
				if _, err := stream.Write(header); err != nil {
					log.Printf("Failed to write tunnel header: %v", err)
					stream.Close()
					return
				}
			}
			// Write initial data that was read for auth check
			if len(initialData) > 0 {
				if _, err := stream.Write(initialData); err != nil {
					log.Printf("Failed to write initial data: %v", err)
					stream.Close()
					return
				}
			}
			handleMuxProxyConnection(proxyConn, stream, client)
			return
		}
	}

	connectionID := uuid.New().String()

	message := map[string]string{
		"type":         "new_connection",
		"connectionID": connectionID,
		"useTLS":       "true",
	}

	// Add tunnel name to message for WebSocket fallback
	if tunnelName != "" {
		message["tunnelName"] = tunnelName
	}

	ch := make(chan *websocket.Conn)

	client.connMutex.Lock()
	client.wsConnChans[connectionID] = ch
	client.connMutex.Unlock()

	if client.conn == nil {
		log.Printf("WebSocket connection for client %s is not established", client.UDID)
		client.connMutex.Lock()
		delete(client.wsConnChans, connectionID)
		client.connMutex.Unlock()
		return
	}

	client.connWriteMutex.Lock()
	err := client.conn.WriteJSON(message)
	client.connWriteMutex.Unlock()
	if err != nil {
		log.Printf("Failed to send new_connection message: %v", err)
		client.connMutex.Lock()
		delete(client.wsConnChans, connectionID)
		client.connMutex.Unlock()
		return
	}

	select {
	case wsConn := <-ch:
		if wsConn == nil {
			client.connMutex.Lock()
			delete(client.wsConnChans, connectionID)
			client.connMutex.Unlock()
			return
		}
		handleProxyWebSocketConnection(proxyConn, wsConn, client)
	case <-time.After(30 * time.Second):
		log.Printf("Timeout waiting for client WebSocket connection")
		client.connMutex.Lock()
		delete(client.wsConnChans, connectionID)
		client.connMutex.Unlock()
	}
}

func handleNewProxyConnectionNoTLS(proxyConn net.Conn, client *serverClientInfo) {
	connectionID := uuid.New().String()
	defer proxyConn.Close()

	log.Printf("Setting up new non-TLS proxy connection with ID: %s", connectionID)

	ch := make(chan *websocket.Conn)

	client.connMutex.Lock()
	if client.conn == nil {
		client.connMutex.Unlock()
		log.Printf("WebSocket connection for client %s is not established", client.UDID)
		return
	}

	client.wsConnChans[connectionID] = ch
	client.connMutex.Unlock()

	client.connWriteMutex.Lock()
	err := client.conn.WriteJSON(map[string]string{
		"type":         "new_connection",
		"connectionID": connectionID,
		"useTLS":       "false",
	})
	client.connWriteMutex.Unlock()

	if err != nil {
		log.Printf("Failed to send new_connection message: %v", err)
		client.connMutex.Lock()
		delete(client.wsConnChans, connectionID)
		client.connMutex.Unlock()
		return
	}

	select {
	case wsConn := <-ch:
		if wsConn == nil {
			client.connMutex.Lock()
			delete(client.wsConnChans, connectionID)
			client.connMutex.Unlock()
			return
		}
		handleProxyWebSocketConnection(proxyConn, wsConn, client)
		client.connMutex.Lock()
		delete(client.wsConnChans, connectionID)
		client.connMutex.Unlock()
	case <-time.After(60 * time.Second):
		client.connMutex.Lock()
		delete(client.wsConnChans, connectionID)
		client.connMutex.Unlock()
	}
}

func handleProxyWebSocketConnection(proxyConn net.Conn, wsConn *websocket.Conn, client *serverClientInfo) {
	timer := newSrvConnectionTimer()
	defer timer.observe()

	srvTotalConnections.Inc()
	srvActiveProxyConnections.Inc()
	defer srvActiveProxyConnections.Dec()

	defer wsConn.Close()
	defer proxyConn.Close()

	done := make(chan struct{}, 2)
	errChan := make(chan error, 2)

	go func() {
		bufPtr := serverBufferPool.Get().(*[]byte)
		buf := *bufPtr
		defer serverBufferPool.Put(bufPtr)

		for {
			n, err := wsConn.UnderlyingConn().Read(buf)
			if n > 0 {
				srvBytesReceived.Add(float64(n))
				if _, writeErr := proxyConn.Write(buf[:n]); writeErr != nil {
					srvConnectionErrors.WithLabelValues("write_proxy").Inc()
					errChan <- writeErr
					return
				}
				client.SetLastActivity(time.Now())
			}
			if err != nil {
				if err != io.EOF {
					srvConnectionErrors.WithLabelValues("read_ws").Inc()
					errChan <- err
				}
				break
			}
		}
		done <- struct{}{}
	}()

	go func() {
		bufPtr := serverBufferPool.Get().(*[]byte)
		buf := *bufPtr
		defer serverBufferPool.Put(bufPtr)

		for {
			n, err := proxyConn.Read(buf)
			if n > 0 {
				srvBytesSent.Add(float64(n))
				if _, writeErr := wsConn.UnderlyingConn().Write(buf[:n]); writeErr != nil {
					srvConnectionErrors.WithLabelValues("write_ws").Inc()
					errChan <- writeErr
					return
				}
				client.SetLastActivity(time.Now())
			}
			if err != nil {
				if err != io.EOF {
					srvConnectionErrors.WithLabelValues("read_proxy").Inc()
					errChan <- err
				}
				break
			}
		}
		done <- struct{}{}
	}()

	select {
	case <-done:
	case <-errChan:
	}
}

func handleMuxProxyConnection(proxyConn net.Conn, stream net.Conn, client *serverClientInfo) {
	timer := newSrvConnectionTimer()
	defer timer.observe()

	srvTotalConnections.Inc()
	srvActiveProxyConnections.Inc()
	defer srvActiveProxyConnections.Dec()

	defer stream.Close()
	defer proxyConn.Close()

	done := make(chan struct{}, 2)
	errChan := make(chan error, 2)

	go func() {
		bufPtr := serverBufferPool.Get().(*[]byte)
		buf := *bufPtr
		defer serverBufferPool.Put(bufPtr)

		for {
			n, err := stream.Read(buf)
			if n > 0 {
				srvBytesReceived.Add(float64(n))
				if _, writeErr := proxyConn.Write(buf[:n]); writeErr != nil {
					srvConnectionErrors.WithLabelValues("mux_write_proxy").Inc()
					errChan <- writeErr
					return
				}
				client.SetLastActivity(time.Now())
			}
			if err != nil {
				if err != io.EOF {
					srvConnectionErrors.WithLabelValues("mux_read_stream").Inc()
					errChan <- err
				}
				break
			}
		}
		done <- struct{}{}
	}()

	go func() {
		bufPtr := serverBufferPool.Get().(*[]byte)
		buf := *bufPtr
		defer serverBufferPool.Put(bufPtr)

		for {
			n, err := proxyConn.Read(buf)
			if n > 0 {
				srvBytesSent.Add(float64(n))
				if _, writeErr := stream.Write(buf[:n]); writeErr != nil {
					srvConnectionErrors.WithLabelValues("mux_write_stream").Inc()
					errChan <- writeErr
					return
				}
				client.SetLastActivity(time.Now())
			}
			if err != nil {
				if err != io.EOF {
					srvConnectionErrors.WithLabelValues("mux_read_proxy").Inc()
					errChan <- err
				}
				break
			}
		}
		done <- struct{}{}
	}()

	select {
	case <-done:
	case <-errChan:
	}
}

func handleQUICProxyConnection(proxyConn net.Conn, stream *quic.Stream, client *serverClientInfo) {
	timer := newSrvConnectionTimer()
	defer timer.observe()

	srvTotalConnections.Inc()
	srvActiveProxyConnections.Inc()
	defer srvActiveProxyConnections.Dec()

	defer (*stream).Close()
	defer proxyConn.Close()

	done := make(chan struct{}, 2)
	errChan := make(chan error, 2)

	go func() {
		bufPtr := serverBufferPool.Get().(*[]byte)
		buf := *bufPtr
		defer serverBufferPool.Put(bufPtr)

		for {
			n, err := (*stream).Read(buf)
			if n > 0 {
				srvBytesReceived.Add(float64(n))
				if _, writeErr := proxyConn.Write(buf[:n]); writeErr != nil {
					srvConnectionErrors.WithLabelValues("quic_write_proxy").Inc()
					errChan <- writeErr
					return
				}
				client.SetLastActivity(time.Now())
			}
			if err != nil {
				if err != io.EOF {
					srvConnectionErrors.WithLabelValues("quic_read_stream").Inc()
					errChan <- err
				}
				break
			}
		}
		done <- struct{}{}
	}()

	go func() {
		bufPtr := serverBufferPool.Get().(*[]byte)
		buf := *bufPtr
		defer serverBufferPool.Put(bufPtr)

		for {
			n, err := proxyConn.Read(buf)
			if n > 0 {
				srvBytesSent.Add(float64(n))
				if _, writeErr := (*stream).Write(buf[:n]); writeErr != nil {
					srvConnectionErrors.WithLabelValues("quic_write_stream").Inc()
					errChan <- writeErr
					return
				}
				client.SetLastActivity(time.Now())
			}
			if err != nil {
				if err != io.EOF {
					srvConnectionErrors.WithLabelValues("quic_read_proxy").Inc()
					errChan <- err
				}
				break
			}
		}
		done <- struct{}{}
	}()

	select {
	case <-done:
	case <-errChan:
	}
}

const (
	srvPongWait   = 60 * time.Second
	srvPingPeriod = (srvPongWait * 9) / 10
)

func waitForServerClientConnection(ctx context.Context, client *serverClientInfo, clientPort int, useTLS bool, udid string) {
	mux := http.NewServeMux()

	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" {
			http.Error(w, "400 Bad Request", http.StatusBadRequest)
			return
		}

		select {
		case serverConnLimiter <- struct{}{}:
			defer func() { <-serverConnLimiter }()
		default:
			http.Error(w, "Server too busy", http.StatusServiceUnavailable)
			return
		}

		wsConn, err := serverUpgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("Failed to upgrade to WebSocket: %v", err)
			return
		}
		srvWsUpgrades.Inc()

		wsConn.SetReadDeadline(time.Now().Add(srvPongWait))

		var pingTimeMutex sync.Mutex
		var lastPingTime time.Time

		wsConn.SetPongHandler(func(string) error {
			wsConn.SetReadDeadline(time.Now().Add(srvPongWait))
			client.SetLastActivity(time.Now())

			pingTimeMutex.Lock()
			if !lastPingTime.IsZero() {
				latency := time.Since(lastPingTime).Seconds()
				srvWsPingPongLatency.Observe(latency)
			}
			pingTimeMutex.Unlock()

			return nil
		})

		client.connMutex.Lock()
		client.conn = wsConn
		client.connMutex.Unlock()
		client.SetLastActivity(time.Now())

		go func() {
			ticker := time.NewTicker(srvPingPeriod)
			defer ticker.Stop()

			disconnected := make(chan struct{})

			go func() {
				for {
					_, _, err := wsConn.ReadMessage()
					if err != nil {
						close(disconnected)
						return
					}
				}
			}()

			for {
				select {
				case <-ticker.C:
					pingTimeMutex.Lock()
					lastPingTime = time.Now()
					pingTimeMutex.Unlock()

					client.connWriteMutex.Lock()
					err := wsConn.WriteMessage(websocket.PingMessage, nil)
					client.connWriteMutex.Unlock()
					if err != nil {
						wsConn.Close()
						return
					}
				case <-ctx.Done():
					wsConn.Close()
					return
				case <-disconnected:
					return
				}
			}
		}()

		for {
			_, _, err := wsConn.ReadMessage()
			if err != nil {
				wsConn.Close()
				return
			}
		}
	})

	mux.HandleFunc("/client_ws", serverRateLimitMiddleware(serverClientWebSocketHandler))
	mux.HandleFunc("/mux", serverRateLimitMiddleware(serverMuxWebSocketHandler))

	server := &http.Server{
		Addr:         fmt.Sprintf(":%d", clientPort),
		Handler:      mux,
		ReadTimeout:  5 * time.Minute,
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  5 * time.Minute,
	}

	if useTLS {
		client.clientListenerTLS = server
	} else {
		client.clientListenerNoTLS = server
	}

	go func() {
		if useTLS {
			certs, err := serverCertReloader()
			if err != nil {
				log.Printf("Failed to load certificate for client port %d: %v", clientPort, err)
				return
			}
			server.TLSConfig = &tls.Config{GetCertificate: certs.GetCertificate, MinVersion: tls.VersionTLS12}
			log.Printf("Secure WebSocket server listening on port %d", clientPort)
			if err := server.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("Failed to start TLS server on port %d: %v", clientPort, err)
			}
		} else {
			log.Printf("WebSocket server listening on port %d", clientPort)
			if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("Failed to start server on port %d: %v", clientPort, err)
			}
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	server.Shutdown(shutdownCtx)
}

func saveServerState() {
	// Registrations save concurrently; the snapshot+write+rename must not
	// interleave on the shared temp file.
	serverStateSaveMu.Lock()
	defer serverStateSaveMu.Unlock()
	state := struct {
		Clients   map[string]serializableClientInfoForSave
		UsedPorts map[int]bool
	}{
		Clients:   make(map[string]serializableClientInfoForSave),
		UsedPorts: make(map[int]bool),
	}

	for id, client := range legacyClientRegistry.Snapshot() {
		state.Clients[id] = serializableClientInfoForSave{
			UDID:            client.UDID,
			PortNoTLS:       client.PortNoTLS,
			ClientPortTLS:   client.ClientPortTLS,
			ClientPortNoTLS: client.ClientPortNoTLS,
			LastActivity:    client.GetLastActivity(),
			ConnectionType:  client.ConnectionType,
			ConnectionID:    client.ConnectionID,
			Registered:      client.Registered,
		}
	}

	serverPortMutex.Lock()
	for port, used := range serverUsedPorts {
		state.UsedPorts[port] = used
	}
	serverPortMutex.Unlock()

	// Write to a temp file and rename so a crash mid-write never leaves a
	// truncated state file behind.
	tmp := serverStateFile + ".tmp"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		log.Printf("Failed to create state file: %v", err)
		return
	}
	if err := gob.NewEncoder(file).Encode(state); err != nil {
		log.Printf("Failed to encode state: %v", err)
		file.Close()
		os.Remove(tmp)
		return
	}
	if err := file.Close(); err != nil {
		log.Printf("Failed to write state file: %v", err)
		os.Remove(tmp)
		return
	}
	if err := os.Rename(tmp, serverStateFile); err != nil {
		log.Printf("Failed to replace state file: %v", err)
	}
}

func loadServerState() {
	file, err := os.Open(serverStateFile)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("Failed to open state file: %v", err)
		}
		return
	}
	defer file.Close()

	state := struct {
		Clients   map[string]serializableClientInfoForSave
		UsedPorts map[int]bool
	}{}

	decoder := gob.NewDecoder(file)
	if err := decoder.Decode(&state); err != nil {
		log.Printf("Failed to decode state: %v", err)
		return
	}

	for id, clientData := range state.Clients {
		if !serverLegacyListeners && clientData.ConnectionType == "non-tls" {
			continue // disabled on this server; do not reopen its port
		}
		client := &serverClientInfo{
			serializableClientInfo: serializableClientInfo{
				UDID:            clientData.UDID,
				PortNoTLS:       clientData.PortNoTLS,
				ClientPortTLS:   clientData.ClientPortTLS,
				ClientPortNoTLS: clientData.ClientPortNoTLS,
				ConnectionType:  clientData.ConnectionType,
				ConnectionID:    clientData.ConnectionID,
				Registered:      clientData.Registered,
			},
			wsConnChans:    make(map[string]chan *websocket.Conn),
			p2pConnections: make(map[string]net.Conn),
		}
		client.SetLastActivity(clientData.LastActivity)
		legacyClientRegistry.Put(id, client)
		srvActiveClients.Inc() // Count restored clients

		ctx, cancel := context.WithCancel(context.Background())
		client.cancelFunc = cancel
		client.ctx = ctx

		if serverLegacyListeners {
			serverWg.Add(1)
			go func(ctx context.Context, client *serverClientInfo, id string) {
				defer serverWg.Done()
				waitForServerClientConnection(ctx, client, client.ClientPortTLS, true, id)
			}(ctx, client, id)
		}
		// A restored registration whose client never comes back must not
		// keep its subdomains. Legacy clients attach through /ws, which the
		// reaper cannot see, so it only runs when legacy listeners are off.
		if !serverLegacyListeners {
			go reapUnattachedRegistration(ctx, id, client)
		}

		if client.ConnectionType == "non-tls" && serverLegacyListeners {
			serverWg.Add(1)
			go func(ctx context.Context, client *serverClientInfo) {
				defer serverWg.Done()
				waitForProxyConnectionNoTLS(ctx, client, client.PortNoTLS)
			}(ctx, client)
		}

		serverWg.Add(1)
		go func(id string, client *serverClientInfo) {
			defer serverWg.Done()
			monitorServerClientActivity(ctx, id, client)
		}(id, client)
	}
	serverPortMutex.Lock()
	if serverLegacyListeners {
		for port, used := range state.UsedPorts {
			serverUsedPorts[port] = used
		}
	}
	serverPortMutex.Unlock()
}

func closeServerShutdown(shutdown chan struct{}) {
	serverShutdownMu.Lock()
	defer serverShutdownMu.Unlock()
	select {
	case <-shutdown:
	default:
		close(shutdown)
	}
}

func gracefulServerShutdown(shutdown chan struct{}) {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigChan)

	var sig os.Signal
	select {
	case sig = <-sigChan:
	case <-shutdown:
		return
	}
	log.Printf("Received signal %v, initiating graceful shutdown...", sig)

	closeServerShutdown(shutdown)
}

func serverMuxWebSocketHandler(w http.ResponseWriter, r *http.Request) {
	clientID := r.URL.Query().Get("clientID")
	if clientID == "" {
		http.Error(w, "Missing clientID", http.StatusBadRequest)
		return
	}

	client, exists := legacyClientRegistry.Get(clientID)
	if !exists {
		http.Error(w, "Unknown client", http.StatusNotFound)
		return
	}
	sessionToken := r.Header.Get("X-Xrok-Session")
	if sessionToken == "" {
		sessionToken = r.URL.Query().Get("session_token")
	}
	if !client.validSessionToken(sessionToken) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	wsConn, err := serverUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Failed to upgrade mux WebSocket: %v", err)
		return
	}
	srvWsUpgrades.Inc()

	config := yamux.DefaultConfig()
	config.AcceptBacklog = 256
	config.EnableKeepAlive = true
	config.KeepAliveInterval = 30 * time.Second

	session, err := yamux.Server(wsConn.UnderlyingConn(), config)
	if err != nil {
		log.Printf("Failed to create yamux session: %v", err)
		wsConn.Close()
		return
	}

	displaced, ok := claimServerAttach(client, func() io.Closer {
		client.muxSessionMutex.Lock()
		defer client.muxSessionMutex.Unlock()
		old := client.muxSession
		client.muxSession = session
		if old != nil && old != session {
			return old
		}
		return nil
	})
	if !ok {
		log.Printf("Mux: registration for client %s already expired", clientID)
		session.Close()
		return
	}
	if displaced != nil {
		displaced.Close() // its handler sees it is no longer current
	}

	// Whatever ends the registration (replacement, reaper, inactivity,
	// shutdown) also ends its session, so the client notices and
	// re-registers instead of holding a session whose routes are gone.
	if client.ctx != nil {
		go func() {
			select {
			case <-client.ctx.Done():
				session.Close()
			case <-session.CloseChan():
			}
		}()
	}

	log.Printf("Yamux session established for client %s", clientID)

	// Accept streams from client (for SOCKS5/dynamic connections). A
	// per-session cap bounds goroutines/FDs a single client can pin.
	go func() {
		slots := make(chan struct{}, serverMaxClientStreams)
		for {
			stream, err := session.Accept()
			if err != nil {
				if err != io.EOF {
					log.Printf("Error accepting stream from client %s: %v", clientID, err)
				}
				return
			}
			select {
			case slots <- struct{}{}:
			default:
				log.Printf("Client %s exceeded %d concurrent streams, rejecting", clientID, serverMaxClientStreams)
				stream.Close()
				continue
			}
			go func() {
				defer func() { <-slots }()
				handleClientInitiatedStream(stream, client)
			}()
		}
	}()

	<-session.CloseChan()

	current := releaseServerAttach(client, func() bool {
		client.muxSessionMutex.Lock()
		defer client.muxSessionMutex.Unlock()
		if client.muxSession != session {
			return false
		}
		client.muxSession = nil
		return true
	})
	if !current {
		// A newer session of the same registration took over; it owns
		// the routes and ports now.
		log.Printf("Superseded yamux session closed for client %s", clientID)
		return
	}

	// Release this registration's routes, entry and ports (stateless
	// behaviour) unless a re-registration already replaced it. Cancel the
	// registration context first so its per-client and -R listeners unbind
	// before the port numbers are handed out again.
	if forgetServerClient(clientID, client) {
		if client.cancelFunc != nil {
			client.cancelFunc()
		}
		releaseServerPorts(client.PortNoTLS, client.ClientPortTLS, client.ClientPortNoTLS)
	}

	client.decrementOnce.Do(func() {
		srvActiveClients.Dec()
	})
	log.Printf("Yamux session closed for client %s, resources released", clientID)
}

// handleClientInitiatedStream handles streams opened by the client (e.g., SOCKS5, UDP)
func handleClientInitiatedStream(stream net.Conn, client *serverClientInfo) {
	defer stream.Close()

	// Read first byte to determine stream type
	header := make([]byte, 2)
	if _, err := io.ReadFull(stream, header); err != nil {
		log.Printf("Failed to read stream header: %v", err)
		return
	}

	// Check for SOCKS5/dynamic connection marker
	if header[0] == 0xFF {
		// Dynamic TCP connection: header[1] is target length
		targetLen := int(header[1])
		if targetLen == 0 || targetLen > 255 {
			log.Printf("Invalid SOCKS5 target length: %d", targetLen)
			return
		}

		targetBuf := make([]byte, targetLen)
		if _, err := io.ReadFull(stream, targetBuf); err != nil {
			log.Printf("Failed to read SOCKS5 target: %v", err)
			return
		}

		target := string(targetBuf)
		if serverDebug {
			log.Printf("SOCKS5 dynamic connection to: %s", target)
		}

		// Connect to target under the egress policy (no loopback/private/metadata)
		targetConn, err := dialEgress("tcp", target, 30*time.Second)
		if err != nil {
			log.Printf("Failed to connect to SOCKS5 target %s: %v", target, err)
			return
		}
		defer targetConn.Close()

		client.SetLastActivity(time.Now())
		relayEgress(stream, targetConn, serverEgressIdleTimeout)
		return
	}

	// Check for UDP marker
	if header[0] == 0xFE {
		// UDP packet: header[1] is remote address length
		remoteAddrLen := int(header[1])
		if remoteAddrLen == 0 || remoteAddrLen > 255 {
			log.Printf("Invalid UDP remote address length: %d", remoteAddrLen)
			return
		}

		remoteAddrBuf := make([]byte, remoteAddrLen)
		if _, err := io.ReadFull(stream, remoteAddrBuf); err != nil {
			log.Printf("Failed to read UDP remote address: %v", err)
			return
		}

		remoteAddr := string(remoteAddrBuf)

		// Read packet length (2 bytes big-endian)
		packetLenBuf := make([]byte, 2)
		if _, err := io.ReadFull(stream, packetLenBuf); err != nil {
			log.Printf("Failed to read UDP packet length: %v", err)
			return
		}

		packetLen := int(packetLenBuf[0])<<8 | int(packetLenBuf[1])
		if packetLen > 65535 {
			log.Printf("Invalid UDP packet length: %d", packetLen)
			return
		}

		// Read packet data
		packetData := make([]byte, packetLen)
		if _, err := io.ReadFull(stream, packetData); err != nil {
			log.Printf("Failed to read UDP packet data: %v", err)
			return
		}

		if serverDebug {
			log.Printf("UDP packet to %s, %d bytes", remoteAddr, packetLen)
		}

		udpConn, err := dialEgress("udp", remoteAddr, 10*time.Second)
		if err != nil {
			log.Printf("Failed to dial UDP %s: %v", remoteAddr, err)
			return
		}
		defer udpConn.Close()

		// Send packet
		udpConn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := udpConn.Write(packetData); err != nil {
			log.Printf("Failed to send UDP packet: %v", err)
			return
		}

		client.SetLastActivity(time.Now())

		// Wait for response
		respBuf := make([]byte, 65535)
		udpConn.SetReadDeadline(time.Now().Add(30 * time.Second))
		n, err := udpConn.Read(respBuf)
		if err != nil {
			if serverDebug {
				log.Printf("UDP no response or error: %v", err)
			}
			return
		}

		// Send response back: length (2 bytes big-endian) + data
		respHeader := make([]byte, 2+n)
		respHeader[0] = byte(n >> 8)
		respHeader[1] = byte(n)
		copy(respHeader[2:], respBuf[:n])

		stream.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := stream.Write(respHeader); err != nil {
			log.Printf("Failed to send UDP response: %v", err)
		}
		return
	}

	// Unknown stream type - close it
	log.Printf("Unknown stream type marker: 0x%02X", header[0])
}

func serverClientWebSocketHandler(w http.ResponseWriter, r *http.Request) {
	connectionID := r.URL.Query().Get("connectionID")
	clientID := r.URL.Query().Get("clientID")
	if connectionID == "" || clientID == "" {
		http.Error(w, "Missing connectionID or clientID", http.StatusBadRequest)
		return
	}
	if _, ok := requirePrincipalClient(w, r, clientID); !ok {
		return
	}

	if r.Header.Get("Upgrade") != "websocket" {
		http.Error(w, "WebSocket upgrade required", http.StatusBadRequest)
		return
	}

	wsConn, err := serverUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Failed to upgrade to WebSocket: %v", err)
		return
	}
	srvWsUpgrades.Inc()

	client, exists := legacyClientRegistry.Get(clientID)
	if !exists {
		wsConn.Close()
		return
	}

	client.connMutex.Lock()
	ch, exists := client.wsConnChans[connectionID]
	client.connMutex.Unlock()
	if !exists {
		wsConn.Close()
		return
	}

	ch <- wsConn
	client.SetLastActivity(time.Now())
}

// handleP2PWebSocket handles P2P signaling WebSocket connections
func handleP2PWebSocket(w http.ResponseWriter, r *http.Request) {
	clientID := r.URL.Query().Get("client_id")
	if clientID == "" {
		http.Error(w, "client_id required", http.StatusBadRequest)
		return
	}
	var allowed bool
	clientID, allowed = requirePrincipalClient(w, r, clientID)
	if !allowed {
		return
	}

	client, exists := legacyClientRegistry.Get(clientID)

	if !exists {
		http.Error(w, "Client not found", http.StatusNotFound)
		return
	}

	// Upgrade to WebSocket
	conn, err := serverUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("P2P WebSocket upgrade failed: %v", err)
		return
	}
	conn.SetReadLimit(p2p.MaxSignalMessageSize)

	// Store connection
	client.p2pSignalMutex.Lock()
	if client.p2pSignalConn != nil {
		client.p2pSignalConn.Close()
	}
	client.p2pSignalConn = conn
	client.p2pSignalMutex.Unlock()

	log.Printf("P2P signaling WebSocket connected for client %s", clientID)

	// Handle incoming signals
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			log.Printf("P2P WebSocket read error for %s: %v", clientID, err)
			break
		}

		signal, err := p2p.DecodeSignal(message)
		if err != nil {
			log.Printf("P2P signal decode error: %v", err)
			continue
		}

		// Process and relay signal
		handleP2PSignalRelay(clientID, signal)
	}

	// Cleanup
	client.p2pSignalMutex.Lock()
	if client.p2pSignalConn == conn {
		client.p2pSignalConn = nil
	}
	client.p2pSignalMutex.Unlock()
	conn.Close()
}

// handleP2PSignalRelay relays P2P signals between clients
func handleP2PSignalRelay(fromClientID string, signal *p2p.SignalMessage) {
	signal.FromPeer = fromClientID

	switch signal.Type {
	case p2p.SignalTypeDiscover:
		// Client wants to discover another peer
		targetClient, exists := legacyClientRegistry.Get(signal.ToPeer)

		if !exists {
			sendP2PSignalToClient(fromClientID, p2p.NewFallbackSignal(
				"server", fromClientID, signal.SessionID, "peer not found"))
			return
		}

		if targetClient.p2pInfo == nil {
			sendP2PSignalToClient(fromClientID, p2p.NewFallbackSignal(
				"server", fromClientID, signal.SessionID, "peer has no P2P"))
			return
		}

		// Send peer info back to requester
		sendP2PSignalToClient(fromClientID, p2p.NewPeerInfoSignal(signal.ToPeer, targetClient.p2pInfo))

		// Also notify target that someone wants to connect
		fromClient, _ := legacyClientRegistry.Get(fromClientID)

		if fromClient != nil && fromClient.p2pInfo != nil {
			sendP2PSignalToClient(signal.ToPeer, p2p.NewPeerInfoSignal(fromClientID, fromClient.p2pInfo))
		}

	case p2p.SignalTypePunch, p2p.SignalTypeICEOffer, p2p.SignalTypeICEAnswer, p2p.SignalTypeICECandidate:
		// Relay punch signal to target
		sendP2PSignalToClient(signal.ToPeer, signal)

	case p2p.SignalTypeReady:
		// Relay ready signal to target
		sendP2PSignalToClient(signal.ToPeer, signal)
		log.Printf("P2P connection ready between %s and %s", fromClientID, signal.ToPeer)

	case p2p.SignalTypeFallback:
		// Relay fallback signal
		sendP2PSignalToClient(signal.ToPeer, signal)
		log.Printf("P2P fallback for %s <-> %s: %s", fromClientID, signal.ToPeer, signal.Error)
	}
}

// sendP2PSignalToClient sends a P2P signal to a specific client
func sendP2PSignalToClient(clientID string, signal *p2p.SignalMessage) {
	client, exists := legacyClientRegistry.Get(clientID)

	if !exists {
		return
	}

	client.p2pSignalMutex.Lock()
	defer client.p2pSignalMutex.Unlock()

	if client.p2pSignalConn == nil {
		return
	}

	data, err := json.Marshal(signal)
	if err != nil {
		return
	}

	if err := client.p2pSignalConn.WriteMessage(websocket.TextMessage, data); err != nil {
		log.Printf("Failed to send P2P signal to %s: %v", clientID, err)
	}
}

// handleP2PSignaling handles P2P signaling requests
func handleP2PSignaling(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var signal p2p.SignalMessage
	r.Body = http.MaxBytesReader(w, r.Body, p2p.MaxSignalMessageSize)
	if err := json.NewDecoder(r.Body).Decode(&signal); err != nil {
		http.Error(w, "Invalid signal", http.StatusBadRequest)
		return
	}
	fromPeer, allowed := requirePrincipalClient(w, r, signal.FromPeer)
	if !allowed {
		return
	}
	signal.FromPeer = fromPeer

	switch signal.Type {
	case p2p.SignalTypeDiscover:
		// Return peer info for requested client
		targetClient, exists := legacyClientRegistry.Get(signal.ToPeer)

		if !exists || targetClient.p2pInfo == nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(p2p.NewFallbackSignal(
				"server", signal.FromPeer, "", "peer not found or P2P not enabled"))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(p2p.NewPeerInfoSignal(signal.ToPeer, targetClient.p2pInfo))

	case p2p.SignalTypePunch:
		// Acknowledge punch signal - actual hole punching is client-side
		_, exists := legacyClientRegistry.Get(signal.ToPeer)

		if !exists {
			http.Error(w, "Peer not found", http.StatusNotFound)
			return
		}

		w.WriteHeader(http.StatusOK)

	default:
		http.Error(w, "Unknown signal type", http.StatusBadRequest)
	}
}

// handleP2PClients returns list of P2P-enabled clients for debugging
func handleP2PClients(w http.ResponseWriter, r *http.Request) {
	publicPeerInfo := func(info *p2p.PeerInfo) *p2p.PeerInfo {
		if info == nil {
			return nil
		}
		copy := *info
		copy.AuthToken = ""
		return &copy
	}
	type clientInfo struct {
		ClientID   string        `json:"client_id"`
		Tunnels    []string      `json:"tunnels"`
		P2PEnabled bool          `json:"p2p_enabled"`
		P2PInfo    *p2p.PeerInfo `json:"p2p_info,omitempty"`
		HasSignal  bool          `json:"has_signal"`
	}

	clients := make([]clientInfo, 0)
	for id, client := range legacyClientRegistry.Snapshot() {
		tunnels := make([]string, 0)
		for _, t := range client.tunnels {
			tunnels = append(tunnels, t.Name)
		}

		clients = append(clients, clientInfo{
			ClientID:   id,
			Tunnels:    tunnels,
			P2PEnabled: client.p2pInfo != nil,
			P2PInfo:    publicPeerInfo(client.p2pInfo),
			HasSignal:  client.p2pSignalConn != nil,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(clients)
}

// handleP2PDiscoverTrigger triggers P2P discovery between two clients for testing
func handleP2PDiscoverTrigger(w http.ResponseWriter, r *http.Request) {
	from := r.URL.Query().Get("from")
	to := r.URL.Query().Get("to")

	if from == "" || to == "" {
		http.Error(w, "Missing 'from' and 'to' parameters", http.StatusBadRequest)
		return
	}

	fromClient, fromExists := legacyClientRegistry.Get(from)
	toClient, toExists := legacyClientRegistry.Get(to)

	if !fromExists || !toExists {
		http.Error(w, "Client not found", http.StatusNotFound)
		return
	}

	if fromClient.p2pInfo == nil || toClient.p2pInfo == nil {
		http.Error(w, "P2P not enabled on both clients", http.StatusBadRequest)
		return
	}

	if fromClient.p2pSignalConn == nil || toClient.p2pSignalConn == nil {
		http.Error(w, "Signaling not connected on both clients", http.StatusBadRequest)
		return
	}

	// Send peer info to both clients to initiate hole punching
	log.Printf("P2P: Triggering discover between %s and %s", from, to)
	sendP2PSignalToClient(from, p2p.NewPeerInfoSignal(to, toClient.p2pInfo))
	sendP2PSignalToClient(to, p2p.NewPeerInfoSignal(from, fromClient.p2pInfo))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "triggered",
		"from":    from,
		"to":      to,
		"message": "P2P discovery initiated between both clients",
	})
}

// handleP2PTunnelOwner returns the client ID that owns a tunnel
func handleP2PTunnelOwner(w http.ResponseWriter, r *http.Request) {
	tunnelName := r.URL.Query().Get("tunnel")
	if tunnelName == "" {
		http.Error(w, "Missing 'tunnel' parameter", http.StatusBadRequest)
		return
	}

	route, exists := legacyRouteRegistry.Get(tunnelName)

	if !exists {
		http.Error(w, "Tunnel not found", http.StatusNotFound)
		return
	}

	// Get client P2P info
	client, clientExists := serverDepsFromRequest(r).Clients.Get(route.ClientID)

	response := map[string]interface{}{
		"tunnel":      tunnelName,
		"client_id":   route.ClientID,
		"p2p_enabled": false,
	}

	if clientExists && client.p2pInfo != nil {
		response["p2p_enabled"] = true
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}
