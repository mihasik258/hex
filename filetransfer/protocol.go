// Package filetransfer implements chunked file transfer over libp2p QUIC
// streams for NEC. Files are split into 256KB chunks, each verified with
// SHA-256. Resume is supported via byte-offset negotiation.
package filetransfer

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
)

// ProtocolID is the libp2p protocol identifier for file transfers.
const ProtocolID = "/nec/files/1.0.0"

// ChunkSize is 256 KB — the size of each file chunk.
const ChunkSize = 256 * 1024 // 256 KB

// FileHeader is sent by the sender at the start of a transfer to describe
// the file being offered. The receiver uses this to decide whether to accept
// and at which offset to resume.
type FileHeader struct {
	// FileName is the base name of the file.
	FileName string `json:"file_name"`
	// FileSize is the total size in bytes.
	FileSize int64 `json:"file_size"`
	// FileHash is the SHA-256 hex digest of the entire file.
	FileHash string `json:"file_hash"`
	// TotalChunks is the number of 256KB chunks.
	TotalChunks int `json:"total_chunks"`
}

// ResumeRequest is sent by the receiver to indicate from which byte offset
// the transfer should start. Offset=0 means start from the beginning.
type ResumeRequest struct {
	// Accepted indicates whether the receiver wants the file.
	Accepted bool `json:"accepted"`
	// Offset is the byte offset to resume from (0 = full transfer).
	Offset int64 `json:"offset"`
}

// ChunkHeader precedes each chunk's raw data on the wire.
type ChunkHeader struct {
	// Index is the 0-based chunk number.
	Index int `json:"index"`
	// Offset is the byte offset in the file.
	Offset int64 `json:"offset"`
	// Size is the actual number of bytes in this chunk (<=ChunkSize).
	Size int `json:"size"`
	// Hash is the SHA-256 hex digest of this chunk's data.
	Hash string `json:"hash"`
}

// TransferResult is sent by the receiver after all chunks are received.
type TransferResult struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// HashFile computes the SHA-256 hash of an entire file via streaming reads.
func HashFile(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", fmt.Errorf("hash file: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// HashBytes computes SHA-256 of a byte slice.
func HashBytes(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// writeJSON writes a length-prefixed JSON message to the stream.
// Format: [4 bytes big-endian length][JSON payload]
func writeJSON(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	// Write 4-byte length prefix.
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(data)))
	if _, err := w.Write(lenBuf); err != nil {
		return fmt.Errorf("write length: %w", err)
	}

	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("write payload: %w", err)
	}
	return nil
}

// readJSON reads a length-prefixed JSON message from the stream.
func readJSON(r io.Reader, v any) error {
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(r, lenBuf); err != nil {
		return fmt.Errorf("read length: %w", err)
	}
	n := binary.BigEndian.Uint32(lenBuf)

	// Sanity check: max 1MB for JSON headers.
	if n > 1<<20 {
		return fmt.Errorf("json payload too large: %d bytes", n)
	}

	data := make([]byte, n)
	if _, err := io.ReadFull(r, data); err != nil {
		return fmt.Errorf("read payload: %w", err)
	}

	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("unmarshal: %w", err)
	}
	return nil
}
