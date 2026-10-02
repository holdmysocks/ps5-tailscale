package main

import (
	"net"
	"net/netip"
	"strconv"
	"testing"
	"time"
)

// The exposer must relay datagrams arriving on a "tailnet" socket to the same
// port on the target host and bring the replies back to the sender.
func TestUDPExposer(t *testing.T) {
	// The console's service: echoes with a prefix.
	service, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := service.ReadFrom(buf)
			if err != nil {
				return
			}
			service.WriteTo(append([]byte("ps5:"), buf[:n]...), from)
		}
	}()
	port := uint16(service.LocalAddr().(*net.UDPAddr).Port)

	// Stand-in for tsnet: "listening on the tailnet address" is a loopback
	// socket on some other port, whose address the test then sends to.
	listening := make(chan net.Addr, 4)
	var asked []string
	e := &udpExposer{
		targetHost: "127.0.0.1",
		logf:       t.Logf,
		listen: func(network, addr string) (net.PacketConn, error) {
			asked = append(asked, network+" "+addr)
			pc, err := net.ListenPacket("udp", "127.0.0.1:0")
			if err == nil {
				listening <- pc.LocalAddr()
			}
			return pc, err
		},
	}
	tailnetIP := netip.MustParseAddr("100.64.0.5")
	e.update([]netip.Addr{tailnetIP}, []uint16{port})
	defer e.update(nil, nil)

	want := "udp4 100.64.0.5:" + strconv.Itoa(int(port))
	if len(asked) != 1 || asked[0] != want {
		t.Fatalf("listened on %v, want [%s]", asked, want)
	}
	if got := e.activePorts(); len(got) != 1 || got[0] != port {
		t.Fatalf("activePorts = %v", got)
	}

	client, err := net.Dial("udp", (<-listening).String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for _, msg := range []string{"SRCH", "again"} {
		client.Write([]byte(msg))
		buf := make([]byte, 100)
		client.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := client.Read(buf)
		if err != nil || string(buf[:n]) != "ps5:"+msg {
			t.Fatalf("%q: got %q, %v", msg, buf[:n], err)
		}
	}

	// Unchanged input must not reopen anything.
	e.update([]netip.Addr{tailnetIP}, []uint16{port})
	if len(asked) != 1 {
		t.Errorf("update with the same addresses and ports listened again: %v", asked)
	}
	// An empty port list turns it off.
	e.update([]netip.Addr{tailnetIP}, nil)
	if got := e.activePorts(); len(got) != 0 {
		t.Errorf("activePorts after clearing = %v", got)
	}
}
