package main

import (
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
	"time"

	"github.com/gorilla/websocket"
)

var (
	rp       int
	debug    bool
	clientID string
)

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

func serveFolderLocally(folderPath string, localPort string) {
	absPath, err := filepath.Abs(folderPath)
	if err != nil {
		log.Fatalf("Invalid folder path: %s", folderPath)
	}
	if _, err := os.Stat(absPath); os.IsNotExist(err) {
		log.Fatalf("Directory %s does not exist", absPath)
	}

	fileServer := http.FileServer(http.Dir(absPath))
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		filePath := absPath + r.URL.Path

		fileInfo, err := os.Stat(filePath)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if fileInfo.Size() > 100*1024*1024*1024 { // Fifile size lmit: 100 GB
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

	log.Printf("Serving folder %s on %s", absPath, localPort)
	if err := http.ListenAndServe(localPort, nil); err != nil {
		log.Fatalf("Failed to start local file server: %v", err)
	}
}

func connectAndForward(localPort, serverAddr, clientPort, proxyURL string, useTLS bool) {
	const (
		reconnectInterval = 1 * time.Second
		connReadLimit     = 1024 * 1024
	)

	scheme := "ws"
	if useTLS {
		scheme = "wss"
	}
	u := url.URL{Scheme: scheme, Host: fmt.Sprintf("%s:%s", serverAddr, clientPort), Path: "/ws"}

	for {
		time.Sleep(reconnectInterval)

		conn, resp, err := websocket.DefaultDialer.Dial(u.String(), nil)
		if err != nil {
			if resp != nil {
				log.Printf("WebSocket connection failed with status: %d %s", resp.StatusCode, resp.Status)
			} else {
				log.Printf("WebSocket connection failed: %v", err)
			}
			continue
		}

		conn.SetReadLimit(connReadLimit)
		conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		conn.SetWriteDeadline(time.Now().Add(120 * time.Second))

		log.Printf("Control connection established")

		go func() {
			for {
				_, msg, err := conn.ReadMessage()
				if err != nil {
					if debug {
						log.Printf("Error reading from control WebSocket: %v", err)
					}
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
					go establishNewWebSocketConnection(localPort, serverAddr, clientPort, connectionID, useTLSConn)
				}
			}
		}()

		select {}
	}
}

func establishNewWebSocketConnection(localPort, serverAddr, clientPort, connectionID string, useTLS bool) {
	scheme := "ws"
	if useTLS {
		scheme = "wss"
	}

	wsURL := url.URL{
		Scheme: scheme,
		Host:   fmt.Sprintf("%s:%s", serverAddr, clientPort),
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

	localConn, err := net.Dial("tcp", localPort)
	if err != nil {
		log.Printf("Failed to connect to local application: %v", err)
		wsConn.Close()
		return
	}

	log.Printf("New WebSocket connection established for connectionID %s", connectionID)

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
		<-done
	}()
}

func registerClient(serverURL string, useTLS bool) (string, string) {
	const maxRetries = 3
	const backoffFactor = 500 * time.Millisecond

	connectionType := "tls"
	if !useTLS {
		connectionType = "non-tls"
	}

	log.Printf("Registering client at %s with connection type %s", serverURL, connectionType)
	fullURL := fmt.Sprintf("%s?connection_type=%s", serverURL, connectionType)

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

		proxyURL := res.ProxyURLTLS
		clientPort := res.ClientPortTLS
		if !useTLS {
			proxyURL = res.ProxyURLNoTLS
			clientPort = res.ClientPortNoTLS
		}

		if proxyURL == "" || clientPort == "" {
			log.Printf("Attempt %d: Received empty proxy URL or client port", attempt)
			time.Sleep(backoffFactor * time.Duration(attempt))
			continue
		}

		log.Printf("proxy URL: %s", proxyURL)
		return proxyURL, clientPort
	}

	log.Printf("Registration failed after %d attempts", maxRetries)
	return "", ""
}

func startLocalProxy(port int, host string) {
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

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
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

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	go func() {
		<-stop
		log.Println("Shutting down the local HTTP proxy server")
		os.Exit(0)
	}()

	if debug {
		log.Printf("Starting local HTTP proxy on %s", localAddr)
	}
	if err := http.ListenAndServe(localAddr, nil); err != nil {
		log.Fatalf("Failed to start local HTTP proxy: %v", err)
	}
}

func main() {
	localPort := flag.String("port", "", "address:port of the existing application to forward to")
	folderPath := flag.String("folder", "", "path to a folder to serve files from")
	flag.IntVar(&rp, "rp", 7645, "Port for client registration (default 7645)")
	serverAddr := flag.String("server", "", "server name (required)")
	useProxy := flag.Bool("proxy", false, "use local HTTP proxy (forwards requests through proxy URL)")
	useTLS := flag.Bool("tls", true, "use TLS for WebSocket and proxy connections (true or false).")
	flag.BoolVar(&debug, "debug", false, "Enable debug logging")
	flag.Parse()

	if *serverAddr == "" {
		log.Fatal("The --server=name.domain flag is required. Please specify the server address.")
	}

	if *localPort == "" && *folderPath == "" && *useProxy == false {
		log.Fatal("Specify either --port=N to forward or --folder=/path to serve files.")
	}
	if *localPort != "" && *folderPath != "" {
		log.Fatal("Specify only one of --port or --folder, not both.")
	}

	if *folderPath != "" {
		port, err := getRandomFreePort(8000, 9000)
		if err != nil {
			log.Fatalf("Failed to find a free port: %v", err)
		}
		*localPort = fmt.Sprintf("127.0.0.1:%d", port)
		go serveFolderLocally(*folderPath, *localPort)
	}

	serverURL := fmt.Sprintf("https://%s:%d/register_client", *serverAddr, rp)
	proxyURL, clientPort := registerClient(serverURL, *useTLS)

	if proxyURL != "" && clientPort != "" && *useProxy == false {
		go connectAndForward(*localPort, *serverAddr, clientPort, proxyURL, *useTLS)
		log.Printf("Proxy URL: %s", proxyURL)
	}

	if *useProxy && proxyURL != "" {
		port, err := getRandomFreePort(8000, 9000)
		if err != nil {
			log.Fatalf("Failed to find a free port: %v", err)
		}
		go startLocalProxy(port, *localPort)
		*localPort = fmt.Sprintf("127.0.0.1:%d", port)
		go connectAndForward(*localPort, *serverAddr, clientPort, proxyURL, *useTLS)
		select {}
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	<-stop
	log.Println("Shutting down gracefully...")
}
