package messaging

import (
	"context"
	"fmt"
	"log"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

// DMProtocolID is the libp2p protocol for direct (private) messages.
const DMProtocolID = "/nec/dm/1.0.0"

// DMHandler is a callback invoked when a direct message is received.
type DMHandler func(msg *Message)

// RegisterDMHandler sets up the stream handler for incoming direct messages.
// Each DM arrives on a fresh QUIC stream, is read as a single JSON message,
// and then the stream is closed.
func RegisterDMHandler(h host.Host, handler DMHandler) {
	h.SetStreamHandler(DMProtocolID, func(s network.Stream) {
		defer s.Close()

		// Read the length-prefixed JSON message.
		msg, err := readDM(s)
		if err != nil {
			log.Printf("[dm] Failed to read DM from %s: %v",
				s.Conn().RemotePeer().String()[:16], err)
			return
		}

		log.Printf("[dm] Received DM from %s (type=%s, %d bytes)",
			msg.Sender[:8], msg.Type, len(msg.Payload))

		handler(msg)
	})

	log.Printf("[dm] Handler registered (protocol=%s)", DMProtocolID)
}

// SendDM sends a direct (private) message to a specific peer over a new
// QUIC stream. This bypasses GossipSub and is truly peer-to-peer.
func SendDM(ctx context.Context, h host.Host, self peer.ID, nick string, target peer.ID, text string) error {
	msg, err := NewChatMessage(self, nick, text)
	if err != nil {
		return fmt.Errorf("create DM: %w", err)
	}
	// Mark as DM type for the receiver.
	msg.Type = TypeDM

	s, err := h.NewStream(ctx, target, DMProtocolID)
	if err != nil {
		return fmt.Errorf("open DM stream to %s: %w", target.String()[:16], err)
	}
	defer s.Close()

	if err := writeDM(s, msg); err != nil {
		return fmt.Errorf("write DM: %w", err)
	}

	log.Printf("[dm] Sent DM to %s (%d bytes)", target.String()[:16], len(text))
	return nil
}

// writeDM writes a JSON message with a length prefix to the stream.
func writeDM(s network.Stream, msg *Message) error {
	data, err := msg.Marshal()
	if err != nil {
		return err
	}

	// Simple framing: write all data then close.
	if _, err := s.Write(data); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// readDM reads a single JSON message from the stream (until EOF).
func readDM(s network.Stream) (*Message, error) {
	// Read all data from stream (sender closes after writing).
	var buf []byte
	tmp := make([]byte, 4096)
	for {
		n, err := s.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break // EOF or error = we have the full message.
		}
	}

	if len(buf) == 0 {
		return nil, fmt.Errorf("empty DM stream")
	}

	return UnmarshalMessage(buf)
}
