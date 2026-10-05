package main

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"
)

func TestOwnDevice(t *testing.T) {
	const suffix = "tail1234.ts.net"
	me := identity{User: 7}
	for _, tt := range []struct {
		name string
		self identity
		peer identity
		want bool
	}{
		{"same user", me, identity{User: 7, DNSName: "pc.tail1234.ts.net."}, true},
		{"another user of the tailnet", me, identity{User: 8, DNSName: "pc.tail1234.ts.net."}, false},
		{"tagged device of the tailnet", me, identity{User: 7, Tagged: true, DNSName: "srv.tail1234.ts.net."}, false},
		{"shared in from another tailnet", me, identity{User: 9, DNSName: "pc.other.ts.net."}, false},
		{"another tailnet claiming the same user", me, identity{User: 7, DNSName: "pc.other.ts.net."}, false},
		{"unknown user", me, identity{DNSName: "pc.tail1234.ts.net."}, false},
		{"tagged console, device of its tailnet", identity{Tagged: true}, identity{User: 8, DNSName: "pc.tail1234.ts.net."}, true},
		{"tagged console, shared device", identity{Tagged: true}, identity{User: 8, DNSName: "pc.other.ts.net."}, false},
	} {
		if got := ownDevice(tt.self, tt.peer, suffix); got != tt.want {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestAccessCache(t *testing.T) {
	var c accessCache
	a := netip.MustParseAddr("100.64.0.2")
	if _, ok := c.get(a); ok {
		t.Fatal("an empty cache had an answer")
	}
	c.put(a, true)
	if allowed, ok := c.get(a); !ok || !allowed {
		t.Fatalf("got %v, %v", allowed, ok)
	}
	c.clear()
	if _, ok := c.get(a); ok {
		t.Fatal("the cache kept its answer after clear")
	}
}

// With the default setting nothing is asked and everything is allowed.
func TestAllowedFromDefault(t *testing.T) {
	d := &daemon{cfg: defaultConfig(), logf: t.Logf}
	if !d.allowedFrom(netip.MustParseAddr("100.64.0.9")) {
		t.Error("the default setting turned a device away")
	}
	// Limited to own devices, a device that cannot be identified is refused.
	d.cfg.AllowFrom = accessOwn
	if d.allowedFrom(netip.MustParseAddr("100.64.0.9")) {
		t.Error("an unidentified device was let in")
	}
}

func TestKeyWarnStage(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	for _, tt := range []struct {
		left  time.Duration
		stage int
		days  int
	}{
		{90 * day, -1, 90},
		{14*day + time.Hour, -1, 14},
		{14 * day, 0, 14},
		{5 * day, 0, 5},
		{3 * day, 1, 3},
		{36 * time.Hour, 1, 1},
		{20 * time.Hour, 2, 0},
		{-time.Hour, 2, -1},
	} {
		expiry := now.Add(tt.left)
		if got := keyWarnStage(now, expiry); got != tt.stage {
			t.Errorf("%v left: stage %d, want %d", tt.left, got, tt.stage)
		}
		if got := daysLeft(now, expiry); got != tt.days {
			t.Errorf("%v left: %d days, want %d", tt.left, got, tt.days)
		}
	}
	if plural(1, "day") != "1 day" || plural(3, "day") != "3 days" {
		t.Error("plural")
	}
}

// A relay with an allow function serves the clients it accepts and stays
// silent towards the ones it turns down.
func TestUDPRelayAllow(t *testing.T) {
	echo, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := echo.ReadFrom(buf)
			if err != nil {
				return
			}
			echo.WriteTo(buf[:n], from)
		}
	}()

	for _, allow := range []bool{true, false} {
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
				return d.DialContext(ctx, "udp", echo.LocalAddr().String())
			},
			allow: func(net.Addr) bool { return allow },
			logf:  t.Logf,
		})
		if err != nil {
			t.Fatal(err)
		}
		c, err := net.Dial("udp", relayAddr.String())
		if err != nil {
			t.Fatal(err)
		}
		c.Write([]byte("ping"))
		c.SetReadDeadline(time.Now().Add(700 * time.Millisecond))
		buf := make([]byte, 16)
		n, err := c.Read(buf)
		if allow && (err != nil || string(buf[:n]) != "ping") {
			t.Errorf("allowed client: got %q, %v", buf[:n], err)
		}
		if !allow && err == nil {
			t.Errorf("refused client got a reply: %q", buf[:n])
		}
		c.Close()
		relay.stop()
	}
}
