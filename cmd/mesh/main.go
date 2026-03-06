// AroundFM Mesh Messenger — entry point.
//
// This binary starts a P2P node with Ed25519 identity, QUIC transport,
// and mDNS discovery. It prints the node's identity information including
// the Safety Number for TOFU verification, then blocks until interrupted.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"aroundfm/network"
)

func main() {
	port := flag.Int("port", 0, "Listen port for QUIC transport (0 = random)")
	flag.Parse()

	log.SetFlags(log.Ltime | log.Lshortfile)

	// ── Start the P2P node ──────────────────────────────────────────────
	node, err := network.NewNode(*port)
	if err != nil {
		log.Fatalf("Failed to create node: %v", err)
	}
	defer node.Close()

	// ── Print identity information ──────────────────────────────────────
	fmt.Println("═══════════════════════════════════════════════════")
	fmt.Println("  AroundFM Mesh Messenger")
	fmt.Println("═══════════════════════════════════════════════════")
	fmt.Printf("  PeerID:        %s\n", node.PeerID())
	fmt.Printf("  Safety Number: %s\n", network.SafetyNumber(node.PeerID()))
	fmt.Println("───────────────────────────────────────────────────")
	for _, addr := range node.Host.Addrs() {
		fmt.Printf("  Listen: %s/p2p/%s\n", addr, node.PeerID())
	}
	fmt.Println("═══════════════════════════════════════════════════")
	fmt.Println("  Waiting for peers on the local network (mDNS)...")
	fmt.Println()

	// ── Start mDNS discovery ────────────────────────────────────────────
	mdnsSvc := network.SetupMDNS(node)
	defer mdnsSvc.Close()

	// ── Block until SIGINT / SIGTERM ────────────────────────────────────
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	fmt.Printf("\nReceived %s, shutting down...\n", sig)
}
