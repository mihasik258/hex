// Package voicecall implements voice calls for NEC using direct libp2p
// QUIC streams instead of WebRTC. This is more reliable on real networks
// since it reuses the existing P2P connection rather than establishing
// a separate WebRTC/ICE connection.
package voicecall

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

// AudioProtocolID is the libp2p protocol for audio streams.
const AudioProtocolID = "/nec/audio/1.0.0"

// CallManager handles voice call lifecycle over libp2p QUIC streams.
type CallManager struct {
	host   host.Host
	mu     sync.Mutex
	active *ActiveCall
	OnCall func(remotePeer peer.ID, metrics *CallMetrics)
	OnEnd  func(remotePeer peer.ID, final MetricsSnapshot)
}

// ActiveCall represents an ongoing voice call.
type ActiveCall struct {
	RemotePeer peer.ID
	Metrics    *CallMetrics
	stream     network.Stream // The audio stream.
	ctx        context.Context
	cancel     context.CancelFunc
	lastSeq    uint32
	ended      bool
}

// NewCallManager creates a CallManager and registers the audio stream handler.
func NewCallManager(h host.Host) *CallManager {
	cm := &CallManager{host: h}

	h.SetStreamHandler(AudioProtocolID, func(s network.Stream) {
		cm.handleIncomingCall(s)
	})

	log.Printf("[call] Manager ready (protocol=%s)", AudioProtocolID)
	return cm
}

// Call initiates a voice call to the specified peer.
func (cm *CallManager) Call(ctx context.Context, target peer.ID) error {
	cm.mu.Lock()
	if cm.active != nil {
		cm.mu.Unlock()
		return fmt.Errorf("already in a call with %s", cm.active.RemotePeer.String()[:16])
	}
	cm.mu.Unlock()

	// Open audio stream to the peer.
	s, err := cm.host.NewStream(ctx, target, AudioProtocolID)
	if err != nil {
		return fmt.Errorf("open audio stream: %w", err)
	}

	log.Printf("[call] Calling peer %s...", target.String()[:16])

	callCtx, callCancel := context.WithCancel(ctx)
	metrics := NewCallMetrics()

	call := &ActiveCall{
		RemotePeer: target,
		Metrics:    metrics,
		stream:     s,
		ctx:        callCtx,
		cancel:     callCancel,
	}

	cm.mu.Lock()
	cm.active = call
	cm.mu.Unlock()

	log.Printf("[call] Call established with %s!", target.String()[:16])

	if cm.OnCall != nil {
		cm.OnCall(target, metrics)
	}

	// Start sending audio.
	go cm.sendAudioLoop(call)
	// Start receiving audio.
	go cm.recvAudioLoop(call)
	// Start metrics logging.
	go cm.metricsLoop(call)

	return nil
}

// handleIncomingCall processes an incoming call.
func (cm *CallManager) handleIncomingCall(s network.Stream) {
	remotePeer := s.Conn().RemotePeer()
	log.Printf("[call] Incoming call from %s", remotePeer.String()[:16])

	cm.mu.Lock()
	if cm.active != nil {
		cm.mu.Unlock()
		log.Printf("[call] Busy — rejecting call from %s", remotePeer.String()[:16])
		s.Close()
		return
	}

	callCtx, callCancel := context.WithCancel(context.Background())
	metrics := NewCallMetrics()

	call := &ActiveCall{
		RemotePeer: remotePeer,
		Metrics:    metrics,
		stream:     s,
		ctx:        callCtx,
		cancel:     callCancel,
	}

	cm.active = call
	cm.mu.Unlock()

	log.Printf("[call] Call established with %s!", remotePeer.String()[:16])

	if cm.OnCall != nil {
		cm.OnCall(remotePeer, metrics)
	}

	// Start audio exchange.
	go cm.sendAudioLoop(call)
	go cm.recvAudioLoop(call)
	go cm.metricsLoop(call)
}

// sendAudioLoop generates and sends audio tone packets every 20ms.
func (cm *CallManager) sendAudioLoop(call *ActiveCall) {
	var seq uint32
	sampleOffset := 0
	ticker := time.NewTicker(ToneInterval)
	defer ticker.Stop()

	for {
		select {
		case <-call.ctx.Done():
			return
		case <-ticker.C:
			pkt := GenerateTonePacket(seq, sampleOffset)
			data := MarshalAudioPacket(pkt)

			if _, err := call.stream.Write(data); err != nil {
				// Stream closed = remote hung up.
				cm.cleanupCall(call)
				return
			}
			call.Metrics.RecordSent(len(data))

			seq++
			sampleOffset += len(pkt.Data)
		}
	}
}

// recvAudioLoop reads audio packets from the stream.
func (cm *CallManager) recvAudioLoop(call *ActiveCall) {
	// Each audio packet = 12 bytes header + TonePacketSize bytes data.
	pktSize := 12 + TonePacketSize
	buf := make([]byte, pktSize)

	for {
		// Read exactly one packet.
		n, err := readFull(call.stream, buf)
		if err != nil || n == 0 {
			// Stream closed = remote hung up.
			cm.cleanupCall(call)
			return
		}

		pkt, err := UnmarshalAudioPacket(buf[:n])
		if err != nil {
			continue
		}

		sendTime := time.Unix(0, pkt.Timestamp)
		call.Metrics.RecordReceived(n, sendTime)

		// Detect lost packets via sequence gap.
		if call.lastSeq > 0 && pkt.Sequence > call.lastSeq+1 {
			lost := int(pkt.Sequence - call.lastSeq - 1)
			call.Metrics.RecordLost(lost)
		}
		call.lastSeq = pkt.Sequence
	}
}

// readFull reads exactly len(buf) bytes from the stream.
func readFull(s network.Stream, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := s.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// metricsLoop logs call metrics every 3 seconds.
func (cm *CallManager) metricsLoop(call *ActiveCall) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-call.ctx.Done():
			return
		case <-ticker.C:
			call.Metrics.LogSnapshot()
		}
	}
}

// Hangup ends the active call.
func (cm *CallManager) Hangup() {
	cm.mu.Lock()
	call := cm.active
	cm.mu.Unlock()

	if call == nil {
		return
	}

	cm.cleanupCall(call)
}

// cleanupCall tears down the call.
func (cm *CallManager) cleanupCall(call *ActiveCall) {
	cm.mu.Lock()
	if call.ended {
		cm.mu.Unlock()
		return
	}
	call.ended = true
	if cm.active == call {
		cm.active = nil
	}
	cm.mu.Unlock()

	call.cancel()
	call.stream.Close()

	final := call.Metrics.Snapshot()
	log.Printf("[call] Call ended. Final: %s", FormatSnapshot(final))

	if cm.OnEnd != nil {
		cm.OnEnd(call.RemotePeer, final)
	}
}

// IsInCall returns true if there's an active call.
func (cm *CallManager) IsInCall() bool {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return cm.active != nil
}

// ActiveCallMetrics returns the metrics of the current call, or nil.
func (cm *CallManager) ActiveCallMetrics() *CallMetrics {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if cm.active == nil {
		return nil
	}
	return cm.active.Metrics
}
