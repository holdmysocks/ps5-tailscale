package main

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// startEcho runs a TCP and a UDP echo server on the same port and returns it.
func startEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	pc, err := net.ListenPacket("udp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 65535)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			pc.WriteTo(append([]byte("echo:"), buf[:n]...), from)
		}
	}()
	return ln.Addr().String()
}

// freePort returns a localhost address nothing listens on.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func TestLocalForward(t *testing.T) {
	echo := startEcho(t)
	listen := freePort(t)

	// The "tailnet" is the loopback interface; record what gets dialed.
	var dialed []string
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed = append(dialed, network+" "+addr)
		var d net.Dialer
		return d.DialContext(ctx, network, echo)
	}
	f := newForwarder(dial, t.Logf)
	err := f.set([]forwardRule{
		{"tcp", listen, "sunshine-host:47989"},
		{"udp", listen, "sunshine-host:47998"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer f.set(nil)

	_, port, _ := net.SplitHostPort(listen)
	if got := f.rules(); len(got) != 2 {
		t.Fatalf("active rules: %v", got)
	}
	var portNum uint16
	for _, c := range port {
		portNum = portNum*10 + uint16(c-'0')
	}
	if !f.listensOnTCP(portNum) {
		t.Errorf("listensOnTCP(%d) = false", portNum)
	}

	// TCP: data and the half-close go through.
	c, err := net.Dial("tcp", listen)
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte("hello"))
	c.(*net.TCPConn).CloseWrite()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	b, err := io.ReadAll(c)
	c.Close()
	if err != nil || string(b) != "hello" {
		t.Fatalf("tcp: got %q, %v", b, err)
	}

	// UDP: several datagrams from one client share a flow and come back to it.
	u, err := net.Dial("udp", listen)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	for _, msg := range []string{"one", "two", "three"} {
		u.Write([]byte(msg))
		buf := make([]byte, 100)
		u.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := u.Read(buf)
		if err != nil || string(buf[:n]) != "echo:"+msg {
			t.Fatalf("udp %q: got %q, %v", msg, buf[:n], err)
		}
	}

	want := "tcp sunshine-host:47989,udp sunshine-host:47998"
	if got := strings.Join(dialed, ","); got != want {
		t.Errorf("dialed %q, want %q (one dial per TCP connection and per UDP client)", got, want)
	}

	// Replacing the rules frees the ports.
	if err := f.set(nil); err != nil {
		t.Fatal(err)
	}
	if c, err := net.DialTimeout("tcp", listen, time.Second); err == nil {
		c.Close()
		t.Errorf("TCP forward still accepting after it was removed")
	}
}

func TestSunshineRules(t *testing.T) {
	if got := sunshineRules(""); got != nil {
		t.Errorf("no host: got %v", got)
	}
	rules := sunshineRules("gaming-pc")
	if len(rules) != 7 {
		t.Fatalf("got %d rules, want 7", len(rules))
	}
	if got, want := rules[0].String(), "tcp 127.0.0.1:47984 -> gaming-pc:47984"; got != want {
		t.Errorf("first rule %q, want %q", got, want)
	}
	for _, r := range rules {
		if !strings.HasPrefix(r.Listen, "127.0.0.1:") {
			t.Errorf("%v does not listen on localhost only", r)
		}
	}
}

func TestIsLocalDestination(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8080":          true,
		"localhost:80":            true,
		"192.168.1.50:2121":       true,
		"10.0.0.5:443":            true,
		"[::1]:80":                true,
		"100.64.0.5:8090":         false, // tailnet address
		"my-pc.tailnet.ts.net:80": false,
		"example.com:443":         false,
	} {
		if got := isLocalDestination(addr); got != want {
			t.Errorf("isLocalDestination(%q) = %v, want %v", addr, got, want)
		}
	}
}

// A listener that dies must be reopened without the server noticing.
func TestResilientListenerReopens(t *testing.T) {
	ln, err := listenResilient("tcp", "127.0.0.1:0", t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()

	accepted := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()

	connect := func() {
		t.Helper()
		var c net.Conn
		var err error
		for i := 0; i < 50; i++ {
			if c, err = net.DialTimeout("tcp", addr, time.Second); err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		c.Close()
		select {
		case s := <-accepted:
			s.Close()
		case <-time.After(5 * time.Second):
			t.Fatal("connection was not accepted")
		}
	}

	connect()
	// Kill the socket underneath, as a network reconfiguration would.
	inner, _ := ln.current()
	inner.Close()
	connect()
}
