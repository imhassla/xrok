package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type activeConnection struct {
	WebSocketConn *websocket.Conn
	LocalConn     net.Conn
}

var activeConnections = struct {
	sync.RWMutex
	connections map[string]*activeConnection
}{connections: make(map[string]*activeConnection)}

var (
	rp       int
	debug    bool
	clientID string

	state      *clientState
	stateMutex sync.Mutex
)

type clientState struct {
	ProxyURL     string
	ClientPort   string
	ServerAddr   string
	LocalPort    string
	UseTLS       bool
	UseProxy     bool
	ConnectionID string
}

type RegisterResponse struct {
	UDID          string `json:"udid"`
	ProxyURLTLS   string `json:"proxy_url_tls"`
	ProxyURLNoTLS string `json:"proxy_url_notls"`
	ClientPortTLS string `json:"client_port_tls"`
}

// getRandomFreePort finds a random free port within the specified range.
func getRandomFreePort(min, max int) (int, error) {
	rand.Seed(time.Now().UnixNano())
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

// serveFolderLocally serves a folder locally on the specified port.
func serveFolderLocally(ctx context.Context, folderPath string, localPort string, wg *sync.WaitGroup) {
	// Decrement the WaitGroup counter when the function completes.
	defer wg.Done()

	// Convert the folder path to an absolute path.
	absPath, err := filepath.Abs(folderPath)
	if err != nil {
		// Log a fatal error and exit if the folder path is invalid.
		log.Fatalf("Invalid folder path: %s", folderPath)
	}

	// Check if the directory exists.
	if _, err := os.Stat(absPath); os.IsNotExist(err) {
		// Log a fatal error and exit if the directory does not exist.
		log.Fatalf("Directory %s does not exist", absPath)
	}

	// Create a file server to serve files from the absolute path.
	fileServer := http.FileServer(http.Dir(absPath))

	// Create a new HTTP request multiplexer.
	mux := http.NewServeMux()

	// Define a handler function for the root URL path.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Construct the full file path by appending the request URL path to the absolute path.
		filePath := absPath + r.URL.Path

		// Get file information.
		fileInfo, err := os.Stat(filePath)
		if err != nil {
			// Return a 404 Not Found response if the file does not exist.
			http.NotFound(w, r)
			return
		}

		// Check if the file size exceeds the limit of 100 GB.
		if fileInfo.Size() > 100*1024*1024*1024 {
			// Return a 403 Forbidden response if the file is too large.
			http.Error(w, "File is too large", http.StatusForbidden)
			return
		}

		// If the file is not a directory, set various HTTP headers.
		if !fileInfo.IsDir() {
			w.Header().Set("Content-Disposition", "attachment; filename="+url.QueryEscape(fileInfo.Name()))
			w.Header().Set("Cache-Control", "public, max-age=86400")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.Header().Set("X-XSS-Protection", "1; mode=block")
		}

		// Serve the file using the file server.
		fileServer.ServeHTTP(w, r)
	})

	// Create a new HTTP server with the specified address and handler.
	server := &http.Server{
		Addr:    localPort,
		Handler: mux,
	}

	// Start a goroutine to shut down the server gracefully when the context is done.
	go func() {
		<-ctx.Done()
		server.Shutdown(context.Background())
	}()

	// Log the address and port on which the folder is being served.
	log.Printf("Serving folder %s on %s", absPath, localPort)

	// Start the HTTP server and listen for incoming connections.
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		// Log a fatal error and exit if the server fails to start.
		log.Fatalf("Failed to start local file server: %v", err)
	}
}

// connectAndForward establishes a WebSocket connection and forwards data.
func connectAndForward(ctx context.Context, state *clientState, wg *sync.WaitGroup) {
	defer wg.Done()

	const (
		reconnectInterval = 6 * time.Second
		connReadLimit     = 1024 * 1024
		connTimeout       = 30 * time.Second
	)

	scheme := "wss"

	u := url.URL{Scheme: scheme, Host: fmt.Sprintf("%s:%s", state.ServerAddr, state.ClientPort), Path: "/ws"}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		dialer := websocket.Dialer{HandshakeTimeout: connTimeout}
		conn, resp, err := dialer.Dial(u.String(), nil)
		if err != nil {
			if resp != nil {
				log.Printf("WebSocket connection failed with status: %d %s", resp.StatusCode, resp.Status)
			} else {
				log.Printf("WebSocket connection failed: %v", err)
			}
			time.Sleep(reconnectInterval)
			continue
		}

		conn.SetReadLimit(connReadLimit)
		if debug {
			log.Printf("Control connection established")
		}

		go handleWebSocketConnection(ctx, conn, state)
		time.Sleep(reconnectInterval) // Keep reconnect attempts spaced apart.
	}
}

// establishNewWebSocketConnection establishes a new WebSocket connection for data forwarding.
func establishNewWebSocketConnection(ctx context.Context, state *clientState, connectionID string, useTLS bool) {
	scheme := "wss"

	wsURL := url.URL{
		Scheme: scheme,
		Host:   fmt.Sprintf("%s:%s", state.ServerAddr, state.ClientPort),
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
			dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
			wsConn, _, err := dialer.Dial(wsURL.String(), nil)
			if err != nil {
				log.Printf("Failed to establish WebSocket connection for connectionID %s: %v", connectionID, err)
				time.Sleep(2 * time.Second)
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

// handleDataForwarding handles data forwarding between WebSocket and local connection.
func handleDataForwarding(ctx context.Context, wsConn *websocket.Conn, localConn net.Conn, connectionID string) {
	defer func() {
		wsConn.Close()
		localConn.Close()
		activeConnections.Lock()
		delete(activeConnections.connections, connectionID)
		activeConnections.Unlock()
		log.Printf("Connection with ID %s closed", connectionID)
	}()

	done := make(chan struct{})

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

// handleWebSocketConnection handles incoming WebSocket messages.
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

// registerClient registers the client with the server.
func registerClient(state *clientState) bool {
	// Define constants for the maximum number of retries and the backoff factor.
	const maxRetries = 3
	const backoffFactor = 500 * time.Millisecond

	// Determine the connection type based on whether TLS is used.
	connectionType := "tls"
	if !state.UseTLS {
		connectionType = "non-tls"
	}

	// Log a message indicating the start of the client registration process.
	log.Printf("Registering client at %s with connection type %s", state.ServerAddr, connectionType)

	// Construct the full URL for the registration request.
	fullURL := fmt.Sprintf("https://%s:%d/register_client?connection_type=%s", state.ServerAddr, rp, connectionType)

	// Add the client ID to the registration URL if specified.
	if clientID != "" {
		fullURL += "&id=" + clientID
	}

	// Create an HTTP client with a timeout of 10 seconds.
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        1000,
			MaxIdleConnsPerHost: 1000,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	// Variable to store the registration response.
	var res RegisterResponse

	// Loop to attempt registration up to the maximum number of retries.
	for attempt := 1; attempt <= maxRetries; attempt++ {
		// Send a POST request to the registration URL.
		response, err := client.Post(fullURL, "application/json", nil)
		if err != nil {
			// Log an error message and wait before retrying if the request fails.
			log.Printf("Attempt %d: Registration failed: %v", attempt, err)
			time.Sleep(backoffFactor * time.Duration(attempt))
			continue
		}
		defer response.Body.Close()

		// Check if the server returned a non-OK status code.
		if response.StatusCode != http.StatusOK {
			// Log an error message and wait before retrying if the server returns an error.
			log.Printf("Attempt %d: Server error, status: %d", attempt, response.StatusCode)
			time.Sleep(backoffFactor * time.Duration(attempt))
			continue
		}

		// Decode the JSON response into the RegisterResponse struct.
		if err := json.NewDecoder(response.Body).Decode(&res); err != nil {
			// Log an error message and wait before retrying if the response cannot be decoded.
			log.Printf("Attempt %d: Failed to decode response: %v", attempt, err)
			time.Sleep(backoffFactor * time.Duration(attempt))
			continue
		}

		// Update the client ID with the UDID from the response.
		clientID = res.UDID

		state.ClientPort = res.ClientPortTLS

		if state.UseTLS {
			state.ProxyURL = res.ProxyURLTLS
		} else {
			state.ProxyURL = res.ProxyURLNoTLS
		}

		if state.ProxyURL == "" || state.ClientPort == "" {
			log.Printf("Attempt %d: Received empty proxy URL or client port", attempt)
			time.Sleep(backoffFactor * time.Duration(attempt))
			continue
		}

		// Log the proxy URL for third-party connections.
		fmt.Printf("Proxy URL for third-party connections: %s\n", state.ProxyURL)
		return true
	}

	// Log a message indicating that registration failed after the maximum number of attempts.
	log.Printf("Registration failed after %d attempts", maxRetries)
	return false
}

// startLocalProxy starts a local HTTP proxy.
func startLocalProxy(ctx context.Context, port int, host string, wg *sync.WaitGroup) {
	// Decrement the WaitGroup counter when the function completes.
	defer wg.Done()

	// Construct the local address string using the host and port.
	localAddr := fmt.Sprintf("%s:%d", host, port)

	// Create an HTTP client with a timeout of 30 seconds and custom transport settings.
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives:   false,
			MaxIdleConns:        1000,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     120 * time.Second,
		},
	}

	// Create a new HTTP request multiplexer.
	mux := http.NewServeMux()

	// Define a handler function for the root URL path.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Log a message indicating the request being proxied, if debug mode is enabled.
		if debug {
			log.Printf("Proxying request: %s %s", r.Method, r.URL.String())
		}

		// Ensure the request URL has a host.
		if r.URL.Host == "" {
			r.URL.Host = r.Host
		}
		// Set the request URL scheme to HTTP.
		r.URL.Scheme = "http"
		// Set the request host to the URL host.
		r.Host = r.URL.Host

		// Clear the request URI.
		r.RequestURI = ""

		// Remove specific headers from the request.
		r.Header.Del("Connection")
		r.Header.Del("Proxy-Connection")
		r.Header.Del("Transfer-Encoding")
		r.Header.Del("Content-Length")

		// Forward the request using the HTTP client.
		resp, err := client.Do(r)
		if err != nil {
			// Return a 502 Bad Gateway response if the request fails.
			http.Error(w, "Failed to forward request", http.StatusBadGateway)
			// Log an error message if debug mode is enabled.
			if debug {
				log.Printf("Error forwarding request: %v", err)
			}
			return
		}
		defer resp.Body.Close()

		// Copy the response headers to the response writer.
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		// Set additional security headers.
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")

		// Log the response status code if debug mode is enabled.
		if debug {
			log.Printf("Response status: %d", resp.StatusCode)
		}
		// Write the response status code to the response writer.
		w.WriteHeader(resp.StatusCode)

		// Copy the response body to the response writer.
		_, copyErr := io.Copy(w, resp.Body)
		if copyErr != nil && debug {
			// Log an error message if copying the response body fails and debug mode is enabled.
			log.Printf("Error copying response body: %v", copyErr)
		}
	})

	// Create a new HTTP server with the specified address and handler.
	server := &http.Server{
		Addr:    localAddr,
		Handler: mux,
	}

	// Start a goroutine to shut down the server gracefully when the context is done.
	go func() {
		<-ctx.Done()
		server.Shutdown(context.Background())
	}()

	// Log a message indicating the start of the local HTTP proxy, if debug mode is enabled.
	if debug {
		log.Printf("Starting local HTTP proxy on %s", localAddr)
	}
	// Start the HTTP server and listen for incoming connections.
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		// Log a fatal error and exit if the server fails to start.
		log.Fatalf("Failed to start local HTTP proxy: %v", err)
	}
}

// setupLogging sets up logging to a file.
func setupLogging() {
	// Define the log file name.
	logFile := "client.log"

	// Open the log file with create, write-only, and append flags.
	file, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		// Print an error message and exit if the log file cannot be opened.
		fmt.Printf("Failed to open log file %s: %v\n", logFile, err)
		os.Exit(1)
	}

	// Set the log output to the opened file.
	log.SetOutput(file)

	// Set the log flags to include the standard flags and the short file name.
	log.SetFlags(log.LstdFlags | log.Lshortfile)
}

func main() {
	// Set up logging.
	setupLogging()

	// Define command-line flags.
	localPort := flag.String("port", "", "address:port of the existing application to forward to")
	folderPath := flag.String("folder", "", "path to a folder to serve files from")
	flag.IntVar(&rp, "rp", 7645, "Port for client registration (default 7645)")
	serverAddr := flag.String("server", "", "server name (required)")
	useProxy := flag.Bool("proxy", false, "use local HTTP proxy (forwards requests through proxy URL)")
	useTLS := flag.Bool("tls", true, "use TLS for WebSocket and proxy connections (true or false).")
	flag.BoolVar(&debug, "debug", false, "Enable debug logging")
	flag.StringVar(&clientID, "id", "", "Custom client ID for registration") // New ID flag
	flag.Parse()

	// Check required flags.
	if *serverAddr == "" {
		fmt.Println("The --server=name.domain flag is required. Please specify the server address.")
		os.Exit(1)
	}

	if *localPort == "" && *folderPath == "" && *useProxy == false {
		fmt.Println("Specify either --port=N to forward or --folder=/path to serve files.")
		os.Exit(1)
	}
	if *localPort != "" && *folderPath != "" {
		fmt.Println("Specify only one of --port or --folder, not both.")
		os.Exit(1)
	}

	// Initialize state.
	stateMutex.Lock()
	state = &clientState{
		ServerAddr: *serverAddr,
		UseTLS:     *useTLS,
		UseProxy:   *useProxy,
	}
	stateMutex.Unlock()

	// Set up context for cancellation.
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	// Handle Ctrl+C (SIGINT).
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)

	go func() {
		<-stop
		fmt.Println("\nShutting down gracefully...")
		cancel()
	}()

	// Start folder server if specified.
	if *folderPath != "" {
		port, err := getRandomFreePort(8000, 9000)
		if err != nil {
			log.Fatalf("Failed to find a free port: %v", err)
		}
		stateMutex.Lock()
		state.LocalPort = fmt.Sprintf("127.0.0.1:%d", port)
		stateMutex.Unlock()
		wg.Add(1)
		go serveFolderLocally(ctx, *folderPath, state.LocalPort, &wg)
	} else {
		stateMutex.Lock()
		state.LocalPort = *localPort
		stateMutex.Unlock()
	}

	// Register client with custom ID if provided.
	if !registerClient(state) {
		log.Fatal("Failed to register client. Exiting.")
	}

	// Start local proxy if specified.
	if state.UseProxy && state.ProxyURL != "" {
		port, err := getRandomFreePort(8000, 9000)
		if err != nil {
			log.Fatalf("Failed to find a free port: %v", err)
		}
		wg.Add(1)
		go startLocalProxy(ctx, port, "127.0.0.1", &wg)
		stateMutex.Lock()
		state.LocalPort = fmt.Sprintf("127.0.0.1:%d", port)
		stateMutex.Unlock()
	}

	// Start connection forwarding.
	wg.Add(1)
	go connectAndForward(ctx, state, &wg)

	// Wait for all goroutines to finish.
	wg.Wait()

	stateMutex.Lock()
	state = nil
	stateMutex.Unlock()

	log.Println("Client has shut down.")
}
