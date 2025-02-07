package main

import (
	"context"
	"crypto/tls"
	"encoding/gob"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type SerializableClientInfo struct {
	UDID            string
	PortNoTLS       int
	ClientPortTLS   int
	ClientPortNoTLS int
	LastActivity    time.Time
	ConnectionType  string
	ConnectionID    string
	Registered      bool
}

type clientInfo struct {
	SerializableClientInfo
	conn                *websocket.Conn
	listenerTLS         net.Listener
	listenerNoTLS       net.Listener
	clientListenerTLS   *http.Server
	clientListenerNoTLS *http.Server
	cancelFunc          context.CancelFunc
	connMutex           sync.Mutex
	wsConnChans         map[string]chan *websocket.Conn
}

var (
	rp                       int
	debug                    bool
	clients                  = make(map[string]*clientInfo)
	clientsMutex             sync.Mutex
	domain                   string
	certFile                 string
	keyFile                  string
	upgrader                 = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	maxConcurrentConnections = 5000
	connLimiter              = make(chan struct{}, maxConcurrentConnections)
	usedPorts                = make(map[int]bool)
	portMutex                sync.Mutex
	inactivityTimeout        = 1000 * time.Minute

	basePortNoTLS     = 4000
	baseClientPortTLS = 7000

	stateFile = "state.dat"
	logFile   = "server.log"
)

func setupFlags() {
	flag.StringVar(&domain, "domain", "", "Domain to use in URLs (required)")
	flag.BoolVar(&debug, "debug", false, "Enable debug logging")
	flag.IntVar(&rp, "rp", 7645, "Port for client registration (default 7645)")
}

func initCertificates() {
	certFile = fmt.Sprintf("/etc/letsencrypt/live/%s/fullchain.pem", domain)
	keyFile = fmt.Sprintf("/etc/letsencrypt/live/%s/privkey.pem", domain)
}

func generatePorts() (int, int) {
	portMutex.Lock()
	defer portMutex.Unlock()

	portNoTLS := findAvailablePort(basePortNoTLS, 1)
	clientPortTLS := findAvailablePort(baseClientPortTLS, 1)

	usedPorts[portNoTLS] = true
	usedPorts[clientPortTLS] = true

	return portNoTLS, clientPortTLS
}

func findAvailablePort(basePort, step int) int {
	for port := basePort; ; port += step {
		if !usedPorts[port] {
			return port
		}
	}
}

func releasePorts(ports ...int) {
	portMutex.Lock()
	defer portMutex.Unlock()
	for _, port := range ports {
		delete(usedPorts, port)
	}
}

func closeClientConnections(clientID string, client *clientInfo) {

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

	releasePorts(client.PortNoTLS, client.ClientPortTLS, client.ClientPortNoTLS)
	clientsMutex.Lock()
	delete(clients, clientID)
	clientsMutex.Unlock()
	if debug {
		log.Printf("Resources released for client %s on ports TLS: %d, non-TLS: %d", clientID, client.PortNoTLS)
	}
}

func monitorClientActivity(clientID string, client *clientInfo) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		clientsMutex.Lock()
		if time.Since(client.LastActivity) > inactivityTimeout {
			if debug {
				log.Printf("Client on ports %d (TLS) and %d (non-TLS) inactive, closing connections", client.ClientPortTLS, client.ClientPortNoTLS)
			}

			if client.cancelFunc != nil {
				client.cancelFunc()
			}

			go func() {
				closeClientConnections(clientID, client)
			}()
			clientsMutex.Unlock()
			return
		}
		clientsMutex.Unlock()
	}
}

func registerClientHandler(w http.ResponseWriter, r *http.Request) {
	connectionType := r.URL.Query().Get("connection_type")
	if connectionType == "" {
		http.Error(w, "connection_type parameter is required", http.StatusBadRequest)
		return
	}

	customID := r.URL.Query().Get("id")

	portNoTLS, clientPortTLS := generatePorts()
	ctx, cancel := context.WithCancel(context.Background())

	clientID := customID
	if clientID == "" {
		clientID = uuid.New().String()
	}

	client := &clientInfo{
		SerializableClientInfo: SerializableClientInfo{
			UDID:           clientID,
			PortNoTLS:      portNoTLS,
			ClientPortTLS:  clientPortTLS,
			LastActivity:   time.Now(),
			ConnectionType: connectionType,
			Registered:     true,
		},
		cancelFunc:  cancel,
		wsConnChans: make(map[string]chan *websocket.Conn),
	}

	clientsMutex.Lock()
	if _, exists := clients[clientID]; exists && customID != "" {
		clientsMutex.Unlock()
		http.Error(w, "Client ID already exists", http.StatusConflict)
		return
	}
	clients[clientID] = client
	clientsMutex.Unlock()

	go waitForClientConnection(ctx, client, client.ClientPortTLS, true, clientID)

	switch connectionType {
	case "tls":
	case "non-tls":
		go waitForProxyConnectionNoTLS(ctx, client, client.PortNoTLS)
	default:
		http.Error(w, "Invalid connection_type parameter", http.StatusBadRequest)
		return
	}

	go monitorClientActivity(clientID, client)
	saveState()

	response := map[string]string{
		"udid":            clientID,
		"client_port_tls": strconv.Itoa(clientPortTLS),
		"proxy_url_notls": fmt.Sprintf("%s:%d", domain, portNoTLS),
	}

	if connectionType == "tls" {
		response["proxy_url_tls"] = fmt.Sprintf("https://%s.%s", clientID, domain)
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("Failed to encode response: %v", err)
	}
}

func extractClientIDFromHost(host string) string {
	parts := strings.Split(host, ".")
	if len(parts) < 2 {
		return ""
	}
	return parts[0] // The client ID assumed to be the first part of the domain
}

func waitForProxyConnectionTLS(ctx context.Context, proxyPort int) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		log.Printf("Failed to load certificate for port %d: %v", proxyPort, err)
		return
	}

	listener, err := tls.Listen("tcp", fmt.Sprintf(":%d", proxyPort), &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		log.Printf("Error listening on port %d (TLS): %v", proxyPort, err)
		return
	}
	defer listener.Close()

	log.Printf("Secure proxy server listening on port %d (TLS)", proxyPort)

	for {
		proxyConn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				log.Printf("Failed to accept connection on port %d: %v", proxyPort, err)
				continue
			}
		}

		go func(conn net.Conn) {
			defer conn.Close()

			tlsConn, ok := conn.(*tls.Conn)
			if !ok {
				log.Printf("Connection is not a TLS connection")
				return
			}

			if err := tlsConn.Handshake(); err != nil {
				log.Printf("TLS handshake failed: %v", err)
				return
			}

			clientID := extractClientIDFromHost(tlsConn.ConnectionState().ServerName)
			if clientID == "" {
				log.Printf("ClientID not found in subdomain")
				return
			}

			clientsMutex.Lock()
			client, exists := clients[clientID]
			clientsMutex.Unlock()

			if !exists {
				log.Printf("Client not found for subdomain: %s", clientID)
				return
			}

			handleNewProxyConnectionTLS(tlsConn, client)
		}(proxyConn)
	}
}

func waitForProxyConnectionNoTLS(ctx context.Context, client *clientInfo, proxyPort int) {
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
		} else {
			log.Printf("Non-TLS listener on port %d closed successfully", proxyPort)
		}
	}()

	log.Printf("Non-TLS proxy server successfully listening on port %d for client %s", proxyPort, client.UDID)

	acceptConnections(ctx, listener, client, handleNewProxyConnectionNoTLS)
}

func acceptConnections(ctx context.Context, listener net.Listener, client *clientInfo, handleFunc func(net.Conn, *clientInfo)) {
	log.Printf("Starting to accept connections on port %d", listener.Addr().(*net.TCPAddr).Port)
	var wg sync.WaitGroup

	for {
		select {
		case <-ctx.Done():
			log.Printf("Shutting down listener on port %d", listener.Addr().(*net.TCPAddr).Port)
			if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				log.Printf("Error while closing listener on port %d: %v", listener.Addr().(*net.TCPAddr).Port, err)
			}

			wg.Wait()
			return

		default:
			proxyConn, err := listener.Accept()
			if err != nil {
				select {
				case <-ctx.Done():
					log.Printf("Listener for port %d already canceled, exit loop", listener.Addr().(*net.TCPAddr).Port)
					return
				default:
					log.Printf("Failed to accept connection on port %d: %v", listener.Addr().(*net.TCPAddr).Port, err)
					continue
				}
			}

			log.Printf("Accepted connection on port %d for client %s", listener.Addr().(*net.TCPAddr).Port, client.UDID)
			wg.Add(1)
			go func() {
				defer wg.Done()
				handleFunc(proxyConn, client)
			}()
		}
	}
}

func handleNewProxyConnectionTLS(proxyConn net.Conn, client *clientInfo) {
	connectionID := uuid.New().String()
	defer proxyConn.Close()

	message := map[string]string{
		"type":         "new_connection",
		"connectionID": connectionID,
		"useTLS":       "true",
	}

	ch := make(chan *websocket.Conn)

	client.connMutex.Lock()
	client.wsConnChans[connectionID] = ch
	client.connMutex.Unlock()

	clientsMutex.Lock()
	client.LastActivity = time.Now()
	clientsMutex.Unlock()

	if client.conn == nil {
		log.Printf("WebSocket connection for client %s is not established, aborting connection ID %s", client.UDID, connectionID)
		client.connMutex.Lock()
		delete(client.wsConnChans, connectionID)
		client.connMutex.Unlock()
		return
	}

	err := client.conn.WriteJSON(message)
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
			log.Printf("Received nil WebSocket connection for connectionID %s, aborting", connectionID)
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

func handleNewProxyConnectionNoTLS(proxyConn net.Conn, client *clientInfo) {
	connectionID := uuid.New().String()
	defer func() {
		if err := proxyConn.Close(); err != nil {
			log.Printf("Error closing proxy connection with ID %s: %v", connectionID, err)
		}
	}()

	log.Printf("Setting up new non-TLS proxy connection with ID: %s", connectionID)

	ch := make(chan *websocket.Conn)

	client.connMutex.Lock()
	if client.conn == nil {
		client.connMutex.Unlock()
		log.Printf("WebSocket connection for client %s is not established, aborting connection ID %s", client.UDID, connectionID)
		return
	}

	client.wsConnChans[connectionID] = ch

	err := client.conn.WriteJSON(map[string]string{
		"type":         "new_connection",
		"connectionID": connectionID,
		"useTLS":       "false",
	})

	client.connMutex.Unlock()

	if err != nil {
		log.Printf("Failed to send new_connection message for non-TLS connection ID %s: %v", connectionID, err)
		client.connMutex.Lock()
		delete(client.wsConnChans, connectionID)
		client.connMutex.Unlock()
		return
	}

	log.Printf("Awaiting WebSocket connection for connectionID %s", connectionID)

	select {
	case wsConn := <-ch:
		if wsConn == nil {
			log.Printf("Received nil WebSocket connection for connectionID %s, aborting", connectionID)
			client.connMutex.Lock()
			delete(client.wsConnChans, connectionID)
			client.connMutex.Unlock()
			return
		}
		log.Printf("Non-TLS WebSocket connection established for connectionID %s", connectionID)
		handleProxyWebSocketConnection(proxyConn, wsConn, client)
		client.connMutex.Lock()
		delete(client.wsConnChans, connectionID)
		client.connMutex.Unlock()
	case <-time.After(60 * time.Second):
		log.Printf("Timeout waiting for non-TLS WebSocket connection for ID %s", connectionID)
		client.connMutex.Lock()
		delete(client.wsConnChans, connectionID)
		client.connMutex.Unlock()
	}
}

func handleProxyWebSocketConnection(proxyConn net.Conn, wsConn *websocket.Conn, client *clientInfo) {
	defer wsConn.Close()
	defer proxyConn.Close()

	done := make(chan struct{})
	errChan := make(chan error, 2)

	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := wsConn.UnderlyingConn().Read(buf)
			if n > 0 {
				if _, writeErr := proxyConn.Write(buf[:n]); writeErr != nil {
					errChan <- fmt.Errorf("error writing to proxy connection: %w", writeErr)
					return
				}
				clientsMutex.Lock()
				client.LastActivity = time.Now()
				clientsMutex.Unlock()
			}
			if err != nil {
				if err != io.EOF {
					errChan <- fmt.Errorf("error reading from WebSocket: %w", err)
				}
				break
			}
		}
		done <- struct{}{}
	}()

	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := proxyConn.Read(buf)
			if n > 0 {
				if _, writeErr := wsConn.UnderlyingConn().Write(buf[:n]); writeErr != nil {
					errChan <- fmt.Errorf("error writing to WebSocket: %w", writeErr)
					return
				}
				clientsMutex.Lock()
				client.LastActivity = time.Now()
				clientsMutex.Unlock()
			}
			if err != nil {
				if err != io.EOF {
					errChan <- fmt.Errorf("error reading from proxy connection: %w", err)
				}
				break
			}
		}
		done <- struct{}{}
	}()

	select {
	case <-done:
		log.Printf("Data forwarding completed between WebSocket and proxy connection")
	case err := <-errChan:
		log.Printf("Data forwarding encountered an error: %v", err)
	}

	log.Printf("Closing connection between WebSocket and proxy")
}

// Constants for WebSocket ping/pong handling
const (
	pongWait   = 60 * time.Second
	pingPeriod = (pongWait * 9) / 10 // Send pings at this period
)

// waitForClientConnection sets up a WebSocket server for a client and manages the connection lifecycle.
func waitForClientConnection(ctx context.Context, client *clientInfo, clientPort int, useTLS bool, udid string) {

	// Create a new HTTP request multiplexer (router)
	mux := http.NewServeMux()

	// Handle the "/ws" endpoint for control WebSocket connections
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		if debug {
			log.Printf("Received request on /ws endpoint: %v", r)
			log.Printf("Request Headers: %v", r.Header)
		}

		// Check if the request is a WebSocket upgrade request
		if r.Header.Get("Upgrade") != "websocket" {
			http.Error(w, "400 Bad Request - Upgrade header missing", http.StatusBadRequest)
			return
		}

		// Limit the number of concurrent connections using connLimiter
		select {
		case connLimiter <- struct{}{}:
			// Release the slot in connLimiter when the function returns
			defer func() { <-connLimiter }()
		default:
			log.Printf("Server too busy, connLimiter is full")
			http.Error(w, "Server too busy", http.StatusServiceUnavailable)
			return
		}

		// Upgrade the HTTP connection to a WebSocket connection
		wsConn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("Failed to upgrade to WebSocket: %v", err)
			return
		}

		// Set the read deadline for the WebSocket connection
		wsConn.SetReadDeadline(time.Now().Add(pongWait))

		// Set the Pong handler to reset the read deadline upon receiving a Pong message
		wsConn.SetPongHandler(func(string) error {
			wsConn.SetReadDeadline(time.Now().Add(pongWait))
			// Update the client's last activity timestamp
			clientsMutex.Lock()
			client.LastActivity = time.Now()
			clientsMutex.Unlock()
			return nil
		})

		// Update client information with the new WebSocket connection
		clientsMutex.Lock()
		client.connMutex.Lock()
		client.conn = wsConn
		client.connMutex.Unlock()
		client.LastActivity = time.Now()
		clientsMutex.Unlock()
		if debug {
			log.Printf("Control WebSocket client connected on port %d", clientPort)
		}

		// Start a goroutine to send Ping messages periodically
		go func() {
			// Create a ticker to send Ping messages at regular intervals
			ticker := time.NewTicker(pingPeriod)
			defer ticker.Stop()

			for {
				select {
				case <-ticker.C:
					// Send a Ping message to the client
					if err := wsConn.WriteMessage(websocket.PingMessage, nil); err != nil {
						log.Printf("Error sending ping: %v", err)
						wsConn.Close()
						return
					}
					if debug {
						log.Printf("Ping sent successfully")
					}
				case <-ctx.Done():
					// Context has been canceled; close the WebSocket connection
					wsConn.Close()
					return
				}
			}
		}()

		// Listen for incoming messages from the client
		for {
			// Read a message from the WebSocket connection
			_, _, err := wsConn.ReadMessage()
			if err != nil {
				// An error occurred while reading (e.g., connection closed or timeout)
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					log.Printf("Unexpected WebSocket closure: %v", err)
				}
				wsConn.Close()
				return
			}
			// No need to handle messages here; Pong messages are handled by the Pong handler
		}
	})

	// Handle the "/client_ws" endpoint for client WebSocket connections
	mux.HandleFunc("/client_ws", clientWebSocketHandler)

	// Create the HTTP server
	server := &http.Server{
		Addr:         fmt.Sprintf(":%d", clientPort),
		Handler:      mux,
		ReadTimeout:  5 * time.Minute,
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  5 * time.Minute,
	}

	// Store the server in the client information for later shutdown
	if useTLS {
		client.clientListenerTLS = server
	} else {
		client.clientListenerNoTLS = server
	}

	// Start the server in a new goroutine
	go func() {
		if useTLS {
			// Load the TLS certificate and key
			cert, err := tls.LoadX509KeyPair(certFile, keyFile)
			if err != nil {
				log.Fatalf("Failed to load certificate for port %d: %v", clientPort, err)
			}
			// Set up TLS configuration
			server.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
			log.Printf("Secure WebSocket server listening on port %d", clientPort)
			// Start the HTTPS server with TLS
			if err := server.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("Failed to start TLS client server on port %d: %v", clientPort, err)
			}
		} else {
			log.Printf("WebSocket server listening on port %d", clientPort)
			// Start the HTTP server without TLS
			if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("Failed to start client server on port %d: %v", clientPort, err)
			}
		}
	}()

	// Wait until the context is canceled
	<-ctx.Done()
	// Create a context with timeout for server shutdown
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	// Attempt to gracefully shut down the server
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("Error shutting down client server on port %d: %v", clientPort, err)
	} else {
		if debug {
			log.Printf("The client server on port %d has stopped", clientPort)
		}
	}
}

func saveState() {
	clientsMutex.Lock()
	defer clientsMutex.Unlock()

	state := struct {
		Clients   map[string]SerializableClientInfo
		UsedPorts map[int]bool
	}{
		Clients:   make(map[string]SerializableClientInfo),
		UsedPorts: make(map[int]bool),
	}

	for id, client := range clients {
		state.Clients[id] = client.SerializableClientInfo
	}

	portMutex.Lock()
	for port, used := range usedPorts {
		state.UsedPorts[port] = used
	}
	portMutex.Unlock()

	file, err := os.Create(stateFile)
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

func loadState() {
	file, err := os.Open(stateFile)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("Failed to open state file: %v", err)
		}
		return
	}
	defer file.Close()

	state := struct {
		Clients   map[string]SerializableClientInfo
		UsedPorts map[int]bool
	}{}

	decoder := gob.NewDecoder(file)
	if err := decoder.Decode(&state); err != nil {
		log.Printf("Failed to decode state: %v", err)
		return
	}

	clientsMutex.Lock()
	for id, clientData := range state.Clients {
		client := &clientInfo{
			SerializableClientInfo: clientData,
			wsConnChans:            make(map[string]chan *websocket.Conn),
		}
		clients[id] = client

		ctx, cancel := context.WithCancel(context.Background())
		client.cancelFunc = cancel

		go waitForClientConnection(ctx, client, client.ClientPortTLS, true, id)

		if client.ConnectionType == "non-tls" {
			go waitForProxyConnectionNoTLS(ctx, client, client.PortNoTLS)
		}

		go monitorClientActivity(id, client)
	}
	clientsMutex.Unlock()

	portMutex.Lock()
	for port, used := range state.UsedPorts {
		usedPorts[port] = used
	}
	portMutex.Unlock()
}

func main() {
	setupLogging()
	setupFlags()
	flag.Parse()

	if domain == "" {
		log.Fatal("Flag -domain is required.")
	}

	initCertificates()
	loadState()

	go func() {
		waitForProxyConnectionTLS(context.Background(), 443)
	}()

	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("Recovered from panic: %v", r)
					saveState()
					log.Printf("Restarting server...")
				}
			}()
			runServer()
		}()
		time.Sleep(1 * time.Second)
	}
}

func clientWebSocketHandler(w http.ResponseWriter, r *http.Request) {
	if debug {
		log.Printf("Received request on /client_ws endpoint: %v", r)
	}

	connectionID := r.URL.Query().Get("connectionID")
	clientID := r.URL.Query().Get("clientID")
	if connectionID == "" || clientID == "" {
		log.Printf("Missing connectionID or clientID in request")
		http.Error(w, "Missing connectionID or clientID", http.StatusBadRequest)
		return
	}

	if r.Header.Get("Upgrade") != "websocket" {
		http.Error(w, "400 Bad Request - WebSocket upgrade required", http.StatusBadRequest)
		return
	}

	wsConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Failed to upgrade to WebSocket: %v", err)
		return
	}

	log.Printf("Upgraded to WebSocket on /client_ws for connectionID: %s, clientID: %s", connectionID, clientID)

	clientsMutex.Lock()
	client, exists := clients[clientID]
	clientsMutex.Unlock()
	if !exists {
		log.Printf("Client not found: %s", clientID)
		wsConn.Close()
		return
	}

	client.connMutex.Lock()
	ch, exists := client.wsConnChans[connectionID]
	client.connMutex.Unlock()
	if !exists {
		log.Printf("No waiting connection for connectionID %s", connectionID)
		wsConn.Close()
		return
	}

	ch <- wsConn

	clientsMutex.Lock()
	client.LastActivity = time.Now()
	clientsMutex.Unlock()
}

func runServer() {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		log.Fatalf("Failed to load main certificate: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/register_client", registerClientHandler)
	mux.HandleFunc("/client_ws", clientWebSocketHandler)

	tlsServer := &http.Server{
		Addr:         fmt.Sprintf(":%d", rp),
		Handler:      mux,
		TLSConfig:    &tls.Config{Certificates: []tls.Certificate{cert}},
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  600 * time.Second,
	}

	go func() {
		log.Printf("Listening on port %d...", rp)
		if err := tlsServer.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("Failed to start TLS server: %v", err)
		}
	}()

	select {}
}

func setupLogging() {
	file, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		fmt.Printf("Failed to open log file %s: %v", logFile, err)
		os.Exit(1)
	}
	log.SetOutput(file)
	log.SetFlags(log.LstdFlags | log.Lshortfile)
}
