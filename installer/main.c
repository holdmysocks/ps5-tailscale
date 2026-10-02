/* Tailscale installer payload for jailbroken PS5s.
 *
 * Stores the Tailscale daemon payload on the console, registers it with the
 * payload autoloader if one is set up, adds a home screen icon that opens
 * the status page, and starts the daemon through the ELF loader on this
 * console. Build with tools\build-installer.ps1, which sets DAEMON_ELF and
 * ASSET_DIR and links the system libraries the app installer needs. */

#include <errno.h>
#include <fcntl.h>
#include <stdarg.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#include <arpa/inet.h>
#include <netinet/in.h>
#include <sys/select.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/time.h>

#include <ps5/kernel.h>

#ifndef DAEMON_ELF
#error "DAEMON_ELF must name the daemon payload to embed"
#endif

#define DATA_DIR "/data/tailscale"
#define DAEMON_NAME "tailscale.elf"
#define LOADER_PORT 9021

extern const uint8_t daemon_elf[];
extern const uint8_t daemon_elf_end[];

__asm__(".section .rodata\n"
        ".balign 16\n"
        ".global daemon_elf\n"
        "daemon_elf:\n"
        ".incbin \"" DAEMON_ELF "\"\n"
        ".global daemon_elf_end\n"
        "daemon_elf_end:\n"
        ".text\n");

/* The home screen launcher: a media app whose only content is a link, which
 * the console opens in its browser. ASSET_DIR is set by the build. */
#ifndef ASSET_DIR
#error "ASSET_DIR must name the folder with param.json and icon0.png"
#endif
#define LAUNCHER_TITLE_ID "TSCL00001"
#define LAUNCHER_DIR "/user/app/" LAUNCHER_TITLE_ID

#define INCASSET(name, file)                                \
  __asm__(".section .rodata\n"                              \
          ".balign 16\n"                                    \
          ".global " #name "\n" #name ":\n"                 \
          ".incbin \"" file "\"\n"                          \
          ".global " #name "_end\n" #name "_end:\n"         \
          ".text\n");                                       \
  extern const uint8_t name[];                              \
  extern const uint8_t name##_end[];

INCASSET(launcher_param_json, ASSET_DIR "/param.json")
INCASSET(launcher_icon_png, ASSET_DIR "/icon0.png")

int sceAppInstUtilInitialize(void);
int sceAppInstUtilTerminate(void);
int sceAppInstUtilAppInstallAll(void *);

/* Where payload autoloaders keep their load order, and where the daemon has
 * to be stored for each. Keep in sync with tsd/autostart.go. */
static const struct autoloader {
  const char *name;
  const char *list;        /* load order: one file name per line */
  const char *payload_dir; /* where the payload goes */
} autoloaders[] = {
    /* Payload Manager finds entries by file name anywhere below /data/pldmgr
     * and keeps each payload in a folder of its own. */
    {"Payload Manager", "/data/pldmgr/autoload.txt", "/data/pldmgr/payloads/Tailscale"},
    {"PS5 autoloader", "/data/ps5_autoloader/autoload.txt", "/data/ps5_autoloader"},
    {"PS5 autoloader (usb0)", "/mnt/usb0/ps5_autoloader/autoload.txt", "/mnt/usb0/ps5_autoloader"},
    {"PS5 autoloader (usb1)", "/mnt/usb1/ps5_autoloader/autoload.txt", "/mnt/usb1/ps5_autoloader"},
    {"PS5 autoloader (usb2)", "/mnt/usb2/ps5_autoloader/autoload.txt", "/mnt/usb2/ps5_autoloader"},
    {"PS5 autoloader (usb3)", "/mnt/usb3/ps5_autoloader/autoload.txt", "/mnt/usb3/ps5_autoloader"},
};

typedef struct notify_request {
  char useless1[45];
  char message[3075];
} notify_request_t;

int sceKernelSendNotificationRequest(int, notify_request_t *, size_t, int);

static void
notify(const char *fmt, ...) {
  notify_request_t req;
  va_list args;

  memset(&req, 0, sizeof(req));
  va_start(args, fmt);
  vsnprintf(req.message, sizeof(req.message), fmt, args);
  va_end(args);
  sceKernelSendNotificationRequest(0, &req, sizeof(req), 0);
}

static int
write_all(int fd, const uint8_t *data, size_t size) {
  while (size > 0) {
    ssize_t n = write(fd, data, size);
    if (n < 0) {
      if (errno == EINTR) {
        continue;
      }
      return -1;
    }
    data += n;
    size -= n;
  }
  return 0;
}

/* Write a file through a temporary name so a failed write never leaves a
 * truncated payload where the autoloader would pick it up. */
static int
write_file(const char *path, const uint8_t *data, size_t size) {
  char tmp[512];
  int fd;

  snprintf(tmp, sizeof(tmp), "%s.tmp", path);
  if ((fd = open(tmp, O_WRONLY | O_CREAT | O_TRUNC, 0755)) < 0) {
    return -1;
  }
  if (write_all(fd, data, size)) {
    close(fd);
    unlink(tmp);
    return -1;
  }
  close(fd);
  if (rename(tmp, path)) {
    unlink(tmp);
    return -1;
  }
  return 0;
}

static int
install_daemon(const char *dir) {
  char path[512];

  mkdir(dir, 0755);
  snprintf(path, sizeof(path), "%s/%s", dir, DAEMON_NAME);
  if (write_file(path, daemon_elf, daemon_elf_end - daemon_elf)) {
    printf("  could not write %s: %s\n", path, strerror(errno));
    return -1;
  }
  printf("  wrote %s (%.1f MB)\n", path, (daemon_elf_end - daemon_elf) / 1048576.0);
  return 0;
}

/* Report whether the load order already lists the daemon on a line of its own. */
static int
autoload_lists_daemon(const char *text) {
  size_t namelen = strlen(DAEMON_NAME);

  for (const char *line = text; line && *line;) {
    const char *end = strchr(line, '\n');
    size_t len = end ? (size_t)(end - line) : strlen(line);
    while (len > 0 && (line[len - 1] == '\r' || line[len - 1] == ' ' || line[len - 1] == '\t')) {
      len--;
    }
    if (len == namelen && !strncmp(line, DAEMON_NAME, namelen)) {
      return 1;
    }
    line = end ? end + 1 : 0;
  }
  return 0;
}

/* If this autoloader is set up on the console, store the daemon for it and
 * add it to the end of the load order. Returns 1 if registered, 0 if the
 * autoloader is not present, -1 on error. */
static int
register_autoload(const struct autoloader *al) {
  struct stat st;
  char *text;
  size_t got;
  FILE *f;

  if (stat(al->list, &st)) {
    return 0;
  }
  printf("%s:\n", al->name);
  if (install_daemon(al->payload_dir)) {
    return -1;
  }

  if (!(f = fopen(al->list, "rb"))) {
    printf("  could not read %s: %s\n", al->list, strerror(errno));
    return -1;
  }
  text = calloc(1, st.st_size + 1);
  got = fread(text, 1, st.st_size, f);
  fclose(f);
  text[got] = 0;

  if (autoload_lists_daemon(text)) {
    printf("  %s already lists %s\n", al->list, DAEMON_NAME);
  } else {
    if (!(f = fopen(al->list, "ab"))) {
      printf("  could not update %s: %s\n", al->list, strerror(errno));
      free(text);
      return -1;
    }
    if (got > 0 && text[got - 1] != '\n') {
      fputc('\n', f);
    }
    fputs(DAEMON_NAME "\n", f);
    fclose(f);
    printf("  added %s to the end of %s\n", DAEMON_NAME, al->list);
  }
  free(text);
  return 1;
}

/* Report whether the file at path already has exactly these contents. */
static int
file_matches(const char *path, const uint8_t *data, size_t size) {
  struct stat st;
  uint8_t *buf;
  int same = 0;
  FILE *f;

  if (stat(path, &st) || (size_t)st.st_size != size || !(f = fopen(path, "rb"))) {
    return 0;
  }
  if ((buf = malloc(size))) {
    same = fread(buf, 1, size, f) == size && !memcmp(buf, data, size);
    free(buf);
  }
  fclose(f);
  return same;
}

/* Put a "Tailscale" icon on the home screen that opens the status page in
 * the console's browser. Does nothing if it is already there and current.
 * Returns 0 on success. */
static int
install_launcher_app(void) {
  int (*install_title_dir)(const char *, const char *, void *) = 0;
  size_t param_size = launcher_param_json_end - launcher_param_json;
  size_t icon_size = launcher_icon_png_end - launcher_icon_png;
  uint32_t handle;
  int err;

  if (file_matches(LAUNCHER_DIR "/sce_sys/param.json", launcher_param_json, param_size) &&
      file_matches(LAUNCHER_DIR "/sce_sys/icon0.png", launcher_icon_png, icon_size)) {
    printf("  already installed\n");
    return 0;
  }

  if ((err = sceAppInstUtilInitialize())) {
    printf("  sceAppInstUtilInitialize failed: 0x%08x\n", err);
    return -1;
  }
  mkdir(LAUNCHER_DIR, 0755);
  mkdir(LAUNCHER_DIR "/sce_sys", 0755);
  if (write_file(LAUNCHER_DIR "/sce_sys/param.json", launcher_param_json, param_size) ||
      write_file(LAUNCHER_DIR "/sce_sys/icon0.png", launcher_icon_png, icon_size)) {
    printf("  could not write to %s: %s\n", LAUNCHER_DIR, strerror(errno));
    sceAppInstUtilTerminate();
    return -1;
  }

  /* Register just this title where the firmware supports it; otherwise ask
   * for a rescan of everything under /user/app. */
  if (!kernel_dynlib_handle(-1, "libSceAppInstUtil.sprx", &handle)) {
    install_title_dir = (void *)kernel_dynlib_resolve(-1, handle, "Wudg3Xe3heE");
  }
  if (install_title_dir) {
    err = install_title_dir(LAUNCHER_TITLE_ID, "/user/app/", 0);
  } else {
    err = sceAppInstUtilAppInstallAll(0);
  }
  sceAppInstUtilTerminate();
  if (err) {
    printf("  registering the app failed: 0x%08x\n", err);
    return -1;
  }
  printf("  installed (%s), opens http://127.0.0.1:8090/\n", LAUNCHER_TITLE_ID);
  return 0;
}

/* Hand the daemon to the ELF loader on this console and relay what it prints
 * for a while, so that the login link reaches whoever sent the installer. */
static int
start_daemon(int relay_seconds) {
  struct sockaddr_in addr = {0};
  struct timeval start, now;
  char buf[4096];
  int fd;

  if ((fd = socket(AF_INET, SOCK_STREAM, 0)) < 0) {
    return -1;
  }
  addr.sin_family = AF_INET;
  addr.sin_port = htons(LOADER_PORT);
  addr.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
  if (connect(fd, (struct sockaddr *)&addr, sizeof(addr))) {
    close(fd);
    return -1;
  }
  if (write_all(fd, daemon_elf, daemon_elf_end - daemon_elf)) {
    close(fd);
    return -1;
  }

  gettimeofday(&start, 0);
  for (;;) {
    struct timeval tv = {1, 0};
    fd_set rfds;

    gettimeofday(&now, 0);
    if (now.tv_sec - start.tv_sec >= relay_seconds) {
      break;
    }
    FD_ZERO(&rfds);
    FD_SET(fd, &rfds);
    if (select(fd + 1, &rfds, 0, 0, &tv) <= 0) {
      continue;
    }
    ssize_t n = read(fd, buf, sizeof(buf));
    if (n <= 0) {
      break;
    }
    if (write_all(STDOUT_FILENO, (uint8_t *)buf, n)) {
      break;
    }
  }
  close(fd);
  return 0;
}

int
main(void) {
  pid_t pid = getpid();
  intptr_t rootvnode;
  int registered = 0;

  setvbuf(stdout, 0, _IONBF, 0);

  /* Run as root outside the sandbox so /data and USB drives are writable. */
  if ((rootvnode = kernel_get_root_vnode())) {
    kernel_set_proc_rootdir(pid, rootvnode);
    kernel_set_proc_jaildir(pid, 0);
  }
  kernel_set_ucred_uid(pid, 0);
  kernel_set_ucred_ruid(pid, 0);
  kernel_set_ucred_svuid(pid, 0);
  kernel_set_ucred_rgid(pid, 0);
  kernel_set_ucred_svgid(pid, 0);

#ifdef LAUNCHER_ONLY
  /* Test build: only (re)install the home screen icon. */
  printf("Home screen icon:\n");
  return install_launcher_app() ? 1 : 0;
#endif

  printf("Tailscale for PS5 installer\n\n");
  mkdir(DATA_DIR, 0755);

  for (size_t i = 0; i < sizeof(autoloaders) / sizeof(autoloaders[0]); i++) {
    if (register_autoload(&autoloaders[i]) > 0) {
      registered++;
    }
  }
  if (!registered) {
    /* No autoloader: keep the payload where the user can find it. */
    printf("No payload autoloader found on this console.\n");
    if (install_daemon(DATA_DIR)) {
      notify("Tailscale install failed:\ncould not write to %s", DATA_DIR);
      return 1;
    }
    printf("  Tailscale will not start by itself after a reboot.\n"
           "  Send %s/%s to the ELF loader to start it.\n",
           DATA_DIR, DAEMON_NAME);
  }

  printf("Home screen icon:\n");
  install_launcher_app();

  printf("\nStarting Tailscale...\n");
  if (start_daemon(45)) {
    printf("Could not reach the ELF loader on port %d: %s\n", LOADER_PORT, strerror(errno));
    notify("Tailscale is installed but could not be started:\nno ELF loader on port %d.", LOADER_PORT);
    return 1;
  }

  printf("\nDone. The status page is on port 8090 of this console.\n");
  return 0;
}
