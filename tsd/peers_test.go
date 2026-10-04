package main

import (
	"net/netip"
	"testing"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/views"
)

func testStatus() *ipnstate.Status {
	tags := views.SliceOf([]string{"tag:server"})
	peers := []*ipnstate.PeerStatus{
		{HostName: "Desk PC", DNSName: "desk.tail1234.ts.net.", OS: "windows", Online: true,
			TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.2")}},
		{HostName: "nas", DNSName: "nas.tail1234.ts.net.", OS: "linux", ExitNodeOption: true, Tags: &tags},
		{HostName: "friend", DNSName: "laptop.tail9999.ts.net.", OS: "macOS", Online: true},
		{HostName: "at-vie-wg-001", DNSName: "at-vie-wg-001.mullvad.ts.net.", Online: true, ExitNodeOption: true,
			Location: &tailcfg.Location{Country: "Austria", City: "Vienna"}},
		{HostName: "se-sto-wg-001", DNSName: "se-sto-wg-001.mullvad.ts.net.", ExitNodeOption: true},
	}
	st := &ipnstate.Status{MagicDNSSuffix: "tail1234.ts.net", Peer: map[key.NodePublic]*ipnstate.PeerStatus{}}
	for _, p := range peers {
		st.Peer[key.NewNode().Public()] = p
	}
	return st
}

func TestPeersFromStatus(t *testing.T) {
	peers, vpn := peersFromStatus(testStatus(), false)
	if vpn != (peerCount{Total: 2, Online: 1}) {
		t.Errorf("vpn count = %+v", vpn)
	}
	// Online first, then by name; no VPN servers.
	want := []peerInfo{
		{Name: "desk", IP: "100.64.0.2", OS: "windows", Online: true, Kind: peerOwn},
		{Name: "laptop", OS: "macOS", Online: true, Kind: peerShared},
		{Name: "nas", OS: "linux", Kind: peerOwn, ExitNode: "offered", Tags: []string{"tag:server"}},
	}
	if len(peers) != len(want) {
		t.Fatalf("got %d peers, want %d: %+v", len(peers), len(want), peers)
	}
	for i := range want {
		g, w := peers[i], want[i]
		if g.Name != w.Name || g.IP != w.IP || g.OS != w.OS || g.Online != w.Online || g.Kind != w.Kind ||
			g.ExitNode != w.ExitNode || len(g.Tags) != len(w.Tags) {
			t.Errorf("peer %d = %+v, want %+v", i, g, w)
		}
	}

	peers, _ = peersFromStatus(testStatus(), true)
	if len(peers) != 5 {
		t.Fatalf("with VPN servers: got %d peers, want 5", len(peers))
	}
	for _, p := range peers {
		if p.Name == "at-vie-wg-001" && (p.Kind != peerVPN || p.Location != "Vienna, Austria" || p.ExitNode != "offered") {
			t.Errorf("VPN server = %+v", p)
		}
	}
}

func TestExitServerInUseIsAlwaysListed(t *testing.T) {
	st := testStatus()
	for _, p := range st.Peer {
		if p.HostName == "se-sto-wg-001" {
			p.ExitNode = true
		}
	}
	peers, _ := peersFromStatus(st, false)
	found := false
	for _, p := range peers {
		if p.Name == "se-sto-wg-001" {
			found = p.Kind == peerVPN && p.ExitNode == "used"
		}
	}
	if !found {
		t.Errorf("the exit server in use is missing: %+v", peers)
	}
}

func TestPeerKind(t *testing.T) {
	for _, tt := range []struct {
		name string
		p    ipnstate.PeerStatus
		want string
	}{
		{"own", ipnstate.PeerStatus{DNSName: "a.tail1234.ts.net."}, peerOwn},
		{"own exit node", ipnstate.PeerStatus{DNSName: "a.tail1234.ts.net.", ExitNodeOption: true}, peerOwn},
		{"no DNS name", ipnstate.PeerStatus{HostName: "a"}, peerOwn},
		{"shared", ipnstate.PeerStatus{DNSName: "a.other.ts.net."}, peerShared},
		{"suffix must match a whole label", ipnstate.PeerStatus{DNSName: "a.xtail1234.ts.net."}, peerShared},
		{"vpn", ipnstate.PeerStatus{DNSName: "x.mullvad.ts.net.", ExitNodeOption: true}, peerVPN},
		{"vpn domain but no exit node", ipnstate.PeerStatus{DNSName: "x.mullvad.ts.net."}, peerShared},
	} {
		if got := peerKind(&tt.p, "tail1234.ts.net"); got != tt.want {
			t.Errorf("%s: got %s, want %s", tt.name, got, tt.want)
		}
	}
	// Without a suffix (not logged in yet) nothing is taken for shared.
	if got := peerKind(&ipnstate.PeerStatus{DNSName: "a.other.ts.net."}, ""); got != peerOwn {
		t.Errorf("no suffix: got %s", got)
	}
}
