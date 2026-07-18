/*
 * ln_ui_scr_onboarding.c — Provisioning screen.
 *
 * Layout (owner decision 2026-07-18): welcome message at the TOP, the QR
 * code heroed at the BOTTOM, and setup is completable entirely on the
 * touchscreen via two options in the middle:
 *   - "Join a Wi-Fi network"  -> full-screen scanned-SSID list (list-select,
 *     not an overlay dropdown) -> on-screen keyboard for the passphrase only
 *     (LCD rule: keyboard is the last resort, passphrases qualify).
 *   - "Use the setup hotspot" -> stay in AP mode, subnet select 10.0.0.x
 *     (default) or 192.168.4.x.
 *
 * Phase 2 (pairing) keeps the same top/bottom frame: instructions replace
 * the option buttons, the claim-URL QR takes the bottom hero.
 *
 * Every setter here assumes the LVGL lock is held (ln_ui_internal.h rule).
 * lv_timer callbacks run on the LVGL task, so they may touch widgets freely;
 * ln_net_scan_results()/ln_net_portal_url() never block.
 */
#include <stdio.h>
#include <string.h>

#include "ln_net.h"
#include "ln_ui_internal.h"

#define ONB_MAX_APS 24

/* ---- top-level frame ---- */
static lv_obj_t *s_view_main;
static lv_obj_t *s_view_list;
static lv_obj_t *s_view_pw;
static lv_obj_t *s_view_ap;

static lv_obj_t *s_title;
static lv_obj_t *s_subtitle;
static lv_obj_t *s_options_row;
static lv_obj_t *s_qr;
static lv_obj_t *s_qr_head;
static lv_obj_t *s_qr_caption;
static lv_obj_t *s_code_label;
static lv_obj_t *s_status;

/* ---- network list view ---- */
static lv_obj_t *s_net_count;
static lv_obj_t *s_net_list;
static lv_timer_t *s_scan_timer;
static ln_net_scan_ap_t s_aps[ONB_MAX_APS];
static int s_ap_count;

/* ---- password view ---- */
static lv_obj_t *s_pw_title;
static lv_obj_t *s_pw_ta;
static lv_obj_t *s_pw_kb;
static char s_sel_ssid[33];

/* ---- AP-mode view ---- */
static lv_obj_t *s_ap_btn_10;
static lv_obj_t *s_ap_btn_192;
static char s_ap_subnet[16] = "10.0.0";

static lv_timer_t *s_qr_refresh_timer;

/* ------------------------------------------------------------- helpers */

static void stop_scan_timer(void)
{
    if (s_scan_timer != NULL) {
        lv_timer_delete(s_scan_timer);
        s_scan_timer = NULL;
    }
}

static void show_view(lv_obj_t *view)
{
    lv_obj_t *views[] = { s_view_main, s_view_list, s_view_pw, s_view_ap };
    for (size_t i = 0; i < sizeof(views) / sizeof(views[0]); i++) {
        if (views[i] != NULL) {
            if (views[i] == view) {
                lv_obj_remove_flag(views[i], LV_OBJ_FLAG_HIDDEN);
            } else {
                lv_obj_add_flag(views[i], LV_OBJ_FLAG_HIDDEN);
            }
        }
    }
    if (view != s_view_list) {
        stop_scan_timer();
    }
}

static void set_qr(const char *url)
{
    if (s_qr == NULL || url == NULL || url[0] == '\0') {
        return;
    }
    lv_qrcode_update(s_qr, url, (uint32_t)strlen(url));
    lv_label_set_text(s_qr_caption, url);
}

static void qr_refresh_cb(lv_timer_t *t)
{
    (void)t;
    s_qr_refresh_timer = NULL;
    char url[40];
    ln_net_portal_url(url, sizeof(url));
    set_qr(url);
}

/* The SoftAP re-IP after a subnet change is deferred ~600ms (ln_net) — pull
 * the fresh gateway into the QR shortly after. */
static void schedule_qr_refresh(void)
{
    if (s_qr_refresh_timer != NULL) {
        lv_timer_delete(s_qr_refresh_timer);
    }
    s_qr_refresh_timer = lv_timer_create(qr_refresh_cb, 1200, NULL);
    lv_timer_set_repeat_count(s_qr_refresh_timer, 1);
}

/* ---------------------------------------------------- network list view */

static const char *rssi_word(int rssi)
{
    if (rssi >= -55) return "strong";
    if (rssi >= -70) return "good";
    return "weak";
}

static void pw_open_for(const char *ssid);

static void net_row_cb(lv_event_t *e)
{
    int idx = (int)(intptr_t)lv_event_get_user_data(e);
    if (idx < 0 || idx >= s_ap_count) {
        return;
    }
    strlcpy(s_sel_ssid, s_aps[idx].ssid, sizeof(s_sel_ssid));
    if (s_aps[idx].secure) {
        pw_open_for(s_sel_ssid);
    } else {
        /* Open network — join straight away. */
        ln_net_join_wifi(s_sel_ssid, "");
        char line[64];
        snprintf(line, sizeof(line), "Connecting to %s…", s_sel_ssid);
        show_view(s_view_main);
        lv_label_set_text(s_status, line);
    }
}

static void rebuild_net_list(void)
{
    lv_obj_clean(s_net_list);
    for (int i = 0; i < s_ap_count; i++) {
        lv_obj_t *row = lv_button_create(s_net_list);
        lv_obj_remove_style_all(row);
        lv_obj_set_size(row, lv_pct(100), 64);
        lv_obj_set_style_bg_color(row, LN_COL_SURFACE, 0);
        lv_obj_set_style_bg_opa(row, LV_OPA_COVER, 0);
        lv_obj_set_style_bg_color(row, LN_COL_SURFACE2, LV_STATE_PRESSED);
        lv_obj_set_style_radius(row, LN_RADIUS, 0);
        lv_obj_set_style_pad_hor(row, 22, 0);
        lv_obj_add_event_cb(row, net_row_cb, LV_EVENT_CLICKED,
                            (void *)(intptr_t)i);

        lv_obj_t *ssid = ln_w_label(row, s_aps[i].ssid, LN_FONT_MD, LN_COL_TEXT);
        lv_label_set_long_mode(ssid, LV_LABEL_LONG_DOT);
        lv_obj_set_style_max_width(ssid, 760, 0);
        lv_obj_align(ssid, LV_ALIGN_LEFT_MID, 0, 0);

        char detail[48];
        snprintf(detail, sizeof(detail), "%s %s · %s",
                 LV_SYMBOL_WIFI, rssi_word(s_aps[i].rssi),
                 s_aps[i].secure ? "secured" : "open");
        lv_obj_t *d = ln_w_label(row, detail, LN_FONT_SM, LN_COL_MUTED);
        lv_obj_align(d, LV_ALIGN_RIGHT_MID, 0, 0);
    }
}

static void scan_tick_cb(lv_timer_t *t)
{
    (void)t;
    bool scanning = false;
    int64_t age_ms = -1;
    int n = ln_net_scan_results(s_aps, ONB_MAX_APS, &scanning, &age_ms);
    if (n != s_ap_count || (!scanning && n > 0)) {
        s_ap_count = n;
        rebuild_net_list();
    }
    s_ap_count = n;

    char line[64];
    if (scanning) {
        snprintf(line, sizeof(line), "Scanning… %d network%s so far",
                 n, n == 1 ? "" : "s");
    } else {
        snprintf(line, sizeof(line), "%d network%s found",
                 n, n == 1 ? "" : "s");
        stop_scan_timer();
    }
    lv_label_set_text(s_net_count, line);
}

static void open_net_list(void)
{
    show_view(s_view_list);
    s_ap_count = 0;
    lv_obj_clean(s_net_list);
    lv_label_set_text(s_net_count, "Scanning…");
    /* Explicit user action — a rescan is allowed to blip the hotspot. */
    ln_net_scan_request();
    stop_scan_timer();
    s_scan_timer = lv_timer_create(scan_tick_cb, 700, NULL);
}

static void join_option_cb(lv_event_t *e)
{
    (void)e;
    open_net_list();
}

static void rescan_cb(lv_event_t *e)
{
    (void)e;
    open_net_list();
}

static void list_back_cb(lv_event_t *e)
{
    (void)e;
    show_view(s_view_main);
}

/* ------------------------------------------------------- password view */

static void pw_open_for(const char *ssid)
{
    char title[64];
    snprintf(title, sizeof(title), "Password for %s", ssid);
    lv_label_set_text(s_pw_title, title);
    lv_textarea_set_text(s_pw_ta, "");
    show_view(s_view_pw);
    lv_keyboard_set_textarea(s_pw_kb, s_pw_ta);
}

static void pw_back_cb(lv_event_t *e)
{
    (void)e;
    show_view(s_view_list);
    /* Re-enter the list flow (fresh cache read, no forced rescan). */
    stop_scan_timer();
    s_scan_timer = lv_timer_create(scan_tick_cb, 700, NULL);
}

static void pw_toggle_cb(lv_event_t *e)
{
    lv_obj_t *btn = lv_event_get_target(e);
    bool hidden = lv_textarea_get_password_mode(s_pw_ta);
    lv_textarea_set_password_mode(s_pw_ta, !hidden);
    lv_obj_t *lbl = lv_obj_get_child(btn, 0);
    if (lbl != NULL) {
        /* Open eye while hidden ("tap to reveal"), closed eye while shown. */
        lv_label_set_text(lbl, hidden ? LV_SYMBOL_EYE_CLOSE : LV_SYMBOL_EYE_OPEN);
    }
}

static void pw_connect(void)
{
    const char *pass = lv_textarea_get_text(s_pw_ta);
    if (ln_net_join_wifi(s_sel_ssid, pass) != ESP_OK) {
        lv_label_set_text(s_status, "Couldn't start the connection — try again.");
        show_view(s_view_main);
        return;
    }
    char line[64];
    snprintf(line, sizeof(line), "Connecting to %s…", s_sel_ssid);
    show_view(s_view_main);
    lv_label_set_text(s_status, line);
}

static void pw_connect_cb(lv_event_t *e)
{
    (void)e;
    pw_connect();
}

static void pw_kb_event_cb(lv_event_t *e)
{
    if (lv_event_get_code(e) == LV_EVENT_READY) {
        pw_connect();          /* keyboard checkmark == Connect */
    }
}

/* -------------------------------------------------------- AP-mode view */

static void ap_render_choice(void)
{
    bool ten = (strcmp(s_ap_subnet, "10.0.0") == 0);
    lv_obj_set_style_border_color(s_ap_btn_10, ten ? LN_COL_TEAL : LN_COL_BORDER, 0);
    lv_obj_set_style_border_width(s_ap_btn_10, ten ? 3 : 1, 0);
    lv_obj_set_style_border_color(s_ap_btn_192, ten ? LN_COL_BORDER : LN_COL_TEAL, 0);
    lv_obj_set_style_border_width(s_ap_btn_192, ten ? 1 : 3, 0);
}

static void ap_pick_10_cb(lv_event_t *e)
{
    (void)e;
    strlcpy(s_ap_subnet, "10.0.0", sizeof(s_ap_subnet));
    ap_render_choice();
}

static void ap_pick_192_cb(lv_event_t *e)
{
    (void)e;
    strlcpy(s_ap_subnet, "192.168.4", sizeof(s_ap_subnet));
    ap_render_choice();
}

static void ap_option_cb(lv_event_t *e)
{
    (void)e;
    /* Preselect whatever subnet is live right now. */
    char url[40] = {0};
    ln_net_portal_url(url, sizeof(url)); /* "http://a.b.c.1/" */
    strlcpy(s_ap_subnet, strstr(url, "192.168.4") != NULL ? "192.168.4" : "10.0.0",
            sizeof(s_ap_subnet));
    ap_render_choice();
    show_view(s_view_ap);
}

static void ap_back_cb(lv_event_t *e)
{
    (void)e;
    show_view(s_view_main);
}

static void ap_apply_cb(lv_event_t *e)
{
    (void)e;
    ln_net_choose_ap_mode(s_ap_subnet);
    show_view(s_view_main);
    char line[96];
    snprintf(line, sizeof(line),
             "Hotspot mode — connect to \"%s\" and scan the QR below.",
             "LiveNinja-Setup");
    lv_label_set_text(s_status, line);
    schedule_qr_refresh();
}

/* ------------------------------------------------------------ builders */

/* Big two-line option button (title + subtitle), ~48-64px-rule compliant.
 * Full-width: the Tab5 panel is 720x1280 PORTRAIT (ln_ui logs "UI ready
 * (720x1280)") — fixed-width side-by-side options overflowed it. */
static lv_obj_t *make_option(lv_obj_t *parent, const char *title,
                             const char *sub, lv_event_cb_t cb)
{
    lv_obj_t *btn = lv_button_create(parent);
    lv_obj_remove_style_all(btn);
    lv_obj_set_size(btn, lv_pct(100), 116);
    lv_obj_set_style_bg_color(btn, LN_COL_SURFACE, 0);
    lv_obj_set_style_bg_opa(btn, LV_OPA_COVER, 0);
    lv_obj_set_style_bg_color(btn, LN_COL_SURFACE2, LV_STATE_PRESSED);
    lv_obj_set_style_border_color(btn, LN_COL_BORDER, 0);
    lv_obj_set_style_border_width(btn, 1, 0);
    lv_obj_set_style_radius(btn, LN_RADIUS, 0);
    lv_obj_set_style_pad_all(btn, 20, 0);
    lv_obj_add_event_cb(btn, cb, LV_EVENT_CLICKED, NULL);

    lv_obj_t *col = ln_w_col(btn, 6);
    lv_obj_set_width(col, lv_pct(100));
    lv_obj_align(col, LV_ALIGN_LEFT_MID, 0, 0);
    ln_w_label(col, title, LN_FONT_LG, LN_COL_TEAL);
    lv_obj_t *s = ln_w_label(col, sub, LN_FONT_SM, LN_COL_MUTED);
    lv_label_set_long_mode(s, LV_LABEL_LONG_WRAP);
    lv_obj_set_width(s, lv_pct(100));
    return btn;
}

static lv_obj_t *make_header(lv_obj_t *parent, const char *title_txt,
                             lv_event_cb_t back_cb)
{
    lv_obj_t *row = ln_w_row(parent, 18);
    lv_obj_set_width(row, lv_pct(100));
    ln_w_button(row, LV_SYMBOL_LEFT " Back", LN_COL_SURFACE2, LN_COL_TEXT,
                back_cb);
    lv_obj_t *t = ln_w_label(row, title_txt, LN_FONT_XL, LN_COL_TEXT);
    lv_obj_set_flex_grow(t, 1);
    return row;
}

lv_obj_t *ln_scr_onboarding_create(void)
{
    lv_obj_t *scr = ln_w_screen();

    /* ================= main view: welcome top, options mid, QR bottom */
    s_view_main = ln_w_plain(scr);
    lv_obj_set_size(s_view_main, lv_pct(100), lv_pct(100));
    lv_obj_set_flex_flow(s_view_main, LV_FLEX_FLOW_COLUMN);
    lv_obj_set_flex_align(s_view_main, LV_FLEX_ALIGN_START,
                          LV_FLEX_ALIGN_CENTER, LV_FLEX_ALIGN_CENTER);
    lv_obj_set_style_pad_all(s_view_main, 32, 0);
    lv_obj_set_style_pad_row(s_view_main, 14, 0);

    lv_obj_t *badge = ln_w_label(s_view_main, "FIRST-TIME SETUP", LN_FONT_XS,
                                 LN_COL_WARN);
    lv_obj_set_style_text_letter_space(badge, 2, 0);

    s_title = ln_w_label(s_view_main, "Welcome to Live Ninja", LN_FONT_XXL,
                         LN_COL_TEXT);
    s_subtitle = ln_w_label(s_view_main,
        "Let's get this device online. Set up Wi-Fi right here on the "
        "screen, or use your phone.", LN_FONT_SM, LN_COL_MUTED);
    lv_label_set_long_mode(s_subtitle, LV_LABEL_LONG_WRAP);
    lv_obj_set_width(s_subtitle, lv_pct(88));
    lv_obj_set_style_text_align(s_subtitle, LV_TEXT_ALIGN_CENTER, 0);

    /* Portrait screen: options stack vertically, full width. */
    s_options_row = ln_w_col(s_view_main, 16);
    lv_obj_set_width(s_options_row, lv_pct(100));
    lv_obj_set_style_pad_top(s_options_row, 10, 0);
    make_option(s_options_row, "Join a Wi-Fi network",
                "Pick your network from a list and type its password here.",
                join_option_cb);
    make_option(s_options_row, "Use the setup hotspot",
                "Keep this device as its own access point (AP mode).",
                ap_option_cb);

    /* QR hero — fills all remaining screen below the options (the "box"
     * is tall; the QR itself stays its normal size, centered in it). */
    lv_obj_t *hero = ln_w_card(s_view_main);
    lv_obj_set_width(hero, lv_pct(100));
    lv_obj_set_flex_grow(hero, 1);
    lv_obj_set_flex_flow(hero, LV_FLEX_FLOW_COLUMN);
    lv_obj_set_flex_align(hero, LV_FLEX_ALIGN_CENTER, LV_FLEX_ALIGN_CENTER,
                          LV_FLEX_ALIGN_CENTER);
    lv_obj_set_style_pad_row(hero, 10, 0);

    s_qr_head = ln_w_label(hero, "Or scan with your phone", LN_FONT_MD,
                           LN_COL_TEXT);

    s_code_label = ln_w_label(hero, "", LN_FONT_HUGE, LN_COL_TEAL);
    lv_obj_set_style_text_letter_space(s_code_label, 8, 0);
    lv_obj_add_flag(s_code_label, LV_OBJ_FLAG_HIDDEN);

    s_qr = lv_qrcode_create(hero);
    lv_qrcode_set_size(s_qr, 230);
    lv_qrcode_set_dark_color(s_qr, LN_COL_BG);
    lv_qrcode_set_light_color(s_qr, LN_COL_TEXT);
    lv_qrcode_update(s_qr, "http://10.0.0.1",
                     (uint32_t)strlen("http://10.0.0.1"));

    s_qr_caption = ln_w_label(hero, "http://10.0.0.1", LN_FONT_SM,
                              LN_COL_TEAL);

    s_status = ln_w_label(s_view_main, "Waiting for a device to connect…",
                          LN_FONT_SM, LN_COL_DIM);

    /* ================= network list view */
    s_view_list = ln_w_plain(scr);
    lv_obj_set_size(s_view_list, lv_pct(100), lv_pct(100));
    lv_obj_set_flex_flow(s_view_list, LV_FLEX_FLOW_COLUMN);
    lv_obj_set_style_pad_all(s_view_list, 32, 0);
    lv_obj_set_style_pad_row(s_view_list, 14, 0);
    lv_obj_add_flag(s_view_list, LV_OBJ_FLAG_HIDDEN);

    lv_obj_t *lh = make_header(s_view_list, "Choose your Wi-Fi network",
                               list_back_cb);
    ln_w_button(lh, LV_SYMBOL_REFRESH " Rescan", LN_COL_SURFACE2, LN_COL_TEXT,
                rescan_cb);

    s_net_count = ln_w_label(s_view_list, "Scanning…", LN_FONT_SM, LN_COL_DIM);

    s_net_list = ln_w_col(s_view_list, 10);
    lv_obj_set_width(s_net_list, lv_pct(100));
    lv_obj_set_flex_grow(s_net_list, 1);
    lv_obj_add_flag(s_net_list, LV_OBJ_FLAG_SCROLLABLE);
    lv_obj_set_scroll_dir(s_net_list, LV_DIR_VER);

    /* ================= password view */
    s_view_pw = ln_w_plain(scr);
    lv_obj_set_size(s_view_pw, lv_pct(100), lv_pct(100));
    lv_obj_set_flex_flow(s_view_pw, LV_FLEX_FLOW_COLUMN);
    /* Cross-axis center: full-width rows are unaffected, content-sized
     * items (the Connect button) center horizontally. */
    lv_obj_set_flex_align(s_view_pw, LV_FLEX_ALIGN_START, LV_FLEX_ALIGN_CENTER,
                          LV_FLEX_ALIGN_CENTER);
    lv_obj_set_style_pad_all(s_view_pw, 32, 0);
    lv_obj_set_style_pad_row(s_view_pw, 16, 0);
    lv_obj_add_flag(s_view_pw, LV_OBJ_FLAG_HIDDEN);

    /* Owner-specified layout: Back alone on top; "Password for <ssid>"
     * centered on its own line; the password field with an eye
     * show/hide icon; Connect below; keyboard at the bottom. */
    lv_obj_t *ph = ln_w_row(s_view_pw, 18);
    lv_obj_set_width(ph, lv_pct(100));
    ln_w_button(ph, LV_SYMBOL_LEFT " Back", LN_COL_SURFACE2, LN_COL_TEXT,
                pw_back_cb);

    s_pw_title = ln_w_label(s_view_pw, "Password", LN_FONT_XL, LN_COL_TEXT);
    lv_obj_set_width(s_pw_title, lv_pct(100));
    lv_obj_set_style_text_align(s_pw_title, LV_TEXT_ALIGN_CENTER, 0);
    lv_label_set_long_mode(s_pw_title, LV_LABEL_LONG_WRAP);

    lv_obj_t *prow = ln_w_row(s_view_pw, 14);
    lv_obj_set_width(prow, lv_pct(100));
    s_pw_ta = lv_textarea_create(prow);
    lv_textarea_set_one_line(s_pw_ta, true);
    lv_textarea_set_password_mode(s_pw_ta, true);
    lv_textarea_set_placeholder_text(s_pw_ta, "Wi-Fi password");
    lv_obj_set_flex_grow(s_pw_ta, 1);
    lv_obj_set_height(s_pw_ta, 64);
    lv_obj_set_style_bg_color(s_pw_ta, LN_COL_SURFACE, 0);
    lv_obj_set_style_text_color(s_pw_ta, LN_COL_TEXT, 0);
    lv_obj_set_style_border_color(s_pw_ta, LN_COL_BORDER, 0);
    lv_obj_set_style_text_font(s_pw_ta, LN_FONT_MD, 0);
    /* Eye icon toggles show/hide (replaces the old "Show" text button). */
    ln_w_button(prow, LV_SYMBOL_EYE_OPEN, LN_COL_SURFACE2, LN_COL_TEXT,
                pw_toggle_cb);

    /* Natural button size, centered by the column's cross-axis alignment. */
    ln_w_button(s_view_pw, "Connect", LN_COL_TEAL, LN_COL_INK, pw_connect_cb);

    /* Spacer pushes the keyboard to the bottom quarter — 50% made each key
     * row ~160px, twice what a finger needs (owner feedback). */
    lv_obj_t *kb_spacer = ln_w_plain(s_view_pw);
    lv_obj_set_flex_grow(kb_spacer, 1);

    s_pw_kb = lv_keyboard_create(s_view_pw);
    lv_obj_set_width(s_pw_kb, lv_pct(100));
    lv_obj_set_height(s_pw_kb, lv_pct(25));
    lv_obj_set_style_text_font(s_pw_kb, &lv_font_montserrat_28, 0);
    lv_keyboard_set_textarea(s_pw_kb, s_pw_ta);
    lv_obj_add_event_cb(s_pw_kb, pw_kb_event_cb, LV_EVENT_READY, NULL);

    /* ================= AP-mode view */
    s_view_ap = ln_w_plain(scr);
    lv_obj_set_size(s_view_ap, lv_pct(100), lv_pct(100));
    lv_obj_set_flex_flow(s_view_ap, LV_FLEX_FLOW_COLUMN);
    lv_obj_set_style_pad_all(s_view_ap, 32, 0);
    lv_obj_set_style_pad_row(s_view_ap, 16, 0);
    lv_obj_add_flag(s_view_ap, LV_OBJ_FLAG_HIDDEN);

    make_header(s_view_ap, "Use the setup hotspot", ap_back_cb);

    lv_obj_t *hint = ln_w_label(s_view_ap,
        "This device keeps broadcasting the open \"LiveNinja-Setup\" network. "
        "Connect to it from any phone or laptop to reach the device. Choose "
        "the address range the hotspot should use:", LN_FONT_SM, LN_COL_MUTED);
    lv_label_set_long_mode(hint, LV_LABEL_LONG_WRAP);
    lv_obj_set_width(hint, lv_pct(100));

    s_ap_btn_10 = make_option(s_view_ap, "10.0.0.x  (recommended)",
                              "Device address 10.0.0.1 — avoids clashing "
                              "with most home routers.", ap_pick_10_cb);
    s_ap_btn_192 = make_option(s_view_ap, "192.168.4.x",
                               "Device address 192.168.4.1 — the classic "
                               "ESP hotspot range.", ap_pick_192_cb);
    lv_obj_set_width(s_ap_btn_10, lv_pct(100));
    lv_obj_set_width(s_ap_btn_192, lv_pct(100));

    ln_w_button(s_view_ap, "Keep hotspot mode", LN_COL_TEAL, LN_COL_INK,
                ap_apply_cb);
    ap_render_choice();

    ln_scr_onboarding_portal("LiveNinja-Setup", NULL);
    return scr;
}

/* ------------------------------------------------------ phase switchers */

void ln_scr_onboarding_portal(const char *ssid, const char *url)
{
    if (s_title == NULL) {
        return;
    }
    char live[40];
    if (url == NULL || url[0] == '\0') {
        ln_net_portal_url(live, sizeof(live));
        url = live;
    }
    (void)ssid;

    lv_label_set_text(s_title, "Welcome to Live Ninja");
    lv_label_set_text(s_subtitle,
        "Let's get this device online. Set up Wi-Fi right here on the "
        "screen, or use your phone.");
    lv_obj_remove_flag(s_options_row, LV_OBJ_FLAG_HIDDEN);
    lv_label_set_text(s_qr_head, "Or scan with your phone");
    lv_obj_add_flag(s_code_label, LV_OBJ_FLAG_HIDDEN);
    set_qr(url);
    lv_label_set_text(s_status, "Waiting for a device to connect…");
    show_view(s_view_main);
}

void ln_scr_onboarding_pairing(const char *claim_url, const char *code)
{
    if (s_title == NULL || claim_url == NULL || claim_url[0] == '\0') {
        return;
    }
    lv_label_set_text(s_title, "Link your Amazon account");
    lv_label_set_text(s_subtitle,
        "Wi-Fi is connected. Scan the QR code below (or open the link) on "
        "your phone and sign in with Amazon to claim this device.");
    lv_obj_add_flag(s_options_row, LV_OBJ_FLAG_HIDDEN);
    lv_label_set_text(s_qr_head, "Scan to sign in");

    if (code != NULL && code[0] != '\0') {
        lv_label_set_text(s_code_label, code);
        lv_obj_remove_flag(s_code_label, LV_OBJ_FLAG_HIDDEN);
    } else {
        lv_obj_add_flag(s_code_label, LV_OBJ_FLAG_HIDDEN);
    }
    set_qr(claim_url);
    lv_label_set_text(s_status, "Waiting for you to approve in the browser…");
    show_view(s_view_main);
}

void ln_scr_onboarding_connected(const char *ip)
{
    if (s_title == NULL || ip == NULL || ip[0] == '\0') {
        return;
    }
    lv_label_set_text(s_title, "Wi-Fi connected");
    lv_label_set_text(s_subtitle,
        "This device is online. Linking your account — the QR below will "
        "update when the sign-in link is ready.");
    lv_obj_add_flag(s_options_row, LV_OBJ_FLAG_HIDDEN);
    lv_label_set_text(s_qr_head, "Device address");

    char url[40];
    snprintf(url, sizeof(url), "http://%s/", ip);
    set_qr(url);
    lv_obj_add_flag(s_code_label, LV_OBJ_FLAG_HIDDEN);
    lv_label_set_text(s_status, "Connected — linking your account…");
    show_view(s_view_main);
}

void ln_scr_onboarding_status(const char *text)
{
    if (s_status != NULL && text != NULL) {
        lv_label_set_text(s_status, text);
    }
}
