package main

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"sync"
)

// Inbound UDP: tailnet devices reaching UDP services on the console.
//
// TCP needs no configuration, because tsnet asks about every incoming
// connection and it can be passed to localhost on the spot. UDP has no such
// hook, so the ports have to be listed and listened on, on the console's
// tailnet addresses. The default list is what PS5 Remote Play uses.

// remotePlayUDPPorts are the UDP ports of the console's Remote Play service:
// registration (9295), the stream (9296), the connection test (9297) and
// discovery (9302). Its session port, TCP 9295, is covered by the TCP
// forwarding.
var remotePlayUDPPorts = []uint16{9295, 9296, 9297, 9302}

// udpExposer keeps a set of the console's UDP ports reachable on its tailnet
// addresses.
type udpExposer struct {
	// listen opens a UDP socket on a tailnet address
	// (tsnet.Server.ListenPacket).
	listen func(network, addr string) (net.PacketConn, error)
	logf   func(format string, args ...any)
	// targetHost is where the console's services are reached.
	targetHost string

	mu     sync.Mutex
	addrs  []netip.Addr
	ports  []uint16
	relays []*udpRelay
	active []uint16
}

// update makes ports reachable on addrs, replacing whatever was exposed
// before. It does nothing if neither has changed and every relay is still
// running.
func (e *udpExposer) update(addrs []netip.Addr, ports []uint16) {
	e.mu.Lock()
	defer e.mu.Unlock()
	healthy := !slices.ContainsFunc(e.relays, func(r *udpRelay) bool { return !r.running() })
	if healthy && slices.Equal(addrs, e.addrs) && slices.Equal(ports, e.ports) {
		return
	}
	for _, r := range e.relays {
		r.stop()
	}
	e.relays, e.active = nil, nil
	e.addrs, e.ports = slices.Clone(addrs), slices.Clone(ports)

	for _, port := range ports {
		target := net.JoinHostPort(e.targetHost, strconv.Itoa(int(port)))
		ok := false
		for _, addr := range addrs {
			network := "udp4"
			if addr.Is6() {
				network = "udp6"
			}
			listenAddr := netip.AddrPortFrom(addr, port).String()
			relay, err := startUDPRelay(udpRelayConfig{
				name:   "udp " + listenAddr,
				listen: func() (net.PacketConn, error) { return e.listen(network, listenAddr) },
				dial: func(ctx context.Context) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "udp", target)
				},
				logf: e.logf,
			})
			if err != nil {
				e.logf("udp %s: %v", listenAddr, err)
				continue
			}
			e.relays = append(e.relays, relay)
			ok = true
		}
		if ok {
			e.active = append(e.active, port)
		}
	}
	if len(e.active) > 0 {
		e.logf("UDP ports reachable from the tailnet: %v", e.active)
	}
}

// activePorts returns the ports currently exposed.
func (e *udpExposer) activePorts() []uint16 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.active)
}
