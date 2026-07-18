/*
 * ln_ui_scr_error.c — Error / offline screen (mockup 09).
 * Retry posts LN_UI_RETRY_TAPPED; Wi-Fi settings posts
 * LN_UI_WIFI_SETUP_REQUESTED (Error -> Provisioning path).
 */
#include "ln_ui_internal.h"

static lv_obj_t *s_err_title;
static lv_obj_t *s_err_detail;
static lv_obj_t *s_err_code;
static lv_obj_t *s_err_wifi;
static lv_obj_t *s_err_countdown;

static void retry_cb(lv_event_t *e)
{
    (void)e;
    ln_ui_post(LN_UI_RETRY_TAPPED, NULL, 0);
}

static void wifi_cb(lv_event_t *e)
{
    (void)e;
    ln_ui_post(LN_UI_WIFI_SETUP_REQUESTED, NULL, 0);
}

lv_obj_t *ln_scr_error_create(void)
{
    lv_obj_t *scr = ln_w_screen();

    lv_obj_t *col = ln_w_col(scr, 18);
    lv_obj_center(col);
    lv_obj_set_width(col, lv_pct(90)); /* 820px overflowed the 720px portrait panel */
    lv_obj_set_flex_align(col, LV_FLEX_ALIGN_CENTER, LV_FLEX_ALIGN_CENTER,
                          LV_FLEX_ALIGN_CENTER);

    ln_w_label(col, LV_SYMBOL_WARNING, LN_FONT_HUGE, LN_COL_ERROR);

    s_err_title = ln_w_label(col, "Can't reach Live Ninja cloud", LN_FONT_XXL,
                             LN_COL_ERROR);
    lv_obj_set_style_text_align(s_err_title, LV_TEXT_ALIGN_CENTER, 0);

    s_err_detail = ln_w_label(col,
        "Your device is still powered on and listening for the wake word — "
        "it reconnects the moment the service is back.",
        LN_FONT_MD, LN_COL_MUTED);
    lv_label_set_long_mode(s_err_detail, LV_LABEL_LONG_WRAP);
    lv_obj_set_width(s_err_detail, lv_pct(100));
    lv_obj_set_style_text_align(s_err_detail, LV_TEXT_ALIGN_CENTER, 0);

    s_err_code = ln_w_label(col, "", LN_FONT_SM, LN_COL_DIM);

    lv_obj_t *chip = ln_w_card(col);
    lv_obj_set_style_pad_all(chip, 14, 0);
    lv_obj_set_flex_flow(chip, LV_FLEX_FLOW_ROW);
    lv_obj_set_style_pad_column(chip, 12, 0);
    ln_w_label(chip, LV_SYMBOL_WIFI, LN_FONT_MD, LN_COL_SUCCESS);
    s_err_wifi = ln_w_label(chip, "Wi-Fi status unknown", LN_FONT_SM,
                            LN_COL_MUTED);

    lv_obj_t *btns = ln_w_row(col, 24);
    lv_obj_set_style_pad_top(btns, 10, 0);
    ln_w_button(btns, LV_SYMBOL_REFRESH "  Retry connection", LN_COL_TEAL,
                LN_COL_INK, retry_cb);
    ln_w_button(btns, LV_SYMBOL_WIFI "  Wi-Fi settings", LN_COL_SURFACE2,
                LN_COL_TEXT, wifi_cb);

    s_err_countdown = ln_w_label(col, "", LN_FONT_SM, LN_COL_DIM);

    return scr;
}

void ln_scr_error_set(const char *title, const char *detail, const char *code)
{
    if (s_err_title == NULL) {
        return;
    }
    if (title != NULL && title[0] != '\0') {
        lv_label_set_text(s_err_title, title);
    }
    if (detail != NULL && detail[0] != '\0') {
        lv_label_set_text(s_err_detail, detail);
    }
    if (code != NULL) {
        lv_label_set_text(s_err_code, code);
    }
}

void ln_scr_error_set_wifi(const char *text)
{
    if (s_err_wifi != NULL && text != NULL) {
        lv_label_set_text(s_err_wifi, text);
    }
}

void ln_scr_error_countdown(int secs)
{
    if (s_err_countdown == NULL) {
        return;
    }
    if (secs < 0) {
        lv_label_set_text(s_err_countdown, "");
    } else {
        lv_label_set_text_fmt(s_err_countdown, "Auto-retry in %d s", secs);
    }
}
