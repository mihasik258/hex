// NEC (Novatorskaya Entryways Connections) Mesh Messenger — entry point.
//
// This binary starts a P2P node with Ed25519 identity, QUIC transport,
// mDNS discovery, and GossipSub messaging. It provides an interactive CLI
// for chatting, verifying peers, and managing trust.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"

	"nec/messaging"
	"nec/network"
)

func main() {
	port := flag.Int("port", 0, "Listen port for QUIC transport (0 = random)")
	nick := flag.String("nick", "", "Nickname for chat (default: first 8 chars of PeerID)")
	flag.Parse()

	log.SetFlags(log.Ltime | log.Lshortfile)

	// ── Start the P2P node ──────────────────────────────────────────────
	node, err := network.NewNode(*port)
	if err != nil {
		log.Fatalf("Failed to create node: %v", err)
	}
	defer node.Close()

	// Default nick to short PeerID if not provided.
	if *nick == "" {
		*nick = node.PeerID().String()[:8]
	}

	// ── Print identity information ──────────────────────────────────────
	fmt.Println("═══════════════════════════════════════════════════")
	fmt.Println("  NEC — Novatorskaya Entryways Connections")
	fmt.Println("═══════════════════════════════════════════════════")
	fmt.Printf("  PeerID:        %s\n", node.PeerID())
	fmt.Printf("  Safety Number: %s\n", network.SafetyNumber(node.PeerID()))
	fmt.Printf("  Nickname:      %s\n", *nick)
	fmt.Println("───────────────────────────────────────────────────")
	for _, addr := range node.Host.Addrs() {
		fmt.Printf("  Listen: %s/p2p/%s\n", addr, node.PeerID())
	}
	fmt.Println("═══════════════════════════════════════════════════")

	// ── Start mDNS discovery ────────────────────────────────────────────
	mdnsSvc := network.SetupMDNS(node)
	defer mdnsSvc.Close()

	// ── Initialize messaging subsystem ──────────────────────────────────
	trustStore := messaging.NewTrustStore()
	msgStore := messaging.NewMessageStore(24 * time.Hour)

	chatRoom, err := messaging.JoinChat(node.Context(), node.Host, *nick)
	if err != nil {
		log.Fatalf("Failed to join chat: %v", err)
	}
	defer chatRoom.Close()

	// ── Start store cleanup ticker ──────────────────────────────────────
	cleanupTicker := time.NewTicker(5 * time.Minute)
	defer cleanupTicker.Stop()
	go func() {
		for range cleanupTicker.C {
			msgStore.Cleanup()
		}
	}()

	// ── Print help ──────────────────────────────────────────────────────
	printHelp()

	// ── Handle incoming messages ────────────────────────────────────────
	go handleIncoming(chatRoom, trustStore, msgStore, node.PeerID())

	// ── Handle graceful shutdown ────────────────────────────────────────
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// ── Interactive CLI loop ────────────────────────────────────────────
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			handleCommand(line, chatRoom, trustStore, msgStore)
		}
	}()

	// Block until signal.
	sig := <-sigCh
	fmt.Printf("\nReceived %s, shutting down...\n", sig)
}

// handleCommand processes CLI commands and chat messages.
func handleCommand(line string, cr *messaging.ChatRoom, ts *messaging.TrustStore, ms *messaging.MessageStore) {
	switch {
	case line == "/help":
		printHelp()

	case line == "/peers":
		peers := cr.ListPeers()
		if len(peers) == 0 {
			fmt.Println("  No peers connected.")
			return
		}
		fmt.Printf("  Connected peers (%d):\n", len(peers))
		for _, p := range peers {
			status, sn, known := ts.GetStatus(p)
			if !known {
				fmt.Printf("    • %s [unknown]\n", p.String()[:16])
			} else {
				fmt.Printf("    • %s  SN: %s  [%s]\n", p.String()[:16], sn, status)
			}
		}

	case strings.HasPrefix(line, "/verify "):
		peerStr := strings.TrimPrefix(line, "/verify ")
		pid, err := peer.Decode(peerStr)
		if err != nil {
			fmt.Printf("  Invalid PeerID: %v\n", err)
			return
		}
		if err := ts.Verify(pid); err != nil {
			fmt.Printf("  Error: %v\n", err)
		} else {
			fmt.Printf("  ✅ Peer %s marked as VERIFIED.\n", peerStr[:16])
		}

	case strings.HasPrefix(line, "/block "):
		peerStr := strings.TrimPrefix(line, "/block ")
		pid, err := peer.Decode(peerStr)
		if err != nil {
			fmt.Printf("  Invalid PeerID: %v\n", err)
			return
		}
		if err := ts.Block(pid); err != nil {
			fmt.Printf("  Error: %v\n", err)
		} else {
			fmt.Printf("  🚫 Peer %s BLOCKED.\n", peerStr[:16])
		}

	case line == "/trust":
		records := ts.ListAll()
		if len(records) == 0 {
			fmt.Println("  No known peers.")
			return
		}
		fmt.Printf("  Trust store (%d peer(s)):\n", len(records))
		for _, r := range records {
			fmt.Printf("    • %s  SN: %s  [%s]  nick=%q  last_seen=%s\n",
				r.PeerID.String()[:16], r.SafetyNumber, r.Status,
				r.Nick, r.LastSeen.Format("15:04:05"))
		}

	case line == "/store":
		fmt.Printf("  Store-and-forward: %d pending message(s)\n", ms.TotalPending())

	case strings.HasPrefix(line, "/"):
		fmt.Printf("  Unknown command: %s (type /help)\n", line)

	default:
		// Regular chat message.
		if err := cr.Publish(line); err != nil {
			fmt.Printf("  Error sending: %v\n", err)
		}
	}
}

// handleIncoming reads messages from the chat room and displays them.
// It also records peers in the trust store and sends ACKs.
func handleIncoming(cr *messaging.ChatRoom, ts *messaging.TrustStore, ms *messaging.MessageStore, self peer.ID) {
	for msg := range cr.Messages {
		senderID, err := peer.Decode(msg.Sender)
		if err != nil {
			log.Printf("[chat] Cannot decode sender PeerID: %v", err)
			continue
		}

		// Drop messages from blocked peers.
		if ts.IsBlocked(senderID) {
			continue
		}

		// Record the peer in trust store (TOFU: first use = unverified).
		ts.RecordPeer(senderID, msg.SenderNick)

		// Build trust indicator for display.
		trustTag := "⚠ UNVERIFIED"
		if ts.IsVerified(senderID) {
			trustTag = "✅ verified"
		}

		switch msg.Type {
		case messaging.TypeChat:
			displayNick := msg.SenderNick
			if displayNick == "" {
				displayNick = msg.Sender[:8]
			}
			ts := time.UnixMilli(msg.Timestamp).Format("15:04:05")
			fmt.Printf("\n  [%s] <%s> [%s]: %s\n", ts, displayNick, trustTag, msg.Payload)

			// Send ACK back.
			if err := cr.PublishAck(msg.ID); err != nil {
				log.Printf("[chat] Failed to send ACK: %v", err)
			}

		case messaging.TypeAck:
			log.Printf("[chat] ACK received from %s for message %s",
				msg.Sender[:8], msg.Payload[:8])

		case messaging.TypeStatus:
			log.Printf("[chat] Status from %s: %s", msg.Sender[:8], msg.Payload)
		}
	}
}

// printHelp displays available CLI commands.
func printHelp() {
	fmt.Println("───────────────────────────────────────────────────")
	fmt.Println("  Commands:")
	fmt.Println("    /peers           — list connected peers")
	fmt.Println("    /trust           — show trust store")
	fmt.Println("    /verify <PeerID> — mark peer as verified (TOFU)")
	fmt.Println("    /block <PeerID>  — block a peer")
	fmt.Println("    /store           — show pending offline messages")
	fmt.Println("    /help            — show this help")
	fmt.Println("    <text>           — send a chat message")
	fmt.Println("───────────────────────────────────────────────────")
}
