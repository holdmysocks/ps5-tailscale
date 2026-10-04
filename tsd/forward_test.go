package main

import (
	"io"
	"net"
	"net/netip"
	"testing"
)

func TestAddressedTo(t *testing.T) {
	ip4, ip6 := netip.MustParseAddr("100.64.0.1"), netip.MustParseAddr("fd7a:115c:a1e0::1")
	for addr, want := range map[string]bool{
		"100.64.0.1":         true,
		"::ffff:100.64.0.1":  true,
		"fd7a:115c:a1e0::1":  true,
		"100.64.0.2":         false,
		"93.184.216.34":      false,
		"127.0.0.1":          false,
		"2606:4700:4700::64": false,
	} {
		if got := addressedTo(netip.MustParseAddr(addr), ip4, ip6); got != want {
			t.Errorf("addressedTo(%s) = %v, want %v", addr, got, want)
		}
	}
	// Before the node has its addresses nothing is for it.
	if addressedTo(netip.Addr{}, netip.Addr{}, netip.Addr{}) {
		t.Error("an invalid address matched")
	}
}

// pipe must pass a half-close through: the ELF loader protocol and FTP data
// connections both rely on the reader seeing EOF while the other direction
// stays open.
func TestPipeHalfClose(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// "Local service": reads everything, then answers.
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		b, _ := io.ReadAll(c)
		c.Write(append([]byte("got "), b...))
	}()

	tcpClient, tcpProxy := tcpPair(t)

	local, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		pipe(tcpProxy, local)
		tcpProxy.Close()
		local.Close()
	}()

	tcpClient.Write([]byte("payload"))
	tcpClient.(*net.TCPConn).CloseWrite()
	b, err := io.ReadAll(tcpClient)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "got payload" {
		t.Fatalf("got %q", b)
	}
}

func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ch := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		ch <- c
	}()
	a, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return a, <-ch
}
