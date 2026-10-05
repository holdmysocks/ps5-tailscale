// Command ps5tailscale runs Tailscale on a jailbroken PS5.
//
// The PS5 kernel has no tunnel device, so Tailscale runs entirely in
// userspace (tsnet + netstack). The console joins the tailnet as a node, and
// every TCP connection that arrives for it over the tailnet is handed to the
// matching port on localhost. That makes whatever is listening on the console
// (FTP, the payload loader, web servers, ...) reachable from the tailnet.
package main

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "golang.org/x/crypto/x509roots/fallback" // the PS5 has no system CA bundle

	"tailscale.com/client/local"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

// version is shown on the status page. Set at build time with -ldflags -X.
var version = "dev"

// forceVerbose turns on Tailscale's own logging regardless of the config
// file. Set to "1" at build time with -ldflags -X for debugging builds.
var forceVerbose = ""

// dataDir holds the config, the log and Tailscale's state. PS5TS_DATA overrides
// it when testing on a development machine.
var dataDir = func() string {
	if d := os.Getenv("PS5TS_DATA"); d != "" {
		return d
	}
	return "/data/tailscale"
}()

func main() {
	console := newConsole()
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		console.Printf("tailscale: cannot create %s: %v\n", dataDir, err)
		os.Exit(1)
	}
	logFile, err := openLog(filepath.Join(dataDir, "tailscale.log"))
	if err != nil {
		console.Printf("tailscale: cannot open log: %v\n", err)
		os.Exit(1)
	}
	logf := newLogger(logFile)

	cfg, err := loadConfig(filepath.Join(dataDir, "config.json"))
	if err != nil {
		logf("config: %v (using defaults)", err)
	}
	logf("ps5-tailscale %s starting, hostname %q, web UI on %s", version, cfg.Hostname, cfg.WebAddr)

	// A payload that is sent again replaces the running instance, which is
	// how an upgrade or a restart is done.
	if stopRunningInstance(cfg.WebAddr) {
		logf("stopped the instance that was already running")
	}
	// The status page of the old instance may have been unreachable, so
	// also go by the process it recorded.
	pidFile := filepath.Join(dataDir, "tailscale.pid")
	if pid := stopRecordedInstance(pidFile); pid != 0 {
		logf("stopped a previous instance that was still running (pid %d)", pid)
	}
	if err := recordInstance(pidFile); err != nil {
		logf("pid file: %v", err)
	}

	// Everything in the main log also goes to the debug log, so that it
	// reads as one timeline with Tailscale's own messages.
	debug := openDebugLog(filepath.Join(dataDir, "tailscale-debug.log"), 4<<20)
	mainLogf := logf
	logf = func(format string, args ...any) {
		mainLogf(format, args...)
		debug.Printf(format, args...)
	}

	// What an update installed from the status page downloaded; this may be
	// the very copy that is running now, and it is not needed again.
	os.RemoveAll(filepath.Join(dataDir, updateDirName))

	d := &daemon{cfg: cfg, cfgPath: filepath.Join(dataDir, "config.json"), logf: logf, debug: debug, console: console, started: time.Now()}
	if err := d.run(); err != nil {
		logf("fatal: %v", err)
		notify("Tailscale failed to start:\n%v", err)
		os.Exit(1)
	}
}

type daemon struct {
	cfg     config // guarded by mu once the web UI is up
	cfgPath string
	debug   *debugLog
	logf    func(format string, args ...any)
	console *console
	started time.Time

	srv *tsnet.Server
	lc  *local.Client
	fwd *forwarder
	udp *udpExposer

	mu       sync.Mutex
	state    string // ipn backend state, e.g. "NeedsLogin", "Running"
	authURL  string
	lastErr  string
	notified string // last state the user was notified about

	lastTsnetMsg  string
	proxyLn       *resilientListener // the outbound HTTP proxy, nil if disabled
	proxyPort     uint16             // its port, 0 if disabled
	webPort       uint16             // port of the status page
	lastRelogin   time.Time
	lastNetChange time.Time
	latest        releaseInfo    // newest release known, see update.go
	update        updateProgress // an update being installed, see selfupdate.go

	sessions   sessions         // browsers that have entered the password
	access     accessCache      // recent decisions about who may connect
	tailnetWeb *tailnetListener // status page connections arriving over the tailnet

	quit     chan struct{}
	quitOnce sync.Once
	// removeDataOnExit is set by Uninstall: delete the data directory once
	// everything that writes to it has shut down.
	removeDataOnExit bool
}

func (d *daemon) run() error {
	d.quit = make(chan struct{})

	d.srv = &tsnet.Server{
		Dir:      filepath.Join(dataDir, "state"),
		Hostname: d.cfg.Hostname,
		AuthKey:  d.cfg.AuthKey,
		UserLogf: d.tsnetLogf,
	}
	if d.cfg.ControlURL != "" {
		d.srv.ControlURL = d.cfg.ControlURL
	}
	// Tailscale's own log always goes to the debug log; the main log only
	// gets it when asked.
	if d.cfg.Verbose || forceVerbose == "1" {
		d.srv.Logf = func(format string, args ...any) { d.logf("ts: "+format, args...) }
	} else {
		d.srv.Logf = func(format string, args ...any) { d.debug.Printf("ts: "+format, args...) }
	}

	// The web UI comes up first so that it can show progress and errors even
	// if Tailscale itself has trouble starting.
	d.fwd = newForwarder(d.dialTailnet, d.logf)
	webLn, err := listenResilient("tcp", d.cfg.WebAddr, d.logf)
	if err != nil {
		return fmt.Errorf("web UI: %w", err)
	}
	webLn.onReopen = d.networkChanged
	d.webPort = webLn.port()
	d.tailnetWeb = newTailnetListener()
	handler := d.webHandler()
	go d.serveWeb(webLn, handler)
	go d.serveWeb(d.tailnetWeb, handler)
	d.writePriorityFile()

	if err := d.srv.Start(); err != nil {
		return fmt.Errorf("starting tailscale: %w", err)
	}
	d.lc, err = d.srv.LocalClient()
	if err != nil {
		return fmt.Errorf("local client: %w", err)
	}

	if err := d.setProxy(d.cfg.HTTPProxyAddr); err != nil {
		d.logf("http proxy: %v", err)
	}

	d.fwd.set(d.localForwardRules())

	d.srv.RegisterFallbackTCPHandler(d.forwardToLocalhost)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.udp = &udpExposer{listen: d.srv.ListenPacket, logf: d.logf, targetHost: "127.0.0.1", allow: d.allowedFromAddr}
	go d.watch(ctx)
	go d.recoverLogin(ctx)
	go d.exposeUDP(ctx)
	go d.watchForUpdates(ctx)
	go d.watchKeyExpiry(ctx)
	go d.collectFiles(ctx)

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGTERM, syscall.SIGINT)
	select {
	case <-d.quit:
		d.logf("stop requested")
	case s := <-sigc:
		d.logf("received %v", s)
	}
	cancel()
	webLn.Close()
	d.tailnetWeb.Close()

	done := make(chan struct{})
	go func() {
		d.srv.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		d.logf("shutdown timed out")
	}
	d.logf("stopped")

	d.mu.Lock()
	remove := d.removeDataOnExit
	d.mu.Unlock()
	if remove {
		os.RemoveAll(dataDir)
	}
	return nil
}

// dialTailnet opens a connection through Tailscale.
func (d *daemon) dialTailnet(ctx context.Context, network, addr string) (net.Conn, error) {
	return d.srv.Dial(ctx, network, addr)
}

// localForwardRules returns the local forwards the config asks for.
func (d *daemon) localForwardRules() []forwardRule {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append(sunshineRules(d.cfg.SunshineHosts), d.cfg.Forwards...)
}

// networkChanged is called when a listening socket has died and been
// reopened, which on the PS5 means the network was reconfigured (connection
// settings changed, Wi-Fi to Ethernet, ...). Tailscale notices changes by
// polling the interfaces; this tells it straight away to open fresh sockets
// and work out its addresses again.
func (d *daemon) networkChanged() {
	d.mu.Lock()
	recent := time.Since(d.lastNetChange) < 10*time.Second
	d.lastNetChange = time.Now()
	lc := d.lc
	d.mu.Unlock()
	if recent || lc == nil {
		return
	}
	d.logf("the console's network changed; asking Tailscale to rebind")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		for _, action := range []string{"rebind", "restun"} {
			if err := lc.DebugAction(ctx, action); err != nil {
				d.logf("tailscale %s: %v", action, err)
			}
		}
	}()
}

// exposeUDP keeps the configured UDP ports listening on the console's tailnet
// addresses. Those are only known once Tailscale is connected and can change,
// so they are checked periodically.
func (d *daemon) exposeUDP(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		d.mu.Lock()
		running := d.state == "Running"
		ports := slices.Clone(d.cfg.UDPPorts)
		d.mu.Unlock()
		if running {
			var addrs []netip.Addr
			v4, v6 := d.srv.TailscaleIPs()
			for _, a := range []netip.Addr{v4, v6} {
				if a.IsValid() {
					addrs = append(addrs, a)
				}
			}
			if len(addrs) > 0 {
				d.udp.update(addrs, ports)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// tsnetLogf receives tsnet's messages for the user. While it waits for a
// login it repeats the same line every few seconds, which would drown the
// log, so a message is only logged again when it changes.
func (d *daemon) tsnetLogf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	d.mu.Lock()
	repeat := msg == d.lastTsnetMsg
	d.lastTsnetMsg = msg
	d.mu.Unlock()
	if !repeat {
		d.logf("tsnet: %s", msg)
	}
}

// watch follows the backend state and tells the user, on screen, when
// something needs their attention.
func (d *daemon) watch(ctx context.Context) {
	for ctx.Err() == nil {
		err := d.watchOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		d.logf("state watcher: %v; retrying", err)
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
	}
}

func (d *daemon) watchOnce(ctx context.Context) error {
	w, err := d.lc.WatchIPNBus(ctx, ipn.NotifyInitialState)
	if err != nil {
		return err
	}
	defer w.Close()
	for {
		n, err := w.Next()
		if err != nil {
			return err
		}
		if n.ErrMessage != nil {
			d.logf("backend error: %s", *n.ErrMessage)
			d.mu.Lock()
			d.lastErr = *n.ErrMessage
			d.mu.Unlock()
		}
		if n.BrowseToURL != nil && *n.BrowseToURL != "" {
			d.setAuthURL(*n.BrowseToURL)
		}
		if n.State != nil {
			d.setState(ctx, n.State.String())
		}
	}
}

// freshLogin abandons whatever login is pending and starts a new one, which
// produces a new login link.
func (d *daemon) freshLogin(ctx context.Context) error {
	d.mu.Lock()
	d.authURL = ""
	d.lastRelogin = time.Now()
	d.mu.Unlock()
	if err := d.lc.Logout(ctx); err != nil {
		d.logf("fresh login: logout: %v", err)
	}
	return d.lc.StartLoginInteractive(ctx)
}

// recoverLogin gets the daemon out of a dead-end seen on the console: the
// user completes the login in the browser, but the confirmation never
// arrives and Tailscale's retry is answered with "auth path not found". The
// backend then keeps offering the used-up link forever. When that error
// shows up, start over with a new link.
func (d *daemon) recoverLogin(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		d.mu.Lock()
		waiting := d.state == "NeedsLogin" && time.Since(d.lastRelogin) > time.Minute
		d.mu.Unlock()
		if !waiting {
			continue
		}
		st, err := d.status(ctx)
		if err != nil {
			continue
		}
		for _, h := range st.Health {
			if strings.Contains(h, "auth path not found") {
				d.logf("the pending login link is no longer valid; requesting a new one")
				if err := d.freshLogin(ctx); err != nil {
					d.logf("fresh login: %v", err)
				}
				break
			}
		}
	}
}

func (d *daemon) setAuthURL(u string) {
	d.mu.Lock()
	changed := d.authURL != u
	d.authURL = u
	d.mu.Unlock()
	if !changed {
		return
	}
	d.logf("login URL: %s", u)
	d.console.Printf("\nTo connect this PS5 to your tailnet, open:\n\n    %s\n\n(also shown at %s)\n", u, d.webURL())
	notify("Tailscale needs you to log in.\nOpen %s on a phone or PC.", d.webURL())
}

func (d *daemon) setState(ctx context.Context, state string) {
	d.mu.Lock()
	prev := d.state
	d.state = state
	if state == "Running" {
		d.authURL = ""
		d.lastErr = ""
	}
	already := d.notified == state
	d.notified = state
	d.mu.Unlock()
	if prev != state {
		d.logf("state: %s -> %s", prev, state)
	}
	if already {
		return
	}

	switch state {
	case "Running":
		name, ip := d.cfg.Hostname, ""
		if st, err := d.status(ctx); err == nil && st.Self != nil {
			if st.Self.DNSName != "" {
				name = strings.TrimSuffix(st.Self.DNSName, ".")
			}
			if len(st.Self.TailscaleIPs) > 0 {
				ip = st.Self.TailscaleIPs[0].String()
			}
		}
		d.console.Printf("Tailscale is connected: %s %s\n", name, ip)
		notify("Tailscale connected\n%s\n%s", name, ip)
	case "NeedsMachineAuth":
		notify("Tailscale: this PS5 is waiting for approval in the admin console.")
	}
}

func (d *daemon) status(ctx context.Context) (*ipnstate.Status, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return d.lc.Status(ctx)
}

// webURL is the address of the status page as seen from the local network.
func (d *daemon) webURL() string {
	_, port, _ := net.SplitHostPort(d.cfg.WebAddr)
	return "http://" + net.JoinHostPort(lanIP(), port)
}

// lanIP returns the console's address on the local network.
func lanIP() string {
	// No packet is sent; connecting a UDP socket only selects a route.
	c, err := net.Dial("udp4", "192.0.2.1:9")
	if err != nil {
		return "127.0.0.1"
	}
	defer c.Close()
	if a, ok := c.LocalAddr().(*net.UDPAddr); ok && !a.IP.IsUnspecified() {
		return a.IP.String()
	}
	return "127.0.0.1"
}
