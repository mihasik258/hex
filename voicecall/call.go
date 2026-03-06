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
	"github.com/pion/webrtc/v4"
)

// CallManager handles WebRTC call lifecycle: signaling over libp2p QUIC
// streams, PeerConnection management, DataChannel audio, and metrics.
type CallManager struct {
	host   host.Host
	mu     sync.Mutex
	active *ActiveCall
	OnCall func(remotePeer peer.ID, metrics *CallMetrics)  // Callback when call starts.
	OnEnd  func(remotePeer peer.ID, final MetricsSnapshot) // Callback when call ends.
}

// ActiveCall represents an ongoing voice call.
type ActiveCall struct {
	RemotePeer peer.ID
	PC         *webrtc.PeerConnection
	DC         *webrtc.DataChannel
	Metrics    *CallMetrics
	ctx        context.Context
	cancel     context.CancelFunc
	lastSeq    uint32 // Last received sequence number for loss detection.
}

// NewCallManager creates a CallManager and registers the signaling stream handler.
func NewCallManager(h host.Host) *CallManager {
	cm := &CallManager{host: h}

	// Register handler for incoming call signaling.
	h.SetStreamHandler(SignalProtocolID, func(s network.Stream) {
		cm.handleIncomingCall(s)
	})

	log.Printf("[call] Manager ready (protocol=%s)", SignalProtocolID)
	return cm
}

// Call initiates a voice call to the specified peer (caller side).
func (cm *CallManager) Call(ctx context.Context, target peer.ID) error {
	cm.mu.Lock()
	if cm.active != nil {
		cm.mu.Unlock()
		return fmt.Errorf("already in a call with %s", cm.active.RemotePeer.String()[:16])
	}
	cm.mu.Unlock()

	// Open signaling stream.
	s, err := cm.host.NewStream(ctx, target, SignalProtocolID)
	if err != nil {
		return fmt.Errorf("open signaling stream: %w", err)
	}

	log.Printf("[call] Calling peer %s...", target.String()[:16])

	// Create WebRTC PeerConnection (caller).
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		s.Close()
		return fmt.Errorf("create peer connection: %w", err)
	}

	// Create DataChannel for audio.
	dc, err := pc.CreateDataChannel("audio", &webrtc.DataChannelInit{
		Ordered:        boolPtr(false), // Unordered for lower latency.
		MaxRetransmits: uint16Ptr(0),   // No retransmits — real-time audio.
	})
	if err != nil {
		pc.Close()
		s.Close()
		return fmt.Errorf("create data channel: %w", err)
	}

	callCtx, callCancel := context.WithCancel(ctx)
	metrics := NewCallMetrics()

	call := &ActiveCall{
		RemotePeer: target,
		PC:         pc,
		DC:         dc,
		Metrics:    metrics,
		ctx:        callCtx,
		cancel:     callCancel,
	}

	// Handle incoming audio on this DataChannel.
	dc.OnOpen(func() {
		log.Printf("[call] DataChannel open — starting audio tone")
		go cm.sendToneLoop(call)
	})
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		cm.handleAudioPacket(call, msg.Data)
	})

	// ICE connection state monitoring.
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Printf("[call] ICE state: %s", state.String())
		if state == webrtc.ICEConnectionStateFailed || state == webrtc.ICEConnectionStateDisconnected {
			cm.Hangup()
		}
	})

	// Create and send SDP offer.
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		callCancel()
		pc.Close()
		s.Close()
		return fmt.Errorf("create offer: %w", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		callCancel()
		pc.Close()
		s.Close()
		return fmt.Errorf("set local desc: %w", err)
	}

	// Wait for ICE gathering to complete.
	gatherDone := webrtc.GatheringCompletePromise(pc)
	<-gatherDone

	// Send offer with gathered candidates.
	if err := WriteSignal(s, &SignalMessage{
		Type:    SignalOffer,
		Payload: pc.LocalDescription().SDP,
	}); err != nil {
		callCancel()
		pc.Close()
		s.Close()
		return fmt.Errorf("send offer: %w", err)
	}
	log.Printf("[call] SDP offer sent")

	// Read answer.
	ansMsg, err := ReadSignal(s)
	if err != nil {
		callCancel()
		pc.Close()
		s.Close()
		return fmt.Errorf("read answer: %w", err)
	}
	if ansMsg.Type == SignalHangup {
		callCancel()
		pc.Close()
		s.Close()
		return fmt.Errorf("call rejected by peer")
	}
	if ansMsg.Type != SignalAnswer {
		callCancel()
		pc.Close()
		s.Close()
		return fmt.Errorf("unexpected signal type: %s", ansMsg.Type)
	}

	answer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  ansMsg.Payload,
	}
	if err := pc.SetRemoteDescription(answer); err != nil {
		callCancel()
		pc.Close()
		s.Close()
		return fmt.Errorf("set remote desc: %w", err)
	}
	log.Printf("[call] SDP answer received — call established!")

	cm.mu.Lock()
	cm.active = call
	cm.mu.Unlock()

	if cm.OnCall != nil {
		cm.OnCall(target, metrics)
	}

	// Start metrics logging in background.
	go cm.metricsLoop(call)

	// Wait for hangup signal in background.
	go func() {
		defer s.Close()
		for {
			msg, err := ReadSignal(s)
			if err != nil {
				return
			}
			if msg.Type == SignalHangup {
				log.Printf("[call] Remote peer hung up")
				cm.Hangup()
				return
			}
		}
	}()

	return nil
}

// handleIncomingCall processes an incoming call (callee side).
func (cm *CallManager) handleIncomingCall(s network.Stream) {
	remotePeer := s.Conn().RemotePeer()
	log.Printf("[call] Incoming call from %s", remotePeer.String()[:16])

	cm.mu.Lock()
	if cm.active != nil {
		cm.mu.Unlock()
		log.Printf("[call] Busy — rejecting call from %s", remotePeer.String()[:16])
		_ = WriteSignal(s, &SignalMessage{Type: SignalHangup, Payload: "busy"})
		s.Close()
		return
	}
	cm.mu.Unlock()

	// Read SDP offer.
	offerMsg, err := ReadSignal(s)
	if err != nil {
		log.Printf("[call] Failed to read offer: %v", err)
		s.Close()
		return
	}
	if offerMsg.Type != SignalOffer {
		log.Printf("[call] Expected offer, got %s", offerMsg.Type)
		s.Close()
		return
	}

	// Create PeerConnection (callee).
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		log.Printf("[call] Failed to create PC: %v", err)
		s.Close()
		return
	}

	callCtx, callCancel := context.WithCancel(context.Background())
	metrics := NewCallMetrics()

	call := &ActiveCall{
		RemotePeer: remotePeer,
		PC:         pc,
		Metrics:    metrics,
		ctx:        callCtx,
		cancel:     callCancel,
	}

	// Handle DataChannel created by caller.
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		log.Printf("[call] DataChannel %q received", dc.Label())
		call.DC = dc

		dc.OnOpen(func() {
			log.Printf("[call] DataChannel open — starting audio tone")
			go cm.sendToneLoop(call)
		})
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			cm.handleAudioPacket(call, msg.Data)
		})
	})

	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Printf("[call] ICE state: %s", state.String())
		if state == webrtc.ICEConnectionStateFailed || state == webrtc.ICEConnectionStateDisconnected {
			cm.Hangup()
		}
	})

	// Set remote description (offer).
	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  offerMsg.Payload,
	}
	if err := pc.SetRemoteDescription(offer); err != nil {
		log.Printf("[call] Set remote desc: %v", err)
		callCancel()
		pc.Close()
		s.Close()
		return
	}

	// Create and send answer.
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		log.Printf("[call] Create answer: %v", err)
		callCancel()
		pc.Close()
		s.Close()
		return
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		log.Printf("[call] Set local desc: %v", err)
		callCancel()
		pc.Close()
		s.Close()
		return
	}

	gatherDone := webrtc.GatheringCompletePromise(pc)
	<-gatherDone

	if err := WriteSignal(s, &SignalMessage{
		Type:    SignalAnswer,
		Payload: pc.LocalDescription().SDP,
	}); err != nil {
		log.Printf("[call] Send answer: %v", err)
		callCancel()
		pc.Close()
		s.Close()
		return
	}

	log.Printf("[call] SDP answer sent — call established!")

	cm.mu.Lock()
	cm.active = call
	cm.mu.Unlock()

	if cm.OnCall != nil {
		cm.OnCall(remotePeer, metrics)
	}

	go cm.metricsLoop(call)

	// Wait for hangup.
	go func() {
		defer s.Close()
		for {
			msg, err := ReadSignal(s)
			if err != nil {
				return
			}
			if msg.Type == SignalHangup {
				log.Printf("[call] Remote peer hung up")
				cm.Hangup()
				return
			}
		}
	}()
}

// sendToneLoop generates and sends test audio tone packets every 20ms.
func (cm *CallManager) sendToneLoop(call *ActiveCall) {
	var seq uint32
	sampleOffset := 0
	ticker := time.NewTicker(ToneInterval)
	defer ticker.Stop()

	for {
		select {
		case <-call.ctx.Done():
			return
		case <-ticker.C:
			if call.DC == nil || call.DC.ReadyState() != webrtc.DataChannelStateOpen {
				continue
			}

			pkt := GenerateTonePacket(seq, sampleOffset)
			data := MarshalAudioPacket(pkt)

			if err := call.DC.Send(data); err != nil {
				log.Printf("[call] Send audio error: %v", err)
				return
			}
			call.Metrics.RecordSent(len(data))

			seq++
			sampleOffset += len(pkt.Data)
		}
	}
}

// handleAudioPacket processes a received audio packet, updating metrics.
func (cm *CallManager) handleAudioPacket(call *ActiveCall, data []byte) {
	pkt, err := UnmarshalAudioPacket(data)
	if err != nil {
		log.Printf("[call] Invalid audio packet: %v", err)
		return
	}

	sendTime := time.Unix(0, pkt.Timestamp)
	call.Metrics.RecordReceived(len(data), sendTime)

	// Detect lost packets via sequence gap.
	if call.lastSeq > 0 && pkt.Sequence > call.lastSeq+1 {
		lost := int(pkt.Sequence - call.lastSeq - 1)
		call.Metrics.RecordLost(lost)
	}
	call.lastSeq = pkt.Sequence
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
	defer cm.mu.Unlock()

	if cm.active == nil {
		return
	}

	call := cm.active
	cm.active = nil

	call.cancel()
	if call.DC != nil {
		call.DC.Close()
	}
	call.PC.Close()

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

func boolPtr(b bool) *bool       { return &b }
func uint16Ptr(v uint16) *uint16 { return &v }
