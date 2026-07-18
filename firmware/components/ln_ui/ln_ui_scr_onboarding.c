/*
 * ln_ui_scr_onboarding.c — Provisioning screen (mockup 01).
 *
 * Phase 1 (portal): join-the-hotspot steps + QR of http://192.168.4.1.
 * Phase 2 (pairing): claim-URL QR + short pairing code (contracts/api.md
 * device register -> browser claim -> poll).
 */
#include <stdio.h>
#include <string.h>

#include "ln_ui_internal.h"

static lv_obj_t *s_step_labels[3];
static lv_obj_t *s_step_subs[3];
static lv_obj_t *s_title;
static lv_obj_t *s_subtitle;
static lv_obj_t *s_qr;
static lv_obj_t *s_qr_caption;
static lv_obj_t *s_code_label;
static lv_obj_t *s_status;

static void set_step(int idx, const char *main_txt, const char *sub_txt)
{
    if (idx < 0 || idx >= 3 || s_step_labels[idx] == NULL) {
        return;
    }
    lv_label_set_text(s_step_labels[idx], main_txt);
    lv_label_set_text(s_step_subs[idx], sub_txt);
}

static lv_obj_t *make_step_row(lv_obj_t *parent, int num)
{
    lv_obj_t *row = ln_w_row(parent, 18);
    lv_obj_set_width(row, lv_pct(100));
    lv_obj_set_flex_align(row, LV_FLEX_ALIGN_START, LV_FLEX_ALIGN_START,
                          LV_FLEX_ALIGN_START);

    lv_obj_t *badge = lv_obj_create(row);
    lv_obj_remove_style_all(badge);
    lv_obj_set_size(badge, 44, 44);
    lv_obj_set_style_radius(badge, LV_RADIUS_CIRCLE, 0);
    lv_obj_set_style_bg_color(badge, LN_COL_TEAL_DARK, 0);
    lv_obj_set_style_bg_opa(badge, LV_OPA_COVER, 0);
    lv_obj_t *n = lv_label_create(badge);
    lv_label_set_text_fmt(n, "%d", num);
    lv_obj_set_style_text_font(n, LN_FONT_MD, 0);
    lv_obj_set_style_text_color(n, LN_COL_TEAL, 0);
    lv_obj_center(n);

    lv_obj_t *col = ln_w_col(row, 4);
    lv_obj_set_flex_grow(col, 1);
    s_step_labels[num - 1] = ln_w_label(col, "", LN_FONT_MD, LN_COL_TEXT);
    lv_label_set_long_mode(s_step_labels[num - 1], LV_LABEL_LONG_WRAP);
    lv_obj_set_width(s_step_labels[num - 1], lv_pct(100));
    s_step_subs[num - 1] = ln_w_label(col, "", LN_FONT_XS, LN_COL_DIM);
    lv_label_set_long_mode(s_step_subs[num - 1], LV_LABEL_LONG_WRAP);
    lv_obj_set_width(s_step_subs[num - 1], lv_pct(100));
    return row;
}

lv_obj_t *ln_scr_onboarding_create(void)
{
    lv_obj_t *scr = ln_w_screen();

    lv_obj_t *body = ln_w_plain(scr);
    lv_obj_set_size(body, lv_pct(100), lv_pct(100));
    lv_obj_set_flex_flow(body, LV_FLEX_FLOW_ROW);
    lv_obj_set_style_pad_all(body, 40, 0);
    lv_obj_set_style_pad_column(body, 40, 0);

    /* left: steps */
    lv_obj_t *left = ln_w_col(body, 22);
    lv_obj_set_flex_grow(left, 1);
    lv_obj_set_height(left, lv_pct(100));

    lv_obj_t *badge = ln_w_label(left, "FIRST-TIME SETUP", LN_FONT_XS,
                                 LN_COL_WARN);
    lv_obj_set_style_text_letter_space(badge, 2, 0);

    s_title = ln_w_label(left, "Let's get you connected", LN_FONT_XXL,
                         LN_COL_TEXT);
    s_subtitle = ln_w_label(left,
        "Live Ninja needs Wi-Fi to reach the cloud voice assistant. Use a "
        "phone or laptop to finish setup.", LN_FONT_SM, LN_COL_MUTED);
    lv_label_set_long_mode(s_subtitle, LV_LABEL_LONG_WRAP);
    lv_obj_set_width(s_subtitle, lv_pct(100));

    make_step_row(left, 1);
    make_step_row(left, 2);
    make_step_row(left, 3);

    /* right: QR card */
    lv_obj_t *card = ln_w_card(body);
    lv_obj_set_size(card, 420, lv_pct(100));
    lv_obj_set_flex_flow(card, LV_FLEX_FLOW_COLUMN);
    lv_obj_set_flex_align(card, LV_FLEX_ALIGN_CENTER, LV_FLEX_ALIGN_CENTER,
                          LV_FLEX_ALIGN_CENTER);
    lv_obj_set_style_pad_row(card, 16, 0);

    ln_w_label(card, "Scan to open", LN_FONT_MD, LN_COL_TEXT);

    s_qr = lv_qrcode_create(card);
    lv_qrcode_set_size(s_qr, 260);
    lv_qrcode_set_dark_color(s_qr, LN_COL_BG);
    lv_qrcode_set_light_color(s_qr, LN_COL_TEXT);
    lv_qrcode_update(s_qr, "http://192.168.4.1",
                     (uint32_t)strlen("http://192.168.4.1"));

    s_qr_caption = ln_w_label(card, "http://192.168.4.1", LN_FONT_SM,
                              LN_COL_TEAL);

    s_code_label = ln_w_label(card, "", LN_FONT_HUGE, LN_COL_TEAL);
    lv_obj_set_style_text_letter_space(s_code_label, 8, 0);
    lv_obj_add_flag(s_code_label, LV_OBJ_FLAG_HIDDEN);

    /* footer status */
    s_status = ln_w_label(scr, "Waiting for a device to connect…", LN_FONT_SM,
                          LN_COL_DIM);
    lv_obj_align(s_status, LV_ALIGN_BOTTOM_MID, 0, -14);

    ln_scr_onboarding_portal("LiveNinja-Setup", "http://192.168.4.1");
    return scr;
}

void ln_scr_onboarding_portal(const char *ssid, const char *url)
{
    if (s_title == NULL) {
        return;
    }
    const char *use_ssid = (ssid != NULL && ssid[0]) ? ssid : "LiveNinja-Setup";
    const char *use_url = (url != NULL && url[0]) ? url : "http://192.168.4.1";

    lv_label_set_text(s_title, "Let's get you connected");

    char buf[96];
    set_step(1, "Open Wi-Fi settings on your phone or laptop",
             "On the device you're holding right now.");
    snprintf(buf, sizeof(buf), "Join the network \"%s\"", use_ssid);
    set_step(2, buf, "Open network — no password required.");
    snprintf(buf, sizeof(buf), "Open the setup page at %s", use_url);
    set_step(3, buf, "It usually pops up on its own (captive portal).");

    lv_qrcode_update(s_qr, use_url, (uint32_t)strlen(use_url));
    lv_label_set_text(s_qr_caption, use_url);
    lv_obj_add_flag(s_code_label, LV_OBJ_FLAG_HIDDEN);
    lv_label_set_text(s_status, "Waiting for a device to connect…");
}

void ln_scr_onboarding_pairing(const char *claim_url, const char *code)
{
    if (s_title == NULL || claim_url == NULL || claim_url[0] == '\0') {
        return;
    }
    lv_label_set_text(s_title, "Link your Amazon account");

    set_step(1, "Wi-Fi connected", "This device is online.");
    set_step(2, "Scan the QR code (or open the link) on your phone",
             "Sign in with Amazon to claim this device.");
    if (code != NULL && code[0] != '\0') {
        char buf[64];
        snprintf(buf, sizeof(buf), "Confirm the code %s matches", code);
        set_step(3, buf, "The code is shown below the QR too.");
        lv_label_set_text(s_code_label, code);
        lv_obj_remove_flag(s_code_label, LV_OBJ_FLAG_HIDDEN);
    } else {
        set_step(3, "Approve this device in the browser", "");
        lv_obj_add_flag(s_code_label, LV_OBJ_FLAG_HIDDEN);
    }

    lv_qrcode_update(s_qr, claim_url, (uint32_t)strlen(claim_url));
    lv_label_set_text(s_qr_caption, claim_url);
    lv_label_set_text(s_status, "Waiting for you to approve in the browser…");
}

void ln_scr_onboarding_connected(const char *ip)
{
    if (s_title == NULL || ip == NULL || ip[0] == '\0') {
        return;
    }
    lv_label_set_text(s_title, "Wi-Fi connected");

    set_step(1, "Wi-Fi connected", "This device is online.");
    set_step(2, "Linking your account", "Fetching a pairing link…");
    set_step(3, "Almost there", "The QR will update when the link is ready.");

    char url[40];
    snprintf(url, sizeof(url), "http://%s/", ip);
    lv_qrcode_update(s_qr, url, (uint32_t)strlen(url));
    lv_label_set_text(s_qr_caption, url);
    lv_obj_add_flag(s_code_label, LV_OBJ_FLAG_HIDDEN);
    lv_label_set_text(s_status, "Connected — linking your account…");
}

void ln_scr_onboarding_status(const char *text)
{
    if (s_status != NULL && text != NULL) {
        lv_label_set_text(s_status, text);
    }
}
