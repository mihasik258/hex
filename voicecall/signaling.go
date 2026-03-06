// Package voicecall implements WebRTC-based voice calls for NEC.
// SDP signaling is performed over libp2p QUIC streams, and audio data
// flows through WebRTC DataChannels with jitter/loss metrics.
package voicecall

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/libp2p/go-libp2p/core/network"
)

// SignalProtocolID is the libp2p protocol for WebRTC SDP signaling.
const SignalProtocolID = "/nec/webrtc-signal/1.0.0"

// SignalType distinguishes SDP messages exchanged during call setup.
type SignalType string

const (
	SignalOffer     SignalType = "offer"
	SignalAnswer    SignalType = "answer"
	SignalCandidate SignalType = "candidate"
	SignalHangup    SignalType = "hangup"
)

// SignalMessage is the JSON envelope exchanged over the QUIC signaling stream.
type SignalMessage struct {
	Type    SignalType `json:"type"`
	Payload string     `json:"payload"` // SDP string or ICE candidate JSON.
}

// WriteSignal serializes and sends a SignalMessage over a libp2p stream.
func WriteSignal(s network.Stream, msg *SignalMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal signal: %w", err)
	}
	data = append(data, '\n') // Newline-delimited JSON.
	if _, err := s.Write(data); err != nil {
		return fmt.Errorf("write signal: %w", err)
	}
	return nil
}

// ReadSignal reads a single newline-delimited SignalMessage from the stream.
func ReadSignal(s network.Stream) (*SignalMessage, error) {
	// Read until newline (simple framing for signaling — messages are small).
	var buf []byte
	one := make([]byte, 1)
	for {
		n, err := s.Read(one)
		if n > 0 {
			if one[0] == '\n' {
				break
			}
			buf = append(buf, one[0])
		}
		if err == io.EOF {
			if len(buf) > 0 {
				break
			}
			return nil, fmt.Errorf("stream closed before signal received")
		}
		if err != nil {
			return nil, fmt.Errorf("read signal: %w", err)
		}
	}

	var msg SignalMessage
	if err := json.Unmarshal(buf, &msg); err != nil {
		return nil, fmt.Errorf("unmarshal signal: %w", err)
	}
	return &msg, nil
}
