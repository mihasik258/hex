package messaging

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

const CourierProtocolID = "/nec/courier/1.0.0"

// CourierMessage represents an encapsulated message meant for an offline peer.
type CourierMessage struct {
	Target peer.ID  `json:"target"`
	Msg    *Message `json:"msg"`
}

type CourierStats struct {
	OutboxSize  int `json:"outbox_size"`
	MuleBagSize int `json:"mulebag_size"`
}

// CourierManager manages the Outbox and MuleBag for explicit delivery.
type CourierManager struct {
	host    host.Host
	mu      sync.Mutex
	outbox  []CourierMessage // Messages I want to send, but peer is offline
	muleBag []CourierMessage // Messages I am carrying for others
}

func NewCourierManager(h host.Host) *CourierManager {
	cm := &CourierManager{
		host: h,
	}

	h.SetStreamHandler(CourierProtocolID, func(s network.Stream) {
		cm.handleCourierPull(s)
	})

	// Add a periodic loop to try and deliver MuleBag messages to connected peers.
	go cm.deliveryLoop()

	log.Printf("[courier] Manager ready (protocol=%s)", CourierProtocolID)
	return cm
}

// QueueInOutbox stores a message when it can't be sent directly.
func (cm *CourierManager) QueueInOutbox(target peer.ID, msg *Message) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.outbox = append(cm.outbox, CourierMessage{Target: target, Msg: msg})
	log.Printf("[courier] Queued 1 message for %s in Outbox", target.String()[:16])
}

// CollectFromPeers opens streams to all connected peers and asks them for their Outbox.
func (cm *CourierManager) CollectFromPeers(ctx context.Context) (int, error) {
	peers := cm.host.Network().Peers()
	collected := 0

	for _, p := range peers {
		s, err := cm.host.NewStream(ctx, p, CourierProtocolID)
		if err != nil {
			continue
		}

		// Read the array of CourierMessage (the peer's outbox)
		var outbox []CourierMessage
		if err := json.NewDecoder(s).Decode(&outbox); err == nil {
			cm.mu.Lock()
			cm.muleBag = append(cm.muleBag, outbox...)
			collected += len(outbox)
			cm.mu.Unlock()
		}
		s.Close()
	}

	log.Printf("[courier] Collected %d messages for MuleBag from %d peers", collected, len(peers))
	return collected, nil
}

// handleCourierPull serves the outbox to a collecting courier.
func (cm *CourierManager) handleCourierPull(s network.Stream) {
	defer s.Close()

	cm.mu.Lock()
	outboxCopy := make([]CourierMessage, len(cm.outbox))
	copy(outboxCopy, cm.outbox)
	// Empty outbox since courier took responsibility
	cm.outbox = nil
	cm.mu.Unlock()

	// Send outbox as JSON
	json.NewEncoder(s).Encode(outboxCopy)
	log.Printf("[courier] Couriered %d outbox messages to %s", len(outboxCopy), s.Conn().RemotePeer().String()[:16])
}

// deliveryLoop periodically checks connected peers and delivers matched MuleBag messages.
func (cm *CourierManager) deliveryLoop() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		<-ticker.C
		cm.mu.Lock()
		if len(cm.muleBag) == 0 {
			cm.mu.Unlock()
			continue
		}

		// Find connected peers
		connected := make(map[peer.ID]bool)
		for _, p := range cm.host.Network().Peers() {
			connected[p] = true
		}

		// Check if we can deliver anything
		var remaining []CourierMessage
		for _, cmgs := range cm.muleBag {
			if connected[cmgs.Target] {
				// Deliver! Let's just use the regular SendDM logic.
				err := cm.sendStoredDM(context.Background(), cmgs.Target, cmgs.Msg)
				if err != nil {
					// Keep in bag if failed
					remaining = append(remaining, cmgs)
				} else {
					log.Printf("[courier] Successfully delivered MuleBag message to %s", cmgs.Target.String()[:16])
				}
			} else {
				remaining = append(remaining, cmgs)
			}
		}
		cm.muleBag = remaining
		cm.mu.Unlock()
	}
}

func (cm *CourierManager) sendStoredDM(ctx context.Context, target peer.ID, msg *Message) error {
	s, err := cm.host.NewStream(ctx, target, DMProtocolID)
	if err != nil {
		return err
	}
	defer s.Close()
	return writeDM(s, msg)
}

func (cm *CourierManager) Stats() CourierStats {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return CourierStats{
		OutboxSize:  len(cm.outbox),
		MuleBagSize: len(cm.muleBag),
	}
}
