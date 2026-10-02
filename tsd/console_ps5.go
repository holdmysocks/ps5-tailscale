//go:build freebsd

package main

import (
	"fmt"
	"os"
	"sync"
	"syscall"
)

// console writes to whoever sent the payload. The ELF loader hands a payload
// its TCP connection as stdin/stdout/stderr, and that connection goes away as
// soon as the sender disconnects.
//
// Go kills the process when a write to fd 1 or 2 fails with EPIPE, so the
// connection is moved to another descriptor and the first failed write turns
// the console off for good.
type console struct {
	mu sync.Mutex
	fd int
}

func newConsole() *console {
	fd, err := syscall.Dup(1)
	if err != nil {
		return &console{fd: -1}
	}
	syscall.CloseOnExec(fd)
	return &console{fd: fd}
}

func (c *console) Printf(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fd < 0 {
		return
	}
	b := []byte(fmt.Sprintf(format, args...))
	for len(b) > 0 {
		n, err := syscall.Write(c.fd, b)
		if err != nil || n <= 0 {
			syscall.Close(c.fd)
			c.fd = -1
			return
		}
		b = b[n:]
	}
}

// redirectStdio points stdout and stderr at the log file.
func redirectStdio(f *os.File) {
	syscall.Dup2(int(f.Fd()), 1)
	syscall.Dup2(int(f.Fd()), 2)
}

// notify shows a pop-up on the PS5 screen.
//
// It does what libkernel's sceKernelSendNotificationRequest does: write a
// fixed-size request to the notification device. The request is 0xC30 bytes
// with the UTF-8 message at offset 0x2D.
func notify(format string, args ...any) {
	const (
		requestSize   = 0xC30
		messageOffset = 0x2D
	)
	var req [requestSize]byte
	msg := fmt.Sprintf(format, args...)
	if max := requestSize - messageOffset - 1; len(msg) > max {
		msg = msg[:max]
	}
	copy(req[messageOffset:], msg)

	fd, err := syscall.Open("/dev/notification0", syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return
	}
	defer syscall.Close(fd)
	syscall.Write(fd, req[:])
}
