/* List processes and kill stray test payloads.
 *
 * Only processes whose main thread is named "payload.elf" (what the ELF
 * loader calls anything sent to it as raw bytes) and whose pid is at least
 * MIN_PID are killed, so payloads the user started before this development
 * session are left alone. */

#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#include <sys/sysctl.h>
#include <sys/types.h>
#include <sys/user.h>

#ifndef MIN_PID
#define MIN_PID 104
#endif

int
main(void) {
  int mib[4] = {CTL_KERN, KERN_PROC, KERN_PROC_PROC, 0};
  size_t size = 0;
  pid_t self = getpid();
  char *buf;

  setvbuf(stdout, 0, _IONBF, 0);
  if (sysctl(mib, 4, 0, &size, 0, 0) || !(buf = malloc(size)) || sysctl(mib, 4, buf, &size, 0, 0)) {
    perror("sysctl");
    return 1;
  }

  for (char *p = buf; p < buf + size;) {
    struct kinfo_proc *ki = (struct kinfo_proc *)p;
    const char *tdname = p + 447;
    p += ki->ki_structsize;

#ifdef LIST_ONLY
    int stray = 0;
    {
#elif defined(KILL_PID)
    /* Kill exactly one process, and only if it is a payload. */
    int stray = ki->ki_pid == KILL_PID && ki->ki_pid != self && !strcmp(tdname, "payload.elf");
    if (stray) {
#else
    int stray = ki->ki_pid >= MIN_PID && ki->ki_pid != self && !strcmp(tdname, "payload.elf");
    if (ki->ki_pid >= MIN_PID - 40 || stray) {
#endif
      printf("%6d %-20s %-20s rss=%ldMB threads=%d cpu=%.2fs stat=%d wait=%.8s%s\n", ki->ki_pid, ki->ki_comm,
             tdname, (long)(ki->ki_rssize * 16384L >> 20), ki->ki_numthreads, ki->ki_runtime / 1e6, (int)ki->ki_stat,
             ki->ki_wmesg, stray ? "  <- killing" : "");
    }
    if (stray) {
      if (kill(ki->ki_pid, SIGKILL)) {
        perror("  kill");
      }
    }
  }
  free(buf);
  return 0;
}
