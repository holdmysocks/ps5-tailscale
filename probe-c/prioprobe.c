/* Find out which scheduling class/priority changes the PS5 kernel accepts
 * for a payload thread, and whether new threads inherit them. */

#include <pthread.h>
#include <sched.h>
#include <stdio.h>
#include <string.h>
#include <unistd.h>

struct rtprio {
  unsigned short type;
  unsigned short prio;
};

static long
raw3(long n, long a, long b, long c) {
  unsigned char failed;
  __asm__ volatile("syscall; setc %1" : "+a"(n), "=q"(failed) : "D"(a), "S"(b), "d"(c) : "rcx", "r11", "memory");
  return failed ? -n : n;
}

#define SYS_rtprio_thread 466

static struct rtprio
lookup(void) {
  struct rtprio r = {0};
  raw3(SYS_rtprio_thread, 0, 0, (long)&r);
  return r;
}

static void *
child(void *arg) {
  struct rtprio r = lookup();
  printf("    new raw-inherited pthread: %u/%u\n", r.type, r.prio);
  return 0;
}

int
main(void) {
  static const struct rtprio tries[] = {
      {3, 0}, {3, 20}, {4, 0}, {4, 31}, {2, 767}, {2, 31}, {10, 767}, {10, 760}, {10, 768}, {10, 700},
      {2, 700}, {3, 767}, {6, 767},
  };
  struct sched_param sp;
  int policy = -1;
  struct rtprio r = lookup();

  setvbuf(stdout, 0, _IONBF, 0);
  printf("start: %u/%u\n", r.type, r.prio);
  if (!pthread_getschedparam(pthread_self(), &policy, &sp)) {
    printf("pthread policy=%d prio=%d (FIFO=%d RR=%d OTHER=%d)\n", policy, sp.sched_priority, SCHED_FIFO, SCHED_RR,
           SCHED_OTHER);
  }

  for (size_t i = 0; i < sizeof(tries) / sizeof(tries[0]); i++) {
    struct rtprio want = tries[i];
    long err = raw3(SYS_rtprio_thread, 1, 0, (long)&want);
    r = lookup();
    printf("set %2u/%-3u -> ret=%ld now %u/%u\n", tries[i].type, tries[i].prio, err, r.type, r.prio);
  }

  /* Leave it at the lowest setting that stuck and see what a new thread gets. */
  struct rtprio low = {10, 767};
  raw3(SYS_rtprio_thread, 1, 0, (long)&low);
  r = lookup();
  printf("final: %u/%u\n", r.type, r.prio);
  pthread_t t;
  if (!pthread_create(&t, 0, child, 0)) {
    pthread_join(t, 0);
  }

  for (int pol = 0; pol <= 3; pol++) {
    sp.sched_priority = 767;
    int e = pthread_setschedparam(pthread_self(), pol, &sp);
    r = lookup();
    printf("pthread_setschedparam(policy=%d, 767) -> %d, now %u/%u\n", pol, e, r.type, r.prio);
  }
  return 0;
}
