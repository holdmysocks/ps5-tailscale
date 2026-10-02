/* List a few directories under /data (two levels) and print small text
 * files, to learn how the console's payload autoloader is configured.
 * Read-only. */

#include <dirent.h>
#include <stdio.h>
#include <string.h>
#include <sys/stat.h>

static int
is_text_name(const char *name) {
  const char *ext = strrchr(name, '.');
  return ext && (!strcmp(ext, ".txt") || !strcmp(ext, ".json") || !strcmp(ext, ".ini") || !strcmp(ext, ".cfg") ||
                 !strcmp(ext, ".conf") || !strcmp(ext, ".lst"));
}

static void
print_file(const char *path, long size) {
  char buf[3000];
  FILE *f;
  size_t n;

  if (size > 20000 || !(f = fopen(path, "rb"))) {
    return;
  }
  n = fread(buf, 1, sizeof(buf) - 1, f);
  fclose(f);
  buf[n] = 0;
  printf("----- %s\n%s\n-----\n", path, buf);
}

static void
list(const char *dir, int depth) {
  struct dirent *e;
  DIR *d = opendir(dir);

  if (!d) {
    printf("%*s(cannot open %s)\n", depth * 2, "", dir);
    return;
  }
  while ((e = readdir(d))) {
    char path[1024];
    struct stat st;

    if (!strcmp(e->d_name, ".") || !strcmp(e->d_name, "..")) {
      continue;
    }
    snprintf(path, sizeof(path), "%s/%s", dir, e->d_name);
    if (stat(path, &st)) {
      continue;
    }
    printf("%*s%s%s  %ld\n", depth * 2, "", e->d_name, S_ISDIR(st.st_mode) ? "/" : "", (long)st.st_size);
    if (S_ISDIR(st.st_mode) && depth < 2) {
      list(path, depth + 1);
    } else if (!S_ISDIR(st.st_mode) && is_text_name(e->d_name)) {
      print_file(path, (long)st.st_size);
    }
  }
  closedir(d);
}

int
main(void) {
  static const char *dirs[] = {"/data/pldmgr", "/data/homebrew", "/data/ps5_autoloader", "/data/etaHEN", "/mnt/usb0", 0};

  setvbuf(stdout, 0, _IONBF, 0);
  for (int i = 0; dirs[i]; i++) {
    printf("== %s\n", dirs[i]);
    list(dirs[i], 1);
  }
  return 0;
}
