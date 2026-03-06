package voicecall_test

import (
	"context"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/peer"

	"nec/voicecall"
)

// TestCallEstablishment verifies that two peers can establish a WebRTC call
// via libp2p signaling, exchange audio tone packets over DataChannel, and
// collect jitter/loss metrics.
func TestCallEstablishment(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Create two libp2p hosts.
	hostA, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/udp/0/quic-v1"))
	if err != nil {
		t.Fatalf("create hostA: %v", err)
	}
	defer hostA.Close()

	hostB, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/udp/0/quic-v1"))
	if err != nil {
		t.Fatalf("create hostB: %v", err)
	}
	defer hostB.Close()

	// Connect A → B.
	hostA.Peerstore().AddAddrs(hostB.ID(), hostB.Addrs(), time.Hour)
	if err := hostA.Connect(ctx, peer.AddrInfo{ID: hostB.ID(), Addrs: hostB.Addrs()}); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// Create call managers.
	cmA := voicecall.NewCallManager(hostA)
	cmB := voicecall.NewCallManager(hostB)

	// Track when call is established on B side.
	callEstablished := make(chan struct{}, 1)
	cmB.OnCall = func(remote peer.ID, m *voicecall.CallMetrics) {
		t.Logf("B: call established with %s", remote.String()[:16])
		callEstablished <- struct{}{}
	}

	// A calls B.
	t.Log("A: initiating call to B...")
	if err := cmA.Call(ctx, hostB.ID()); err != nil {
		t.Fatalf("call: %v", err)
	}

	// Wait for call to establish on B side.
	select {
	case <-callEstablished:
		t.Log("✅ Call established on both sides!")
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for call to establish")
	}

	// Let audio tone flow for 2 seconds.
	t.Log("Letting audio flow for 2 seconds...")
	time.Sleep(2 * time.Second)

	// Check metrics on both sides.
	metricsA := cmA.ActiveCallMetrics()
	metricsB := cmB.ActiveCallMetrics()

	if metricsA == nil || metricsB == nil {
		t.Fatal("expected active metrics on both sides")
	}

	snapA := metricsA.Snapshot()
	snapB := metricsB.Snapshot()

	t.Logf("A metrics: %s", voicecall.FormatSnapshot(snapA))
	t.Logf("B metrics: %s", voicecall.FormatSnapshot(snapB))

	// Verify packets were sent and received.
	if snapA.PacketsSent == 0 {
		t.Error("A sent 0 packets")
	}
	if snapB.PacketsSent == 0 {
		t.Error("B sent 0 packets")
	}
	if snapA.PacketsRecv == 0 {
		t.Error("A received 0 packets")
	}
	if snapB.PacketsRecv == 0 {
		t.Error("B received 0 packets")
	}

	// Verify jitter is reasonable (should be very low on localhost).
	if snapA.JitterMs > 50 {
		t.Errorf("A jitter too high: %.2f ms", snapA.JitterMs)
	}
	if snapB.JitterMs > 50 {
		t.Errorf("B jitter too high: %.2f ms", snapB.JitterMs)
	}

	t.Logf("✅ Audio flowing: A sent %d pkts, recv %d | B sent %d pkts, recv %d",
		snapA.PacketsSent, snapA.PacketsRecv, snapB.PacketsSent, snapB.PacketsRecv)

	// Hangup.
	cmA.Hangup()
	cmB.Hangup()

	if cmA.IsInCall() || cmB.IsInCall() {
		t.Error("calls should be ended after hangup")
	}

	t.Log("✅ Call test passed: signaling, audio, metrics, hangup all working!")
}
