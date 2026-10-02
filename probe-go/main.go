// Command probe exercises the parts of the Go runtime and standard library
// that Tailscale depends on, to find out what works on the PS5.
//
// Every test is bounded in time by construction: nothing here may spin
// forever, because a runaway thread can starve the whole console.
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var failed int

func step(name string, fn func() (string, error)) {
	done := make(chan struct{})
	var res string
	var err error
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic: %v", r)
			}
		}()
		res, err = fn()
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		err = fmt.Errorf("timed out")
	}
	if err != nil {
		failed++
		fmt.Printf("FAIL %-22s %v\n", name, err)
		return
	}
	fmt.Printf("ok   %-22s %s\n", name, res)
}

// spin burns CPU without ever calling a function, so only asynchronous
// (signal based) preemption can interrupt it. It stops by itself after a
// fixed number of iterations, a few seconds at most.
//
//go:noinline
func spin(iterations uint64, stop *atomic.Bool) uint64 {
	var x uint64 = 88172645463325252
	for i := uint64(0); i < iterations; i++ {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		if i&0xfffff == 0 && stop.Load() {
			break
		}
	}
	return x
}

func main() {
	// The process must die on its own if anything goes badly wrong.
	go func() {
		time.Sleep(150 * time.Second)
		fmt.Println("probe: watchdog expired, exiting")
		os.Exit(3)
	}()

	fmt.Printf("== go probe: %s %s/%s cpus=%d gomaxprocs=%d pagesize=%d pid=%d GODEBUG=%q ==\n",
		runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.GOMAXPROCS(0),
		os.Getpagesize(), os.Getpid(), os.Getenv("GODEBUG"))

	step("goroutines+timers", func() (string, error) {
		var wg sync.WaitGroup
		var n atomic.Int64
		start := time.Now()
		for i := 0; i < 200; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(20 * time.Millisecond)
				n.Add(1)
			}()
		}
		wg.Wait()
		return fmt.Sprintf("%d goroutines in %v", n.Load(), time.Since(start).Round(time.Millisecond)), nil
	})

	step("os threads", func() (string, error) {
		var wg sync.WaitGroup
		for i := 0; i < 24; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				runtime.LockOSThread()
				defer runtime.UnlockOSThread()
				ts := syscall.Timespec{Nsec: 30e6}
				syscall.Nanosleep(&ts, nil)
			}()
		}
		wg.Wait()
		return "24 locked threads slept in the kernel", nil
	})

	step("nil deref -> panic", func() (res string, err error) {
		defer func() {
			if r := recover(); r != nil {
				res = fmt.Sprintf("recovered: %v", r)
			}
		}()
		var p *int
		*p = 1
		return "", fmt.Errorf("no panic")
	})

	// One spinner per P plus garbage collections that have to stop them.
	// With working async preemption each GC takes milliseconds; without it
	// each GC waits for the spinners to finish.
	step("async preemption+GC", func() (string, error) {
		var stop atomic.Bool
		var wg sync.WaitGroup
		n := runtime.GOMAXPROCS(0)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				spin(3_000_000_000, &stop) // about 3 seconds
			}()
		}
		time.Sleep(50 * time.Millisecond)
		start := time.Now()
		var worst time.Duration
		for i := 0; i < 5; i++ {
			t := time.Now()
			runtime.GC()
			if d := time.Since(t); d > worst {
				worst = d
			}
		}
		total := time.Since(start)
		stop.Store(true)
		wg.Wait()
		res := fmt.Sprintf("%d spinners, 5 GCs in %v (worst %v)", n, total.Round(time.Millisecond), worst.Round(time.Millisecond))
		if worst > time.Second {
			return "", fmt.Errorf("preemption is not working: %s", res)
		}
		return res, nil
	})

	step("heap 256MB", func() (string, error) {
		bufs := make([][]byte, 0, 64)
		for i := 0; i < 64; i++ {
			b := make([]byte, 4<<20)
			for j := 0; j < len(b); j += 4096 {
				b[j] = byte(i)
			}
			bufs = append(bufs, b)
		}
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		bufs = nil
		debug.FreeOSMemory()
		return fmt.Sprintf("sys=%dMB heap=%dMB", ms.Sys>>20, ms.HeapAlloc>>20), nil
	})

	step("crypto/rand", func() (string, error) {
		b := make([]byte, 600)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		return hex.EncodeToString(b[:8]) + "...", nil
	})

	step("time", func() (string, error) {
		return time.Now().Format(time.RFC3339Nano), nil
	})

	step("hostname/cwd/exe", func() (string, error) {
		h, herr := os.Hostname()
		wd, werr := os.Getwd()
		exe, eerr := os.Executable()
		return fmt.Sprintf("host=%q(%v) cwd=%q(%v) exe=%q(%v)", h, herr, wd, werr, exe, eerr), nil
	})

	dir := "/data/tailscale/probe"
	step("mkdir+write+read", func() (string, error) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		p := filepath.Join(dir, "a.txt")
		if err := os.WriteFile(p, []byte("hello ps5\n"), 0o644); err != nil {
			return "", err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%q", b), nil
	})

	step("stat+chtimes", func() (string, error) {
		p := filepath.Join(dir, "a.txt")
		when := time.Date(2024, 2, 3, 4, 5, 6, 0, time.UTC)
		if err := os.Chtimes(p, when, when); err != nil {
			return "", fmt.Errorf("chtimes: %w", err)
		}
		fi, err := os.Stat(p)
		if err != nil {
			return "", err
		}
		li, err := os.Lstat(dir)
		if err != nil {
			return "", err
		}
		if !fi.ModTime().Equal(when) {
			return "", fmt.Errorf("mtime %v, want %v", fi.ModTime(), when)
		}
		return fmt.Sprintf("size=%d mode=%v mtime ok; dir=%v", fi.Size(), fi.Mode(), li.Mode()), nil
	})

	step("rename+remove", func() (string, error) {
		a, b := filepath.Join(dir, "a.txt"), filepath.Join(dir, "b.txt")
		if err := os.Rename(a, b); err != nil {
			return "", err
		}
		f, err := os.OpenFile(b, os.O_RDWR|os.O_APPEND, 0)
		if err != nil {
			return "", err
		}
		if _, err := f.WriteString("more\n"); err != nil {
			return "", err
		}
		if err := f.Sync(); err != nil {
			return "", fmt.Errorf("sync: %w", err)
		}
		f.Close()
		if err := os.Remove(b); err != nil {
			return "", err
		}
		return "ok", nil
	})

	step("readdir", func() (string, error) {
		for i := 0; i < 300; i++ {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("file-with-a-longish-name-%03d", i)), nil, 0o644); err != nil {
				return "", err
			}
		}
		ents, err := os.ReadDir(dir)
		if err != nil {
			return "", err
		}
		data, err := os.ReadDir("/data")
		if err != nil {
			return "", fmt.Errorf("/data: %w", err)
		}
		names := ""
		for _, e := range data {
			names += e.Name() + " "
		}
		if err := os.RemoveAll(dir); err != nil {
			return "", fmt.Errorf("removeall: %w", err)
		}
		if len(ents) != 300 {
			return "", fmt.Errorf("probe dir has %d entries, want 300", len(ents))
		}
		return fmt.Sprintf("probe=%d; /data: %s", len(ents), names), nil
	})

	step("autoloader dir", func() (string, error) {
		b, err := os.ReadFile("/data/ps5_autoloader/autoload.txt")
		if err != nil {
			return fmt.Sprintf("no autoload.txt: %v", err), nil
		}
		ents, _ := os.ReadDir("/data/ps5_autoloader")
		names := ""
		for _, e := range ents {
			names += e.Name() + " "
		}
		return fmt.Sprintf("files: %s\n%s", names, b), nil
	})

	step("notification", func() (string, error) {
		var req [0xC30]byte
		copy(req[0x2D:], "Tailscale probe: notification test")
		fd, err := syscall.Open("/dev/notification0", syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return "", fmt.Errorf("open: %w", err)
		}
		defer syscall.Close(fd)
		n, err := syscall.Write(fd, req[:])
		if err != nil {
			return "", fmt.Errorf("write: %w", err)
		}
		return fmt.Sprintf("wrote %d bytes", n), nil
	})

	step("net.Interfaces", func() (string, error) {
		ifs, err := net.Interfaces()
		if err != nil {
			return "", err
		}
		s := ""
		for _, ifc := range ifs {
			addrs, aerr := ifc.Addrs()
			s += fmt.Sprintf("\n       %d %s %v mtu=%d %v addrs=%v err=%v", ifc.Index, ifc.Name, ifc.HardwareAddr, ifc.MTU, ifc.Flags, addrs, aerr)
		}
		return s, nil
	})

	step("tcp listen+accept", func() (string, error) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return "", err
		}
		defer ln.Close()
		go func() {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			io.Copy(c, c)
			c.Close()
		}()
		c, err := net.DialTimeout("tcp", ln.Addr().String(), 5*time.Second)
		if err != nil {
			return "", err
		}
		defer c.Close()
		c.Write([]byte("ping"))
		c.(*net.TCPConn).CloseWrite()
		b, err := io.ReadAll(c)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("echo %q via %v", b, ln.Addr()), nil
	})

	step("udp v4+v6 sockets", func() (string, error) {
		c4, err := net.ListenPacket("udp4", ":0")
		if err != nil {
			return "", fmt.Errorf("udp4: %w", err)
		}
		defer c4.Close()
		c6, err6 := net.ListenPacket("udp6", ":0")
		if err6 == nil {
			defer c6.Close()
		}
		return fmt.Sprintf("udp4=%v udp6 err=%v", c4.LocalAddr(), err6), nil
	})

	step("dns default resolver", func() (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		ips, err := net.DefaultResolver.LookupHost(ctx, "controlplane.tailscale.com")
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%d addresses, first %s", len(ips), ips[0]), nil
	})

	step("https", func() (string, error) {
		c := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}}
		resp, err := c.Get("https://controlplane.tailscale.com/key?v=100")
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 60))
		return fmt.Sprintf("%s %s %q", resp.Proto, resp.Status, b), nil
	})

	fmt.Printf("== done, %d failed ==\n", failed)
	if failed > 0 {
		os.Exit(1)
	}
}
