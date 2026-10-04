package main

import (
	"sort"
	"strings"

	"tailscale.com/ipn/ipnstate"
)

// The device list of the status page. A tailnet with a VPN add-on has
// hundreds of exit servers among its peers, so peers are sorted into kinds
// and the exit servers are only sent to the page when it asks for them.

const (
	peerOwn    = "own"    // a device of this tailnet
	peerShared = "shared" // a device of another tailnet, shared with this one
	peerVPN    = "vpn"    // an exit server of a VPN add-on
)

// vpnDomains are the DNS suffixes of the exit servers that VPN add-ons put
// in a tailnet. Tailscale's own "status" command hides them the same way.
var vpnDomains = []string{"mullvad.ts.net"}

type peerInfo struct {
	Name   string `json:"name"`
	IP     string `json:"ip"`
	OS     string `json:"os"`
	Online bool   `json:"online"`
	Kind   string `json:"kind"`
	// ExitNode is "offered" for a device that can be used as an exit node
	// and "used" for the one this console uses.
	ExitNode string `json:"exitNode,omitempty"`
	// Location is where an exit server says it is ("Vienna, Austria").
	Location string   `json:"location,omitempty"`
	Tags     []string `json:"tags,omitempty"`
}

// peerCount counts the peers of one kind.
type peerCount struct {
	Total  int `json:"total"`
	Online int `json:"online"`
}

func hasDNSSuffix(name, suffix string) bool {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	suffix = strings.ToLower(strings.Trim(suffix, "."))
	return suffix != "" && (name == suffix || strings.HasSuffix(name, "."+suffix))
}

// peerKind sorts a peer into one of the kinds. suffix is this tailnet's
// MagicDNS suffix.
func peerKind(p *ipnstate.PeerStatus, suffix string) string {
	if p.ExitNodeOption || p.ExitNode {
		for _, d := range vpnDomains {
			if hasDNSSuffix(p.DNSName, d) {
				return peerVPN
			}
		}
	}
	if p.DNSName != "" && suffix != "" && !hasDNSSuffix(p.DNSName, suffix) {
		return peerShared
	}
	return peerOwn
}

func newPeerInfo(p *ipnstate.PeerStatus, suffix string) peerInfo {
	pi := peerInfo{Name: p.HostName, OS: p.OS, Online: p.Online, Kind: peerKind(p, suffix)}
	if p.DNSName != "" {
		pi.Name = strings.SplitN(p.DNSName, ".", 2)[0]
	}
	if len(p.TailscaleIPs) > 0 {
		pi.IP = p.TailscaleIPs[0].String()
	}
	switch {
	case p.ExitNode:
		pi.ExitNode = "used"
	case p.ExitNodeOption:
		pi.ExitNode = "offered"
	}
	if l := p.Location; l != nil {
		parts := []string{}
		for _, s := range []string{l.City, l.Country} {
			if s != "" {
				parts = append(parts, s)
			}
		}
		pi.Location = strings.Join(parts, ", ")
	}
	if p.Tags != nil {
		pi.Tags = p.Tags.AsSlice()
	}
	return pi
}

// peersFromStatus lists the peers for the status page, online ones first.
// VPN exit servers are counted, and listed only if withVPN is set; the one
// in use is always listed.
func peersFromStatus(st *ipnstate.Status, withVPN bool) (peers []peerInfo, vpn peerCount) {
	peers = []peerInfo{}
	for _, p := range st.Peer {
		pi := newPeerInfo(p, st.MagicDNSSuffix)
		if pi.Kind == peerVPN {
			vpn.Total++
			if pi.Online {
				vpn.Online++
			}
			if !withVPN && pi.ExitNode != "used" {
				continue
			}
		}
		peers = append(peers, pi)
	}
	sort.Slice(peers, func(i, j int) bool {
		if peers[i].Online != peers[j].Online {
			return peers[i].Online
		}
		return peers[i].Name < peers[j].Name
	})
	return peers, vpn
}
