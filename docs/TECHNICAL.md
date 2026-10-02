# Technical notes

How Tailscale, a Go program, runs on a PS5, and what was learned about the
console along the way. Everything here was measured on one console
(firmware 13.42, `kern.osreldate` 900000); treat it as observations, not a
specification.

## Architecture

`tailscale.elf` is one payload made of two parts.

**The launcher** (`launcher/`, C, built with ps5-payload-sdk):

1. The SDK's crt runs first. Among other things it patches the process so
   that system calls may be issued from any address. Without that, the kernel
   only accepts syscalls made from libkernel, and Go makes its own.
2. `main.c` raises privileges, moves the process to the lowest scheduling
   priority (see [Scheduling](#scheduling)) and sets `GOMAXPROCS=4`.
3. `goload.c` maps the embedded Go program (a position-independent ELF),
   applies its `R_X86_64_RELATIVE` relocations, makes its text executable,
   builds the argc/argv/envp/auxv block a FreeBSD kernel would pass, switches
   to a fresh 1 MB stack and jumps to the Go entry point. The embedded copy
   is then released with `madvise(MADV_FREE)`.

**The daemon** (`tsd/`, Go): a `tsnet` server.

- Inbound: tsnet's fallback TCP handler pipes each tailnet connection to
  `127.0.0.1:<same port>`. It dials the local port before accepting, so
  ports with no listener are refused properly.
- Inbound UDP (`inboundudp.go`): tsnet has no catch-all for UDP, so the ports
  in `udpPorts` are listened on with `tsnet.Server.ListenPacket` on the
  node's tailnet addresses and relayed to `127.0.0.1`. The default list is
  PS5 Remote Play's (9295, 9296, 9297, 9302); its service answers clients
  that arrive from loopback. Both UDP directions share `udprelay.go`: one
  connection to the target per client address, dropped after two idle
  minutes.
- Outbound: local forwards (`localforward.go`) listen on localhost and relay
  TCP and UDP to a tailnet host through `tsnet.Server.Dial`. UDP is relayed
  per client address with an idle timeout. Each Sunshine host is a preset of
  seven such forwards on the ports that host really uses, derived from
  Sunshine's port setting (HTTPS -5, HTTP +0, RTSP +21, video +9, control
  +10, audio +11, microphone +13). The ports cannot be remapped, because the
  host tells the Moonlight client which ports to use; several hosts can only
  coexist on 127.0.0.1 if their Sunshine ports differ.
- A status page and JSON API on port 8090 (`web.go`, `settings.go`), an
  optional HTTP proxy, PS5 notifications by writing a request to
  `/dev/notification0`.
- Password (`auth.go`): PBKDF2-SHA256 hash in the config, session cookie,
  one attempt per second. Requests from loopback are exempt. For that to be
  safe, connections for the status page that arrive over the tailnet are not
  piped to localhost like other ports but handed to the page's HTTP server
  directly, so it sees the tailnet address.
- Settings are applied live where possible. The launcher needs one of them,
  the priority, before any Go code runs, so the daemon leaves it in
  `/data/tailscale/priority` for the next start.
- Update notice (`update.go`): the latest release tag from the GitHub API,
  twice a day, compared with the running version.
- When a listener reports that it had to reopen its socket (the PS5's
  network was reconfigured), the daemon asks Tailscale to rebind and re-STUN
  instead of waiting for its interface polling. See [Rest mode](#rest-mode)
  for the one time this has been seen.
- A payload that is sent again stops the running instance (through the
  status page, or failing that by the pid it recorded) and takes over.

**The icon helper** (`appicon/`, C) is a second, tiny payload embedded in the
launcher. The first time `tailscale.elf` runs, the launcher sends it to the
ELF loader on `127.0.0.1:9021`, where it runs as a process of its own,
registers a home screen app whose `param.json` has a `deeplinkUri` to the
status page, reports the result and exits. The launcher then writes
`/data/tailscale/icon-installed` and never does it again. It is a separate
payload so that the system libraries it needs are never loaded into the
long-running daemon process, where their threads could receive signals meant
for the Go runtime.

The same helper removes the icon (`sceAppInstUtilAppUnInstall`). A payload
sent to the ELF loader gets no arguments, so the mode is a byte in the file
after the marker `TSICON-MODE=`. The launcher leaves a copy of the helper in
`/data/tailscale/icon-helper.elf`; for Uninstall the daemon flips that byte
and sends it to the loader (`tsd/homeicon.go`).

There is no installer. Nothing is copied anywhere and no payload autoloader
is touched: the payload is run from wherever the user keeps it.

## The PS5 as a Go target

Go is built for `GOOS=freebsd GOARCH=amd64` with cgo off and
`-buildmode=pie`, from a Go 1.27.1 tree with
`patches/go1.27.1-ps5.patch`. What the patch changes and why:

| File | Change | Reason |
| --- | --- | --- |
| `internal/platform/supported.go` | Allow internally linked PIE on freebsd/amd64. | The image must load at any address and no C toolchain is involved. |
| `runtime/defs_freebsd_amd64.go` | 48 bytes of padding in `ucontext` before `uc_mcontext`. | The PS5's signal context has extra fields there. Without it, nil-dereference panics and preemption read garbage. |
| `runtime/defs_freebsd_amd64.go`, `sys_freebsd_amd64.s` | `kevent` 363 with the 32 byte struct. | The FreeBSD 12 `kevent` (560) does not exist. |
| `runtime/sys_freebsd_amd64.s`, `os_freebsd.go` | `pipe2` built from `pipe` and `fcntl`. | `pipe2` (542) does not exist. |
| `runtime/os_freebsd.go` | CPU count from `hw.ncpu`. | `kern.smp.maxcpus` does not exist. |
| `syscall/asm_unix_amd64.s`, `asm9_unix2_amd64.s`, `ps5_freebsd_amd64.go` | Syscall emulation layer. | See below. |
| `internal/routebsd/interface_freebsd.go` | Find the link address using `if_data`'s own length field. | Sony's `if_data` is 0xb0 bytes, so interface names came back empty. |
| `os/executable_sysctl.go` | `os.Executable` falls back to a name from argv[0]. | The kernel has no path for a payload; tsnet treats the error as fatal. |

### The syscall table

The kernel implements the FreeBSD 9 system call numbers. **Every number from
532 up is a Sony syscall with an unrelated meaning**, so nothing FreeBSD
added later exists: `pipe2`, `accept4`, `ppoll`, `utimensat`, `futimens`,
`getrandom`, the 64-bit inode `fstat`/`fstatat`/`getdirentries`/`statfs`
family, the extended `kevent`. Issuing those numbers would call something
else entirely.

Go's `Syscall`, `Syscall6`, `RawSyscall`, `RawSyscall6` and `Syscall9` are
patched to divert numbers >= 532 to `ps5_freebsd_amd64.go`, which rebuilds
them from FreeBSD 9 calls and converts the structures (old `stat`, `dirent`
and `statfs` layouts). Anything not emulated returns `ENOSYS`. Because
`golang.org/x/sys/unix` and `internal/syscall/unix` go through the same entry
points, they are covered too. `socket` and `socketpair` are also diverted, to
apply `SOCK_NONBLOCK`/`SOCK_CLOEXEC` with `fcntl`.

### Other kernel behaviour

- Page size is 16384. The Go linker aligns segments to 4096; the loader
  applies protection per 16 KiB page.
- Working as on FreeBSD: `thr_new`, `_umtx_op`, `sigaction`, `sigaltstack`,
  `thr_kill`, kqueue including `EVFILT_USER`, `mmap` reservations with
  `PROT_NONE` (64 MB and 4 GB tried), `MAP_FIXED`, `madvise(MADV_FREE)`,
  `sysctl` for `hw.pagesize`, `hw.ncpu`, `kern.arandom`, `kern.boottime`,
  `NET_RT_IFLIST` and `NET_RT_DUMP`, `AF_ROUTE` sockets.
- Directory reads: `/data` (nullfs) and `/user` (bfs) only accept a buffer of
  exactly 65536 bytes for `getdirentries`. The emulation always reads 64 KiB
  blocks and hands the converted entries out across calls.
- Unix domain sockets cannot be bound on `/data`.
- There is no `/etc/resolv.conf` and no CA bundle. Go's resolver falls back
  to `127.0.0.1:53`; the daemon imports `x509roots/fallback` for TLS roots.
- A payload's stdin, stdout and stderr are the ELF loader's TCP connection.
  Go kills a process whose write to fd 1 or 2 fails with `EPIPE`, so the
  daemon moves that connection to another descriptor and points 1 and 2 at
  its log file.
- The SDK's `kernel_mprotect()` rewrites the protection of the whole kernel
  map entry that contains the address. The loader first splits the text range
  off with an ordinary `mprotect()`.
- When the network is reconfigured (connection settings changed, Wi-Fi to
  Ethernet), listening sockets fail with errno 163, a Sony-specific code, and
  do not recover. The daemon's listeners reopen themselves
  (`tsd/listener.go`).

## Scheduling

This is the part to read before running any new test on a console.

Payload threads start as FIFO threads at priority 700 (`rtprio_thread`
reports class 10, priority 700; the range is 256 to 767 and lower runs
first). A FIFO thread that never blocks is never descheduled. A Go test with
a busy loop on each of the 16 logical cores froze the console completely:
the kernel still answered ping, but no user process ran, and only a
power-cycle recovered it.

`rtprio_thread(RTP_SET)` silently ignores priorities outside that range, so
the usual `RTP_PRIO_NORMAL, 0` does nothing and reports success. These are
accepted and read back: class 3 (time-sharing) at 767, class 2 (round-robin)
at 767, class 10 (FIFO) at 767. New threads inherit the setting.

The launcher sets class 3, priority 767 before Go starts, reads it back, and
refuses to start if it did not take. With that, Go's signal-based preemption
works across cores (4 spinning goroutines, 5 garbage collections in about
350 ms). Test builds can add a watchdog thread that kills the process after
a fixed time (`build-payload.ps1 -Watchdog`).

The "high" priority setting uses class 2 (round-robin) at priority 700
instead: equal to games and system threads, but equal-priority round-robin
threads take turns. With it, the same test passes (5 collections in about
300 ms) and the console stays responsive: the status page answered within
60 ms throughout while four goroutines spun.

## Rest mode

Observed once, for a rest of about a minute on Ethernet. The process is not
killed: it is frozen with the rest of the console and continues afterwards.

- Going to sleep, the network is taken down first. Every socket fails with
  errno 163 at the same moment: the status page listener, Tailscale's relay
  connection and its connection to the coordination server. The listener
  reopened at once, the daemon asked for a rebind, and Tailscale saw "all
  links down" and paused.
- Nothing is logged while the console sleeps, and it is not reachable on the
  tailnet.
- On waking, Tailscale's monitor noticed the jump in the clock, rebound its
  sockets, reconnected to its relay and had its endpoints back within about
  300 ms. The status page, forwarded TCP ports and the Remote Play UDP ports
  answered through the tailnet address afterwards without anything being
  restarted.

A rest of hours has not been tried, nor one on Wi-Fi.

## Home screen icon

`/user/app/TSCL00001/sce_sys/param.json` with `applicationCategoryType` 65536
and a `deeplinkUri`, plus `icon0.png`, registered with
`sceAppInstUtilAppInstallTitleDir`. The console opens the link in its
browser.

Linking `libSceAppInstUtil` alone leaves the payload stopped before it runs.
It needs `-lSceIpmi -lSceAppInstUtil -lSceUserService -lSceSystemService`, in
that order, as in the SDK's `install_app` sample.

## Known problems

- Once, switching the console from Wi-Fi to Ethernet during a Moonlight
  stream through the forwards froze the console. At that moment two daemon
  instances were running because of a bug that has since been fixed; whether
  that, the Moonlight client or something else caused the freeze is not
  known.
- Once, a first login completed in the browser but the daemon's follow-up
  request was answered with "auth path not found". The daemon now requests a
  new link when it sees that error; the recovery path has not been observed
  in practice.
- Tailscale's sockets recovered after rest mode took the network down and
  brought it back. A change of interface (Wi-Fi to Ethernet or back) while
  the daemon runs has not been observed since the rebind request was added.
