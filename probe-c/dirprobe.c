/* Find out which buffer sizes getdirentries(196)/getdents(272) accept on the
 * PS5's filesystems, and what the returned records look like. */

#include <fcntl.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#include <sys/mount.h>
#include <sys/param.h>
#include <sys/stat.h>

static long
raw(long n, long a, long b, long c, long d) {
  long ret;
  unsigned char cf;
  register long r10 __asm__("r10") = d;
  __asm__ volatile("syscall; setc %1" : "+a"(n), "=q"(cf) : "D"(a), "S"(b), "d"(c), "r"(r10) : "rcx", "r11", "r8", "r9", "memory");
  ret = n;
  return cf ? -ret : ret;
}

int
main(void) {
  static const char *dirs[] = {"/", "/data", "/dev", "/system", "/user", "/system_data", "/mnt", 0};
  static const int sizes[] = {256, 512, 1024, 2048, 4096, 8192, 16384, 32768, 65536};
  static char buf[65536];

  setvbuf(stdout, 0, _IONBF, 0);
  for (int d = 0; dirs[d]; d++) {
    struct statfs sfs;
    struct stat st;
    memset(&sfs, 0, sizeof(sfs));
    if (statfs(dirs[d], &sfs)) {
      printf("%s: statfs failed\n", dirs[d]);
      continue;
    }
    stat(dirs[d], &st);
    printf("%s: fstype=%s bsize=%lu iosize=%lu st_blksize=%d sizeof(statfs)=%zu\n", dirs[d], sfs.f_fstypename,
           (unsigned long)sfs.f_bsize, (unsigned long)sfs.f_iosize, (int)st.st_blksize, sizeof(sfs));
    for (size_t s = 0; s < sizeof(sizes) / sizeof(sizes[0]); s++) {
      int fd = open(dirs[d], O_RDONLY | O_DIRECTORY);
      if (fd < 0) {
        printf("  open failed\n");
        break;
      }
      long base = 0;
      long n1 = raw(196, fd, (long)buf, sizes[s], (long)&base);
      long off1 = lseek(fd, 0, SEEK_CUR);
      long n2 = raw(196, fd, (long)buf, sizes[s], (long)&base);
      close(fd);
      fd = open(dirs[d], O_RDONLY | O_DIRECTORY);
      long n3 = raw(272, fd, (long)buf, sizes[s], 0);
      close(fd);
      printf("  size %-6d getdirentries=%ld (off after=%ld, next=%ld) getdents=%ld\n", sizes[s], n1, off1, n2, n3);
    }
  }

  /* Dump the first records of /data to confirm the dirent layout. */
  int fd = open("/data", O_RDONLY | O_DIRECTORY);
  long base = 0;
  long n = raw(196, fd, (long)buf, sizeof(buf), (long)&base);
  printf("/data records (n=%ld):\n", n);
  for (long pos = 0; pos + 8 <= n && pos < 400;) {
    uint32_t fileno;
    uint16_t reclen;
    memcpy(&fileno, buf + pos, 4);
    memcpy(&reclen, buf + pos + 4, 2);
    printf("  +%ld fileno=%u reclen=%u type=%u namlen=%u name=%.*s\n", pos, fileno, reclen, (uint8_t)buf[pos + 6],
           (uint8_t)buf[pos + 7], (uint8_t)buf[pos + 7], buf + pos + 8);
    if (!reclen) {
      break;
    }
    pos += reclen;
  }
  close(fd);
  return 0;
}
