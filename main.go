package main

import (
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

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

func logMessage(prefix string, msg *Message) {
	log.Printf("%s - Type: %s, From: %s, To: %s, PayloadLen: %d\n",
		prefix, msg.Type, msg.From, msg.To, len(msg.Payload))
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[ERROR] Websocket upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	clientID := r.URL.Query().Get("id")
	if clientID == "" {
		log.Printf("[ERROR] Client ID is required")
		return
	}

	log.Printf("[INFO] New client connected: %s", clientID)

	clientsMux.Lock()
	clients[clientID] = conn
	clientCount := len(clients)
	clientsMux.Unlock()

	log.Printf("[INFO] Total connected clients: %d", clientCount)

	defer func() {
		clientsMux.Lock()
		delete(clients, clientID)
		clientCount := len(clients)
		clientsMux.Unlock()
		log.Printf("[INFO] Client disconnected: %s, Total clients: %d", clientID, clientCount)
	}()

	// Set read deadline to detect stale connections
	conn.SetReadDeadline(time.Now().Add(24 * time.Hour))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(24 * time.Hour))
		return nil
	})

	// Start ping-pong routine
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			<-ticker.C
			if err := conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(10*time.Second)); err != nil {
				log.Printf("[ERROR] Failed to send ping to client %s: %v", clientID, err)
				return
			}
		}
	}()

	for {
		var msg Message
		err := conn.ReadJSON(&msg)
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("[ERROR] Unexpected close error for client %s: %v", clientID, err)
			}
			break
		}

		logMessage(fmt.Sprintf("[INFO] Received message from %s", clientID), &msg)

		clientsMux.RLock()
		targetConn, exists := clients[msg.To]
		clientsMux.RUnlock()

		if exists {
			err = targetConn.WriteJSON(msg)
			if err != nil {
				log.Printf("[ERROR] Failed to send message to client %s: %v", msg.To, err)
			} else {
				logMessage(fmt.Sprintf("[INFO] Forwarded message to %s", msg.To), &msg)
			}
		} else {
			log.Printf("[WARN] Target client %s not found", msg.To)
		}
	}
}

func main() {
	// Serve static files
	fs := http.FileServer(http.Dir("static"))
	http.Handle("/", fs)

	// WebSocket handler for signaling
	http.HandleFunc("/ws", handleWebSocket)

	// Start server
	log.Println("Server starting on http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
