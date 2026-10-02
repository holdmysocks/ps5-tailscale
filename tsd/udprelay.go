package main

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// A UDP relay sits between a listening socket and a target. Every client
// address that sends to the socket gets its own connection to the target, so
// the target's replies find their way back to the right client. It is used in
// both directions: console apps to a tailnet host (local forwards) and
// tailnet devices to a service on the console (inbound UDP).

// UDP flows that have been silent this long are forgotten.
const udpIdleTimeout = 2 * time.Minute

type udpRelayConfig struct {
	name string
	// listen opens the socket clients send to. It is called again if the
	// socket fails, which on the PS5 happens when the network is
	// reconfigured.
	listen func() (net.PacketConn, error)
	// dial opens the connection to the target for one client.
	dial func(ctx context.Context) (net.Conn, error)
	logf func(format string, args ...any)
}

// udpFlow is the relay state for one client address.
type udpFlow struct {
	out      chan []byte // datagrams from the client waiting to go to the target
	lastSeen atomic.Int64
}

func (fl *udpFlow) touch() { fl.lastSeen.Store(time.Now().UnixNano()) }

func (fl *udpFlow) idle() bool {
	return time.Since(time.Unix(0, fl.lastSeen.Load())) > udpIdleTimeout
}

type udpRelay struct {
	cfg udpRelayConfig

	mu     sync.Mutex
	sock   net.PacketConn
	closed bool
	flows  map[string]*udpFlow
}

// startUDPRelay opens the listening socket and relays until stop is called.
func startUDPRelay(cfg udpRelayConfig) (stop func(), err error) {
	sock, err := cfg.listen()
	if err != nil {
		return nil, err
	}
	r := &udpRelay{cfg: cfg, sock: sock, flows: map[string]*udpFlow{}}
	go r.readLoop()
	return r.stop, nil
}

func (r *udpRelay) stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	r.sock.Close()
}

// socket returns the current listening socket and whether the relay has been
// stopped.
func (r *udpRelay) socket() (net.PacketConn, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sock, r.closed
}

func (r *udpRelay) readLoop() {
	buf := make([]byte, 65535)
	for {
		sock, stopped := r.socket()
		if stopped {
			return
		}
		n, from, err := sock.ReadFrom(buf)
		if err != nil {
			if _, stopped := r.socket(); stopped {
				return
			}
			r.cfg.logf("%s: %v; reopening", r.cfg.name, err)
			sock.Close()
			time.Sleep(time.Second)
			if reopened, err := r.cfg.listen(); err == nil {
				r.mu.Lock()
				if r.closed {
					reopened.Close()
				} else {
					r.sock = reopened
				}
				r.mu.Unlock()
			}
			continue
		}

		key := from.String()
		r.mu.Lock()
		fl := r.flows[key]
		if fl == nil {
			fl = &udpFlow{out: make(chan []byte, 256)}
			r.flows[key] = fl
			go r.serveFlow(key, fl, from)
		}
		r.mu.Unlock()

		fl.touch()
		select {
		case fl.out <- append([]byte(nil), buf[:n]...):
		default: // the target is not keeping up; UDP may drop
		}
	}
}

// serveFlow relays one client's datagrams to the target and the replies
// back, until the flow goes quiet or the relay is stopped.
func (r *udpRelay) serveFlow(key string, fl *udpFlow, client net.Addr) {
	defer func() {
		r.mu.Lock()
		if r.flows[key] == fl {
			delete(r.flows, key)
		}
		r.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	up, err := r.cfg.dial(ctx)
	cancel()
	if err != nil {
		r.cfg.logf("%s: %v", r.cfg.name, err)
		return
	}
	defer up.Close()

	// Replies: target -> client.
	go func() {
		buf := make([]byte, 65535)
		for {
			up.SetReadDeadline(time.Now().Add(udpIdleTimeout))
			n, err := up.Read(buf)
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() && !fl.idle() {
					continue
				}
				return
			}
			fl.touch()
			if sock, stopped := r.socket(); !stopped {
				sock.WriteTo(buf[:n], client)
			}
		}
	}()

	idle := time.NewTicker(udpIdleTimeout / 4)
	defer idle.Stop()
	for {
		select {
		case b := <-fl.out:
			if _, err := up.Write(b); err != nil {
				return
			}
		case <-idle.C:
			if _, stopped := r.socket(); stopped || fl.idle() {
				return
			}
		}
	}
}
