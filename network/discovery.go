package network

import (
	"log"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns"
)

// ServiceTag is the mDNS service identifier. All AroundFM nodes advertise and
// discover each other using this tag on the local network.
const ServiceTag = "aroundfm.mesh.local"

// discoveryNotifee is notified by mDNS when new peers appear on the LAN.
// It automatically attempts to connect to every discovered peer.
type discoveryNotifee struct {
	node *Node
}

// HandlePeerFound is called by the mDNS service each time a new peer is
// discovered on the local network. We log the event and initiate a connection.
func (n *discoveryNotifee) HandlePeerFound(pi peer.AddrInfo) {
	// Ignore self-discovery.
	if pi.ID == n.node.Host.ID() {
		return
	}

	log.Printf("[discovery] Found peer: %s", pi.ID)
	log.Printf("[discovery]   Safety Number: %s", SafetyNumber(pi.ID))

	if err := n.node.Host.Connect(n.node.ctx, pi); err != nil {
		log.Printf("[discovery] Failed to connect to %s: %v", pi.ID, err)
	} else {
		log.Printf("[discovery] Connected to peer: %s", pi.ID)
	}
}

// SetupMDNS initializes mDNS-based peer discovery on the local network.
// It broadcasts the node's presence and listens for other AroundFM nodes.
// The returned mdns.Service can be used to close the discovery later.
func SetupMDNS(node *Node) mdns.Service {
	notifee := &discoveryNotifee{node: node}
	svc := mdns.NewMdnsService(node.Host, ServiceTag, notifee)

	if err := svc.Start(); err != nil {
		log.Fatalf("[discovery] Failed to start mDNS: %v", err)
	}

	log.Printf("[discovery] mDNS service started (tag=%q)", ServiceTag)
	return svc
}
