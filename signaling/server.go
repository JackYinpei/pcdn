package main

import (
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

var (
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}
	clients    = make(map[string]*websocket.Conn)
	clientsMux sync.RWMutex
)

type Message struct {
	Type    string `json:"type"`
	From    string `json:"from"`
	To      string `json:"to"`
	Payload string `json:"payload"`
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Websocket upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	// Generate a unique ID for this client
	clientID := r.URL.Query().Get("id")
	if clientID == "" {
		log.Println("Client ID is required")
		return
	}

	// Register client
	clientsMux.Lock()
	clients[clientID] = conn
	clientsMux.Unlock()

	defer func() {
		clientsMux.Lock()
		delete(clients, clientID)
		clientsMux.Unlock()
	}()

	for {
		var msg Message
		err := conn.ReadJSON(&msg)
		if err != nil {
			log.Printf("Error reading message: %v", err)
			break
		}

		// Forward the message to the target client
		clientsMux.RLock()
		targetConn, exists := clients[msg.To]
		clientsMux.RUnlock()

		if exists {
			err = targetConn.WriteJSON(msg)
			if err != nil {
				log.Printf("Error sending message: %v", err)
			}
		}
	}
}

func main() {
	http.HandleFunc("/ws", handleWebSocket)
	log.Println("Starting signaling server on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
