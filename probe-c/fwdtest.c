/* Exercise the daemon's local forwards from the console: an HTTP GET through
 * the TCP forward on 127.0.0.1:47989, and a UDP round trip through the
 * forward on 127.0.0.1:47998. Build with -DHTTP_PATH="/serverinfo" etc. */

#include <stdio.h>
#include <string.h>
#include <unistd.h>

#include <arpa/inet.h>
#include <netinet/in.h>
#include <sys/socket.h>
#include <sys/time.h>

#ifndef HTTP_PATH
#define HTTP_PATH "/hello.txt"
#endif

static struct sockaddr_in
local(int port) {
  struct sockaddr_in addr = {0};
  addr.sin_family = AF_INET;
  addr.sin_port = htons(port);
  addr.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
  return addr;
}

static double
now(void) {
  struct timeval tv;
  gettimeofday(&tv, 0);
  return tv.tv_sec + tv.tv_usec / 1e6;
}

int
main(void) {
  struct timeval tv = {10, 0};
  struct sockaddr_in addr;
  char buf[2048];
  ssize_t n;
  int fd;

  setvbuf(stdout, 0, _IONBF, 0);

  /* TCP */
  addr = local(47989);
  fd = socket(AF_INET, SOCK_STREAM, 0);
  setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof(tv));
  double t0 = now();
  if (connect(fd, (struct sockaddr *)&addr, sizeof(addr))) {
    printf("tcp 127.0.0.1:47989: cannot connect (no forward listening)\n");
  } else {
    const char *req = "GET " HTTP_PATH " HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n";
    size_t total = 0;
    write(fd, req, strlen(req));
    while (total < sizeof(buf) - 1 && (n = read(fd, buf + total, sizeof(buf) - 1 - total)) > 0) {
      total += n;
    }
    buf[total] = 0;
    char *body = strstr(buf, "\r\n\r\n");
    char *eol = strstr(buf, "\r\n");
    if (!total) {
      printf("tcp 127.0.0.1:47989: connected but no response\n");
    } else {
      printf("tcp 127.0.0.1:47989: %.*s (%.0f ms)\n", eol ? (int)(eol - buf) : 60, buf, (now() - t0) * 1000);
      if (body) {
        printf("  body: %.400s\n", body + 4);
      }
    }
  }
  close(fd);

#ifndef SKIP_UDP
  /* UDP */
  addr = local(47998);
  fd = socket(AF_INET, SOCK_DGRAM, 0);
  setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof(tv));
  int ok = 0;
  double worst = 0;
  for (int i = 0; i < 20; i++) {
    char msg[1300];
    memset(msg, 'a' + i, sizeof(msg));
    t0 = now();
    sendto(fd, msg, sizeof(msg), 0, (struct sockaddr *)&addr, sizeof(addr));
    n = recv(fd, buf, sizeof(buf), 0);
    double ms = (now() - t0) * 1000;
    if (n == sizeof(msg) && !memcmp(buf, msg, sizeof(msg))) {
      ok++;
      if (ms > worst) {
        worst = ms;
      }
    }
  }
  printf("udp 127.0.0.1:47998: %d/20 datagrams of 1300 bytes echoed, worst round trip %.1f ms\n", ok, worst);
  close(fd);
#endif
  return 0;
}
