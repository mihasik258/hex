package messaging

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"

	"nec/network"
)

// TrustStatus represents the verification state of a peer in the TOFU model.
type TrustStatus string

const (
	// StatusUnverified means the peer has been seen but not manually verified.
	StatusUnverified TrustStatus = "unverified"
	// StatusVerified means the peer's identity was confirmed via OOB check
	// (e.g. comparing Safety Numbers in person).
	StatusVerified TrustStatus = "verified"
	// StatusBlocked means the peer is explicitly blocked.
	StatusBlocked TrustStatus = "blocked"
)

// PeerRecord holds metadata about a known peer.
type PeerRecord struct {
	PeerID       peer.ID     `json:"peer_id"`
	SafetyNumber string      `json:"safety_number"`
	Status       TrustStatus `json:"status"`
	Nick         string      `json:"nick,omitempty"`
	FirstSeen    time.Time   `json:"first_seen"`
	LastSeen     time.Time   `json:"last_seen"`
}

// TrustStore is a thread-safe in-memory table of known peers and their
// verification status. It implements the TOFU (Trust On First Use) model:
// peers are initially "unverified" and can be manually promoted to "verified"
// after an out-of-band Safety Number comparison.
type TrustStore struct {
	mu    sync.RWMutex
	peers map[peer.ID]*PeerRecord
}

// NewTrustStore creates an empty trust store.
func NewTrustStore() *TrustStore {
	return &TrustStore{
		peers: make(map[peer.ID]*PeerRecord),
	}
}

// RecordPeer registers a peer in the trust store on first contact, or updates
// the LastSeen timestamp if already known. New peers default to StatusUnverified.
func (ts *TrustStore) RecordPeer(id peer.ID, nick string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	if rec, ok := ts.peers[id]; ok {
		rec.LastSeen = time.Now()
		if nick != "" {
			rec.Nick = nick
		}
		return
	}

	ts.peers[id] = &PeerRecord{
		PeerID:       id,
		SafetyNumber: network.SafetyNumber(id),
		Status:       StatusUnverified,
		Nick:         nick,
		FirstSeen:    time.Now(),
		LastSeen:     time.Now(),
	}
	log.Printf("[trust] New peer recorded: %s (Safety Number: %s) [UNVERIFIED]",
		id.String()[:16], network.SafetyNumber(id))
}

// Verify marks a peer as verified. This should be called after the user
// has compared Safety Numbers out-of-band (e.g. in person or via QR code).
func (ts *TrustStore) Verify(id peer.ID) error {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	rec, ok := ts.peers[id]
	if !ok {
		return fmt.Errorf("peer %s not found in trust store", id)
	}

	rec.Status = StatusVerified
	log.Printf("[trust] Peer VERIFIED: %s (Safety Number: %s)", id.String()[:16], rec.SafetyNumber)
	return nil
}

// Block marks a peer as blocked. Messages from blocked peers should be dropped.
func (ts *TrustStore) Block(id peer.ID) error {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	rec, ok := ts.peers[id]
	if !ok {
		return fmt.Errorf("peer %s not found in trust store", id)
	}

	rec.Status = StatusBlocked
	log.Printf("[trust] Peer BLOCKED: %s", id.String()[:16])
	return nil
}

// IsVerified returns true if the peer is in the trust store and verified.
func (ts *TrustStore) IsVerified(id peer.ID) bool {
	ts.mu.RLock()
	defer ts.mu.RUnlock()

	rec, ok := ts.peers[id]
	return ok && rec.Status == StatusVerified
}

// IsBlocked returns true if the peer is explicitly blocked.
func (ts *TrustStore) IsBlocked(id peer.ID) bool {
	ts.mu.RLock()
	defer ts.mu.RUnlock()

	rec, ok := ts.peers[id]
	return ok && rec.Status == StatusBlocked
}

// GetStatus returns the trust status and safety number for a peer.
func (ts *TrustStore) GetStatus(id peer.ID) (TrustStatus, string, bool) {
	ts.mu.RLock()
	defer ts.mu.RUnlock()

	rec, ok := ts.peers[id]
	if !ok {
		return "", "", false
	}
	return rec.Status, rec.SafetyNumber, true
}

// ListAll returns a snapshot of all known peer records.
func (ts *TrustStore) ListAll() []PeerRecord {
	ts.mu.RLock()
	defer ts.mu.RUnlock()

	result := make([]PeerRecord, 0, len(ts.peers))
	for _, rec := range ts.peers {
		result = append(result, *rec)
	}
	return result
}
