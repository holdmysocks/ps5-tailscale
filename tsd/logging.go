package main

import (
	"fmt"
	"os"
	"sync"
	"time"
)

const maxLogSize = 2 << 20

// openLog opens the log file and points stdout and stderr at it, so that Go
// runtime crash output is kept too. A large log is rotated once at startup.
func openLog(path string) (*os.File, error) {
	if fi, err := os.Stat(path); err == nil && fi.Size() > maxLogSize {
		os.Rename(path, path+".old")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	redirectStdio(f)
	return f, nil
}

// newLogger returns a logf that writes timestamped lines to the log file and
// keeps the most recent ones for the status page.
func newLogger(f *os.File) func(format string, args ...any) {
	var mu sync.Mutex
	return func(format string, args ...any) {
		line := time.Now().Format("2006-01-02 15:04:05 ") + fmt.Sprintf(format, args...)
		mu.Lock()
		fmt.Fprintln(f, line)
		mu.Unlock()
		recentLogs.add(line)
	}
}

// logRing keeps the last lines of the log in memory.
type logRing struct {
	mu    sync.Mutex
	lines []string
}

var recentLogs logRing

func (r *logRing) add(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, line)
	if len(r.lines) > 200 {
		r.lines = r.lines[len(r.lines)-200:]
	}
}

func (r *logRing) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}
