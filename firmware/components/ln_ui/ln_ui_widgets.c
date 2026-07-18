/*
 * ln_ui_widgets.c — shared themed widget builders + modal confirm dialog.
 * All builders must be called with the LVGL lock held.
 */
#include "ln_ui_internal.h"

/* ------------------------------------------------------------------ */

lv_obj_t *ln_w_screen(void)
{
    lv_obj_t *scr = lv_obj_create(NULL);
    lv_obj_set_style_bg_color(scr, LN_COL_BG, 0);
    lv_obj_set_style_bg_opa(scr, LV_OPA_COVER, 0);
    lv_obj_set_style_text_color(scr, LN_COL_TEXT, 0);
    return scr;
}

lv_obj_t *ln_w_plain(lv_obj_t *parent)
{
    lv_obj_t *o = lv_obj_create(parent);
    lv_obj_remove_style_all(o);
    lv_obj_set_size(o, LV_SIZE_CONTENT, LV_SIZE_CONTENT);
    return o;
}

lv_obj_t *ln_w_col(lv_obj_t *parent, int gap)
{
    lv_obj_t *o = ln_w_plain(parent);
    lv_obj_set_flex_flow(o, LV_FLEX_FLOW_COLUMN);
    lv_obj_set_style_pad_row(o, gap, 0);
    return o;
}

lv_obj_t *ln_w_row(lv_obj_t *parent, int gap)
{
    lv_obj_t *o = ln_w_plain(parent);
    lv_obj_set_flex_flow(o, LV_FLEX_FLOW_ROW);
    lv_obj_set_flex_align(o, LV_FLEX_ALIGN_START, LV_FLEX_ALIGN_CENTER,
                          LV_FLEX_ALIGN_CENTER);
    lv_obj_set_style_pad_column(o, gap, 0);
    return o;
}

lv_obj_t *ln_w_card(lv_obj_t *parent)
{
    lv_obj_t *o = lv_obj_create(parent);
    lv_obj_remove_style_all(o);
    lv_obj_set_style_bg_color(o, LN_COL_SURFACE, 0);
    lv_obj_set_style_bg_opa(o, LV_OPA_COVER, 0);
    lv_obj_set_style_border_color(o, LN_COL_BORDER, 0);
    lv_obj_set_style_border_width(o, 1, 0);
    lv_obj_set_style_radius(o, LN_RADIUS, 0);
    lv_obj_set_style_pad_all(o, 24, 0);
    return o;
}

lv_obj_t *ln_w_label(lv_obj_t *parent, const char *txt, const lv_font_t *font,
                     lv_color_t color)
{
    lv_obj_t *l = lv_label_create(parent);
    lv_label_set_text(l, txt);
    lv_obj_set_style_text_font(l, font, 0);
    lv_obj_set_style_text_color(l, color, 0);
    return l;
}

lv_obj_t *ln_w_button(lv_obj_t *parent, const char *txt, lv_color_t bg,
                      lv_color_t fg, lv_event_cb_t cb)
{
    lv_obj_t *btn = lv_button_create(parent);
    lv_obj_remove_style_all(btn);
    lv_obj_set_style_bg_color(btn, bg, 0);
    lv_obj_set_style_bg_opa(btn, LV_OPA_COVER, 0);
    lv_obj_set_style_radius(btn, LN_RADIUS, 0);
    lv_obj_set_style_pad_hor(btn, 28, 0);
    lv_obj_set_style_pad_ver(btn, 16, 0);
    lv_obj_set_style_min_height(btn, LN_TOUCH_MIN, 0);
    lv_obj_set_style_min_width(btn, LN_TOUCH_MIN, 0);
    lv_obj_set_style_bg_opa(btn, LV_OPA_80, LV_STATE_PRESSED);
    lv_obj_set_style_border_color(btn, LN_COL_TEAL, LV_STATE_FOCUS_KEY);
    lv_obj_set_style_border_width(btn, 2, LV_STATE_FOCUS_KEY);
    if (cb != NULL) {
        lv_obj_add_event_cb(btn, cb, LV_EVENT_CLICKED, NULL);
    }

    lv_obj_t *l = ln_w_label(btn, txt, LN_FONT_MD, fg);
    lv_obj_center(l);
    return btn;
}

lv_obj_t *ln_w_kv_row(lv_obj_t *parent, const char *key, const char *val,
                      lv_obj_t **val_out)
{
    lv_obj_t *row = ln_w_plain(parent);
    lv_obj_set_size(row, lv_pct(100), LV_SIZE_CONTENT);
    lv_obj_set_flex_flow(row, LV_FLEX_FLOW_ROW);
    lv_obj_set_flex_align(row, LV_FLEX_ALIGN_SPACE_BETWEEN,
                          LV_FLEX_ALIGN_CENTER, LV_FLEX_ALIGN_CENTER);
    lv_obj_set_style_pad_ver(row, 10, 0);
    lv_obj_set_style_border_color(row, LN_COL_BORDER, 0);
    lv_obj_set_style_border_width(row, 1, 0);
    lv_obj_set_style_border_side(row, LV_BORDER_SIDE_BOTTOM, 0);

    ln_w_label(row, key, LN_FONT_SM, LN_COL_DIM);
    lv_obj_t *v = ln_w_label(row, val, LN_FONT_MD, LN_COL_TEXT);
    lv_label_set_long_mode(v, LV_LABEL_LONG_DOT);
    /* Config detail cards are ~350px inside on the 720px portrait panel;
     * 420 (landscape-era) let long values overflow the card. */
    lv_obj_set_style_max_width(v, 220, 0);
    if (val_out != NULL) {
        *val_out = v;
    }
    return row;
}

/* ------------------------------------------------------------------ */
/* Modal confirm dialog                                                */
/* ------------------------------------------------------------------ */

static void (*s_confirm_cb)(void);
static lv_obj_t *s_confirm_overlay;

static void confirm_close(void)
{
    if (s_confirm_overlay != NULL) {
        lv_obj_delete(s_confirm_overlay);
        s_confirm_overlay = NULL;
    }
    s_confirm_cb = NULL;
}

static void confirm_cancel_cb(lv_event_t *e)
{
    (void)e;
    confirm_close();
}

static void confirm_ok_cb(lv_event_t *e)
{
    (void)e;
    void (*cb)(void) = s_confirm_cb;
    confirm_close();
    if (cb != NULL) {
        cb();
    }
}

void ln_w_confirm(const char *title, const char *body, const char *confirm_txt,
                  lv_color_t confirm_bg, void (*on_confirm)(void))
{
    confirm_close(); /* only one at a time */
    s_confirm_cb = on_confirm;

    s_confirm_overlay = lv_obj_create(lv_layer_top());
    lv_obj_remove_style_all(s_confirm_overlay);
    lv_obj_set_size(s_confirm_overlay, lv_pct(100), lv_pct(100));
    lv_obj_set_style_bg_color(s_confirm_overlay, lv_color_hex(0x000000), 0);
    lv_obj_set_style_bg_opa(s_confirm_overlay, LV_OPA_60, 0);
    lv_obj_add_flag(s_confirm_overlay, LV_OBJ_FLAG_CLICKABLE); /* eat taps */

    lv_obj_t *card = ln_w_card(s_confirm_overlay);
    lv_obj_set_width(card, 560);
    lv_obj_center(card);
    lv_obj_set_flex_flow(card, LV_FLEX_FLOW_COLUMN);
    lv_obj_set_style_pad_row(card, 18, 0);

    ln_w_label(card, title, LN_FONT_XL, LN_COL_TEXT);
    lv_obj_t *b = ln_w_label(card, body, LN_FONT_MD, LN_COL_MUTED);
    lv_label_set_long_mode(b, LV_LABEL_LONG_WRAP);
    lv_obj_set_width(b, lv_pct(100));

    lv_obj_t *btns = ln_w_row(card, 20);
    lv_obj_set_width(btns, lv_pct(100));
    lv_obj_set_flex_align(btns, LV_FLEX_ALIGN_END, LV_FLEX_ALIGN_CENTER,
                          LV_FLEX_ALIGN_CENTER);
    ln_w_button(btns, "Cancel", LN_COL_SURFACE2, LN_COL_TEXT, confirm_cancel_cb);
    ln_w_button(btns, confirm_txt, confirm_bg, LN_COL_INK, confirm_ok_cb);
}
