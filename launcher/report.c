/* The launcher's log and its way of telling the user that a start failed. */

#include <fcntl.h>
#include <stdarg.h>
#include <stdio.h>
#include <string.h>
#include <time.h>
#include <unistd.h>

#include <sys/stat.h>

#include <ps5/kernel.h>

#include "report.h"

#define DATA_DIR "/data/tailscale"
/* The log is started afresh once it has grown past this. */
#define LOG_MAX_SIZE (64 * 1024)

typedef struct {
  char unused[45];
  char message[3075];
} notify_request_t;

int sceKernelSendNotificationRequest(int, notify_request_t *, size_t, int);

static int
log_open(void) {
  struct stat st;
  int flags = O_WRONLY | O_CREAT | O_APPEND;

  mkdir(DATA_DIR, 0755);
  if (!stat(LAUNCHER_LOG, &st) && st.st_size > LOG_MAX_SIZE) {
    flags |= O_TRUNC;
  }
  return open(LAUNCHER_LOG, flags, 0644);
}

static void
log_line(const char *line) {
  char stamp[32] = "";
  time_t now = time(0);
  struct tm tm;
  int fd = log_open();

  if (fd < 0) {
    return;
  }
  if (gmtime_r(&now, &tm)) {
    strftime(stamp, sizeof(stamp), "%Y-%m-%d %H:%M:%S UTC ", &tm);
  }
  write(fd, stamp, strlen(stamp));
  write(fd, line, strlen(line));
  write(fd, "\n", 1);
  close(fd);
}

void
report_begin(void) {
  char line[128];
  unsigned fw = kernel_get_fw_version();

  snprintf(line, sizeof(line), "launcher: starting, firmware %x.%02x, pid %d", fw >> 24, (fw >> 16) & 0xff,
           (int)getpid());
  log_line(line);
}

void
report_log(const char *fmt, ...) {
  char line[512];
  va_list ap;

  va_start(ap, fmt);
  vsnprintf(line, sizeof(line), fmt, ap);
  va_end(ap);
  fprintf(stderr, "%s\n", line);
  log_line(line);
}

void
report_fail(const char *fmt, ...) {
  static notify_request_t req;
  char line[512];
  va_list ap;

  va_start(ap, fmt);
  vsnprintf(line, sizeof(line), fmt, ap);
  va_end(ap);
  fprintf(stderr, "%s\n", line);
  log_line(line);

  memset(&req, 0, sizeof(req));
  snprintf(req.message, sizeof(req.message), "Tailscale did not start:\n%s\nDetails: " LAUNCHER_LOG, line);
  sceKernelSendNotificationRequest(0, &req, sizeof(req), 0);
}

void
report_capture_stderr(void) {
  int fd = log_open();

  if (fd < 0) {
    return;
  }
  fflush(stderr);
  dup2(fd, 2);
  close(fd);
}
