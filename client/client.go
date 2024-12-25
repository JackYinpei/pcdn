package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
)

type Client struct {
	ID              string
	SignalingServer string
	WsConn          *websocket.Conn
	PeerConnection  *webrtc.PeerConnection
	DataChannel     *webrtc.DataChannel
}

type Message struct {
	Type    string `json:"type"`
	From    string `json:"from"`
	To      string `json:"to"`
	Payload string `json:"payload"`
}

func NewClient(id, signalingServer string) (*Client, error) {
	// Create WebRTC configuration
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}

	// Create a new PeerConnection
	peerConnection, err := webrtc.NewPeerConnection(config)
	if err != nil {
		return nil, err
	}

	return &Client{
		ID:              id,
		SignalingServer: signalingServer,
		PeerConnection:  peerConnection,
	}, nil
}

func (c *Client) Connect() error {
	// Connect to signaling server
	wsURL := fmt.Sprintf("%s/ws?id=%s", c.SignalingServer, c.ID)
	var err error
	c.WsConn, _, err = websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return err
	}

	// Handle incoming messages from signaling server
	go c.handleSignaling()

	return nil
}

func (c *Client) handleSignaling() {
	for {
		var msg Message
		err := c.WsConn.ReadJSON(&msg)
		if err != nil {
			log.Printf("Error reading message: %v", err)
			return
		}

		switch msg.Type {
		case "offer":
			err = c.handleOffer(msg.Payload)
		case "answer":
			err = c.handleAnswer(msg.Payload)
		case "candidate":
			err = c.handleCandidate(msg.Payload)
		}

		if err != nil {
			log.Printf("Error handling message: %v", err)
		}
	}
}

func (c *Client) InitiateConnection(targetID string) error {
	// Create data channel
	dataChannel, err := c.PeerConnection.CreateDataChannel("fileTransfer", nil)
	if err != nil {
		return err
	}
	c.DataChannel = dataChannel

	// Set up data channel handlers
	c.setupDataChannelHandlers()

	// Create offer
	offer, err := c.PeerConnection.CreateOffer(nil)
	if err != nil {
		return err
	}

	err = c.PeerConnection.SetLocalDescription(offer)
	if err != nil {
		return err
	}

	// Send offer through signaling server
	return c.sendSignalingMessage("offer", targetID, offer.SDP)
}

func (c *Client) SendFile(filePath string) error {
	if c.DataChannel == nil {
		return fmt.Errorf("no data channel established")
	}

	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	// Get file info
	fileInfo, err := file.Stat()
	if err != nil {
		return err
	}

	// Send file metadata
	metadata := struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	}{
		Name: fileInfo.Name(),
		Size: fileInfo.Size(),
	}

	metadataBytes, err := json.Marshal(metadata)
	if err != nil {
		return err
	}

	err = c.DataChannel.Send(metadataBytes)
	if err != nil {
		return err
	}

	// Send file in chunks
	buffer := make([]byte, 16384) // 16KB chunks
	for {
		n, err := file.Read(buffer)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		err = c.DataChannel.Send(buffer[:n])
		if err != nil {
			return err
		}

		// Simple rate limiting
		time.Sleep(time.Millisecond)
	}

	return nil
}

func (c *Client) setupDataChannelHandlers() {
	c.DataChannel.OnOpen(func() {
		log.Println("Data channel opened")
	})

	c.DataChannel.OnMessage(func(msg webrtc.DataChannelMessage) {
		// Handle incoming file data
		log.Printf("Received %d bytes", len(msg.Data))
	})
}

func (c *Client) handleOffer(sdp string) error {
	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sdp,
	}

	err := c.PeerConnection.SetRemoteDescription(offer)
	if err != nil {
		return err
	}

	answer, err := c.PeerConnection.CreateAnswer(nil)
	if err != nil {
		return err
	}

	err = c.PeerConnection.SetLocalDescription(answer)
	if err != nil {
		return err
	}

	return c.sendSignalingMessage("answer", "", answer.SDP)
}

func (c *Client) handleAnswer(sdp string) error {
	answer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  sdp,
	}

	return c.PeerConnection.SetRemoteDescription(answer)
}

func (c *Client) handleCandidate(candidate string) error {
	var iceCandidate webrtc.ICECandidateInit
	err := json.Unmarshal([]byte(candidate), &iceCandidate)
	if err != nil {
		return err
	}

	return c.PeerConnection.AddICECandidate(iceCandidate)
}

func (c *Client) sendSignalingMessage(msgType, to, payload string) error {
	msg := Message{
		Type:    msgType,
		From:    c.ID,
		To:      to,
		Payload: payload,
	}

	return c.WsConn.WriteJSON(msg)
}
