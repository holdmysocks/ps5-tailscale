package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ps5tailscale/relsig"
)

func withVersion(t *testing.T, v string) {
	old := version
	version = v
	t.Cleanup(func() { version = old })
}

func TestCheckSignature(t *testing.T) {
	withVersion(t, "1.0.0")
	pub, priv, _ := ed25519.GenerateKey(nil)
	otherPub, otherPriv, _ := ed25519.GenerateKey(nil)
	_ = otherPub
	sum := hex.EncodeToString(make([]byte, 32))
	sign := func(key ed25519.PrivateKey, v string) []byte {
		s, err := relsig.Sign(key, v, sum)
		if err != nil {
			t.Fatal(err)
		}
		return s.Marshal()
	}

	if _, err := checkSignature(sign(priv, "1.1.0"), pub, "1.1.0"); err != nil {
		t.Errorf("a good signature was refused: %v", err)
	}
	for name, tt := range map[string]struct {
		sig  []byte
		want string
	}{
		"signed with another key":        {sign(otherPriv, "1.1.0"), "1.1.0"},
		"signature of another version":   {sign(priv, "1.0.5"), "1.1.0"},
		"a properly signed older build":  {sign(priv, "0.9.0"), "0.9.0"},
		"the version that is running":    {sign(priv, "1.0.0"), "1.0.0"},
		"not a signature file":           {[]byte("<html>not found</html>"), "1.1.0"},
		"tampered version, same payload": {bytes.Replace(sign(priv, "1.0.5"), []byte("1.0.5"), []byte("1.1.0"), 1), "1.1.0"},
	} {
		if _, err := checkSignature(tt.sig, pub, tt.want); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestBuiltInKeyIsValid(t *testing.T) {
	if _, err := relsig.ParsePublicKey(updatePublicKey); err != nil {
		t.Fatalf("updatePublicKey: %v", err)
	}
}

func TestCanInstall(t *testing.T) {
	withVersion(t, "1.0.0")
	both := map[string]string{"tailscale.elf": "u1", "tailscale.elf.sig": "u2"}
	for _, tt := range []struct {
		rel  releaseInfo
		want bool
	}{
		{releaseInfo{Version: "1.1.0", Assets: both}, true},
		{releaseInfo{Version: "1.0.0", Assets: both}, false},
		{releaseInfo{Version: "1.1.0", Assets: map[string]string{"tailscale.elf": "u1"}}, false},
		{releaseInfo{Version: "1.1.0"}, false},
	} {
		if got := canInstall(tt.rel); got != tt.want {
			t.Errorf("%+v: got %v", tt.rel, got)
		}
	}
}

func TestFetchLatestReleaseAssets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"tag_name":"v1.2.3","html_url":"https://example.com/r","assets":[
			{"name":"tailscale.elf","browser_download_url":"https://example.com/a"},
			{"name":"tailscale.elf.sig","browser_download_url":"https://example.com/b"}]}`)
	}))
	defer srv.Close()
	rel, err := fetchLatestRelease(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != "1.2.3" || rel.Assets["tailscale.elf"] != "https://example.com/a" || rel.Assets["tailscale.elf.sig"] != "https://example.com/b" {
		t.Errorf("got %+v", rel)
	}
}

func TestDownloadAndReplace(t *testing.T) {
	payload := bytes.Repeat([]byte("payload "), 100_000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
			w.Write(payload)
		case "/short":
			w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
			w.Write(payload[:1000])
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	d := &daemon{logf: t.Logf}
	dir := t.TempDir()
	file := filepath.Join(dir, "tailscale.elf")
	sum, err := d.download(context.Background(), srv.URL+"/ok", file, "1.1.0")
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(payload)
	if sum != hex.EncodeToString(want[:]) {
		t.Errorf("sum = %s", sum)
	}
	if d.update.State != "downloading" || d.update.Percent != 100 {
		t.Errorf("progress = %+v", d.update)
	}
	if _, err := d.download(context.Background(), srv.URL+"/short", filepath.Join(dir, "short"), "1.1.0"); err == nil {
		t.Error("a download that was cut short was accepted")
	}
	if _, err := d.download(context.Background(), srv.URL+"/missing", filepath.Join(dir, "missing"), "1.1.0"); err == nil {
		t.Error("a 404 was accepted")
	}

	// Replacing the copy started at boot.
	dst := filepath.Join(dir, "boot", "tailscale.elf")
	os.MkdirAll(filepath.Dir(dst), 0o755)
	os.WriteFile(dst, []byte("old"), 0o644)
	if err := replaceFile(file, dst); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, payload) {
		t.Error("the file was not replaced")
	}
	if _, err := os.Stat(dst + ".new"); err == nil {
		t.Error("the temporary file was left behind")
	}
	// A folder that does not exist: the old file elsewhere stays untouched.
	if err := replaceFile(file, filepath.Join(dir, "nowhere", "tailscale.elf")); err == nil {
		t.Error("replacing into a missing folder succeeded")
	}
}

func TestStartUpdateRefusals(t *testing.T) {
	withVersion(t, "1.0.0")
	d := &daemon{logf: t.Logf}
	if err := d.startUpdate(); err == nil {
		t.Error("started with no release known")
	}
	d.latest = releaseInfo{Version: "1.1.0", Assets: map[string]string{"tailscale.elf": "x"}}
	if err := d.startUpdate(); err == nil || !strings.Contains(err.Error(), "signed") {
		t.Errorf("an unsigned release: %v", err)
	}
	d.update = updateProgress{State: "downloading"}
	if err := d.startUpdate(); err == nil {
		t.Error("started a second update")
	}
}

func TestValidPayloadPath(t *testing.T) {
	for p, ok := range map[string]bool{
		"": true,
		"/data/pldmgr/payloads/Tailscale/tailscale.elf": true,
		"/mnt/usb0/tailscale.elf":                       true,
		"tailscale.elf":                                 false,
		"/data/../etc/tailscale.elf":                    false,
		"/data//tailscale.elf":                          false,
		"/data/pldmgr/autoload.txt":                     false,
		"/data/tailscale/":                              false,
	} {
		if err := validPayloadPath(p); (err == nil) != ok {
			t.Errorf("%q: %v", p, err)
		}
	}
}
