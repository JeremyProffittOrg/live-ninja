/*
 * Live Ninja — wake-word + audio front-end (ln_wake).
 *
 * Consumes 16 kHz mic + echo-reference frames from ln_audio, runs the ESP-SR
 * AFE (AEC / NS / VAD) and WakeNet on the ESP32-P4, and:
 *   - posts LN_WAKE_EVENT events (wake detected, VAD transitions) on the
 *     default esp_event loop, and
 *   - streams the AFE-processed (echo-cancelled, noise-suppressed) 16 kHz
 *     mono audio to subscribers — this is the stream the realtime uplink
 *     (net_uplink) should send to OpenAI, not the raw mic.
 *
 * The WakeNet model name comes from NVS ("ln_wake"/"model", default
 * CONFIG_LN_WAKE_DEFAULT_MODEL) and is matched against the models packed in
 * the "model" flash partition, keeping the pipeline model-agnostic (M6 swaps
 * in the custom "Hey Live Ninja" model by name only).
 *
 * Fallback (documented, per plan): if the model partition holds no WakeNet
 * model, the AFE still runs (AEC/NS/VAD) and LN_WAKE_EVT_FALLBACK is posted —
 * wake is then push-to-talk only via ln_wake_trigger(). If ESP-SR cannot
 * start at all (e.g. missing/corrupt model partition), an energy-VAD fallback
 * runs directly on the mic frames and emits the same VAD events.
 */
#pragma once

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include "esp_err.h"
#include "esp_event.h"

#ifdef __cplusplus
extern "C" {
#endif

ESP_EVENT_DECLARE_BASE(LN_WAKE_EVENT);

typedef enum {
    LN_WAKE_EVT_READY = 0,     /* pipeline running; payload: ln_wake_ready_evt_t */
    LN_WAKE_EVT_DETECTED,      /* wake word heard;  payload: ln_wake_detect_evt_t */
    LN_WAKE_EVT_VAD_SPEECH,    /* voice activity started (no payload) */
    LN_WAKE_EVT_VAD_SILENCE,   /* voice activity ended (no payload) */
    LN_WAKE_EVT_FALLBACK,      /* running without WakeNet; payload: ln_wake_ready_evt_t */
} ln_wake_event_id_t;

#define LN_WAKE_MODEL_NAME_MAX 64

typedef struct {
    char model[LN_WAKE_MODEL_NAME_MAX]; /* active model, "" in fallback */
    bool wakenet_active;
    bool afe_active;                    /* false => energy-VAD last-resort path */
} ln_wake_ready_evt_t;

typedef struct {
    char model[LN_WAKE_MODEL_NAME_MAX];
    int  word_index;   /* 1-based WakeNet word; 0 = manual ln_wake_trigger() */
} ln_wake_detect_evt_t;

/**
 * Processed-audio sink. Called from the ww_infer task per AFE fetch chunk
 * (typically 32 ms). pcm is 16 kHz mono, echo-cancelled/noise-suppressed.
 * speech reflects the AFE VAD state for this chunk. Must not block.
 */
typedef void (*ln_wake_audio_cb_t)(const int16_t *pcm, size_t samples,
                                   bool speech, void *ctx);

/** Start the pipeline (requires ln_audio_init() done). Posts LN_WAKE_EVT_READY. */
esp_err_t ln_wake_init(void);

/** Full teardown (tasks, AFE, model list, ln_audio subscription). */
esp_err_t ln_wake_deinit(void);

/** Pause/resume WakeNet detection (AFE keeps running, e.g. during a session). */
esp_err_t ln_wake_enable(bool enable);

/** Manual wake (push-to-talk): posts LN_WAKE_EVT_DETECTED with word_index 0. */
esp_err_t ln_wake_trigger(void);

esp_err_t ln_wake_audio_subscribe(ln_wake_audio_cb_t cb, void *ctx);
esp_err_t ln_wake_audio_unsubscribe(ln_wake_audio_cb_t cb, void *ctx);

/** Active model name ("" when none). */
const char *ln_wake_model_name(void);

/**
 * Persist a new model name to NVS and live-restart the pipeline against it.
 * Returns ESP_ERR_NOT_FOUND (pipeline restarted on previous/any model) if the
 * name matches nothing in the model partition.
 */
esp_err_t ln_wake_set_model(const char *name);

/** True when running without a WakeNet model (push-to-talk only). */
bool ln_wake_fallback_active(void);

#ifdef __cplusplus
}
#endif
