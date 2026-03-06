// Package messaging implements the GossipSub-based messaging layer for NEC.
// It provides multihop message delivery, a TOFU trust model for peers,
// and in-memory store-and-forward for offline message queuing.
package messaging

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

// MessageType distinguishes between different kinds of messages.
type MessageType string

const (
	// TypeChat is a regular text message.
	TypeChat MessageType = "chat"
	// TypeAck acknowledges receipt of a message.
	TypeAck MessageType = "ack"
	// TypeStatus indicates a peer status update (online/offline).
	TypeStatus MessageType = "status"
	// TypeDM is a direct (private) message to a specific peer.
	TypeDM MessageType = "dm"
)

// Message is the canonical JSON wire format for all NEC messages exchanged
// over GossipSub. Every field is populated by the sender before publishing.
type Message struct {
	// ID is a unique random hex string (16 bytes = 32 hex chars) used for
	// deduplication across the GossipSub mesh.
	ID string `json:"id"`

	// Sender is the PeerID of the originator.
	Sender string `json:"sender"`

	// SenderNick is an optional human-readable nickname.
	SenderNick string `json:"sender_nick,omitempty"`

	// Type classifies the message (chat, ack, status).
	Type MessageType `json:"type"`

	// Payload carries the actual content. For TypeChat this is the text;
	// for TypeAck this is the original message ID being acknowledged.
	Payload string `json:"payload"`

	// Timestamp is Unix milliseconds when the message was created.
	Timestamp int64 `json:"timestamp"`
}

// NewChatMessage creates a new chat message with a random unique ID.
func NewChatMessage(senderID peer.ID, nick, text string) (*Message, error) {
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	return &Message{
		ID:         id,
		Sender:     senderID.String(),
		SenderNick: nick,
		Type:       TypeChat,
		Payload:    text,
		Timestamp:  time.Now().UnixMilli(),
	}, nil
}

// NewAckMessage creates an acknowledgement for a received message.
func NewAckMessage(senderID peer.ID, originalMsgID string) (*Message, error) {
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	return &Message{
		ID:        id,
		Sender:    senderID.String(),
		Type:      TypeAck,
		Payload:   originalMsgID,
		Timestamp: time.Now().UnixMilli(),
	}, nil
}

// Marshal serializes the message to JSON bytes for transmission.
func (m *Message) Marshal() ([]byte, error) {
	data, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal message: %w", err)
	}
	return data, nil
}

// UnmarshalMessage deserializes a JSON byte slice into a Message.
func UnmarshalMessage(data []byte) (*Message, error) {
	var msg Message
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, fmt.Errorf("unmarshal message: %w", err)
	}
	return &msg, nil
}

// randomID generates a cryptographically random 16-byte hex string.
func randomID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random ID: %w", err)
	}
	return hex.EncodeToString(b), nil
}
