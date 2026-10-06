package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"
)

// The diagnostics file: everything someone helping with a problem needs, in
// one download, so that a bug report does not depend on getting files off
// the console by hand.
//
// It goes on the internet when it is attached to a report, so what can be
// left out without making it useless is left out or blanked: the password
// hash, auth keys, login links, e-mail addresses, the tailnet's name and
// public IP addresses. Device names and tailnet addresses stay; without them
// the logs cannot be followed.

const (
	diagLauncherTail = 64 << 10
	diagMainTail     = 256 << 10
	diagDebugTail    = 512 << 10
)

var (
	reEmail   = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	reTailnet = regexp.MustCompile(`\b([A-Za-z0-9-]+)\.[A-Za-z0-9-]+\.ts\.net\b`)
	// The tailnet's name on its own, as it appears in DNS settings.
	reTailnetBare = regexp.MustCompile(`\b[A-Za-z0-9-]+\.ts\.net\b`)
	reLoginURL    = regexp.MustCompile(`https://login\.tailscale\.com/a/[A-Za-z0-9]+`)
	reAuthKey     = regexp.MustCompile(`tskey-[A-Za-z0-9-]+`)
	reIPv4        = regexp.MustCompile(`\b(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})\b`)
	reFirmware    = regexp.MustCompile(`firmware (\d+\.\d+)`)
)

// scrub blanks what should not be published. It keeps the shape of the
// text, so the logs still read as logs.
func scrub(b []byte) []byte {
	b = reLoginURL.ReplaceAll(b, []byte("https://login.tailscale.com/a/<removed>"))
	b = reAuthKey.ReplaceAll(b, []byte("tskey-<removed>"))
	b = reEmail.ReplaceAll(b, []byte("<email>"))
	b = reTailnet.ReplaceAll(b, []byte("$1.<tailnet>.ts.net"))
	b = reTailnetBare.ReplaceAll(b, []byte("<tailnet>.ts.net"))
	return reIPv4.ReplaceAllFunc(b, func(ip []byte) []byte {
		if publicIPv4(string(ip)) {
			return []byte("<public-ip>")
		}
		return ip
	})
}

// publicIPv4 reports whether s is an address on the internet, as opposed to
// a private, tailnet, loopback or otherwise special one. Text that only
// looks like an address (a version number, say) is left alone.
func publicIPv4(s string) bool {
	var a, b, c, d int
	if n, _ := fmt.Sscanf(s, "%d.%d.%d.%d", &a, &b, &c, &d); n != 4 || a > 255 || b > 255 || c > 255 || d > 255 {
		return false
	}
	switch {
	case a == 0, a == 10, a == 127, a >= 224:
		return false
	case a == 100 && b >= 64 && b <= 127: // tailnet addresses
		return false
	case a == 169 && b == 254:
		return false
	case a == 172 && b >= 16 && b <= 31:
		return false
	case a == 192 && b == 168:
		return false
	case a == 192 && b == 0 && c == 2:
		return false
	}
	// A well-known public resolver says nothing about the user.
	switch s {
	case "1.1.1.1", "8.8.8.8", "9.9.9.9":
		return false
	}
	return true
}

func fileTail(path string, max int64) []byte {
	f, err := os.Open(path)
	if err != nil {
		return []byte("(" + err.Error() + ")\n")
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > max {
		f.Seek(fi.Size()-max, io.SeekStart)
	}
	b, _ := io.ReadAll(onlyReader{f})
	return b
}

// diagnostics assembles the file.
func (d *daemon) diagnostics(r *http.Request) []byte {
	var out bytes.Buffer
	section := func(title string) { fmt.Fprintf(&out, "\n===== %s =====\n", title) }

	launcher := fileTail(filepath.Join(dataDir, "launcher.log"), diagLauncherTail)
	firmware := "unknown"
	if m := reFirmware.FindAllSubmatch(launcher, -1); len(m) > 0 {
		firmware = string(m[len(m)-1][1])
	}

	d.mu.Lock()
	cfg := d.cfg
	state, lastErr := d.state, d.lastErr
	latest := d.latest.Version
	d.mu.Unlock()

	fmt.Fprintf(&out, "ps5-tailscale diagnostics\n")
	fmt.Fprintf(&out, "Please read this file before posting it. E-mail addresses, the tailnet's name, public IP\n")
	fmt.Fprintf(&out, "addresses, keys and the password have been removed; device names and tailnet addresses have not.\n\n")
	fmt.Fprintf(&out, "version:      %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&out, "firmware:     %s\n", firmware)
	fmt.Fprintf(&out, "created:      %s\n", time.Now().UTC().Format("2006-01-02 15:04:05 UTC"))
	fmt.Fprintf(&out, "running for:  %s\n", time.Since(d.started).Round(time.Second))
	fmt.Fprintf(&out, "state:        %s\n", state)
	if lastErr != "" {
		fmt.Fprintf(&out, "last error:   %s\n", lastErr)
	}
	if latest != "" {
		fmt.Fprintf(&out, "latest known: %s\n", latest)
	}
	if d.dns != nil {
		fmt.Fprintf(&out, "name lookups: %s\n", d.dns.describe())
	}
	if d.udp != nil {
		fmt.Fprintf(&out, "UDP ports:    %v\n", d.udp.activePorts())
	}
	if d.fwd != nil {
		fmt.Fprintf(&out, "forwards:     %d active\n", len(d.fwd.rules()))
	}

	if d.lc != nil {
		if st, err := d.status(r.Context()); err == nil {
			section("tailscale")
			fmt.Fprintf(&out, "backend state: %s\n", st.BackendState)
			for _, h := range st.Health {
				fmt.Fprintf(&out, "health: %s\n", h)
			}
			if st.Self != nil {
				fmt.Fprintf(&out, "self: %s, addresses %v, relay %q, key expiry %v\n", st.Self.DNSName, st.Self.TailscaleIPs, st.Self.Relay, st.Self.KeyExpiry)
			}
			peers, vpn := peersFromStatus(st, false)
			fmt.Fprintf(&out, "peers: %d, VPN exit servers: %d\n", len(peers), vpn.Total)
			for _, p := range peers {
				fmt.Fprintf(&out, "  %-24s %-16s %-8s online=%-5v %s %s\n", p.Name, p.IP, p.OS, p.Online, p.Kind, strings.TrimSpace(p.Conn+" "+p.Via))
			}
		} else {
			section("tailscale")
			fmt.Fprintf(&out, "status: %v\n", err)
		}
	}

	section("settings (password and auth key removed)")
	if cfg.PasswordHash != "" {
		cfg.PasswordHash = "(set)"
	}
	if cfg.AuthKey != "" {
		cfg.AuthKey = "(set)"
	}
	// cfg is a copy, but its slices are the daemon's own: copy before
	// blanking.
	cfg.Wake = slices.Clone(cfg.Wake)
	for i := range cfg.Wake {
		cfg.Wake[i].MAC = "(set)"
	}
	if b, err := json.MarshalIndent(cfg, "", "  "); err == nil {
		out.Write(b)
		out.WriteByte('\n')
	}

	section("launcher.log")
	out.Write(launcher)
	section("tailscale.log (end)")
	out.Write(fileTail(filepath.Join(dataDir, "tailscale.log"), diagMainTail))
	section("tailscale-debug.log (end)")
	out.Write(fileTail(filepath.Join(dataDir, "tailscale-debug.log"), diagDebugTail))
	return scrub(out.Bytes())
}

func (d *daemon) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	name := fmt.Sprintf("ps5-tailscale-diagnostics-%s-%s.txt", version, time.Now().UTC().Format("20060102-150405"))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(d.diagnostics(r))
}
