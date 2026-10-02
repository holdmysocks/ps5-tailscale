//go:build freebsd

package main

import (
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Only one daemon may run: two would share one Tailscale identity and fight
// over it. A new instance first asks the old one to stop through its status
// page, but that page can be unreachable, so the daemon also records its
// process id and a new instance stops whatever process holds it.
//
// Process ids start over after a reboot, and a stale file could then name an
// unrelated process. The file therefore also holds the kernel's boot time, and
// the pid is only trusted when that still matches.

// bootID identifies the current boot, or is empty if the kernel will not say.
func bootID() string {
	raw, err := syscall.Sysctl("kern.boottime")
	if err != nil || raw == "" {
		return ""
	}
	return hex.EncodeToString([]byte(raw))
}

// stopRecordedInstance stops the daemon named in the pid file, if it is
// still running. It reports the pid it stopped, or 0.
func stopRecordedInstance(pidFile string) int {
	b, err := os.ReadFile(pidFile)
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(b))
	if len(fields) != 2 {
		return 0
	}
	pid, err := strconv.Atoi(fields[0])
	boot := bootID()
	if err != nil || pid <= 1 || pid == os.Getpid() || boot == "" || fields[1] != boot {
		return 0
	}
	if syscall.Kill(pid, 0) != nil {
		return 0 // already gone
	}

	syscall.Kill(pid, syscall.SIGTERM)
	for i := 0; i < 40; i++ {
		time.Sleep(250 * time.Millisecond)
		if syscall.Kill(pid, 0) != nil {
			return pid
		}
	}
	syscall.Kill(pid, syscall.SIGKILL)
	time.Sleep(500 * time.Millisecond)
	return pid
}

// recordInstance writes this process to the pid file.
func recordInstance(pidFile string) error {
	boot := bootID()
	if boot == "" {
		// Without a boot id the pid could not be trusted later.
		os.Remove(pidFile)
		return fmt.Errorf("kern.boottime is not available")
	}
	return os.WriteFile(pidFile, []byte(fmt.Sprintf("%d %s\n", os.Getpid(), boot)), 0o644)
}
