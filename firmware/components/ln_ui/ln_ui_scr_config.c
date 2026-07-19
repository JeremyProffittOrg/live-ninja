/*
 * ln_ui_scr_config.c — Settings screen (mockups 07/08).
 *
 * Left menu (64px rows) + right detail panel. Sections per plan.md M5:
 *   Voice (list-select, settings.schema.json enum, never free text)
 *   Volume (slider + steppers)
 *   Wake sensitivity (bounded slider, FR-K06)
 *   Device name (textarea + on-screen keyboard — the one last-resort keyboard)
 *   Wi-Fi (info + re-run portal, confirmed)
 *   About (identity, firmware, uptime + guarded factory reset)
 *
 * Every edit posts an LN_UI_EVENT; the owning component persists and calls
 * ln_ui_config_values() to reflect the applied state.
 */
#include <stdio.h>
#include <string.h>

#include "esp_heap_caps.h"
#include "esp_timer.h"

#include "ln_ui_internal.h"

/* settings.schema.json #/properties/voice enum — keep in sync (additive). */
static const struct {
    const char *id;
    const char *desc;
} k_voices[] = {
    { "alloy",   "Neutral · balanced"          },
    { "ash",     "Warm · gravelly"             },
    { "ballad",  "Calm · storyteller"          },
    { "cedar",   "Natural · grounded (default)"},
    { "coral",   "Bright · friendly"           },
    { "echo",    "Crisp · precise"             },
    { "marin",   "Smooth · confident"          },
    { "sage",    "Soft · thoughtful"           },
    { "shimmer", "Light · upbeat"              },
    { "verse",   "Expressive · dynamic"        },
};
#define VOICE_COUNT ((int)(sizeof(k_voices) / sizeof(k_voices[0])))

/* WakeNet models packed into the "model" flash partition (sdkconfig
 * CONFIG_SR_WN_*) — selection only, never free text. Keep in sync with the
 * packed set; a name that's missing from the partition makes
 * ln_wake_set_model() return ESP_ERR_NOT_FOUND and the previous model stays. */
static const struct {
    const char *model;
    const char *phrase;
    const char *desc;
} k_wake_models[] = {
    { "wn9_hilili_tts", "\"Hi, Lily\"", "Distinct and easy to say (default)" },
    { "wn9_hiesp",      "\"Hi, ESP\"",  "Espressif classic — very robust"    },
    { "wn9_alexa",      "\"Alexa\"",    "May also wake nearby Echo devices"  },
};
#define WAKE_MODEL_COUNT ((int)(sizeof(k_wake_models) / sizeof(k_wake_models[0])))

typedef enum {
    SEC_WAKEWORD = 0,
    SEC_VOICE,
    SEC_VOLUME,
    SEC_SENSITIVITY,
    SEC_NAME,
    SEC_WIFI,
    SEC_ABOUT,
    SEC_COUNT,
} section_t;

static const char *k_section_names[SEC_COUNT] = {
    "Wake word", "Voice", "Volume", "Wake sensitivity", "Device name", "Wi-Fi",
    "About",
};

static lv_obj_t *s_panels[SEC_COUNT];
static lv_obj_t *s_menu_btns[SEC_COUNT];
static section_t s_active_section = SEC_VOICE;

/* voice */
static lv_obj_t *s_voice_rows[VOICE_COUNT];
static lv_obj_t *s_voice_checks[VOICE_COUNT];
static lv_obj_t *s_voice_pos_label;
static int s_voice_active = 3; /* cedar */

/* wake word */
static lv_obj_t *s_wake_rows[WAKE_MODEL_COUNT];
static lv_obj_t *s_wake_checks[WAKE_MODEL_COUNT];
static int s_wake_active = 0; /* Hi Lily (default) */

/* volume + sensitivity */
static lv_obj_t *s_vol_slider;
static lv_obj_t *s_vol_value;
static lv_obj_t *s_bri_slider;
static lv_obj_t *s_bri_value;
static lv_obj_t *s_sens_slider;
static lv_obj_t *s_sens_value;

/* name */
static lv_obj_t *s_name_ta;
static lv_obj_t *s_name_kb;

/* wifi + about value labels */
static lv_obj_t *s_wifi_ssid_v;
static lv_obj_t *s_wifi_ip_v;
static lv_obj_t *s_wifi_sig_v;
static lv_obj_t *s_about_fw_v;
static lv_obj_t *s_about_thing_v;
static lv_obj_t *s_about_mac_v;
static lv_obj_t *s_about_ip_v;
static lv_obj_t *s_about_uptime_v;
static lv_obj_t *s_about_heap_v;

/* ------------------------------------------------------------------ */

static void refresh_about_dynamic(void)
{
    if (s_about_uptime_v == NULL) {
        return;
    }
    int64_t up_s = esp_timer_get_time() / 1000000;
    lv_label_set_text_fmt(s_about_uptime_v, "%dd %dh %dm",
                          (int)(up_s / 86400), (int)((up_s / 3600) % 24),
                          (int)((up_s / 60) % 60));
    lv_label_set_text_fmt(s_about_heap_v, "%u KB free",
                          (unsigned)(heap_caps_get_free_size(MALLOC_CAP_DEFAULT) / 1024));
}

static void show_section(section_t sec)
{
    s_active_section = sec;
    for (int i = 0; i < SEC_COUNT; i++) {
        if (i == (int)sec) {
            lv_obj_remove_flag(s_panels[i], LV_OBJ_FLAG_HIDDEN);
            lv_obj_set_style_bg_color(s_menu_btns[i], LN_COL_TEAL_DARK, 0);
        } else {
            lv_obj_add_flag(s_panels[i], LV_OBJ_FLAG_HIDDEN);
            lv_obj_set_style_bg_color(s_menu_btns[i], LN_COL_SURFACE, 0);
        }
    }
    if (sec == SEC_ABOUT) {
        refresh_about_dynamic();
    }
    if (sec != SEC_NAME && s_name_kb != NULL) {
        lv_obj_add_flag(s_name_kb, LV_OBJ_FLAG_HIDDEN);
    }
}

static void menu_btn_cb(lv_event_t *e)
{
    show_section((section_t)(intptr_t)lv_event_get_user_data(e));
}

static void back_btn_cb(lv_event_t *e)
{
    (void)e;
    ln_ui_post(LN_UI_SETTINGS_CLOSED, NULL, 0);
}

/* ---- wake word ---- */

static void wake_mark_active(int idx)
{
    if (idx < 0 || idx >= WAKE_MODEL_COUNT) {
        return;
    }
    for (int i = 0; i < WAKE_MODEL_COUNT; i++) {
        if (i == idx) {
            lv_obj_remove_flag(s_wake_checks[i], LV_OBJ_FLAG_HIDDEN);
            lv_obj_set_style_border_color(s_wake_rows[i], LN_COL_TEAL, 0);
        } else {
            lv_obj_add_flag(s_wake_checks[i], LV_OBJ_FLAG_HIDDEN);
            lv_obj_set_style_border_color(s_wake_rows[i], LN_COL_BORDER, 0);
        }
    }
    s_wake_active = idx;
}

static void wake_row_cb(lv_event_t *e)
{
    int idx = (int)(intptr_t)lv_event_get_user_data(e);
    wake_mark_active(idx);
    ln_evt_wake_model_t sel = { 0 };
    strlcpy(sel.model, k_wake_models[idx].model, sizeof(sel.model));
    ln_ui_post(LN_UI_WAKE_MODEL_SELECTED, &sel, sizeof(sel));
}

static lv_obj_t *build_wakeword_panel(lv_obj_t *parent)
{
    lv_obj_t *panel = ln_w_col(parent, 12);
    lv_obj_set_size(panel, lv_pct(100), lv_pct(100));

    ln_w_label(panel, "Wake word", LN_FONT_XL, LN_COL_TEXT);
    lv_obj_t *desc = ln_w_label(panel,
        "What this device listens for. Applies immediately — say the new "
        "phrase to start a conversation.", LN_FONT_SM, LN_COL_MUTED);
    lv_label_set_long_mode(desc, LV_LABEL_LONG_WRAP);
    lv_obj_set_width(desc, lv_pct(100));

    lv_obj_t *list = ln_w_col(panel, 10);
    lv_obj_set_width(list, lv_pct(100));
    lv_obj_set_flex_grow(list, 1);
    lv_obj_set_scroll_dir(list, LV_DIR_VER);
    lv_obj_add_flag(list, LV_OBJ_FLAG_SCROLLABLE);

    for (int i = 0; i < WAKE_MODEL_COUNT; i++) {
        lv_obj_t *row = lv_button_create(list);
        lv_obj_remove_style_all(row);
        lv_obj_set_size(row, lv_pct(100), 64);
        lv_obj_set_style_bg_color(row, LN_COL_SURFACE, 0);
        lv_obj_set_style_bg_opa(row, LV_OPA_COVER, 0);
        lv_obj_set_style_bg_color(row, LN_COL_SURFACE2, LV_STATE_PRESSED);
        lv_obj_set_style_radius(row, LN_RADIUS, 0);
        lv_obj_set_style_border_color(row, LN_COL_BORDER, 0);
        lv_obj_set_style_border_width(row, 1, 0);
        lv_obj_set_style_pad_hor(row, 20, 0);
        lv_obj_add_event_cb(row, wake_row_cb, LV_EVENT_CLICKED,
                            (void *)(intptr_t)i);
        s_wake_rows[i] = row;

        lv_obj_t *name = ln_w_label(row, k_wake_models[i].phrase, LN_FONT_MD,
                                    LN_COL_TEXT);
        lv_obj_align(name, LV_ALIGN_LEFT_MID, 0, -12);

        lv_obj_t *d = ln_w_label(row, k_wake_models[i].desc, LN_FONT_XS,
                                 LN_COL_DIM);
        lv_obj_align(d, LV_ALIGN_LEFT_MID, 0, 14);

        lv_obj_t *chk = ln_w_label(row, LV_SYMBOL_OK, LN_FONT_LG, LN_COL_TEAL);
        lv_obj_align(chk, LV_ALIGN_RIGHT_MID, 0, 0);
        lv_obj_add_flag(chk, LV_OBJ_FLAG_HIDDEN);
        s_wake_checks[i] = chk;
    }
    wake_mark_active(s_wake_active);
    return panel;
}

/* ---- voice ---- */

static void voice_mark_active(int idx)
{
    if (idx < 0 || idx >= VOICE_COUNT) {
        return;
    }
    for (int i = 0; i < VOICE_COUNT; i++) {
        if (i == idx) {
            lv_obj_remove_flag(s_voice_checks[i], LV_OBJ_FLAG_HIDDEN);
            lv_obj_set_style_border_color(s_voice_rows[i], LN_COL_TEAL, 0);
        } else {
            lv_obj_add_flag(s_voice_checks[i], LV_OBJ_FLAG_HIDDEN);
            lv_obj_set_style_border_color(s_voice_rows[i], LN_COL_BORDER, 0);
        }
    }
    s_voice_active = idx;
    lv_label_set_text_fmt(s_voice_pos_label, "Voice %d of %d", idx + 1,
                          VOICE_COUNT);
}

static void voice_row_cb(lv_event_t *e)
{
    int idx = (int)(intptr_t)lv_event_get_user_data(e);
    voice_mark_active(idx);
    ln_evt_voice_sel_t sel = { 0 };
    strlcpy(sel.voice, k_voices[idx].id, sizeof(sel.voice));
    ln_ui_post(LN_UI_VOICE_SELECTED, &sel, sizeof(sel));
}

static lv_obj_t *build_voice_panel(lv_obj_t *parent)
{
    lv_obj_t *panel = ln_w_col(parent, 12);
    lv_obj_set_size(panel, lv_pct(100), lv_pct(100));

    ln_w_label(panel, "Assistant voice", LN_FONT_XL, LN_COL_TEXT);
    s_voice_pos_label = ln_w_label(panel, "", LN_FONT_SM, LN_COL_DIM);

    lv_obj_t *list = ln_w_col(panel, 10);
    lv_obj_set_width(list, lv_pct(100));
    lv_obj_set_flex_grow(list, 1);
    lv_obj_set_scroll_dir(list, LV_DIR_VER);
    lv_obj_add_flag(list, LV_OBJ_FLAG_SCROLLABLE);

    for (int i = 0; i < VOICE_COUNT; i++) {
        lv_obj_t *row = lv_button_create(list);
        lv_obj_remove_style_all(row);
        lv_obj_set_size(row, lv_pct(100), 64);
        lv_obj_set_style_bg_color(row, LN_COL_SURFACE, 0);
        lv_obj_set_style_bg_opa(row, LV_OPA_COVER, 0);
        lv_obj_set_style_bg_color(row, LN_COL_SURFACE2, LV_STATE_PRESSED);
        lv_obj_set_style_radius(row, LN_RADIUS, 0);
        lv_obj_set_style_border_color(row, LN_COL_BORDER, 0);
        lv_obj_set_style_border_width(row, 1, 0);
        lv_obj_set_style_pad_hor(row, 20, 0);
        lv_obj_add_event_cb(row, voice_row_cb, LV_EVENT_CLICKED,
                            (void *)(intptr_t)i);
        s_voice_rows[i] = row;

        char cap[8];
        snprintf(cap, sizeof(cap), "%c%s", k_voices[i].id[0] - 'a' + 'A',
                 k_voices[i].id + 1);
        lv_obj_t *name = ln_w_label(row, cap, LN_FONT_MD, LN_COL_TEXT);
        lv_obj_align(name, LV_ALIGN_LEFT_MID, 0, -12);

        lv_obj_t *desc = ln_w_label(row, k_voices[i].desc, LN_FONT_XS,
                                    LN_COL_DIM);
        lv_obj_align(desc, LV_ALIGN_LEFT_MID, 0, 14);

        lv_obj_t *chk = ln_w_label(row, LV_SYMBOL_OK, LN_FONT_LG, LN_COL_TEAL);
        lv_obj_align(chk, LV_ALIGN_RIGHT_MID, 0, 0);
        lv_obj_add_flag(chk, LV_OBJ_FLAG_HIDDEN);
        s_voice_checks[i] = chk;
    }
    voice_mark_active(s_voice_active);
    return panel;
}

/* ---- volume / brightness / sensitivity ---- */

static lv_obj_t *make_slider(lv_obj_t *parent, int32_t min, int32_t max,
                             lv_event_cb_t cb)
{
    lv_obj_t *sl = lv_slider_create(parent);
    lv_slider_set_range(sl, min, max);
    lv_obj_set_size(sl, lv_pct(100), 20);
    lv_obj_set_style_margin_left(sl, 16, 0);
    lv_obj_set_style_margin_right(sl, 16, 0);
    lv_obj_set_style_margin_top(sl, 22, 0);    /* fat touch band */
    lv_obj_set_style_margin_bottom(sl, 22, 0);
    lv_obj_set_style_bg_color(sl, LN_COL_SURFACE2, LV_PART_MAIN);
    lv_obj_set_style_bg_opa(sl, LV_OPA_COVER, LV_PART_MAIN);
    lv_obj_set_style_bg_color(sl, LN_COL_TEAL, LV_PART_INDICATOR);
    lv_obj_set_style_bg_color(sl, LN_COL_TEXT, LV_PART_KNOB);
    lv_obj_set_style_pad_all(sl, 14, LV_PART_KNOB); /* ~48px knob */
    lv_obj_add_event_cb(sl, cb, LV_EVENT_VALUE_CHANGED, NULL);
    lv_obj_add_event_cb(sl, cb, LV_EVENT_RELEASED, NULL);
    return sl;
}

static void vol_changed_cb(lv_event_t *e)
{
    int32_t v = lv_slider_get_value(s_vol_slider);
    lv_label_set_text_fmt(s_vol_value, "%d%%", (int)v);
    if (lv_event_get_code(e) == LV_EVENT_RELEASED) {
        ln_evt_pct_t p = { .pct = (uint8_t)v };
        ln_ui_post(LN_UI_VOLUME_CHANGED, &p, sizeof(p));
    }
}

static void vol_step_cb(lv_event_t *e)
{
    int step = (int)(intptr_t)lv_event_get_user_data(e);
    int32_t v = lv_slider_get_value(s_vol_slider) + step;
    if (v < 0) {
        v = 0;
    }
    if (v > 100) {
        v = 100;
    }
    lv_slider_set_value(s_vol_slider, v, LV_ANIM_OFF);
    lv_label_set_text_fmt(s_vol_value, "%d%%", (int)v);
    ln_evt_pct_t p = { .pct = (uint8_t)v };
    ln_ui_post(LN_UI_VOLUME_CHANGED, &p, sizeof(p));
}

static void bri_changed_cb(lv_event_t *e)
{
    int32_t v = lv_slider_get_value(s_bri_slider);
    lv_label_set_text_fmt(s_bri_value, "%d%%", (int)v);
    if (lv_event_get_code(e) == LV_EVENT_RELEASED) {
        ln_evt_pct_t p = { .pct = (uint8_t)v };
        ln_ui_post(LN_UI_BRIGHTNESS_CHANGED, &p, sizeof(p));
    }
}

static lv_obj_t *build_volume_panel(lv_obj_t *parent)
{
    lv_obj_t *panel = ln_w_col(parent, 16);
    lv_obj_set_size(panel, lv_pct(100), lv_pct(100));

    ln_w_label(panel, "Speaker volume", LN_FONT_XL, LN_COL_TEXT);

    lv_obj_t *card = ln_w_card(panel);
    lv_obj_set_width(card, lv_pct(100));
    lv_obj_set_flex_flow(card, LV_FLEX_FLOW_COLUMN);
    lv_obj_set_style_pad_row(card, 8, 0);

    s_vol_value = ln_w_label(card, "70%", LN_FONT_HUGE, LN_COL_TEAL);

    lv_obj_t *row = ln_w_row(card, 18);
    lv_obj_set_width(row, lv_pct(100));
    lv_obj_t *minus = ln_w_button(row, LV_SYMBOL_MINUS, LN_COL_SURFACE2,
                                  LN_COL_TEXT, NULL);
    lv_obj_set_size(minus, 64, 64);
    lv_obj_add_event_cb(minus, vol_step_cb, LV_EVENT_CLICKED,
                        (void *)(intptr_t)-5);

    s_vol_slider = make_slider(row, 0, 100, vol_changed_cb);
    lv_obj_set_flex_grow(s_vol_slider, 1);
    lv_slider_set_value(s_vol_slider, 70, LV_ANIM_OFF);

    lv_obj_t *plus = ln_w_button(row, LV_SYMBOL_PLUS, LN_COL_SURFACE2,
                                 LN_COL_TEXT, NULL);
    lv_obj_set_size(plus, 64, 64);
    lv_obj_add_event_cb(plus, vol_step_cb, LV_EVENT_CLICKED,
                        (void *)(intptr_t)5);

    /* brightness */
    ln_w_label(panel, "Screen brightness", LN_FONT_XL, LN_COL_TEXT);
    lv_obj_t *bcard = ln_w_card(panel);
    lv_obj_set_width(bcard, lv_pct(100));
    lv_obj_set_flex_flow(bcard, LV_FLEX_FLOW_COLUMN);
    lv_obj_set_style_pad_row(bcard, 8, 0);

    s_bri_value = ln_w_label(bcard, "80%", LN_FONT_HUGE, LN_COL_WARN);
    s_bri_slider = make_slider(bcard, 10, 100, bri_changed_cb);
    lv_slider_set_value(s_bri_slider, 80, LV_ANIM_OFF);

    return panel;
}

static void sens_changed_cb(lv_event_t *e)
{
    int32_t v = lv_slider_get_value(s_sens_slider);
    lv_label_set_text_fmt(s_sens_value, "%d%%", (int)v);
    if (lv_event_get_code(e) == LV_EVENT_RELEASED) {
        ln_evt_float_t f = { .value = (float)v / 100.0f };
        ln_ui_post(LN_UI_SENSITIVITY_CHANGED, &f, sizeof(f));
    }
}

static lv_obj_t *build_sensitivity_panel(lv_obj_t *parent)
{
    lv_obj_t *panel = ln_w_col(parent, 16);
    lv_obj_set_size(panel, lv_pct(100), lv_pct(100));

    ln_w_label(panel, "Wake-word sensitivity", LN_FONT_XL, LN_COL_TEXT);
    lv_obj_t *desc = ln_w_label(panel,
        "Higher sensitivity wakes more easily but may trigger by mistake. "
        "Lower sensitivity needs a clearer, closer voice.",
        LN_FONT_SM, LN_COL_MUTED);
    lv_label_set_long_mode(desc, LV_LABEL_LONG_WRAP);
    lv_obj_set_width(desc, lv_pct(100));

    lv_obj_t *card = ln_w_card(panel);
    lv_obj_set_width(card, lv_pct(100));
    lv_obj_set_flex_flow(card, LV_FLEX_FLOW_COLUMN);
    lv_obj_set_style_pad_row(card, 8, 0);

    s_sens_value = ln_w_label(card, "50%", LN_FONT_HUGE, LN_COL_TEAL);
    s_sens_slider = make_slider(card, 0, 100, sens_changed_cb);
    lv_slider_set_value(s_sens_slider, 50, LV_ANIM_OFF);

    lv_obj_t *ticks = ln_w_plain(card);
    lv_obj_set_size(ticks, lv_pct(100), LV_SIZE_CONTENT);
    lv_obj_set_flex_flow(ticks, LV_FLEX_FLOW_ROW);
    lv_obj_set_flex_align(ticks, LV_FLEX_ALIGN_SPACE_BETWEEN,
                          LV_FLEX_ALIGN_CENTER, LV_FLEX_ALIGN_CENTER);
    ln_w_label(ticks, "Low", LN_FONT_SM, LN_COL_DIM);
    ln_w_label(ticks, "Balanced", LN_FONT_SM, LN_COL_DIM);
    ln_w_label(ticks, "High", LN_FONT_SM, LN_COL_DIM);

    return panel;
}

/* ---- device name (keyboard = the sanctioned last resort) ---- */

static void name_save_cb(lv_event_t *e)
{
    (void)e;
    const char *txt = lv_textarea_get_text(s_name_ta);
    if (txt == NULL || txt[0] == '\0') {
        return;
    }
    ln_evt_name_t n = { 0 };
    strlcpy(n.name, txt, sizeof(n.name));
    ln_ui_post(LN_UI_DEVICE_NAME_CHANGED, &n, sizeof(n));
    lv_obj_add_flag(s_name_kb, LV_OBJ_FLAG_HIDDEN);
}

static void name_ta_cb(lv_event_t *e)
{
    lv_event_code_t code = lv_event_get_code(e);
    if (code == LV_EVENT_FOCUSED || code == LV_EVENT_CLICKED) {
        lv_obj_remove_flag(s_name_kb, LV_OBJ_FLAG_HIDDEN);
    } else if (code == LV_EVENT_DEFOCUSED) {
        lv_obj_add_flag(s_name_kb, LV_OBJ_FLAG_HIDDEN);
    }
}

static void name_kb_cb(lv_event_t *e)
{
    lv_event_code_t code = lv_event_get_code(e);
    if (code == LV_EVENT_READY) { /* keyboard OK key */
        name_save_cb(e);
    } else if (code == LV_EVENT_CANCEL) {
        lv_obj_add_flag(s_name_kb, LV_OBJ_FLAG_HIDDEN);
    }
}

static lv_obj_t *build_name_panel(lv_obj_t *parent)
{
    lv_obj_t *panel = ln_w_col(parent, 16);
    lv_obj_set_size(panel, lv_pct(100), lv_pct(100));

    ln_w_label(panel, "Device name", LN_FONT_XL, LN_COL_TEXT);
    lv_obj_t *desc = ln_w_label(panel,
        "Shown in the Live Ninja app and Alexa-style device lists.",
        LN_FONT_SM, LN_COL_MUTED);
    lv_label_set_long_mode(desc, LV_LABEL_LONG_WRAP);
    lv_obj_set_width(desc, lv_pct(100));

    s_name_ta = lv_textarea_create(panel);
    lv_textarea_set_one_line(s_name_ta, true);
    lv_textarea_set_max_length(s_name_ta, 32);
    lv_textarea_set_placeholder_text(s_name_ta, "e.g. Kitchen Ninja");
    lv_obj_set_width(s_name_ta, lv_pct(100));
    lv_obj_set_style_bg_color(s_name_ta, LN_COL_SURFACE, 0);
    lv_obj_set_style_text_color(s_name_ta, LN_COL_TEXT, 0);
    lv_obj_set_style_text_font(s_name_ta, LN_FONT_LG, 0);
    lv_obj_set_style_border_color(s_name_ta, LN_COL_BORDER, 0);
    lv_obj_set_style_radius(s_name_ta, LN_RADIUS, 0);
    lv_obj_set_style_min_height(s_name_ta, LN_TOUCH_MIN, 0);
    lv_obj_add_event_cb(s_name_ta, name_ta_cb, LV_EVENT_ALL, NULL);

    lv_obj_t *save_row = ln_w_row(panel, 0);
    lv_obj_set_width(save_row, lv_pct(100));
    lv_obj_set_flex_align(save_row, LV_FLEX_ALIGN_END, LV_FLEX_ALIGN_CENTER,
                          LV_FLEX_ALIGN_CENTER);
    ln_w_button(save_row, LV_SYMBOL_SAVE "  Save name", LN_COL_TEAL,
                LN_COL_INK, name_save_cb);

    s_name_kb = lv_keyboard_create(panel);
    lv_keyboard_set_textarea(s_name_kb, s_name_ta);
    lv_obj_set_size(s_name_kb, lv_pct(100), 280);
    lv_obj_set_style_bg_color(s_name_kb, LN_COL_SURFACE, 0);
    lv_obj_set_style_text_font(s_name_kb, LN_FONT_MD, 0);
    lv_obj_add_flag(s_name_kb, LV_OBJ_FLAG_HIDDEN);
    lv_obj_add_event_cb(s_name_kb, name_kb_cb, LV_EVENT_ALL, NULL);

    return panel;
}

/* ---- wifi ---- */

static void wifi_confirmed(void)
{
    ln_ui_post(LN_UI_WIFI_SETUP_REQUESTED, NULL, 0);
}

static void wifi_setup_cb(lv_event_t *e)
{
    (void)e;
    ln_w_confirm("Re-run Wi-Fi setup?",
                 "The device will start its setup hotspot and drop off the "
                 "current network until you finish the portal steps.",
                 "Start setup", LN_COL_WARN, wifi_confirmed);
}

static lv_obj_t *build_wifi_panel(lv_obj_t *parent)
{
    lv_obj_t *panel = ln_w_col(parent, 16);
    lv_obj_set_size(panel, lv_pct(100), lv_pct(100));

    ln_w_label(panel, "Wi-Fi", LN_FONT_XL, LN_COL_TEXT);

    lv_obj_t *card = ln_w_card(panel);
    lv_obj_set_width(card, lv_pct(100));
    lv_obj_set_flex_flow(card, LV_FLEX_FLOW_COLUMN);
    ln_w_kv_row(card, "Network", "Not connected", &s_wifi_ssid_v);
    ln_w_kv_row(card, "IP address", "—", &s_wifi_ip_v);
    ln_w_kv_row(card, "Signal", "—", &s_wifi_sig_v);

    ln_w_button(panel, LV_SYMBOL_WIFI "  Re-run Wi-Fi setup", LN_COL_SURFACE2,
                LN_COL_TEXT, wifi_setup_cb);
    return panel;
}

/* ---- about ---- */

static void reset_confirmed(void)
{
    ln_ui_post(LN_UI_FACTORY_RESET_REQUESTED, NULL, 0);
}

static void reset_cb(lv_event_t *e)
{
    (void)e;
    ln_w_confirm("Erase this device?",
                 "This removes the account login, Wi-Fi credentials and all "
                 "cached data, then reboots into first-time setup. It cannot "
                 "be undone.",
                 "Erase everything", LN_COL_ERROR, reset_confirmed);
}

static lv_obj_t *build_about_panel(lv_obj_t *parent)
{
    lv_obj_t *panel = ln_w_col(parent, 16);
    lv_obj_set_size(panel, lv_pct(100), lv_pct(100));
    lv_obj_set_scroll_dir(panel, LV_DIR_VER);
    lv_obj_add_flag(panel, LV_OBJ_FLAG_SCROLLABLE);

    ln_w_label(panel, "About this device", LN_FONT_XL, LN_COL_TEXT);

    lv_obj_t *card = ln_w_card(panel);
    lv_obj_set_width(card, lv_pct(100));
    lv_obj_set_flex_flow(card, LV_FLEX_FLOW_COLUMN);
    ln_w_kv_row(card, "Firmware", "—", &s_about_fw_v);
    ln_w_kv_row(card, "Thing name", "Not provisioned", &s_about_thing_v);
    ln_w_kv_row(card, "Device MAC", "—", &s_about_mac_v);
    ln_w_kv_row(card, "IP address", "—", &s_about_ip_v);
    ln_w_kv_row(card, "Uptime", "—", &s_about_uptime_v);
    ln_w_kv_row(card, "Heap", "—", &s_about_heap_v);

    lv_obj_t *danger = ln_w_card(panel);
    lv_obj_set_width(danger, lv_pct(100));
    lv_obj_set_flex_flow(danger, LV_FLEX_FLOW_COLUMN);
    lv_obj_set_style_pad_row(danger, 12, 0);
    lv_obj_set_style_border_color(danger, LN_COL_ERROR, 0);
    ln_w_label(danger, LV_SYMBOL_WARNING "  Danger zone", LN_FONT_MD,
               LN_COL_ERROR);
    lv_obj_t *txt = ln_w_label(danger,
        "Factory reset erases the account login, Wi-Fi credentials and all "
        "cached data.", LN_FONT_SM, LN_COL_MUTED);
    lv_label_set_long_mode(txt, LV_LABEL_LONG_WRAP);
    lv_obj_set_width(txt, lv_pct(100));
    ln_w_button(danger, LV_SYMBOL_TRASH "  Factory reset device",
                LN_COL_ERROR, LN_COL_TEXT, reset_cb);

    return panel;
}

/* ------------------------------------------------------------------ */

lv_obj_t *ln_scr_config_create(void)
{
    lv_obj_t *scr = ln_w_screen();

    /* header */
    lv_obj_t *hdr = ln_w_plain(scr);
    lv_obj_set_size(hdr, lv_pct(100), 72);
    lv_obj_align(hdr, LV_ALIGN_TOP_MID, 0, 0);
    lv_obj_set_style_bg_color(hdr, LN_COL_SURFACE, 0);
    lv_obj_set_style_bg_opa(hdr, LV_OPA_COVER, 0);
    lv_obj_set_style_border_color(hdr, LN_COL_BORDER, 0);
    lv_obj_set_style_border_width(hdr, 1, 0);
    lv_obj_set_style_border_side(hdr, LV_BORDER_SIDE_BOTTOM, 0);
    lv_obj_set_style_pad_hor(hdr, 20, 0);

    lv_obj_t *back = ln_w_button(hdr, LV_SYMBOL_LEFT "  Back", LN_COL_SURFACE2,
                                 LN_COL_TEXT, back_btn_cb);
    lv_obj_align(back, LV_ALIGN_LEFT_MID, 0, 0);

    lv_obj_t *title = ln_w_label(hdr, "Settings", LN_FONT_XL, LN_COL_TEXT);
    lv_obj_align(title, LV_ALIGN_CENTER, 0, 0);

    /* body: left menu + right panel host. Full portrait height (1280)
     * minus the 72px header — 720-72 was a landscape leftover that left
     * the bottom 560px of the screen empty. */
    lv_obj_t *body = ln_w_plain(scr);
    lv_obj_set_size(body, lv_pct(100), 1280 - 72);
    lv_obj_align(body, LV_ALIGN_BOTTOM_MID, 0, 0);
    lv_obj_set_flex_flow(body, LV_FLEX_FLOW_ROW);

    /* 260px menu leaves the detail panel ~460px on the 720px-wide panel. */
    lv_obj_t *menu = ln_w_col(body, 10);
    lv_obj_set_size(menu, 260, lv_pct(100));
    lv_obj_set_style_pad_all(menu, 20, 0);
    lv_obj_set_style_bg_color(menu, LN_COL_BG, 0);
    lv_obj_set_style_border_color(menu, LN_COL_BORDER, 0);
    lv_obj_set_style_border_width(menu, 1, 0);
    lv_obj_set_style_border_side(menu, LV_BORDER_SIDE_RIGHT, 0);
    lv_obj_set_scroll_dir(menu, LV_DIR_VER);
    lv_obj_add_flag(menu, LV_OBJ_FLAG_SCROLLABLE);

    for (int i = 0; i < SEC_COUNT; i++) {
        lv_obj_t *btn = lv_button_create(menu);
        lv_obj_remove_style_all(btn);
        lv_obj_set_size(btn, lv_pct(100), 64);
        lv_obj_set_style_bg_color(btn, LN_COL_SURFACE, 0);
        lv_obj_set_style_bg_opa(btn, LV_OPA_COVER, 0);
        lv_obj_set_style_bg_color(btn, LN_COL_SURFACE2, LV_STATE_PRESSED);
        lv_obj_set_style_radius(btn, LN_RADIUS, 0);
        lv_obj_set_style_pad_hor(btn, 20, 0);
        lv_obj_add_event_cb(btn, menu_btn_cb, LV_EVENT_CLICKED,
                            (void *)(intptr_t)i);
        s_menu_btns[i] = btn;

        lv_obj_t *l = ln_w_label(btn, k_section_names[i], LN_FONT_MD,
                                 LN_COL_TEXT);
        lv_obj_align(l, LV_ALIGN_LEFT_MID, 0, 0);

        lv_obj_t *arrow = ln_w_label(btn, LV_SYMBOL_RIGHT, LN_FONT_SM,
                                     LN_COL_DIM);
        lv_obj_align(arrow, LV_ALIGN_RIGHT_MID, 0, 0);
    }

    lv_obj_t *host = ln_w_plain(body);
    lv_obj_set_flex_grow(host, 1);
    lv_obj_set_height(host, lv_pct(100));
    lv_obj_set_style_pad_all(host, 28, 0);

    s_panels[SEC_WAKEWORD]    = build_wakeword_panel(host);
    s_panels[SEC_VOICE]       = build_voice_panel(host);
    s_panels[SEC_VOLUME]      = build_volume_panel(host);
    s_panels[SEC_SENSITIVITY] = build_sensitivity_panel(host);
    s_panels[SEC_NAME]        = build_name_panel(host);
    s_panels[SEC_WIFI]        = build_wifi_panel(host);
    s_panels[SEC_ABOUT]       = build_about_panel(host);

    show_section(SEC_WAKEWORD);
    return scr;
}

void ln_scr_config_set_values(const ln_ui_config_t *cfg)
{
    if (cfg == NULL) {
        return;
    }
    for (int i = 0; i < VOICE_COUNT; i++) {
        if (strcmp(cfg->voice, k_voices[i].id) == 0) {
            voice_mark_active(i);
            break;
        }
        /* unknown voice id: fall back to cedar per settings schema */
        if (i == VOICE_COUNT - 1) {
            voice_mark_active(3);
        }
    }
    if (s_vol_slider != NULL) {
        lv_slider_set_value(s_vol_slider, cfg->volume_pct, LV_ANIM_OFF);
        lv_label_set_text_fmt(s_vol_value, "%d%%", cfg->volume_pct);
    }
    if (s_bri_slider != NULL) {
        uint8_t b = cfg->brightness_pct < 10 ? 10 : cfg->brightness_pct;
        lv_slider_set_value(s_bri_slider, b, LV_ANIM_OFF);
        lv_label_set_text_fmt(s_bri_value, "%d%%", b);
    }
    if (s_sens_slider != NULL) {
        int v = (int)(cfg->sensitivity * 100.0f + 0.5f);
        if (v < 0) {
            v = 0;
        }
        if (v > 100) {
            v = 100;
        }
        lv_slider_set_value(s_sens_slider, v, LV_ANIM_OFF);
        lv_label_set_text_fmt(s_sens_value, "%d%%", v);
    }
    if (s_name_ta != NULL && cfg->device_name[0] != '\0') {
        lv_textarea_set_text(s_name_ta, cfg->device_name);
    }
    if (cfg->wake_model[0] != '\0') {
        for (int i = 0; i < WAKE_MODEL_COUNT; i++) {
            if (strcmp(cfg->wake_model, k_wake_models[i].model) == 0) {
                wake_mark_active(i);
                break;
            }
        }
    }
}

void ln_scr_config_set_shadow(const char *voice, float sensitivity)
{
    if (voice != NULL && voice[0] != '\0') {
        int idx = 3; /* cedar fallback per settings schema */
        for (int i = 0; i < VOICE_COUNT; i++) {
            if (strcmp(voice, k_voices[i].id) == 0) {
                idx = i;
                break;
            }
        }
        voice_mark_active(idx);
    }
    if (sensitivity >= 0.0f && sensitivity <= 1.0f && s_sens_slider != NULL) {
        int v = (int)(sensitivity * 100.0f + 0.5f);
        lv_slider_set_value(s_sens_slider, v, LV_ANIM_OFF);
        lv_label_set_text_fmt(s_sens_value, "%d%%", v);
    }
}

void ln_scr_config_set_net(const char *ssid, const char *ip, const char *signal)
{
    if (s_wifi_ssid_v == NULL) {
        return;
    }
    if (ssid != NULL) {
        lv_label_set_text(s_wifi_ssid_v, ssid[0] ? ssid : "Not connected");
    }
    if (ip != NULL) {
        lv_label_set_text(s_wifi_ip_v, ip[0] ? ip : "—");
        if (s_about_ip_v != NULL) {
            lv_label_set_text(s_about_ip_v, ip[0] ? ip : "—");
        }
    }
    if (signal != NULL) {
        lv_label_set_text(s_wifi_sig_v, signal[0] ? signal : "—");
    }
}

void ln_scr_config_set_about(const char *fw, const char *thing, const char *mac,
                             const char *ip)
{
    if (s_about_fw_v == NULL) {
        return;
    }
    if (fw != NULL) {
        lv_label_set_text(s_about_fw_v, fw);
    }
    if (thing != NULL) {
        lv_label_set_text(s_about_thing_v, thing);
    }
    if (mac != NULL) {
        lv_label_set_text(s_about_mac_v, mac);
    }
    if (ip != NULL) {
        lv_label_set_text(s_about_ip_v, ip);
    }
    refresh_about_dynamic();
}
