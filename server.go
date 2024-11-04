package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type clientInfo struct {
	udid                string
	conn                *websocket.Conn
	portTLS             int
	portNoTLS           int
	clientPortTLS       int
	clientPortNoTLS     int
	listenerTLS         net.Listener
	listenerNoTLS       net.Listener
	clientListenerTLS   *http.Server
	clientListenerNoTLS *http.Server
	cancelFunc          context.CancelFunc
	lastActivity        time.Time
	connMutex           sync.Mutex
	wsConnChan          chan *websocket.Conn
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
	maxConcurrentConnections = 2000
	connLimiter              = make(chan struct{}, maxConcurrentConnections)
	usedPorts                = make(map[int]bool)
	portMutex                sync.Mutex
	inactivityTimeout        = 5 * time.Minute

	basePortTLS         = 3000
	basePortNoTLS       = 4000
	baseClientPortTLS   = 7000
	baseClientPortNoTLS = 9000
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

func generatePorts() (int, int, int, int) {
	portMutex.Lock()
	defer portMutex.Unlock()

	portTLS := findAvailablePort(basePortTLS, 1)
	portNoTLS := findAvailablePort(basePortNoTLS, 1)
	clientPortTLS := findAvailablePort(baseClientPortTLS, 1)
	clientPortNoTLS := findAvailablePort(baseClientPortNoTLS, 1)

	usedPorts[portTLS] = true
	usedPorts[portNoTLS] = true
	usedPorts[clientPortTLS] = true
	usedPorts[clientPortNoTLS] = true

	return portTLS, portNoTLS, clientPortTLS, clientPortNoTLS
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

	client.cancelFunc()

	releasePorts(client.portTLS, client.portNoTLS, client.clientPortTLS, client.clientPortNoTLS)
	clientsMutex.Lock()
	delete(clients, clientID)
	clientsMutex.Unlock()
	if debug {
		log.Printf("Resources released for client %s on ports TLS: %d, non-TLS: %d", clientID, client.portTLS, client.portNoTLS)
	}
}

func monitorClientActivity(clientID string, client *clientInfo) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		clientsMutex.Lock()
		if time.Since(client.lastActivity) > inactivityTimeout {
			if debug {
				log.Printf("Client on ports %d (TLS) and %d (non-TLS) inactive, closing connections", client.clientPortTLS, client.clientPortNoTLS)
			}

			client.cancelFunc()

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

	portTLS, portNoTLS, clientPortTLS, clientPortNoTLS := generatePorts()
	ctx, cancel := context.WithCancel(context.Background())

	udid := uuid.New().String()

	client := &clientInfo{
		udid:            udid,
		portTLS:         portTLS,
		portNoTLS:       portNoTLS,
		clientPortTLS:   clientPortTLS,
		clientPortNoTLS: clientPortNoTLS,
		cancelFunc:      cancel,
		lastActivity:    time.Now(),
		wsConnChan:      make(chan *websocket.Conn),
	}

	clientID := udid

	switch connectionType {
	case "tls":
		go waitForProxyConnectionTLS(ctx, client, portTLS)
		go waitForClientConnection(ctx, client.clientPortTLS, true, clientID)
	case "non-tls":
		go waitForProxyConnectionNoTLS(ctx, client, portNoTLS)
		go waitForClientConnection(ctx, client.clientPortNoTLS, false, clientID)
	default:
		http.Error(w, "Invalid connection_type parameter", http.StatusBadRequest)
		return
	}

	clientsMutex.Lock()
	clients[clientID] = client
	clientsMutex.Unlock()

	go monitorClientActivity(clientID, client)

	response := map[string]string{
		"udid":              udid,
		"client_port_tls":   strconv.Itoa(clientPortTLS),
		"client_port_notls": strconv.Itoa(clientPortNoTLS),
	}

	if connectionType == "tls" || connectionType == "both" {
		response["proxy_url_tls"] = fmt.Sprintf("https://%s:%d", domain, portTLS)
	}
	if connectionType == "non-tls" || connectionType == "both" {
		response["proxy_url_notls"] = fmt.Sprintf("%s:%d", domain, portNoTLS)
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("Failed to encode response: %v", err)
	}
}

func waitForProxyConnectionTLS(ctx context.Context, client *clientInfo, proxyPort int) {
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
	client.listenerTLS = listener
	defer listener.Close()

	log.Printf("Secure proxy server listening on port %d (TLS)", proxyPort)
	acceptConnections(ctx, listener, client, handleNewProxyConnectionTLS)
}

func waitForProxyConnectionNoTLS(ctx context.Context, client *clientInfo, proxyPort int) {
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", proxyPort))
	if err != nil {
		log.Printf("Error listening on port %d (non-TLS): %v", proxyPort, err)
		return
	}
	client.listenerNoTLS = listener
	defer listener.Close()

	log.Printf("Proxy server listening on port %d (non-TLS)", proxyPort)
	acceptConnections(ctx, listener, client, handleNewProxyConnectionNoTLS)
}

func acceptConnections(ctx context.Context, listener net.Listener, client *clientInfo, handleFunc func(net.Conn, *clientInfo)) {
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
					if debug {
						log.Printf("Listener for port %d already canceled, exit loop", listener.Addr().(*net.TCPAddr).Port)
					}
					return
				default:
					if debug {
						log.Printf("Failed to accept connection on port %d: %v", listener.Addr().(*net.TCPAddr).Port, err)
					}
					continue
				}
			}

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
	client.connMutex.Lock()
	err := client.conn.WriteJSON(message)
	client.connMutex.Unlock()
	if err != nil {
		if debug {
			log.Printf("Failed to send new_connection message: %v", err)
		}
		return
	}

	select {
	case wsConn := <-client.wsConnChan:
		handleProxyWebSocketConnection(proxyConn, wsConn)
	case <-time.After(30 * time.Second):
		log.Printf("Timeout waiting for client WebSocket connection")
	}
}

func handleNewProxyConnectionNoTLS(proxyConn net.Conn, client *clientInfo) {
	connectionID := uuid.New().String()
	defer proxyConn.Close()

	message := map[string]string{
		"type":         "new_connection",
		"connectionID": connectionID,
		"useTLS":       "false",
	}
	client.connMutex.Lock()
	err := client.conn.WriteJSON(message)
	client.connMutex.Unlock()
	if err != nil {
		if debug {
			log.Printf("Failed to send new_connection message: %v", err)
		}
		return
	}

	select {
	case wsConn := <-client.wsConnChan:
		handleProxyWebSocketConnection(proxyConn, wsConn)
	case <-time.After(30 * time.Second):
		log.Printf("Timeout waiting for client WebSocket connection")
	}
}

func handleProxyWebSocketConnection(proxyConn net.Conn, wsConn *websocket.Conn) {
	defer wsConn.Close()
	defer proxyConn.Close()

	done := make(chan struct{})

	go func() {
		defer func() {
			done <- struct{}{}
		}()
		io.Copy(proxyConn, wsConn.UnderlyingConn())
	}()

	go func() {
		defer func() {
			done <- struct{}{}
		}()
		io.Copy(wsConn.UnderlyingConn(), proxyConn)
	}()

	<-done
	if debug {
		log.Printf("Data forwarding complete for connection %s", proxyConn.LocalAddr())
	}
}

func clientWebSocketHandler(w http.ResponseWriter, r *http.Request) {
	if debug {
		log.Printf("Received request on /client_ws endpoint: %v", r)
	}

	connectionID := r.URL.Query().Get("connectionID")
	clientID := r.URL.Query().Get("clientID")
	if connectionID == "" || clientID == "" {
		log.Printf("Missing connectionID or clientID")
		http.Error(w, "Missing connectionID or clientID", http.StatusBadRequest)
		return
	}

	wsConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Failed to upgrade to WebSocket: %v", err)
		return
	}

	clientsMutex.Lock()
	client, exists := clients[clientID]
	clientsMutex.Unlock()
	if !exists {
		log.Printf("Client not found: %s", clientID)
		wsConn.Close()
		return
	}

	select {
	case client.wsConnChan <- wsConn:
		if debug {
			log.Printf("Accepted new WebSocket connection for client %s, connectionID %s", clientID, connectionID)
		}
	default:
		log.Printf("No proxy connection waiting for client %s, connectionID %s", clientID, connectionID)
		wsConn.Close()
	}
}

func waitForClientConnection(ctx context.Context, clientPort int, useTLS bool, udid string) {

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		if debug {
			log.Printf("Received request on /ws endpoint: %v", r)
			log.Printf("Request Headers: %v", r.Header)
		}

		if r.Header.Get("Upgrade") != "websocket" {
			http.Error(w, "400 Bad Request - Upgrade header missing", http.StatusBadRequest)
			return
		}

		select {
		case connLimiter <- struct{}{}:
			defer func() { <-connLimiter }()
		default:
			http.Error(w, "Server too busy", http.StatusServiceUnavailable)
			return
		}

		wsConn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("Failed to upgrade to WebSocket: %v", err)
			return
		}

		clientsMutex.Lock()
		client, exists := clients[udid]
		if exists {
			client.conn = wsConn
			client.lastActivity = time.Now()
		} else {
			log.Printf("Client not found for UDID %s when setting up WebSocket", udid)
			wsConn.Close()
			clientsMutex.Unlock()
			return
		}
		clientsMutex.Unlock()
		if debug {
			log.Printf("Control WebSocket client connected on port %d", clientPort)
		}

		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()

			for {
				select {
				case <-ticker.C:
					if err := wsConn.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
						if debug {
							log.Printf("Error sending ping: %v", err)
						}
						wsConn.Close()
						return
					}
				case <-ctx.Done():
					wsConn.Close()
					return
				}
			}
		}()

		for {
			_, _, err := wsConn.ReadMessage()
			if err != nil {
				if debug {
					log.Printf("Control WebSocket connection closed: %v", err)
				}
				wsConn.Close()
				return
			}
		}
	})

	mux.HandleFunc("/client_ws", clientWebSocketHandler)

	server := &http.Server{
		Addr:         fmt.Sprintf(":%d", clientPort),
		Handler:      mux,
		ReadTimeout:  120 * time.Second,
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  300 * time.Second,
	}

	go func() {
		if useTLS {
			cert, err := tls.LoadX509KeyPair(certFile, keyFile)
			if err != nil {
				log.Fatalf("Failed to load certificate for port %d: %v", clientPort, err)
			}
			server.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
			log.Printf("Secure WebSocket server listening on port %d", clientPort)
			if err := server.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("Failed to start TLS client server on port %d: %v", clientPort, err)
			}
		} else {
			log.Printf("Non-secure WebSocket server listening on port %d", clientPort)
			if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("Failed to start non-TLS client server on port %d: %v", clientPort, err)
			}
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("Error shutting down client server on port %d: %v", clientPort, err)
	} else {
		if debug {
			log.Printf("The client server on port %d has stopped", clientPort)
		}
	}
}

func main() {
	setupFlags()
	flag.Parse()

	if domain == "" {
		log.Fatal("Flag -domain is required.")
	}

	initCertificates()
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
		if err := tlsServer.ListenAndServeTLS("", ""); err != nil {
			log.Fatalf("Failed to start TLS server: %v", err)
		}
	}()

	select {}
}
