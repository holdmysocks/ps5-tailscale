/* PS5 payload entry point: prepare the process, then hand control to the
 * embedded Go program. Build with -DGO_IMAGE="path/to/program.bin". */

#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <unistd.h>

#include <sys/mman.h>

#include <ps5/kernel.h>

#include "goload.h"
#include "homeicon.h"

#ifndef GO_IMAGE
#error "GO_IMAGE must name the Go binary to embed"
#endif

/* How many OS threads may run Go code at once. The console has 16 logical
 * cores, but this is a background service and games need them more. */
#ifndef GO_MAXPROCS
#define GO_MAXPROCS "4"
#endif

extern const uint8_t go_image[];
extern const uint8_t go_image_end[];

__asm__(".section .rodata\n"
        ".balign 0x4000\n"
        ".global go_image\n"
        "go_image:\n"
        ".incbin \"" GO_IMAGE "\"\n"
        ".global go_image_end\n"
        "go_image_end:\n"
        ".text\n");

#define SYS_rtprio_thread 466
#define RTP_LOOKUP 0
#define RTP_SET 1
#define RTP_PRIO_REALTIME 2 /* round-robin */
#define RTP_PRIO_NORMAL 3   /* time-sharing */
#define PS5_PRIO_DEFAULT 700
#define PS5_PRIO_LOWEST 767

struct rtprio {
  unsigned short type;
  unsigned short prio;
};

static long
raw_syscall3(long n, long a, long b, long c) {
  unsigned char failed;
  __asm__ volatile("syscall; setc %1" : "+a"(n), "=q"(failed) : "D"(a), "S"(b), "d"(c) : "rcx", "r11", "memory");
  return failed ? -n : n;
}

/* Payload threads start as FIFO threads at the PS5's default priority (class
 * 10, priority 700; lower numbers run first, 767 is the lowest). A FIFO
 * thread that never blocks is never descheduled, so a busy loop on every core
 * freezes the whole console.
 *
 * Before any Go thread exists, move to the time-sharing class at the lowest
 * priority, where nothing this process does can keep the system's own threads
 * off the CPU. Threads created later inherit the setting. The kernel ignores
 * priorities outside its own range without reporting an error, so the result
 * is read back. Round-robin at the lowest priority is the fallback.
 *
 * With the "high" priority setting the process instead becomes round-robin
 * at the default priority: it then competes with games on equal terms, but
 * equal-priority round-robin threads take turns, so even then a thread that
 * never blocks cannot shut the others out. */
static int
high_priority_requested(void) {
  char buf[16] = {0};
  FILE *f = fopen("/data/tailscale/priority", "r");

  if (!f) {
    return 0;
  }
  fgets(buf, sizeof(buf), f);
  fclose(f);
  return !strncmp(buf, "high", 4);
}

static int
leave_realtime_class(void) {
  static const struct rtprio choices[] = {
      {RTP_PRIO_REALTIME, PS5_PRIO_DEFAULT}, /* only with the "high" setting */
      {RTP_PRIO_NORMAL, PS5_PRIO_LOWEST},
      {RTP_PRIO_REALTIME, PS5_PRIO_LOWEST},
  };
  struct rtprio before = {0}, after = {0};
  int ok = 0;

  raw_syscall3(SYS_rtprio_thread, RTP_LOOKUP, 0, (long)&before);
  for (size_t i = high_priority_requested() ? 0 : 1; i < sizeof(choices) / sizeof(choices[0]) && !ok; i++) {
    struct rtprio want = choices[i];
    raw_syscall3(SYS_rtprio_thread, RTP_SET, 0, (long)&want);
    raw_syscall3(SYS_rtprio_thread, RTP_LOOKUP, 0, (long)&after);
    ok = after.type == choices[i].type && after.prio == choices[i].prio;
  }
#ifdef GOLOAD_DEBUG
  fprintf(stderr, "launcher: scheduling class %u/%u -> %u/%u\n", before.type, before.prio, after.type, after.prio);
#else
  (void)before;
#endif
  return ok ? 0 : -1;
}

#ifdef GOLOAD_WATCHDOG
#include <pthread.h>
#include <signal.h>

/* Test builds only: kill the process after a fixed time, from a thread the Go
 * scheduler knows nothing about, so that a hung test cannot outlive its
 * welcome. */
static void *
watchdog_main(void *arg) {
  sleep(GOLOAD_WATCHDOG);
  static const char msg[] = "launcher: watchdog expired, killing the process\n";
  write(2, msg, sizeof(msg) - 1);
  kill(getpid(), SIGKILL);
  return 0;
}

static void
start_watchdog(void) {
  sigset_t all, old;
  pthread_t thread;

  /* The thread must never run a Go signal handler, so it starts with every
   * signal blocked. */
  sigfillset(&all);
  pthread_sigmask(SIG_SETMASK, &all, &old);
  pthread_create(&thread, 0, watchdog_main, 0);
  pthread_sigmask(SIG_SETMASK, &old, 0);
}
#endif

int
main(int argc, char **argv) {
  pid_t pid = getpid();
  intptr_t rootvnode;

  /* Run as root outside the sandbox so /data and the network are reachable. */
  if ((rootvnode = kernel_get_root_vnode())) {
    kernel_set_proc_rootdir(pid, rootvnode);
    kernel_set_proc_jaildir(pid, 0);
  }
  kernel_set_ucred_uid(pid, 0);
  kernel_set_ucred_ruid(pid, 0);
  kernel_set_ucred_svuid(pid, 0);
  kernel_set_ucred_rgid(pid, 0);
  kernel_set_ucred_svgid(pid, 0);

  home_icon_install_once();

  if (leave_realtime_class()) {
    /* Without this a runaway goroutine could hang the console, so do not
     * take the chance. */
    fprintf(stderr, "launcher: could not leave the real-time scheduling class; not starting\n");
    return 1;
  }

#ifdef GOLOAD_WATCHDOG
  start_watchdog();
#endif

  static char *envp[] = {
      "HOME=/data/tailscale",
      "TMPDIR=/data/tailscale/tmp",
      "GOMAXPROCS=" GO_MAXPROCS,
#ifdef GO_DEBUG
      "GODEBUG=" GO_DEBUG,
#endif
      "PS5=1",
      0,
  };

  return goload_run(go_image, (size_t)(go_image_end - go_image), argv, envp);
}
