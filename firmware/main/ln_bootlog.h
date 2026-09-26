/*
 * ln_bootlog.h — persistent evidence of why the previous boot ended.
 */
#pragma once

#include <stddef.h>

#ifdef __cplusplus
extern "C" {
#endif

/** Call once, right after NVS init: records this boot's reset reason, saves
 *  the previous boot's log tail and panic details to NVS, starts capturing
 *  this boot's log, and prints the stored report ("bootlog:" lines). */
void ln_bootlog_init(void);

/** Short human summary of the last few resets for the screen, e.g.
 *  "#14 brownout after 3h12m; #13 serial port open after 41m". */
void ln_bootlog_summary(char *buf, size_t len);

/** Reprint the stored last-unplanned-reset report and the reset history. */
void ln_bootlog_dump(void);

#ifdef __cplusplus
}
#endif
