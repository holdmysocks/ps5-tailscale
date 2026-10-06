package main

import (
	_ "embed"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

//go:embed status.html
var statusHTML []byte

// faviconPNG is the logo from the home screen icon (appicon/icon0.png)
// without its text, 128x128, for the browser tab.
//
//go:embed favicon.png
var faviconPNG []byte

// State-changing requests must carry this header, which a web page on
// another origin cannot send, so a stray link or image tag cannot log the
// console out. Who may use the page at all is decided in auth.go.
const apiHeader = "X-PS5-Tailscale"

// sunshineInfo is a forwarded Sunshine host as the status page shows it.
type sunshineInfo struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	// Address is what to enter in a Moonlight client on the console.
	Address string `json:"address"`
}

type statusInfo struct {
	Version  string     `json:"version"`
	State    string     `json:"state"`
	AuthURL  string     `json:"authURL,omitempty"`
	Error    string     `json:"error,omitempty"`
	Hostname string     `json:"hostname"`
	DNSName  string     `json:"dnsName,omitempty"`
	IPs      []string   `json:"ips"`
	Tailnet  string     `json:"tailnet,omitempty"`
	Health   []string   `json:"health,omitempty"`
	Peers    []peerInfo `json:"peers"`
	// VPNServers counts the exit servers of a VPN add-on. They are only in
	// Peers when the page asks for them (?vpn=1).
	VPNServers peerCount `json:"vpnServers"`
	Proxy      string    `json:"proxy,omitempty"`
	// SunshineHosts and Forwards describe the local forwards.
	SunshineHosts []sunshineInfo `json:"sunshineHosts"`
	Forwards      []string       `json:"forwards"`
	// UDPPorts are the console's UDP ports reachable from the tailnet.
	UDPPorts []uint16 `json:"udpPorts"`
	Priority string   `json:"priority"`
	// PasswordSet says whether the page is password protected.
	PasswordSet bool `json:"passwordSet"`
	// AllowFrom is "all" or "own": which tailnet devices may connect.
	AllowFrom string `json:"allowFrom"`
	// KeyExpiry is when this console's Tailscale key expires, if it does.
	KeyExpiry *time.Time `json:"keyExpiry,omitempty"`
	// LatestVersion and UpdateURL are set when a newer release exists.
	LatestVersion string `json:"latestVersion,omitempty"`
	UpdateURL     string `json:"updateURL,omitempty"`
	// CanUpdate says that the newer release can be installed from the page;
	// Update reports on an installation in progress.
	CanUpdate bool `json:"canUpdate"`
	// PayloadPath is the copy an update replaces as well, if one is set.
	PayloadPath string         `json:"payloadPath,omitempty"`
	Update      updateProgress `json:"update"`
	Uptime      int64          `json:"uptimeSeconds"`
}

// webHandler builds the status page and its API.
func (d *daemon) webHandler() http.Handler {
	mux := http.NewServeMux()

	// Open to everyone who can reach the page: the page itself (which shows
	// nothing until its API answers), the icon, and what a new instance
	// needs to recognise this one.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(statusHTML)
	})
	favicon := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "max-age=86400")
		w.Write(faviconPNG)
	}
	mux.HandleFunc("GET /favicon.png", favicon)
	mux.HandleFunc("GET /favicon.ico", favicon) // what browsers ask for unprompted
	mux.HandleFunc("GET /api/ping", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ps5-tailscale "+version+"\n")
	})
	mux.HandleFunc("POST /api/auth", d.guard(d.handleAuth))
	mux.HandleFunc("POST /api/lock", d.guard(d.handleLock))

	// Everything else needs the password, if one is set.
	mux.HandleFunc("GET /api/status", d.protect(d.handleStatus))
	mux.HandleFunc("GET /api/logs", d.protect(d.handleLogs))
	mux.HandleFunc("GET /qr.png", d.protect(d.handleQR))
	mux.HandleFunc("GET /api/config", d.protect(d.handleGetConfig))
	mux.HandleFunc("GET /api/files", d.protect(d.handleFiles))
	mux.HandleFunc("GET /api/files/get", d.protect(d.handleFileGet))
	for path, h := range map[string]http.HandlerFunc{
		"/api/config":    d.handleSetConfig,
		"/api/login":     d.handleLogin,
		"/api/logout":    d.handleLogout,
		"/api/quit":      d.handleQuit,
		"/api/uninstall": d.handleUninstall,
		"/api/sunshine":  d.handleSunshine,
		"/api/update":    d.handleUpdate,
	} {
		mux.HandleFunc("POST "+path, d.protect(d.guard(h)))
	}
	return mux
}

// serveWeb serves the status page on one listener.
func (d *daemon) serveWeb(ln net.Listener, h http.Handler) {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed && !d.stopping() {
		d.logf("web UI stopped: %v", err)
	}
}

// stopping reports whether a shutdown has been requested.
func (d *daemon) stopping() bool {
	select {
	case <-d.quit:
		return true
	default:
		return false
	}
}

// writeFileTail copies the last max bytes of a file to w.
func writeFileTail(w io.Writer, path string, max int64) {
	f, err := os.Open(path)
	if err != nil {
		io.WriteString(w, err.Error()+"\n")
		return
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > max {
		f.Seek(fi.Size()-max, io.SeekStart)
	}
	io.Copy(w, onlyReader{f}) // no sendfile, see sendToLoader
}

func (d *daemon) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(apiHeader) == "" {
			http.Error(w, "missing "+apiHeader+" header", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

func (d *daemon) handleLogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	switch {
	case r.URL.Query().Get("full") == "1":
		// The end of the log file itself.
		writeFileTail(w, filepath.Join(dataDir, "tailscale.log"), 512<<10)
	case r.URL.Query().Get("debug") == "1":
		// The end of the debug log, which includes Tailscale's own messages.
		writeFileTail(w, filepath.Join(dataDir, "tailscale-debug.log"), 1<<20)
	case r.URL.Query().Get("debug") == "old":
		writeFileTail(w, filepath.Join(dataDir, "tailscale-debug.log.old"), 1<<20)
	default:
		io.WriteString(w, strings.Join(recentLogs.snapshot(), "\n")+"\n")
	}
}

func (d *daemon) handleStatus(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	info := statusInfo{
		Version:       version,
		State:         d.state,
		AuthURL:       d.authURL,
		Error:         d.lastErr,
		Hostname:      d.cfg.Hostname,
		Proxy:         d.cfg.HTTPProxyAddr,
		Priority:      priorityLow,
		AllowFrom:     accessAll,
		PasswordSet:   d.cfg.PasswordHash != "",
		Uptime:        int64(time.Since(d.started).Seconds()),
		IPs:           []string{},
		Peers:         []peerInfo{},
		SunshineHosts: []sunshineInfo{},
	}
	if d.cfg.Priority == priorityHigh {
		info.Priority = priorityHigh
	}
	if d.cfg.AllowFrom == accessOwn {
		info.AllowFrom = accessOwn
	}
	for _, h := range d.cfg.SunshineHosts {
		info.SunshineHosts = append(info.SunshineHosts, sunshineInfo{Host: h.Host, Port: h.basePort(), Address: h.clientAddress()})
	}
	if newerVersion(version, d.latest.Version) {
		info.LatestVersion, info.UpdateURL = d.latest.Version, d.latest.URL
		info.CanUpdate = canInstall(d.latest)
	}
	info.Update = d.update
	info.PayloadPath = d.cfg.PayloadPath
	d.mu.Unlock()
	info.UDPPorts = []uint16{}
	if d.udp != nil {
		info.UDPPorts = append(info.UDPPorts, d.udp.activePorts()...)
	}
	info.Forwards = []string{}
	for _, r := range d.fwd.rules() {
		info.Forwards = append(info.Forwards, r.String())
	}
	if info.State == "" {
		info.State = "Starting"
	}

	if d.lc != nil {
		if st, err := d.status(r.Context()); err == nil {
			info.State = st.BackendState
			info.Health = st.Health
			if info.AuthURL == "" {
				info.AuthURL = st.AuthURL
			}
			if st.CurrentTailnet != nil {
				info.Tailnet = st.CurrentTailnet.Name
			}
			if st.Self != nil {
				info.DNSName = strings.TrimSuffix(st.Self.DNSName, ".")
				for _, ip := range st.Self.TailscaleIPs {
					info.IPs = append(info.IPs, ip.String())
				}
				info.KeyExpiry = st.Self.KeyExpiry
			}
			info.Peers, info.VPNServers = peersFromStatus(st, r.URL.Query().Get("vpn") == "1")
		}
	}
	if info.State == "Running" {
		info.AuthURL = ""
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(info)
}

// handleQR renders the current login URL as a QR code. It only ever encodes
// the URL the daemon holds, never one supplied by the request.
func (d *daemon) handleQR(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	u := d.authURL
	d.mu.Unlock()
	if u == "" {
		http.NotFound(w, r)
		return
	}
	png, err := qrcode.Encode(u, qrcode.Medium, 320)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(png)
}

func (d *daemon) handleLogin(w http.ResponseWriter, r *http.Request) {
	if d.lc == nil {
		http.Error(w, "tailscale is still starting", http.StatusServiceUnavailable)
		return
	}
	d.mu.Lock()
	loggedOut := d.state == "NeedsLogin"
	d.mu.Unlock()
	var err error
	if loggedOut {
		// The pending link may be used up or expired; get a new one.
		err = d.freshLogin(r.Context())
	} else {
		err = d.lc.StartLoginInteractive(r.Context())
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	io.WriteString(w, "ok\n")
}

func (d *daemon) handleLogout(w http.ResponseWriter, r *http.Request) {
	if d.lc == nil {
		http.Error(w, "tailscale is still starting", http.StatusServiceUnavailable)
		return
	}
	if err := d.lc.Logout(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d.mu.Lock()
	d.notified = ""
	d.mu.Unlock()
	d.logf("logged out via the status page")
	io.WriteString(w, "ok\n")
}

// handleSunshine replaces the list of Sunshine hosts whose streaming ports are
// forwarded from 127.0.0.1, saves the config and applies it at once.
func (d *daemon) handleSunshine(w http.ResponseWriter, r *http.Request) {
	var hosts []sunshineHost
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&hosts); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	for i := range hosts {
		hosts[i].Host = strings.TrimSpace(hosts[i].Host)
		if hosts[i].Port == sunshineDefaultPort {
			hosts[i].Port = 0
		}
	}
	if err := validateSunshineHosts(hosts); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	d.mu.Lock()
	d.cfg.SunshineHosts = hosts
	cfg := d.cfg
	d.mu.Unlock()
	if err := saveConfig(d.cfgPath, cfg); err != nil {
		d.logf("saving config: %v", err)
	}
	d.logf("sunshine hosts set to %v", hosts)
	if err := d.fwd.set(d.localForwardRules()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	io.WriteString(w, "ok\n")
}

// validHostName accepts an empty string, a DNS name or an IP address.
func validHostName(s string) bool {
	if len(s) > 253 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '-', c == ':':
		default:
			return false
		}
	}
	return true
}

func (d *daemon) handleQuit(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, "stopping\n")
	d.stop()
}

func (d *daemon) stop() {
	d.quitOnce.Do(func() { close(d.quit) })
}

// handleUninstall takes the home screen icon away, logs the console out of
// the tailnet and stops the daemon, which deletes its data directory (login,
// settings, logs) on the way out. The payload file itself is wherever the
// user keeps it.
func (d *daemon) handleUninstall(w http.ResponseWriter, r *http.Request) {
	d.logf("uninstall requested from the status page")
	iconNote := "The home screen icon was removed."
	if err := removeHomeIcon(); err != nil {
		d.logf("uninstall: home screen icon: %v", err)
		iconNote = "The home screen icon could not be removed (" + err.Error() + "); delete it from the home screen."
	}
	if d.lc != nil {
		if err := d.lc.Logout(r.Context()); err != nil {
			d.logf("uninstall: logout: %v", err)
		}
	}
	d.mu.Lock()
	d.removeDataOnExit = true
	d.mu.Unlock()
	notify("Tailscale was removed from this PS5.")
	io.WriteString(w, "Tailscale was removed from this PS5. "+iconNote+"\n")
	d.stop()
}

// stopRunningInstance asks an instance that is already serving the status
// page to exit and waits for the port to become free. It reports whether
// there was one. The request comes from the console itself, so it needs no
// password.
func stopRunningInstance(webAddr string) bool {
	_, port, err := net.SplitHostPort(webAddr)
	if err != nil {
		return false
	}
	base := "http://" + net.JoinHostPort("127.0.0.1", port)
	client := &http.Client{Timeout: 3 * time.Second}

	resp, err := client.Get(base + "/api/ping")
	if err != nil {
		return false
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 100))
	resp.Body.Close()
	if !strings.HasPrefix(string(body), "ps5-tailscale") {
		return false
	}

	req, _ := http.NewRequest("POST", base+"/api/quit", nil)
	req.Header.Set(apiHeader, "1")
	if resp, err := client.Do(req); err == nil {
		resp.Body.Close()
	}
	for i := 0; i < 40; i++ {
		c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", port), time.Second)
		if err != nil {
			return true
		}
		c.Close()
		time.Sleep(250 * time.Millisecond)
	}
	return true
}

// handleUpdate starts installing the newest release.
func (d *daemon) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if err := d.startUpdate(); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	d.logf("update requested from the status page")
	io.WriteString(w, "The update has started.\n")
}
