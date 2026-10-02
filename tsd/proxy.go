package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/netip"
	"time"
)

// serveProxy runs a plain HTTP proxy whose outbound connections go through
// Tailscale's dialer: tailnet addresses and MagicDNS names are reached over
// the tailnet, everything else goes out directly. Setting it as the proxy
// server in the PS5's network settings lets the console's own apps, such as
// the browser, open tailnet hosts.
func (d *daemon) serveProxy(ln net.Listener) {
	d.logf("http proxy listening on %s", ln.Addr())
	transport := &http.Transport{
		DialContext:           d.proxyDial,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          32,
		IdleConnTimeout:       60 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}
	srv := &http.Server{
		ReadHeaderTimeout: 15 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodConnect {
				d.proxyConnect(w, r)
				return
			}
			if !r.URL.IsAbs() {
				http.Error(w, "this is a proxy; requests must use an absolute URL", http.StatusBadRequest)
				return
			}
			r.RequestURI = ""
			r.Header.Del("Proxy-Connection")
			r.Header.Del("Proxy-Authorization")
			resp, err := transport.RoundTrip(r)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			defer resp.Body.Close()
			for k, vs := range resp.Header {
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(resp.StatusCode)
			io.Copy(w, resp.Body)
		}),
	}
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed && !d.stopping() {
		d.logf("http proxy stopped: %v", err)
	}
}

// proxyDial connects on behalf of a proxy client. Tailscale's dialer handles
// tailnet addresses and names but waits until Tailscale is connected, so it
// is only used once that is the case, and never for the console's own or
// LAN addresses: those must keep working whatever state Tailscale is in.
func (d *daemon) proxyDial(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	running := d.state == "Running"
	d.mu.Unlock()
	if running && !isLocalDestination(addr) {
		return d.srv.Dial(ctx, network, addr)
	}
	var direct net.Dialer
	return direct.DialContext(ctx, network, addr)
}

// isLocalDestination reports whether addr is a literal loopback, private or
// link-local address, or "localhost".
func isLocalDestination(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
}

func (d *daemon) proxyConnect(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	upstream, err := d.proxyDial(ctx, "tcp", r.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer upstream.Close()

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer client.Close()
	io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n")
	// Anything the client sent right after its CONNECT request is already
	// sitting in the server's read buffer.
	if n := buf.Reader.Buffered(); n > 0 {
		io.CopyN(upstream, buf.Reader, int64(n))
	}
	pipe(client, upstream)
}
