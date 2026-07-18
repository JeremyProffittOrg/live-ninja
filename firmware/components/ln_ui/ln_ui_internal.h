/*
 * ln_ui_internal.h — shared internals of the ln_ui component.
 * Screen modules expose create() plus setters; ALL setters here assume the
 * caller already holds the LVGL port lock (bsp_display_lock).
 */
#pragma once

#include <stddef.h>

#include "lvgl.h"

#include "ln_events.h"
#include "ln_ui.h"

/* ---- LIVE NINJA design tokens (mockups/m5stack) ---- */
#define LN_COL_BG        lv_color_hex(0x060d18) /* navy-900 */
#define LN_COL_SURFACE   lv_color_hex(0x0f1e35) /* navy-800 */
#define LN_COL_SURFACE2  lv_color_hex(0x16283f) /* raised   */
#define LN_COL_BORDER    lv_color_hex(0x2a3c58)
#define LN_COL_TEAL      lv_color_hex(0x22e0d0)
#define LN_COL_TEAL_DARK lv_color_hex(0x0d5f58)
#define LN_COL_CYAN      lv_color_hex(0x38d0ff)
#define LN_COL_TEXT      lv_color_hex(0xf4f8fb)
#define LN_COL_MUTED     lv_color_hex(0x9fb0c4) /* gray-300 */
#define LN_COL_DIM       lv_color_hex(0x6b7d94) /* gray-500 */
#define LN_COL_SUCCESS   lv_color_hex(0x34e39b)
#define LN_COL_WARN      lv_color_hex(0xffca4a)
#define LN_COL_ERROR     lv_color_hex(0xff5c72)
#define LN_COL_INK       lv_color_hex(0x04211e) /* text on teal */

/* All sizes bumped >=20% (owner request 2026-07-18: on-device text was too
 * small). HUGE stays 48 — the largest Montserrat LVGL ships. */
#define LN_FONT_XS   (&lv_font_montserrat_18)
#define LN_FONT_SM   (&lv_font_montserrat_20)
#define LN_FONT_MD   (&lv_font_montserrat_24)
#define LN_FONT_LG   (&lv_font_montserrat_30)
#define LN_FONT_XL   (&lv_font_montserrat_34)
#define LN_FONT_XXL  (&lv_font_montserrat_44)
#define LN_FONT_HUGE (&lv_font_montserrat_48)

#define LN_TOUCH_MIN  56 /* min touch-target px (48-64 rule) */
#define LN_RADIUS     14

/* ---- shared widget helpers (ln_ui_widgets.c) ---- */
lv_obj_t *ln_w_screen(void);
lv_obj_t *ln_w_plain(lv_obj_t *parent); /* style-less container */
lv_obj_t *ln_w_col(lv_obj_t *parent, int gap);
lv_obj_t *ln_w_row(lv_obj_t *parent, int gap);
lv_obj_t *ln_w_card(lv_obj_t *parent);
lv_obj_t *ln_w_label(lv_obj_t *parent, const char *txt, const lv_font_t *font,
                     lv_color_t color);
lv_obj_t *ln_w_button(lv_obj_t *parent, const char *txt, lv_color_t bg,
                      lv_color_t fg, lv_event_cb_t cb);
lv_obj_t *ln_w_kv_row(lv_obj_t *parent, const char *key, const char *val,
                      lv_obj_t **val_out);
/* Modal confirm dialog on lv_layer_top(). on_confirm runs in the LVGL task. */
void ln_w_confirm(const char *title, const char *body, const char *confirm_txt,
                  lv_color_t confirm_bg, void (*on_confirm)(void));

/* Post an LN_UI_EVENT on the default loop (safe if no loop exists). */
void ln_ui_post(int32_t id, const void *data, size_t size);

/* ---- screen modules ---- */
lv_obj_t *ln_scr_boot_create(void);

lv_obj_t *ln_scr_idle_create(void);
void ln_scr_idle_tick(void); /* 1 Hz clock refresh (LVGL timer)   */
void ln_scr_idle_set_wifi(const char *text);
void ln_scr_idle_set_cloud(const char *text);
void ln_scr_idle_set_account(const char *text);
void ln_scr_idle_set_wake_phrase(const char *phrase);

lv_obj_t *ln_scr_listening_create(void);
void ln_scr_listening_reset(void);
void ln_scr_listening_set_transcript(const char *text);
void ln_scr_listening_set_level(uint8_t pct);

lv_obj_t *ln_scr_thinking_create(void);
void ln_scr_thinking_set_request(const char *text);

lv_obj_t *ln_scr_speaking_create(void);
void ln_scr_speaking_reset(void);
void ln_scr_speaking_set_text(const char *text);

lv_obj_t *ln_scr_config_create(void);
void ln_scr_config_set_values(const ln_ui_config_t *cfg);
/* shadow-synced subset only (voice + sensitivity); NULL/-1 to skip a field */
void ln_scr_config_set_shadow(const char *voice, float sensitivity);
void ln_scr_config_set_net(const char *ssid, const char *ip,
                           const char *signal);
void ln_scr_config_set_about(const char *fw, const char *thing,
                             const char *mac, const char *ip);

lv_obj_t *ln_scr_onboarding_create(void);
void ln_scr_onboarding_portal(const char *ssid, const char *url);
void ln_scr_onboarding_pairing(const char *claim_url, const char *code);
void ln_scr_onboarding_connected(const char *ip);
void ln_scr_onboarding_status(const char *text);

lv_obj_t *ln_scr_error_create(void);
void ln_scr_error_set(const char *title, const char *detail, const char *code);
void ln_scr_error_set_wifi(const char *text);
void ln_scr_error_countdown(int secs);
