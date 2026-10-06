package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"tailscale.com/ipn"
)

// Settings editing from the status page. Most settings take effect at once;
// the few that are only read when the payload starts are reported back so
// the page can say so.

const (
	priorityLow  = "low"
	priorityHigh = "high"
	// priorityFile tells the launcher which scheduling class to use. The
	// launcher is C and runs before any of this, so it gets the one setting
	// it needs in a file of its own rather than parsing the config.
	priorityFile = "priority"
)

// settings is the editable part of the config as the status page sees it.
type settings struct {
	Hostname      string         `json:"hostname"`
	WebAddr       string         `json:"webAddr"`
	HTTPProxyAddr string         `json:"httpProxyAddr"`
	SunshineHosts []sunshineHost `json:"sunshineHosts"`
	Forwards      []forwardRule  `json:"forwards"`
	UDPPorts      []uint16       `json:"udpPorts"`
	BlockedPorts  []uint16       `json:"blockedPorts"`
	Priority      string         `json:"priority"`
	AllowFrom     string         `json:"allowFrom"`
	PayloadPath   string         `json:"payloadPath"`
	ReceiveDir    string         `json:"receiveDir"`
	CheckUpdates  bool           `json:"checkUpdates"`
	Verbose       bool           `json:"verbose"`

	// PasswordSet says whether a password is in place. Password is only
	// read: absent leaves the password alone, empty removes it, anything
	// else sets it.
	PasswordSet bool    `json:"passwordSet"`
	Password    *string `json:"password,omitempty"`
}

func settingsFromConfig(cfg config) settings {
	s := settings{
		Hostname:      cfg.Hostname,
		WebAddr:       cfg.WebAddr,
		HTTPProxyAddr: cfg.HTTPProxyAddr,
		SunshineHosts: append([]sunshineHost{}, cfg.SunshineHosts...),
		Forwards:      append([]forwardRule{}, cfg.Forwards...),
		UDPPorts:      append([]uint16{}, cfg.UDPPorts...),
		BlockedPorts:  append([]uint16{}, cfg.BlockedPorts...),
		Priority:      priorityLow,
		AllowFrom:     accessAll,
		PayloadPath:   cfg.PayloadPath,
		ReceiveDir:    cfg.ReceiveDir,
		CheckUpdates:  cfg.CheckUpdates,
		Verbose:       cfg.Verbose,
		PasswordSet:   cfg.PasswordHash != "",
	}
	if cfg.Priority == priorityHigh {
		s.Priority = priorityHigh
	}
	if cfg.AllowFrom == accessOwn {
		s.AllowFrom = accessOwn
	}
	return s
}

// validate checks the settings and tidies them.
func (s *settings) validate() error {
	s.Hostname = strings.TrimSpace(s.Hostname)
	if !validTailnetName(s.Hostname) {
		return fmt.Errorf("the name may only contain letters, digits and hyphens (at most 63)")
	}
	if err := validListenAddr(s.WebAddr); err != nil {
		return fmt.Errorf("status page address: %w", err)
	}
	s.HTTPProxyAddr = strings.TrimSpace(s.HTTPProxyAddr)
	if s.HTTPProxyAddr != "" {
		if err := validListenAddr(s.HTTPProxyAddr); err != nil {
			return fmt.Errorf("HTTP proxy address: %w", err)
		}
	}
	if err := validateSunshineHosts(s.SunshineHosts); err != nil {
		return err
	}
	if err := validateForwards(s.Forwards); err != nil {
		return err
	}
	if slices.Contains(s.UDPPorts, 0) || slices.Contains(s.BlockedPorts, 0) {
		return fmt.Errorf("0 is not a port")
	}
	s.PayloadPath = strings.TrimSpace(s.PayloadPath)
	if err := validPayloadPath(s.PayloadPath); err != nil {
		return fmt.Errorf("payload file: %w", err)
	}
	s.ReceiveDir = strings.TrimSpace(s.ReceiveDir)
	if s.ReceiveDir == "" {
		s.ReceiveDir = defaultReceiveDir
	}
	if !strings.HasPrefix(s.ReceiveDir, "/") || path.Clean(s.ReceiveDir) != s.ReceiveDir || s.ReceiveDir == "/" {
		return fmt.Errorf("folder for received files: it must be a full path, such as %s", defaultReceiveDir)
	}
	if s.AllowFrom == "" {
		s.AllowFrom = accessAll
	}
	if s.AllowFrom != accessAll && s.AllowFrom != accessOwn {
		return fmt.Errorf("who may connect must be all or own")
	}
	if s.Priority != priorityLow && s.Priority != priorityHigh {
		return fmt.Errorf("the priority must be low or high")
	}
	if s.Password != nil && len(*s.Password) > 0 && len(*s.Password) < 4 {
		return fmt.Errorf("the password must be at least 4 characters")
	}
	return nil
}

// validateForwards checks a list of local forwards.
func validateForwards(rules []forwardRule) error {
	seen := map[string]bool{}
	for _, f := range rules {
		if f.Proto != "tcp" && f.Proto != "udp" {
			return fmt.Errorf("forward %v: the protocol must be tcp or udp", f)
		}
		if err := validListenAddr(f.Listen); err != nil {
			return fmt.Errorf("forward %v: listen address: %w", f, err)
		}
		host, port, err := net.SplitHostPort(f.Target)
		if err != nil || host == "" || !validHostName(host) || !validPort(port) {
			return fmt.Errorf("forward %v: the target must be a device and a port", f)
		}
		_, lport, _ := net.SplitHostPort(f.Listen)
		if key := f.Proto + " " + lport; seen[key] {
			return fmt.Errorf("two forwards use %s port %s on the console", f.Proto, lport)
		} else {
			seen[key] = true
		}
	}
	return nil
}

func validTailnetName(s string) bool {
	if len(s) == 0 || len(s) > 63 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func validPort(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n >= 1 && n <= 65535
}

// validListenAddr accepts "host:port" and ":port".
func validListenAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q is not host:port", addr)
	}
	if !validHostName(host) || !validPort(port) {
		return fmt.Errorf("%q is not a valid address", addr)
	}
	return nil
}

func (d *daemon) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	s := settingsFromConfig(d.cfg)
	d.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(s)
}

// handleSetConfig saves new settings and applies what can be applied without
// a restart. The reply lists the settings that need one.
func (d *daemon) handleSetConfig(w http.ResponseWriter, r *http.Request) {
	var s settings
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&s); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var newHash *string
	if s.Password != nil {
		hash := ""
		if *s.Password != "" {
			var err error
			if hash, err = hashPassword(*s.Password); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		newHash = &hash
	}

	d.mu.Lock()
	old := d.cfg
	cfg := d.cfg
	cfg.Hostname = s.Hostname
	cfg.WebAddr = s.WebAddr
	cfg.HTTPProxyAddr = s.HTTPProxyAddr
	if s.SunshineHosts != nil {
		// The settings form leaves the Sunshine hosts out: they have a
		// panel of their own.
		cfg.SunshineHosts = s.SunshineHosts
	}
	if s.Forwards != nil {
		// Like the Sunshine hosts, the forwards have a panel of their own
		// and are left alone when the settings form does not send them.
		cfg.Forwards = s.Forwards
	}
	cfg.UDPPorts = s.UDPPorts
	cfg.BlockedPorts = s.BlockedPorts
	cfg.Priority = ""
	if s.Priority == priorityHigh {
		cfg.Priority = priorityHigh
	}
	cfg.PayloadPath = s.PayloadPath
	cfg.ReceiveDir = s.ReceiveDir
	cfg.AllowFrom = ""
	if s.AllowFrom == accessOwn {
		cfg.AllowFrom = accessOwn
	}
	cfg.CheckUpdates = s.CheckUpdates
	cfg.Verbose = s.Verbose
	if newHash != nil {
		cfg.PasswordHash = *newHash
	}
	d.cfg = cfg
	d.mu.Unlock()

	if err := saveConfig(d.cfgPath, cfg); err != nil {
		d.logf("saving config: %v", err)
		http.Error(w, "the settings are in effect but could not be saved: "+err.Error(), http.StatusInternalServerError)
		return
	}
	d.logf("settings changed from the status page")

	// Apply.
	problems := []string{}
	if cfg.Hostname != old.Hostname && d.lc != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		_, err := d.lc.EditPrefs(ctx, &ipn.MaskedPrefs{Prefs: ipn.Prefs{Hostname: cfg.Hostname}, HostnameSet: true})
		cancel()
		if err != nil {
			problems = append(problems, "name: "+err.Error())
		}
	}
	if err := d.fwd.set(d.localForwardRules()); err != nil {
		problems = append(problems, err.Error())
	}
	if cfg.HTTPProxyAddr != old.HTTPProxyAddr {
		if err := d.setProxy(cfg.HTTPProxyAddr); err != nil {
			problems = append(problems, "HTTP proxy: "+err.Error())
		}
	}
	if newHash != nil && cfg.PasswordHash != old.PasswordHash {
		// A changed password ends every session but the one that changed it.
		d.sessions.clear()
		if cfg.PasswordHash != "" {
			if token, err := d.sessions.create(); err == nil {
				http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/",
					MaxAge: int(sessionLifetime.Seconds()), HttpOnly: true, SameSite: http.SameSiteStrictMode})
			}
		}
	}
	d.writePriorityFile()
	d.access.clear()
	// UDP ports and blocked ports are read from the config where they are used.

	restart := []string{}
	if cfg.WebAddr != old.WebAddr {
		restart = append(restart, "status page address")
	}
	if cfg.Priority != old.Priority {
		restart = append(restart, "priority")
	}
	if cfg.Verbose != old.Verbose {
		restart = append(restart, "verbose log")
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"restart": restart, "problems": problems})
}

// writePriorityFile leaves the launcher its instruction for the next start.
func (d *daemon) writePriorityFile() {
	d.mu.Lock()
	high := d.cfg.Priority == priorityHigh
	d.mu.Unlock()
	path := filepath.Join(dataDir, priorityFile)
	if high {
		os.WriteFile(path, []byte(priorityHigh+"\n"), 0o644)
	} else {
		os.Remove(path)
	}
}

// setProxy starts, stops or moves the outbound HTTP proxy.
func (d *daemon) setProxy(addr string) error {
	d.mu.Lock()
	old := d.proxyLn
	d.proxyLn, d.proxyPort = nil, 0
	d.mu.Unlock()
	if old != nil {
		old.Close()
	}
	if addr == "" {
		return nil
	}
	ln, err := listenResilient("tcp", addr, d.logf)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.proxyLn, d.proxyPort = ln, ln.port()
	d.mu.Unlock()
	go d.serveProxy(ln)
	return nil
}
