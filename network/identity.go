package network

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/libp2p/go-libp2p/core/peer"
)

// SafetyNumberLength is the number of hex characters used for the safety number
// fingerprint (out of 64 for SHA-256). 8 hex chars = 4 bytes = 32 bits of the
// hash, sufficient for human-readable OOB verification (TOFU model).
const SafetyNumberLength = 8

// SafetyNumber computes a human-readable fingerprint of a PeerID for
// out-of-band (OOB) verification in the TOFU trust model.
//
// The fingerprint is the first 8 hex characters of SHA-256(PeerID raw bytes).
// Users compare safety numbers in person (e.g. by scanning a QR code or reading
// aloud) to verify they're talking to the correct peer, defeating MITM attacks.
func SafetyNumber(id peer.ID) string {
	hash := sha256.Sum256([]byte(id))
	full := hex.EncodeToString(hash[:])
	return strings.ToUpper(full[:SafetyNumberLength])
}

// FormatSafetyNumber returns a formatted safety number string with the PeerID
// prefix for display purposes (e.g. "PeerID: Qm... | Safety Number: A1B2C3D4").
func FormatSafetyNumber(id peer.ID) string {
	sn := SafetyNumber(id)
	short := id.String()
	if len(short) > 16 {
		short = short[:16] + "..."
	}
	return fmt.Sprintf("PeerID: %s | Safety Number: %s", short, sn)
}
