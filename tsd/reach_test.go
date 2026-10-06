package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"tailscale.com/ipn/ipnstate"
)

func TestValidateForwards(t *testing.T) {
	ok := []forwardRule{
		{"tcp", "127.0.0.1:8096", "my-nas:8096"},
		{"udp", "127.0.0.1:8096", "my-nas:8096"}, // same port, other protocol
		{"tcp", "127.0.0.1:8080", "100.64.0.4:80"},
		{"tcp", "127.0.0.1:8443", "[fd7a:115c:a1e0::4]:443"},
	}
	if err := validateForwards(ok); err != nil {
		t.Errorf("good rules refused: %v", err)
	}
	for name, rules := range map[string][]forwardRule{
		"unknown protocol":      {{"icmp", "127.0.0.1:1", "a:1"}},
		"no port on the device": {{"tcp", "127.0.0.1:8096", "my-nas"}},
		"no device":             {{"tcp", "127.0.0.1:8096", ":8096"}},
		"bad local address":     {{"tcp", "8096", "my-nas:8096"}},
		"port 0":                {{"tcp", "127.0.0.1:8096", "my-nas:0"}},
		"same local port twice": {{"tcp", "127.0.0.1:8096", "a:1"}, {"tcp", "127.0.0.1:8096", "b:2"}},
		"odd characters":        {{"tcp", "127.0.0.1:8096", "my nas;rm:80"}},
	} {
		if err := validateForwards(rules); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestHandleForwards(t *testing.T) {
	dir := t.TempDir()
	d := &daemon{cfg: defaultConfig(), cfgPath: filepath.Join(dir, "config.json"), logf: t.Logf}
	d.fwd = newForwarder(nil, t.Logf)
	free := freePort(t)

	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		d.handleForwards(rec, httptest.NewRequest("POST", "/api/forwards", strings.NewReader(body)))
		return rec
	}
	body, _ := json.Marshal([]forwardRule{{" TCP ", free, " my-nas:8096 "}})
	if rec := post(string(body)); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if len(d.cfg.Forwards) != 1 || d.cfg.Forwards[0] != (forwardRule{"tcp", free, "my-nas:8096"}) {
		t.Errorf("config = %+v", d.cfg.Forwards)
	}
	if got := d.fwd.rules(); len(got) != 1 {
		t.Errorf("active forwards = %v", got)
	}
	saved, err := loadConfig(d.cfgPath)
	if err != nil || len(saved.Forwards) != 1 {
		t.Errorf("saved config: %+v, %v", saved.Forwards, err)
	}

	if rec := post(`[{"proto":"tcp","listen":"x","target":"y"}]`); rec.Code != http.StatusBadRequest {
		t.Errorf("a bad rule: status %d", rec.Code)
	}
	if len(d.cfg.Forwards) != 1 {
		t.Error("a refused request changed the config")
	}
	// An empty list removes them all.
	if rec := post(`[]`); rec.Code != http.StatusOK || len(d.cfg.Forwards) != 0 || len(d.fwd.rules()) != 0 {
		t.Errorf("clearing: status %d, %v", rec.Code, d.cfg.Forwards)
	}
	d.fwd.set(nil)
}

// The settings form no longer carries the forwards; saving it must not
// wipe them.
func TestSettingsLeaveForwardsAlone(t *testing.T) {
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.Forwards = []forwardRule{{"tcp", freePort(t), "my-nas:8096"}}
	d := &daemon{cfg: cfg, cfgPath: filepath.Join(dir, "config.json"), logf: t.Logf}
	d.fwd = newForwarder(nil, t.Logf)
	defer d.fwd.set(nil)

	s := settingsFromConfig(cfg)
	s.Forwards = nil
	s.SunshineHosts = nil
	body, _ := json.Marshal(s)
	rec := httptest.NewRecorder()
	d.handleSetConfig(rec, httptest.NewRequest("POST", "/api/config", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if len(d.cfg.Forwards) != 1 {
		t.Errorf("the forwards were lost: %+v", d.cfg.Forwards)
	}
}

func TestPeerConn(t *testing.T) {
	for name, tt := range map[string]struct {
		p         ipnstate.PeerStatus
		conn, via string
	}{
		"direct":           {ipnstate.PeerStatus{Online: true, Active: true, CurAddr: "192.168.1.5:41641", Relay: "nyc"}, "direct", ""},
		"relayed":          {ipnstate.PeerStatus{Online: true, Active: true, Relay: "nyc"}, "relay", "nyc"},
		"peer relay":       {ipnstate.PeerStatus{Online: true, Active: true, PeerRelay: "1.2.3.4:5:6", Relay: "nyc"}, "relay", "a peer relay"},
		"no traffic":       {ipnstate.PeerStatus{Online: true, Relay: "nyc"}, "", ""},
		"offline":          {ipnstate.PeerStatus{Active: true, CurAddr: "192.168.1.5:41641"}, "", ""},
		"active, no route": {ipnstate.PeerStatus{Online: true, Active: true}, "", ""},
	} {
		if conn, via := peerConn(&tt.p); conn != tt.conn || via != tt.via {
			t.Errorf("%s: got %q %q", name, conn, via)
		}
	}
}

func TestPingPeerRefusesOtherAddresses(t *testing.T) {
	d := &daemon{logf: t.Logf}
	for _, ip := range []string{"", "8.8.8.8", "192.168.1.1", "not-an-ip", "127.0.0.1"} {
		rec := httptest.NewRecorder()
		d.handlePingPeer(rec, httptest.NewRequest("POST", "/api/pingpeer?ip="+ip, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%q: status %d", ip, rec.Code)
		}
	}
}

func TestDNSDescribe(t *testing.T) {
	p := &dnsPicker{}
	if got := p.describe(); got != "" {
		t.Errorf("before any lookup: %q", got)
	}
	for server, want := range map[string]string{
		dnsLocal:         "DNS payload",
		"192.168.1.1:53": "router",
		"1.1.1.1:53":     "public",
	} {
		p.server, p.working = server, true
		if got := p.describe(); !strings.Contains(got, want) {
			t.Errorf("%s: %q", server, got)
		}
	}
	p.working = false
	if got := p.describe(); !strings.Contains(got, "no server") {
		t.Errorf("not working: %q", got)
	}
}

func TestCheckForwardPorts(t *testing.T) {
	// Something else listening on the console.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	busy := ln.Addr().String()
	free := freePort(t)

	cfg := defaultConfig()
	cfg.SunshineHosts = []sunshineHost{{Host: "gaming-pc"}}
	d := &daemon{cfg: cfg, logf: t.Logf, webPort: 8090, proxyPort: 8118}
	d.fwd = newForwarder(nil, t.Logf)
	defer d.fwd.set(nil)

	for name, tt := range map[string]struct {
		rule forwardRule
		ok   bool
	}{
		"a free port":               {forwardRule{"tcp", free, "nas:80"}, true},
		"the status page's port":    {forwardRule{"tcp", "127.0.0.1:8090", "nas:80"}, false},
		"the proxy's port":          {forwardRule{"tcp", "127.0.0.1:8118", "nas:80"}, false},
		"a game streaming port":     {forwardRule{"tcp", "127.0.0.1:47989", "nas:80"}, false},
		"a game streaming UDP port": {forwardRule{"udp", "127.0.0.1:47998", "nas:80"}, false},
		"UDP on the page's port":    {forwardRule{"udp", "127.0.0.1:8090", "nas:80"}, true},
		"a port something else has": {forwardRule{"tcp", busy, "nas:80"}, false},
	} {
		rule, err := d.checkForwardPorts([]forwardRule{tt.rule})
		if (err == nil) != tt.ok {
			t.Errorf("%s: %v", name, err)
		}
		if err != nil && rule != tt.rule {
			t.Errorf("%s: blamed %v", name, rule)
		}
	}

	// A forward that is already running may stay, and may be pointed
	// somewhere else, although its port is of course in use: by us.
	running := forwardRule{"tcp", free, "nas:80"}
	if err := d.fwd.set([]forwardRule{running}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.checkForwardPorts([]forwardRule{running}); err != nil {
		t.Errorf("a running forward was refused: %v", err)
	}
	if _, err := d.checkForwardPorts([]forwardRule{{"tcp", free, "laptop:8080"}}); err != nil {
		t.Errorf("retargeting a running forward was refused: %v", err)
	}
}
