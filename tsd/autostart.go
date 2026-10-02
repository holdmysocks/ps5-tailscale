package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// The installer registers the daemon with whichever payload autoloader the
// console uses: it copies the payload where the autoloader looks for files
// and adds its file name to the autoloader's load order (one payload per
// line, "!N" lines are delays). This file is the daemon's side of that:
// reporting the registration and undoing it. Keep the table in sync with
// installer/main.c.

const daemonFileName = "tailscale.elf"

// An autoloader is one place a payload autoloader keeps its load order.
type autoloader struct {
	name       string // for messages
	list       string // the load order file
	payloadDir string // where the daemon payload is stored for it
}

var autoloaders = func() []autoloader {
	al := []autoloader{
		// Payload Manager finds entries by file name anywhere below
		// /data/pldmgr and keeps each payload in a folder of its own.
		{"Payload Manager", "/data/pldmgr/autoload.txt", "/data/pldmgr/payloads/Tailscale"},
		{"PS5 autoloader", "/data/ps5_autoloader/autoload.txt", "/data/ps5_autoloader"},
	}
	for _, usb := range []string{"usb0", "usb1", "usb2", "usb3"} {
		dir := "/mnt/" + usb + "/ps5_autoloader"
		al = append(al, autoloader{"PS5 autoloader (" + usb + ")", dir + "/autoload.txt", dir})
	}
	return al
}()

// autostartNames returns the autoloaders whose load order includes the daemon.
func autostartNames() []string {
	names := []string{}
	for _, al := range autoloaders {
		b, err := os.ReadFile(al.list)
		if err != nil {
			continue
		}
		if _, found := removeAutoloadEntry(string(b), daemonFileName); found {
			names = append(names, al.name)
		}
	}
	return names
}

// removeAutoloadEntry returns text without the lines that name the payload,
// and whether there were any. Everything else, including line endings, is
// kept as it was.
func removeAutoloadEntry(text, name string) (string, bool) {
	var out strings.Builder
	found := false
	for len(text) > 0 {
		line := text
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			line = text[:i+1]
		}
		text = text[len(line):]
		if strings.TrimSpace(line) == name {
			found = true
			continue
		}
		out.WriteString(line)
	}
	return out.String(), found
}

// uninstall removes the daemon from every autoloader and deletes the
// installed payload. With purge it also deletes the Tailscale state and
// config, which forgets the login. The running process is not affected.
func uninstall(purge bool, logf func(string, ...any)) error {
	var errs []error
	remove := func(path string) {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	for _, al := range autoloaders {
		b, err := os.ReadFile(al.list)
		if err != nil {
			continue
		}
		if text, found := removeAutoloadEntry(string(b), daemonFileName); found {
			if err := os.WriteFile(al.list, []byte(text), 0o644); err != nil {
				errs = append(errs, err)
				continue
			}
			logf("removed %s from %s", daemonFileName, al.list)
		}
		remove(filepath.Join(al.payloadDir, daemonFileName))
		if filepath.Base(al.payloadDir) == "Tailscale" {
			// A folder the installer created just for this payload.
			os.Remove(al.payloadDir)
		}
	}
	remove(filepath.Join(dataDir, daemonFileName))
	if purge {
		for _, name := range []string{"state", "config.json", ".cache"} {
			if err := os.RemoveAll(filepath.Join(dataDir, name)); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
