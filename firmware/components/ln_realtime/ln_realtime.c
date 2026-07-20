/*
 * ln_realtime.c — realtime voice WSS client core (M5Stack Tab5).
 *
 * Three transports behind one lifecycle, chosen by the device's voiceEngine
 * pin (resolved server-side; see ln_rt_session.c):
 *   - OPENAI_DIRECT (default): client-direct WSS to wss://api.openai.com with a
 *     Bearer ephemeral token; a session.update pins pcm16 in/out on connect.
 *   - NOVA_BRIDGE (M12, FR-VE-03): WSS to the backend Nova Sonic media bridge
 *     (nova.live.jeremy.ninja) with a single-use token carried in the URL and
 *     no OpenAI ephemeral / no session.update — the bridge holds the Bedrock
 *     bidirectional stream and speaks the same pcm16 event framing, so uplink
 *     (input_audio_buffer.append), downlink (response.output_audio.delta),
 *     transcripts and barge-in flow through unchanged. HIL-unverified.
 *   - GEMINI_DIRECT (M13): client-direct WSS to Gemini Live
 *     (generativelanguage.googleapis.com, BidiGenerateContentConstrained) with
 *     a config-constrained ephemeral token URL-escaped into ?access_token=.
 *     The client sends the broker-staged {"setup":...} frame on connect and
 *     readiness gates on the server's setupComplete. Uplink is the AFE's
 *     native 16 kHz (realtimeInput.audio JSON+base64 — the 16k->24k resampler
 *     is BYPASSED); downlink stays base64 pcm16 @ 24 kHz
 *     (serverContent.modelTurn.parts[].inlineData). goAway triggers the normal
 *     reconnect, carrying the sessionResumptionUpdate handle so the fresh
 *     token resumes the same conversation. HIL-unverified (locked decision D1,
 *     gemini-plan.md §2).
 *
 * Worker task owns the connection lifecycle (session fetch -> WSS connect ->
 * supervise -> reconnect/backoff). The esp_websocket_client task delivers RX
 * frames to ws_event_handler(), which reassembles fragmented payloads,
 * cJSON-parses server events, streams audio deltas to ln_audio, and posts
 * LN_RT_EVENT_* to the default esp_event loop.
 *
 * RAM discipline: all large buffers (RX reassembly, base64 decode, uplink
 * frame) are allocated once in PSRAM at init and reused; the audio delta
 * decoder works in bounded chunks with a carry byte so arbitrarily large
 * deltas never need a proportional buffer.
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "freertos/FreeRTOS.h"
#include "freertos/event_groups.h"
#include "freertos/queue.h"
#include "freertos/semphr.h"
#include "freertos/stream_buffer.h"
#include "freertos/task.h"

#include "cJSON.h"
#include "esp_crt_bundle.h"
#include "esp_event.h"
#include "esp_heap_caps.h"
#include "esp_log.h"
#include "esp_websocket_client.h"
#include "mbedtls/base64.h"

#include "ln_realtime.h"
#include "ln_rt_internal.h"
#include "ln_rt_ports.h"

ESP_EVENT_DEFINE_BASE(LN_RT_EVENT);

static const char *TAG = "ln_rt";

#define LN_RT_WS_URL_FMT        "wss://api.openai.com/v1/realtime?model=%s"
#define LN_RT_RX_BUF_INIT       (32 * 1024)
#define LN_RT_RX_BUF_MAX        (256 * 1024)
#define LN_RT_DEC_BUF_SZ        (24 * 1024)
#define LN_RT_B64_CHUNK         30000 /* multiple of 4; decodes to 22500 B < DEC_BUF */
#define LN_RT_UPLINK_BUF_SZ     (16 * 1024)
#define LN_RT_UPLINK_MAX_SAMPLES 4800 /* 200 ms @ 24 kHz -> ~12.8 KB base64 frame */
/* Uplink resampler: the AFE delivers 16 kHz frames but a GA realtime pcm
 * session only speaks 24 kHz (audio/pcm rate=24000 is the sole supported
 * rate) — sending 16 kHz raw made the server hear 1.5x-speed audio, so VAD
 * and transcription never produced a usable turn (the "hears me but never
 * answers" HIL failure). Input is sliced so one slice's 3/2 output fits the
 * uplink frame. */
#define LN_RT_RESAMPLE_IN_MAX   3200  /* 3200 in @16k -> 4800 out @24k */
#define LN_RT_MAX_RECONNECT     5
#define LN_RT_CONNECT_TIMEOUT_MS 15000
/* 4 s, was 1 s: the SDIO TX path stalls transiently for 1-2 s under
 * combined RX/TX load (HIL 2026-07-19) and a 1 s write timeout made
 * esp_websocket_client abort the whole session on what was a survivable
 * hiccup — the dedicated uplink task absorbs the block, the audio
 * pipeline no longer cares (stream-buffer decoupling). */
#define LN_RT_SEND_TIMEOUT      pdMS_TO_TICKS(4000)
#define LN_RT_TASK_STACK        10240
#define LN_RT_TASK_PRIO         5
#define LN_RT_WS_TASK_STACK     8192

typedef enum {
    LN_RT_CMD_START = 1,
} ln_rt_cmd_t;

#define EG_WS_CONNECTED BIT0
#define EG_WS_DOWN      BIT1
#define EG_STOP_REQ     BIT2

static QueueHandle_t s_cmd_q;
static EventGroupHandle_t s_eg;
static SemaphoreHandle_t s_send_mtx; /* guards s_ws handle + uplink buffer */
static TaskHandle_t s_task;

static esp_websocket_client_handle_t s_ws;
static volatile bool s_connected;
static volatile bool s_should_run;
static volatile bool s_response_active;
static bool s_session_ready_posted; /* ws task only */

static char *s_rx_buf; /* PSRAM, grows to LN_RT_RX_BUF_MAX */
static size_t s_rx_cap;
static size_t s_rx_len;

static uint8_t *s_dec_buf; /* PSRAM, base64 decode staging (byte 0 = carry slot) */
static uint8_t s_carry;
static bool s_have_carry;

static char *s_uplink_buf; /* PSRAM, JSON frame + base64 payload */
static int16_t *s_rs_buf;  /* PSRAM, 16k->24k resampled uplink staging */

/* Uplink decoupling (HIL find 2026-07-19): ln_realtime_send_audio used to
 * base64+send synchronously on the CALLER's task — the AFE/wake pipeline —
 * with a 1 s blocking send timeout, so one WSS/TLS/SDIO hiccup back-pressured
 * straight into the audio path (AFE FEED ringbuffer overflow) and dropped the
 * link. Now the producer only resamples into a PSRAM stream buffer (zero
 * block; drop-newest on full — a real-time mic must never wait) and a
 * dedicated sender task drains it to the socket. ~2 s of 24 kHz pcm16. */
#define LN_RT_UP_SB_BYTES   (2 * 24000 * 2)
#define LN_RT_UP_TASK_STACK 4096
#define LN_RT_UP_TASK_PRIO  4
static StreamBufferHandle_t s_up_sb;
static StaticStreamBuffer_t s_up_sb_struct;
static uint8_t *s_up_storage; /* PSRAM */
static uint32_t s_up_dropped; /* bytes dropped while the buffer was full */

static char s_ws_url[1280];   /* OpenAI URL is short; Nova bridge URL (ws_url[640]) + "?token=" + token[512]
                                 needs headroom. Gemini worst case: 110-char endpoint + "?access_token=" (14)
                                 + a fully %-escaped ~76-char token (228) ≈ 352 — comfortable. */
static char s_ws_headers[576]; /* "Authorization: Bearer ek_...\r\n" (OpenAI-direct only) */

/* Transport the current session negotiated (set in ws_open, read by the WS
 * event handler on the esp_websocket_client task). Only the OpenAI-direct path
 * sends OpenAI's session.update on connect; the Nova bridge owns session
 * config server-side and would not understand that frame. The Gemini path
 * instead sends s_setup_frame on connect (below). */
static volatile ln_rt_engine_mode_t s_engine_mode = LN_RT_ENGINE_OPENAI_DIRECT;

/* GEMINI_DIRECT session state (M13). s_setup_frame points into the PSRAM
 * staging buffer owned by ln_rt_session.c (valid for the whole session: the
 * next fetch only happens after teardown); set by ws_open on the worker task
 * before the WS client starts, read in WEBSOCKET_EVENT_CONNECTED on the WS
 * task. s_resume_handle holds the latest sessionResumptionUpdate handle
 * (written on the WS task, read by the worker during a reconnect fetch —
 * never concurrently, since fetches only run while no WSS session is live).
 * Google documents no handle size; a few hundred bytes observed, 512 is
 * generous. s_tool_buf stages toolResponse refusal frames (WS task only). */
static const char *s_setup_frame;
static char s_resume_handle[512];
#define LN_RT_TOOL_BUF_SZ 4096
static char *s_tool_buf; /* PSRAM */

/* ---------------------------------------------------------------- events -- */

static void post_evt(int32_t id)
{
    esp_event_post(LN_RT_EVENT, id, NULL, 0, pdMS_TO_TICKS(50));
}

static void post_err(const char *code, const char *msg, bool fatal)
{
    ln_rt_error_info_t e = { .fatal = fatal };
    strlcpy(e.code, code, sizeof(e.code));
    strlcpy(e.message, msg, sizeof(e.message));
    esp_event_post(LN_RT_EVENT, LN_RT_EVENT_ERROR, &e, sizeof(e), pdMS_TO_TICKS(50));
}

static void post_transcript(const char *text, size_t len, bool final)
{
    ln_rt_transcript_chunk_t c = { .final = final };
    if (len >= sizeof(c.text)) {
        len = sizeof(c.text) - 1;
    }
    memcpy(c.text, text, len);
    c.text[len] = '\0';
    esp_event_post(LN_RT_EVENT, LN_RT_EVENT_TRANSCRIPT_DELTA, &c, sizeof(c), pdMS_TO_TICKS(20));
}

/* ------------------------------------------------------- uplink resampler -- */

/*
 * Rational 3/2 resampler (16 kHz -> 24 kHz), polyphase over a virtual 48 kHz
 * stream: zero-stuff the input x3, lowpass, keep every 2nd virtual sample.
 * The prototype filter is ln_resample.c's 33-tap Hamming windowed-sinc
 * (fc = 6.8 kHz @ 48 kHz) scaled x3 — a zero-stuffed x3 interpolation needs
 * passband gain 3 to restore unity (same rule as ln_itp2's gain-2 taps).
 * Self-contained here because ln_audio keeps ln_resample.h private.
 */
#define LN_R32_TAPS 33
static const int16_t s_r32_coef[LN_R32_TAPS] = {
    156,   132,   -27,   -318,  -531,  -288,  567,   1542,  1566,  -147,
    -3015, -4800, -2748, 4359,  14742, 24060, 27804, 24060, 14742, 4359,
    -2748, -4800, -3015, -147,  1566,  1542,  567,   -288,  -531,  -318,
    -27,   132,   156,
};
#define LN_R32_HIST ((LN_R32_TAPS + 2) / 3) /* past input samples a tap can reach */

static struct {
    int16_t hist[LN_R32_HIST];
    uint8_t odd; /* parity of the next virtual 48 kHz position */
} s_r32;

static void r32_reset(void)
{
    memset(&s_r32, 0, sizeof(s_r32));
}

/** Resample in (16 kHz) into out (24 kHz). out must hold 3*in_samples/2 + 2.
 *  Returns output sample count. */
static size_t r32_process(const int16_t *in, size_t in_samples, int16_t *out)
{
    size_t out_n = 0;
    for (size_t i = 0; i < in_samples; i++) {
        /* Input sample i sits at virtual positions [3i, 3i+2]; emit the even
         * ones. `sub` is also the polyphase index (v mod 3) for tap
         * selection: y[v] = sum h[t] * x[(v - t)/3] over t ≡ sub (mod 3). */
        for (int sub = 0; sub < 3; sub++) {
            if (s_r32.odd) {
                s_r32.odd = 0;
                continue;
            }
            s_r32.odd = 1;
            int32_t acc = 0;
            for (int t = sub; t < LN_R32_TAPS; t += 3) {
                int32_t idx = (int32_t)i - (t - sub) / 3;
                int16_t x;
                if (idx >= 0) {
                    x = in[idx];
                } else if (idx >= -LN_R32_HIST) {
                    x = s_r32.hist[LN_R32_HIST + idx];
                } else {
                    x = 0;
                }
                acc += (int32_t)x * s_r32_coef[t];
            }
            int32_t v = acc >> 15;
            if (v > 32767) {
                v = 32767;
            } else if (v < -32768) {
                v = -32768;
            }
            out[out_n++] = (int16_t)v;
        }
    }
    if (in_samples >= LN_R32_HIST) {
        memcpy(s_r32.hist, &in[in_samples - LN_R32_HIST],
               LN_R32_HIST * sizeof(int16_t));
    } else {
        size_t keep = LN_R32_HIST - in_samples;
        memmove(s_r32.hist, &s_r32.hist[in_samples], keep * sizeof(int16_t));
        memcpy(&s_r32.hist[keep], in, in_samples * sizeof(int16_t));
    }
    return out_n;
}

/* ------------------------------------------------------------------ send -- */

/** Send a raw text frame; safe from any task. */
static esp_err_t ws_send_str(const char *frame)
{
    esp_err_t ret = ESP_ERR_INVALID_STATE;
    if (xSemaphoreTake(s_send_mtx, LN_RT_SEND_TIMEOUT) != pdTRUE) {
        return ESP_ERR_TIMEOUT;
    }
    if (s_ws != NULL && s_connected) {
        int len = (int)strlen(frame);
        int sent = esp_websocket_client_send_text(s_ws, frame, len, LN_RT_SEND_TIMEOUT);
        ret = (sent == len) ? ESP_OK : ESP_FAIL;
    }
    xSemaphoreGive(s_send_mtx);
    return ret;
}

static esp_err_t send_audio_slice(const int16_t *samples, size_t n_samples)
{
    /* OpenAI + Nova bridge speak input_audio_buffer.append (24 kHz pcm16);
     * Gemini Live takes realtimeInput.audio with the AFE's native 16 kHz —
     * the mimeType rate is the authoritative uplink rate server-side. */
    static const char k_oai_prefix[] = "{\"type\":\"input_audio_buffer.append\",\"audio\":\"";
    static const char k_oai_suffix[] = "\"}";
    static const char k_gem_prefix[] = "{\"realtimeInput\":{\"audio\":{\"data\":\"";
    static const char k_gem_suffix[] = "\",\"mimeType\":\"audio/pcm;rate=16000\"}}}";
    esp_err_t ret = ESP_ERR_INVALID_STATE;
    const bool gemini = (s_engine_mode == LN_RT_ENGINE_GEMINI_DIRECT);
    const char *prefix = gemini ? k_gem_prefix : k_oai_prefix;
    const char *suffix = gemini ? k_gem_suffix : k_oai_suffix;
    const size_t prefix_len = gemini ? sizeof(k_gem_prefix) - 1 : sizeof(k_oai_prefix) - 1;
    const size_t suffix_len = gemini ? sizeof(k_gem_suffix) - 1 : sizeof(k_oai_suffix) - 1;

    if (xSemaphoreTake(s_send_mtx, LN_RT_SEND_TIMEOUT) != pdTRUE) {
        return ESP_ERR_TIMEOUT;
    }
    if (s_ws != NULL && s_connected) {
        size_t pos = prefix_len;
        memcpy(s_uplink_buf, prefix, prefix_len);
        size_t olen = 0;
        int rc = mbedtls_base64_encode((unsigned char *)s_uplink_buf + pos,
                                       LN_RT_UPLINK_BUF_SZ - pos - suffix_len - 1, &olen,
                                       (const unsigned char *)samples, n_samples * 2);
        if (rc == 0) {
            pos += olen;
            memcpy(s_uplink_buf + pos, suffix, suffix_len);
            pos += suffix_len;
            int sent = esp_websocket_client_send_text(s_ws, s_uplink_buf, (int)pos,
                                                      LN_RT_SEND_TIMEOUT);
            ret = (sent == (int)pos) ? ESP_OK : ESP_FAIL;
        } else {
            ret = ESP_ERR_NO_MEM;
        }
    }
    xSemaphoreGive(s_send_mtx);
    return ret;
}

esp_err_t ln_realtime_send_audio(const int16_t *samples, size_t n_samples)
{
    if (samples == NULL || n_samples == 0) {
        return ESP_ERR_INVALID_ARG;
    }
    if (!s_connected) {
        return ESP_ERR_INVALID_STATE;
    }
    /* Producer side (AFE/wake task): resample and enqueue only — the
     * uplink task owns the (blocking) socket writes. Send with zero
     * timeout: when the network is behind, newest audio is dropped here
     * rather than stalling the real-time capture pipeline. */
    if (s_engine_mode == LN_RT_ENGINE_GEMINI_DIRECT) {
        /* Gemini takes the AFE's native 16 kHz directly (send_audio_slice
         * frames it as audio/pcm;rate=16000) — the 16k->24k resampler is
         * OpenAI-only and MUST be bypassed here or the server would hear
         * 2/3-speed audio. The stream buffer carries raw pcm16 bytes either
         * way; its 2 s @ 24 kHz sizing is 3 s of headroom at 16 kHz. */
        if (s_up_sb != NULL) {
            size_t want = n_samples * sizeof(int16_t);
            size_t put = xStreamBufferSend(s_up_sb, samples, want, 0);
            if (put < want) {
                s_up_dropped += (uint32_t)(want - put);
            }
        }
        return ESP_OK;
    }
    while (n_samples > 0) {
        size_t slice = (n_samples > LN_RT_RESAMPLE_IN_MAX) ? LN_RT_RESAMPLE_IN_MAX
                                                           : n_samples;
        size_t rs_n = r32_process(samples, slice, s_rs_buf);
        if (rs_n > 0 && s_up_sb != NULL) {
            size_t want = rs_n * sizeof(int16_t);
            size_t put = xStreamBufferSend(s_up_sb, s_rs_buf, want, 0);
            if (put < want) {
                s_up_dropped += (uint32_t)(want - put);
            }
        }
        samples += slice;
        n_samples -= slice;
    }
    return ESP_OK;
}

/* Drains the uplink stream buffer to the WSS in ≤200 ms slices. Runs
 * forever; harmlessly idles while disconnected (the buffer is reset on
 * every fresh connect). */
static void ln_rt_uplink_task(void *arg)
{
    (void)arg;
    /* Local staging: keep whole samples; stream-buffer reads are byte-wise. */
    static int16_t frame[LN_RT_UPLINK_MAX_SAMPLES];
    static uint8_t carry_byte;
    static bool have_carry;
    for (;;) {
        if (!s_connected) {
            vTaskDelay(pdMS_TO_TICKS(50));
            continue;
        }
        uint8_t *dst = (uint8_t *)frame;
        size_t off = 0;
        if (have_carry) {
            dst[off++] = carry_byte;
            have_carry = false;
        }
        size_t got = xStreamBufferReceive(s_up_sb, dst + off,
                                          sizeof(frame) - off,
                                          pdMS_TO_TICKS(100));
        size_t total = off + got;
        if (total < sizeof(int16_t)) {
            if (total == 1) {
                carry_byte = dst[0];
                have_carry = true;
            }
            continue;
        }
        if ((total & 1U) != 0) {
            carry_byte = dst[total - 1];
            have_carry = true;
            total--;
        }
        if (s_up_dropped != 0) {
            ESP_LOGW(TAG, "uplink behind — dropped %u bytes of mic audio",
                     (unsigned)s_up_dropped);
            s_up_dropped = 0;
        }
        (void)send_audio_slice(frame, total / sizeof(int16_t));
    }
}

esp_err_t ln_realtime_barge_in(void)
{
    if (!s_connected) {
        return ESP_ERR_INVALID_STATE;
    }
    ln_audio_play_stop();
    s_have_carry = false;
    /* Gemini has no client->server cancel primitive in auto-VAD mode — the
     * local playback flush above is the whole barge-in; the server's own VAD
     * interrupts generation when the user keeps talking. */
    if (s_response_active && s_engine_mode != LN_RT_ENGINE_GEMINI_DIRECT) {
        return ws_send_str("{\"type\":\"response.cancel\"}");
    }
    return ESP_OK;
}

/* ------------------------------------------------------------ downlink RX -- */

/** Decode a base64 pcm16 delta in bounded chunks and stream it to playback.
 *  A carry byte bridges the 3-byte base64 quantum to 2-byte samples. */
static void handle_audio_delta(const char *b64)
{
    size_t len = strlen(b64);
    size_t pos = 0;
    while (pos < len) {
        size_t chunk = len - pos;
        if (chunk > LN_RT_B64_CHUNK) {
            chunk = LN_RT_B64_CHUNK;
        }
        size_t off = s_have_carry ? 1 : 0;
        if (off != 0) {
            s_dec_buf[0] = s_carry;
        }
        size_t olen = 0;
        int rc = mbedtls_base64_decode(s_dec_buf + off, LN_RT_DEC_BUF_SZ - off, &olen,
                                       (const unsigned char *)(b64 + pos), chunk);
        if (rc != 0) {
            ESP_LOGW(TAG, "audio delta base64 decode failed (%d)", rc);
            s_have_carry = false;
            return;
        }
        size_t total = off + olen;
        size_t n_samples = total / 2;
        if (n_samples > 0) {
            ln_audio_play((const int16_t *)s_dec_buf, n_samples);
        }
        if ((total & 1U) != 0) {
            s_carry = s_dec_buf[total - 1];
            s_have_carry = true;
        } else {
            s_have_carry = false;
        }
        pos += chunk;
    }
}

static void handle_server_error(const cJSON *root)
{
    const cJSON *e = cJSON_GetObjectItemCaseSensitive(root, "error");
    const char *code = NULL;
    const char *msg = NULL;
    if (cJSON_IsObject(e)) {
        const cJSON *c = cJSON_GetObjectItemCaseSensitive(e, "code");
        const cJSON *m = cJSON_GetObjectItemCaseSensitive(e, "message");
        code = cJSON_IsString(c) ? c->valuestring : NULL;
        msg = cJSON_IsString(m) ? m->valuestring : NULL;
    }
    ESP_LOGW(TAG, "server error: %s — %s", code ? code : "?", msg ? msg : "?");
    post_err(code ? code : "server_error", msg ? msg : "OpenAI realtime error event", false);
}

static void handle_msg(const cJSON *root)
{
    const cJSON *t = cJSON_GetObjectItemCaseSensitive(root, "type");
    if (!cJSON_IsString(t)) {
        return;
    }
    const char *type = t->valuestring;

    /* GA event names first, beta aliases second — same handling. */
    if (strcmp(type, "response.output_audio.delta") == 0 ||
        strcmp(type, "response.audio.delta") == 0) {
        const cJSON *d = cJSON_GetObjectItemCaseSensitive(root, "delta");
        if (cJSON_IsString(d)) {
            handle_audio_delta(d->valuestring);
        }
    } else if (strcmp(type, "response.output_audio_transcript.delta") == 0 ||
               strcmp(type, "response.audio_transcript.delta") == 0) {
        const cJSON *d = cJSON_GetObjectItemCaseSensitive(root, "delta");
        if (cJSON_IsString(d)) {
            post_transcript(d->valuestring, strlen(d->valuestring), false);
        }
    } else if (strcmp(type, "response.output_audio_transcript.done") == 0 ||
               strcmp(type, "response.audio_transcript.done") == 0) {
        const cJSON *d = cJSON_GetObjectItemCaseSensitive(root, "transcript");
        if (cJSON_IsString(d)) {
            post_transcript(d->valuestring, strlen(d->valuestring), true);
        }
    } else if (strcmp(type, "input_audio_buffer.speech_started") == 0) {
        /* Instant local barge-in: kill playback NOW, then tell the server. */
        ln_audio_play_stop();
        s_have_carry = false;
        if (s_response_active) {
            ws_send_str("{\"type\":\"response.cancel\"}");
        }
        post_evt(LN_RT_EVENT_SPEECH_STARTED);
    } else if (strcmp(type, "input_audio_buffer.speech_stopped") == 0) {
        post_evt(LN_RT_EVENT_SPEECH_STOPPED);
    } else if (strcmp(type, "response.created") == 0) {
        s_response_active = true;
        s_have_carry = false;
        post_evt(LN_RT_EVENT_RESPONSE_STARTED);
    } else if (strcmp(type, "response.output_audio.done") == 0 ||
               strcmp(type, "response.audio.done") == 0) {
        ln_audio_play_end(); /* drain any sub-prebuffer tail immediately */
        post_evt(LN_RT_EVENT_RESPONSE_AUDIO_DONE);
    } else if (strcmp(type, "response.done") == 0) {
        s_response_active = false;
        post_evt(LN_RT_EVENT_RESPONSE_DONE);
    } else if (strcmp(type, "session.created") == 0 ||
               strcmp(type, "session.updated") == 0 ||
               strcmp(type, "session.start") == 0) { /* Nova bridge common-schema alias */
        if (!s_session_ready_posted) {
            s_session_ready_posted = true;
            post_evt(LN_RT_EVENT_SESSION_READY);
        }
    } else if (strcmp(type, "error") == 0) {
        handle_server_error(root);
    }
    /* Everything else (rate_limits.updated, conversation.item.*,
     * response.output_item.*, input_audio_buffer.committed, ...) is
     * intentionally ignored on-device. */
}

/* ---------------------------------------------- Gemini Live events (M13) -- */

const char *ln_rt_resumption_handle(void)
{
    return s_resume_handle;
}

/** First model output of a turn — mirror OpenAI's response.created effects
 *  (ln_ui clears the caption, ln_ctrl moves LISTENING->THINKING). Gemini has
 *  no explicit response-started event, so audio/transcript arrival is it. */
static void gemini_mark_response_started(void)
{
    if (!s_response_active) {
        s_response_active = true;
        s_have_carry = false;
        post_evt(LN_RT_EVENT_RESPONSE_STARTED);
    }
}

/** Answer every functionCall in a toolCall with a structured refusal.
 *
 *  Deliberate device parity, NOT a stub: this firmware has no tool router —
 *  the OpenAI path silently ignores its function_call events, and the
 *  device-side POST /api/v1/tools/invoke flow (contracts/api.md) was never
 *  implemented on this surface. Unlike OpenAI, an unanswered Gemini
 *  functionCall stalls the model's turn, so each call gets an immediate
 *  {"error":...} result and the model voices a graceful "can't do that
 *  here". Full on-device tool invocation is a backlog item. */
static void gemini_refuse_tool_calls(const cJSON *calls)
{
    cJSON *frame = cJSON_CreateObject();
    if (frame == NULL) {
        return;
    }
    cJSON *tr = cJSON_AddObjectToObject(frame, "toolResponse");
    cJSON *arr = (tr != NULL) ? cJSON_AddArrayToObject(tr, "functionResponses") : NULL;
    int n = 0;
    const cJSON *call = NULL;
    cJSON_ArrayForEach(call, calls) {
        const cJSON *id = cJSON_GetObjectItemCaseSensitive(call, "id");
        const cJSON *name = cJSON_GetObjectItemCaseSensitive(call, "name");
        if (arr == NULL || !cJSON_IsString(id)) {
            continue;
        }
        cJSON *fr = cJSON_CreateObject();
        if (fr == NULL) {
            continue;
        }
        cJSON_AddStringToObject(fr, "id", id->valuestring);
        if (cJSON_IsString(name)) {
            cJSON_AddStringToObject(fr, "name", name->valuestring);
        }
        cJSON *resp = cJSON_AddObjectToObject(fr, "response");
        cJSON *result = (resp != NULL) ? cJSON_AddObjectToObject(resp, "result") : NULL;
        if (result != NULL) {
            cJSON_AddStringToObject(result, "error",
                                    "tool execution is not available on this device");
        }
        cJSON_AddItemToArray(arr, fr);
        n++;
    }
    if (n > 0) {
        if (s_tool_buf != NULL &&
            cJSON_PrintPreallocated(frame, s_tool_buf, LN_RT_TOOL_BUF_SZ, 0)) {
            if (ws_send_str(s_tool_buf) != ESP_OK) {
                ESP_LOGW(TAG, "toolResponse refusal send failed");
            }
            ESP_LOGW(TAG, "refused %d tool call(s) — no on-device tool router", n);
        } else {
            ESP_LOGE(TAG, "toolResponse refusal frame exceeds %d B — dropped",
                     LN_RT_TOOL_BUF_SZ);
        }
    }
    cJSON_Delete(frame);
}

/** Gemini Live server messages (no OpenAI-style "type" field — each message
 *  is keyed by which top-level member is present). */
static void handle_gemini_msg(const cJSON *root)
{
    /* Readiness: {"setupComplete":{}} acks our setup frame — audio may flow. */
    if (cJSON_GetObjectItemCaseSensitive(root, "setupComplete") != NULL) {
        if (!s_session_ready_posted) {
            s_session_ready_posted = true;
            post_evt(LN_RT_EVENT_SESSION_READY);
        }
        return;
    }

    const cJSON *tc = cJSON_GetObjectItemCaseSensitive(root, "toolCall");
    if (cJSON_IsObject(tc)) {
        gemini_refuse_tool_calls(cJSON_GetObjectItemCaseSensitive(tc, "functionCalls"));
        return;
    }
    /* toolCallCancellation: nothing is ever pending on-device — no-op. */
    if (cJSON_GetObjectItemCaseSensitive(root, "toolCallCancellation") != NULL) {
        return;
    }

    /* Connection lifecycle: the server recycles connections every ~10 min and
     * announces it with goAway. Wake the worker's supervise wait exactly like
     * a link drop: it tears the WSS down and reconnects through a fresh
     * bootstrap (fresh token) carrying s_resume_handle, so the SAME
     * conversation continues (ln_rt_session.c injects the handle). */
    const cJSON *ga = cJSON_GetObjectItemCaseSensitive(root, "goAway");
    if (cJSON_IsObject(ga)) {
        const cJSON *tl = cJSON_GetObjectItemCaseSensitive(ga, "timeLeft");
        ESP_LOGI(TAG, "gemini goAway (timeLeft=%s) — recycling the connection",
                 cJSON_IsString(tl) ? tl->valuestring : "?");
        xEventGroupSetBits(s_eg, EG_WS_DOWN);
        return;
    }
    const cJSON *sru = cJSON_GetObjectItemCaseSensitive(root, "sessionResumptionUpdate");
    if (cJSON_IsObject(sru)) {
        const cJSON *h = cJSON_GetObjectItemCaseSensitive(sru, "newHandle");
        if (cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(sru, "resumable")) &&
            cJSON_IsString(h) && h->valuestring[0] != '\0') {
            strlcpy(s_resume_handle, h->valuestring, sizeof(s_resume_handle));
        }
        return;
    }

    const cJSON *sc = cJSON_GetObjectItemCaseSensitive(root, "serverContent");
    if (!cJSON_IsObject(sc)) {
        return; /* usageMetadata-only frames etc. — ignored on-device */
    }

    /* Barge-in: auto VAD heard the user over playback. There is no
     * client->server cancel primitive on Gemini — flush locally only, and
     * surface it as SPEECH_STARTED (same UI/state effect as OpenAI's
     * input_audio_buffer.speech_started barge-in). */
    if (cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(sc, "interrupted"))) {
        ln_audio_play_stop();
        s_have_carry = false;
        s_response_active = false;
        post_evt(LN_RT_EVENT_SPEECH_STARTED);
    }

    /* Assistant audio: serverContent.modelTurn.parts[].inlineData.data —
     * base64 pcm16 @ 24 kHz, the same downlink rate/decoder as the other
     * engines. */
    const cJSON *mt = cJSON_GetObjectItemCaseSensitive(sc, "modelTurn");
    if (cJSON_IsObject(mt)) {
        const cJSON *parts = cJSON_GetObjectItemCaseSensitive(mt, "parts");
        const cJSON *part = NULL;
        cJSON_ArrayForEach(part, parts) {
            const cJSON *inl = cJSON_GetObjectItemCaseSensitive(part, "inlineData");
            if (!cJSON_IsObject(inl)) {
                continue;
            }
            const cJSON *data = cJSON_GetObjectItemCaseSensitive(inl, "data");
            if (!cJSON_IsString(data) || data->valuestring[0] == '\0') {
                continue;
            }
            gemini_mark_response_started();
            handle_audio_delta(data->valuestring);
        }
    }

    /* Assistant transcript deltas (outputTranscription.text). Never posted
     * with final=true: Gemini has no full-transcript recap event, and a
     * final chunk REPLACES the ln_ui caption — an empty final would wipe it.
     * inputTranscription (user speech) has no on-device consumer; the other
     * engines emit assistant transcripts only, so parity keeps it ignored. */
    const cJSON *ot = cJSON_GetObjectItemCaseSensitive(sc, "outputTranscription");
    if (cJSON_IsObject(ot)) {
        const cJSON *txt = cJSON_GetObjectItemCaseSensitive(ot, "text");
        if (cJSON_IsString(txt) && txt->valuestring[0] != '\0') {
            gemini_mark_response_started();
            post_transcript(txt->valuestring, strlen(txt->valuestring), false);
        }
    }

    /* Turn teardown: generationComplete = all audio generated (drain the
     * playback tail now), turnComplete = the assistant turn is over. Both
     * may arrive in one message; play_end is an idempotent flag. */
    if (cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(sc, "generationComplete"))) {
        ln_audio_play_end();
        post_evt(LN_RT_EVENT_RESPONSE_AUDIO_DONE);
    }
    if (cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(sc, "turnComplete"))) {
        ln_audio_play_end();
        s_response_active = false;
        post_evt(LN_RT_EVENT_RESPONSE_DONE);
    }
}

static bool rx_reserve(size_t needed)
{
    if (needed <= s_rx_cap) {
        return true;
    }
    if (needed > LN_RT_RX_BUF_MAX) {
        return false;
    }
    size_t new_cap = s_rx_cap;
    while (new_cap < needed) {
        new_cap *= 2;
    }
    if (new_cap > LN_RT_RX_BUF_MAX) {
        new_cap = LN_RT_RX_BUF_MAX;
    }
    char *nb = heap_caps_realloc(s_rx_buf, new_cap, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
    if (nb == NULL) {
        return false;
    }
    s_rx_buf = nb;
    s_rx_cap = new_cap;
    return true;
}

static void handle_rx(const esp_websocket_event_data_t *d)
{
    if (d->data_len <= 0 || d->data_ptr == NULL) {
        return;
    }
    if (!rx_reserve(s_rx_len + (size_t)d->data_len)) {
        ESP_LOGE(TAG, "RX message exceeds %d bytes — dropping", LN_RT_RX_BUF_MAX);
        s_rx_len = 0;
        return;
    }
    memcpy(s_rx_buf + s_rx_len, d->data_ptr, (size_t)d->data_len);
    s_rx_len += (size_t)d->data_len;

    /* payload_offset/payload_len track one WS frame across TCP reads. Only try
     * to parse once the frame is complete; if the JSON is still incomplete the
     * message was WS-fragmented (continuation frames follow) — keep buffering. */
    if (d->payload_offset + d->data_len < d->payload_len) {
        return;
    }
    cJSON *root = cJSON_ParseWithLength(s_rx_buf, s_rx_len);
    if (root == NULL) {
        if (s_rx_len > LN_RT_RX_BUF_MAX / 2) {
            ESP_LOGW(TAG, "unparseable oversized RX buffer (%u B) — resetting",
                     (unsigned)s_rx_len);
            s_rx_len = 0;
        }
        return;
    }
    s_rx_len = 0;
    if (s_engine_mode == LN_RT_ENGINE_GEMINI_DIRECT) {
        handle_gemini_msg(root);
    } else {
        handle_msg(root);
    }
    cJSON_Delete(root);
}

/* ------------------------------------------------------------- WS client -- */

static void ws_event_handler(void *arg, esp_event_base_t base, int32_t event_id,
                             void *event_data)
{
    esp_websocket_event_data_t *d = (esp_websocket_event_data_t *)event_data;
    switch (event_id) {
    case WEBSOCKET_EVENT_CONNECTED: {
        s_rx_len = 0;
        s_have_carry = false;
        s_response_active = false;
        s_session_ready_posted = false;
        r32_reset(); /* fresh session — drop stale uplink filter history */
        if (s_up_sb != NULL) {
            xStreamBufferReset(s_up_sb); /* stale mic audio must not lead the turn */
        }
        s_up_dropped = 0;
        /* No session.update here (GA API). A WSS GA session already defaults
         * to audio/pcm @ 24 kHz both directions, and the broker's mint is
         * config-bound (turn_detection/noise_reduction/transcription live in
         * audio.input). GA session.update REPLACES the whole audio.input
         * object, so a format-only update would silently wipe that minted
         * config — and the old beta-shape update (input_audio_format) was
         * rejected by GA sessions with an error event anyway. */
        s_connected = true;
        if (s_engine_mode == LN_RT_ENGINE_GEMINI_DIRECT) {
            /* Gemini DOES require a client-sent frame: the broker-staged
             * {"setup":...} (config also locked into the token via
             * liveConnectConstraints — sending it client-side too is the
             * documented workaround for the constraints-only
             * systemInstruction bug). Readiness gates on the server's
             * setupComplete ack in handle_gemini_msg. Sending from this
             * handler is safe: the client lock is recursive and this runs
             * on the client task. */
            if (s_setup_frame != NULL && ws_send_str(s_setup_frame) == ESP_OK) {
                ESP_LOGI(TAG, "gemini setup frame sent (%u B)",
                         (unsigned)strlen(s_setup_frame));
            } else {
                ESP_LOGE(TAG, "gemini setup frame send failed — server will close");
            }
        }
        xEventGroupSetBits(s_eg, EG_WS_CONNECTED);
        post_evt(LN_RT_EVENT_CONNECTED);
        break;
    }
    case WEBSOCKET_EVENT_DATA:
        /* text / continuation; Gemini Live additionally delivers its JSON in
         * BINARY frames (op 0x02 — browsers see Blobs), so parse those like
         * text on that engine. */
        if (d->op_code == 0x01 || d->op_code == 0x00 ||
            (d->op_code == 0x02 && s_engine_mode == LN_RT_ENGINE_GEMINI_DIRECT)) {
            handle_rx(d);
        } else if (d->op_code == 0x08) {
            ESP_LOGI(TAG, "server sent close frame");
        }
        break;
    case WEBSOCKET_EVENT_ERROR:
    case WEBSOCKET_EVENT_DISCONNECTED:
    case WEBSOCKET_EVENT_CLOSED:
        s_connected = false;
        xEventGroupSetBits(s_eg, EG_WS_DOWN);
        break;
    default:
        break;
    }
}

/** Tear down the WS client (idempotent). Takes the send mutex so no sender
 *  can touch a dying handle. */
static void ws_teardown(void)
{
    esp_websocket_client_handle_t ws;
    xSemaphoreTake(s_send_mtx, portMAX_DELAY);
    ws = s_ws;
    s_ws = NULL;
    s_connected = false;
    xSemaphoreGive(s_send_mtx);
    if (ws != NULL) {
        (void)esp_websocket_client_close(ws, pdMS_TO_TICKS(1000));
        esp_websocket_client_destroy(ws);
    }
}

/** Percent-encode src into dst as an RFC 3986 query value (everything but
 *  unreserved chars escaped — matches Go's url.QueryEscape treatment of the
 *  token in the proven Phase 0 spike; Gemini access tokens contain '/').
 *  Returns false if dst (cap incl. NUL) would overflow. */
static bool url_escape_into(char *dst, size_t cap, const char *src)
{
    static const char k_hex[] = "0123456789ABCDEF";
    size_t o = 0;
    for (const unsigned char *p = (const unsigned char *)src; *p != '\0'; p++) {
        unsigned char c = *p;
        bool unreserved = (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
                          (c >= '0' && c <= '9') ||
                          c == '-' || c == '.' || c == '_' || c == '~';
        if (unreserved) {
            if (o + 1 >= cap) {
                return false;
            }
            dst[o++] = (char)c;
        } else {
            if (o + 3 >= cap) {
                return false;
            }
            dst[o++] = '%';
            dst[o++] = k_hex[c >> 4];
            dst[o++] = k_hex[c & 0x0F];
        }
    }
    dst[o] = '\0';
    return true;
}

static esp_err_t ws_open(const ln_rt_session_info_t *si)
{
    const char *headers = NULL;
    s_engine_mode = si->mode;
    s_setup_frame = NULL;

    if (si->mode == LN_RT_ENGINE_NOVA_BRIDGE) {
        /* Connect straight to the backend Nova Sonic bridge. Auth is the
         * single-use bridge token carried in the URL query string (WS upgrade
         * requests can't reliably carry a Bearer header across every client
         * stack — contracts/api.md). If the broker returned the token as a
         * separate field and it isn't already in the URL, append it. */
        if (si->token[0] != '\0' && strstr(si->ws_url, "token=") == NULL) {
            const char *sep = (strchr(si->ws_url, '?') != NULL) ? "&" : "?";
            snprintf(s_ws_url, sizeof(s_ws_url), "%s%stoken=%s", si->ws_url, sep, si->token);
        } else {
            strlcpy(s_ws_url, si->ws_url, sizeof(s_ws_url));
        }
        s_ws_headers[0] = '\0';
        headers = NULL;
    } else if (si->mode == LN_RT_ENGINE_GEMINI_DIRECT) {
        /* Client-direct WSS to Gemini Live. Ephemeral tokens are only honored
         * by the v1alpha *Constrained* endpoint with the token URL-escaped in
         * an access_token query param — no Authorization header, no
         * subprotocol (live-verified, gemini-plan.md §10 correction 2). */
        int n = snprintf(s_ws_url, sizeof(s_ws_url), "%s?access_token=", si->ws_url);
        if (n < 0 || (size_t)n >= sizeof(s_ws_url) ||
            !url_escape_into(s_ws_url + n, sizeof(s_ws_url) - (size_t)n, si->token)) {
            ESP_LOGE(TAG, "gemini WSS URL exceeds %u B", (unsigned)sizeof(s_ws_url));
            return ESP_FAIL;
        }
        if (si->setup_frame == NULL) {
            ESP_LOGE(TAG, "gemini bootstrap carried no setup frame");
            return ESP_FAIL;
        }
        s_setup_frame = si->setup_frame;
        s_ws_headers[0] = '\0';
        headers = NULL;
    } else {
        snprintf(s_ws_url, sizeof(s_ws_url), LN_RT_WS_URL_FMT, si->model);
        snprintf(s_ws_headers, sizeof(s_ws_headers), "Authorization: Bearer %s\r\n", si->token);
        headers = s_ws_headers;
    }

    esp_websocket_client_config_t cfg = {
        .uri = s_ws_url,
        .headers = headers,
        .crt_bundle_attach = esp_crt_bundle_attach,
        .buffer_size = 4096,
        .task_stack = LN_RT_WS_TASK_STACK,
        .network_timeout_ms = 10000,
        .disable_auto_reconnect = true, /* we own reconnect + fresh-token policy */
        .ping_interval_sec = 15,
    };

    xEventGroupClearBits(s_eg, EG_WS_CONNECTED | EG_WS_DOWN);

    esp_websocket_client_handle_t ws = esp_websocket_client_init(&cfg);
    if (ws == NULL) {
        return ESP_FAIL;
    }
    esp_websocket_register_events(ws, WEBSOCKET_EVENT_ANY, ws_event_handler, NULL);

    xSemaphoreTake(s_send_mtx, portMAX_DELAY);
    s_ws = ws;
    xSemaphoreGive(s_send_mtx);

    if (esp_websocket_client_start(ws) != ESP_OK) {
        return ESP_FAIL;
    }
    EventBits_t bits = xEventGroupWaitBits(s_eg, EG_WS_CONNECTED | EG_WS_DOWN | EG_STOP_REQ,
                                           pdFALSE, pdFALSE,
                                           pdMS_TO_TICKS(LN_RT_CONNECT_TIMEOUT_MS));
    if ((bits & EG_WS_CONNECTED) == 0) {
        return ESP_FAIL;
    }
    xEventGroupClearBits(s_eg, EG_WS_CONNECTED);
    return ESP_OK;
}

/* ------------------------------------------------------------ worker task -- */

static int backoff_ms(int attempt)
{
    int ms = 1000 << (attempt - 1); /* 1s, 2s, 4s, 8s, 16s */
    return (ms > 30000) ? 30000 : ms;
}

static void run_session(void)
{
    int attempt = 0;

    /* A START command is a brand-new conversation — never resume a previous
     * Gemini session across it. Reconnects WITHIN this loop keep the handle
     * (that is the goAway/link-drop resume path). */
    s_resume_handle[0] = '\0';

    while (s_should_run) {
        if (attempt > 0) {
            if (attempt > LN_RT_MAX_RECONNECT) {
                post_err("reconnect_exhausted", "Gave up reconnecting to the realtime session",
                         true);
                break;
            }
            int delay = backoff_ms(attempt);
            ln_rt_reconnect_info_t ri = {
                .attempt = attempt,
                .max_attempts = LN_RT_MAX_RECONNECT,
                .delay_ms = delay,
            };
            esp_event_post(LN_RT_EVENT, LN_RT_EVENT_RECONNECTING, &ri, sizeof(ri),
                           pdMS_TO_TICKS(50));
            EventBits_t b = xEventGroupWaitBits(s_eg, EG_STOP_REQ, pdFALSE, pdFALSE,
                                               pdMS_TO_TICKS(delay));
            if ((b & EG_STOP_REQ) != 0 || !s_should_run) {
                break;
            }
        }

        post_evt(LN_RT_EVENT_CONNECTING);

        ln_rt_session_info_t si;
        ln_rt_error_info_t ei;
        if (ln_rt_session_fetch(&si, &ei) != ESP_OK) {
            esp_event_post(LN_RT_EVENT, LN_RT_EVENT_ERROR, &ei, sizeof(ei), pdMS_TO_TICKS(50));
            if (ei.fatal) {
                break;
            }
            attempt++;
            continue;
        }

        if (ws_open(&si) != ESP_OK) {
            ws_teardown();
            post_err("ws_connect", "Could not open the realtime WebSocket", false);
            attempt++;
            continue;
        }
        /* Ephemeral token consumed; scrub it. */
        memset(&si, 0, sizeof(si));

        /* Connected. Supervise until the link drops or a stop is requested. */
        attempt = 0;
        EventBits_t b = xEventGroupWaitBits(s_eg, EG_WS_DOWN | EG_STOP_REQ, pdFALSE, pdFALSE,
                                            portMAX_DELAY);
        ws_teardown();
        if ((b & EG_STOP_REQ) != 0 || !s_should_run) {
            break;
        }
        ESP_LOGW(TAG, "realtime link dropped — reconnecting");
        attempt = 1;
    }

    ws_teardown();
    s_should_run = false;
    post_evt(LN_RT_EVENT_DISCONNECTED);
}

static void ln_rt_task(void *arg)
{
    ln_rt_cmd_t cmd;
    for (;;) {
        if (xQueueReceive(s_cmd_q, &cmd, portMAX_DELAY) != pdTRUE) {
            continue;
        }
        if (cmd != LN_RT_CMD_START) {
            continue;
        }
        xEventGroupClearBits(s_eg, EG_STOP_REQ | EG_WS_CONNECTED | EG_WS_DOWN);
        s_should_run = true;
        run_session();
    }
}

/* ------------------------------------------------------------- public API -- */

static void *ln_rt_alloc(size_t sz)
{
    void *p = heap_caps_malloc(sz, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
    if (p == NULL) {
        p = malloc(sz);
    }
    return p;
}

esp_err_t ln_realtime_init(void)
{
    if (s_task != NULL) {
        return ESP_ERR_INVALID_STATE;
    }
    s_cmd_q = xQueueCreate(2, sizeof(ln_rt_cmd_t));
    s_eg = xEventGroupCreate();
    s_send_mtx = xSemaphoreCreateMutex();
    s_rx_buf = ln_rt_alloc(LN_RT_RX_BUF_INIT);
    s_rx_cap = LN_RT_RX_BUF_INIT;
    s_dec_buf = ln_rt_alloc(LN_RT_DEC_BUF_SZ);
    s_uplink_buf = ln_rt_alloc(LN_RT_UPLINK_BUF_SZ);
    s_rs_buf = ln_rt_alloc((3 * LN_RT_RESAMPLE_IN_MAX / 2 + 2) * sizeof(int16_t));
    s_up_storage = ln_rt_alloc(LN_RT_UP_SB_BYTES);
    s_tool_buf = ln_rt_alloc(LN_RT_TOOL_BUF_SZ);
    if (s_cmd_q == NULL || s_eg == NULL || s_send_mtx == NULL || s_rx_buf == NULL ||
        s_dec_buf == NULL || s_uplink_buf == NULL || s_rs_buf == NULL ||
        s_up_storage == NULL || s_tool_buf == NULL) {
        ESP_LOGE(TAG, "init: out of memory");
        return ESP_ERR_NO_MEM;
    }
    s_up_sb = xStreamBufferCreateStatic(LN_RT_UP_SB_BYTES, 1, s_up_storage,
                                        &s_up_sb_struct);
    if (s_up_sb == NULL) {
        return ESP_ERR_NO_MEM;
    }
    if (xTaskCreate(ln_rt_task, "ln_rt", LN_RT_TASK_STACK, NULL, LN_RT_TASK_PRIO,
                    &s_task) != pdPASS) {
        return ESP_ERR_NO_MEM;
    }
    if (xTaskCreate(ln_rt_uplink_task, "ln_rt_up", LN_RT_UP_TASK_STACK, NULL,
                    LN_RT_UP_TASK_PRIO, NULL) != pdPASS) {
        return ESP_ERR_NO_MEM;
    }
    ESP_LOGI(TAG, "initialized (backend %s)", CONFIG_LN_RT_BACKEND_BASE_URL);
    return ESP_OK;
}

esp_err_t ln_realtime_start(void)
{
    if (s_task == NULL) {
        return ESP_ERR_INVALID_STATE;
    }
    if (s_should_run) {
        return ESP_ERR_INVALID_STATE;
    }
    ln_rt_cmd_t cmd = LN_RT_CMD_START;
    return (xQueueSend(s_cmd_q, &cmd, 0) == pdTRUE) ? ESP_OK : ESP_FAIL;
}

esp_err_t ln_realtime_stop(void)
{
    if (s_task == NULL) {
        return ESP_ERR_INVALID_STATE;
    }
    s_should_run = false;
    s_connected = false; /* stop uplink immediately */
    xEventGroupSetBits(s_eg, EG_STOP_REQ);
    return ESP_OK;
}

bool ln_realtime_is_connected(void)
{
    return s_connected;
}

bool ln_realtime_is_running(void)
{
    return s_should_run;
}
