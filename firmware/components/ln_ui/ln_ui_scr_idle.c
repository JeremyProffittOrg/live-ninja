/*
 * ln_ui_scr_idle.c — Boot splash + Idle screen (mockups 03).
 * Idle: brand bar, big clock + date, wake-phrase hint, tap-to-talk orb
 * (manual wake via ln_wake_trigger()), status chips, settings button
 * (posts LN_UI_SETTINGS_OPEN_REQUESTED).
 */
#include <stdio.h>
#include <string.h>
#include <time.h>

#include "ln_wake.h"

#include "ln_ui_internal.h"

/* ---- Boot splash ---- */

lv_obj_t *ln_scr_boot_create(void)
{
    lv_obj_t *scr = ln_w_screen();

    lv_obj_t *col = ln_w_col(scr, 18);
    lv_obj_center(col);
    lv_obj_set_flex_align(col, LV_FLEX_ALIGN_CENTER, LV_FLEX_ALIGN_CENTER,
                          LV_FLEX_ALIGN_CENTER);

    lv_obj_t *brand = ln_w_label(col, "LIVE NINJA", LN_FONT_HUGE, LN_COL_TEAL);
    lv_obj_set_style_text_letter_space(brand, 6, 0);
    ln_w_label(col, "Starting…", LN_FONT_MD, LN_COL_MUTED);

    lv_obj_t *spin = lv_spinner_create(col);
    lv_obj_set_size(spin, 56, 56);
    lv_obj_set_style_arc_color(spin, LN_COL_SURFACE2, LV_PART_MAIN);
    lv_obj_set_style_arc_color(spin, LN_COL_TEAL, LV_PART_INDICATOR);
    lv_obj_set_style_arc_width(spin, 6, LV_PART_MAIN);
    lv_obj_set_style_arc_width(spin, 6, LV_PART_INDICATOR);
    return scr;
}

/* ---- Idle ---- */

static lv_obj_t *s_clock_small;   /* topbar HH:MM      */
static lv_obj_t *s_clock_big;     /* center clock      */
static lv_obj_t *s_date_label;    /* center date       */
static lv_obj_t *s_wake_hint;     /* Say "…"           */
static lv_obj_t *s_wifi_value;
static lv_obj_t *s_cloud_value;
static lv_obj_t *s_account_value;

static char s_wake_phrase[48] = "Hey Live Ninja";

static void settings_btn_cb(lv_event_t *e)
{
    (void)e;
    ln_ui_post(LN_UI_SETTINGS_OPEN_REQUESTED, NULL, 0);
}

/* Tap-to-talk: manual wake, same path as the wake word (LN_WAKE_EVT_DETECTED
 * with word_index 0 — ln_ctrl starts Listening from Idle). The pressed-state
 * styles on the orb give the immediate visual feedback (LCD UI rule). */
static void orb_cb(lv_event_t *e)
{
    (void)e;
    ln_wake_trigger();
}

/* One "TITLE / value" chip in the bottom bar. */
static void make_chip(lv_obj_t *parent, const char *symbol, const char *title,
                      lv_obj_t **value_out)
{
    lv_obj_t *chip = ln_w_row(parent, 14);

    lv_obj_t *icon = ln_w_label(chip, symbol, LN_FONT_XL, LN_COL_DIM);
    lv_obj_set_style_pad_top(icon, 2, 0);

    lv_obj_t *col = ln_w_col(chip, 2);
    ln_w_label(col, title, LN_FONT_XS, LN_COL_DIM);
    lv_obj_t *val = ln_w_label(col, "—", LN_FONT_MD, LN_COL_TEXT);
    lv_label_set_long_mode(val, LV_LABEL_LONG_DOT);
    lv_obj_set_style_max_width(val, 300, 0);
    *value_out = val;
}

lv_obj_t *ln_scr_idle_create(void)
{
    lv_obj_t *scr = ln_w_screen();

    /* topbar */
    lv_obj_t *top = ln_w_plain(scr);
    lv_obj_set_size(top, lv_pct(100), 72);
    lv_obj_align(top, LV_ALIGN_TOP_MID, 0, 0);
    lv_obj_set_style_bg_color(top, LN_COL_SURFACE, 0);
    lv_obj_set_style_bg_opa(top, LV_OPA_COVER, 0);
    lv_obj_set_style_border_color(top, LN_COL_BORDER, 0);
    lv_obj_set_style_border_width(top, 1, 0);
    lv_obj_set_style_border_side(top, LV_BORDER_SIDE_BOTTOM, 0);
    lv_obj_set_style_pad_hor(top, 28, 0);

    lv_obj_t *brand = ln_w_label(top, "LIVE NINJA", LN_FONT_XL, LN_COL_TEAL);
    lv_obj_set_style_text_letter_space(brand, 3, 0);
    lv_obj_align(brand, LV_ALIGN_LEFT_MID, 0, 0);

    s_clock_small = ln_w_label(top, "--:--", LN_FONT_MD, LN_COL_MUTED);
    lv_obj_align(s_clock_small, LV_ALIGN_CENTER, 0, 0);

    lv_obj_t *gear = ln_w_button(top, LV_SYMBOL_SETTINGS "  Settings",
                                 LN_COL_SURFACE2, LN_COL_TEXT, settings_btn_cb);
    lv_obj_align(gear, LV_ALIGN_RIGHT_MID, 0, 0);

    /* center */
    lv_obj_t *center = ln_w_col(scr, 10);
    lv_obj_align(center, LV_ALIGN_CENTER, 0, -20);
    lv_obj_set_flex_align(center, LV_FLEX_ALIGN_CENTER, LV_FLEX_ALIGN_CENTER,
                          LV_FLEX_ALIGN_CENTER);

    s_clock_big = ln_w_label(center, "--:--", LN_FONT_HUGE, LN_COL_TEXT);
    s_date_label = ln_w_label(center, "", LN_FONT_LG, LN_COL_MUTED);

    lv_obj_t *ready = ln_w_row(center, 10);
    lv_obj_set_style_pad_top(ready, 14, 0);
    lv_obj_t *dot = lv_obj_create(ready);
    lv_obj_remove_style_all(dot);
    lv_obj_set_size(dot, 14, 14);
    lv_obj_set_style_radius(dot, LV_RADIUS_CIRCLE, 0);
    lv_obj_set_style_bg_color(dot, LN_COL_SUCCESS, 0);
    lv_obj_set_style_bg_opa(dot, LV_OPA_COVER, 0);
    ln_w_label(ready, "Ready · listening for wake phrase", LN_FONT_SM,
               LN_COL_DIM);

    s_wake_hint = ln_w_label(center, "Say \"Hey Live Ninja\"", LN_FONT_XXL,
                             LN_COL_TEAL);
    lv_obj_set_style_pad_top(s_wake_hint, 18, 0);

    /* Tap-to-talk orb — the touch path into a conversation (before this,
     * the wake word was the only way in). ~160px target, well over the
     * 48-64px rule. */
    lv_obj_t *orb = lv_button_create(center);
    lv_obj_remove_style_all(orb);
    lv_obj_set_size(orb, 160, 160);
    lv_obj_set_style_margin_top(orb, 26, 0);
    lv_obj_set_style_radius(orb, LV_RADIUS_CIRCLE, 0);
    lv_obj_set_style_bg_color(orb, LN_COL_TEAL, 0);
    lv_obj_set_style_bg_opa(orb, LV_OPA_COVER, 0);
    lv_obj_set_style_border_color(orb, LN_COL_TEAL_DARK, 0);
    lv_obj_set_style_border_width(orb, 4, 0);
    lv_obj_set_style_shadow_color(orb, LN_COL_TEAL, 0);
    lv_obj_set_style_shadow_width(orb, 30, 0);
    lv_obj_set_style_shadow_opa(orb, LV_OPA_40, 0);
    /* immediate tap feedback: brighter fill + wider glow while pressed */
    lv_obj_set_style_bg_color(orb, LN_COL_CYAN, LV_STATE_PRESSED);
    lv_obj_set_style_shadow_width(orb, 60, LV_STATE_PRESSED);
    lv_obj_set_style_shadow_opa(orb, LV_OPA_70, LV_STATE_PRESSED);
    lv_obj_add_event_cb(orb, orb_cb, LV_EVENT_CLICKED, NULL);
    lv_obj_t *orb_ico = ln_w_label(orb, LV_SYMBOL_AUDIO, LN_FONT_HUGE,
                                   LN_COL_INK);
    lv_obj_center(orb_ico);

    ln_w_label(center, "or tap the orb to talk", LN_FONT_MD, LN_COL_MUTED);

    /* bottom status bar */
    lv_obj_t *bot = ln_w_plain(scr);
    lv_obj_set_size(bot, lv_pct(100), 92);
    lv_obj_align(bot, LV_ALIGN_BOTTOM_MID, 0, 0);
    lv_obj_set_style_bg_color(bot, LN_COL_SURFACE, 0);
    lv_obj_set_style_bg_opa(bot, LV_OPA_COVER, 0);
    lv_obj_set_style_border_color(bot, LN_COL_BORDER, 0);
    lv_obj_set_style_border_width(bot, 1, 0);
    lv_obj_set_style_border_side(bot, LV_BORDER_SIDE_TOP, 0);
    lv_obj_set_style_pad_hor(bot, 28, 0);
    lv_obj_set_flex_flow(bot, LV_FLEX_FLOW_ROW);
    lv_obj_set_flex_align(bot, LV_FLEX_ALIGN_SPACE_BETWEEN,
                          LV_FLEX_ALIGN_CENTER, LV_FLEX_ALIGN_CENTER);

    make_chip(bot, LV_SYMBOL_WIFI, "WI-FI", &s_wifi_value);
    make_chip(bot, LV_SYMBOL_UPLOAD, "CLOUD LINK", &s_cloud_value);
    make_chip(bot, LV_SYMBOL_ENVELOPE, "ACCOUNT", &s_account_value);

    ln_scr_idle_set_wake_phrase(s_wake_phrase);
    return scr;
}

void ln_scr_idle_tick(void)
{
    if (s_clock_big == NULL) {
        return;
    }
    time_t now = time(NULL);
    struct tm tm_now;
    localtime_r(&now, &tm_now);

    if (tm_now.tm_year + 1900 < 2020) { /* clock not yet SNTP-synced */
        lv_label_set_text(s_clock_big, "--:--");
        lv_label_set_text(s_clock_small, "--:--");
        lv_label_set_text(s_date_label, "Clock syncs when online");
        return;
    }

    char buf[40];
    int hr12 = tm_now.tm_hour % 12;
    if (hr12 == 0) {
        hr12 = 12;
    }
    snprintf(buf, sizeof(buf), "%d:%02d %s", hr12, tm_now.tm_min,
             tm_now.tm_hour < 12 ? "AM" : "PM");
    lv_label_set_text(s_clock_big, buf);
    lv_label_set_text(s_clock_small, buf);

    strftime(buf, sizeof(buf), "%A, %B %d", &tm_now);
    lv_label_set_text(s_date_label, buf);
}

void ln_scr_idle_set_wifi(const char *text)
{
    if (s_wifi_value != NULL) {
        lv_label_set_text(s_wifi_value, text);
    }
}

void ln_scr_idle_set_cloud(const char *text)
{
    if (s_cloud_value != NULL) {
        lv_label_set_text(s_cloud_value, text);
    }
}

void ln_scr_idle_set_account(const char *text)
{
    if (s_account_value != NULL) {
        lv_label_set_text(s_account_value, text);
    }
}

void ln_scr_idle_set_wake_phrase(const char *phrase)
{
    if (phrase == NULL || phrase[0] == '\0') {
        return;
    }
    strlcpy(s_wake_phrase, phrase, sizeof(s_wake_phrase));
    if (s_wake_hint != NULL) {
        lv_label_set_text_fmt(s_wake_hint, "Say \"%s\"", s_wake_phrase);
    }
}
