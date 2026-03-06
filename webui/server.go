// Package webui provides an HTTP server with WebSocket support that bridges
// browser clients to the NEC P2P node. Each browser connects via WebSocket
// and can send/receive chat messages, view peers, manage trust, and more.
package webui

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"

	"nec/messaging"
	"nec/network"
	"nec/voicecall"
)

//go:embed static/*
var staticFiles embed.FS

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true }, // Allow all origins for LAN use.
}

// WSMessage is the JSON envelope for all WebSocket communication.
type WSMessage struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Server manages the HTTP/WebSocket server and bridges browser clients
// to the P2P node's messaging, file transfer, and call subsystems.
type Server struct {
	host    host.Host
	chat    *messaging.ChatRoom
	trust   *messaging.TrustStore
	store   *messaging.MessageStore
	callMgr *voicecall.CallManager
	selfID  peer.ID
	nick    string

	mu      sync.Mutex
	clients map[*websocket.Conn]bool
}

// NewServer creates a new WebUI server connected to the P2P subsystems.
func NewServer(
	h host.Host,
	chat *messaging.ChatRoom,
	trust *messaging.TrustStore,
	store *messaging.MessageStore,
	callMgr *voicecall.CallManager,
	nick string,
) *Server {
	return &Server{
		host:    h,
		chat:    chat,
		trust:   trust,
		store:   store,
		callMgr: callMgr,
		selfID:  h.ID(),
		nick:    nick,
		clients: make(map[*websocket.Conn]bool),
	}
}

// Start launches the HTTP server on the given address (e.g. ":8080")
// and begins forwarding incoming GossipSub messages to all connected browsers.
func (s *Server) Start(ctx context.Context, addr string) error {
	// Serve embedded static files.
	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return fmt.Errorf("static fs: %w", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(staticFS)))
	mux.HandleFunc("/ws", s.handleWS)
	mux.HandleFunc("/api/info", s.handleInfo)

	srv := &http.Server{Addr: addr, Handler: mux}

	// Forward incoming P2P messages to all browser clients.
	go s.forwardMessages(ctx)

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	log.Printf("[webui] HTTP server starting on %s", addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}

// broadcast sends a WSMessage to all connected WebSocket clients.
func (s *Server) broadcast(msg WSMessage) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for conn := range s.clients {
		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			conn.Close()
			delete(s.clients, conn)
		}
	}
}

// handleInfo returns node identity info as JSON (for initial page load).
func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(map[string]string{
		"peer_id":       s.selfID.String(),
		"safety_number": network.SafetyNumber(s.selfID),
		"nick":          s.nick,
	})
}

// handleWS upgrades HTTP to WebSocket and handles client messages.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[webui] Upgrade error: %v", err)
		return
	}

	s.mu.Lock()
	s.clients[conn] = true
	s.mu.Unlock()

	log.Printf("[webui] Client connected (%d total)", len(s.clients))

	// Send current peers list on connect.
	s.sendPeersList(conn)

	defer func() {
		s.mu.Lock()
		delete(s.clients, conn)
		s.mu.Unlock()
		conn.Close()
		log.Printf("[webui] Client disconnected (%d total)", len(s.clients))
	}()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}

		var msg WSMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}

		s.handleClientMessage(conn, msg)
	}
}

// handleClientMessage processes a message from a browser client.
func (s *Server) handleClientMessage(conn *websocket.Conn, msg WSMessage) {
	switch msg.Type {
	case "send_chat":
		var payload struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(msg.Payload, &payload); err != nil || payload.Text == "" {
			return
		}
		if err := s.chat.Publish(payload.Text); err != nil {
			log.Printf("[webui] Publish error: %v", err)
		}

	case "get_peers":
		s.sendPeersList(conn)

	case "get_trust":
		s.sendTrustList(conn)

	case "verify_peer":
		var payload struct {
			PeerID string `json:"peer_id"`
		}
		if err := json.Unmarshal(msg.Payload, &payload); err != nil {
			return
		}
		pid, err := peer.Decode(payload.PeerID)
		if err != nil {
			return
		}
		s.trust.Verify(pid)
		s.sendTrustList(conn)

	case "call_peer":
		var payload struct {
			PeerID string `json:"peer_id"`
		}
		if err := json.Unmarshal(msg.Payload, &payload); err != nil {
			return
		}
		pid, err := peer.Decode(payload.PeerID)
		if err != nil {
			return
		}
		go func() {
			if err := s.callMgr.Call(context.Background(), pid); err != nil {
				s.sendToConn(conn, WSMessage{
					Type:    "call_error",
					Payload: jsonRaw(map[string]string{"error": err.Error()}),
				})
			}
		}()

	case "hangup":
		s.callMgr.Hangup()

	case "get_callstats":
		m := s.callMgr.ActiveCallMetrics()
		if m == nil {
			return
		}
		snap := m.Snapshot()
		s.sendToConn(conn, WSMessage{
			Type: "callstats",
			Payload: jsonRaw(map[string]any{
				"duration":   snap.Duration.Seconds(),
				"sent":       snap.PacketsSent,
				"recv":       snap.PacketsRecv,
				"lost":       snap.PacketsLost,
				"loss_pct":   snap.LossPercent,
				"jitter_ms":  snap.JitterMs,
				"latency_ms": snap.AvgLatencyMs,
			}),
		})
	}
}

// sendPeersList sends the current peer list to a single client.
func (s *Server) sendPeersList(conn *websocket.Conn) {
	peers := s.chat.ListPeers()
	list := make([]map[string]string, 0, len(peers))
	for _, p := range peers {
		status, sn, known := s.trust.GetStatus(p)
		entry := map[string]string{
			"peer_id":       p.String(),
			"safety_number": network.SafetyNumber(p),
		}
		if known {
			entry["status"] = string(status)
			entry["safety_number"] = sn
		} else {
			entry["status"] = "unknown"
		}
		list = append(list, entry)
	}
	s.sendToConn(conn, WSMessage{
		Type:    "peers",
		Payload: jsonRaw(list),
	})
}

// sendTrustList sends the trust store to a single client.
func (s *Server) sendTrustList(conn *websocket.Conn) {
	records := s.trust.ListAll()
	list := make([]map[string]string, 0, len(records))
	for _, r := range records {
		list = append(list, map[string]string{
			"peer_id":       r.PeerID.String(),
			"safety_number": r.SafetyNumber,
			"status":        string(r.Status),
			"nick":          r.Nick,
			"last_seen":     r.LastSeen.Format("15:04:05"),
		})
	}
	s.sendToConn(conn, WSMessage{
		Type:    "trust",
		Payload: jsonRaw(list),
	})
}

// forwardMessages reads from the GossipSub ChatRoom and broadcasts to all WS clients.
func (s *Server) forwardMessages(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-s.chat.Messages:
			if !ok {
				return
			}

			senderID, _ := peer.Decode(msg.Sender)
			if s.trust.IsBlocked(senderID) {
				continue
			}
			s.trust.RecordPeer(senderID, msg.SenderNick)

			trustTag := "unverified"
			if s.trust.IsVerified(senderID) {
				trustTag = "verified"
			}

			switch msg.Type {
			case messaging.TypeChat:
				displayNick := msg.SenderNick
				if displayNick == "" {
					displayNick = msg.Sender[:8]
				}
				s.broadcast(WSMessage{
					Type: "chat",
					Payload: jsonRaw(map[string]string{
						"id":        msg.ID,
						"sender":    msg.Sender,
						"nick":      displayNick,
						"text":      msg.Payload,
						"timestamp": time.UnixMilli(msg.Timestamp).Format("15:04:05"),
						"trust":     trustTag,
					}),
				})
				// Send ACK.
				s.chat.PublishAck(msg.ID)

			case messaging.TypeAck:
				s.broadcast(WSMessage{
					Type: "ack",
					Payload: jsonRaw(map[string]string{
						"original_id": msg.Payload,
						"from":        msg.Sender[:8],
					}),
				})
			}
		}
	}
}

func (s *Server) sendToConn(conn *websocket.Conn, msg WSMessage) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	conn.WriteMessage(websocket.TextMessage, data)
}

func jsonRaw(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}
