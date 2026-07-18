/*
 * ln_ui — Live Ninja LVGL UI (M5Stack Tab5, 1280x720 landscape).
 *
 * Owns every screen of the M5 state machine (Boot / Provisioning / Idle /
 * Listening / Thinking / Speaking / Config / Error) per mockups/m5stack/.
 *
 * Data flows IN two ways (both supported, same effect):
 *   1. esp_events on the default loop (see ln_events.h) — ln_ui subscribes to
 *      LN_CTRL/LN_NET/LN_AUDIO/LN_WAKE/LN_RT/LN_IOT events in ln_ui_init().
 *   2. The direct setter functions below (used by main/ before ctrl exists,
 *      and usable by any task — every setter takes the LVGL port lock, which
 *      is recursive, so calls from LVGL callbacks are safe too).
 *
 * User intents flow OUT as LN_UI_EVENT posts on the default loop
 * (stop/cancel/finish taps, settings opened/closed, voice/volume/sensitivity/
 * name changes, wifi re-setup, factory reset, retry). ln_ui never changes the
 * app state itself — the ctrl state machine reacts to LN_UI_EVENTs and drives
 * screens via LN_CTRL_STATE_CHANGED (or ln_ui_set_state()).
 */
#pragma once

#include <stdbool.h>
#include <stdint.h>

#include "esp_err.h"

#ifdef __cplusplus
extern "C" {
#endif

/** Top-level device states (mirrors ln_app_state_t in ln_events.h, same order). */
typedef enum {
    LN_UI_STATE_BOOT = 0,
    LN_UI_STATE_PROVISIONING,
    LN_UI_STATE_IDLE,
    LN_UI_STATE_LISTENING,
    LN_UI_STATE_THINKING,
    LN_UI_STATE_SPEAKING,
    LN_UI_STATE_CONFIG,
    LN_UI_STATE_ERROR,
} ln_ui_state_t;

/** Values shown/edited on the Config screen (see contracts/settings.schema.json). */
typedef struct {
    char    voice[16];      /*!< active voice id, e.g. "cedar"      */
    uint8_t volume_pct;     /*!< speaker volume 0-100               */
    uint8_t brightness_pct; /*!< display brightness 0-100           */
    float   sensitivity;    /*!< wake sensitivity 0.0-1.0           */
    char    device_name[33];/*!< user-visible device name           */
} ln_ui_config_t;

/**
 * Build all screens, subscribe to LN_* events on the default loop and load the
 * Boot screen. Must be called after bsp_display_start() (and ideally after
 * esp_event_loop_create_default(); without a default loop the UI still works
 * through the direct setters, it just can't emit/receive events).
 */
esp_err_t ln_ui_init(void);

/** Switch the displayed screen. Also driven by LN_CTRL_STATE_CHANGED events. */
void ln_ui_set_state(ln_ui_state_t state);

/* ---- Status (Idle bottom bar, Error screen) ---- */
void ln_ui_set_wifi_status(const char *text);  /*!< e.g. "Home Studio · Strong" */
void ln_ui_set_cloud_status(const char *text); /*!< e.g. "Connected"            */
void ln_ui_set_account(const char *name);      /*!< e.g. "Jeremy P."            */

/** Wake phrase shown on Idle/Listening (model-agnostic; from NVS config). */
void ln_ui_set_wake_phrase(const char *phrase);

/** Identity strings for the Config > About panel. Any arg may be NULL (kept). */
void ln_ui_set_device_info(const char *fw_version, const char *thing_name,
                           const char *mac, const char *ip);

/* ---- Session (Listening / Thinking / Speaking) ---- */
/** User speech transcript. replace=false appends a fragment, true replaces. */
void ln_ui_user_transcript(const char *text, bool replace);
/** Assistant response text. replace=false appends a fragment, true replaces. */
void ln_ui_assistant_transcript(const char *text, bool replace);
/** Mic input level 0-100 for the Listening visualizer (post <= ~20 Hz). */
void ln_ui_mic_level(uint8_t pct);

/* ---- Config screen ---- */
/** Push current values into the Config screen controls (from NVS/shadow). */
void ln_ui_config_values(const ln_ui_config_t *cfg);

/* ---- Onboarding (Provisioning screen) ---- */
/** Phase 1: SoftAP portal is up — show join steps + QR of the portal URL. */
void ln_ui_onboarding_portal(const char *ssid, const char *url);
/** Phase 2: WiFi done, account pairing — show claim URL QR + pairing code. */
void ln_ui_onboarding_pairing(const char *claim_url, const char *code);
/** WiFi joined — refresh the QR to the device's new STA URL (http://<ip>/). */
void ln_ui_onboarding_connected(const char *ip);
/** Footer status line, e.g. "Waiting for a device to connect…". */
void ln_ui_onboarding_status(const char *text);

/* ---- Error screen ---- */
/** Fill the Error screen. Any arg may be NULL to keep the previous value. */
void ln_ui_error_show(const char *title, const char *detail, const char *code);
/** Auto-retry countdown seconds; pass a negative value to hide the line. */
void ln_ui_error_countdown(int secs);

#ifdef __cplusplus
}
#endif
