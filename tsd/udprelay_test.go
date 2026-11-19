package main

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"
)

// A datagram that reaches the target before its port is open gets an ICMP
// port unreachable back, and the flow's socket reports it on its next read.
// The flow must survive that: once the port opens, the replies have to reach
// the client. Moonlight's first audio ping to a Sunshine host arrives before
// the host opens its audio port, so without this there is no sound.
func TestUDPRelayTargetPortOpensLate(t *testing.T) {
	// Find a free port and leave it closed for now.
	probe, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	target := probe.LocalAddr().String()
	probe.Close()

	var relayAddr net.Addr
	relay, err := startUDPRelay(udpRelayConfig{
		name: "test",
		listen: func() (net.PacketConn, error) {
			pc, err := net.ListenPacket("udp", "127.0.0.1:0")
			if err == nil {
				relayAddr = pc.LocalAddr()
			}
			return pc, err
		},
		dial: func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "udp", target)
		},
		logf: t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.stop()

	client, err := net.Dial("udp", relayAddr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// The first datagram finds the port closed.
	client.Write([]byte("early"))
	time.Sleep(300 * time.Millisecond)

	// Now the service opens: it echoes with a prefix.
	service, err := net.ListenPacket("udp", target)
	if err != nil {
		t.Skipf("could not reopen %s for the service: %v", target, err)
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

	// Like Moonlight's pings, the client keeps sending until it hears back.
	buf := make([]byte, 100)
	for i := 0; i < 10; i++ {
		client.Write([]byte("late"))
		client.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		n, err := client.Read(buf)
		if err == nil {
			if got := string(buf[:n]); got != "ps5:late" {
				t.Fatalf("got %q, want %q", got, "ps5:late")
			}
			return
		}
	}
	t.Fatal("no reply reached the client after the target's port opened")
}

// A flow's kernel socket to the target gets a larger receive buffer, so a
// video burst from a Sunshine host does not overflow the kernel default.
func TestUDPRelayGrowsReadBuffer(t *testing.T) {
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
			service.WriteTo(buf[:n], from)
		}
	}()

	logged := make(chan string, 4)
	var relayAddr net.Addr
	relay, err := startUDPRelay(udpRelayConfig{
		name: "test",
		listen: func() (net.PacketConn, error) {
			pc, err := net.ListenPacket("udp", "127.0.0.1:0")
			if err == nil {
				relayAddr = pc.LocalAddr()
			}
			return pc, err
		},
		dial: func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "udp", service.LocalAddr().String())
		},
		logf: func(format string, args ...any) { logged <- fmt.Sprintf(format, args...) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.stop()

	client, err := net.Dial("udp", relayAddr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.Write([]byte("ping"))
	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := client.Read(make([]byte, 16)); err != nil {
		t.Fatalf("no reply through the relay: %v", err)
	}

	select {
	case msg := <-logged:
		if want := "test: receive buffer set to 1024 KiB"; msg != want {
			t.Fatalf("logged %q, want %q", msg, want)
		}
	case <-time.After(time.Second):
		t.Fatal("the relay did not report the receive buffer")
	}
}
