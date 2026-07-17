/*
 * Live Ninja — Tab5 audio pipeline implementation.
 *
 * Hardware constraints that shape this file:
 *  - The Tab5 BSP runs ONE I2S port full-duplex (shared MCLK/BCLK/WS between
 *    the ES8388 DAC and the ES7210 ADC), so capture and playback MUST share a
 *    sample rate. We fix the link at 48 kHz / 16-bit / stereo and do all rate
 *    conversion in software (48k -> 16k mic decimation, 24k -> 48k playback
 *    interpolation) — see ln_resample.c.
 *  - ES7210 default routing (esp_codec_dev) enables MIC1+MIC2 onto the I2S
 *    L/R slots; we use MIC1 (left slot) as the primary voice channel.
 *  - There is no hardware echo-loopback channel wired to the ES7210 on this
 *    board config, so the AEC reference is generated in software: every chunk
 *    written to the DAC is also decimated to 16 kHz and queued in a reference
 *    ring that the capture path consumes sample-synchronously (both sides are
 *    clocked by the same I2S master clock, so there is no drift).
 */
#include "ln_audio.h"
#include "ln_resample.h"

#include <string.h>

#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"
#include "freertos/task.h"

#include "esp_check.h"
#include "esp_codec_dev.h"
#include "esp_heap_caps.h"
#include "esp_log.h"
#include "nvs.h"
#include "nvs_flash.h"

#include "bsp/m5stack_tab5.h"
#include "driver/i2s_std.h"

static const char *TAG = "ln_audio";

#define LN_I2S_RATE        48000
#define LN_I2S_CH          2
#define LN_BLOCK_MS        10
#define LN_BLK_48K         (LN_I2S_RATE / 1000 * LN_BLOCK_MS)   /* 480 samples/ch */
#define LN_BLK_24K         (LN_AUDIO_PLAYBACK_SAMPLE_RATE / 1000 * LN_BLOCK_MS) /* 240 */
#define LN_BLK_16K         LN_AUDIO_FRAME_SAMPLES                /* 160 */

#define LN_MAX_SINKS       4

#define LN_NVS_NAMESPACE   "ln_audio"
#define LN_NVS_KEY_VOL     "vol"
#define LN_NVS_KEY_MICMUTE "mic_mute"
#define LN_NVS_KEY_SPKMUTE "spk_mute"

/* ---- state ---------------------------------------------------------------- */

typedef struct {
    ln_audio_capture_cb_t cb;
    void *ctx;
} ln_sink_t;

typedef struct {
    int16_t *buf;          /* mono int16 ring */
    size_t   cap;          /* capacity in samples */
    size_t   head;         /* write index */
    size_t   tail;         /* read index */
    size_t   count;        /* samples stored */
    portMUX_TYPE mux;
} ln_ring_t;

typedef enum {
    PB_IDLE = 0,      /* ring empty, outputting nothing */
    PB_BUFFERING,     /* accumulating up to prebuffer */
    PB_PLAYING,       /* draining */
} pb_state_t;

static struct {
    bool inited;
    esp_codec_dev_handle_t spk;
    esp_codec_dev_handle_t mic;

    ln_sink_t sinks[LN_MAX_SINKS];
    portMUX_TYPE sink_mux;

    /* playback jitter ring (24 kHz mono, PSRAM) */
    ln_ring_t pb_ring;
    SemaphoreHandle_t pb_space_sem;   /* signalled when space freed */
    SemaphoreHandle_t pb_data_sem;    /* signalled when data queued */
    volatile pb_state_t pb_state;
    volatile bool pb_eos;             /* end-of-stream marked */
    volatile uint32_t pb_last_push_tick;
    volatile bool pb_flush;           /* barge-in flush request */

    /* software AEC reference ring (16 kHz mono, PSRAM) */
    ln_ring_t ref_ring;

    /* resampler states */
    ln_dec3_t dec_mic;     /* capture: 48k -> 16k (MIC1) */
    ln_dec3_t dec_ref;     /* playback echo ref: 48k -> 16k */
    ln_itp2_t itp_play;    /* playback: 24k -> 48k */

    volatile uint8_t volume;
    volatile bool mic_mute;
    volatile bool spk_mute;

    TaskHandle_t cap_task;
    TaskHandle_t pb_task;
} s = {
    .sink_mux = portMUX_INITIALIZER_UNLOCKED,
};

/* ---- small SPSC ring helpers (samples) ----------------------------------- */

static esp_err_t ring_init(ln_ring_t *r, size_t cap_samples)
{
    r->buf = heap_caps_malloc(cap_samples * sizeof(int16_t),
                              MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
    ESP_RETURN_ON_FALSE(r->buf, ESP_ERR_NO_MEM, TAG, "ring alloc %u", (unsigned)cap_samples);
    r->cap = cap_samples;
    r->head = r->tail = r->count = 0;
    portMUX_TYPE m = portMUX_INITIALIZER_UNLOCKED;
    r->mux = m;
    return ESP_OK;
}

static size_t ring_push(ln_ring_t *r, const int16_t *src, size_t n)
{
    taskENTER_CRITICAL(&r->mux);
    size_t space = r->cap - r->count;
    if (n > space) {
        n = space;
    }
    size_t first = r->cap - r->head;
    if (first > n) {
        first = n;
    }
    memcpy(&r->buf[r->head], src, first * sizeof(int16_t));
    if (n > first) {
        memcpy(r->buf, src + first, (n - first) * sizeof(int16_t));
    }
    r->head = (r->head + n) % r->cap;
    r->count += n;
    taskEXIT_CRITICAL(&r->mux);
    return n;
}

/* Pop up to n samples; zero-fill the remainder. Returns samples actually popped. */
static size_t ring_pop_zerofill(ln_ring_t *r, int16_t *dst, size_t n)
{
    taskENTER_CRITICAL(&r->mux);
    size_t take = r->count < n ? r->count : n;
    size_t first = r->cap - r->tail;
    if (first > take) {
        first = take;
    }
    memcpy(dst, &r->buf[r->tail], first * sizeof(int16_t));
    if (take > first) {
        memcpy(dst + first, r->buf, (take - first) * sizeof(int16_t));
    }
    r->tail = (r->tail + take) % r->cap;
    r->count -= take;
    taskEXIT_CRITICAL(&r->mux);
    if (take < n) {
        memset(dst + take, 0, (n - take) * sizeof(int16_t));
    }
    return take;
}

static void ring_clear(ln_ring_t *r)
{
    taskENTER_CRITICAL(&r->mux);
    r->head = r->tail = r->count = 0;
    taskEXIT_CRITICAL(&r->mux);
}

static size_t ring_count(ln_ring_t *r)
{
    taskENTER_CRITICAL(&r->mux);
    size_t c = r->count;
    taskEXIT_CRITICAL(&r->mux);
    return c;
}

/* ---- NVS persistence ------------------------------------------------------ */

static void settings_load(void)
{
    s.volume = CONFIG_LN_AUDIO_DEFAULT_VOLUME;
    s.mic_mute = false;
    s.spk_mute = false;

    nvs_handle_t h;
    if (nvs_open(LN_NVS_NAMESPACE, NVS_READONLY, &h) == ESP_OK) {
        uint8_t v;
        if (nvs_get_u8(h, LN_NVS_KEY_VOL, &v) == ESP_OK && v <= 100) {
            s.volume = v;
        }
        if (nvs_get_u8(h, LN_NVS_KEY_MICMUTE, &v) == ESP_OK) {
            s.mic_mute = (v != 0);
        }
        if (nvs_get_u8(h, LN_NVS_KEY_SPKMUTE, &v) == ESP_OK) {
            s.spk_mute = (v != 0);
        }
        nvs_close(h);
    }
}

static esp_err_t settings_save_u8(const char *key, uint8_t val)
{
    nvs_handle_t h;
    esp_err_t err = nvs_open(LN_NVS_NAMESPACE, NVS_READWRITE, &h);
    if (err != ESP_OK) {
        return err;
    }
    err = nvs_set_u8(h, key, val);
    if (err == ESP_OK) {
        err = nvs_commit(h);
    }
    nvs_close(h);
    return err;
}

/* ---- capture path --------------------------------------------------------- */

static void deliver_frame(const int16_t *mic, const int16_t *ref, size_t n)
{
    for (int i = 0; i < LN_MAX_SINKS; i++) {
        ln_audio_capture_cb_t cb;
        void *ctx;
        taskENTER_CRITICAL(&s.sink_mux);
        cb = s.sinks[i].cb;
        ctx = s.sinks[i].ctx;
        taskEXIT_CRITICAL(&s.sink_mux);
        if (cb) {
            cb(mic, ref, n, ctx);
        }
    }
}

static void capture_task(void *arg)
{
    (void)arg;
    /* 10 ms stereo 48k block */
    static int16_t raw[LN_BLK_48K * LN_I2S_CH];
    static int16_t mono48[LN_BLK_48K];
    static int16_t mic16[LN_BLK_16K + 2];
    static int16_t ref16[LN_BLK_16K + 2];

    while (true) {
        int err = esp_codec_dev_read(s.mic, raw, sizeof(raw));
        if (err != ESP_CODEC_DEV_OK) {
            ESP_LOGW(TAG, "mic read err %d", err);
            vTaskDelay(pdMS_TO_TICKS(50));
            continue;
        }
        /* De-interleave: left slot = ES7210 MIC1 (primary). */
        for (int i = 0; i < LN_BLK_48K; i++) {
            mono48[i] = raw[i * LN_I2S_CH];
        }
        size_t n = ln_dec3_process(&s.dec_mic, mono48, LN_BLK_48K, mic16);
        if (n == 0) {
            continue;
        }
        if (n > LN_BLK_16K) {
            n = LN_BLK_16K; /* keep the fixed frame contract */
        }
        if (s.mic_mute) {
            memset(mic16, 0, n * sizeof(int16_t));
        }
        ring_pop_zerofill(&s.ref_ring, ref16, n);
        deliver_frame(mic16, ref16, n);
    }
}

/* ---- playback path -------------------------------------------------------- */

static uint32_t queued_ms(void)
{
    return (uint32_t)(ring_count(&s.pb_ring) * 1000 / LN_AUDIO_PLAYBACK_SAMPLE_RATE);
}

static void playback_write_block(const int16_t *pcm24, size_t n24)
{
    static int16_t up48[LN_BLK_24K * 2];
    static int16_t stereo[LN_BLK_24K * 2 * LN_I2S_CH];
    static int16_t ref16[(LN_BLK_24K * 2) / 3 + 2];

    size_t n48 = ln_itp2_process(&s.itp_play, pcm24, n24, up48);
    for (size_t i = 0; i < n48; i++) {
        stereo[2 * i] = up48[i];
        stereo[2 * i + 1] = up48[i];
    }
    /* Software AEC reference: what we are about to emit, at 16 kHz. When the
     * speaker is muted nothing reaches the air, so reference stays silent. */
    size_t nref = ln_dec3_process(&s.dec_ref, up48, n48, ref16);
    if (nref > 0) {
        if (s.spk_mute) {
            memset(ref16, 0, nref * sizeof(int16_t));
        }
        ring_push(&s.ref_ring, ref16, nref); /* drop-on-full is fine (idle consumer) */
    }
    /* Blocking write paced by I2S DMA. */
    int err = esp_codec_dev_write(s.spk, stereo, n48 * LN_I2S_CH * sizeof(int16_t));
    if (err != ESP_CODEC_DEV_OK) {
        ESP_LOGW(TAG, "spk write err %d", err);
        vTaskDelay(pdMS_TO_TICKS(20));
    }
}

static void playback_task(void *arg)
{
    (void)arg;
    static int16_t chunk[LN_BLK_24K];
    const uint32_t prebuf_ms = CONFIG_LN_AUDIO_JITTER_PREBUFFER_MS;
    /* If the producer stalls mid-buffering this long, play what we have. */
    const uint32_t stall_flush_ms = 60;

    while (true) {
        if (s.pb_flush) {
            ring_clear(&s.pb_ring);
            ln_itp2_reset(&s.itp_play);
            s.pb_flush = false;
            s.pb_eos = false;
            s.pb_state = PB_IDLE;
            xSemaphoreGive(s.pb_space_sem);
        }

        switch (s.pb_state) {
        case PB_IDLE:
            if (ring_count(&s.pb_ring) > 0) {
                s.pb_state = PB_BUFFERING;
            } else {
                xSemaphoreTake(s.pb_data_sem, pdMS_TO_TICKS(100));
            }
            break;

        case PB_BUFFERING: {
            uint32_t q = queued_ms();
            bool stalled = (xTaskGetTickCount() - s.pb_last_push_tick) >
                           pdMS_TO_TICKS(stall_flush_ms);
            if (q >= prebuf_ms || s.pb_eos || (q > 0 && stalled)) {
                s.pb_state = PB_PLAYING;
            } else {
                xSemaphoreTake(s.pb_data_sem, pdMS_TO_TICKS(20));
            }
            break;
        }

        case PB_PLAYING: {
            size_t got = ring_pop_zerofill(&s.pb_ring, chunk, LN_BLK_24K);
            if (got == 0) {
                /* drained */
                if (s.pb_eos) {
                    s.pb_eos = false;
                }
                s.pb_state = PB_IDLE;
                xSemaphoreGive(s.pb_space_sem);
                break;
            }
            /* zero-fill tail already applied; play the full block for a clean
             * fade into silence, then signal space. */
            playback_write_block(chunk, LN_BLK_24K);
            xSemaphoreGive(s.pb_space_sem);
            break;
        }
        }
    }
}

/* ---- public API ----------------------------------------------------------- */

esp_err_t ln_audio_init(void)
{
    if (s.inited) {
        return ESP_OK;
    }

    settings_load();

    /* The BSP default I2S config is 48 kHz MONO; we need STEREO (MIC1+MIC2 on
     * L/R for capture, L/R duplicated for the DAC), so pass our own config to
     * bsp_audio_init() BEFORE the codec helpers call it with defaults. */
    const i2s_std_config_t i2s_cfg = {
        .clk_cfg = I2S_STD_CLK_DEFAULT_CONFIG(LN_I2S_RATE),
        .slot_cfg = I2S_STD_PHILIP_SLOT_DEFAULT_CONFIG(I2S_DATA_BIT_WIDTH_16BIT,
                                                       I2S_SLOT_MODE_STEREO),
        .gpio_cfg = {
            .mclk = BSP_I2S_MCLK,
            .bclk = BSP_I2S_SCLK,
            .ws = BSP_I2S_LCLK,
            .dout = BSP_I2S_DOUT,
            .din = BSP_I2S_DSIN,
            .invert_flags = { .mclk_inv = false, .bclk_inv = false, .ws_inv = false },
        },
    };
    ESP_RETURN_ON_ERROR(bsp_i2c_init(), TAG, "i2c init");
    ESP_RETURN_ON_ERROR(bsp_audio_init(&i2s_cfg), TAG, "i2s init");

    s.spk = bsp_audio_codec_speaker_init();
    ESP_RETURN_ON_FALSE(s.spk, ESP_FAIL, TAG, "speaker codec init failed");
    s.mic = bsp_audio_codec_microphone_init();
    ESP_RETURN_ON_FALSE(s.mic, ESP_FAIL, TAG, "mic codec init failed");

    esp_codec_dev_sample_info_t fs = {
        .sample_rate = LN_I2S_RATE,
        .channel = LN_I2S_CH,
        .bits_per_sample = 16,
    };
    ESP_RETURN_ON_FALSE(esp_codec_dev_open(s.spk, &fs) == ESP_CODEC_DEV_OK,
                        ESP_FAIL, TAG, "spk open");
    ESP_RETURN_ON_FALSE(esp_codec_dev_open(s.mic, &fs) == ESP_CODEC_DEV_OK,
                        ESP_FAIL, TAG, "mic open");

    esp_codec_dev_set_out_vol(s.spk, s.volume);
    esp_codec_dev_set_out_mute(s.spk, s.spk_mute);
    esp_codec_dev_set_in_gain(s.mic, (float)CONFIG_LN_AUDIO_MIC_GAIN_DB);
    esp_codec_dev_set_in_mute(s.mic, s.mic_mute);

    /* rings */
    size_t pb_cap = (size_t)LN_AUDIO_PLAYBACK_SAMPLE_RATE *
                    CONFIG_LN_AUDIO_PLAYBACK_RING_MS / 1000;
    ESP_RETURN_ON_ERROR(ring_init(&s.pb_ring, pb_cap), TAG, "pb ring");
    ESP_RETURN_ON_ERROR(ring_init(&s.ref_ring, LN_AUDIO_CAPTURE_SAMPLE_RATE), TAG,
                        "ref ring"); /* 1 s */

    s.pb_space_sem = xSemaphoreCreateBinary();
    s.pb_data_sem = xSemaphoreCreateBinary();
    ESP_RETURN_ON_FALSE(s.pb_space_sem && s.pb_data_sem, ESP_ERR_NO_MEM, TAG, "sems");

    ln_dec3_reset(&s.dec_mic);
    ln_dec3_reset(&s.dec_ref);
    ln_itp2_reset(&s.itp_play);
    s.pb_state = PB_IDLE;
    s.pb_last_push_tick = xTaskGetTickCount();

    BaseType_t ok;
    ok = xTaskCreatePinnedToCore(capture_task, "audio_rx", 4096, NULL,
                                 CONFIG_LN_AUDIO_CAPTURE_TASK_PRIORITY,
                                 &s.cap_task, CONFIG_LN_AUDIO_TASK_CORE);
    ESP_RETURN_ON_FALSE(ok == pdPASS, ESP_ERR_NO_MEM, TAG, "audio_rx task");
    ok = xTaskCreatePinnedToCore(playback_task, "audio_tx", 4096, NULL,
                                 CONFIG_LN_AUDIO_PLAYBACK_TASK_PRIORITY,
                                 &s.pb_task, CONFIG_LN_AUDIO_TASK_CORE);
    ESP_RETURN_ON_FALSE(ok == pdPASS, ESP_ERR_NO_MEM, TAG, "audio_tx task");

    s.inited = true;
    ESP_LOGI(TAG, "audio up: I2S 48k/16/stereo duplex, capture 16k mono, "
                  "playback 24k mono, prebuffer %d ms, vol %d%%",
             CONFIG_LN_AUDIO_JITTER_PREBUFFER_MS, s.volume);
    return ESP_OK;
}

esp_err_t ln_audio_capture_subscribe(ln_audio_capture_cb_t cb, void *ctx)
{
    ESP_RETURN_ON_FALSE(cb, ESP_ERR_INVALID_ARG, TAG, "null cb");
    esp_err_t err = ESP_ERR_NO_MEM;
    taskENTER_CRITICAL(&s.sink_mux);
    for (int i = 0; i < LN_MAX_SINKS; i++) {
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

esp_err_t ln_audio_capture_unsubscribe(ln_audio_capture_cb_t cb, void *ctx)
{
    esp_err_t err = ESP_ERR_NOT_FOUND;
    taskENTER_CRITICAL(&s.sink_mux);
    for (int i = 0; i < LN_MAX_SINKS; i++) {
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

esp_err_t ln_audio_play(const int16_t *pcm, size_t samples)
{
    ESP_RETURN_ON_FALSE(s.inited, ESP_ERR_INVALID_STATE, TAG, "not inited");
    ESP_RETURN_ON_FALSE(pcm || samples == 0, ESP_ERR_INVALID_ARG, TAG, "null pcm");

    /* First audio of a burst: pre-load the echo-reference delay padding so the
     * reference roughly lines up with the acoustic echo (DAC+DMA latency). */
    if (s.pb_state == PB_IDLE && ring_count(&s.pb_ring) == 0) {
        static int16_t zeros[LN_AUDIO_CAPTURE_SAMPLE_RATE / 1000 * 8];
        size_t pad = (size_t)LN_AUDIO_CAPTURE_SAMPLE_RATE *
                     CONFIG_LN_AUDIO_AEC_REF_DELAY_MS / 1000;
        memset(zeros, 0, sizeof(zeros));
        while (pad > 0) {
            size_t n = pad < (sizeof(zeros) / sizeof(zeros[0]))
                           ? pad : (sizeof(zeros) / sizeof(zeros[0]));
            ring_push(&s.ref_ring, zeros, n);
            pad -= n;
        }
    }

    s.pb_eos = false;
    while (samples > 0) {
        size_t pushed = ring_push(&s.pb_ring, pcm, samples);
        s.pb_last_push_tick = xTaskGetTickCount();
        if (pushed > 0) {
            xSemaphoreGive(s.pb_data_sem);
        }
        pcm += pushed;
        samples -= pushed;
        if (samples > 0) {
            /* ring full: wait for the player to free space (backpressure). */
            if (xSemaphoreTake(s.pb_space_sem, pdMS_TO_TICKS(2000)) != pdTRUE) {
                ESP_LOGW(TAG, "playback ring full, dropped %u samples",
                         (unsigned)samples);
                return ESP_ERR_TIMEOUT;
            }
        }
    }
    return ESP_OK;
}

void ln_audio_play_end(void)
{
    s.pb_eos = true;
    xSemaphoreGive(s.pb_data_sem);
}

esp_err_t ln_audio_play_stop(void)
{
    ESP_RETURN_ON_FALSE(s.inited, ESP_ERR_INVALID_STATE, TAG, "not inited");
    s.pb_flush = true;
    xSemaphoreGive(s.pb_data_sem);
    ring_clear(&s.ref_ring); /* stop feeding stale echo reference too */
    return ESP_OK;
}

bool ln_audio_is_playing(void)
{
    return s.pb_state == PB_PLAYING;
}

uint32_t ln_audio_play_queued_ms(void)
{
    return s.inited ? queued_ms() : 0;
}

esp_err_t ln_audio_set_volume(uint8_t pct)
{
    ESP_RETURN_ON_FALSE(pct <= 100, ESP_ERR_INVALID_ARG, TAG, "vol %u", pct);
    s.volume = pct;
    if (s.inited) {
        esp_codec_dev_set_out_vol(s.spk, pct);
    }
    return settings_save_u8(LN_NVS_KEY_VOL, pct);
}

uint8_t ln_audio_get_volume(void)
{
    return s.volume;
}

esp_err_t ln_audio_set_mic_mute(bool mute)
{
    s.mic_mute = mute;
    if (s.inited) {
        esp_codec_dev_set_in_mute(s.mic, mute);
    }
    return settings_save_u8(LN_NVS_KEY_MICMUTE, mute ? 1 : 0);
}

bool ln_audio_get_mic_mute(void)
{
    return s.mic_mute;
}

esp_err_t ln_audio_set_spk_mute(bool mute)
{
    s.spk_mute = mute;
    if (s.inited) {
        esp_codec_dev_set_out_mute(s.spk, mute);
    }
    return settings_save_u8(LN_NVS_KEY_SPKMUTE, mute ? 1 : 0);
}

bool ln_audio_get_spk_mute(void)
{
    return s.spk_mute;
}
