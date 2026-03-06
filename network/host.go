// Package network provides core P2P networking primitives for the NEC
// (Novatorskaya Entryways Connections) mesh messenger. It wraps go-libp2p to
// manage host creation, peer discovery, and cryptographic identity.
package network

import (
	"context"
	"fmt"
	"log"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
)

// Node is the central P2P entity. It holds the libp2p host, the private key
// used for identity, and a context for lifecycle management.
type Node struct {
	Host   host.Host
	privK  crypto.PrivKey
	ctx    context.Context
	cancel context.CancelFunc
}

// NewNode creates and starts a libp2p host.
//
// The host is configured with:
//   - Ed25519 identity key (generated fresh on every start).
//   - QUIC transport (provides TLS 1.3 encryption and multiplexed streams,
//     solving the Head-of-Line blocking problem).
//   - Listen address on the given port (0 = random available port).
func NewNode(listenPort int) (*Node, error) {
	ctx, cancel := context.WithCancel(context.Background())

	// Generate a fresh Ed25519 key pair.
	priv, _, err := crypto.GenerateEd25519Key(nil)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("generate ed25519 key: %w", err)
	}

	// Build multiaddr for QUIC listener.
	// We listen on all interfaces (0.0.0.0) over UDP with QUIC-v1.
	listenAddr, err := multiaddr.NewMultiaddr(
		fmt.Sprintf("/ip4/0.0.0.0/udp/%d/quic-v1", listenPort),
	)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("parse multiaddr: %w", err)
	}

	// Create the libp2p host. QUIC transport is included by default when we
	// specify a QUIC listen address and use DefaultTransports.
	h, err := libp2p.New(
		libp2p.Identity(priv),
		libp2p.ListenAddrs(listenAddr),
	)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("create libp2p host: %w", err)
	}

	node := &Node{
		Host:   h,
		privK:  priv,
		ctx:    ctx,
		cancel: cancel,
	}

	log.Printf("[network] Host started. PeerID: %s", h.ID())
	for _, addr := range h.Addrs() {
		log.Printf("[network] Listening on: %s/p2p/%s", addr, h.ID())
	}

	return node, nil
}

// PeerID returns the libp2p PeerID derived from the node's public key.
func (n *Node) PeerID() peer.ID {
	return n.Host.ID()
}

// Context returns the node's lifecycle context.
func (n *Node) Context() context.Context {
	return n.ctx
}

// Close gracefully shuts down the libp2p host and cancels the context.
func (n *Node) Close() error {
	n.cancel()
	if err := n.Host.Close(); err != nil {
		return fmt.Errorf("close host: %w", err)
	}
	log.Println("[network] Host stopped.")
	return nil
}
