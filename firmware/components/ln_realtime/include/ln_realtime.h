/*
 * ln_realtime — OpenAI Realtime WSS client for the Live Ninja M5Stack Tab5.
 *
 * Owns the realtime voice transport (plan.md M5, WS-F):
 *   1. Session bootstrap over HTTPS: GET {backend}/v1/realtime/session with the
 *      device JWT (contracts/api.md) -> broker-minted OpenAI ephemeral token.
 *   2. WSS to wss://api.openai.com/v1/realtime?model=... with
 *      "Authorization: Bearer ek_..." (locked decision: WSS + pcm16, no WebRTC).
 *   3. Uplink: ln_realtime_send_audio() -> input_audio_buffer.append frames
 *      (base64 pcm16, 16 kHz mono per the M5 audio contract).
 *   4. Downlink: response.output_audio.delta -> base64 decode -> ln_audio
 *      playback queue (24 kHz pcm16). input_audio_buffer.speech_started ->
 *      instant local barge-in (playback flush + response.cancel) + esp_event.
 *   5. Error/close -> bounded exponential-backoff reconnect (fresh ephemeral
 *      token per attempt) with LN_RT_EVENT_* posted on the default event loop.
 *
 * Threading: all public functions are task-safe. ln_realtime_send_audio() is
 * designed to be called from the audio uplink task at 20 ms cadence. Events
 * are posted to the default esp_event loop; handlers run in that loop's task.
 *
 * External interfaces consumed (implemented by ln_net / ln_audio) are declared
 * in ln_rt_ports.h.
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

/** Event base for all realtime-client events (default event loop). */
ESP_EVENT_DECLARE_BASE(LN_RT_EVENT);

typedef enum {
    /** Fetching an ephemeral token / opening the WSS link. No payload. */
    LN_RT_EVENT_CONNECTING = 0,
    /** WSS link is up and session.update has been sent. No payload. */
    LN_RT_EVENT_CONNECTED,
    /** Server acked the session (session.created/updated). Audio may flow. No payload. */
    LN_RT_EVENT_SESSION_READY,
    /** Server VAD detected user speech — local barge-in already executed
     *  (playback flushed, response.cancel sent). No payload. */
    LN_RT_EVENT_SPEECH_STARTED,
    /** Server VAD detected end of user speech. No payload. */
    LN_RT_EVENT_SPEECH_STOPPED,
    /** A model response started (response.created) — "Thinking/Speaking". No payload. */
    LN_RT_EVENT_RESPONSE_STARTED,
    /** All response audio has been delivered (response.output_audio.done). No payload. */
    LN_RT_EVENT_RESPONSE_AUDIO_DONE,
    /** Response fully finished (response.done). No payload. */
    LN_RT_EVENT_RESPONSE_DONE,
    /** Assistant transcript text chunk. Payload: ln_rt_transcript_chunk_t. */
    LN_RT_EVENT_TRANSCRIPT_DELTA,
    /** Link lost while a session was active; a reconnect attempt is scheduled.
     *  Payload: ln_rt_reconnect_info_t. */
    LN_RT_EVENT_RECONNECTING,
    /** Session ended (requested stop, or reconnect budget exhausted). No payload. */
    LN_RT_EVENT_DISCONNECTED,
    /** An error occurred. Payload: ln_rt_error_info_t. fatal=true means the
     *  client gave up (auth/quota rejection or reconnect exhaustion) and ctrl
     *  must decide what to do (re-auth, cooldown, show Error state). */
    LN_RT_EVENT_ERROR,
} ln_rt_event_id_t;

/** Payload for LN_RT_EVENT_TRANSCRIPT_DELTA (copied by esp_event). */
typedef struct {
    char text[120];  /**< NUL-terminated UTF-8 chunk (truncated if longer). */
    bool final;      /**< true = end-of-response full transcript (also truncated). */
} ln_rt_transcript_chunk_t;

/** Payload for LN_RT_EVENT_RECONNECTING. */
typedef struct {
    int attempt;      /**< 1-based attempt number about to run. */
    int max_attempts; /**< Attempt budget before giving up. */
    int delay_ms;     /**< Backoff delay before this attempt. */
} ln_rt_reconnect_info_t;

/** Payload for LN_RT_EVENT_ERROR. */
typedef struct {
    char code[48];     /**< Short machine code, e.g. "auth", "quota", "ws_connect". */
    char message[160]; /**< Human-readable detail (truncated). */
    bool fatal;        /**< true = client stopped trying; session is over. */
} ln_rt_error_info_t;

/**
 * One-time init: allocates PSRAM buffers and starts the realtime worker task.
 * Requires the default event loop to exist (esp_event_loop_create_default()).
 */
esp_err_t ln_realtime_init(void);

/**
 * Begin a realtime session: fetch an ephemeral token from the backend broker
 * and open the WSS link. Asynchronous — progress is reported via LN_RT_EVENT.
 * Returns ESP_ERR_INVALID_STATE if a session is already running.
 */
esp_err_t ln_realtime_start(void);

/**
 * End the current session (graceful WSS close). Asynchronous; a final
 * LN_RT_EVENT_DISCONNECTED is posted when torn down. Safe to call anytime.
 */
esp_err_t ln_realtime_stop(void);

/** true once the WSS link is connected and frames can be sent. */
bool ln_realtime_is_connected(void);

/** true from ln_realtime_start() until the session fully ends. */
bool ln_realtime_is_running(void);

/**
 * Stream microphone audio uplink as input_audio_buffer.append.
 * samples: pcm16 little-endian mono at the negotiated capture rate
 * (16 kHz per plan.md M5 audio path). Large buffers are sliced internally.
 * Returns ESP_ERR_INVALID_STATE when not connected (caller should drop the
 * chunk — no queueing here; the AFE ring buffer upstream owns buffering).
 */
esp_err_t ln_realtime_send_audio(const int16_t *samples, size_t n_samples);

/**
 * Locally-initiated barge-in (e.g. wake word re-triggered or touch tap while
 * the assistant is speaking): flushes the playback queue immediately and sends
 * response.cancel. Server-VAD barge-in (speech_started) is handled internally.
 */
esp_err_t ln_realtime_barge_in(void);

#ifdef __cplusplus
}
#endif
