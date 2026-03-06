package messaging

import (
	"context"
	"fmt"
	"log"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
)

// DefaultChatTopic is the GossipSub topic for the global NEC chat room.
const DefaultChatTopic = "nec/chat/1.0.0"

// ChatRoom manages GossipSub subscription for a single topic and provides
// high-level Publish / Subscribe semantics with deduplication built into
// GossipSub v1.1.
type ChatRoom struct {
	ps       *pubsub.PubSub
	topic    *pubsub.Topic
	sub      *pubsub.Subscription
	self     peer.ID
	nick     string
	Messages chan *Message // Incoming messages delivered to the consumer.
	ctx      context.Context
	cancel   context.CancelFunc
}

// JoinChat initializes GossipSub v1.1 on the given libp2p host and joins
// the default chat topic. It starts a background goroutine that reads incoming
// messages and pipes them into the Messages channel.
func JoinChat(ctx context.Context, h host.Host, nick string) (*ChatRoom, error) {
	// Create a new GossipSub router attached to the host.
	// GossipSub v1.1 provides:
	//   - Flood publishing for immediate message delivery.
	//   - Mesh management with peer scoring to limit broadcast storms.
	//   - Built-in message deduplication via message IDs.
	ps, err := pubsub.NewGossipSub(ctx, h)
	if err != nil {
		return nil, fmt.Errorf("create gossipsub: %w", err)
	}

	// Join the global chat topic.
	topic, err := ps.Join(DefaultChatTopic)
	if err != nil {
		return nil, fmt.Errorf("join topic %q: %w", DefaultChatTopic, err)
	}

	// Subscribe to receive messages from this topic.
	sub, err := topic.Subscribe()
	if err != nil {
		return nil, fmt.Errorf("subscribe to topic %q: %w", DefaultChatTopic, err)
	}

	roomCtx, cancel := context.WithCancel(ctx)

	cr := &ChatRoom{
		ps:       ps,
		topic:    topic,
		sub:      sub,
		self:     h.ID(),
		nick:     nick,
		Messages: make(chan *Message, 128),
		ctx:      roomCtx,
		cancel:   cancel,
	}

	// Start background reader.
	go cr.readLoop()

	log.Printf("[pubsub] Joined topic %q as %q", DefaultChatTopic, nick)
	return cr, nil
}

// Publish sends a chat message to the topic. The message is serialized to JSON
// and broadcast via GossipSub to all subscribed peers (with multihop relay).
func (cr *ChatRoom) Publish(text string) error {
	msg, err := NewChatMessage(cr.self, cr.nick, text)
	if err != nil {
		return fmt.Errorf("create chat message: %w", err)
	}

	data, err := msg.Marshal()
	if err != nil {
		return err
	}

	if err := cr.topic.Publish(cr.ctx, data); err != nil {
		return fmt.Errorf("publish message: %w", err)
	}

	log.Printf("[pubsub] Published message %s (%d bytes)", msg.ID[:8], len(data))
	return nil
}

// PublishAck sends an acknowledgement for a received message.
func (cr *ChatRoom) PublishAck(originalMsgID string) error {
	msg, err := NewAckMessage(cr.self, originalMsgID)
	if err != nil {
		return fmt.Errorf("create ack message: %w", err)
	}

	data, err := msg.Marshal()
	if err != nil {
		return err
	}

	if err := cr.topic.Publish(cr.ctx, data); err != nil {
		return fmt.Errorf("publish ack: %w", err)
	}

	log.Printf("[pubsub] Sent ACK for message %s", originalMsgID[:8])
	return nil
}

// ListPeers returns all peers currently subscribed to the chat topic.
func (cr *ChatRoom) ListPeers() []peer.ID {
	return cr.ps.ListPeers(DefaultChatTopic)
}

// Close unsubscribes from the topic and cancels the read loop.
func (cr *ChatRoom) Close() {
	cr.cancel()
	cr.sub.Cancel()
	if err := cr.topic.Close(); err != nil {
		log.Printf("[pubsub] Error closing topic: %v", err)
	}
	log.Println("[pubsub] Chat room closed.")
}

// readLoop continuously reads messages from the GossipSub subscription.
// It deserializes them and forwards non-self messages to the Messages channel.
func (cr *ChatRoom) readLoop() {
	for {
		raw, err := cr.sub.Next(cr.ctx)
		if err != nil {
			// Context cancelled = normal shutdown.
			if cr.ctx.Err() != nil {
				return
			}
			log.Printf("[pubsub] Read error: %v", err)
			continue
		}

		// Skip messages from ourselves (GossipSub echoes back our own msgs).
		if raw.ReceivedFrom == cr.self {
			continue
		}

		msg, err := UnmarshalMessage(raw.Data)
		if err != nil {
			log.Printf("[pubsub] Invalid message from %s: %v", raw.ReceivedFrom, err)
			continue
		}

		// Deliver to the Messages channel (non-blocking drop if full).
		select {
		case cr.Messages <- msg:
		default:
			log.Printf("[pubsub] Messages channel full, dropping msg %s", msg.ID[:8])
		}
	}
}
