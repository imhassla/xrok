package cmd

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"xrok/cmd/logger"
)

// ServerStatus represents the server status response
type ServerStatus struct {
	Status       string        `json:"status"`
	Uptime       string        `json:"uptime"`
	Version      string        `json:"version"`
	Clients      int           `json:"clients"`
	Connections  int           `json:"connections"`
	BytesSent    int64         `json:"bytes_sent"`
	BytesRecv    int64         `json:"bytes_received"`
	Tunnels      []TunnelStatus `json:"tunnels,omitempty"`
}

// TunnelStatus represents a tunnel's status
type TunnelStatus struct {
	Name       string `json:"name"`
	Target     string `json:"target"`
	URL        string `json:"url"`
	ClientID   string `json:"client_id"`
	Connected  string `json:"connected"`
	Requests   int64  `json:"requests"`
}

// RunStatus runs the status command
func RunStatus(args []string) {
	log := logger.New()

	fs := flag.NewFlagSet("status", flag.ExitOnError)
	serverAddr := fs.String("server", "", "Server address to check status")
	metricsPort := fs.Int("metrics-port", 9090, "Metrics port")
	jsonOutput := fs.Bool("json", false, "Output in JSON format")
	fs.Parse(args)

	if *serverAddr == "" {
		log.Error("--server flag is required")
		fmt.Println("\nUsage: xrok status --server <address>")
		os.Exit(1)
	}

	// Try to fetch status from server
	statusURL := fmt.Sprintf("http://%s:%d/status", *serverAddr, *metricsPort)
	healthURL := fmt.Sprintf("http://%s:%d/health", *serverAddr, *metricsPort)

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	// Check health first
	healthResp, err := client.Get(healthURL)
	if err != nil {
		log.Error("Cannot connect to server: %v", err)
		os.Exit(1)
	}
	defer healthResp.Body.Close()

	if healthResp.StatusCode != http.StatusOK {
		log.Error("Server health check failed: %s", healthResp.Status)
		os.Exit(1)
	}

	// Try to get detailed status
	statusResp, err := client.Get(statusURL)
	if err != nil || statusResp.StatusCode != http.StatusOK {
		// Fallback to basic health info
		if *jsonOutput {
			fmt.Println(`{"status":"healthy"}`)
		} else {
			log.Connected(fmt.Sprintf("Server %s is healthy", *serverAddr))
		}
		return
	}
	defer statusResp.Body.Close()

	var status ServerStatus
	if err := json.NewDecoder(statusResp.Body).Decode(&status); err != nil {
		log.Error("Failed to parse status response: %v", err)
		os.Exit(1)
	}

	if *jsonOutput {
		data, _ := json.MarshalIndent(status, "", "  ")
		fmt.Println(string(data))
		return
	}

	// Pretty print status
	fmt.Println()
	log.Status("●", logger.Green, "Server Status: %s%s%s", logger.Bold, status.Status, logger.Reset)
	fmt.Println()

	fmt.Printf("  %-15s %s\n", "Uptime:", status.Uptime)
	fmt.Printf("  %-15s %s\n", "Version:", status.Version)
	fmt.Printf("  %-15s %d\n", "Clients:", status.Clients)
	fmt.Printf("  %-15s %d\n", "Connections:", status.Connections)
	fmt.Printf("  %-15s %s\n", "Bytes Sent:", formatBytes(status.BytesSent))
	fmt.Printf("  %-15s %s\n", "Bytes Received:", formatBytes(status.BytesRecv))

	if len(status.Tunnels) > 0 {
		fmt.Println()
		log.Info("Active Tunnels:")
		for _, t := range status.Tunnels {
			fmt.Printf("  %s→%s %s%s%s → %s\n",
				logger.Cyan, logger.Reset,
				logger.Bold, t.Name, logger.Reset,
				t.Target)
			fmt.Printf("    URL: %s\n", t.URL)
			fmt.Printf("    Requests: %d, Connected: %s\n", t.Requests, t.Connected)
		}
	}
	fmt.Println()
}

func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
