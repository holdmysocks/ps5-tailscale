package main

import (
	"net"
	"sync"
	"time"
)

// resilientListener is a TCP listener that reopens itself when accepting
// fails.
//
// On the PS5 a listening socket dies when the network is reconfigured, for
// example when the connection settings are changed: accept returns the
// Sony-specific errno 163 and never works again. Servers treat that as fatal
// and stop. This listener closes the dead socket, listens on the same address
// again and carries on, so the server on top of it never notices.
type resilientListener struct {
	network string
	addr    string
	logf    func(format string, args ...any)
	// onReopen, if set, is called after the socket had to be reopened.
	onReopen func()

	mu     sync.Mutex
	ln     net.Listener
	closed bool
}

func listenResilient(network, addr string, logf func(string, ...any)) (*resilientListener, error) {
	ln, err := net.Listen(network, addr)
	if err != nil {
		return nil, err
	}
	// Remember the concrete address, so that ":0" reopens on the same port.
	return &resilientListener{network: network, addr: ln.Addr().String(), logf: logf, ln: ln}, nil
}

func (l *resilientListener) current() (net.Listener, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ln, l.closed
}

func (l *resilientListener) Accept() (net.Conn, error) {
	for {
		ln, closed := l.current()
		if closed {
			return nil, net.ErrClosed
		}
		c, err := ln.Accept()
		if err == nil {
			return c, nil
		}
		if _, closed := l.current(); closed {
			return nil, net.ErrClosed
		}
		l.logf("listener %s: %v; reopening", l.addr, err)
		ln.Close()
		if !l.reopen() {
			return nil, net.ErrClosed
		}
	}
}

// reopen listens again, retrying until it works or the listener is closed.
func (l *resilientListener) reopen() bool {
	for delay := 250 * time.Millisecond; ; delay = min(2*delay, 5*time.Second) {
		time.Sleep(delay)
		if _, closed := l.current(); closed {
			return false
		}
		ln, err := net.Listen(l.network, l.addr)
		if err != nil {
			continue
		}
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			ln.Close()
			return false
		}
		l.ln = ln
		l.mu.Unlock()
		l.logf("listener %s: reopened", l.addr)
		if l.onReopen != nil {
			l.onReopen()
		}
		return true
	}
}

func (l *resilientListener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	return l.ln.Close()
}

func (l *resilientListener) Addr() net.Addr {
	ln, _ := l.current()
	return ln.Addr()
}

// port returns the TCP port the listener is bound to.
func (l *resilientListener) port() uint16 {
	if a, ok := l.Addr().(*net.TCPAddr); ok {
		return uint16(a.Port)
	}
	return 0
}
