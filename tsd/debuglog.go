package main

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// debugLog keeps Tailscale's own verbose log on disk, in a file of bounded
// size next to the main log.
//
// The main log only has the daemon's few messages, which is not enough to
// tell what Tailscale was doing when something goes wrong, and the things
// that go wrong on a console (a freeze, a crash) take the process down
// before anyone can ask it. So the detail is always recorded and synced to
// disk every couple of seconds.
type debugLog struct {
	path string
	max  int64

	mu    sync.Mutex
	f     *os.File
	size  int64
	dirty bool
}

func openDebugLog(path string, max int64) *debugLog {
	l := &debugLog{path: path, max: max}
	l.open()
	go l.syncLoop()
	return l
}

func (l *debugLog) open() {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	l.f = f
	if fi, err := f.Stat(); err == nil {
		l.size = fi.Size()
	}
}

func (l *debugLog) Printf(format string, args ...any) {
	line := time.Now().Format("2006-01-02 15:04:05.000 ") + fmt.Sprintf(format, args...) + "\n"
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return
	}
	if l.size+int64(len(line)) > l.max {
		// Keep one previous file, so there is always between one and two
		// times max of history.
		l.f.Close()
		os.Rename(l.path, l.path+".old")
		l.f, l.size = nil, 0
		l.open()
		if l.f == nil {
			return
		}
	}
	n, _ := l.f.WriteString(line)
	l.size += int64(n)
	l.dirty = true
}

func (l *debugLog) syncLoop() {
	for range time.Tick(2 * time.Second) {
		l.mu.Lock()
		if l.dirty && l.f != nil {
			l.f.Sync()
			l.dirty = false
		}
		l.mu.Unlock()
	}
}
