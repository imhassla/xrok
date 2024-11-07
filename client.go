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
		if fileInfo.Size() > 100*1024*1024*1024 { // File size limit: 100 GB
			http.Error(w, "File is too large", http.StatusForbidden)
			return
		}

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

func connectAndForward(ctx context.Context, state *clientState, wg *sync.WaitGroup) {
	defer wg.Done()
	const (
		reconnectInterval = 1 * time.Second
		connReadLimit     = 1024 * 1024
	)

	scheme := "ws"
	if state.UseTLS {
		scheme = "wss"
	}
	u := url.URL{Scheme: scheme, Host: fmt.Sprintf("%s:%s", state.ServerAddr, state.ClientPort), Path: "/ws"}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		conn, resp, err := websocket.DefaultDialer.Dial(u.String(), nil)
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
		conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		conn.SetWriteDeadline(time.Now().Add(120 * time.Second))

		if debug {
			log.Printf("Control connection established")
		}

		go func() {
			defer conn.Close()
			for {
				_, msg, err := conn.ReadMessage()
				if err != nil {
					if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
						log.Printf("WebSocket connection closed normally: %v", err)
						return
					} else {
						if debug {
							log.Printf("Error reading from WebSocket: %v", err)
						}
						break
					}
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
				conn.Close()
				return
			default:
			}

			if err := conn.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
				if debug {
					log.Printf("Lost connection, attempting to reconnect...")
				}
				break
			}
			time.Sleep(3 * time.Second)
		}
	}
}

func establishNewWebSocketConnection(ctx context.Context, state *clientState, connectionID string, useTLS bool) {
	scheme := "ws"
	if useTLS {
		scheme = "wss"
	}

	wsURL := url.URL{
		Scheme: scheme,
		Host:   fmt.Sprintf("%s:%s", state.ServerAddr, state.ClientPort),
		Path:   "/client_ws",
		RawQuery: url.Values{
			"connectionID": {connectionID},
			"clientID":     {clientID},
		}.Encode(),
	}

	wsConn, _, err := websocket.DefaultDialer.Dial(wsURL.String(), nil)
	if err != nil {
		log.Printf("Failed to establish new WebSocket connection: %v", err)
		return
	}

	localConn, err := net.Dial("tcp", state.LocalPort)
	if err != nil {
		log.Printf("Failed to connect to local application: %v", err)
		wsConn.Close()
		return
	}
	if debug {
		log.Printf("New WebSocket connection established for connectionID %s", connectionID)
	}

	go func() {
		defer wsConn.Close()
		defer localConn.Close()

		done := make(chan struct{})
		go func() {
			io.Copy(localConn, wsConn.UnderlyingConn())
			done <- struct{}{}
		}()
		go func() {
			io.Copy(wsConn.UnderlyingConn(), localConn)
			done <- struct{}{}
		}()
		select {
		case <-done:
		case <-ctx.Done():
		}
	}()
}

func registerClient(state *clientState) bool {
	const maxRetries = 3
	const backoffFactor = 500 * time.Millisecond

	connectionType := "tls"
	if !state.UseTLS {
		connectionType = "non-tls"
	}

	log.Printf("Registering client at %s with connection type %s", state.ServerAddr, connectionType)
	fullURL := fmt.Sprintf("https://%s:%d/register_client?connection_type=%s", state.ServerAddr, rp, connectionType)

	client := &http.Client{Timeout: 5 * time.Second}

	var res RegisterResponse
	for attempt := 1; attempt <= maxRetries; attempt++ {
		response, err := client.Post(fullURL, "application/json", nil)
		if err != nil {
			log.Printf("Attempt %d: Registration failed: %v", attempt, err)
			time.Sleep(backoffFactor * time.Duration(attempt))
			continue
		}
		defer response.Body.Close()

		if response.StatusCode != http.StatusOK {
			log.Printf("Attempt %d: Server error, status: %d", attempt, response.StatusCode)
			time.Sleep(backoffFactor * time.Duration(attempt))
			continue
		}

		if err := json.NewDecoder(response.Body).Decode(&res); err != nil {
			log.Printf("Attempt %d: Failed to decode response: %v", attempt, err)
			time.Sleep(backoffFactor * time.Duration(attempt))
			continue
		}

		clientID = res.UDID

		if state.UseTLS {
			state.ProxyURL = res.ProxyURLTLS
			state.ClientPort = res.ClientPortTLS
		} else {
			state.ProxyURL = res.ProxyURLNoTLS
			state.ClientPort = res.ClientPortNoTLS
		}

		if state.ProxyURL == "" || state.ClientPort == "" {
			log.Printf("Attempt %d: Received empty proxy URL or client port", attempt)
			time.Sleep(backoffFactor * time.Duration(attempt))
			continue
		}

		// Print the proxy URL to the console
		fmt.Printf("Proxy URL: %s\n", state.ProxyURL)
		return true
	}

	log.Printf("Registration failed after %d attempts", maxRetries)
	return false
}

func startLocalProxy(ctx context.Context, port int, host string, wg *sync.WaitGroup) {
	defer wg.Done()

	localAddr := fmt.Sprintf("%s:%d", host, port)

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives:   false,
			MaxIdleConns:        1000,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     60 * time.Second,
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if debug {
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
			if debug {
				log.Printf("Error forwarding request: %v", err)
			}
			return
		}
		defer resp.Body.Close()

		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")

		if debug {
			log.Printf("Response status: %d", resp.StatusCode)
		}
		w.WriteHeader(resp.StatusCode)

		_, copyErr := io.Copy(w, resp.Body)
		if copyErr != nil && debug {
			log.Printf("Error copying response body: %v", copyErr)
		}
	})

	server := &http.Server{
		Addr:    localAddr,
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		server.Shutdown(context.Background())
	}()

	if debug {
		log.Printf("Starting local HTTP proxy on %s", localAddr)
	}
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Failed to start local HTTP proxy: %v", err)
	}
}

func setupLogging() {
	logFile := "client.log"
	file, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		fmt.Printf("Failed to open log file %s: %v\n", logFile, err)
		os.Exit(1)
	}
	log.SetOutput(file)
	log.SetFlags(log.LstdFlags | log.Lshortfile)
}

func main() {
	setupLogging()
	localPort := flag.String("port", "", "address:port of the existing application to forward to")
	folderPath := flag.String("folder", "", "path to a folder to serve files from")
	flag.IntVar(&rp, "rp", 7645, "Port for client registration (default 7645)")
	serverAddr := flag.String("server", "", "server name (required)")
	useProxy := flag.Bool("proxy", false, "use local HTTP proxy (forwards requests through proxy URL)")
	useTLS := flag.Bool("tls", true, "use TLS for WebSocket and proxy connections (true or false).")
	flag.BoolVar(&debug, "debug", false, "Enable debug logging")
	flag.Parse()

	// Check required flags
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

	// Initialize state
	stateMutex.Lock()
	state = &clientState{
		ServerAddr: *serverAddr,
		UseTLS:     *useTLS,
		UseProxy:   *useProxy,
	}
	stateMutex.Unlock()

	// Set up context for cancellation
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	// Handle Ctrl+C (SIGINT)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)

	go func() {
		<-stop
		fmt.Println("\nShutting down gracefully...")
		cancel()
	}()

	// Start folder server if specified
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

	// Register client
	if !registerClient(state) {
		log.Fatal("Failed to register client. Exiting.")
	}

	// Start local proxy if specified
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

	// Start connection forwarding
	wg.Add(1)
	go connectAndForward(ctx, state, &wg)

	// Wait for all goroutines to finish
	wg.Wait()

	// Clear in-memory state on exit
	stateMutex.Lock()
	state = nil
	stateMutex.Unlock()

	log.Println("Client has shut down.")
}
