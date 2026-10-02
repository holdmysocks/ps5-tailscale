package main

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The home screen icon is installed by a small helper payload that the
// launcher carries (see appicon/ and launcher/homeicon.c). The launcher also
// leaves a copy of the helper in the data directory, so that Uninstall can
// run it again to take the icon away: the system call for that lives in
// libraries this process must not load.
//
// The helper installs or removes depending on one byte in it, after a marker
// string; removing is a matter of flipping that byte before sending it to
// the ELF loader.

const (
	iconHelperFile    = "icon-helper.elf"
	iconModeMarker    = "TSICON-MODE="
	iconModeRemove    = 'R'
	elfLoaderAddr     = "127.0.0.1:9021"
	iconHelperTimeout = 20 * time.Second
)

// removeHomeIcon takes the Tailscale icon off the home screen.
func removeHomeIcon() error {
	helper, err := os.ReadFile(filepath.Join(dataDir, iconHelperFile))
	if err != nil {
		return err
	}
	i := bytes.Index(helper, []byte(iconModeMarker))
	if i < 0 || i+len(iconModeMarker) >= len(helper) {
		return errors.New("the icon helper is not one this version understands")
	}
	helper[i+len(iconModeMarker)] = iconModeRemove

	c, err := net.DialTimeout("tcp", elfLoaderAddr, 3*time.Second)
	if err != nil {
		return fmt.Errorf("no ELF loader to run the icon helper: %w", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(iconHelperTimeout))
	if _, err := c.Write(helper); err != nil {
		return err
	}

	// The helper prints "icon: removed" or what went wrong, and exits.
	var reply []byte
	buf := make([]byte, 512)
	for len(reply) < 4096 {
		n, err := c.Read(buf)
		reply = append(reply, buf[:n]...)
		if line := iconReplyLine(reply); line != "" {
			if strings.HasPrefix(line, "icon: removed") {
				return nil
			}
			return errors.New(line)
		}
		if err != nil {
			break
		}
	}
	return errors.New("no answer from the icon helper")
}

// iconReplyLine returns the helper's complete "icon: ..." line, if it has
// arrived.
func iconReplyLine(reply []byte) string {
	i := bytes.Index(reply, []byte("icon: "))
	if i < 0 {
		return ""
	}
	rest := reply[i:]
	j := bytes.IndexByte(rest, '\n')
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(string(rest[:j]))
}
