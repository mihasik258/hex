package messaging

import (
	"log"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

// maxQueuePerPeer is the maximum number of messages to buffer per offline peer.
// This prevents unbounded memory growth when a peer is offline for a long time.
const maxQueuePerPeer = 256

// PendingMessage wraps a message with its intended recipient and creation time
// for TTL-based expiry in the store-and-forward queue.
type PendingMessage struct {
	To        peer.ID
	Msg       *Message
	CreatedAt time.Time
}

// MessageStore is a thread-safe in-memory store-and-forward queue.
// When a peer is offline, messages destined for them are buffered here.
// Once the peer reconnects, queued messages can be delivered.
type MessageStore struct {
	mu      sync.Mutex
	pending map[peer.ID][]*PendingMessage
	ttl     time.Duration // Messages older than this are discarded.
}

// NewMessageStore creates a store with the given message TTL.
// Messages that exceed the TTL when dequeued are silently dropped.
func NewMessageStore(ttl time.Duration) *MessageStore {
	return &MessageStore{
		pending: make(map[peer.ID][]*PendingMessage),
		ttl:     ttl,
	}
}

// Enqueue adds a message to the offline queue for a specific peer.
// Returns false if the queue for that peer is already at capacity.
func (s *MessageStore) Enqueue(to peer.ID, msg *Message) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	queue := s.pending[to]
	if len(queue) >= maxQueuePerPeer {
		log.Printf("[store] Queue full for peer %s, dropping message %s",
			to.String()[:16], msg.ID[:8])
		return false
	}

	s.pending[to] = append(queue, &PendingMessage{
		To:        to,
		Msg:       msg,
		CreatedAt: time.Now(),
	})

	log.Printf("[store] Enqueued message %s for offline peer %s (queue size: %d)",
		msg.ID[:8], to.String()[:16], len(s.pending[to]))
	return true
}

// Dequeue retrieves and removes all pending messages for a peer.
// Expired messages (older than TTL) are automatically filtered out.
func (s *MessageStore) Dequeue(to peer.ID) []*Message {
	s.mu.Lock()
	defer s.mu.Unlock()

	queue, ok := s.pending[to]
	if !ok || len(queue) == 0 {
		return nil
	}

	now := time.Now()
	var result []*Message
	for _, pm := range queue {
		if now.Sub(pm.CreatedAt) <= s.ttl {
			result = append(result, pm.Msg)
		}
	}

	// Clear the queue after retrieval.
	delete(s.pending, to)

	if len(result) > 0 {
		log.Printf("[store] Dequeued %d message(s) for peer %s", len(result), to.String()[:16])
	}
	return result
}

// PendingCount returns the number of messages queued for a specific peer.
func (s *MessageStore) PendingCount(to peer.ID) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending[to])
}

// TotalPending returns the total number of messages across all queues.
func (s *MessageStore) TotalPending() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	total := 0
	for _, q := range s.pending {
		total += len(q)
	}
	return total
}

// Cleanup removes all expired messages from all queues.
// This can be called periodically to free memory.
func (s *MessageStore) Cleanup() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	removed := 0

	for pid, queue := range s.pending {
		var kept []*PendingMessage
		for _, pm := range queue {
			if now.Sub(pm.CreatedAt) <= s.ttl {
				kept = append(kept, pm)
			} else {
				removed++
			}
		}
		if len(kept) == 0 {
			delete(s.pending, pid)
		} else {
			s.pending[pid] = kept
		}
	}

	if removed > 0 {
		log.Printf("[store] Cleanup: removed %d expired message(s)", removed)
	}
	return removed
}
