package filetransfer

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
)

// DefaultDownloadDir is the directory where received files are saved.
const DefaultDownloadDir = "received_files"

// RegisterHandler sets up the stream handler for incoming file transfers.
// When a remote peer opens a /nec/files/1.0.0 stream, this handler receives
// the file, verifies chunk integrity, and saves it to disk.
func RegisterHandler(h host.Host, downloadDir string) {
	if downloadDir == "" {
		downloadDir = DefaultDownloadDir
	}

	// Ensure download directory exists.
	if err := os.MkdirAll(downloadDir, 0o755); err != nil {
		log.Fatalf("[file-recv] Cannot create download dir %q: %v", downloadDir, err)
	}

	h.SetStreamHandler(ProtocolID, func(s network.Stream) {
		defer s.Close()
		remotePeer := s.Conn().RemotePeer().String()[:16]
		log.Printf("[file-recv] Incoming transfer from %s", remotePeer)

		if err := handleIncomingTransfer(s, downloadDir); err != nil {
			log.Printf("[file-recv] Transfer failed from %s: %v", remotePeer, err)
			// Try to send error result back.
			_ = writeJSON(s, &TransferResult{Success: false, Message: err.Error()})
		}
	})

	log.Printf("[file-recv] Handler registered (protocol=%s, dir=%q)", ProtocolID, downloadDir)
}

// handleIncomingTransfer processes a single incoming file transfer stream.
func handleIncomingTransfer(s network.Stream, downloadDir string) error {
	// ── Step 1: Read file header ────────────────────────────────────────
	var header FileHeader
	if err := readJSON(s, &header); err != nil {
		return fmt.Errorf("read header: %w", err)
	}
	log.Printf("[file-recv] Offered: %q (%d bytes, %d chunks, hash=%s)",
		header.FileName, header.FileSize, header.TotalChunks, header.FileHash[:8])

	// Sanitize filename to prevent path traversal.
	safeName := filepath.Base(header.FileName)
	destPath := filepath.Join(downloadDir, safeName)

	// ── Step 2: Determine resume offset ─────────────────────────────────
	var offset int64
	if existingInfo, err := os.Stat(destPath + ".part"); err == nil {
		// Partial file exists — resume from where we left off.
		offset = existingInfo.Size()
		log.Printf("[file-recv] Resuming from offset %d (partial file exists)", offset)
	}

	// Send resume response (always accept).
	resume := ResumeRequest{
		Accepted: true,
		Offset:   offset,
	}
	if err := writeJSON(s, &resume); err != nil {
		return fmt.Errorf("send resume: %w", err)
	}

	// ── Step 3: Open file for writing ───────────────────────────────────
	partPath := destPath + ".part"
	flags := os.O_CREATE | os.O_WRONLY
	if offset > 0 {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}

	f, err := os.OpenFile(partPath, flags, 0o644)
	if err != nil {
		return fmt.Errorf("open partial file: %w", err)
	}
	defer f.Close()

	// ── Step 4: Receive chunks ──────────────────────────────────────────
	received := offset
	for received < header.FileSize {
		// Read chunk header.
		var ch ChunkHeader
		if err := readJSON(s, &ch); err != nil {
			return fmt.Errorf("read chunk header: %w", err)
		}

		// Read raw chunk data.
		chunkData := make([]byte, ch.Size)
		if _, err := io.ReadFull(s, chunkData); err != nil {
			return fmt.Errorf("read chunk %d data: %w", ch.Index, err)
		}

		// Verify chunk hash.
		actualHash := HashBytes(chunkData)
		if actualHash != ch.Hash {
			return fmt.Errorf("chunk %d hash mismatch: expected %s, got %s",
				ch.Index, ch.Hash[:8], actualHash[:8])
		}

		// Write chunk to disk.
		if _, err := f.Write(chunkData); err != nil {
			return fmt.Errorf("write chunk %d: %w", ch.Index, err)
		}

		received += int64(ch.Size)
		pct := float64(received) / float64(header.FileSize) * 100
		log.Printf("[file-recv] Chunk %d/%d received (%.1f%%) ✓",
			ch.Index+1, header.TotalChunks, pct)
	}

	// Flush to disk.
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync file: %w", err)
	}
	f.Close()

	// ── Step 5: Verify whole-file hash ──────────────────────────────────
	finalFile, err := os.Open(partPath)
	if err != nil {
		return fmt.Errorf("open for hash check: %w", err)
	}
	finalHash, err := HashFile(finalFile)
	finalFile.Close()
	if err != nil {
		return fmt.Errorf("hash final file: %w", err)
	}

	if finalHash != header.FileHash {
		return fmt.Errorf("file hash mismatch: expected %s, got %s",
			header.FileHash[:8], finalHash[:8])
	}

	// Rename .part to final name.
	if err := os.Rename(partPath, destPath); err != nil {
		return fmt.Errorf("rename partial file: %w", err)
	}

	log.Printf("[file-recv] ✅ File saved: %s (%d bytes, hash verified)", destPath, header.FileSize)

	// ── Step 6: Send success result ─────────────────────────────────────
	result := TransferResult{
		Success: true,
		Message: fmt.Sprintf("File %q received and verified (%d bytes)", safeName, header.FileSize),
	}
	if err := writeJSON(s, &result); err != nil {
		return fmt.Errorf("send result: %w", err)
	}

	return nil
}
