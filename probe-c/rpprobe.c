/* Ask the console's Remote Play service for its discovery reply over
 * loopback, to confirm it answers clients that arrive from 127.0.0.1 (which
 * is how the daemon's UDP relay reaches it). Read-only. */

#include <stdio.h>
#include <string.h>
#include <unistd.h>

#include <arpa/inet.h>
#include <netinet/in.h>
#include <sys/socket.h>
#include <sys/time.h>

int
main(void) {
  static const char req[] = "SRCH * HTTP/1.1\ndevice-discovery-protocol-version:00030010\n";
  struct sockaddr_in addr = {0};
  struct timeval tv = {3, 0};
  char buf[1024];
  int fd = socket(AF_INET, SOCK_DGRAM, 0);
  ssize_t n;

  setvbuf(stdout, 0, _IONBF, 0);
  addr.sin_family = AF_INET;
  addr.sin_port = htons(9302);
  addr.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
  setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof(tv));
  sendto(fd, req, sizeof(req) - 1, 0, (struct sockaddr *)&addr, sizeof(addr));
  n = recv(fd, buf, sizeof(buf) - 1, 0);
  if (n <= 0) {
    printf("discovery via 127.0.0.1:9302: no reply\n");
  } else {
    buf[n] = 0;
    buf[strcspn(buf, "\r\n")] = 0;
    printf("discovery via 127.0.0.1:9302: %s\n", buf);
  }
  close(fd);
  return 0;
}
