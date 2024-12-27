package main

import (
	"flag"
	"log"
	"os"
	"path/filepath"

	"pcdn/client"
	"pcdn/signaling"
)

func main() {
	// Parse command line flags
	isServer := flag.Bool("server", false, "Run as signaling server")
	clientID := flag.String("id", "", "Client ID")
	signalingServer := flag.String("signaling", "ws://localhost:8080", "Signaling server address")
	httpPort := flag.Int("port", 3000, "HTTP server port")
	saveDir := flag.String("save", "", "Directory to save received files")
	flag.Parse()

	if *isServer {
		// Run as signaling server
		log.Fatal(signaling.StartSignalingServer(":8080"))
		return
	}

	if *clientID == "" {
		log.Fatal("Client ID is required")
	}

	// Create save directory if it doesn't exist
	if *saveDir == "" {
		*saveDir = filepath.Join(".", "received_files", *clientID)
	}
	err := os.MkdirAll(*saveDir, 0755)
	if err != nil {
		log.Fatal(err)
	}

	// Create and start client
	c := client.NewClient(*clientID, *signalingServer, *httpPort, *saveDir)
	err = c.Start()
	if err != nil {
		log.Fatal(err)
	}

	// Keep the program running
	select {}
}
