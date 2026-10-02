package main

import (
	"io"
	"net"
	"testing"
)

func TestRemoveAutoloadEntry(t *testing.T) {
	tests := []struct {
		name, in, want string
		found          bool
	}{
		{"absent", "kstuff.elf\n!500\nftpsrv.elf\n", "kstuff.elf\n!500\nftpsrv.elf\n", false},
		{"last line", "kstuff.elf\ntailscale.elf\n", "kstuff.elf\n", true},
		{"no trailing newline", "kstuff.elf\ntailscale.elf", "kstuff.elf\n", true},
		{"middle, CRLF kept", "a.elf\r\ntailscale.elf\r\nb.elf\r\n", "a.elf\r\nb.elf\r\n", true},
		{"listed twice", "tailscale.elf\na.elf\n tailscale.elf \n", "a.elf\n", true},
		{"similar names stay", "# tailscale.elf\nmy-tailscale.elf\ntailscale.elf.bak\n", "# tailscale.elf\nmy-tailscale.elf\ntailscale.elf.bak\n", false},
		{"empty", "", "", false},
	}
	for _, tt := range tests {
		got, found := removeAutoloadEntry(tt.in, daemonFileName)
		if got != tt.want || found != tt.found {
			t.Errorf("%s: got %q, %v; want %q, %v", tt.name, got, found, tt.want, tt.found)
		}
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
