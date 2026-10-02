/* Probe which raw FreeBSD syscalls the PS5 kernel accepts, and how the
 * payload process looks (page size, fds, memory behaviour). Output goes to
 * stdout, which the ELF loader pipes back over the socket. */

#include <errno.h>
#include <fcntl.h>
#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#include <sys/mman.h>
#include <sys/stat.h>
#include <sys/sysctl.h>
#include <sys/types.h>
#include <sys/utsname.h>

/* Raw syscall issued from payload memory (not from libkernel), the way the Go
 * runtime will do it. Returns the kernel result, or -errno when carry is set. */
static long
raw(long n, long a, long b, long c, long d, long e, long f) {
  long ret;
  unsigned char cf;
  register long r10 __asm__("r10") = d;
  register long r8 __asm__("r8") = e;
  register long r9 __asm__("r9") = f;
  __asm__ volatile("syscall; setc %1"
                   : "+a"(n), "=q"(cf)
                   : "D"(a), "S"(b), "d"(c), "r"(r10), "r"(r8), "r"(r9)
                   : "rcx", "r11", "memory");
  ret = n;
  return cf ? -ret : ret;
}

#define R0(n) raw(n, 0, 0, 0, 0, 0, 0)

static void
report(const char *name, long ret) {
  if (ret < 0) {
    printf("  %-28s FAIL errno=%ld (%s)\n", name, -ret, strerror((int)-ret));
  } else {
    printf("  %-28s ok    ret=%ld (0x%lx)\n", name, ret, ret);
  }
}

static volatile int got_sig;
static void
on_sig(int sig, siginfo_t *info, void *ctx) {
  got_sig = sig;
}

int
main(int argc, char **argv) {
  setvbuf(stdout, 0, _IONBF, 0);
  printf("== sysprobe ==\n");
  printf("argc=%d argv0=%s pid=%d uid=%d\n", argc, argc ? argv[0] : "-", getpid(), getuid());

  struct utsname un;
  if (!uname(&un)) {
    printf("uname: %s %s %s %s\n", un.sysname, un.release, un.version, un.machine);
  }

  /* sysctls */
  {
    int mib[2] = {CTL_HW, HW_PAGESIZE};
    int v = 0;
    size_t l = sizeof(v);
    long r = raw(202, (long)mib, 2, (long)&v, (long)&l, 0, 0);
    printf("sysctl hw.pagesize -> r=%ld v=%d\n", r, v);
    mib[1] = HW_NCPU;
    v = 0;
    l = sizeof(v);
    r = raw(202, (long)mib, 2, (long)&v, (long)&l, 0, 0);
    printf("sysctl hw.ncpu -> r=%ld v=%d\n", r, v);
    int mib2[2] = {CTL_KERN, 37 /* KERN_ARND */};
    unsigned char rnd[16] = {0};
    l = sizeof(rnd);
    r = raw(202, (long)mib2, 2, (long)rnd, (long)&l, 0, 0);
    printf("sysctl kern.arandom -> r=%ld len=%zu first=%02x%02x%02x%02x\n", r, l, rnd[0], rnd[1], rnd[2], rnd[3]);
    int osrel = 0;
    l = sizeof(osrel);
    if (!sysctlbyname("kern.osreldate", &osrel, &l, 0, 0)) {
      printf("kern.osreldate=%d\n", osrel);
    } else {
      printf("kern.osreldate: errno=%d\n", errno);
    }
    int maxcpus = 0;
    l = sizeof(maxcpus);
    if (!sysctlbyname("kern.smp.maxcpus", &maxcpus, &l, 0, 0)) {
      printf("kern.smp.maxcpus=%d\n", maxcpus);
    } else {
      printf("kern.smp.maxcpus: errno=%d\n", errno);
    }
  }

  printf("raw syscalls from payload text:\n");
  report("getpid(20)", R0(20));
  report("issetugid(253)", R0(253));
  report("sched_yield(331)", R0(331));
  {
    long tid = 0;
    report("thr_self(432)", raw(432, (long)&tid, 0, 0, 0, 0, 0));
    printf("    tid=%ld\n", tid);
  }
  {
    struct timespec ts = {0};
    report("clock_gettime(232,MONO=4)", raw(232, 4, (long)&ts, 0, 0, 0, 0));
    printf("    mono=%ld.%09ld\n", (long)ts.tv_sec, ts.tv_nsec);
    report("clock_gettime(232,REAL=0)", raw(232, 0, (long)&ts, 0, 0, 0, 0));
    printf("    real=%ld.%09ld\n", (long)ts.tv_sec, ts.tv_nsec);
  }
  {
    /* Numbers >= 532 are Sony syscalls on this kernel (pipe2, accept4 and
     * friends do not exist), so they are deliberately not probed. */
    long r = raw(42, 0, 0, 0, 0, 0, 0); /* pipe: fds in rax/rdx */
    report("pipe(42)", r);
    if (r >= 0) {
      close((int)r);
    }
  }
  {
    long kq = R0(362);
    report("kqueue(362)", kq);
    struct timespec zero = {0};
    char evbuf[256];
    report("kevent(363) poll", raw(363, kq, 0, 0, (long)evbuf, 1, (long)&zero));
    close((int)kq);
  }
  {
    unsigned char mask[64] = {0};
    long r = raw(487, 1 /* CPU_LEVEL_WHICH */, 2 /* CPU_WHICH_PID */, -1, 8, (long)mask, 0);
    report("cpuset_getaffinity(487)", r);
    printf("    mask=%02x%02x\n", mask[1], mask[0]);
  }
  {
    stack_t ss = {0};
    report("sigaltstack(53) query", raw(53, 0, (long)&ss, 0, 0, 0, 0));
    printf("    ss_sp=%p size=%zu flags=%d\n", ss.ss_sp, ss.ss_size, ss.ss_flags);
    sigset_t set;
    report("sigprocmask(340) query", raw(340, 1, 0, (long)&set, 0, 0, 0));
    struct sigaction old;
    report("sigaction(416) query URG", raw(416, SIGURG, 0, (long)&old, 0, 0, 0));
    report("sigaction(416) query SEGV", raw(416, SIGSEGV, 0, (long)&old, 0, 0, 0));
    printf("    SEGV handler=%p flags=%x\n", (void *)old.sa_sigaction, old.sa_flags);
  }
  {
    char cwd[256] = {0};
    report("__getcwd(326)", raw(326, (long)cwd, sizeof(cwd), 0, 0, 0, 0));
    printf("    cwd=%s\n", cwd);
  }
  {
    struct stat st;
    report("stat(188) /data", raw(188, (long)"/data", (long)&st, 0, 0, 0, 0));
    printf("    sizeof(struct stat)=%zu mode=%o\n", sizeof(st), st.st_mode);
    report("lstat(190) /data", raw(190, (long)"/data", (long)&st, 0, 0, 0, 0));
    report("fstatat(493) /data", raw(493, AT_FDCWD, (long)"/data", (long)&st, 0, 0, 0));
    long fd = raw(499, AT_FDCWD, (long)"/data", O_RDONLY, 0, 0, 0);
    report("openat(499) /data", fd);
    if (fd >= 0) {
      close((int)fd);
    }
    fd = raw(5, (long)"/data", O_RDONLY | 0x00020000 /* O_DIRECTORY */, 0, 0, 0, 0);
    report("open(5) /data O_DIRECTORY", fd);
    if (fd >= 0) {
      char dents[1024];
      long base = 0;
      report("fstat(189)", raw(189, fd, (long)&st, 0, 0, 0, 0));
      report("getdirentries(196)", raw(196, fd, (long)dents, sizeof(dents), (long)&base, 0, 0));
      report("getdents(272)", raw(272, fd, (long)dents, sizeof(dents), 0, 0, 0));
      report("fstatfs(397)", raw(397, fd, (long)dents, 0, 0, 0, 0));
      close((int)fd);
    }
    char link[256] = {0};
    report("readlink(58) /dev/stdout", raw(58, (long)"/dev/stdout", (long)link, sizeof(link), 0, 0, 0));
    report("access(33) /dev/urandom", raw(33, (long)"/dev/urandom", 0, 0, 0, 0, 0));
    report("access(33) /dev/random", raw(33, (long)"/dev/random", 0, 0, 0, 0, 0));
    report("access(33) /etc/resolv.conf", raw(33, (long)"/etc/resolv.conf", 0, 0, 0, 0, 0));
    report("access(33) /data/tailscale", raw(33, (long)"/data/tailscale", 0, 0, 0, 0, 0));
  }
  {
    long s = raw(97, 2, 1, 0, 0, 0, 0);
    report("socket(97) TCP", s);
    if (s >= 0) {
      close((int)s);
    }
    s = raw(97, 2, 1 | 0x10000000 | 0x20000000, 0, 0, 0, 0);
    report("socket TCP|CLOEXEC|NONBLOCK", s);
    if (s >= 0) {
      close((int)s);
    }
    s = raw(97, 28, 2, 0, 0, 0, 0);
    report("socket(97) UDP6", s);
    if (s >= 0) {
      close((int)s);
    }
    s = raw(97, 17 /* AF_ROUTE */, 3, 0, 0, 0, 0);
    report("socket(97) AF_ROUTE", s);
    if (s >= 0) {
      close((int)s);
    }
    s = raw(97, 1, 1, 0, 0, 0, 0);
    report("socket(97) AF_UNIX", s);
    if (s >= 0) {
      close((int)s);
    }
    int mib[6] = {CTL_NET, 17 /* AF_ROUTE */, 0, 0, 3 /* NET_RT_IFLIST */, 0};
    size_t l = 0;
    long r = raw(202, (long)mib, 6, 0, (long)&l, 0, 0);
    report("sysctl NET_RT_IFLIST size", r);
    printf("    need=%zu\n", l);
    mib[4] = 1; /* NET_RT_DUMP */
    l = 0;
    r = raw(202, (long)mib, 6, 0, (long)&l, 0, 0);
    report("sysctl NET_RT_DUMP size", r);
    printf("    need=%zu\n", l);
  }

  printf("memory:\n");
  {
    long p = raw(477, 0, 64 << 20, PROT_NONE, MAP_ANON | MAP_PRIVATE, -1, 0);
    report("mmap 64MB PROT_NONE", p);
    if (p > 0) {
      long q = raw(477, p, 1 << 20, PROT_READ | PROT_WRITE, MAP_ANON | MAP_PRIVATE | MAP_FIXED, -1, 0);
      report("  remap 1MB RW MAP_FIXED", q);
      if (q > 0) {
        memset((void *)q, 0x5a, 1 << 20);
        printf("    touched ok\n");
        report("  madvise FREE(5)", raw(75, q, 1 << 20, 5, 0, 0, 0));
      }
      report("  munmap", raw(73, p, 64 << 20, 0, 0, 0, 0));
    }
    p = raw(477, 0x00c000000000, 64 << 20, PROT_NONE, MAP_ANON | MAP_PRIVATE, -1, 0);
    report("mmap hint 0xc000000000", p);
    if (p > 0) {
      raw(73, p, 64 << 20, 0, 0, 0, 0);
    }
    p = raw(477, 0, 4L << 30, PROT_NONE, MAP_ANON | MAP_PRIVATE, -1, 0);
    report("mmap 4GB PROT_NONE", p);
    if (p > 0) {
      raw(73, p, 4L << 30, 0, 0, 0, 0);
    }
    p = raw(477, 0, 512 << 20, PROT_READ | PROT_WRITE, MAP_ANON | MAP_PRIVATE, -1, 0);
    report("mmap 512MB RW (untouched)", p);
    if (p > 0) {
      raw(73, p, 512 << 20, 0, 0, 0, 0);
    }
    p = raw(477, 0, 1 << 20, PROT_READ | PROT_WRITE | PROT_EXEC, MAP_ANON | MAP_PRIVATE, -1, 0);
    report("mmap 1MB RWX", p);
    if (p > 0) {
      raw(73, p, 1 << 20, 0, 0, 0, 0);
    }
  }

  printf("signals:\n");
  {
    struct sigaction sa = {0};
    sa.sa_sigaction = on_sig;
    sa.sa_flags = SA_SIGINFO | SA_RESTART;
    sigfillset(&sa.sa_mask);
    report("sigaction(416) set USR1", raw(416, SIGUSR1, (long)&sa, 0, 0, 0, 0));
    long tid = 0;
    raw(432, (long)&tid, 0, 0, 0, 0, 0);
    report("thr_kill(433) USR1", raw(433, tid, SIGUSR1, 0, 0, 0, 0));
    printf("    handler ran: got_sig=%d\n", got_sig);
  }

  printf("fds:\n");
  for (int fd = 0; fd < 16; fd++) {
    struct stat st;
    if (!fstat(fd, &st)) {
      printf("  fd %d mode=%o\n", fd, st.st_mode);
    }
  }

  printf("== done ==\n");
  return 0;
}
