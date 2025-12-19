package main

import (
	"fmt"
	"os"

	"xrok/cmd"
	"xrok/cmd/config"
	"xrok/cmd/logger"
)

const version = "2.0.0"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "server":
		cmd.RunServer(os.Args[2:])
	case "client":
		cmd.RunClient(os.Args[2:])
	case "status":
		cmd.RunStatus(os.Args[2:])
	case "init":
		runInit(os.Args[2:])
	case "version", "-v", "--version":
		printVersion()
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Printf("Unknown command: %s\n\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printVersion() {
	log := logger.New()
	log.Status("", logger.Cyan, "xrok version %s%s%s", logger.Bold, version, logger.Reset)
}

func runInit(args []string) {
	log := logger.New()

	mode := "client"
	if len(args) > 0 {
		mode = args[0]
	}

	var content string
	var filename string

	switch mode {
	case "client":
		content = config.GenerateExampleClientConfig()
		filename = "xrok-client.yaml"
	case "server":
		content = config.GenerateExampleServerConfig()
		filename = "xrok-server.yaml"
	default:
		log.Error("Unknown config type: %s (use 'client' or 'server')", mode)
		os.Exit(1)
	}

	// Check if file exists
	if _, err := os.Stat(filename); err == nil {
		log.Warn("File %s already exists, not overwriting", filename)
		return
	}

	if err := os.WriteFile(filename, []byte(content), 0644); err != nil {
		log.Error("Failed to write config file: %v", err)
		os.Exit(1)
	}

	log.Success("Created example config: %s", filename)
	log.Info("Edit the file and run: xrok %s -config %s", mode, filename)
}

func printUsage() {
	fmt.Println(`xrok - Secure tunneling solution v` + version + `

Usage:
  xrok <command> [options]

Commands:
  server    Start the xrok server
  client    Start the xrok client
  status    Show status of running tunnels
  init      Generate example config file
  version   Show version information
  help      Show this help message

Examples:
  # Start server with config file
  xrok server -config server.yaml

  # Start client with config file
  xrok client -config client.yaml

  # Start client with CLI flags
  xrok client --server example.com --tunnel app:localhost:8080

  # Generate example config
  xrok init client
  xrok init server

  # Show tunnel status
  xrok status --server example.com

Run 'xrok <command> --help' for more information on a command.`)
}
