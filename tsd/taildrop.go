package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	// tsnet leaves Taildrop out unless it is linked in.
	_ "tailscale.com/feature/taildrop"
)

// Receiving files with Taildrop.
//
// A file sent to the console from another device of the same user arrives
// in a holding area inside Tailscale's state directory. The daemon moves
// each one to a folder that can be reached with FTP, says so on screen, and
// lists the folder on the status page.

const defaultReceiveDir = "/data/tailscale/received"

// receivedFile is one entry of the list on the status page.
type receivedFile struct {
	Name string    `json:"name"`
	Size int64     `json:"size"`
	Time time.Time `json:"time"`
}

func (d *daemon) receiveDir() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cfg.ReceiveDir == "" {
		return defaultReceiveDir
	}
	return d.cfg.ReceiveDir
}

// collectFiles waits for files in the holding area and moves them out.
func (d *daemon) collectFiles(ctx context.Context) {
	for ctx.Err() == nil {
		// Returns as soon as there are files, or after the wait with none.
		files, err := d.lc.AwaitWaitingFiles(ctx, time.Minute)
		if ctx.Err() != nil {
			return
		}
		if err != nil || len(files) == 0 {
			if err != nil && !strings.Contains(err.Error(), "context deadline exceeded") {
				// Typically: not logged in yet, or Taildrop is turned off
				// for the tailnet. Try again later without filling the log.
				select {
				case <-ctx.Done():
					return
				case <-time.After(30 * time.Second):
				}
			}
			continue
		}
		dir := d.receiveDir()
		var got []string
		for _, f := range files {
			name, err := d.collectFile(ctx, dir, f.Name)
			if err != nil {
				d.logf("taildrop: %s: %v", f.Name, err)
				continue
			}
			d.logf("taildrop: received %s (%d bytes) into %s", name, f.Size, dir)
			got = append(got, name)
		}
		switch len(got) {
		case 0:
			// Nothing could be moved; do not spin on the same files.
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
			}
		case 1:
			notify("Tailscale: received a file\n%s\nin %s", got[0], dir)
		default:
			notify("Tailscale: received %d files\nin %s", len(got), dir)
		}
	}
}

// collectFile copies one waiting file into dir and removes it from the
// holding area. It returns the name the file got.
func (d *daemon) collectFile(ctx context.Context, dir, name string) (string, error) {
	rc, _, err := d.lc.GetWaitingFile(ctx, name)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	saved, err := saveReceived(dir, name, rc)
	if err != nil {
		return "", err
	}
	if err := d.lc.DeleteWaitingFile(ctx, name); err != nil {
		d.logf("taildrop: %s is saved, but could not be removed from the holding area: %v", name, err)
	}
	return saved, nil
}

// safeFileName reduces a name from another device to a plain file name.
func safeFileName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = name[strings.LastIndex(name, "/")+1:]
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "file"
	}
	return name
}

// saveReceived writes r to a new file in dir, named after name. A file that
// is already there is never overwritten: the new one gets a number instead.
func saveReceived(dir, name string, r io.Reader) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name = safeFileName(name)
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	var f *os.File
	var err error
	final := name
	for n := 1; ; n++ {
		if n > 1 {
			final = fmt.Sprintf("%s (%d)%s", base, n, ext)
		}
		f, err = os.OpenFile(filepath.Join(dir, final), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			break
		}
		if !os.IsExist(err) || n > 10000 {
			return "", err
		}
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return final, nil
}

// listReceived returns the newest files in dir, newest first.
func listReceived(dir string, max int) []receivedFile {
	files := []receivedFile{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return files
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		files = append(files, receivedFile{Name: e.Name(), Size: info.Size(), Time: info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Time.After(files[j].Time) })
	if len(files) > max {
		files = files[:max]
	}
	return files
}

func (d *daemon) handleFiles(w http.ResponseWriter, r *http.Request) {
	dir := d.receiveDir()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]any{"dir": dir, "files": listReceived(dir, 100)})
}

// handleFileGet sends one received file to the browser.
func (d *daemon) handleFileGet(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" || name != safeFileName(name) {
		http.Error(w, "no such file", http.StatusNotFound)
		return
	}
	f, err := os.Open(filepath.Join(d.receiveDir(), name))
	if err != nil {
		http.Error(w, "no such file", http.StatusNotFound)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.Error(w, "no such file", http.StatusNotFound)
		return
	}
	// Always a download, never something the browser renders: the file came
	// from elsewhere and this page's origin has the controls on it.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename*=UTF-8''%s", urlPathEscape(name)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Wrapped so that Go does not use sendfile, which fails on the PS5.
	http.ServeContent(w, r, "", info.ModTime(), struct{ io.ReadSeeker }{f})
}

func urlPathEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
