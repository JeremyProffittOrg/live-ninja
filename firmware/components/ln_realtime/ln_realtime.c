/*
 * ln_realtime.c — realtime voice WSS client core (M5Stack Tab5).
 *
 * Two transports behind one lifecycle, chosen by the device's voiceEngine pin
 * (resolved server-side; see ln_rt_session.c):
 *   - OPENAI_DIRECT (default): client-direct WSS to wss://api.openai.com with a
 *     Bearer ephemeral token; a session.update pins pcm16 in/out on connect.
 *   - NOVA_BRIDGE (M12, FR-VE-03): WSS to the backend Nova Sonic media bridge
 *     (nova.live.jeremy.ninja) with a single-use token carried in the URL and
 *     no OpenAI ephemeral / no session.update — the bridge holds the Bedrock
 *     bidirectional stream and speaks the same pcm16 event framing, so uplink
 *     (input_audio_buffer.append), downlink (response.output_audio.delta),
 *     transcripts and barge-in flow through unchanged. HIL-unverified.
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
#define LN_RT_UPLINK_MAX_SAMPLES 4800 /* 300 ms @ 16 kHz -> ~12.8 KB base64 frame */
#define LN_RT_MAX_RECONNECT     5
#define LN_RT_CONNECT_TIMEOUT_MS 15000
#define LN_RT_SEND_TIMEOUT      pdMS_TO_TICKS(1000)
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

static char s_ws_url[1280];   /* OpenAI URL is short; Nova bridge URL (ws_url[640]) + "?token=" + token[512] needs headroom */
static char s_ws_headers[576]; /* "Authorization: Bearer ek_...\r\n" (OpenAI-direct only) */

/* Transport the current session negotiated (set in ws_open, read by the WS
 * event handler on the esp_websocket_client task). Only the OpenAI-direct path
 * sends OpenAI's session.update on connect; the Nova bridge owns session
 * config server-side and would not understand that frame. */
static volatile ln_rt_engine_mode_t s_engine_mode = LN_RT_ENGINE_OPENAI_DIRECT;

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
    static const char k_prefix[] = "{\"type\":\"input_audio_buffer.append\",\"audio\":\"";
    esp_err_t ret = ESP_ERR_INVALID_STATE;

    if (xSemaphoreTake(s_send_mtx, LN_RT_SEND_TIMEOUT) != pdTRUE) {
        return ESP_ERR_TIMEOUT;
    }
    if (s_ws != NULL && s_connected) {
        size_t pos = sizeof(k_prefix) - 1;
        memcpy(s_uplink_buf, k_prefix, pos);
        size_t olen = 0;
        int rc = mbedtls_base64_encode((unsigned char *)s_uplink_buf + pos,
                                       LN_RT_UPLINK_BUF_SZ - pos - 3, &olen,
                                       (const unsigned char *)samples, n_samples * 2);
        if (rc == 0) {
            pos += olen;
            s_uplink_buf[pos++] = '"';
            s_uplink_buf[pos++] = '}';
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
    esp_err_t ret = ESP_OK;
    while (n_samples > 0 && ret == ESP_OK) {
        size_t slice = (n_samples > LN_RT_UPLINK_MAX_SAMPLES) ? LN_RT_UPLINK_MAX_SAMPLES
                                                              : n_samples;
        ret = send_audio_slice(samples, slice);
        samples += slice;
        n_samples -= slice;
    }
    return ret;
}

esp_err_t ln_realtime_barge_in(void)
{
    if (!s_connected) {
        return ESP_ERR_INVALID_STATE;
    }
    ln_audio_play_stop();
    s_have_carry = false;
    if (s_response_active) {
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
    handle_msg(root);
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
        if (s_engine_mode == LN_RT_ENGINE_OPENAI_DIRECT) {
            /* Locked decision: configure pcm16 in/out. The ephemeral token is
             * config-bound by the broker; this only pins the audio wire format.
             * The Nova bridge fixes pcm16 itself and normalizes events
             * server-side, so no OpenAI session.update is sent on that path. */
            static const char k_session_update[] =
                "{\"type\":\"session.update\",\"session\":{"
                "\"input_audio_format\":\"pcm16\",\"output_audio_format\":\"pcm16\"}}";
            esp_websocket_client_send_text(s_ws, k_session_update,
                                           sizeof(k_session_update) - 1, LN_RT_SEND_TIMEOUT);
        }
        s_connected = true;
        xEventGroupSetBits(s_eg, EG_WS_CONNECTED);
        post_evt(LN_RT_EVENT_CONNECTED);
        break;
    }
    case WEBSOCKET_EVENT_DATA:
        if (d->op_code == 0x01 || d->op_code == 0x00) { /* text / continuation */
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

static esp_err_t ws_open(const ln_rt_session_info_t *si)
{
    const char *headers = NULL;
    s_engine_mode = si->mode;

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
    if (s_cmd_q == NULL || s_eg == NULL || s_send_mtx == NULL || s_rx_buf == NULL ||
        s_dec_buf == NULL || s_uplink_buf == NULL) {
        ESP_LOGE(TAG, "init: out of memory");
        return ESP_ERR_NO_MEM;
    }
    if (xTaskCreate(ln_rt_task, "ln_rt", LN_RT_TASK_STACK, NULL, LN_RT_TASK_PRIO,
                    &s_task) != pdPASS) {
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
