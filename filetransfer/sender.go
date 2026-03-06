package filetransfer

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
)

// RateLimitDelay is the artificial delay per chunk write to simulate a
// rate limiter and avoid saturating the network for other traffic
// (messages, calls).
const RateLimitDelay = 5 * time.Millisecond

// SendFile initiates a file transfer to the specified peer. It opens a new
// QUIC stream, sends the file header, negotiates resume offset, and transmits
// chunks with SHA-256 verification and rate limiting.
func SendFile(ctx context.Context, h host.Host, target peer.ID, filePath string) error {
	// Open and stat the file.
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open file: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat file: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("cannot send a directory")
	}

	// Compute whole-file hash.
	fileHash, err := HashFile(f)
	if err != nil {
		return err
	}
	// Seek back to start after hashing.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek: %w", err)
	}

	totalChunks := int(info.Size()/ChunkSize) + 1
	if info.Size()%ChunkSize == 0 && info.Size() > 0 {
		totalChunks = int(info.Size() / ChunkSize)
	}

	// Open a stream to the peer.
	s, err := h.NewStream(ctx, target, ProtocolID)
	if err != nil {
		return fmt.Errorf("open stream to %s: %w", target.String()[:16], err)
	}
	defer s.Close()

	// ── Step 1: Send file header ────────────────────────────────────────
	header := FileHeader{
		FileName:    filepath.Base(filePath),
		FileSize:    info.Size(),
		FileHash:    fileHash,
		TotalChunks: totalChunks,
	}
	if err := writeJSON(s, &header); err != nil {
		return fmt.Errorf("send header: %w", err)
	}
	log.Printf("[file-send] Offered %q (%d bytes, %d chunks, hash=%s)",
		header.FileName, header.FileSize, header.TotalChunks, header.FileHash[:8])

	// ── Step 2: Read resume response ────────────────────────────────────
	var resume ResumeRequest
	if err := readJSON(s, &resume); err != nil {
		return fmt.Errorf("read resume response: %w", err)
	}
	if !resume.Accepted {
		return fmt.Errorf("peer rejected file transfer")
	}

	// Seek to the requested offset for resume.
	if resume.Offset > 0 {
		if _, err := f.Seek(resume.Offset, io.SeekStart); err != nil {
			return fmt.Errorf("seek to offset %d: %w", resume.Offset, err)
		}
		log.Printf("[file-send] Resuming from offset %d", resume.Offset)
	}

	// ── Step 3: Send chunks ─────────────────────────────────────────────
	buf := make([]byte, ChunkSize)
	offset := resume.Offset
	chunkIdx := int(offset / ChunkSize)

	for {
		n, err := f.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			chunkHash := HashBytes(chunk)

			// Send chunk header.
			ch := ChunkHeader{
				Index:  chunkIdx,
				Offset: offset,
				Size:   n,
				Hash:   chunkHash,
			}
			if err := writeJSON(s, &ch); err != nil {
				return fmt.Errorf("send chunk header %d: %w", chunkIdx, err)
			}

			// Send raw chunk data.
			if _, err := s.Write(chunk); err != nil {
				return fmt.Errorf("send chunk data %d: %w", chunkIdx, err)
			}

			offset += int64(n)
			chunkIdx++

			pct := float64(offset) / float64(info.Size()) * 100
			log.Printf("[file-send] Chunk %d/%d sent (%.1f%%)", chunkIdx, totalChunks, pct)

			// Rate limiting: brief pause between chunks to avoid saturating
			// the link for messages and calls.
			time.Sleep(RateLimitDelay)
		}

		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read file: %w", err)
		}
	}

	// ── Step 4: Read transfer result ────────────────────────────────────
	var result TransferResult
	if err := readJSON(s, &result); err != nil {
		return fmt.Errorf("read transfer result: %w", err)
	}
	if !result.Success {
		return fmt.Errorf("transfer failed: %s", result.Message)
	}

	log.Printf("[file-send] Transfer complete: %s", result.Message)
	return nil
}
