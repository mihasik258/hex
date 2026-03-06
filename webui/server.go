// Package webui provides an HTTP server with WebSocket support that bridges
// browser clients to the NEC P2P node. Each browser connects via WebSocket
// and can send/receive chat messages, direct messages, view peers, manage
// trust, and monitor calls.
package webui

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strings"
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
	CheckOrigin: func(r *http.Request) bool { return true },
}

// WSMessage is the JSON envelope for all WebSocket communication.
type WSMessage struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Server manages the HTTP/WebSocket server and bridges browser clients
// to the P2P node subsystems.
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

// NewServer creates a new WebUI server.
func NewServer(
	h host.Host,
	chat *messaging.ChatRoom,
	trust *messaging.TrustStore,
	store *messaging.MessageStore,
	callMgr *voicecall.CallManager,
	nick string,
) *Server {
	s := &Server{
		host:    h,
		chat:    chat,
		trust:   trust,
		store:   store,
		callMgr: callMgr,
		selfID:  h.ID(),
		nick:    nick,
		clients: make(map[*websocket.Conn]bool),
	}

	// Register DM handler so incoming direct messages get forwarded to browsers.
	messaging.RegisterDMHandler(h, func(msg *messaging.Message) {
		senderID, _ := peer.Decode(msg.Sender)
		s.trust.RecordPeer(senderID, msg.SenderNick)

		// Only deliver DMs from verified peers.
		if !s.trust.IsVerified(senderID) {
			log.Printf("[webui] Blocked DM from unverified peer %s", msg.Sender[:8])
			return
		}

		displayNick := msg.SenderNick
		if displayNick == "" {
			displayNick = msg.Sender[:8]
		}

		s.broadcast(WSMessage{
			Type: "dm",
			Payload: jsonRaw(map[string]string{
				"id":        msg.ID,
				"sender":    msg.Sender,
				"nick":      displayNick,
				"text":      msg.Payload,
				"timestamp": time.UnixMilli(msg.Timestamp).Format("15:04:05"),
				"trust":     "verified",
			}),
		})
	})

	callMgr.OnCallIncoming = func(remotePeer peer.ID) {
		s.broadcast(WSMessage{
			Type: "call_incoming",
			Payload: jsonRaw(map[string]string{
				"peer_id": remotePeer.String(),
			}),
		})
	}

	return s
}

// Start launches the HTTP server. The address should be like ":8080".
// It binds to 0.0.0.0 explicitly so phones on the same LAN can connect.
func (s *Server) Start(ctx context.Context, addr string) error {
	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return fmt.Errorf("static fs: %w", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(staticFS)))
	mux.HandleFunc("/ws", s.handleWS)
	mux.HandleFunc("/api/info", s.handleInfo)

	// Ensure we bind to all interfaces (0.0.0.0) for LAN access from phones.
	listenAddr := addr
	if strings.HasPrefix(addr, ":") {
		listenAddr = "0.0.0.0" + addr
	}

	srv := &http.Server{Addr: listenAddr, Handler: mux}

	// Forward incoming GossipSub messages to browsers.
	go s.forwardMessages(ctx)

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	// Print actual URLs.
	log.Printf("[webui] HTTP server starting on %s", listenAddr)
	if ips := getLocalIPs(); len(ips) > 0 {
		port := addr
		if i := strings.LastIndex(addr, ":"); i >= 0 {
			port = addr[i:]
		}
		for _, ip := range ips {
			log.Printf("[webui]   → http://%s%s", ip, port)
		}
	}

	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}

// broadcast sends to all connected WebSocket clients.
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

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(map[string]string{
		"peer_id":       s.selfID.String(),
		"safety_number": network.SafetyNumber(s.selfID),
		"nick":          s.nick,
	})
}

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

func (s *Server) handleClientMessage(conn *websocket.Conn, msg WSMessage) {
	switch msg.Type {
	case "ping":
		// Just to keep connection alive
		return
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

	case "send_dm":
		var payload struct {
			PeerID string `json:"peer_id"`
			Text   string `json:"text"`
		}
		if err := json.Unmarshal(msg.Payload, &payload); err != nil || payload.Text == "" {
			return
		}
		pid, err := peer.Decode(payload.PeerID)
		if err != nil {
			return
		}
		go func() {
			if err := messaging.SendDM(context.Background(), s.host, s.selfID, s.nick, pid, payload.Text); err != nil {
				log.Printf("[webui] DM error: %v", err)
				s.sendToConn(conn, WSMessage{
					Type:    "dm_error",
					Payload: jsonRaw(map[string]string{"error": err.Error()}),
				})
			}
		}()

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
		s.broadcast(WSMessage{
			Type:    "call_ended",
			Payload: jsonRaw(map[string]string{"reason": "local_hangup"}),
		})

	case "accept_call":
		s.callMgr.AcceptCall()

	case "get_callstats":
		m := s.callMgr.ActiveCallMetrics()
		if m == nil {
			s.sendToConn(conn, WSMessage{
				Type:    "call_ended",
				Payload: jsonRaw(map[string]string{"reason": "no_call"}),
			})
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
	s.sendToConn(conn, WSMessage{Type: "peers", Payload: jsonRaw(list)})
}

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
	s.sendToConn(conn, WSMessage{Type: "trust", Payload: jsonRaw(list)})
}

// forwardMessages reads from GossipSub and broadcasts to all browsers.
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
			s.trust.RecordPeer(senderID, msg.SenderNick)

			// Only deliver messages from verified peers.
			if !s.trust.IsVerified(senderID) {
				continue
			}

			trustTag := "verified"

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
	data, _ := json.Marshal(msg)
	s.mu.Lock()
	defer s.mu.Unlock()
	conn.WriteMessage(websocket.TextMessage, data)
}

func jsonRaw(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}

// getLocalIPs returns all non-loopback IPv4 addresses for LAN access hints.
func getLocalIPs() []string {
	var ips []string
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			ips = append(ips, ipnet.IP.String())
		}
	}
	return ips
}
