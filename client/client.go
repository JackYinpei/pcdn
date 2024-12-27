package client

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
)

const (
	maxChunkSize = 16384 // 16KB chunks
)

type FileTransferMessage struct {
	Type     string `json:"type"`
	FileName string `json:"fileName,omitempty"`
	FileSize int64  `json:"fileSize,omitempty"`
	ChunkNum int    `json:"chunkNum,omitempty"`
	TotalChunks int `json:"totalChunks,omitempty"`
	Data     []byte `json:"data,omitempty"`
}

type Client struct {
	ID              string
	SignalingServer string
	wsConn          *websocket.Conn
	peers           map[string]*Peer
	peersMux        sync.RWMutex
	httpServer      *gin.Engine
	httpPort        int
	saveDir         string
}

type Peer struct {
	ID             string
	PeerConnection *webrtc.PeerConnection
	DataChannel    *webrtc.DataChannel
	fileReceiver   *FileReceiver
}

type FileReceiver struct {
	FileName    string
	FileSize    int64
	TotalChunks int
	ReceivedChunks map[int][]byte
	mu         sync.Mutex
}

func NewClient(id, signalingServer string, httpPort int, saveDir string) *Client {
	return &Client{
		ID:              id,
		SignalingServer: signalingServer,
		peers:           make(map[string]*Peer),
		httpPort:        httpPort,
		saveDir:         saveDir,
	}
}

func (c *Client) Start() error {
	// Setup HTTP server
	c.setupHTTPServer()
	go c.httpServer.Run(fmt.Sprintf(":%d", c.httpPort))

	// Connect to signaling server
	wsURL := fmt.Sprintf("%s/ws?id=%s", c.SignalingServer, c.ID)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return fmt.Errorf("failed to connect to signaling server: %v", err)
	}
	c.wsConn = conn

	go c.handleSignaling()
	return nil
}

func (c *Client) setupHTTPServer() {
	r := gin.Default()

	// Load HTML templates
	r.LoadHTMLGlob("templates/*")

	// Serve the main page
	r.GET("/", func(ctx *gin.Context) {
		ctx.HTML(http.StatusOK, "index.html", gin.H{
			"ClientID": c.ID,
		})
	})

	r.POST("/connect", func(ctx *gin.Context) {
		var req struct {
			PeerID string `json:"peer_id"`
		}
		if err := ctx.BindJSON(&req); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		err := c.ConnectToPeer(req.PeerID)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		ctx.JSON(http.StatusOK, gin.H{"message": "Connection initiated"})
	})

	r.POST("/send", func(ctx *gin.Context) {
		peerID := ctx.PostForm("peer_id")
		file, err := ctx.FormFile("file")
		if err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		err = c.SendFile(peerID, file)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		ctx.JSON(http.StatusOK, gin.H{"message": "File sent successfully"})
	})

	c.httpServer = r
}

func (c *Client) ConnectToPeer(peerID string) error {
	// Check if we already have a connection
	c.peersMux.RLock()
	_, exists := c.peers[peerID]
	c.peersMux.RUnlock()
	if exists {
		return fmt.Errorf("already connected to peer %s", peerID)
	}

	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}

	pc, err := webrtc.NewPeerConnection(config)
	if err != nil {
		return err
	}

	peer := &Peer{
		ID:             peerID,
		PeerConnection: pc,
	}

	// Set up ICE candidate handling
	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		
		c.sendSignalingMessage(peerID, "candidate", candidate.ToJSON())
	})

	// Create data channel only if we're the initiator
	dc, err := pc.CreateDataChannel("fileTransfer", nil)
	if err != nil {
		pc.Close()
		return err
	}
	peer.DataChannel = dc

	dc.OnOpen(func() {
		log.Printf("Data channel opened with peer %s", peerID)
	})

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		c.handleFileTransfer(peerID, msg.Data)
	})

	c.peersMux.Lock()
	c.peers[peerID] = peer
	c.peersMux.Unlock()

	// Set up OnDataChannel handler for the answering peer
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		peer.DataChannel = dc
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			c.handleFileTransfer(peer.ID, msg.Data)
		})
	})

	// Create and send offer
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		pc.Close()
		return err
	}

	err = pc.SetLocalDescription(offer)
	if err != nil {
		pc.Close()
		return err
	}

	return c.sendSignalingMessage(peerID, "offer", offer)
}

func (c *Client) SendFile(peerID string, file *multipart.FileHeader) error {
	c.peersMux.RLock()
	peer, exists := c.peers[peerID]
	c.peersMux.RUnlock()

	if !exists {
		return fmt.Errorf("peer %s not connected", peerID)
	}

	f, err := file.Open()
	if err != nil {
		return err
	}
	defer f.Close()

	// Send file metadata
	metadata := FileTransferMessage{
		Type:     "metadata",
		FileName: file.Filename,
		FileSize: file.Size,
		TotalChunks: int((file.Size + maxChunkSize - 1) / maxChunkSize), // Round up division
	}

	metadataBytes, err := json.Marshal(metadata)
	if err != nil {
		return err
	}

	err = peer.DataChannel.Send(metadataBytes)
	if err != nil {
		return err
	}

	// Send file data in chunks
	buffer := make([]byte, maxChunkSize)
	chunkNum := 0
	for {
		n, err := f.Read(buffer)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		chunk := FileTransferMessage{
			Type:     "chunk",
			ChunkNum: chunkNum,
			Data:     buffer[:n],
		}

		chunkBytes, err := json.Marshal(chunk)
		if err != nil {
			return err
		}

		err = peer.DataChannel.Send(chunkBytes)
		if err != nil {
			return err
		}

		chunkNum++
	}

	// Send completion message
	completion := FileTransferMessage{
		Type:     "complete",
		FileName: file.Filename,
	}

	completionBytes, err := json.Marshal(completion)
	if err != nil {
		return err
	}

	return peer.DataChannel.Send(completionBytes)
}

func (c *Client) handleFileTransfer(peerID string, data []byte) {
	var msg FileTransferMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Error parsing file transfer message: %v", err)
		return
	}

	switch msg.Type {
	case "metadata":
		log.Printf("Receiving file from peer %s: %s (%d bytes)", peerID, msg.FileName, msg.FileSize)
		c.peersMux.Lock()
		if peer, exists := c.peers[peerID]; exists {
			peer.fileReceiver = &FileReceiver{
				FileName:    msg.FileName,
				FileSize:    msg.FileSize,
				TotalChunks: msg.TotalChunks,
				ReceivedChunks: make(map[int][]byte),
			}
		}
		c.peersMux.Unlock()

	case "chunk":
		c.peersMux.Lock()
		peer, exists := c.peers[peerID]
		c.peersMux.Unlock()
		
		if !exists || peer.fileReceiver == nil {
			log.Printf("Received chunk for unknown file transfer from peer %s", peerID)
			return
		}

		peer.fileReceiver.mu.Lock()
		peer.fileReceiver.ReceivedChunks[msg.ChunkNum] = msg.Data
		receivedCount := len(peer.fileReceiver.ReceivedChunks)
		peer.fileReceiver.mu.Unlock()

		// If we have all chunks, save the file
		if receivedCount == peer.fileReceiver.TotalChunks {
			// Combine all chunks
			fileData := make([]byte, 0, peer.fileReceiver.FileSize)
			for i := 0; i < peer.fileReceiver.TotalChunks; i++ {
				fileData = append(fileData, peer.fileReceiver.ReceivedChunks[i]...)
			}

			// Save the file
			newFileName := fmt.Sprintf("%s_%s", c.ID, peer.fileReceiver.FileName)
			filePath := filepath.Join(c.saveDir, newFileName)
			err := os.WriteFile(filePath, fileData, 0644)
			if err != nil {
				log.Printf("Error saving file from peer %s: %v", peerID, err)
				return
			}
			log.Printf("File saved from peer %s: %s", peerID, filePath)

			// Clean up
			peer.fileReceiver = nil
		}

	case "complete":
		log.Printf("File transfer completed from peer %s: %s", peerID, msg.FileName)
	}
}

func (c *Client) handleSignaling() {
	for {
		var msg struct {
			Type    string          `json:"type"`
			From    string          `json:"from"`
			To      string          `json:"to"`
			Payload json.RawMessage `json:"payload"`
		}

		err := c.wsConn.ReadJSON(&msg)
		if err != nil {
			log.Printf("Error reading from websocket: %v", err)
			return
		}

		c.peersMux.RLock()
		peer, exists := c.peers[msg.From]
		c.peersMux.RUnlock()

		switch msg.Type {
		case "offer":
			// If we already have a connection, and we have the lower ID, reject the offer
			if exists && c.ID < msg.From {
				log.Printf("Rejecting duplicate offer from %s", msg.From)
				continue
			}

			// If we don't have a peer connection yet, create one
			if !exists {
				pc, err := webrtc.NewPeerConnection(webrtc.Configuration{
					ICEServers: []webrtc.ICEServer{
						{
							URLs: []string{"stun:stun.l.google.com:19302"},
						},
					},
				})
				if err != nil {
					log.Printf("Error creating peer connection: %v", err)
					continue
				}

				peer = &Peer{
					ID:             msg.From,
					PeerConnection: pc,
				}

				// Set up ICE candidate handling
				pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
					if candidate == nil {
						return
					}
					
					c.sendSignalingMessage(msg.From, "candidate", candidate.ToJSON())
				})

				pc.OnDataChannel(func(dc *webrtc.DataChannel) {
					peer.DataChannel = dc
					dc.OnMessage(func(msg webrtc.DataChannelMessage) {
						c.handleFileTransfer(peer.ID, msg.Data)
					})
				})

				c.peersMux.Lock()
				c.peers[msg.From] = peer
				c.peersMux.Unlock()
			}

			var offer webrtc.SessionDescription
			err = json.Unmarshal(msg.Payload, &offer)
			if err != nil {
				log.Printf("Error parsing offer: %v", err)
				continue
			}

			err = peer.PeerConnection.SetRemoteDescription(offer)
			if err != nil {
				log.Printf("Error setting remote description: %v", err)
				continue
			}

			answer, err := peer.PeerConnection.CreateAnswer(nil)
			if err != nil {
				log.Printf("Error creating answer: %v", err)
				continue
			}

			err = peer.PeerConnection.SetLocalDescription(answer)
			if err != nil {
				log.Printf("Error setting local description: %v", err)
				continue
			}

			err = c.sendSignalingMessage(msg.From, "answer", answer)
			if err != nil {
				log.Printf("Error sending answer: %v", err)
			}

		case "answer":
			if !exists {
				log.Printf("Received answer from unknown peer: %s", msg.From)
				continue
			}

			var answer webrtc.SessionDescription
			err = json.Unmarshal(msg.Payload, &answer)
			if err != nil {
				log.Printf("Error parsing answer: %v", err)
				continue
			}

			err = peer.PeerConnection.SetRemoteDescription(answer)
			if err != nil {
				log.Printf("Error setting remote description: %v", err)
			}

		case "candidate":
			if !exists {
				log.Printf("Received ICE candidate from unknown peer: %s", msg.From)
				continue
			}

			var candidate webrtc.ICECandidateInit
			if err = json.Unmarshal(msg.Payload, &candidate); err != nil {
				// Try unmarshaling the raw payload
				if err = json.Unmarshal([]byte(string(msg.Payload)), &candidate); err != nil {
					log.Printf("Error parsing candidate: %v", err)
					continue
				}
			}

			err = peer.PeerConnection.AddICECandidate(candidate)
			if err != nil {
				log.Printf("Error adding ICE candidate: %v", err)
			}
		}
	}
}

func (c *Client) sendSignalingMessage(to, msgType string, payload interface{}) error {
	msg := struct {
		Type    string      `json:"type"`
		From    string      `json:"from"`
		To      string      `json:"to"`
		Payload interface{} `json:"payload"`
	}{
		Type:    msgType,
		From:    c.ID,
		To:      to,
		Payload: payload,
	}

	return c.wsConn.WriteJSON(msg)
}
