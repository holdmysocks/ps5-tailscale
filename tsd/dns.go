package main

import (
	"context"
	"encoding/binary"
	"net"
	"slices"
	"sync"
	"time"

	"tailscale.com/net/netmon"
)

// Name lookups by the daemon itself: Tailscale's servers, GitHub for the
// update check, whatever goes through the HTTP proxy.
//
// The PS5 has no resolv.conf, so Go asks 127.0.0.1:53. That only works on a
// console that runs a DNS payload, which most jailbreak setups do and some do
// not. Rather than depend on it, the daemon uses the first of these that
// answers: the local DNS payload, the router, a public resolver.

// dnsLocal is where a DNS payload on the console listens. A variable so that
// a test build can pretend there is none.
var dnsLocal = "127.0.0.1:53"

// dnsPublic are used when neither the console nor the router answers.
var dnsPublic = []string{"1.1.1.1:53", "8.8.8.8:53", "9.9.9.9:53"}

const (
	dnsRecheckGood = 5 * time.Minute  // how long a working server is kept
	dnsRecheckBad  = 20 * time.Second // how soon to look again when none works
	dnsProbeWait   = 1200 * time.Millisecond
)

type dnsPicker struct {
	logf func(format string, args ...any)
	// candidates lists the servers to try, in order of preference.
	candidates func() []string
	// probe reports whether a server answers queries.
	probe func(ctx context.Context, server string) bool

	mu      sync.Mutex
	server  string
	working bool
	checked time.Time
}

func newDNSPicker(logf func(format string, args ...any)) *dnsPicker {
	return &dnsPicker{logf: logf, candidates: dnsCandidates, probe: dnsAnswers}
}

// dnsCandidates returns the local DNS payload, the router and the public
// resolvers, in that order.
func dnsCandidates() []string {
	list := []string{dnsLocal}
	if gw, _, ok := netmon.LikelyHomeRouterIP(); ok && gw.IsValid() {
		list = append(list, net.JoinHostPort(gw.String(), "53"))
	}
	return append(list, dnsPublic...)
}

// pick returns the server to ask. The choice is kept for a while, so the
// candidates are not probed for every lookup.
func (p *dnsPicker) pick(ctx context.Context) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	keep := dnsRecheckBad
	if p.working {
		keep = dnsRecheckGood
	}
	if p.server != "" && time.Since(p.checked) < keep {
		return p.server
	}
	candidates := p.candidates()
	chosen, working := candidates[0], false
	for _, c := range candidates {
		if p.probe(ctx, c) {
			chosen, working = c, true
			break
		}
	}
	if chosen != p.server || working != p.working {
		switch {
		case !working:
			p.logf("DNS: no server answers (tried %v); name lookups will fail until one does", candidates)
		case chosen == candidates[0]:
			p.logf("DNS: using the console's own DNS at %s", chosen)
		default:
			p.logf("DNS: nothing answers at %s; using %s instead (in order of preference: %v)", candidates[0], chosen, candidates)
		}
	}
	p.server, p.working, p.checked = chosen, working, time.Now()
	return chosen
}

// reset makes the next lookup choose again, for when the network changed.
func (p *dnsPicker) reset() {
	p.mu.Lock()
	p.checked = time.Time{}
	p.mu.Unlock()
}

// dial is the resolver's Dial: whatever address Go wants to ask, the chosen
// server is asked instead.
func (p *dnsPicker) dial(ctx context.Context, network, _ string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, p.pick(ctx))
}

// install makes every name lookup in the process go through the picker.
func (p *dnsPicker) install() {
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: p.dial}
}

// dnsAnswers asks server for the address of a name that certainly exists
// and reports whether a proper answer came back.
func dnsAnswers(ctx context.Context, server string) bool {
	ctx, cancel := context.WithTimeout(ctx, dnsProbeWait)
	defer cancel()
	var d net.Dialer
	c, err := d.DialContext(ctx, "udp", server)
	if err != nil {
		return false
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(dnsProbeWait))
	query := dnsQuery(uint16(time.Now().UnixNano()), "github.com")
	if _, err := c.Write(query); err != nil {
		return false
	}
	buf := make([]byte, 1500)
	n, err := c.Read(buf)
	if err != nil {
		return false
	}
	return dnsReplyOK(query, buf[:n])
}

// dnsQuery builds a query for the IPv4 address of name.
func dnsQuery(id uint16, name string) []byte {
	q := make([]byte, 12, 64)
	binary.BigEndian.PutUint16(q[0:], id)
	q[2] = 0x01 // recursion desired
	binary.BigEndian.PutUint16(q[4:], 1)
	start := 0
	for i := 0; i <= len(name); i++ {
		if i == len(name) || name[i] == '.' {
			q = append(q, byte(i-start))
			q = append(q, name[start:i]...)
			start = i + 1
		}
	}
	return append(q, 0, 0, 1, 0, 1) // root label, type A, class IN
}

// dnsReplyOK reports whether reply is a successful answer to query.
func dnsReplyOK(query, reply []byte) bool {
	if len(reply) < 12 || reply[0] != query[0] || reply[1] != query[1] {
		return false
	}
	isResponse := reply[2]&0x80 != 0
	rcode := reply[3] & 0x0f
	answers := binary.BigEndian.Uint16(reply[6:])
	return isResponse && rcode == 0 && answers > 0
}

// describe says in words which server is in use, for the status page. It
// does not probe.
func (p *dnsPicker) describe() string {
	p.mu.Lock()
	server, working := p.server, p.working
	p.mu.Unlock()
	if server == "" {
		return ""
	}
	host, _, err := net.SplitHostPort(server)
	if err != nil {
		host = server
	}
	if !working {
		return "no server answers"
	}
	switch {
	case server == dnsLocal:
		return host + " (DNS payload on this console)"
	case slices.Contains(dnsPublic, server):
		return host + " (public resolver)"
	default:
		return host + " (router)"
	}
}
