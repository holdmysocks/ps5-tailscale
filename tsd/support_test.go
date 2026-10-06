package main

import (
	"bytes"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScrub(t *testing.T) {
	in := `2026-10-06 login URL: https://login.tailscale.com/a/1a2b3c4d5e6f
self: ps5.tail-scale-fish.ts.net. user someone@example.com authkey tskey-auth-kABCDEF123-xyzXYZ
endpoints 203.0.113.7:49866 (portmap), 192.168.1.50:49866 (local), 100.64.0.5, 10.0.0.5, 172.20.1.1
derp 198.51.100.9:443, resolver 1.1.1.1:53, version 1.104.0, loopback 127.0.0.1:8090
peer gaming-pc.tail-scale-fish.ts.net
dns: Set: {Routes:{ts.net.:[199.247.155.53] tail-scale-fish.ts.net.:[]} SearchDomains:[tail-scale-fish.ts.net.]}`
	out := string(scrub([]byte(in)))
	for _, gone := range []string{"1a2b3c4d5e6f", "someone@example.com", "kABCDEF123", "tail-scale-fish", "203.0.113.7", "198.51.100.9"} {
		if strings.Contains(out, gone) {
			t.Errorf("%q was not removed:\n%s", gone, out)
		}
	}
	for _, kept := range []string{"192.168.1.50:49866", "100.64.0.5", "10.0.0.5", "172.20.1.1", "1.1.1.1:53", "1.104.0", "127.0.0.1:8090",
		"ps5.<tailnet>.ts.net", "gaming-pc.<tailnet>.ts.net", "<email>", "<public-ip>:49866", "2026-10-06"} {
		if !strings.Contains(out, kept) {
			t.Errorf("%q is missing:\n%s", kept, out)
		}
	}
}

func TestDiagnostics(t *testing.T) {
	old := dataDir
	dataDir = t.TempDir()
	t.Cleanup(func() { dataDir = old })
	os.WriteFile(filepath.Join(dataDir, "launcher.log"), []byte("2026-10-05 launcher: starting, firmware 9.60, pid 5\n2026-10-06 launcher: starting, firmware 13.42, pid 115\n"), 0o644)
	os.WriteFile(filepath.Join(dataDir, "tailscale.log"), []byte("main log line from someone@example.com\n"), 0o644)

	cfg := defaultConfig()
	cfg.PasswordHash = "pbkdf2-sha256$210000$c2FsdA$a2V5"
	cfg.AuthKey = "tskey-auth-secret"
	cfg.Wake = []wakeTarget{{Name: "gaming-pc", MAC: "00:11:22:aa:bb:cc"}}
	d := &daemon{cfg: cfg, logf: t.Logf, started: time.Now(), state: "Running"}
	d.fwd = newForwarder(nil, t.Logf)

	rec := httptest.NewRecorder()
	d.handleDiagnostics(rec, httptest.NewRequest("GET", "/api/diagnostics", nil))
	body := rec.Body.String()
	if cd := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") || !strings.Contains(cd, ".txt") {
		t.Errorf("Content-Disposition = %q", cd)
	}
	for _, want := range []string{"firmware:     13.42", "state:        Running", "main log line", `"passwordHash": "(set)"`, "===== launcher.log =====", "tailscale-debug.log"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, secret := range []string{"c2FsdA", "tskey-auth-secret", "someone@example.com", "00:11:22:aa:bb:cc"} {
		if strings.Contains(body, secret) {
			t.Errorf("%q is in the diagnostics", secret)
		}
	}
	// The daemon's own copy of the settings must not have been touched.
	if d.cfg.PasswordHash != cfg.PasswordHash || d.cfg.Wake[0].MAC != "00:11:22:aa:bb:cc" {
		t.Error("building the diagnostics changed the settings")
	}
}

func TestParseMAC(t *testing.T) {
	for in, want := range map[string]string{
		"00:11:22:AA:BB:CC":   "00:11:22:aa:bb:cc",
		"00-11-22-aa-bb-cc":   "00:11:22:aa:bb:cc",
		"001122AABBCC":        "00:11:22:aa:bb:cc",
		"0011.22aa.bbcc":      "00:11:22:aa:bb:cc",
		" 00:11:22:aa:bb:cc ": "00:11:22:aa:bb:cc",
	} {
		mac, err := parseMAC(in)
		if err != nil || mac.String() != want {
			t.Errorf("parseMAC(%q) = %v, %v", in, mac, err)
		}
	}
	for _, bad := range []string{"", "gaming-pc", "00:11:22:aa:bb", "00:11:22:aa:bb:cc:dd:ee", "zz:11:22:aa:bb:cc", "192.168.1.5"} {
		if _, err := parseMAC(bad); err == nil {
			t.Errorf("parseMAC(%q) accepted", bad)
		}
	}
}

func TestMagicPacket(t *testing.T) {
	mac, _ := parseMAC("00:11:22:aa:bb:cc")
	p := magicPacket(mac)
	if len(p) != 102 {
		t.Fatalf("length %d", len(p))
	}
	if !bytes.Equal(p[:6], bytes.Repeat([]byte{0xff}, 6)) {
		t.Error("the packet does not start with six 0xff")
	}
	for i := 0; i < 16; i++ {
		if !bytes.Equal(p[6+i*6:12+i*6], mac) {
			t.Fatalf("repetition %d is wrong", i)
		}
	}
}

func TestBroadcastAddrs(t *testing.T) {
	list := broadcastAddrs()
	if len(list) == 0 || !list[0].Equal(net.IPv4bcast) {
		t.Fatalf("got %v", list)
	}
	for _, ip := range list {
		if ip.To4() == nil || ip.IsLoopback() {
			t.Errorf("unexpected address %v", ip)
		}
	}
}

func TestWakeHandlers(t *testing.T) {
	d := &daemon{cfg: defaultConfig(), cfgPath: filepath.Join(t.TempDir(), "config.json"), logf: t.Logf}
	post := func(h http.HandlerFunc, url, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest("POST", url, strings.NewReader(body)))
		return rec
	}
	if rec := post(d.handleWakeList, "/api/wakelist", `[{"name":" gaming-pc ","mac":"00-11-22-AA-BB-CC"},{"name":"","mac":"001122aabbdd"}]`); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	want := []wakeTarget{{"gaming-pc", "00:11:22:aa:bb:cc"}, {"00:11:22:aa:bb:dd", "00:11:22:aa:bb:dd"}}
	if len(d.cfg.Wake) != 2 || d.cfg.Wake[0] != want[0] || d.cfg.Wake[1] != want[1] {
		t.Errorf("list = %+v", d.cfg.Wake)
	}
	if saved, err := loadConfig(d.cfgPath); err != nil || len(saved.Wake) != 2 {
		t.Errorf("saved: %+v, %v", saved.Wake, err)
	}
	if rec := post(d.handleWakeList, "/api/wakelist", `[{"name":"x","mac":"not a mac"}]`); rec.Code != http.StatusBadRequest {
		t.Errorf("a bad address: status %d", rec.Code)
	}
	// Only listed devices can be woken.
	if rec := post(d.handleWake, "/api/wake?mac=de:ad:be:ef:00:01", ""); rec.Code != http.StatusNotFound {
		t.Errorf("an unlisted device: status %d", rec.Code)
	}
	if rec := post(d.handleWake, "/api/wake?mac=nonsense", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("nonsense: status %d", rec.Code)
	}
}

func TestDescribeDialError(t *testing.T) {
	for msg, want := range map[string]string{
		"dial tcp 100.64.0.4:80: connect: connection refused": "nothing listens",
		"context deadline exceeded":                           "no answer",
		"dial tcp: i/o timeout":                               "no answer",
		"lookup nosuch: no such host":                         "no device with that name",
		"something else entirely":                             "something else entirely",
	} {
		if got := describeDialError(errors.New(msg)); !strings.Contains(got, want) {
			t.Errorf("%q -> %q", msg, got)
		}
	}
}

func TestTestTargetRefusesBadTargets(t *testing.T) {
	d := &daemon{logf: t.Logf}
	for _, target := range []string{"", "nas", "nas:0", ":80", "na s:80", "nas:99999"} {
		rec := httptest.NewRecorder()
		d.handleTestTarget(rec, httptest.NewRequest("POST", "/api/testtarget?target="+strings.ReplaceAll(target, " ", "%20"), nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%q: status %d", target, rec.Code)
		}
	}
}
