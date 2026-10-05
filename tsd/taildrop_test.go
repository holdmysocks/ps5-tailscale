package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSafeFileName(t *testing.T) {
	for in, want := range map[string]string{
		"photo.jpg":              "photo.jpg",
		"../../etc/passwd":       "passwd",
		`C:\Users\me\notes.txt`:  "notes.txt",
		"dir/sub/file.bin":       "file.bin",
		"..":                     "file",
		"":                       "file",
		"a\x00b\n.txt":           "ab.txt",
		"  spaced name.pkg  ":    "spaced name.pkg",
		"save (1).zip":           "save (1).zip",
		"/data/tailscale/x.json": "x.json",
	} {
		if got := safeFileName(in); got != want {
			t.Errorf("safeFileName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSaveReceivedNeverOverwrites(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "received")
	var names []string
	for _, content := range []string{"one", "two", "three"} {
		name, err := saveReceived(dir, "../save.zip", strings.NewReader(content))
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	want := []string{"save.zip", "save (2).zip", "save (3).zip"}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("file %d was named %q, want %q", i, names[i], want[i])
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "save.zip")); string(b) != "one" {
		t.Errorf("the first file was changed: %q", b)
	}
	// Nothing escaped the folder.
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "save.zip")); err == nil {
		t.Error("a file was written outside the folder")
	}

	files := listReceived(dir, 2)
	if len(files) != 2 {
		t.Errorf("listReceived returned %d files, want 2", len(files))
	}
	if got := listReceived(filepath.Join(dir, "missing"), 10); len(got) != 0 {
		t.Errorf("a missing folder listed %d files", len(got))
	}
}

func TestFileDownload(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "page.html"), []byte("<script>alert(1)</script>"), 0o644)
	os.WriteFile(filepath.Join(filepath.Dir(dir), "secret.txt"), []byte("secret"), 0o644)
	cfg := defaultConfig()
	cfg.ReceiveDir = dir
	d := &daemon{cfg: cfg, logf: t.Logf}

	get := func(name string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		d.handleFileGet(rec, httptest.NewRequest("GET", "/api/files/get?name="+url.QueryEscape(name), nil))
		return rec
	}
	rec := get("page.html")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "alert") {
		t.Fatalf("download: %d", rec.Code)
	}
	// Served as a download, not as a page of this origin.
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Errorf("Content-Disposition = %q", cd)
	}
	for _, name := range []string{"../secret.txt", "..", "", "missing.txt", `..\secret.txt`, "sub/page.html"} {
		if rec := get(name); rec.Code != http.StatusNotFound {
			t.Errorf("%q: status %d", name, rec.Code)
		}
	}
}
