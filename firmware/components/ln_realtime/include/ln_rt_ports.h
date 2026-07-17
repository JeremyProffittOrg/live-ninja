/*
 * ln_rt_ports.h — external interfaces ln_realtime consumes.
 *
 * These are the cross-component seams for the M5 firmware (plan.md M5 task
 * partition). ln_realtime only *declares* them here (kept in exact signature
 * sync with the owning components' public headers) so the components stay
 * decoupled at configure time; they resolve at final link:
 *
 *   - ln_auth_get_jwt()   -> components/ln_net/include/ln_net.h
 *   - ln_audio_play()     -> components/ln_audio/include/ln_audio.h
 *   - ln_audio_play_end() -> components/ln_audio/include/ln_audio.h
 *   - ln_audio_play_stop()-> components/ln_audio/include/ln_audio.h
 */
#pragma once

#include <stddef.h>
#include <stdint.h>

#include "esp_err.h"

#ifdef __cplusplus
extern "C" {
#endif

/**
 * Copy the device's current first-party access JWT (compact JWS string,
 * NUL-terminated) into buf. Implemented by ln_net's auth store, which owns
 * the refresh-token rotation (contracts/api.md POST /auth/refresh) and must
 * return a JWT with enough remaining validity for one HTTPS call —
 * refreshing first if needed (may block briefly on network).
 * Matches ln_net.h exactly.
 *
 * @return ESP_OK on success; ESP_ERR_INVALID_STATE if the device is not
 *         paired/authenticated.
 */
esp_err_t ln_auth_get_jwt(char *buf, size_t len);

/**
 * Queue 24 kHz mono pcm16 assistant audio for playback (OpenAI Realtime
 * downlink rate — ln_audio owns upsampling to the codec rate). Blocks
 * (bounded) only when the jitter ring is full, which is the natural
 * backpressure for the downlink path. Matches ln_audio.h exactly.
 */
esp_err_t ln_audio_play(const int16_t *pcm, size_t samples);

/** Mark end-of-response so a short (< prebuffer) tail drains immediately. */
void ln_audio_play_end(void);

/** Barge-in: drop everything queued and go silent now. */
esp_err_t ln_audio_play_stop(void);

#ifdef __cplusplus
}
#endif
