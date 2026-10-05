package main

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"time"

	"tailscale.com/tailcfg"
)

// Who on the tailnet may reach the console's services.
//
// Tailscale's access rules decide which devices can send to this node at
// all. On top of that the console can be limited to its owner's devices,
// because what it exposes (the payload loader above all) is more than most
// tailnets' rules were written with in mind, and a device that someone else
// shared into the tailnet is governed by rules the owner may not have looked
// at since.

const (
	accessAll = "all" // every device the tailnet's access rules allow
	accessOwn = "own" // only devices of the user this console is logged in as
)

// identity is what the access check needs to know about a node.
type identity struct {
	User    tailcfg.UserID
	Tagged  bool
	DNSName string
}

// ownDevice reports whether peer belongs to the same user as self. A tagged
// node has no user: if the console itself is tagged, every device of its own
// tailnet counts, and a tagged peer never counts otherwise. suffix is the
// tailnet's MagicDNS suffix; a device from another tailnet never counts.
func ownDevice(self, peer identity, suffix string) bool {
	if peer.DNSName != "" && suffix != "" && !hasDNSSuffix(peer.DNSName, suffix) {
		return false
	}
	if self.Tagged {
		return true
	}
	return !peer.Tagged && peer.User != 0 && peer.User == self.User
}

// accessCache remembers recent decisions, so that a busy port does not ask
// Tailscale about the same device for every connection.
type accessCache struct {
	mu      sync.Mutex
	entries map[netip.Addr]accessEntry
}

type accessEntry struct {
	allowed bool
	at      time.Time
}

const accessCacheTime = time.Minute

func (c *accessCache) get(addr netip.Addr) (allowed, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[addr]
	if !ok || time.Since(e.at) > accessCacheTime {
		return false, false
	}
	return e.allowed, true
}

func (c *accessCache) put(addr netip.Addr, allowed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil || len(c.entries) > 1024 {
		c.entries = map[netip.Addr]accessEntry{}
	}
	c.entries[addr] = accessEntry{allowed: allowed, at: time.Now()}
}

func (c *accessCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = nil
}

// allowedFrom reports whether the tailnet device at src may use the
// console's services under the current setting.
func (d *daemon) allowedFrom(src netip.Addr) bool {
	d.mu.Lock()
	mode := d.cfg.AllowFrom
	d.mu.Unlock()
	if mode != accessOwn {
		return true
	}
	src = src.Unmap()
	if allowed, ok := d.access.get(src); ok {
		return allowed
	}
	allowed, who := d.lookupOwnDevice(src)
	d.access.put(src, allowed)
	if !allowed {
		d.logf("refused %s (%s): only this console's owner's devices may connect", src, who)
	}
	return allowed
}

// lookupOwnDevice asks Tailscale who src is. Anything that cannot be
// established counts as not allowed.
func (d *daemon) lookupOwnDevice(src netip.Addr) (allowed bool, who string) {
	if d.lc == nil {
		return false, "unknown"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	st, err := d.lc.StatusWithoutPeers(ctx)
	if err != nil || st.Self == nil {
		return false, "unknown"
	}
	// WhoIs wants an address with a port; the port plays no part for a
	// tailnet address.
	res, err := d.lc.WhoIs(ctx, netip.AddrPortFrom(src, 1).String())
	if err != nil || res.Node == nil {
		return false, "unknown"
	}
	who = res.Node.Name
	if res.UserProfile != nil && res.UserProfile.LoginName != "" {
		who += ", " + res.UserProfile.LoginName
	}
	self := identity{User: st.Self.UserID, Tagged: st.Self.IsTagged()}
	peer := identity{User: res.Node.User, Tagged: res.Node.IsTagged(), DNSName: res.Node.Name}
	return ownDevice(self, peer, st.MagicDNSSuffix), who
}

// allowedFromAddr is allowedFrom for the address of a datagram.
func (d *daemon) allowedFromAddr(from net.Addr) bool {
	ap, err := netip.ParseAddrPort(from.String())
	if err != nil {
		return false
	}
	return d.allowedFrom(ap.Addr())
}
