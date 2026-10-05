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
	ln, pc := listenBoth(t)
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

// listenBoth opens a TCP and a UDP socket on the same localhost port. A port
// the system hands out for TCP is not always available for UDP (Windows
// reserves ranges per protocol), so it tries until both work.
func listenBoth(t *testing.T) (net.Listener, net.PacketConn) {
	t.Helper()
	var lastErr error
	for range 50 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		pc, err := net.ListenPacket("udp", ln.Addr().String())
		if err == nil {
			return ln, pc
		}
		lastErr = err
		ln.Close()
	}
	t.Fatal(lastErr)
	return nil, nil
}

// freePort returns a localhost address nothing listens on, free for both
// TCP and UDP.
func freePort(t *testing.T) string {
	t.Helper()
	ln, pc := listenBoth(t)
	defer ln.Close()
	defer pc.Close()
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
	if got := sunshineRules(nil); got != nil {
		t.Errorf("no hosts: got %v", got)
	}

	// Default port: the well-known Sunshine ports.
	def := sunshineHost{Host: "gaming-pc"}
	var got []string
	for _, r := range def.rules() {
		got = append(got, r.String())
		if !strings.HasPrefix(r.Listen, "127.0.0.1:") {
			t.Errorf("%v does not listen on localhost only", r)
		}
	}
	want := []string{
		"tcp 127.0.0.1:47984 -> gaming-pc:47984",
		"tcp 127.0.0.1:47989 -> gaming-pc:47989",
		"tcp 127.0.0.1:48010 -> gaming-pc:48010",
		"udp 127.0.0.1:47998 -> gaming-pc:47998",
		"udp 127.0.0.1:47999 -> gaming-pc:47999",
		"udp 127.0.0.1:48000 -> gaming-pc:48000",
		"udp 127.0.0.1:48002 -> gaming-pc:48002",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("default port rules:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if a := def.clientAddress(); a != "127.0.0.1" {
		t.Errorf("client address %q", a)
	}

	// A host on another port keeps its own port numbers, shifted as a set.
	alt := sunshineHost{Host: "office-pc", Port: 48989}
	if r := alt.rules(); r[0].String() != "tcp 127.0.0.1:48984 -> office-pc:48984" || r[6].String() != "udp 127.0.0.1:49002 -> office-pc:49002" {
		t.Errorf("custom port rules: %v", r)
	}
	if a := alt.clientAddress(); a != "127.0.0.1:48989" {
		t.Errorf("client address %q", a)
	}
	if n := len(sunshineRules([]sunshineHost{def, alt})); n != 14 {
		t.Errorf("two hosts: %d rules, want 14", n)
	}
}

func TestValidateSunshineHosts(t *testing.T) {
	ok := [][]sunshineHost{
		nil,
		{{Host: "gaming-pc"}},
		{{Host: "gaming-pc"}, {Host: "office-pc", Port: 48989}},
		{{Host: "100.64.0.2", Port: 50000}},
	}
	for _, hosts := range ok {
		if err := validateSunshineHosts(hosts); err != nil {
			t.Errorf("%v: unexpected error %v", hosts, err)
		}
	}
	bad := [][]sunshineHost{
		{{Host: ""}},
		{{Host: "bad host"}},
		{{Host: "a"}, {Host: "b"}},                  // same ports
		{{Host: "a"}, {Host: "b", Port: 47989 + 5}}, // b's HTTPS port is a's HTTP port
		{{Host: "a", Port: 80}},                     // too low
		{{Host: "a", Port: 65530}},                  // derived ports past 65535
	}
	for _, hosts := range bad {
		if err := validateSunshineHosts(hosts); err == nil {
			t.Errorf("%v: expected an error", hosts)
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
