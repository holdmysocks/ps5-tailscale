package main

import (
	"io"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"time"
)

// forwardToLocalhost is tsnet's fallback TCP handler: it is asked about every
// tailnet connection to a port nothing in this process listens on. If a
// service on the console listens on that port, the connection is accepted and
// piped to it. That is what makes FTP, the payload loader and the like
// reachable over the tailnet.
//
// The local connection is made before answering, while the tailnet peer is
// still waiting for its SYN-ACK. If nothing listens on the port the
// connection is declined, so the peer sees an ordinary "connection refused"
// rather than a connection that opens and closes.
func (d *daemon) forwardToLocalhost(src, dst netip.AddrPort) (handler func(net.Conn), intercept bool) {
	ip4, ip6 := d.srv.TailscaleIPs()
	if !addressedTo(dst.Addr(), ip4, ip6) {
		// Only connections to the console's own tailnet addresses are for
		// its services. Nothing else arrives today, but if this node ever
		// advertised routes, a connection to any address on a port that is
		// open here must not end up at the console's service.
		return nil, false
	}
	port := dst.Port()
	if port == d.webPort {
		// The status page is served on the tailnet connection itself rather
		// than through localhost, so that the page sees who is asking.
		return d.tailnetWeb.deliver, true
	}
	d.mu.Lock()
	blocked := slices.Contains(d.cfg.BlockedPorts, port) || port == d.proxyPort
	d.mu.Unlock()
	if blocked || d.fwd.listensOnTCP(port) {
		// The outbound proxy and the local forwards are for the console's
		// own apps. Exposing them would let any tailnet device use the
		// console as a relay.
		return nil, false
	}
	local, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))), 2*time.Second)
	if err != nil {
		return nil, false
	}
	// If the tailnet side never completes its handshake the handler is not
	// called; do not keep the local connection open for it forever.
	abandoned := time.AfterFunc(30*time.Second, func() { local.Close() })
	return func(c net.Conn) {
		defer c.Close()
		defer local.Close()
		if !abandoned.Stop() {
			return
		}
		d.logf("forward %v -> localhost:%d", src, port)
		pipe(c, local)
	}, true
}

// addressedTo reports whether dst is one of the node's own addresses.
func addressedTo(dst netip.Addr, own ...netip.Addr) bool {
	dst = dst.Unmap()
	return dst.IsValid() && slices.Contains(own, dst)
}

// pipe copies in both directions until both sides are done.
func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		io.Copy(dst, src)
		// Pass the end of the stream on, so that protocols relying on a
		// half-close (such as sending a payload to the ELF loader) work.
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else {
			dst.Close()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
}
