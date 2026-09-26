/*
 * ln_bootlog.c — why did the last boot end?
 *
 * The Tab5 reboots while idle with no serial console attached, and a panic or
 * an esp-hosted host restart leaves nothing behind (no coredump partition).
 * This keeps three kinds of evidence across a reset:
 *
 *   1. A ring of the last LN_BL_TAIL bytes of ESP_LOG output, in .noinit RAM
 *      (survives software resets, panics and watchdog resets — not power
 *      loss). esp-hosted logs its reason right before it restarts the host
 *      ("SDIO slave unresponsive, restart host" etc.), so the tail names it.
 *   2. Panic details (exception kind, core, mepc/mcause/mtval, abort/assert
 *      text) captured by wrapping esp_panic_handler (-Wl,--wrap, see
 *      main/CMakeLists.txt).
 *   3. The reset reason and approximate uptime of each previous boot, kept as
 *      a short history in NVS so it outlives the next few reboots too.
 *
 * On boot it prints all of it to the console (prefix "bootlog:") and stores
 * the previous boot's report in NVS namespace "ln_boot" (key "last"), so a
 * console opened later still finds it: ln_bootlog_dump() reprints it.
 */
#include "ln_bootlog.h"

#include <stdarg.h>
#include <stddef.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "esp_attr.h"
#include "esp_log.h"
#include "esp_private/panic_internal.h"
#include "esp_system.h"
#include "esp_timer.h"
#include "freertos/FreeRTOS.h"
#include "nvs.h"
#include "riscv/rvruntime-frames.h"

static const char *TAG = "bootlog";

#define LN_BL_MAGIC      0x4C4E424CU /* "LNBL" */
#define LN_BL_TAIL       3072
#define LN_BL_LINE_MAX   256
#define LN_BL_NVS_NS     "ln_boot"
#define LN_BL_HIST_LEN   8
#define LN_BL_ABORT_MAX  160

typedef struct {
    uint32_t magic;
    uint32_t head;          /* next write offset in tail[] */
    uint32_t len;           /* valid bytes in tail[] (<= LN_BL_TAIL) */
    uint32_t uptime_s;      /* refreshed by a 5 s timer and every log line */
    uint32_t panic_magic;   /* == LN_BL_MAGIC when the fields below are set */
    int32_t panic_core;
    int32_t panic_exception;
    uint32_t panic_mepc;
    uint32_t panic_mcause;
    uint32_t panic_mtval;
    char panic_abort[LN_BL_ABORT_MAX];
    char tail[LN_BL_TAIL];
} ln_bl_ram_t;

static __NOINIT_ATTR ln_bl_ram_t s_bl;
static portMUX_TYPE s_mux = portMUX_INITIALIZER_UNLOCKED;
static vprintf_like_t s_prev_vprintf;
static esp_timer_handle_t s_tick;

/* One history row per previous boot, newest first. */
typedef struct {
    uint32_t boot_no;
    uint8_t reason;         /* esp_reset_reason_t that ENDED this boot */
    uint8_t panicked;
    uint16_t reserved;
    uint32_t uptime_s;      /* how long that boot ran (5 s resolution) */
} ln_bl_hist_t;

extern bool g_panic_abort;
extern char *g_panic_abort_details;

/* ------------------------------------------------------------ capture -- */

static void ring_append(const char *s, size_t n)
{
    taskENTER_CRITICAL(&s_mux);
    for (size_t i = 0; i < n; i++) {
        s_bl.tail[s_bl.head] = s[i];
        s_bl.head = (s_bl.head + 1) % LN_BL_TAIL;
    }
    s_bl.len = (s_bl.len + n > LN_BL_TAIL) ? LN_BL_TAIL : s_bl.len + (uint32_t)n;
    taskEXIT_CRITICAL(&s_mux);
}

static int bl_vprintf(const char *fmt, va_list ap)
{
    char line[LN_BL_LINE_MAX];
    va_list copy;
    va_copy(copy, ap);
    int n = vsnprintf(line, sizeof(line), fmt, copy);
    va_end(copy);
    if (n > 0) {
        ring_append(line, (n < (int)sizeof(line)) ? (size_t)n : sizeof(line) - 1);
    }
    s_bl.uptime_s = (uint32_t)(esp_timer_get_time() / 1000000);
    return s_prev_vprintf(fmt, ap);
}

static void tick_cb(void *arg)
{
    (void)arg;
    s_bl.uptime_s = (uint32_t)(esp_timer_get_time() / 1000000);
}

void __real_esp_panic_handler(panic_info_t *info);

/* Runs in panic context: register values and DRAM copies only. The abort /
 * assert text is built in a RAM buffer by newlib before it lands here. */
void IRAM_ATTR __wrap_esp_panic_handler(panic_info_t *info)
{
    s_bl.panic_core = info->core;
    s_bl.panic_exception = (int32_t)info->exception;
    const RvExcFrame *f = (const RvExcFrame *)info->frame;
    s_bl.panic_mepc = (f != NULL) ? f->mepc : (uint32_t)info->addr;
    s_bl.panic_mcause = (f != NULL) ? f->mcause : 0;
    s_bl.panic_mtval = (f != NULL) ? f->mtval : 0;
    s_bl.panic_abort[0] = '\0';
    if (g_panic_abort && g_panic_abort_details != NULL) {
        size_t i = 0;
        for (; i < LN_BL_ABORT_MAX - 1 && g_panic_abort_details[i] != '\0'; i++) {
            s_bl.panic_abort[i] = g_panic_abort_details[i];
        }
        s_bl.panic_abort[i] = '\0';
    }
    s_bl.panic_magic = LN_BL_MAGIC;
    __real_esp_panic_handler(info);
}

/* ------------------------------------------------------------- report -- */

static const char *reason_str(esp_reset_reason_t r)
{
    switch (r) {
    case ESP_RST_POWERON:   return "power-on";
    case ESP_RST_EXT:       return "external pin";
    case ESP_RST_SW:        return "software restart (esp_restart)";
    case ESP_RST_PANIC:     return "panic/exception";
    case ESP_RST_INT_WDT:   return "interrupt watchdog";
    case ESP_RST_TASK_WDT:  return "task watchdog";
    case ESP_RST_WDT:       return "other watchdog";
    case ESP_RST_DEEPSLEEP: return "deep sleep wake";
    case ESP_RST_BROWNOUT:  return "brownout (supply voltage dip)";
    case ESP_RST_SDIO:      return "SDIO";
    case ESP_RST_USB:       return "USB (serial port open / host reset)";
    case ESP_RST_JTAG:      return "JTAG";
    case ESP_RST_EFUSE:     return "eFuse error";
    case ESP_RST_PWR_GLITCH: return "power glitch";
    case ESP_RST_CPU_LOCKUP: return "CPU lockup";
    default:                return "unknown";
    }
}

static const char *exception_str(int32_t e)
{
    switch (e) {
    case PANIC_EXCEPTION_DEBUG: return "debug";
    case PANIC_EXCEPTION_IWDT:  return "interrupt watchdog";
    case PANIC_EXCEPTION_TWDT:  return "task watchdog";
    case PANIC_EXCEPTION_ABORT: return "abort/assert";
    case PANIC_EXCEPTION_FAULT: return "CPU fault";
    default:                    return "other";
    }
}

static void print_hist(nvs_handle_t h)
{
    ln_bl_hist_t hist[LN_BL_HIST_LEN];
    size_t sz = sizeof(hist);
    if (nvs_get_blob(h, "hist", hist, &sz) != ESP_OK) {
        return;
    }
    size_t n = sz / sizeof(hist[0]);
    for (size_t i = 0; i < n; i++) {
        ESP_LOGW(TAG, "history: boot #%lu ran ~%lu s, ended by %s%s",
                 (unsigned long)hist[i].boot_no, (unsigned long)hist[i].uptime_s,
                 reason_str((esp_reset_reason_t)hist[i].reason),
                 hist[i].panicked ? " (panic captured)" : "");
    }
}

static const char *reason_short(esp_reset_reason_t r)
{
    switch (r) {
    case ESP_RST_POWERON:   return "power-on";
    case ESP_RST_SW:        return "software restart";
    case ESP_RST_PANIC:     return "crash";
    case ESP_RST_INT_WDT:
    case ESP_RST_TASK_WDT:
    case ESP_RST_WDT:       return "watchdog";
    case ESP_RST_BROWNOUT:  return "brownout";
    case ESP_RST_USB:       return "serial port open";
    case ESP_RST_PWR_GLITCH: return "power glitch";
    case ESP_RST_CPU_LOCKUP: return "CPU lockup";
    default:                return reason_str(r);
    }
}

void ln_bootlog_summary(char *buf, size_t len)
{
    if (buf == NULL || len == 0) {
        return;
    }
    buf[0] = '\0';
    nvs_handle_t h;
    if (nvs_open(LN_BL_NVS_NS, NVS_READONLY, &h) != ESP_OK) {
        strlcpy(buf, "None recorded", len);
        return;
    }
    ln_bl_hist_t hist[LN_BL_HIST_LEN];
    size_t sz = sizeof(hist);
    if (nvs_get_blob(h, "hist", hist, &sz) != ESP_OK || sz < sizeof(hist[0])) {
        strlcpy(buf, "None recorded", len);
        nvs_close(h);
        return;
    }
    nvs_close(h);
    size_t n = sz / sizeof(hist[0]);
    size_t off = 0;
    for (size_t i = 0; i < n && i < 3 && off < len; i++) {
        uint32_t m = hist[i].uptime_s / 60;
        int w = snprintf(buf + off, len - off, "%s#%lu %s after %luh%02lum",
                         (i == 0) ? "" : "\n", (unsigned long)hist[i].boot_no,
                         reason_short((esp_reset_reason_t)hist[i].reason),
                         (unsigned long)(m / 60), (unsigned long)(m % 60));
        if (w < 0) {
            break;
        }
        off += (size_t)w;
    }
}

void ln_bootlog_dump(void)
{
    nvs_handle_t h;
    if (nvs_open(LN_BL_NVS_NS, NVS_READONLY, &h) != ESP_OK) {
        ESP_LOGI(TAG, "no previous-boot report stored");
        return;
    }
    size_t sz = 0;
    if (nvs_get_str(h, "last", NULL, &sz) == ESP_OK && sz > 1) {
        char *buf = malloc(sz);
        if (buf != NULL && nvs_get_str(h, "last", buf, &sz) == ESP_OK) {
            ESP_LOGW(TAG, "stored report of the last unplanned reset:");
            /* Line by line so a long report is not truncated by the logger. */
            for (char *line = strtok(buf, "\n"); line != NULL; line = strtok(NULL, "\n")) {
                ESP_LOGW(TAG, "| %s", line);
            }
        }
        free(buf);
    }
    print_hist(h);
    nvs_close(h);
}

void ln_bootlog_init(void)
{
    const esp_reset_reason_t reason = esp_reset_reason();
    const bool have_prev = (s_bl.magic == LN_BL_MAGIC) && s_bl.len <= LN_BL_TAIL &&
                           s_bl.head < LN_BL_TAIL && reason != ESP_RST_POWERON;
    const bool panicked = have_prev && s_bl.panic_magic == LN_BL_MAGIC;

    nvs_handle_t h;
    bool nvs_ok = (nvs_open(LN_BL_NVS_NS, NVS_READWRITE, &h) == ESP_OK);
    uint32_t boot_no = 0;
    if (nvs_ok) {
        (void)nvs_get_u32(h, "n", &boot_no);
        boot_no++;
        (void)nvs_set_u32(h, "n", boot_no);
    }

    ESP_LOGW(TAG, "boot #%lu: reset reason = %s (%d)%s", (unsigned long)boot_no,
             reason_str(reason), (int)reason,
             have_prev ? "" : " — no previous-boot RAM log (power loss or first boot)");

    if (have_prev) {
        ESP_LOGW(TAG, "previous boot ran ~%lu s", (unsigned long)s_bl.uptime_s);
        char *report = malloc(LN_BL_TAIL + 512);
        if (report != NULL) {
            int off = snprintf(report, 512, "boot #%lu ended after ~%lu s: %s\n",
                               (unsigned long)(boot_no - 1), (unsigned long)s_bl.uptime_s,
                               reason_str(reason));
            if (panicked) {
                off += snprintf(report + off, 512 - off,
                                "panic: %s on core %ld, mepc=0x%08lx mcause=0x%08lx mtval=0x%08lx %s\n",
                                exception_str(s_bl.panic_exception), (long)s_bl.panic_core,
                                (unsigned long)s_bl.panic_mepc, (unsigned long)s_bl.panic_mcause,
                                (unsigned long)s_bl.panic_mtval, s_bl.panic_abort);
            }
            /* Linearize the ring, oldest byte first. */
            uint32_t start = (s_bl.len == LN_BL_TAIL) ? s_bl.head : 0;
            for (uint32_t i = 0; i < s_bl.len; i++) {
                char c = s_bl.tail[(start + i) % LN_BL_TAIL];
                report[off++] = (c == '\0') ? ' ' : c;
            }
            report[off] = '\0';
            if (nvs_ok && reason != ESP_RST_USB) {
                /* A serial-port open (USB reset) is never the fault being
                 * hunted — keep the last unplanned reset's report instead. */
                (void)nvs_set_str(h, "last", report);
            }
            free(report);
        }
    }

    if (nvs_ok) {
        ln_bl_hist_t hist[LN_BL_HIST_LEN] = {0};
        size_t sz = sizeof(hist);
        size_t n = 0;
        if (nvs_get_blob(h, "hist", hist, &sz) == ESP_OK) {
            n = sz / sizeof(hist[0]);
        }
        if (reason != ESP_RST_POWERON || have_prev) {
            memmove(&hist[1], &hist[0], sizeof(hist[0]) * (LN_BL_HIST_LEN - 1));
            hist[0] = (ln_bl_hist_t){
                .boot_no = boot_no - 1,
                .reason = (uint8_t)reason,
                .panicked = panicked ? 1 : 0,
                .uptime_s = have_prev ? s_bl.uptime_s : 0,
            };
            n = (n + 1 > LN_BL_HIST_LEN) ? LN_BL_HIST_LEN : n + 1;
            (void)nvs_set_blob(h, "hist", hist, n * sizeof(hist[0]));
        }
        (void)nvs_commit(h);
        nvs_close(h);
    }

    /* Fresh ring for this boot, then start capturing. */
    memset(&s_bl, 0, offsetof(ln_bl_ram_t, tail));
    s_bl.magic = LN_BL_MAGIC;
    s_prev_vprintf = esp_log_set_vprintf(bl_vprintf);

    const esp_timer_create_args_t targs = { .callback = tick_cb, .name = "bootlog" };
    if (esp_timer_create(&targs, &s_tick) == ESP_OK) {
        (void)esp_timer_start_periodic(s_tick, 5 * 1000000);
    }

    ln_bootlog_dump();
}
