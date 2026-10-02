package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"
)

// Local forwards go the opposite way from forwardToLocalhost: they let apps
// on the console reach a host on the tailnet.
//
// Tailscale runs inside this process, not in the PS5's network stack, so
// other apps cannot open connections to tailnet addresses. A forward listens
// on a localhost port and relays to a tailnet host; the app connects to
// 127.0.0.1 instead. TCP and UDP are both supported, which is what game
// streaming (Moonlight to a Sunshine host) needs.

// forwardRule is one local forward.
type forwardRule struct {
	Proto  string `json:"proto"`  // "tcp" or "udp"
	Listen string `json:"listen"` // local address, e.g. "127.0.0.1:47989"
	Target string `json:"target"` // tailnet host and port, e.g. "my-pc:47989"
}

func (r forwardRule) String() string {
	return fmt.Sprintf("%s %s -> %s", r.Proto, r.Listen, r.Target)
}

// sunshineHost is a device on the tailnet that runs Sunshine.
type sunshineHost struct {
	Host string `json:"host"`
	// Port is Sunshine's "port" setting, which all its other ports are
	// derived from. 0 means the default, 47989.
	Port int `json:"port,omitempty"`
}

const sunshineDefaultPort = 47989

func (h sunshineHost) basePort() int {
	if h.Port == 0 {
		return sunshineDefaultPort
	}
	return h.Port
}

// Sunshine's ports as offsets from its "port" setting.
var (
	sunshineTCPOffsets = []int{-5, 0, 21}     // HTTPS, HTTP, RTSP
	sunshineUDPOffsets = []int{9, 10, 11, 13} // video, control, audio, microphone
)

// rules returns the forwards that make this Sunshine host appear on
// 127.0.0.1, on the same ports it really uses. The ports have to match: the
// host tells the Moonlight client which ports to connect to.
func (h sunshineHost) rules() []forwardRule {
	var rules []forwardRule
	add := func(proto string, offsets []int) {
		for _, off := range offsets {
			port := strconv.Itoa(h.basePort() + off)
			rules = append(rules, forwardRule{proto, net.JoinHostPort("127.0.0.1", port), net.JoinHostPort(h.Host, port)})
		}
	}
	add("tcp", sunshineTCPOffsets)
	add("udp", sunshineUDPOffsets)
	return rules
}

// clientAddress is what to enter as the host in a Moonlight client on the
// console to reach this Sunshine host.
func (h sunshineHost) clientAddress() string {
	if h.basePort() == sunshineDefaultPort {
		return "127.0.0.1"
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(h.basePort()))
}

// sunshineRules returns the forwards for all hosts.
func sunshineRules(hosts []sunshineHost) []forwardRule {
	var rules []forwardRule
	for _, h := range hosts {
		rules = append(rules, h.rules()...)
	}
	return rules
}

// validateSunshineHosts checks that the hosts can be forwarded side by side.
// They all share 127.0.0.1, so each needs its own set of ports, which means
// each must use a different port setting in Sunshine.
func validateSunshineHosts(hosts []sunshineHost) error {
	used := map[string]string{}
	for _, h := range hosts {
		if h.Host == "" || !validHostName(h.Host) {
			return fmt.Errorf("%q does not look like a host name or address", h.Host)
		}
		if p := h.basePort(); p < 1024+5 || p > 65535-21 {
			return fmt.Errorf("port %d for %s is out of range", p, h.Host)
		}
		for _, r := range h.rules() {
			key := r.Proto + " " + r.Listen
			if other, taken := used[key]; taken {
				return fmt.Errorf("%s and %s use overlapping ports; give each Sunshine host its own port setting", other, h.Host)
			}
			used[key] = h.Host
		}
	}
	return nil
}

type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// forwarder runs a set of local forwards.
type forwarder struct {
	dial dialFunc
	logf func(format string, args ...any)

	mu       sync.Mutex
	active   []forwardRule
	closers  []func()
	tcpPorts map[uint16]bool
}

func newForwarder(dial dialFunc, logf func(string, ...any)) *forwarder {
	return &forwarder{dial: dial, logf: logf, tcpPorts: map[uint16]bool{}}
}

// set replaces the running forwards with rules. Rules that cannot be started
// are skipped and reported.
func (f *forwarder) set(rules []forwardRule) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.closers {
		c()
	}
	f.closers, f.active, f.tcpPorts = nil, nil, map[uint16]bool{}

	var errs []error
	for _, r := range rules {
		var stop func()
		var err error
		switch r.Proto {
		case "tcp":
			stop, err = f.startTCP(r)
		case "udp":
			stop, err = f.startUDP(r)
		default:
			err = fmt.Errorf("unknown protocol %q", r.Proto)
		}
		if err != nil {
			f.logf("forward %v: %v", r, err)
			errs = append(errs, fmt.Errorf("%v: %w", r, err))
			continue
		}
		f.closers = append(f.closers, stop)
		f.active = append(f.active, r)
	}
	if len(f.active) > 0 {
		f.logf("local forwards active: %d", len(f.active))
	}
	return errors.Join(errs...)
}

func (f *forwarder) rules() []forwardRule {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]forwardRule(nil), f.active...)
}

// listensOnTCP reports whether a local forward owns the TCP port.
func (f *forwarder) listensOnTCP(port uint16) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tcpPorts[port]
}

func (f *forwarder) startTCP(r forwardRule) (stop func(), err error) {
	ln, err := listenResilient("tcp", r.Listen, f.logf)
	if err != nil {
		return nil, err
	}
	f.tcpPorts[ln.port()] = true
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serveTCP(c, r)
		}
	}()
	return func() { ln.Close() }, nil
}

func (f *forwarder) serveTCP(c net.Conn, r forwardRule) {
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	up, err := f.dial(ctx, "tcp", r.Target)
	cancel()
	if err != nil {
		f.logf("forward %v: %v", r, err)
		return
	}
	defer up.Close()
	pipe(c, up)
}

func (f *forwarder) startUDP(r forwardRule) (stop func(), err error) {
	relay, err := startUDPRelay(udpRelayConfig{
		name:   "forward " + r.String(),
		listen: func() (net.PacketConn, error) { return net.ListenPacket("udp", r.Listen) },
		dial:   func(ctx context.Context) (net.Conn, error) { return f.dial(ctx, "udp", r.Target) },
		logf:   f.logf,
	})
	if err != nil {
		return nil, err
	}
	return relay.stop, nil
}
