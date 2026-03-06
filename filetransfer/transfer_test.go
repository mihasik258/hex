package filetransfer_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/peer"

	"nec/filetransfer"
)

// TestFileTransfer verifies end-to-end chunked file transfer between two
// libp2p hosts: file hashing, chunk splitting, transmission, per-chunk
// SHA-256 verification, whole-file hash check, and final rename from .part.
func TestFileTransfer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// ── Create two libp2p hosts ─────────────────────────────────────────
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
		t.Fatalf("connect A->B: %v", err)
	}

	// ── Prepare test file ───────────────────────────────────────────────
	tmpDir := t.TempDir()
	srcPath := filepath.Join(tmpDir, "testfile.bin")
	downloadDir := filepath.Join(tmpDir, "downloads")

	// Create a test file larger than one chunk (256KB) to test multi-chunk.
	fileData := make([]byte, 300*1024) // 300 KB → 2 chunks
	for i := range fileData {
		fileData[i] = byte(i % 251) // Deterministic non-zero pattern.
	}
	if err := os.WriteFile(srcPath, fileData, 0o644); err != nil {
		t.Fatalf("write test file: %v", err)
	}

	// ── Register receiver on host B ─────────────────────────────────────
	filetransfer.RegisterHandler(hostB, downloadDir)

	// ── Send file from A to B ───────────────────────────────────────────
	t.Log("Sending file from A to B...")
	if err := filetransfer.SendFile(ctx, hostA, hostB.ID(), srcPath); err != nil {
		t.Fatalf("SendFile: %v", err)
	}

	// ── Verify received file ────────────────────────────────────────────
	receivedPath := filepath.Join(downloadDir, "testfile.bin")
	receivedData, err := os.ReadFile(receivedPath)
	if err != nil {
		t.Fatalf("read received file: %v", err)
	}

	if len(receivedData) != len(fileData) {
		t.Fatalf("size mismatch: got %d, want %d", len(receivedData), len(fileData))
	}

	for i := range fileData {
		if receivedData[i] != fileData[i] {
			t.Fatalf("data mismatch at byte %d: got %d, want %d", i, receivedData[i], fileData[i])
		}
	}

	// Verify .part file is gone.
	if _, err := os.Stat(receivedPath + ".part"); !os.IsNotExist(err) {
		t.Fatalf(".part file should not exist after successful transfer")
	}

	t.Log("✅ File transfer verified: size, content, and hash all match!")
}

// TestFileTransferSmallFile tests sending a file smaller than one chunk.
func TestFileTransferSmallFile(t *testing.T) {
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

	tmpDir := t.TempDir()
	srcPath := filepath.Join(tmpDir, "small.txt")
	downloadDir := filepath.Join(tmpDir, "downloads")

	content := []byte("Hello, NEC Mesh Messenger! This is a small file test.")
	if err := os.WriteFile(srcPath, content, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	filetransfer.RegisterHandler(hostB, downloadDir)

	if err := filetransfer.SendFile(ctx, hostA, hostB.ID(), srcPath); err != nil {
		t.Fatalf("SendFile: %v", err)
	}

	received, err := os.ReadFile(filepath.Join(downloadDir, "small.txt"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if string(received) != string(content) {
		t.Fatalf("content mismatch: got %q", string(received))
	}

	t.Log("✅ Small file transfer verified!")
}
