package voicecall

import (
	"encoding/binary"
	"math"
	"time"
)

// TonePacketSize is the size of one audio tone packet in bytes.
// Simulates a 20ms Opus frame at ~64kbps → ~160 bytes payload.
const TonePacketSize = 160

// ToneInterval is the interval between tone packets (20ms = standard for VoIP).
const ToneInterval = 20 * time.Millisecond

// ToneFrequency is the frequency of the test sine wave in Hz (440 Hz = A4 note).
const ToneFrequency = 440.0

// SampleRate is the simulated audio sample rate.
const SampleRate = 8000 // 8 kHz (telephone quality).

// AudioPacket is the wire format for audio data sent over WebRTC DataChannel.
// It includes a sequence number and timestamp for jitter/loss detection.
type AudioPacket struct {
	Sequence  uint32 // Monotonically increasing sequence number.
	Timestamp int64  // Unix nanoseconds when the packet was created.
	Data      []byte // Raw audio samples (sine wave tone).
}

// MarshalAudioPacket serializes an AudioPacket to bytes.
// Format: [4B seq][8B timestamp][payload...]
func MarshalAudioPacket(pkt *AudioPacket) []byte {
	buf := make([]byte, 4+8+len(pkt.Data))
	binary.BigEndian.PutUint32(buf[0:4], pkt.Sequence)
	binary.BigEndian.PutUint64(buf[4:12], uint64(pkt.Timestamp))
	copy(buf[12:], pkt.Data)
	return buf
}

// UnmarshalAudioPacket deserializes bytes into an AudioPacket.
func UnmarshalAudioPacket(data []byte) (*AudioPacket, error) {
	if len(data) < 12 {
		return nil, errTooShort
	}
	return &AudioPacket{
		Sequence:  binary.BigEndian.Uint32(data[0:4]),
		Timestamp: int64(binary.BigEndian.Uint64(data[4:12])),
		Data:      data[12:],
	}, nil
}

// errTooShort is returned when a packet is too short to contain the header.
var errTooShort = &tooShortError{}

type tooShortError struct{}

func (e *tooShortError) Error() string { return "audio packet too short (need at least 12 bytes)" }

// GenerateTonePacket creates an audio packet containing a 20ms segment of
// a 440 Hz sine wave. This simulates real Opus audio for demo/testing purposes.
func GenerateTonePacket(seq uint32, sampleOffset int) *AudioPacket {
	samplesPerPacket := SampleRate * int(ToneInterval.Milliseconds()) / 1000 // 160 samples
	data := make([]byte, samplesPerPacket)

	for i := 0; i < samplesPerPacket; i++ {
		t := float64(sampleOffset+i) / float64(SampleRate)
		// Generate sine wave, quantize to 8-bit unsigned PCM.
		sample := math.Sin(2 * math.Pi * ToneFrequency * t)
		data[i] = byte(128 + int(sample*127))
	}

	return &AudioPacket{
		Sequence:  seq,
		Timestamp: time.Now().UnixNano(),
		Data:      data,
	}
}
