/*
 * Live Ninja — wake-word + AFE pipeline implementation (ESP-SR v2 on ESP32-P4).
 *
 * Data flow:
 *   ln_audio capture cb (audio_rx task, 10 ms frames)
 *     -> interleave [mic, ref] per CONFIG_LN_WAKE_INPUT_FORMAT ("MR")
 *     -> StreamBuffer
 *     -> ww_feed task -> afe->feed()
 *   AFE internal SE task (created by ESP-SR on CONFIG_LN_WAKE_TASK_CORE)
 *   ww_infer task -> afe->fetch_with_delay()
 *     -> WakeNet state  -> LN_WAKE_EVT_DETECTED
 *     -> VAD transitions -> LN_WAKE_EVT_VAD_SPEECH / _SILENCE
 *     -> processed 16 kHz mono chunk -> subscribers (realtime uplink)
 *
 * Model selection is by name from NVS ("ln_wake"/"model"), matched against the
 * "model" flash partition via esp_srmodel_filter — fully model-agnostic so the
 * M6 custom "Hey Live Ninja" WakeNet drops in by name change only.
 */
#include "ln_wake.h"

#include <string.h>

#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"
#include "freertos/stream_buffer.h"
#include "freertos/task.h"

#include "esp_check.h"
#include "esp_heap_caps.h"
#include "esp_log.h"
#include "nvs.h"

#include "esp_afe_config.h"
#include "esp_afe_sr_iface.h"
#include "esp_afe_sr_models.h"
#include "esp_wn_models.h"
#include "model_path.h"

#include "ln_audio.h"

static const char *TAG = "ln_wake";

ESP_EVENT_DEFINE_BASE(LN_WAKE_EVENT);

#define LN_WAKE_NVS_NAMESPACE "ln_wake"
#define LN_WAKE_NVS_KEY_MODEL "model"

#define LN_WAKE_MAX_SINKS 4

/* Energy-VAD fallback tuning (only used when ESP-SR cannot start). */
#define LN_EVAD_ON_RMS        900   /* ~ -31 dBFS */
#define LN_EVAD_ON_FRAMES     3     /* 30 ms above threshold -> speech */
#define LN_EVAD_OFF_FRAMES    60    /* 600 ms below threshold -> silence */

typedef struct {
    ln_wake_audio_cb_t cb;
    void *ctx;
} ln_wake_sink_t;

static struct {
    bool inited;
    bool fallback;            /* no WakeNet model (AFE may still run) */
    bool afe_active;          /* false => energy-VAD last resort */
    char model[LN_WAKE_MODEL_NAME_MAX];

    srmodel_list_t *models;
    const esp_afe_sr_iface_t *afe;
    esp_afe_sr_data_t *afe_data;
    int feed_chunk;           /* samples per channel per feed */
    int feed_nch;             /* channels expected by feed */

    StreamBufferHandle_t feed_sb;
    int16_t *feed_buf;        /* ww_feed working buffer, feed_chunk*nch */
    int16_t *ilv_buf;         /* capture-cb interleave buffer */
    size_t   ilv_fill;        /* samples (per channel) accumulated in ilv_buf */

    TaskHandle_t feed_task;
    TaskHandle_t infer_task;
    volatile bool run;
    SemaphoreHandle_t feed_done;
    SemaphoreHandle_t infer_done;

    ln_wake_sink_t sinks[LN_WAKE_MAX_SINKS];
    portMUX_TYPE sink_mux;

    volatile bool detect_enabled;
    bool vad_speech;          /* last published VAD state */

    /* energy-VAD fallback state */
    int evad_above;
    int evad_below;
} s = {
    .sink_mux = portMUX_INITIALIZER_UNLOCKED,
    .detect_enabled = true,
};

/* ---- helpers -------------------------------------------------------------- */

static void post_event(ln_wake_event_id_t id, const void *data, size_t size)
{
    esp_err_t err = esp_event_post(LN_WAKE_EVENT, id, (void *)data, size,
                                   pdMS_TO_TICKS(50));
    if (err != ESP_OK) {
        ESP_LOGW(TAG, "event %d post failed (%s)", (int)id, esp_err_to_name(err));
    }
}

static void post_ready(void)
{
    ln_wake_ready_evt_t evt = {
        .wakenet_active = !s.fallback,
        .afe_active = s.afe_active,
    };
    strlcpy(evt.model, s.model, sizeof(evt.model));
    post_event(s.fallback ? LN_WAKE_EVT_FALLBACK : LN_WAKE_EVT_READY,
               &evt, sizeof(evt));
}

static void post_detected(int word_index)
{
    ln_wake_detect_evt_t evt = { .word_index = word_index };
    strlcpy(evt.model, s.model, sizeof(evt.model));
    post_event(LN_WAKE_EVT_DETECTED, &evt, sizeof(evt));
}

static void publish_vad(bool speech)
{
    if (speech != s.vad_speech) {
        s.vad_speech = speech;
        post_event(speech ? LN_WAKE_EVT_VAD_SPEECH : LN_WAKE_EVT_VAD_SILENCE,
                   NULL, 0);
    }
}

static void deliver_audio(const int16_t *pcm, size_t samples, bool speech)
{
    for (int i = 0; i < LN_WAKE_MAX_SINKS; i++) {
        ln_wake_audio_cb_t cb;
        void *ctx;
        taskENTER_CRITICAL(&s.sink_mux);
        cb = s.sinks[i].cb;
        ctx = s.sinks[i].ctx;
        taskEXIT_CRITICAL(&s.sink_mux);
        if (cb) {
            cb(pcm, samples, speech, ctx);
        }
    }
}

static void model_name_load(char *out, size_t out_len)
{
    strlcpy(out, CONFIG_LN_WAKE_DEFAULT_MODEL, out_len);
    nvs_handle_t h;
    if (nvs_open(LN_WAKE_NVS_NAMESPACE, NVS_READONLY, &h) == ESP_OK) {
        size_t len = out_len;
        char tmp[LN_WAKE_MODEL_NAME_MAX];
        len = sizeof(tmp);
        if (nvs_get_str(h, LN_WAKE_NVS_KEY_MODEL, tmp, &len) == ESP_OK && tmp[0]) {
            strlcpy(out, tmp, out_len);
        }
        nvs_close(h);
    }
}

/* ---- capture-side: ln_audio sink ------------------------------------------ */

static void capture_sink(const int16_t *mic, const int16_t *ref, size_t samples,
                         void *ctx)
{
    (void)ctx;
    if (!s.run) {
        return;
    }

    if (!s.afe_active) {
        /* Energy-VAD fallback: no AFE — VAD directly on the mic frame and
         * pass raw mic audio through to subscribers. */
        uint64_t acc = 0;
        for (size_t i = 0; i < samples; i++) {
            acc += (int32_t)mic[i] * mic[i];
        }
        int rms = 0;
        if (samples > 0) {
            uint32_t mean = (uint32_t)(acc / samples);
            /* integer sqrt */
            uint32_t x = mean, y = 0, b = 1u << 30;
            while (b > x) {
                b >>= 2;
            }
            while (b) {
                if (x >= y + b) {
                    x -= y + b;
                    y = (y >> 1) + b;
                } else {
                    y >>= 1;
                }
                b >>= 2;
            }
            rms = (int)y;
        }
        bool speech = s.vad_speech;
        if (rms >= LN_EVAD_ON_RMS) {
            s.evad_below = 0;
            if (++s.evad_above >= LN_EVAD_ON_FRAMES) {
                speech = true;
            }
        } else {
            s.evad_above = 0;
            if (++s.evad_below >= LN_EVAD_OFF_FRAMES) {
                speech = false;
            }
        }
        publish_vad(speech);
        deliver_audio(mic, samples, speech);
        return;
    }

    /* Interleave into AFE feed layout and forward whole chunks. Layout is
     * driven by feed_nch: 2 => [mic, ref] (input format "MR"); 1 => mic only. */
    for (size_t i = 0; i < samples; i++) {
        size_t base = s.ilv_fill * s.feed_nch;
        s.ilv_buf[base] = mic[i];
        if (s.feed_nch > 1) {
            s.ilv_buf[base + 1] = ref[i];
        }
        s.ilv_fill++;
        if (s.ilv_fill == (size_t)s.feed_chunk) {
            size_t bytes = (size_t)s.feed_chunk * s.feed_nch * sizeof(int16_t);
            size_t sent = xStreamBufferSend(s.feed_sb, s.ilv_buf, bytes, 0);
            if (sent != bytes) {
                /* Feed backlog (AFE stalled) — drop oldest by resetting; the
                 * AFE tolerates a gap far better than audio_rx blocking. */
                xStreamBufferReset(s.feed_sb);
                ESP_LOGW(TAG, "AFE feed backlog, dropped a chunk");
            }
            s.ilv_fill = 0;
        }
    }
}

/* ---- tasks ---------------------------------------------------------------- */

static void ww_feed_task(void *arg)
{
    (void)arg;
    const size_t chunk_bytes = (size_t)s.feed_chunk * s.feed_nch * sizeof(int16_t);
    size_t have = 0;

    while (s.run) {
        size_t got = xStreamBufferReceive(s.feed_sb, ((uint8_t *)s.feed_buf) + have,
                                          chunk_bytes - have, pdMS_TO_TICKS(100));
        have += got;
        if (have < chunk_bytes) {
            continue;
        }
        have = 0;
        s.afe->feed(s.afe_data, s.feed_buf);
    }
    xSemaphoreGive(s.feed_done);
    vTaskDelete(NULL);
}

static void ww_infer_task(void *arg)
{
    (void)arg;
    while (s.run) {
        afe_fetch_result_t *res =
            s.afe->fetch_with_delay(s.afe_data, pdMS_TO_TICKS(200));
        if (!res || res->ret_value != ESP_OK || !s.run) {
            continue;
        }

        bool speech = (res->vad_state == VAD_SPEECH);
        publish_vad(speech);

        if (res->wakeup_state == WAKENET_DETECTED && s.detect_enabled) {
            ESP_LOGI(TAG, "wake word detected (model %s, word %d)",
                     s.model, res->wake_word_index);
            post_detected(res->wake_word_index > 0 ? res->wake_word_index : 1);
        }

        if (res->data && res->data_size > 0) {
            deliver_audio(res->data, (size_t)res->data_size / sizeof(int16_t),
                          speech);
        }
    }
    xSemaphoreGive(s.infer_done);
    vTaskDelete(NULL);
}

/* ---- pipeline bring-up / teardown ----------------------------------------- */

static esp_err_t pipeline_start(void)
{
    s.fallback = false;
    s.afe_active = false;
    s.vad_speech = false;
    s.evad_above = s.evad_below = 0;

    char want[LN_WAKE_MODEL_NAME_MAX];
    model_name_load(want, sizeof(want));

    s.models = esp_srmodel_init("model");
    char *wn_name = NULL;
    if (s.models) {
        wn_name = esp_srmodel_filter(s.models, ESP_WN_PREFIX, want);
        if (!wn_name) {
            ESP_LOGW(TAG, "model '%s' not in partition, trying any WakeNet", want);
            wn_name = esp_srmodel_filter(s.models, ESP_WN_PREFIX, NULL);
        }
    } else {
        ESP_LOGE(TAG, "no 'model' partition / no models — energy-VAD fallback");
    }

    if (s.models) {
        afe_config_t *cfg = afe_config_init(CONFIG_LN_WAKE_INPUT_FORMAT, s.models,
                                            AFE_TYPE_SR, AFE_MODE_HIGH_PERF);
        if (cfg) {
            if (wn_name) {
                cfg->wakenet_init = true;
                cfg->wakenet_model_name = wn_name;
            } else {
                cfg->wakenet_init = false;
                s.fallback = true;
            }
            cfg->vad_init = true;
            cfg->afe_perferred_core = CONFIG_LN_WAKE_TASK_CORE;
            cfg->afe_perferred_priority = CONFIG_LN_WAKE_FETCH_TASK_PRIORITY;
            cfg->memory_alloc_mode = AFE_MEMORY_ALLOC_MORE_PSRAM;
            afe_config_check(cfg);

            s.afe = esp_afe_handle_from_config(cfg);
            if (s.afe) {
                s.afe_data = s.afe->create_from_config(cfg);
            }
            if (s.afe_data) {
                s.feed_chunk = s.afe->get_feed_chunksize(s.afe_data);
                s.feed_nch = s.afe->get_feed_channel_num(s.afe_data);
                s.afe_active = true;
                if (wn_name) {
                    strlcpy(s.model, wn_name, sizeof(s.model));
                }
                ESP_LOGI(TAG, "AFE up: fmt=%s chunk=%d nch=%d wakenet=%s vad=on aec=%s",
                         CONFIG_LN_WAKE_INPUT_FORMAT, s.feed_chunk, s.feed_nch,
                         wn_name ? wn_name : "OFF (fallback)",
                         cfg->aec_init ? "on" : "off");
            } else {
                ESP_LOGE(TAG, "AFE create failed — energy-VAD fallback");
            }
            afe_config_free(cfg);
        } else {
            ESP_LOGE(TAG, "afe_config_init failed — energy-VAD fallback");
        }
    }

    if (!s.afe_active) {
        /* Last resort: energy VAD directly in the capture callback. */
        s.fallback = true;
        s.model[0] = '\0';
        s.run = true;
        ESP_RETURN_ON_ERROR(ln_audio_capture_subscribe(capture_sink, NULL), TAG,
                            "subscribe");
        post_ready();
        return ESP_OK;
    }

    /* AFE path: stream buffer + tasks. */
    size_t chunk_bytes = (size_t)s.feed_chunk * s.feed_nch * sizeof(int16_t);
    s.feed_sb = xStreamBufferCreate(chunk_bytes * 6, 1);
    s.feed_buf = heap_caps_malloc(chunk_bytes, MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT);
    s.ilv_buf = heap_caps_malloc(chunk_bytes, MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT);
    ESP_RETURN_ON_FALSE(s.feed_sb && s.feed_buf && s.ilv_buf, ESP_ERR_NO_MEM, TAG,
                        "feed buffers");
    s.ilv_fill = 0;
    s.run = true;

    BaseType_t ok = xTaskCreatePinnedToCore(ww_feed_task, "ww_feed", 4096, NULL,
                                            CONFIG_LN_WAKE_FEED_TASK_PRIORITY,
                                            &s.feed_task, CONFIG_LN_WAKE_TASK_CORE);
    ESP_RETURN_ON_FALSE(ok == pdPASS, ESP_ERR_NO_MEM, TAG, "ww_feed task");
    ok = xTaskCreatePinnedToCore(ww_infer_task, "ww_infer", 8192, NULL,
                                 CONFIG_LN_WAKE_FETCH_TASK_PRIORITY,
                                 &s.infer_task, CONFIG_LN_WAKE_TASK_CORE);
    ESP_RETURN_ON_FALSE(ok == pdPASS, ESP_ERR_NO_MEM, TAG, "ww_infer task");

    ESP_RETURN_ON_ERROR(ln_audio_capture_subscribe(capture_sink, NULL), TAG,
                        "subscribe");
    post_ready();
    return ESP_OK;
}

static void pipeline_stop(void)
{
    ln_audio_capture_unsubscribe(capture_sink, NULL);

    if (s.run) {
        s.run = false;
        if (s.feed_task) {
            xSemaphoreTake(s.feed_done, pdMS_TO_TICKS(1000));
            s.feed_task = NULL;
        }
        if (s.infer_task) {
            xSemaphoreTake(s.infer_done, pdMS_TO_TICKS(2000));
            s.infer_task = NULL;
        }
    }
    if (s.afe && s.afe_data) {
        s.afe->destroy(s.afe_data);
    }
    s.afe = NULL;
    s.afe_data = NULL;
    if (s.models) {
        esp_srmodel_deinit(s.models);
        s.models = NULL;
    }
    if (s.feed_sb) {
        vStreamBufferDelete(s.feed_sb);
        s.feed_sb = NULL;
    }
    free(s.feed_buf);
    s.feed_buf = NULL;
    free(s.ilv_buf);
    s.ilv_buf = NULL;
    s.afe_active = false;
}

/* ---- public API ----------------------------------------------------------- */

esp_err_t ln_wake_init(void)
{
    if (s.inited) {
        return ESP_OK;
    }
    if (!s.feed_done) {
        s.feed_done = xSemaphoreCreateBinary();
        s.infer_done = xSemaphoreCreateBinary();
        ESP_RETURN_ON_FALSE(s.feed_done && s.infer_done, ESP_ERR_NO_MEM, TAG, "sems");
    }
    esp_err_t err = pipeline_start();
    if (err != ESP_OK) {
        pipeline_stop();
        return err;
    }
    s.inited = true;
    return ESP_OK;
}

esp_err_t ln_wake_deinit(void)
{
    if (!s.inited) {
        return ESP_OK;
    }
    pipeline_stop();
    s.inited = false;
    return ESP_OK;
}

esp_err_t ln_wake_enable(bool enable)
{
    ESP_RETURN_ON_FALSE(s.inited, ESP_ERR_INVALID_STATE, TAG, "not inited");
    s.detect_enabled = enable;
    if (s.afe_active && !s.fallback) {
        int r = enable ? s.afe->enable_wakenet(s.afe_data)
                       : s.afe->disable_wakenet(s.afe_data);
        return (r == 1) ? ESP_OK : ESP_FAIL;
    }
    return ESP_OK;
}

esp_err_t ln_wake_trigger(void)
{
    ESP_RETURN_ON_FALSE(s.inited, ESP_ERR_INVALID_STATE, TAG, "not inited");
    ESP_LOGI(TAG, "manual wake trigger");
    post_detected(0);
    return ESP_OK;
}

esp_err_t ln_wake_audio_subscribe(ln_wake_audio_cb_t cb, void *ctx)
{
    ESP_RETURN_ON_FALSE(cb, ESP_ERR_INVALID_ARG, TAG, "null cb");
    esp_err_t err = ESP_ERR_NO_MEM;
    taskENTER_CRITICAL(&s.sink_mux);
    for (int i = 0; i < LN_WAKE_MAX_SINKS; i++) {
        if (s.sinks[i].cb == NULL) {
            s.sinks[i].cb = cb;
            s.sinks[i].ctx = ctx;
            err = ESP_OK;
            break;
        }
    }
    taskEXIT_CRITICAL(&s.sink_mux);
    return err;
}

esp_err_t ln_wake_audio_unsubscribe(ln_wake_audio_cb_t cb, void *ctx)
{
    esp_err_t err = ESP_ERR_NOT_FOUND;
    taskENTER_CRITICAL(&s.sink_mux);
    for (int i = 0; i < LN_WAKE_MAX_SINKS; i++) {
        if (s.sinks[i].cb == cb && s.sinks[i].ctx == ctx) {
            s.sinks[i].cb = NULL;
            s.sinks[i].ctx = NULL;
            err = ESP_OK;
            break;
        }
    }
    taskEXIT_CRITICAL(&s.sink_mux);
    return err;
}

const char *ln_wake_model_name(void)
{
    return s.model;
}

esp_err_t ln_wake_set_model(const char *name)
{
    ESP_RETURN_ON_FALSE(name && name[0] && strlen(name) < LN_WAKE_MODEL_NAME_MAX,
                        ESP_ERR_INVALID_ARG, TAG, "bad model name");

    nvs_handle_t h;
    ESP_RETURN_ON_ERROR(nvs_open(LN_WAKE_NVS_NAMESPACE, NVS_READWRITE, &h), TAG,
                        "nvs open");
    esp_err_t err = nvs_set_str(h, LN_WAKE_NVS_KEY_MODEL, name);
    if (err == ESP_OK) {
        err = nvs_commit(h);
    }
    nvs_close(h);
    ESP_RETURN_ON_ERROR(err, TAG, "nvs write");

    if (!s.inited) {
        return ESP_OK; /* applied on next init */
    }

    /* Live swap: restart the pipeline against the new name. */
    pipeline_stop();
    err = pipeline_start();
    if (err != ESP_OK) {
        return err;
    }
    if (strcmp(s.model, name) != 0) {
        ESP_LOGW(TAG, "model '%s' not found; running '%s'", name,
                 s.model[0] ? s.model : "(none)");
        return ESP_ERR_NOT_FOUND;
    }
    return ESP_OK;
}

bool ln_wake_fallback_active(void)
{
    return s.fallback;
}
