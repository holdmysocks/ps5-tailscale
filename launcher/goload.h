#pragma once

#include <stddef.h>
#include <stdint.h>

/* Map the Go PIE image held in memory, relocate it and jump into it on a
 * fresh stack. Only returns (with -1) if loading failed. */
int goload_run(const uint8_t *image, size_t size, char *const argv[], char *const envp[]);
