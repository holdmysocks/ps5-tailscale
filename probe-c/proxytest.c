/* Exercise the daemon's HTTP proxy from the console itself: one plain request
 * to an Internet host, one to a tailnet address, and one CONNECT. */

#include <stdio.h>
#include <string.h>
#include <unistd.h>

#include <arpa/inet.h>
#include <netinet/in.h>
#include <sys/socket.h>
#include <sys/time.h>

#ifndef TAILNET_PEER
#define TAILNET_PEER "100.64.0.1:8000" /* a web server on one of your tailnet devices */
#endif

static void
request(const char *label, const char *req) {
  struct sockaddr_in addr = {0};
  struct timeval tv = {15, 0};
  char buf[600];
  int fd = socket(AF_INET, SOCK_STREAM, 0);
  ssize_t n;

  addr.sin_family = AF_INET;
  addr.sin_port = htons(8118);
  addr.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
  setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof(tv));
  if (connect(fd, (struct sockaddr *)&addr, sizeof(addr))) {
    printf("%s: cannot connect to the proxy\n", label);
    close(fd);
    return;
  }
  write(fd, req, strlen(req));
  n = read(fd, buf, sizeof(buf) - 1);
  if (n <= 0) {
    printf("%s: no response\n", label);
  } else {
    buf[n] = 0;
    char *eol = strstr(buf, "\r\n");
    char *body = strstr(buf, "\r\n\r\n");
    if (eol) {
      *eol = 0;
    }
    printf("%s: %s", label, buf);
    if (body && body[4]) {
      body += 4;
      body[strcspn(body, "\r\n")] = 0;
      printf(" | body: %.80s", body);
    }
    printf("\n");
  }
  close(fd);
}

int
main(void) {
  setvbuf(stdout, 0, _IONBF, 0);
  request("internet GET ", "GET http://controlplane.tailscale.com/key?v=100 HTTP/1.1\r\n"
                           "Host: controlplane.tailscale.com\r\nConnection: close\r\n\r\n");
  request("tailnet GET  ", "GET http://" TAILNET_PEER "/hello.txt HTTP/1.1\r\n"
                           "Host: " TAILNET_PEER "\r\nConnection: close\r\n\r\n");
  request("CONNECT      ", "CONNECT controlplane.tailscale.com:443 HTTP/1.1\r\n"
                           "Host: controlplane.tailscale.com:443\r\n\r\n");
  return 0;
}
