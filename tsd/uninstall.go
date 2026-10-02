package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// daemonFileName is the name the installer stores the daemon payload under,
// in the data directory.
const daemonFileName = "tailscale.elf"

// uninstall deletes the installed daemon payload. With purge it also deletes
// the Tailscale state and config, which forgets the login. The running
// process is not affected.
func uninstall(purge bool) error {
	var errs []error
	if err := os.Remove(filepath.Join(dataDir, daemonFileName)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, err)
	}
	if purge {
		for _, name := range []string{"state", "config.json", ".cache"} {
			if err := os.RemoveAll(filepath.Join(dataDir, name)); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
