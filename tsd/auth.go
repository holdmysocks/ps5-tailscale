package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The status page can be given a password. Without one it trusts whoever can
// reach it, like the other services on a jailbroken console. With one, the
// page and its API ask for it, except from the console itself: someone at
// the console can do anything anyway, and typing a password with a
// controller is no fun.
//
// A browser that has entered the password gets a session cookie.

const (
	sessionCookie   = "ps5ts_session"
	sessionLifetime = 30 * 24 * time.Hour
	pbkdf2Rounds    = 210_000
)

// hashPassword returns the stored form of a password:
// "pbkdf2-sha256$<rounds>$<salt hex>$<key hex>".
func hashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Rounds, 32)
	if err != nil {
		return "", err
	}
	return "pbkdf2-sha256$" + strconv.Itoa(pbkdf2Rounds) + "$" + hex.EncodeToString(salt) + "$" + hex.EncodeToString(key), nil
}

// checkPassword reports whether password matches a stored hash.
func checkPassword(stored, password string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	rounds, err := strconv.Atoi(parts[1])
	if err != nil || rounds < 1 || rounds > 10_000_000 {
		return false
	}
	salt, err1 := hex.DecodeString(parts[2])
	want, err2 := hex.DecodeString(parts[3])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, rounds, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

// sessions are the browsers that have entered the password.
type sessions struct {
	mu      sync.Mutex
	tokens  map[string]time.Time // token -> expiry
	attempt sync.Mutex           // serializes password attempts
}

func (s *sessions) create() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens == nil {
		s.tokens = map[string]time.Time{}
	}
	now := time.Now()
	for t, exp := range s.tokens {
		if now.After(exp) {
			delete(s.tokens, t)
		}
	}
	s.tokens[token] = now.Add(sessionLifetime)
	return token, nil
}

func (s *sessions) valid(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.tokens[token]
	return ok && time.Now().Before(exp)
}

func (s *sessions) remove(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tokens, token)
}

// clear ends every session, for when the password changes.
func (s *sessions) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = nil
}

// fromConsole reports whether the request was made on the console itself.
// Connections from the tailnet are served directly (see tailnetListener), so
// they arrive with their tailnet address, not as loopback.
func fromConsole(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.Unmap().IsLoopback()
}

// authorized reports whether the request may use the page: there is no
// password, it comes from the console itself, or it carries a session.
func (d *daemon) authorized(r *http.Request) bool {
	d.mu.Lock()
	hash := d.cfg.PasswordHash
	d.mu.Unlock()
	if hash == "" || fromConsole(r) {
		return true
	}
	c, err := r.Cookie(sessionCookie)
	return err == nil && d.sessions.valid(c.Value)
}

// protect wraps a handler that needs the password, if one is set.
func (d *daemon) protect(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !d.authorized(r) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]any{"locked": true, "version": version})
			return
		}
		h(w, r)
	}
}

// handleAuth checks a password and starts a session.
func (d *daemon) handleAuth(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	d.mu.Lock()
	hash := d.cfg.PasswordHash
	d.mu.Unlock()

	// One attempt at a time, and a wrong one costs a second: enough to make
	// guessing over the network pointless.
	d.sessions.attempt.Lock()
	ok := hash == "" || checkPassword(hash, req.Password)
	if !ok {
		time.Sleep(time.Second)
	}
	d.sessions.attempt.Unlock()
	if !ok {
		d.logf("status page: wrong password from %s", r.RemoteAddr)
		http.Error(w, "wrong password", http.StatusForbidden)
		return
	}

	token, err := d.sessions.create()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(sessionLifetime.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	io.WriteString(w, "ok\n")
}

// handleLock ends the browser's session.
func (d *daemon) handleLock(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		d.sessions.remove(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	io.WriteString(w, "ok\n")
}

// tailnetListener hands the status page's server the connections that arrive
// for it over the tailnet. They could be piped to the page's port on
// localhost like any other, but then every tailnet device would look like
// the console itself and get past the password.
type tailnetListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newTailnetListener() *tailnetListener {
	return &tailnetListener{conns: make(chan net.Conn, 16), closed: make(chan struct{})}
}

// deliver gives a tailnet connection to the server.
func (l *tailnetListener) deliver(c net.Conn) {
	select {
	case l.conns <- c:
	case <-l.closed:
		c.Close()
	}
}

func (l *tailnetListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *tailnetListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *tailnetListener) Addr() net.Addr { return tailnetAddr{} }

type tailnetAddr struct{}

func (tailnetAddr) Network() string { return "tailnet" }
func (tailnetAddr) String() string  { return "tailnet" }
