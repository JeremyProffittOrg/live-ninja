/*
 * ln_ui_scr_session.c — Listening / Thinking / Speaking screens
 * (mockups 04, 05, 06).
 */
#include <string.h>

#include "ln_ui_internal.h"

/* ================================================================== */
/* Listening                                                           */
/* ================================================================== */

#define MIC_BAR_COUNT 12
#define MIC_BAR_MAX_H 110
#define MIC_BAR_MIN_H 10

static lv_obj_t *s_listen_heading;
static lv_obj_t *s_listen_transcript;
static lv_obj_t *s_listen_scroll;
static lv_obj_t *s_mic_bars[MIC_BAR_COUNT];

/* center-weighted multipliers so the bar row looks like a waveform */
static const uint8_t k_bar_weight[MIC_BAR_COUNT] = {
    35, 55, 75, 90, 100, 96, 96, 100, 90, 75, 55, 35,
};

static void cancel_btn_cb(lv_event_t *e)
{
    (void)e;
    ln_ui_post(LN_UI_CANCEL_TAPPED, NULL, 0);
}

static void finish_btn_cb(lv_event_t *e)
{
    (void)e;
    ln_ui_post(LN_UI_FINISH_TAPPED, NULL, 0);
}

lv_obj_t *ln_scr_listening_create(void)
{
    lv_obj_t *scr = ln_w_screen();

    lv_obj_t *col = ln_w_col(scr, 20);
    lv_obj_set_size(col, lv_pct(100), lv_pct(100));
    lv_obj_set_flex_align(col, LV_FLEX_ALIGN_START, LV_FLEX_ALIGN_CENTER,
                          LV_FLEX_ALIGN_CENTER);
    lv_obj_set_style_pad_all(col, 28, 0);

    s_listen_heading = ln_w_label(col, "Listening…", LN_FONT_HUGE, LN_COL_TEAL);

    /* mic level bars */
    lv_obj_t *bars = ln_w_plain(col);
    lv_obj_set_size(bars, MIC_BAR_COUNT * 26, MIC_BAR_MAX_H + 8);
    lv_obj_set_flex_flow(bars, LV_FLEX_FLOW_ROW);
    lv_obj_set_flex_align(bars, LV_FLEX_ALIGN_SPACE_EVENLY, LV_FLEX_ALIGN_END,
                          LV_FLEX_ALIGN_CENTER);
    for (int i = 0; i < MIC_BAR_COUNT; i++) {
        lv_obj_t *bar = lv_obj_create(bars);
        lv_obj_remove_style_all(bar);
        lv_obj_set_size(bar, 14, MIC_BAR_MIN_H);
        lv_obj_set_style_radius(bar, 7, 0);
        lv_obj_set_style_bg_color(bar, LN_COL_TEAL, 0);
        lv_obj_set_style_bg_opa(bar, LV_OPA_COVER, 0);
        s_mic_bars[i] = bar;
    }

    /* live transcript card — the conversation is the point of this screen
     * (owner 2026-07-19), so it flex-grows to claim all height the fixed
     * rows (heading, bars, hint, actions) don't use instead of a fixed
     * 260 px strip. */
    lv_obj_t *card = ln_w_card(col);
    lv_obj_set_width(card, lv_pct(96));
    lv_obj_set_flex_grow(card, 1);
    s_listen_scroll = card;
    lv_obj_set_scroll_dir(card, LV_DIR_VER);

    lv_obj_t *cap = ln_w_label(card, "LIVE TRANSCRIPT", LN_FONT_XS, LN_COL_DIM);
    lv_obj_set_style_text_letter_space(cap, 2, 0);

    s_listen_transcript = ln_w_label(card, "…", LN_FONT_LG, LN_COL_TEXT);
    lv_label_set_long_mode(s_listen_transcript, LV_LABEL_LONG_WRAP);
    lv_obj_set_width(s_listen_transcript, lv_pct(100));
    lv_obj_align_to(s_listen_transcript, cap, LV_ALIGN_OUT_BOTTOM_LEFT, 0, 12);

    ln_w_label(col, "Keep talking naturally — I'll respond once you pause.",
               LN_FONT_SM, LN_COL_DIM);

    /* action row */
    lv_obj_t *actions = ln_w_row(col, 24);
    ln_w_button(actions, LV_SYMBOL_CLOSE "  Cancel", LN_COL_SURFACE2,
                LN_COL_TEXT, cancel_btn_cb);
    ln_w_button(actions, "Finish now  " LV_SYMBOL_RIGHT, LN_COL_TEAL,
                LN_COL_INK, finish_btn_cb);

    return scr;
}

void ln_scr_listening_reset(void)
{
    if (s_listen_transcript != NULL) {
        lv_label_set_text(s_listen_transcript, "…");
    }
    ln_scr_listening_set_level(0);
}

void ln_scr_listening_set_transcript(const char *text)
{
    if (s_listen_transcript == NULL) {
        return;
    }
    lv_label_set_text(s_listen_transcript,
                      (text != NULL && text[0] != '\0') ? text : "…");
    lv_obj_scroll_to_y(s_listen_scroll, LV_COORD_MAX, LV_ANIM_OFF);
}

void ln_scr_listening_set_level(uint8_t pct)
{
    if (pct > 100) {
        pct = 100;
    }
    for (int i = 0; i < MIC_BAR_COUNT; i++) {
        if (s_mic_bars[i] == NULL) {
            return;
        }
        int h = MIC_BAR_MIN_H +
                ((MIC_BAR_MAX_H - MIC_BAR_MIN_H) * pct * k_bar_weight[i]) /
                    (100 * 100);
        lv_obj_set_height(s_mic_bars[i], h);
    }
}

/* ================================================================== */
/* Thinking                                                            */
/* ================================================================== */

static lv_obj_t *s_think_request;

lv_obj_t *ln_scr_thinking_create(void)
{
    lv_obj_t *scr = ln_w_screen();

    lv_obj_t *col = ln_w_col(scr, 24);
    lv_obj_center(col);
    lv_obj_set_flex_align(col, LV_FLEX_ALIGN_CENTER, LV_FLEX_ALIGN_CENTER,
                          LV_FLEX_ALIGN_CENTER);

    lv_obj_t *spin = lv_spinner_create(col);
    lv_obj_set_size(spin, 96, 96);
    lv_obj_set_style_arc_color(spin, LN_COL_SURFACE2, LV_PART_MAIN);
    lv_obj_set_style_arc_color(spin, LN_COL_CYAN, LV_PART_INDICATOR);
    lv_obj_set_style_arc_width(spin, 8, LV_PART_MAIN);
    lv_obj_set_style_arc_width(spin, 8, LV_PART_INDICATOR);

    ln_w_label(col, "Working on it", LN_FONT_HUGE, LN_COL_CYAN);
    ln_w_label(col, "Thinking through your request", LN_FONT_MD, LN_COL_MUTED);

    lv_obj_t *card = ln_w_card(col);
    lv_obj_set_width(card, lv_pct(90)); /* 760px overflowed the 720px portrait panel */
    s_think_request = ln_w_label(card, "", LN_FONT_LG, LN_COL_TEXT);
    lv_label_set_long_mode(s_think_request, LV_LABEL_LONG_WRAP);
    lv_obj_set_width(s_think_request, lv_pct(100));
    lv_obj_set_style_text_align(s_think_request, LV_TEXT_ALIGN_CENTER, 0);

    return scr;
}

void ln_scr_thinking_set_request(const char *text)
{
    if (s_think_request == NULL) {
        return;
    }
    if (text != NULL && text[0] != '\0') {
        lv_label_set_text_fmt(s_think_request, "\"%s\"", text);
    } else {
        lv_label_set_text(s_think_request, "");
    }
}

/* ================================================================== */
/* Speaking                                                            */
/* ================================================================== */

static lv_obj_t *s_speak_text;
static lv_obj_t *s_speak_scroll;

static void stop_tap_cb(lv_event_t *e)
{
    (void)e;
    ln_ui_post(LN_UI_STOP_TAPPED, NULL, 0);
}

lv_obj_t *ln_scr_speaking_create(void)
{
    lv_obj_t *scr = ln_w_screen();
    /* whole screen is the barge-in target */
    lv_obj_add_flag(scr, LV_OBJ_FLAG_CLICKABLE);
    lv_obj_add_event_cb(scr, stop_tap_cb, LV_EVENT_CLICKED, NULL);

    lv_obj_t *col = ln_w_col(scr, 22);
    lv_obj_set_size(col, lv_pct(100), lv_pct(100));
    lv_obj_set_flex_align(col, LV_FLEX_ALIGN_START, LV_FLEX_ALIGN_CENTER,
                          LV_FLEX_ALIGN_CENTER);
    lv_obj_set_style_pad_all(col, 28, 0);

    lv_obj_t *head = ln_w_row(col, 16);
    lv_obj_t *dot = lv_obj_create(head);
    lv_obj_remove_style_all(dot);
    lv_obj_set_size(dot, 18, 18);
    lv_obj_set_style_radius(dot, LV_RADIUS_CIRCLE, 0);
    lv_obj_set_style_bg_color(dot, LN_COL_SUCCESS, 0);
    lv_obj_set_style_bg_opa(dot, LV_OPA_COVER, 0);
    ln_w_label(head, "Speaking", LN_FONT_HUGE, LN_COL_SUCCESS);

    /* Response text card fills the screen (owner 2026-07-19) — same
     * flex-grow treatment as the Listening transcript card. */
    lv_obj_t *card = ln_w_card(col);
    lv_obj_set_width(card, lv_pct(96));
    lv_obj_set_flex_grow(card, 1);
    lv_obj_set_scroll_dir(card, LV_DIR_VER);
    s_speak_scroll = card;

    s_speak_text = ln_w_label(card, "", LN_FONT_LG, LN_COL_TEXT);
    lv_label_set_long_mode(s_speak_text, LV_LABEL_LONG_WRAP);
    lv_obj_set_width(s_speak_text, lv_pct(100));

    lv_obj_t *stop = ln_w_button(col, LV_SYMBOL_PAUSE "  Tap anywhere to interrupt",
                                 LN_COL_SURFACE2, LN_COL_TEXT, stop_tap_cb);
    lv_obj_set_style_border_color(stop, LN_COL_BORDER, 0);
    lv_obj_set_style_border_width(stop, 1, 0);

    return scr;
}

void ln_scr_speaking_reset(void)
{
    if (s_speak_text != NULL) {
        lv_label_set_text(s_speak_text, "");
    }
}

void ln_scr_speaking_set_text(const char *text)
{
    if (s_speak_text == NULL) {
        return;
    }
    lv_label_set_text(s_speak_text, text != NULL ? text : "");
    lv_obj_scroll_to_y(s_speak_scroll, LV_COORD_MAX, LV_ANIM_OFF);
}
