#pragma once

/* Where the launcher records what it did. A payload manager does not show
 * what a payload prints, so this file is how a start that went wrong can be
 * looked into afterwards. */
#define LAUNCHER_LOG "/data/tailscale/launcher.log"

/* Starts a new entry in the launcher log. */
void report_begin(void);

/* Writes a line to the launcher log and to whoever sent the payload. */
void report_log(const char *fmt, ...) __attribute__((format(printf, 1, 2)));

/* Like report_log, and also tells the user on screen that Tailscale did not
 * start. */
void report_fail(const char *fmt, ...) __attribute__((format(printf, 1, 2)));

/* Points stderr at the launcher log, so that whatever the Go runtime prints
 * if it dies before the daemon has opened its own log ends up there. */
void report_capture_stderr(void);
