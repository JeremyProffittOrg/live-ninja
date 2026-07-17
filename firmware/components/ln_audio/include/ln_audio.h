/*
 * Live Ninja — Tab5 audio pipeline (ln_audio).
 *
 * Owns the ES8388 (speaker DAC) + ES7210 (mic ADC) codecs through the
 * m5stack_tab5 BSP / esp_codec_dev, running the shared I2S port full-duplex
 * at 48 kHz / 16-bit / stereo (TX and RX share BCLK/WS on this board, so a
 * single rate is used and all rate conversion is done in software):
 *
 *   capture:  ES7210 MIC1+MIC2 stereo @48k  --FIR /3-->  16 kHz mono frames
 *             delivered to subscribers together with a software AEC echo
 *             reference (16 kHz) derived from whatever is being played.
 *   playback: 24 kHz mono PCM (OpenAI Realtime pcm16 downlink) -> halfband
 *             FIR x2 -> 48 kHz -> stereo duplicate -> codec, behind a PSRAM
 *             jitter/ring buffer (prebuffer CONFIG_LN_AUDIO_JITTER_PREBUFFER_MS).
 *
 * Volume / mic-mute / speaker-mute persist in NVS namespace "ln_audio".
 */
#pragma once

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include "esp_err.h"

#ifdef __cplusplus
extern "C" {
#endif

#define LN_AUDIO_CAPTURE_SAMPLE_RATE  16000  /* mic frames delivered at this rate  */
#define LN_AUDIO_PLAYBACK_SAMPLE_RATE 24000  /* ln_audio_play() expects this rate  */
#define LN_AUDIO_FRAME_SAMPLES        160    /* 10 ms @ 16 kHz per capture frame   */

/**
 * Capture sink. Called from the audio_rx task every 10 ms.
 * @param mic  LN_AUDIO_FRAME_SAMPLES of 16 kHz mono primary-mic PCM (post mute).
 * @param ref  LN_AUDIO_FRAME_SAMPLES of 16 kHz mono echo-reference PCM
 *             (what the speaker is playing; zeros when idle/muted).
 * Callbacks must be quick (copy out / push to a queue) — never block.
 */
typedef void (*ln_audio_capture_cb_t)(const int16_t *mic, const int16_t *ref,
                                      size_t samples, void *ctx);

/** Bring up codecs, I2S, NVS-persisted volume/mute, and the audio tasks. */
esp_err_t ln_audio_init(void);

/** Register/unregister a capture sink (up to 4; ln_wake uses one). */
esp_err_t ln_audio_capture_subscribe(ln_audio_capture_cb_t cb, void *ctx);
esp_err_t ln_audio_capture_unsubscribe(ln_audio_capture_cb_t cb, void *ctx);

/**
 * Queue 24 kHz mono pcm16 for playback. Blocks (bounded) only if the ring —
 * CONFIG_LN_AUDIO_PLAYBACK_RING_MS deep — is full, providing natural
 * backpressure to the downlink task.
 */
esp_err_t ln_audio_play(const int16_t *pcm, size_t samples);

/** Mark end-of-response so a short (< prebuffer) tail drains immediately. */
void ln_audio_play_end(void);

/** Barge-in: drop everything queued and go silent now. */
esp_err_t ln_audio_play_stop(void);

bool   ln_audio_is_playing(void);
uint32_t ln_audio_play_queued_ms(void);

/** Speaker volume 0-100, persisted. */
esp_err_t ln_audio_set_volume(uint8_t pct);
uint8_t   ln_audio_get_volume(void);

/** Mic privacy mute (codec mute + zeroed frames), persisted. */
esp_err_t ln_audio_set_mic_mute(bool mute);
bool      ln_audio_get_mic_mute(void);

/** Speaker mute, persisted. */
esp_err_t ln_audio_set_spk_mute(bool mute);
bool      ln_audio_get_spk_mute(void);

#ifdef __cplusplus
}
#endif
