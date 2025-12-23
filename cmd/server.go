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
	"xrok/cmd/p2p"
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
	Name      string `json:"name"`
	Target    string `json:"target"`
	BasicAuth string `json:"basic_auth,omitempty"` // user:password for HTTP Basic Auth
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
}

// TunnelRoute maps a subdomain to client and tunnel info
type TunnelRoute struct {
	ClientID   string
	TunnelName string
	Target     string
	BasicAuth  string            // user:password for HTTP Basic Auth
	Headers    map[string]string // Custom headers to inject
}

// Server globals
var (
	serverDebug                    bool
	serverClientCA                 string
	serverRequestLog               bool
	serverClients                  = make(map[string]*serverClientInfo)
	serverClientsMutex             sync.Mutex
	serverDomain                   string
	serverCertFile                 string
	serverKeyFile                  string
	serverUpgrader                 = websocket.Upgrader{
		CheckOrigin:       func(r *http.Request) bool { return true },
		EnableCompression: true,
	}
	serverMaxConcurrentConnections = 5000
	serverConnLimiter              = make(chan struct{}, 5000)
	serverUsedPorts                = make(map[int]bool)
	serverPortMutex                sync.Mutex
	serverInactivityTimeout        = 60 * time.Minute

	serverBasePortNoTLS     = 4000
	serverBaseClientPortTLS = 7000

	serverStateFile = "state.dat"
	serverLogFile   = "server.log"

	serverShutdownChan = make(chan struct{})
	serverWg           sync.WaitGroup

	// Tunnel routing: subdomain -> TunnelRoute
	serverTunnelRoutes      = make(map[string]*TunnelRoute)
	serverTunnelRoutesMutex sync.RWMutex

	serverBufferPool = sync.Pool{
		New: func() interface{} {
			buf := make([]byte, 32*1024)
			return &buf
		},
	}
)

// Auth globals
var (
	serverApiToken    string
	serverAuthEnabled bool

	serverRateLimiters     = make(map[string]*rate.Limiter)
	serverRateLimiterMutex sync.Mutex

	serverRateLimit  = rate.Limit(10)
	serverRateBurst  = 20
	serverRegLimit   = rate.Limit(1)
	serverRegBurst   = 5
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
	setupServerLogging()

	fs := flag.NewFlagSet("server", flag.ExitOnError)
	fs.StringVar(&serverDomain, "domain", "", "Domain to use in URLs (required)")
	fs.BoolVar(&serverDebug, "debug", false, "Enable debug logging")
	fs.BoolVar(&serverRequestLog, "request-log", false, "Enable HTTP request logging")
	fs.StringVar(&serverClientCA, "client-ca", "", "Path to CA certificate for mTLS client verification")
	fs.Parse(args)

	if serverDomain == "" {
		fmt.Println("Flag -domain is required.")
		os.Exit(1)
	}

	initServerCertificates()
	initServerAuth()

	serverWg.Add(1)
	go cleanupServerRateLimiters(serverShutdownChan, &serverWg)

	initServerMetrics()

	loadServerState()

	go gracefulServerShutdown()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		<-serverShutdownChan
		cancel()
	}()

	serverWg.Add(1)
	go func() {
		defer serverWg.Done()
		waitForProxyConnectionTLS(ctx, 443)
	}()

	// Start QUIC listener on port 443 (UDP)
	serverWg.Add(1)
	go func() {
		defer serverWg.Done()
		waitForQUICConnections(ctx, 443)
	}()

	defer func() {
		if r := recover(); r != nil {
			log.Printf("Recovered from panic: %v", r)
			saveServerState()
		}
	}()

	// Block until shutdown
	<-serverShutdownChan
	serverWg.Wait()
}

func setupServerLogging() {
	file, err := os.OpenFile(serverLogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		fmt.Printf("Failed to open log file %s: %v", serverLogFile, err)
		os.Exit(1)
	}
	log.SetOutput(file)
	log.SetFlags(log.LstdFlags | log.Lshortfile)
}

func initServerCertificates() {
	serverCertFile = fmt.Sprintf("/etc/letsencrypt/live/%s/fullchain.pem", serverDomain)
	serverKeyFile = fmt.Sprintf("/etc/letsencrypt/live/%s/privkey.pem", serverDomain)
}

func initServerAuth() {
	serverApiToken = os.Getenv("XROK_API_TOKEN")
	if serverApiToken != "" {
		serverAuthEnabled = true
		log.Printf("API authentication enabled")
	} else {
		log.Printf("Warning: XROK_API_TOKEN not set, authentication disabled")
	}
}

func generateServerToken() string {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		log.Fatalf("Failed to generate token: %v", err)
	}
	return hex.EncodeToString(bytes)
}

func validateServerToken(r *http.Request) bool {
	if !serverAuthEnabled {
		return true
	}

	token := r.Header.Get("X-API-Token")
	if token == "" {
		token = r.URL.Query().Get("token")
	}

	return subtle.ConstantTimeCompare([]byte(token), []byte(serverApiToken)) == 1
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

func getServerClientIP(r *http.Request) string {
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
	return func(w http.ResponseWriter, r *http.Request) {
		if !validateServerToken(r) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			log.Printf("Unauthorized request from IP: %s", getServerClientIP(r))
			return
		}
		next(w, r)
	}
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

	// Build headers string
	var headerStr strings.Builder
	for key, value := range headers {
		headerStr.WriteString(key)
		headerStr.WriteString(": ")
		headerStr.WriteString(value)
		headerStr.WriteString("\r\n")
	}

	// Insert headers after the first line
	result := requestStr[:firstLineEnd+2] + headerStr.String() + requestStr[firstLineEnd+2:]
	return []byte(result)
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
	)
}

func generateServerPorts() (int, int) {
	serverPortMutex.Lock()
	defer serverPortMutex.Unlock()

	portNoTLS := findServerAvailablePort(serverBasePortNoTLS, 1)
	clientPortTLS := findServerAvailablePort(serverBaseClientPortTLS, 1)

	serverUsedPorts[portNoTLS] = true
	serverUsedPorts[clientPortTLS] = true

	return portNoTLS, clientPortTLS
}

func findServerAvailablePort(basePort, step int) int {
	for port := basePort; ; port += step {
		if !serverUsedPorts[port] {
			return port
		}
	}
}

func releaseServerPorts(ports ...int) {
	serverPortMutex.Lock()
	defer serverPortMutex.Unlock()
	for _, port := range ports {
		delete(serverUsedPorts, port)
	}
}

func closeServerClientConnections(clientID string, client *serverClientInfo) {
	srvActiveClients.Dec()

	// Clean up tunnel routes for this client
	serverTunnelRoutesMutex.Lock()
	for _, t := range client.tunnels {
		delete(serverTunnelRoutes, t.Name)
	}
	serverTunnelRoutesMutex.Unlock()

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

	releaseServerPorts(client.PortNoTLS, client.ClientPortTLS, client.ClientPortNoTLS)
	serverClientsMutex.Lock()
	delete(serverClients, clientID)
	serverClientsMutex.Unlock()
	if serverDebug {
		log.Printf("Resources released for client %s", clientID)
	}
}

func monitorServerClientActivity(clientID string, client *serverClientInfo) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		serverClientsMutex.Lock()
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
			serverClientsMutex.Unlock()
			return
		}
		serverClientsMutex.Unlock()
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

	portNoTLS, clientPortTLS := generateServerPorts()
	ctx, cancel := context.WithCancel(context.Background())

	clientID := regReq.ClientID
	if clientID == "" {
		clientID = uuid.New().String()
	}

	// Clean up existing client if reconnecting (stateless behavior)
	serverClientsMutex.Lock()
	if existing, exists := serverClients[clientID]; exists {
		// Clean up old tunnel routes
		serverTunnelRoutesMutex.Lock()
		for _, t := range existing.tunnels {
			delete(serverTunnelRoutes, t.Name)
		}
		serverTunnelRoutesMutex.Unlock()

		// Cancel old client context
		if existing.cancelFunc != nil {
			existing.cancelFunc()
		}

		// Close old connections
		if existing.muxSession != nil {
			existing.muxSession.Close()
		}

		// Release old ports
		releaseServerPorts(existing.PortNoTLS, existing.ClientPortTLS, existing.ClientPortNoTLS)
		delete(serverClients, clientID)
		srvActiveClients.Dec()
		log.Printf("Cleaned up existing client %s for reconnection", clientID)
	}
	serverClientsMutex.Unlock()

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
			serverTunnelRoutesMutex.RLock()
			existingRoute, routeExists := serverTunnelRoutes[name]
			serverTunnelRoutesMutex.RUnlock()
			if routeExists && existingRoute.ClientID != clientID {
				http.Error(w, fmt.Sprintf("Subdomain '%s' already in use by another client", name), http.StatusConflict)
				cancel()
				return
			}

			tunnels = append(tunnels, ServerTunnelConfig{Name: name, Target: t.Target, BasicAuth: t.BasicAuth})
			tunnelMap[name] = t.Target

			url := fmt.Sprintf("https://%s.%s", name, serverDomain)
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
		wsConnChans:    make(map[string]chan *websocket.Conn),
		tunnels:        tunnels,
		tunnelMap:      tunnelMap,
		reverseConfigs:  regReq.Reverse,
		customHeaders:   regReq.Headers,
		compress:        regReq.Compress,
		p2pInfo:         regReq.P2PInfo,
		p2pConnections:  make(map[string]net.Conn),
	}
	client.SetLastActivity(time.Now())

	serverClientsMutex.Lock()
	serverClients[clientID] = client
	serverClientsMutex.Unlock()

	// Register tunnel routes
	serverTunnelRoutesMutex.Lock()
	for _, t := range tunnels {
		log.Printf("Registering tunnel route: name=%s, target=%s, basicAuth=%q, headers=%v", t.Name, t.Target, t.BasicAuth, regReq.Headers)
		serverTunnelRoutes[t.Name] = &TunnelRoute{
			ClientID:   clientID,
			TunnelName: t.Name,
			Target:     t.Target,
			BasicAuth:  t.BasicAuth,
			Headers:    regReq.Headers,
		}
	}
	serverTunnelRoutesMutex.Unlock()

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

	serverWg.Add(1)
	go func() {
		defer serverWg.Done()
		waitForServerClientConnection(ctx, client, client.ClientPortTLS, true, clientID)
	}()

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
	default:
		http.Error(w, "Invalid connection_type parameter", http.StatusBadRequest)
		return
	}

	serverWg.Add(1)
	go func() {
		defer serverWg.Done()
		monitorServerClientActivity(clientID, client)
	}()
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
	}

	response := regResponse{
		UDID:          clientID,
		ClientPortTLS: strconv.Itoa(clientPortTLS),
		ProxyURLNoTLS: fmt.Sprintf("%s:%d", serverDomain, portNoTLS),
		Tunnels:       tunnelInfos,
	}

	if regReq.ConnectionType == "tls" {
		response.ProxyURLTLS = fmt.Sprintf("https://%s.%s", clientID, serverDomain)
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("Failed to encode response: %v", err)
	}
}

func extractServerClientIDFromHost(host string) string {
	parts := strings.Split(host, ".")
	if len(parts) < 2 {
		return ""
	}
	return parts[0]
}

func waitForProxyConnectionTLS(ctx context.Context, proxyPort int) {
	cert, err := tls.LoadX509KeyPair(serverCertFile, serverKeyFile)
	if err != nil {
		log.Printf("Failed to load certificate for port %d: %v", proxyPort, err)
		return
	}

	// Create unified HTTP handler for both registration and proxy
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if colonIdx := strings.Index(host, ":"); colonIdx != -1 {
			host = host[:colonIdx]
		}

		// Check if this is a request to the main domain (registration/control)
		if host == serverDomain {
			// Handle registration and control endpoints
			switch r.URL.Path {
			case "/register_client":
				serverAuthMiddleware(serverRegRateLimitMiddleware(registerServerClientHandler))(w, r)
				return
			case "/client_ws":
				serverAuthMiddleware(serverRateLimitMiddleware(serverClientWebSocketHandler))(w, r)
				return
			case "/mux":
				serverRateLimitMiddleware(serverMuxWebSocketHandler)(w, r)
				return
			case "/p2p/signal":
				handleP2PSignaling(w, r)
				return
			case "/p2p/ws":
				handleP2PWebSocket(w, r)
				return
			case "/p2p/clients":
				handleP2PClients(w, r)
				return
			case "/p2p/discover":
				handleP2PDiscoverTrigger(w, r)
				return
			case "/p2p/tunnel-owner":
				handleP2PTunnelOwner(w, r)
				return
			case "/health":
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"status":"healthy"}`))
				return
			case "/ready":
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				serverClientsMutex.Lock()
				clientCount := len(serverClients)
				serverClientsMutex.Unlock()
				w.Write([]byte(fmt.Sprintf(`{"status":"ready","clients":%d}`, clientCount)))
				return
			case "/stats":
				serverClientsMutex.Lock()
				clientCount := len(serverClients)
				serverClientsMutex.Unlock()
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("xrok Server Stats\n"))
				w.Write([]byte("=================\n"))
				w.Write([]byte(fmt.Sprintf("Active Clients: %d\n", clientCount)))
				return
			case "/metrics":
				promhttp.Handler().ServeHTTP(w, r)
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
		serverTunnelRoutesMutex.RLock()
		route, routeExists := serverTunnelRoutes[subdomain]
		serverTunnelRoutesMutex.RUnlock()

		var client *serverClientInfo
		var tunnelName string

		if routeExists {
			serverClientsMutex.Lock()
			client = serverClients[route.ClientID]
			serverClientsMutex.Unlock()
			tunnelName = route.TunnelName
		} else {
			serverClientsMutex.Lock()
			client = serverClients[subdomain]
			serverClientsMutex.Unlock()
			tunnelName = subdomain
		}

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

		// Handle the proxy connection with the buffered data
		go handleHijackedProxyConnection(conn, buf, r, client, tunnelName, basicAuth, customHeaders)
	})

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"http/1.1"}, // Disable HTTP/2 for hijacking support
	}

	// Configure mTLS if client CA is specified
	if serverClientCA != "" {
		caCert, err := os.ReadFile(serverClientCA)
		if err != nil {
			log.Printf("Failed to load client CA certificate: %v", err)
		} else {
			caCertPool := x509.NewCertPool()
			if caCertPool.AppendCertsFromPEM(caCert) {
				tlsConfig.ClientCAs = caCertPool
				tlsConfig.ClientAuth = tls.VerifyClientCertIfGiven // Optional client cert
				log.Printf("mTLS enabled: client certificates will be verified against %s", serverClientCA)
			} else {
				log.Printf("Failed to parse client CA certificate")
			}
		}
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

// waitForQUICConnections handles QUIC connections
func waitForQUICConnections(ctx context.Context, port int) {
	cert, err := tls.LoadX509KeyPair(serverCertFile, serverKeyFile)
	if err != nil {
		log.Printf("Failed to load certificate for QUIC: %v", err)
		return
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"xrok-quic"},
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

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				continue
			}
		}
		go handleQUICServerConnection(ctx, conn.(*transport.QUICConn))
	}
}

func handleQUICServerConnection(ctx context.Context, conn *transport.QUICConn) {
	// Accept first stream for registration
	stream, err := conn.AcceptStream(ctx)
	if err != nil {
		conn.Close()
		return
	}

	// Read registration message
	regBuf := make([]byte, 256)
	n, err := stream.Read(regBuf)
	if err != nil {
		conn.Close()
		return
	}

	regMsg := string(regBuf[:n])
	if !strings.HasPrefix(regMsg, "XROK:") {
		log.Printf("QUIC: Invalid registration message")
		conn.Close()
		return
	}

	clientID := strings.TrimPrefix(regMsg, "XROK:")

	serverClientsMutex.Lock()
	client, exists := serverClients[clientID]
	serverClientsMutex.Unlock()

	if !exists {
		log.Printf("QUIC: Unknown client %s", clientID)
		conn.Close()
		return
	}

	client.connMutex.Lock()
	client.quicConn = conn
	client.connMutex.Unlock()

	log.Printf("QUIC connection established for client %s", clientID)

	// Keep connection open and handle lifecycle
	<-ctx.Done()
	conn.Close()
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

	// Reconstruct the HTTP request
	var reqBuf bytes.Buffer
	reqBuf.WriteString(fmt.Sprintf("%s %s %s\r\n", r.Method, r.URL.RequestURI(), r.Proto))
	// Add Host header (stored in r.Host, not r.Header in Go)
	if r.Host != "" {
		reqBuf.WriteString(fmt.Sprintf("Host: %s\r\n", r.Host))
	}
	for key, values := range r.Header {
		for _, value := range values {
			reqBuf.WriteString(fmt.Sprintf("%s: %s\r\n", key, value))
		}
	}
	reqBuf.WriteString("\r\n")

	// Read any buffered body data
	if buf.Reader.Buffered() > 0 {
		buffered := make([]byte, buf.Reader.Buffered())
		buf.Read(buffered)
		reqBuf.Write(buffered)
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

	listener, err := net.Listen("tcp", remoteAddr)
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

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				if !strings.Contains(err.Error(), "use of closed") {
					log.Printf("Reverse listener accept error: %v", err)
				}
				continue
			}
		}

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
				select {
				case <-ctx.Done():
					return
				default:
					continue
				}
			}

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

		serverClientsMutex.Lock()
		client.connMutex.Lock()
		client.conn = wsConn
		client.connMutex.Unlock()
		serverClientsMutex.Unlock()
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
			cert, err := tls.LoadX509KeyPair(serverCertFile, serverKeyFile)
			if err != nil {
				log.Fatalf("Failed to load certificate: %v", err)
			}
			server.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
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
	serverClientsMutex.Lock()
	defer serverClientsMutex.Unlock()

	state := struct {
		Clients   map[string]serializableClientInfoForSave
		UsedPorts map[int]bool
	}{
		Clients:   make(map[string]serializableClientInfoForSave),
		UsedPorts: make(map[int]bool),
	}

	for id, client := range serverClients {
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

	file, err := os.Create(serverStateFile)
	if err != nil {
		log.Printf("Failed to create state file: %v", err)
		return
	}
	defer file.Close()

	encoder := gob.NewEncoder(file)
	if err := encoder.Encode(state); err != nil {
		log.Printf("Failed to encode state: %v", err)
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

	serverClientsMutex.Lock()
	for id, clientData := range state.Clients {
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
		serverClients[id] = client

		ctx, cancel := context.WithCancel(context.Background())
		client.cancelFunc = cancel

		serverWg.Add(1)
		go func(ctx context.Context, client *serverClientInfo, id string) {
			defer serverWg.Done()
			waitForServerClientConnection(ctx, client, client.ClientPortTLS, true, id)
		}(ctx, client, id)

		if client.ConnectionType == "non-tls" {
			serverWg.Add(1)
			go func(ctx context.Context, client *serverClientInfo) {
				defer serverWg.Done()
				waitForProxyConnectionNoTLS(ctx, client, client.PortNoTLS)
			}(ctx, client)
		}

		serverWg.Add(1)
		go func(id string, client *serverClientInfo) {
			defer serverWg.Done()
			monitorServerClientActivity(id, client)
		}(id, client)
	}
	serverClientsMutex.Unlock()

	serverPortMutex.Lock()
	for port, used := range state.UsedPorts {
		serverUsedPorts[port] = used
	}
	serverPortMutex.Unlock()
}

func gracefulServerShutdown() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigChan
	log.Printf("Received signal %v, initiating graceful shutdown...", sig)

	close(serverShutdownChan)

	serverClientsMutex.Lock()
	for clientID, client := range serverClients {
		log.Printf("Closing client %s", clientID)
		if client.cancelFunc != nil {
			client.cancelFunc()
		}
	}
	serverClientsMutex.Unlock()

	done := make(chan struct{})
	go func() {
		serverWg.Wait()
		close(done)
	}()

	select {
	case <-done:
		log.Println("All connections closed gracefully")
	case <-time.After(30 * time.Second):
		log.Println("Shutdown timeout, forcing exit")
	}

	saveServerState()
	log.Println("Server shutdown complete")
	os.Exit(0)
}

func serverMuxWebSocketHandler(w http.ResponseWriter, r *http.Request) {
	clientID := r.URL.Query().Get("clientID")
	if clientID == "" {
		http.Error(w, "Missing clientID", http.StatusBadRequest)
		return
	}

	wsConn, err := serverUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Failed to upgrade mux WebSocket: %v", err)
		return
	}
	srvWsUpgrades.Inc()

	serverClientsMutex.Lock()
	client, exists := serverClients[clientID]
	serverClientsMutex.Unlock()
	if !exists {
		log.Printf("Client not found for mux: %s", clientID)
		wsConn.Close()
		return
	}

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

	client.muxSessionMutex.Lock()
	client.muxSession = session
	client.muxSessionMutex.Unlock()

	log.Printf("Yamux session established for client %s", clientID)

	// Accept streams from client (for SOCKS5/dynamic connections)
	go func() {
		for {
			stream, err := session.Accept()
			if err != nil {
				if err != io.EOF {
					log.Printf("Error accepting stream from client %s: %v", clientID, err)
				}
				return
			}
			go handleClientInitiatedStream(stream, client)
		}
	}()

	<-session.CloseChan()

	client.muxSessionMutex.Lock()
	client.muxSession = nil
	client.muxSessionMutex.Unlock()

	// Clean up tunnel routes when session closes (stateless behavior)
	serverTunnelRoutesMutex.Lock()
	for _, t := range client.tunnels {
		delete(serverTunnelRoutes, t.Name)
	}
	serverTunnelRoutesMutex.Unlock()

	// Remove client from server clients
	serverClientsMutex.Lock()
	delete(serverClients, clientID)
	serverClientsMutex.Unlock()

	// Release ports
	releaseServerPorts(client.PortNoTLS, client.ClientPortTLS, client.ClientPortNoTLS)

	srvActiveClients.Dec()
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

		// Connect to target
		targetConn, err := net.DialTimeout("tcp", target, 30*time.Second)
		if err != nil {
			log.Printf("Failed to connect to SOCKS5 target %s: %v", target, err)
			return
		}
		defer targetConn.Close()

		client.SetLastActivity(time.Now())

		// Bridge the connections
		done := make(chan struct{}, 2)

		go func() {
			io.Copy(targetConn, stream)
			done <- struct{}{}
		}()

		go func() {
			io.Copy(stream, targetConn)
			done <- struct{}{}
		}()

		<-done
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

		// Resolve and connect UDP
		udpAddr, err := net.ResolveUDPAddr("udp", remoteAddr)
		if err != nil {
			log.Printf("Failed to resolve UDP address %s: %v", remoteAddr, err)
			return
		}

		udpConn, err := net.DialUDP("udp", nil, udpAddr)
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

	serverClientsMutex.Lock()
	client, exists := serverClients[clientID]
	serverClientsMutex.Unlock()
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

	serverClientsMutex.Lock()
	client, exists := serverClients[clientID]
	serverClientsMutex.Unlock()

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

		var signal p2p.SignalMessage
		if err := json.Unmarshal(message, &signal); err != nil {
			log.Printf("P2P signal decode error: %v", err)
			continue
		}

		// Process and relay signal
		handleP2PSignalRelay(clientID, &signal)
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
		serverClientsMutex.Lock()
		targetClient, exists := serverClients[signal.ToPeer]
		serverClientsMutex.Unlock()

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
		serverClientsMutex.Lock()
		fromClient, _ := serverClients[fromClientID]
		serverClientsMutex.Unlock()

		if fromClient != nil && fromClient.p2pInfo != nil {
			sendP2PSignalToClient(signal.ToPeer, p2p.NewPeerInfoSignal(fromClientID, fromClient.p2pInfo))
		}

	case p2p.SignalTypePunch:
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
	serverClientsMutex.Lock()
	client, exists := serverClients[clientID]
	serverClientsMutex.Unlock()

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
	if err := json.NewDecoder(r.Body).Decode(&signal); err != nil {
		http.Error(w, "Invalid signal", http.StatusBadRequest)
		return
	}

	switch signal.Type {
	case p2p.SignalTypeDiscover:
		// Return peer info for requested client
		serverClientsMutex.Lock()
		targetClient, exists := serverClients[signal.ToPeer]
		serverClientsMutex.Unlock()

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
		serverClientsMutex.Lock()
		_, exists := serverClients[signal.ToPeer]
		serverClientsMutex.Unlock()

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
	type clientInfo struct {
		ClientID   string       `json:"client_id"`
		Tunnels    []string     `json:"tunnels"`
		P2PEnabled bool         `json:"p2p_enabled"`
		P2PInfo    *p2p.PeerInfo `json:"p2p_info,omitempty"`
		HasSignal  bool         `json:"has_signal"`
	}

	serverClientsMutex.Lock()
	defer serverClientsMutex.Unlock()

	clients := make([]clientInfo, 0)
	for id, client := range serverClients {
		tunnels := make([]string, 0)
		for _, t := range client.tunnels {
			tunnels = append(tunnels, t.Name)
		}

		clients = append(clients, clientInfo{
			ClientID:   id,
			Tunnels:    tunnels,
			P2PEnabled: client.p2pInfo != nil,
			P2PInfo:    client.p2pInfo,
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

	serverClientsMutex.Lock()
	fromClient, fromExists := serverClients[from]
	toClient, toExists := serverClients[to]
	serverClientsMutex.Unlock()

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

	serverTunnelRoutesMutex.RLock()
	route, exists := serverTunnelRoutes[tunnelName]
	serverTunnelRoutesMutex.RUnlock()

	if !exists {
		http.Error(w, "Tunnel not found", http.StatusNotFound)
		return
	}

	// Get client P2P info
	serverClientsMutex.Lock()
	client, clientExists := serverClients[route.ClientID]
	serverClientsMutex.Unlock()

	response := map[string]interface{}{
		"tunnel":     tunnelName,
		"client_id":  route.ClientID,
		"p2p_enabled": false,
	}

	if clientExists && client.p2pInfo != nil {
		response["p2p_enabled"] = true
		response["p2p_info"] = client.p2pInfo
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}
