package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPasswordHash(t *testing.T) {
	hash, err := hashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "pbkdf2-sha256$") {
		t.Errorf("unexpected hash format %q", hash)
	}
	if !checkPassword(hash, "correct horse") {
		t.Error("the right password was rejected")
	}
	for _, wrong := range []string{"", "Correct horse", "correct horse "} {
		if checkPassword(hash, wrong) {
			t.Errorf("%q was accepted", wrong)
		}
	}
	other, _ := hashPassword("correct horse")
	if other == hash {
		t.Error("two hashes of one password are identical; the salt is not random")
	}
	for _, bad := range []string{"", "plain", "pbkdf2-sha256$x$00$00", "pbkdf2-sha256$1000$zz$00", "md5$1$00$00"} {
		if checkPassword(bad, "anything") {
			t.Errorf("malformed hash %q accepted a password", bad)
		}
	}
}

// newTestDaemon returns a daemon with just enough set up to serve the status
// page's API.
func newTestDaemon(t *testing.T) *daemon {
	t.Helper()
	dir := t.TempDir()
	old := dataDir
	dataDir = dir
	t.Cleanup(func() { dataDir = old })
	d := &daemon{
		cfg:     defaultConfig(),
		cfgPath: filepath.Join(dir, "config.json"),
		logf:    t.Logf,
		quit:    make(chan struct{}),
	}
	d.fwd = newForwarder(nil, t.Logf)
	d.tailnetWeb = newTailnetListener()
	return d
}

// request performs one API call. remote is the client address the server sees.
func request(t *testing.T, h http.Handler, method, path, remote string, body any, cookies []*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	r := httptest.NewRequest(method, path, &buf)
	r.RemoteAddr = remote
	r.Header.Set(apiHeader, "1")
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestPasswordProtection(t *testing.T) {
	d := newTestDaemon(t)
	h := d.webHandler()
	const lan, tailnet, console = "192.168.1.20:5000", "100.64.0.9:5000", "127.0.0.1:5000"

	// No password: everyone gets in.
	if w := request(t, h, "GET", "/api/status", lan, nil, nil); w.Code != 200 {
		t.Fatalf("no password, LAN status: %d", w.Code)
	}

	// Set one from the LAN.
	pw := "hunter22"
	w := request(t, h, "POST", "/api/config", lan, func() settings {
		s := settingsFromConfig(d.cfg)
		s.Password = &pw
		return s
	}(), nil)
	if w.Code != 200 {
		t.Fatalf("setting the password: %d %s", w.Code, w.Body)
	}
	setter := w.Result().Cookies()
	if d.cfg.PasswordHash == "" || strings.Contains(d.cfg.PasswordHash, pw) {
		t.Fatalf("stored password hash: %q", d.cfg.PasswordHash)
	}
	saved, _ := os.ReadFile(d.cfgPath)
	if !bytes.Contains(saved, []byte("passwordHash")) || bytes.Contains(saved, []byte(pw)) {
		t.Errorf("config file should hold the hash and not the password:\n%s", saved)
	}

	// Now locked for the LAN and the tailnet, open for the console itself
	// and for the browser that set it.
	for _, remote := range []string{lan, tailnet} {
		for _, path := range []string{"/api/status", "/api/config", "/api/logs", "/qr.png"} {
			if w := request(t, h, "GET", path, remote, nil, nil); w.Code != http.StatusUnauthorized {
				t.Errorf("GET %s from %s: %d, want 401", path, remote, w.Code)
			}
		}
		for _, path := range []string{"/api/logout", "/api/quit", "/api/uninstall", "/api/config", "/api/sunshine", "/api/login"} {
			if w := request(t, h, "POST", path, remote, nil, nil); w.Code != http.StatusUnauthorized {
				t.Errorf("POST %s from %s: %d, want 401", path, remote, w.Code)
			}
		}
	}
	if w := request(t, h, "GET", "/api/status", console, nil, nil); w.Code != 200 {
		t.Errorf("console status: %d", w.Code)
	}
	if w := request(t, h, "GET", "/api/status", lan, nil, setter); w.Code != 200 {
		t.Errorf("status with the session of the browser that set the password: %d", w.Code)
	}
	// The page shell and ping stay reachable so the unlock form can load.
	for _, path := range []string{"/", "/api/ping", "/favicon.png"} {
		if w := request(t, h, "GET", path, lan, nil, nil); w.Code != 200 {
			t.Errorf("GET %s while locked: %d", path, w.Code)
		}
	}

	// Unlocking.
	if w := request(t, h, "POST", "/api/auth", lan, map[string]string{"password": "nope"}, nil); w.Code != http.StatusForbidden {
		t.Errorf("wrong password: %d", w.Code)
	}
	w = request(t, h, "POST", "/api/auth", tailnet, map[string]string{"password": pw}, nil)
	if w.Code != 200 || len(w.Result().Cookies()) == 0 {
		t.Fatalf("right password: %d, cookies %v", w.Code, w.Result().Cookies())
	}
	session := w.Result().Cookies()
	if !session[0].HttpOnly {
		t.Error("the session cookie should be HttpOnly")
	}
	if w := request(t, h, "GET", "/api/status", tailnet, nil, session); w.Code != 200 {
		t.Errorf("status with a session: %d", w.Code)
	}

	// Locking again ends the session.
	request(t, h, "POST", "/api/lock", tailnet, nil, session)
	if w := request(t, h, "GET", "/api/status", tailnet, nil, session); w.Code != http.StatusUnauthorized {
		t.Errorf("status after lock: %d", w.Code)
	}

	// Removing the password opens the page again.
	empty := ""
	s := settingsFromConfig(d.cfg)
	s.Password = &empty
	if w := request(t, h, "POST", "/api/config", console, s, nil); w.Code != 200 {
		t.Fatalf("removing the password: %d %s", w.Code, w.Body)
	}
	if w := request(t, h, "GET", "/api/status", lan, nil, nil); w.Code != 200 {
		t.Errorf("status after removing the password: %d", w.Code)
	}
}

func TestStateChangesNeedHeader(t *testing.T) {
	d := newTestDaemon(t)
	h := d.webHandler()
	r := httptest.NewRequest("POST", "/api/quit", nil)
	r.RemoteAddr = "192.168.1.20:5000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("POST without the API header: %d, want 403", w.Code)
	}
	if d.stopping() {
		t.Error("the daemon was told to stop by a request without the header")
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	d := newTestDaemon(t)
	h := d.webHandler()
	const console = "127.0.0.1:5000"

	var s settings
	w := request(t, h, "GET", "/api/config", console, nil, nil)
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	if s.Hostname != "ps5" || s.Priority != priorityLow || !s.CheckUpdates || s.HTTPProxyAddr != "" || s.PasswordSet {
		t.Errorf("defaults: %+v", s)
	}

	s.Hostname = "living-room-ps5"
	s.BlockedPorts = []uint16{9021}
	s.UDPPorts = []uint16{9296}
	s.Priority = priorityHigh
	s.CheckUpdates = false
	w = request(t, h, "POST", "/api/config", console, s, nil)
	if w.Code != 200 {
		t.Fatalf("saving: %d %s", w.Code, w.Body)
	}
	var reply struct{ Restart []string }
	json.Unmarshal(w.Body.Bytes(), &reply)
	if len(reply.Restart) != 1 || reply.Restart[0] != "priority" {
		t.Errorf("settings needing a restart: %v, want [priority]", reply.Restart)
	}

	cfg, err := loadConfig(d.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hostname != "living-room-ps5" || cfg.Priority != priorityHigh || cfg.CheckUpdates ||
		len(cfg.BlockedPorts) != 1 || len(cfg.UDPPorts) != 1 {
		t.Errorf("saved config: %+v", cfg)
	}
	if b, _ := os.ReadFile(filepath.Join(dataDir, priorityFile)); strings.TrimSpace(string(b)) != priorityHigh {
		t.Errorf("priority file: %q", b)
	}

	// Back to low removes the launcher's instruction.
	s.Priority = priorityLow
	request(t, h, "POST", "/api/config", console, s, nil)
	if _, err := os.Stat(filepath.Join(dataDir, priorityFile)); !os.IsNotExist(err) {
		t.Errorf("priority file should be gone: %v", err)
	}

	// Invalid input is rejected and changes nothing.
	for name, edit := range map[string]func(*settings){
		"name":     func(s *settings) { s.Hostname = "bad name!" },
		"web":      func(s *settings) { s.WebAddr = "8090" },
		"proxy":    func(s *settings) { s.HTTPProxyAddr = "nonsense" },
		"priority": func(s *settings) { s.Priority = "turbo" },
		"forward":  func(s *settings) { s.Forwards = []forwardRule{{"sctp", "127.0.0.1:1", "a:1"}} },
		"sunshine": func(s *settings) { s.SunshineHosts = []sunshineHost{{Host: "a"}, {Host: "b"}} },
		"password": func(s *settings) { p := "abc"; s.Password = &p },
	} {
		bad := settingsFromConfig(d.cfg)
		edit(&bad)
		if w := request(t, h, "POST", "/api/config", console, bad, nil); w.Code != http.StatusBadRequest {
			t.Errorf("invalid %s: %d, want 400", name, w.Code)
		}
	}
	if d.cfg.Hostname != "living-room-ps5" {
		t.Errorf("hostname changed by a rejected request: %q", d.cfg.Hostname)
	}
}

func TestConfigMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	// A config as v0.4.1 wrote it.
	os.WriteFile(path, []byte(`{"hostname":"ps5","webAddr":":8090","httpProxyAddr":"127.0.0.1:8118","sunshineHost":"gaming-pc","udpPorts":[9295,9296,9297,9302]}`), 0o600)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.SunshineHosts) != 1 || cfg.SunshineHosts[0].Host != "gaming-pc" || cfg.SunshineHost != "" {
		t.Errorf("sunshine host not migrated: %+v", cfg)
	}
	if cfg.HTTPProxyAddr != "127.0.0.1:8118" {
		t.Errorf("an explicitly configured proxy must stay on: %q", cfg.HTTPProxyAddr)
	}
	if !cfg.CheckUpdates {
		t.Error("update checks should default to on for an existing config")
	}
}

func TestVersionCompare(t *testing.T) {
	for _, tt := range []struct {
		current, latest string
		want            bool
	}{
		{"0.4.1", "0.5.0", true},
		{"0.4.1", "v0.4.2", true},
		{"0.4.1", "0.4.1", false},
		{"0.5.0", "0.4.9", false},
		{"0.4.2-dev", "0.4.1", false},
		{"0.4.2-dev", "0.4.2", false},
		{"0.4.2-dev", "0.4.3", true},
		{"0.9.0", "0.10.0", true},
		{"dev", "0.5.0", false},
		{"0.4.1", "", false},
		{"0.4.1", "nonsense", false},
	} {
		if got := newerVersion(tt.current, tt.latest); got != tt.want {
			t.Errorf("newerVersion(%q, %q) = %v, want %v", tt.current, tt.latest, got, tt.want)
		}
	}
}

func TestIconReplyLine(t *testing.T) {
	for in, want := range map[string]string{
		"":                                  "",
		"[SceLncUtil] something\n":          "",
		"icon: remov":                       "",
		"[SceLncUtil] x\nicon: removed\n":   "icon: removed",
		"icon: registering failed: 0x1\r\n": "icon: registering failed: 0x1",
	} {
		if got := iconReplyLine([]byte(in)); got != want {
			t.Errorf("iconReplyLine(%q) = %q, want %q", in, got, want)
		}
	}
}
