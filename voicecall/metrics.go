package voicecall

import (
	"fmt"
	"log"
	"math"
	"sync"
	"time"
)

// CallMetrics tracks real-time quality statistics for a voice call.
// It is thread-safe and accumulates data from incoming audio packets.
type CallMetrics struct {
	mu sync.Mutex

	// Counters.
	PacketsSent     int64
	PacketsReceived int64
	PacketsLost     int64
	BytesSent       int64
	BytesReceived   int64

	// Jitter tracking (RFC 3550 interarrival jitter).
	lastArrival time.Time
	lastTransit float64
	jitterAcc   float64 // Running jitter estimate in ms.

	// Latency.
	totalLatency   time.Duration
	latencySamples int64

	// Timestamps.
	StartTime time.Time
}

// NewCallMetrics creates a fresh metrics tracker.
func NewCallMetrics() *CallMetrics {
	return &CallMetrics{
		StartTime: time.Now(),
	}
}

// RecordSent records an outgoing packet.
func (m *CallMetrics) RecordSent(bytes int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.PacketsSent++
	m.BytesSent += int64(bytes)
}

// RecordReceived records an incoming packet and updates jitter estimation.
// sendTimestamp is the time the packet was originally sent (carried in payload).
func (m *CallMetrics) RecordReceived(bytes int, sendTimestamp time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	m.PacketsReceived++
	m.BytesReceived += int64(bytes)

	// Latency calculation.
	latency := now.Sub(sendTimestamp)
	m.totalLatency += latency
	m.latencySamples++

	// RFC 3550 jitter calculation.
	if !m.lastArrival.IsZero() {
		// Transit time difference.
		transit := float64(now.Sub(sendTimestamp).Milliseconds())
		d := math.Abs(transit - m.lastTransit)
		// Exponential moving average: J(i) = J(i-1) + (|D(i)| - J(i-1)) / 16
		m.jitterAcc += (d - m.jitterAcc) / 16.0
		m.lastTransit = transit
	} else {
		m.lastTransit = float64(now.Sub(sendTimestamp).Milliseconds())
	}
	m.lastArrival = now
}

// RecordLost records N lost packets (detected via sequence gap).
func (m *CallMetrics) RecordLost(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.PacketsLost += int64(n)
}

// Snapshot returns a formatted snapshot of current metrics.
type MetricsSnapshot struct {
	Duration      time.Duration
	PacketsSent   int64
	PacketsRecv   int64
	PacketsLost   int64
	LossPercent   float64
	JitterMs      float64
	AvgLatencyMs  float64
	BytesSent     int64
	BytesReceived int64
}

// Snapshot returns a point-in-time copy of the metrics.
func (m *CallMetrics) Snapshot() MetricsSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	totalExpected := m.PacketsReceived + m.PacketsLost
	var lossPct float64
	if totalExpected > 0 {
		lossPct = float64(m.PacketsLost) / float64(totalExpected) * 100.0
	}

	var avgLatency float64
	if m.latencySamples > 0 {
		avgLatency = float64(m.totalLatency.Milliseconds()) / float64(m.latencySamples)
	}

	return MetricsSnapshot{
		Duration:      time.Since(m.StartTime),
		PacketsSent:   m.PacketsSent,
		PacketsRecv:   m.PacketsReceived,
		PacketsLost:   m.PacketsLost,
		LossPercent:   lossPct,
		JitterMs:      m.jitterAcc,
		AvgLatencyMs:  avgLatency,
		BytesSent:     m.BytesSent,
		BytesReceived: m.BytesReceived,
	}
}

// LogSnapshot prints current metrics to the log.
func (m *CallMetrics) LogSnapshot() {
	s := m.Snapshot()
	log.Printf("[call-metrics] Duration: %s | Sent: %d pkts | Recv: %d pkts | Lost: %d (%.1f%%) | Jitter: %.2f ms | Avg Latency: %.1f ms | Bytes: ↑%d ↓%d",
		s.Duration.Round(time.Second), s.PacketsSent, s.PacketsRecv,
		s.PacketsLost, s.LossPercent, s.JitterMs, s.AvgLatencyMs,
		s.BytesSent, s.BytesReceived)
}

// FormatSnapshot returns a human-readable string of the metrics.
func FormatSnapshot(s MetricsSnapshot) string {
	return fmt.Sprintf(
		"Duration: %s | Sent: %d | Recv: %d | Lost: %d (%.1f%%) | Jitter: %.2fms | Latency: %.1fms",
		s.Duration.Round(time.Second), s.PacketsSent, s.PacketsRecv,
		s.PacketsLost, s.LossPercent, s.JitterMs, s.AvgLatencyMs,
	)
}
