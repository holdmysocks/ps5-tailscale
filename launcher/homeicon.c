/* Installs the home screen icon the first time the payload runs.
 *
 * The icon is installed by a separate small payload (appicon/), embedded
 * here and handed to the ELF loader on this console, so that the system
 * libraries it needs never end up in this process. Build with
 * -DICON_HELPER="path/to/appicon.elf"; without it this file does nothing. */

#include "homeicon.h"

#ifdef ICON_HELPER

#include "report.h"

#include <fcntl.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <unistd.h>

#include <arpa/inet.h>
#include <netinet/in.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/time.h>

#define DATA_DIR "/data/tailscale"
/* Records that the icon has been installed, and which version of it. Once
 * it exists the icon is left alone, so an icon the user deletes from the
 * home screen stays deleted. Remove the file to get the icon back. */
#define ICON_MARKER DATA_DIR "/icon-installed"
#define ICON_VERSION "1\n"
#define ICON_HELPER_FILE DATA_DIR "/icon-helper.elf"
#define LOADER_PORT 9021

extern const uint8_t icon_helper[];
extern const uint8_t icon_helper_end[];

__asm__(".section .rodata\n"
        ".balign 16\n"
        ".global icon_helper\n"
        "icon_helper:\n"
        ".incbin \"" ICON_HELPER "\"\n"
        ".global icon_helper_end\n"
        "icon_helper_end:\n"
        ".text\n");

static int
marker_is_current(void) {
  char buf[16] = {0};
  int fd = open(ICON_MARKER, O_RDONLY);

  if (fd < 0) {
    return 0;
  }
  read(fd, buf, sizeof(buf) - 1);
  close(fd);
  return !strcmp(buf, ICON_VERSION);
}

/* Leave a copy of the helper where the daemon can find it. Uninstall runs it
 * again, switched to removing the icon. */
static void
save_helper(void) {
  size_t size = icon_helper_end - icon_helper;
  struct stat st;
  int fd;

  if (!stat(ICON_HELPER_FILE, &st) && (size_t)st.st_size == size) {
    return;
  }
  if ((fd = open(ICON_HELPER_FILE, O_WRONLY | O_CREAT | O_TRUNC, 0644)) < 0) {
    return;
  }
  for (size_t done = 0; done < size;) {
    ssize_t n = write(fd, icon_helper + done, size - done);
    if (n <= 0) {
      break;
    }
    done += n;
  }
  close(fd);
}

void
home_icon_install_once(void) {
  struct sockaddr_in addr = {0};
  struct timeval tv = {1, 0};
  const uint8_t *data = icon_helper;
  size_t left = icon_helper_end - icon_helper;
  char reply[512] = {0};
  size_t got = 0;
  int fd;

  mkdir(DATA_DIR, 0755);
  save_helper();
  if (marker_is_current()) {
    return;
  }

  if ((fd = socket(AF_INET, SOCK_STREAM, 0)) < 0) {
    return;
  }
  addr.sin_family = AF_INET;
  addr.sin_port = htons(LOADER_PORT);
  addr.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
  if (connect(fd, (struct sockaddr *)&addr, sizeof(addr))) {
    /* No ELF loader on the usual port: go without an icon this time. */
    report_log("launcher: home screen icon not installed: no ELF loader on port %d", LOADER_PORT);
    close(fd);
    return;
  }
  while (left > 0) {
    ssize_t n = write(fd, data, left);
    if (n <= 0) {
      close(fd);
      return;
    }
    data += n;
    left -= n;
  }

  /* The helper reports "icon: ok" or what went wrong, then exits, which
   * closes the connection. Give it 20 seconds. */
  setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof(tv));
  for (int tries = 0; tries < 20 && got < sizeof(reply) - 1; tries++) {
    const char *line = strstr(reply, "icon: ");
    if (line && strchr(line, '\n')) {
      break;
    }
    ssize_t n = read(fd, reply + got, sizeof(reply) - 1 - got);
    if (n == 0) {
      break;
    }
    if (n > 0) {
      got += n;
    }
  }
  close(fd);

  if (strstr(reply, "icon: ok")) {
    if ((fd = open(ICON_MARKER, O_WRONLY | O_CREAT | O_TRUNC, 0644)) >= 0) {
      write(fd, ICON_VERSION, sizeof(ICON_VERSION) - 1);
      close(fd);
    }
    report_log("launcher: home screen icon installed");
  } else {
    report_log("launcher: home screen icon not installed: %s", got ? reply : "no reply from the helper");
  }
}

#else

void
home_icon_install_once(void) {
}

#endif
