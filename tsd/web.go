package main

import (
	_ "embed"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

//go:embed status.html
var statusHTML []byte

// The status page has no login: like the other services on a jailbroken
// console it trusts the local network. State-changing requests must carry
// this header, which a web page on another origin cannot send, so a stray
// link or image tag cannot log the console out.
const apiHeader = "X-PS5-Tailscale"

type peerInfo struct {
	Name   string `json:"name"`
	IP     string `json:"ip"`
	OS     string `json:"os"`
	Online bool   `json:"online"`
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
	Proxy    string     `json:"proxy,omitempty"`
	// SunshineHost and Forwards describe the local forwards.
	SunshineHost string   `json:"sunshineHost"`
	Forwards     []string `json:"forwards"`
	// UDPPorts are the console's UDP ports reachable from the tailnet.
	UDPPorts []uint16 `json:"udpPorts"`
	Uptime   int64    `json:"uptimeSeconds"`
}

func (d *daemon) serveWeb(ln net.Listener) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(statusHTML)
	})
	mux.HandleFunc("GET /api/ping", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ps5-tailscale "+version+"\n")
	})
	mux.HandleFunc("GET /api/status", d.handleStatus)
	mux.HandleFunc("GET /api/logs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		switch {
		case r.URL.Query().Get("full") == "1":
			// The end of the log file itself.
			writeFileTail(w, filepath.Join(dataDir, "tailscale.log"), 512<<10)
			return
		case r.URL.Query().Get("debug") == "1":
			// The end of the debug log, which includes Tailscale's own messages.
			writeFileTail(w, filepath.Join(dataDir, "tailscale-debug.log"), 1<<20)
			return
		case r.URL.Query().Get("debug") == "old":
			writeFileTail(w, filepath.Join(dataDir, "tailscale-debug.log.old"), 1<<20)
			return
		}
		io.WriteString(w, strings.Join(recentLogs.snapshot(), "\n")+"\n")
	})
	mux.HandleFunc("GET /qr.png", d.handleQR)
	mux.HandleFunc("POST /api/login", d.guard(d.handleLogin))
	mux.HandleFunc("POST /api/logout", d.guard(d.handleLogout))
	mux.HandleFunc("POST /api/quit", d.guard(d.handleQuit))
	mux.HandleFunc("POST /api/uninstall", d.guard(d.handleUninstall))
	mux.HandleFunc("POST /api/sunshine", d.guard(d.handleSunshine))

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
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
	io.Copy(w, f)
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

func (d *daemon) handleStatus(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	info := statusInfo{
		Version:  version,
		State:    d.state,
		AuthURL:  d.authURL,
		Error:    d.lastErr,
		Hostname: d.cfg.Hostname,
		Proxy:    d.cfg.HTTPProxyAddr,
		Uptime:   int64(time.Since(d.started).Seconds()),
		IPs:      []string{},
		Peers:    []peerInfo{},
	}
	info.SunshineHost = d.cfg.SunshineHost
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
			}
			for _, p := range st.Peer {
				pi := peerInfo{Name: p.HostName, OS: p.OS, Online: p.Online}
				if p.DNSName != "" {
					pi.Name = strings.SplitN(p.DNSName, ".", 2)[0]
				}
				if len(p.TailscaleIPs) > 0 {
					pi.IP = p.TailscaleIPs[0].String()
				}
				info.Peers = append(info.Peers, pi)
			}
			sort.Slice(info.Peers, func(i, j int) bool {
				if info.Peers[i].Online != info.Peers[j].Online {
					return info.Peers[i].Online
				}
				return info.Peers[i].Name < info.Peers[j].Name
			})
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

// handleSunshine sets (or with an empty host, clears) the Sunshine host whose
// streaming ports are forwarded from 127.0.0.1, saves the config and applies
// it without a restart.
func (d *daemon) handleSunshine(w http.ResponseWriter, r *http.Request) {
	host := strings.TrimSpace(r.URL.Query().Get("host"))
	if !validHostName(host) {
		http.Error(w, "that does not look like a host name or address", http.StatusBadRequest)
		return
	}
	d.mu.Lock()
	d.cfg.SunshineHost = host
	cfg := d.cfg
	d.mu.Unlock()
	if err := saveConfig(d.cfgPath, cfg); err != nil {
		d.logf("saving config: %v", err)
	}
	d.logf("sunshine host set to %q", host)
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

// handleUninstall removes the installed payload, then stops. With ?purge=1 it
// first logs the console out of the tailnet and deletes the saved state as
// well.
func (d *daemon) handleUninstall(w http.ResponseWriter, r *http.Request) {
	purge := r.URL.Query().Get("purge") == "1"
	if purge && d.lc != nil {
		if err := d.lc.Logout(r.Context()); err != nil {
			d.logf("uninstall: logout: %v", err)
		}
	}
	if err := uninstall(purge); err != nil {
		d.logf("uninstall: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d.logf("uninstalled (purge=%v)", purge)
	notify("Tailscale was removed from this PS5.")
	io.WriteString(w, "uninstalled\n")
	d.stop()
}

// stopRunningInstance asks an instance that is already serving the status
// page to exit and waits for the port to become free. It reports whether
// there was one.
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
