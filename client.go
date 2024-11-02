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
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var (
	rp    int
	debug bool
)

type RegisterResponse struct {
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
		if fileInfo.Size() > 100*1024*1024*1024 { // Limit: 100 GB
			http.Error(w, "File is too large", http.StatusForbidden)
			return
		}

		if !fileInfo.IsDir() {
			w.Header().Set("Content-Disposition", "attachment; filename="+url.QueryEscape(fileInfo.Name()))
			w.Header().Set("Cache-Control", "public, max-age=86400")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("X-Frame-Options", "DENY")
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
		readTimeout       = 60 * time.Second
		writeTimeout      = 10 * time.Second
		connReadLimit     = 1024 * 1024
		bufferSize        = 4096
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

		localConn, err := net.Dial("tcp", localPort)
		if err != nil {
			log.Printf("Failed to connect to local application on port %s: %v", localPort, err)
			conn.Close()
			return
		}

		done := make(chan struct{})
		closeOnce := sync.Once{}

		closeDone := func() {
			closeOnce.Do(func() {
				close(done)
				conn.Close()
				localConn.Close()
			})
		}

		go func() {
			defer closeDone()
			for {
				conn.SetReadDeadline(time.Now().Add(readTimeout))
				_, msg, err := conn.ReadMessage()
				if err != nil {
					if debug {
						log.Printf("Error reading from WebSocket: %v", err)
					}
					return
				}
				_, err = localConn.Write(msg)
				if err != nil {
					log.Printf("Error writing to local connection: %v", err)
					return
				}
			}
		}()

		go func() {
			defer closeDone()
			buffer := make([]byte, bufferSize)
			for {
				localConn.SetReadDeadline(time.Now().Add(readTimeout))
				n, err := localConn.Read(buffer)
				if err != nil {
					if debug {
						log.Printf("Error reading from local application: %v", err)
					}
					return
				}
				conn.SetWriteDeadline(time.Now().Add(writeTimeout))
				err = conn.WriteMessage(websocket.BinaryMessage, buffer[:n])
				if err != nil {
					if debug {
						log.Printf("Error writing to WebSocket: %v", err)
					}
					return
				}
			}
		}()

		<-done
		if debug {
			log.Printf("Disconnected. Reconnecting...")
		}
	}
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

func startLocalProxy(port int) {
	localAddr := fmt.Sprintf("127.0.0.1:%d", port)

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

		r.RequestURI = ""
		r.URL.Host = r.Host
		r.URL.Scheme = "http"
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

		limitedReader := io.LimitedReader{R: resp.Body, N: 10 * 1024 * 1024}
		if _, err := io.Copy(w, &limitedReader); err != nil {
			if debug {
				log.Printf("Error copying response body: %v", err)
			}
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
		connectAndForward(*localPort, *serverAddr, clientPort, proxyURL, *useTLS)
		log.Printf("Proxy URL: %s", proxyURL)
	}

	if *useProxy && proxyURL != "" {
		port, err := getRandomFreePort(8000, 9000)
		if err != nil {
			log.Fatalf("Failed to find a free port: %v", err)
		}
		*localPort = fmt.Sprintf("127.0.0.1:%d", port)

		go startLocalProxy(port)
		connectAndForward(*localPort, *serverAddr, clientPort, proxyURL, *useTLS)
		select {}
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	<-stop
	log.Println("Shutting down gracefully...")
}
