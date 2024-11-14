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
	UDID            string `json:"udid"`
	ProxyURLTLS     string `json:"proxy_url_tls"`
	ProxyURLNoTLS   string `json:"proxy_url_notls"`
	ClientPortTLS   string `json:"client_port_tls"`
	ClientPortNoTLS string `json:"client_port_notls"`
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
	// Decrement the WaitGroup counter when the function completes.
	defer wg.Done()

	// Define constants for reconnection interval, connection read limit, and connection timeout.
	const (
		reconnectInterval = 6 * time.Second
		connReadLimit     = 1024 * 1024
		connTimeout       = 3 * time.Second
	)

	// Use the secure WebSocket scheme (wss).
	scheme := "wss"

	// Construct the WebSocket URL using the server address and client port from the state.
	u := url.URL{Scheme: scheme, Host: fmt.Sprintf("%s:%s", state.ServerAddr, state.ClientPort), Path: "/ws"}

	// Infinite loop to continuously attempt to establish a WebSocket connection.
	for {
		select {
		case <-ctx.Done():
			// Exit the loop if the context is done (e.g., due to a cancellation signal).
			return
		default:
			// Continue with the connection attempt if the context is not done.
		}

		// Create a WebSocket dialer with the specified handshake timeout.
		dialer := websocket.Dialer{HandshakeTimeout: connTimeout}

		// Attempt to establish a WebSocket connection.
		conn, resp, err := dialer.Dial(u.String(), nil)
		if err != nil {
			// If the connection attempt fails, log the error and wait before retrying.
			if resp != nil {
				log.Printf("WebSocket connection failed with status: %d %s", resp.StatusCode, resp.Status)
			} else {
				log.Printf("WebSocket connection failed: %v", err)
			}
			time.Sleep(reconnectInterval)
			continue
		}

		// Set the read limit for the WebSocket connection.
		conn.SetReadLimit(connReadLimit)

		// Log a message indicating that the control connection has been established, if debug mode is enabled.
		if debug {
			log.Printf("Control connection established")
		}

		// Handle the WebSocket connection.
		handleWebSocketConnection(ctx, conn, state)

		// Close the WebSocket connection.
		conn.Close()

		// Wait before attempting to reconnect.
		time.Sleep(reconnectInterval)
	}
}

// handleWebSocketConnection handles incoming WebSocket messages.
func handleWebSocketConnection(ctx context.Context, conn *websocket.Conn, state *clientState) {
	// Define a constant for the write wait timeout.
	const (
		writeWait = 10 * time.Second
	)

	// Set the ping handler for the WebSocket connection.
	conn.SetPingHandler(func(appData string) error {
		// Log a message indicating that a ping was received from the server, if debug mode is enabled.
		if debug {
			log.Printf("Received ping from server")
		}
		// Send a pong message in response to the ping.
		return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(writeWait))
	})

	// Set the pong handler for the WebSocket connection.
	conn.SetPongHandler(func(appData string) error {
		// Log a message indicating that a pong was received from the server, if debug mode is enabled.
		if debug {
			log.Printf("Received pong from server")
		}
		return nil
	})

	// Start a goroutine to read messages from the WebSocket connection.
	go func() {
		for {
			// Read a message from the WebSocket connection.
			_, msg, err := conn.ReadMessage()
			if err != nil {
				// Log an error message and return if there is an error reading from the WebSocket.
				log.Printf("Error reading from WebSocket: %v", err)
				return
			}

			// Unmarshal the message into a control message map.
			var controlMsg map[string]string
			if err := json.Unmarshal(msg, &controlMsg); err != nil {
				// Log an error message and continue if the control message is invalid.
				log.Printf("Invalid control message: %v", err)
				continue
			}

			// Check if the control message type is "new_connection".
			if controlMsg["type"] == "new_connection" {
				// Extract the connection ID and useTLS flag from the control message.
				connectionID := controlMsg["connectionID"]
				useTLSStr := controlMsg["useTLS"]
				useTLSConn := useTLSStr == "true"
				// Start a new goroutine to establish a new WebSocket connection for data forwarding.
				go establishNewWebSocketConnection(ctx, state, connectionID, useTLSConn)
			}
		}
	}()

	// Infinite loop to periodically send ping messages to the server.
	for {
		select {
		case <-ctx.Done():
			// Close the WebSocket connection and return if the context is done.
			conn.Close()
			return
		default:
			// Send a ping message to the server.
			if err := conn.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
				// Log an error message and return if there is an error sending the ping message.
				log.Printf("Lost connection, attempting to reconnect...")
				return
			}
			// Wait for 5 seconds before sending the next ping message.
			time.Sleep(5 * time.Second)
		}
	}
}

// establishNewWebSocketConnection establishes a new WebSocket connection for data forwarding.
func establishNewWebSocketConnection(ctx context.Context, state *clientState, connectionID string, useTLS bool) {
	// Use the secure WebSocket scheme (wss).
	scheme := "wss"

	// Construct the WebSocket URL using the server address, client port, and query parameters.
	wsURL := url.URL{
		Scheme: scheme,
		Host:   fmt.Sprintf("%s:%s", state.ServerAddr, state.ClientPort),
		Path:   "/client_ws",
		RawQuery: url.Values{
			"connectionID": {connectionID},
			"clientID":     {clientID},
		}.Encode(),
	}

	// Create a WebSocket dialer with a handshake timeout of 10 seconds.
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}

	// Attempt to establish a new WebSocket connection.
	wsConn, _, err := dialer.Dial(wsURL.String(), nil)
	if err != nil {
		// Log an error message and return if the connection attempt fails.
		log.Printf("Failed to establish new WebSocket connection for connectionID %s: %v", connectionID, err)
		return
	}

	// Attempt to establish a TCP connection to the local application.
	localConn, err := net.Dial("tcp", state.LocalPort)
	if err != nil {
		// Log an error message, close the WebSocket connection, and return if the TCP connection attempt fails.
		log.Printf("Failed to connect to local application for connectionID %s: %v", connectionID, err)
		wsConn.Close()
		return
	}

	// Log a message indicating that the new WebSocket connection has been established.
	log.Printf("New WebSocket connection established for connectionID %s", connectionID)

	// Handle data forwarding between the WebSocket connection and the local TCP connection.
	handleDataForwarding(wsConn, localConn, connectionID)
}

// handleDataForwarding handles data forwarding between WebSocket and local connection.
func handleDataForwarding(wsConn *websocket.Conn, localConn net.Conn, connectionID string) {
	// Ensure that both the WebSocket connection and the local TCP connection are closed when the function completes.
	defer wsConn.Close()
	defer localConn.Close()

	// Create a channel to signal when data forwarding is complete.
	done := make(chan struct{})

	// Start a goroutine to copy data from the WebSocket connection to the local TCP connection.
	go func() {
		io.Copy(localConn, wsConn.UnderlyingConn())
		done <- struct{}{}
	}()

	// Start a goroutine to copy data from the local TCP connection to the WebSocket connection.
	go func() {
		io.Copy(wsConn.UnderlyingConn(), localConn)
		done <- struct{}{}
	}()

	// Wait for either of the goroutines to complete.
	select {
	case <-done:
		// Log a message indicating that data forwarding has completed for the specified connection ID.
		log.Printf("Data forwarding completed for connectionID %s", connectionID)
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
	client := &http.Client{Timeout: 10 * time.Second}

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

		// Update the state with the appropriate proxy URL and client port based on the connection type.
		if state.UseTLS {
			state.ProxyURL = res.ProxyURLTLS
			state.ClientPort = res.ClientPortTLS
		} else {
			state.ProxyURL = res.ProxyURLNoTLS
			state.ClientPort = res.ClientPortNoTLS
		}

		// Check if the proxy URL or client port is empty.
		if state.ProxyURL == "" || state.ClientPort == "" {
			// Log an error message and wait before retrying if the proxy URL or client port is empty.
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

	// Clear in-memory state on exit.
	stateMutex.Lock()
	state = nil
	stateMutex.Unlock()

	log.Println("Client has shut down.")
}
