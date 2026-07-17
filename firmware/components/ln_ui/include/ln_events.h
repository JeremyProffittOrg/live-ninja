/*
 * ln_events.h — canonical index of Live Ninja esp_event contracts + the
 * shared app-state enum + the bases owned by the UI/ctrl layer.
 *
 * All events travel on the DEFAULT esp_event loop
 * (esp_event_loop_create_default(), created in app_main before components
 * start).
 *
 * Base ownership (one ESP_EVENT_DEFINE_BASE per base, in the producer):
 *
 *   Base            Defined in                 Contract header
 *   --------------  -------------------------  --------------------------------
 *   LN_NET_EVENT    ln_net/src/ln_net.c        ln_net.h        (WiFi/portal/pairing)
 *   LN_RT_EVENT     ln_realtime/ln_realtime.c  ln_realtime.h   (OpenAI Realtime)
 *   LN_IOT_EVENT    ln_iot/src/ln_iot.c        ln_iot.h        (MQTT/shadow/OTA)
 *   LN_WAKE_EVENT   ln_wake/ln_wake.c          ln_wake.h       (AFE/WakeNet)
 *   LN_CTRL_EVENT   ln_ui/ln_events.c          this header     (ctrl state machine)
 *   LN_UI_EVENT     ln_ui/ln_events.c          this header     (touch intents)
 *   LN_AUDIO_EVENT  ln_ui/ln_events.c          this header     (mic level for the
 *                                              Listening visualizer; posted by
 *                                              whichever audio task owns levels)
 *
 * Rules:
 *  - Never re-define a base you don't own; include the producer's header.
 *  - Payloads are fixed-size structs (esp_event copies by value) unless the
 *    producer's header documents otherwise; strings are NUL-terminated.
 *  - Additive evolution only: append new IDs, never renumber.
 */
#pragma once

#include <stdbool.h>
#include <stdint.h>

#include "esp_event.h"

#ifdef __cplusplus
extern "C" {
#endif

/* Bases owned by this layer (defined once in ln_ui/ln_events.c). */
ESP_EVENT_DECLARE_BASE(LN_CTRL_EVENT);  /*!< ctrl state machine (main/)   */
ESP_EVENT_DECLARE_BASE(LN_UI_EVENT);    /*!< ln_ui user intents (touch)   */
ESP_EVENT_DECLARE_BASE(LN_AUDIO_EVENT); /*!< audio-path UI signals        */

/* ------------------------------------------------------------------ */
/* Shared app state (plan.md M5 state diagram)                         */
/* ------------------------------------------------------------------ */

typedef enum {
    LN_STATE_BOOT = 0,
    LN_STATE_PROVISIONING,
    LN_STATE_IDLE,
    LN_STATE_LISTENING,
    LN_STATE_THINKING,
    LN_STATE_SPEAKING,
    LN_STATE_CONFIG,
    LN_STATE_ERROR,
} ln_app_state_t;

/* ------------------------------------------------------------------ */
/* Small shared payloads                                               */
/* ------------------------------------------------------------------ */

/** Generic 0-100 percentage payload. */
typedef struct {
    uint8_t pct;
} ln_evt_pct_t;

/** Generic bounded-float payload (e.g. wake sensitivity 0.0-1.0). */
typedef struct {
    float value;
} ln_evt_float_t;

/* ------------------------------------------------------------------ */
/* LN_CTRL_EVENT — posted by the ctrl state machine (main/)            */
/* ------------------------------------------------------------------ */

typedef enum {
    LN_CTRL_STATE_CHANGED = 0, /*!< payload: ln_evt_state_changed_t */
} ln_ctrl_event_id_t;

typedef struct {
    ln_app_state_t state; /*!< new state    */
    ln_app_state_t prev;  /*!< previous one */
} ln_evt_state_changed_t;

/* ------------------------------------------------------------------ */
/* LN_AUDIO_EVENT — audio-path signals the UI renders                  */
/* ------------------------------------------------------------------ */

typedef enum {
    LN_AUDIO_MIC_LEVEL = 0, /*!< payload: ln_evt_pct_t; post <= ~20 Hz.
                              Drives the Listening-screen level bars.   */
} ln_audio_event_id_t;

/* ------------------------------------------------------------------ */
/* LN_UI_EVENT — posted by ln_ui (user touch intents). Consumed by     */
/* ctrl (state transitions) and by the owning component (settings).    */
/* ------------------------------------------------------------------ */

typedef enum {
    LN_UI_STOP_TAPPED = 0,        /*!< no payload — barge-in / stop speaking   */
    LN_UI_CANCEL_TAPPED,          /*!< no payload — cancel listening           */
    LN_UI_FINISH_TAPPED,          /*!< no payload — end turn now               */
    LN_UI_SETTINGS_OPEN_REQUESTED,/*!< no payload — Idle -> Config             */
    LN_UI_SETTINGS_CLOSED,        /*!< no payload — Config -> Idle (back)      */
    LN_UI_RETRY_TAPPED,           /*!< no payload — Error retry                */
    LN_UI_VOICE_SELECTED,         /*!< payload: ln_evt_voice_sel_t             */
    LN_UI_VOLUME_CHANGED,         /*!< payload: ln_evt_pct_t                   */
    LN_UI_BRIGHTNESS_CHANGED,     /*!< payload: ln_evt_pct_t                   */
    LN_UI_SENSITIVITY_CHANGED,    /*!< payload: ln_evt_float_t (0.0-1.0)       */
    LN_UI_DEVICE_NAME_CHANGED,    /*!< payload: ln_evt_name_t                  */
    LN_UI_WIFI_SETUP_REQUESTED,   /*!< no payload — re-run SoftAP portal       */
    LN_UI_FACTORY_RESET_REQUESTED,/*!< no payload — confirmed on-screen        */
} ln_ui_event_id_t;

typedef struct {
    char voice[16]; /*!< selected voice id, settings.schema.json enum */
} ln_evt_voice_sel_t;

typedef struct {
    char name[33]; /*!< user-chosen device display name */
} ln_evt_name_t;

#ifdef __cplusplus
}
#endif
