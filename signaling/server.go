package signaling

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

type Client struct {
	ID   string
	Conn *websocket.Conn
}

type Message struct {
	Type    string          `json:"type"`
	From    string          `json:"from"`
	To      string          `json:"to"`
	Payload json.RawMessage `json:"payload"`
}

type Server struct {
	clients    map[string]*Client
	clientsMux sync.RWMutex
	upgrader   websocket.Upgrader
}

func NewServer() *Server {
	return &Server{
		clients: make(map[string]*Client),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
	}
}

func (s *Server) HandleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Failed to upgrade connection: %v", err)
		return
	}

	clientID := r.URL.Query().Get("id")
	if clientID == "" {
		log.Println("Client ID is required")
		conn.Close()
		return
	}

	client := &Client{
		ID:   clientID,
		Conn: conn,
	}

	s.clientsMux.Lock()
	s.clients[clientID] = client
	s.clientsMux.Unlock()

	log.Printf("New client connected: %s", clientID)

	go s.handleMessages(client)
}

func (s *Server) handleMessages(client *Client) {
	defer func() {
		s.clientsMux.Lock()
		delete(s.clients, client.ID)
		s.clientsMux.Unlock()
		client.Conn.Close()
	}()

	for {
		var msg Message
		err := client.Conn.ReadJSON(&msg)
		if err != nil {
			log.Printf("Error reading message from client %s: %v", client.ID, err)
			return
		}

		msg.From = client.ID
		s.routeMessage(&msg)
	}
}

func (s *Server) routeMessage(msg *Message) {
	s.clientsMux.RLock()
	targetClient, exists := s.clients[msg.To]
	s.clientsMux.RUnlock()

	if !exists {
		log.Printf("Target client %s not found", msg.To)
		return
	}

	err := targetClient.Conn.WriteJSON(msg)
	if err != nil {
		log.Printf("Error sending message to client %s: %v", msg.To, err)
	}
}

func StartSignalingServer(addr string) error {
	server := NewServer()
	http.HandleFunc("/ws", server.HandleWS)
	log.Printf("Signaling server starting on %s", addr)
	return http.ListenAndServe(addr, nil)
}
