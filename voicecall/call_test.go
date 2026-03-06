package voicecall_test

import (
	"context"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/peer"

	"nec/voicecall"
)

// TestCallEstablishment verifies that two peers can establish a voice call
// via direct QUIC streams, exchange audio packets, and collect metrics.
func TestCallEstablishment(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

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

	hostA.Peerstore().AddAddrs(hostB.ID(), hostB.Addrs(), time.Hour)
	if err := hostA.Connect(ctx, peer.AddrInfo{ID: hostB.ID(), Addrs: hostB.Addrs()}); err != nil {
		t.Fatalf("connect: %v", err)
	}

	cmA := voicecall.NewCallManager(hostA)
	cmB := voicecall.NewCallManager(hostB)

	callEstablished := make(chan struct{}, 1)
	cmB.OnCall = func(remote peer.ID, m *voicecall.CallMetrics) {
		t.Logf("B: call established with %s", remote.String()[:16])
		callEstablished <- struct{}{}
	}

	t.Log("A: initiating call to B...")
	if err := cmA.Call(ctx, hostB.ID()); err != nil {
		t.Fatalf("call: %v", err)
	}

	select {
	case <-callEstablished:
		t.Log("✅ Call established on both sides!")
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for call")
	}

	// Let audio flow for 2 seconds.
	time.Sleep(2 * time.Second)

	metricsA := cmA.ActiveCallMetrics()
	metricsB := cmB.ActiveCallMetrics()

	if metricsA == nil || metricsB == nil {
		t.Fatal("expected active metrics on both sides")
	}

	snapA := metricsA.Snapshot()
	snapB := metricsB.Snapshot()

	t.Logf("A metrics: %s", voicecall.FormatSnapshot(snapA))
	t.Logf("B metrics: %s", voicecall.FormatSnapshot(snapB))

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

	t.Logf("✅ Audio flowing: A sent %d, recv %d | B sent %d, recv %d",
		snapA.PacketsSent, snapA.PacketsRecv, snapB.PacketsSent, snapB.PacketsRecv)

	// Hangup from A — should end call on both sides.
	cmA.Hangup()
	time.Sleep(500 * time.Millisecond) // Wait for stream close to propagate.

	if cmA.IsInCall() {
		t.Error("A should not be in call")
	}
	if cmB.IsInCall() {
		t.Error("B should not be in call (hangup should propagate via stream close)")
	}

	t.Log("✅ Call test passed: audio exchange and hangup propagation working!")
}
