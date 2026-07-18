/*
 * ln_ctrl.c — Live Ninja Tab5 control state machine (main/, plan.md M5).
 *
 * Event-driven: consumes LN_NET / LN_WAKE / LN_RT / LN_IOT / LN_UI events on
 * the default esp_event loop plus a 250 ms tick timer, and owns every
 * transition of the M5 state diagram:
 *
 *   Boot -> Provisioning (portal/pairing raised) | Idle (paired + auth ready)
 *   Idle -> Listening (wake word / manual trigger)
 *   Listening -> Thinking (end of turn) -> Speaking (response audio)
 *   Speaking -> Listening (barge-in: wake word, Stop tap, or server VAD)
 *   Speaking -> Idle (response done + playback drained)
 *   Listening -> Idle (cancel / timeout)
 *   Idle <-> Config (settings)
 *   Listening/Thinking/Speaking -> Error (link down / fatal realtime error)
 *   Error -> Idle (reconnected) | Provisioning (auth invalid)
 *
 * Audio wiring owned here:
 *   - uplink: ln_wake AFE-processed 16 kHz frames -> ln_realtime_send_audio()
 *     while a session is active (Listening/Thinking/Speaking),
 *   - mic level: raw capture peak -> LN_AUDIO_MIC_LEVEL posts (<= 20 Hz) for
 *     the Listening visualizer,
 *   - downlink + barge-in live inside ln_realtime/ln_audio.
 *
 * Settings ("ln_ctrl" NVS namespace): voice, sensitivity, wake word/engine,
 * turn detection, voice engine, device name, brightness, settingsVersion —
 * applied from the IoT `config` shadow delta (contracts/shadow.md,
 * higher-version-wins) and reported back in full after each apply.
 * The shadow-driven wake-MODEL swap (wakeword-manifest mapping) is an M6
 * task; M5 stores the wakeWord id and keeps the flashed WakeNet model.
 */
#include <math.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"

#include "esp_app_desc.h"
#include "esp_event.h"
#include "esp_log.h"
#include "esp_mac.h"
#include "esp_system.h"
#include "esp_timer.h"
#include "nvs.h"
#include "nvs_flash.h"

#include "bsp/m5stack_tab5.h"

#include "ln_audio.h"
#include "ln_ctrl.h"
#include "ln_events.h"
#include "ln_iot.h"
#include "ln_net.h"
#include "ln_realtime.h"
#include "ln_ui.h"
#include "ln_wake.h"

static const char *TAG = "ln_ctrl";

#define LN_CTRL_NVS_NS          "ln_ctrl"
#define LN_CTRL_TICK_US         (250 * 1000)
#define LN_CTRL_LISTEN_TIMEOUT_US   (25LL * 1000 * 1000)
#define LN_CTRL_THINK_TIMEOUT_US    (30LL * 1000 * 1000)
#define LN_CTRL_SESSION_LINGER_US   (60LL * 1000 * 1000)
#define LN_CTRL_ERROR_RETRY_US      (10LL * 1000 * 1000)
#define LN_CTRL_MIC_LEVEL_FRAMES    5   /* 5 x 10 ms = 20 Hz posts */

/* ------------------------------------------------------------- settings */

typedef struct {
    int32_t settings_version;
    char    voice[24];
    float   sensitivity;
    char    wake_word[48];
    char    wake_engine[24];
    char    turn_detection[24];
    char    voice_engine[32];
    char    device_name[33];
    uint8_t brightness;
} ln_ctrl_cfg_t;

static ln_ctrl_cfg_t s_cfg = {
    .settings_version = 0,
    .voice = "cedar",
    .sensitivity = 0.5f,
    .wake_word = "hey-live-ninja",
    .wake_engine = "wakenet",
    .turn_detection = "semantic_vad",
    .voice_engine = "openai-realtime",
    .device_name = "Live Ninja",
    .brightness = 80,
};

/* ---------------------------------------------------------------- state */

static SemaphoreHandle_t s_mx;
static volatile ln_app_state_t s_state = LN_STATE_BOOT;
static int64_t s_state_since_us;
static bool s_resp_done_pending;   /* response.done seen, waiting for drain */
static esp_timer_handle_t s_tick;
static int s_err_cd_last = -1;     /* last "Auto-retry in N s" value shown */

/* ------------------------------------------------------------ NVS load  */

static void cfg_get_str(nvs_handle_t h, const char *key, char *dst, size_t cap)
{
    size_t len = cap;
    char tmp[64];
    if (len > sizeof(tmp)) {
        len = sizeof(tmp);
    }
    if (nvs_get_str(h, key, tmp, &len) == ESP_OK && tmp[0] != '\0') {
        strlcpy(dst, tmp, cap);
    }
}

static void cfg_load(void)
{
    nvs_handle_t h;
    if (nvs_open(LN_CTRL_NVS_NS, NVS_READONLY, &h) != ESP_OK) {
        return;
    }
    nvs_get_i32(h, "sver", &s_cfg.settings_version);
    cfg_get_str(h, "voice", s_cfg.voice, sizeof(s_cfg.voice));
    cfg_get_str(h, "wword", s_cfg.wake_word, sizeof(s_cfg.wake_word));
    cfg_get_str(h, "wengine", s_cfg.wake_engine, sizeof(s_cfg.wake_engine));
    cfg_get_str(h, "turndet", s_cfg.turn_detection, sizeof(s_cfg.turn_detection));
    cfg_get_str(h, "vengine", s_cfg.voice_engine, sizeof(s_cfg.voice_engine));
    cfg_get_str(h, "name", s_cfg.device_name, sizeof(s_cfg.device_name));
    nvs_get_u8(h, "bright", &s_cfg.brightness);
    uint32_t sens_bits;
    if (nvs_get_u32(h, "sens", &sens_bits) == ESP_OK) {
        float f;
        memcpy(&f, &sens_bits, sizeof(f));
        if (f >= 0.0f && f <= 1.0f) {
            s_cfg.sensitivity = f;
        }
    }
    nvs_close(h);
}

static void cfg_save(void)
{
    nvs_handle_t h;
    if (nvs_open(LN_CTRL_NVS_NS, NVS_READWRITE, &h) != ESP_OK) {
        ESP_LOGW(TAG, "cfg_save: nvs_open failed");
        return;
    }
    nvs_set_i32(h, "sver", s_cfg.settings_version);
    nvs_set_str(h, "voice", s_cfg.voice);
    nvs_set_str(h, "wword", s_cfg.wake_word);
    nvs_set_str(h, "wengine", s_cfg.wake_engine);
    nvs_set_str(h, "turndet", s_cfg.turn_detection);
    nvs_set_str(h, "vengine", s_cfg.voice_engine);
    nvs_set_str(h, "name", s_cfg.device_name);
    nvs_set_u8(h, "bright", s_cfg.brightness);
    uint32_t sens_bits;
    memcpy(&sens_bits, &s_cfg.sensitivity, sizeof(sens_bits));
    nvs_set_u32(h, "sens", sens_bits);
    nvs_commit(h);
    nvs_close(h);
}

/* --------------------------------------------------------- state change */

static const char *state_str(ln_app_state_t st)
{
    switch (st) {
    case LN_STATE_BOOT:         return "boot";
    case LN_STATE_PROVISIONING: return "provisioning";
    case LN_STATE_IDLE:         return "idle";
    case LN_STATE_LISTENING:    return "listening";
    case LN_STATE_THINKING:     return "thinking";
    case LN_STATE_SPEAKING:     return "speaking";
    case LN_STATE_CONFIG:       return "config";
    case LN_STATE_ERROR:        return "error";
    default:                    return "unknown";
    }
}

/* Called with s_mx held. Posts LN_CTRL_STATE_CHANGED (ln_ui reacts). */
static void set_state(ln_app_state_t st)
{
    if (st == s_state) {
        return;
    }
    ln_evt_state_changed_t ev = { .state = st, .prev = s_state };
    ESP_LOGI(TAG, "state %s -> %s", state_str(ev.prev), state_str(st));
    s_state = st;
    s_state_since_us = esp_timer_get_time();
    ln_iot_set_app_state(state_str(st));
    esp_event_post(LN_CTRL_EVENT, LN_CTRL_STATE_CHANGED, &ev, sizeof(ev), 0);

    /* Error-screen auto-retry countdown: seed it on entry, clear on exit
     * (the tick keeps it updated once per second while in Error). */
    if (ev.prev == LN_STATE_ERROR) {
        ln_ui_error_countdown(-1);
    }
    if (st == LN_STATE_ERROR) {
        s_err_cd_last = (int)(LN_CTRL_ERROR_RETRY_US / 1000000);
        ln_ui_error_countdown(s_err_cd_last);
    }
}

static bool in_session(ln_app_state_t st)
{
    return st == LN_STATE_LISTENING || st == LN_STATE_THINKING ||
           st == LN_STATE_SPEAKING;
}

/* -------------------------------------------------------- shadow report */

static void shadow_report_current(void)
{
    if (!ln_iot_is_connected()) {
        return;
    }
    ln_iot_shadow_reported_t rep = {
        .settings_version = s_cfg.settings_version,
        .wake_word = s_cfg.wake_word,
        .wake_engine = s_cfg.wake_engine,
        .sensitivity = s_cfg.sensitivity,
        .voice = s_cfg.voice,
        .turn_detection = s_cfg.turn_detection,
        .voice_engine = s_cfg.voice_engine,
        .wake_model_sha256_applied = NULL, /* model swap arrives in M6 */
    };
    esp_err_t err = ln_iot_shadow_report(&rep);
    if (err != ESP_OK) {
        ESP_LOGW(TAG, "shadow report failed: %s", esp_err_to_name(err));
    }
}

/* ------------------------------------------------------- session control */

static void session_begin_listening(bool from_barge_in)
{
    if (!ln_realtime_is_running()) {
        esp_err_t err = ln_realtime_start();
        if (err != ESP_OK && err != ESP_ERR_INVALID_STATE) {
            ESP_LOGE(TAG, "realtime start failed: %s", esp_err_to_name(err));
            ln_ui_error_show("Can't start a session",
                             "The realtime client could not start. "
                             "Check the network and try again.",
                             "ERR_RT_START");
            set_state(LN_STATE_ERROR);
            return;
        }
    }
    s_resp_done_pending = false;
    ln_ui_user_transcript("", true);
    if (!from_barge_in) {
        ln_ui_assistant_transcript("", true);
    }
    set_state(LN_STATE_LISTENING);
}

/* ------------------------------------------------------- event handlers */

static void on_net_event(void *arg, esp_event_base_t base, int32_t id,
                         void *data)
{
    (void)arg;
    (void)base;
    xSemaphoreTake(s_mx, portMAX_DELAY);
    switch (id) {
    case LN_NET_EVENT_PORTAL_STARTED:
    case LN_NET_EVENT_PAIRING_STARTED:
        if (!in_session(s_state) && s_state != LN_STATE_CONFIG) {
            set_state(LN_STATE_PROVISIONING);
        }
        break;
    case LN_NET_EVENT_WIFI_CONNECTED:
        if (data != NULL) {
            const ln_net_wifi_info_t *w = data;
            ln_ui_set_device_info(NULL, NULL, NULL, w->ip);
        }
        if (ln_net_is_paired() || ln_iot_is_provisioned()) {
            ln_iot_start(); /* idempotent; provisions if bootstrap stored */
        }
        break;
    case LN_NET_EVENT_PAIRED:
        /* Pairing stored the IoT bootstrap material — provision now. */
        ln_iot_start();
        break;
    case LN_NET_EVENT_AUTH_READY:
        if (s_state == LN_STATE_BOOT || s_state == LN_STATE_PROVISIONING ||
            s_state == LN_STATE_ERROR) {
            set_state(LN_STATE_IDLE);
        }
        break;
    case LN_NET_EVENT_AUTH_INVALID:
        ln_realtime_stop();
        set_state(LN_STATE_PROVISIONING); /* net task re-runs pairing */
        break;
    case LN_NET_EVENT_WIFI_DISCONNECTED:
        if (in_session(s_state)) {
            ln_realtime_stop();
            ln_ui_error_show("Connection lost",
                             "Wi-Fi dropped during the conversation. "
                             "Reconnecting…", "ERR_WIFI_DOWN");
            set_state(LN_STATE_ERROR);
        }
        break;
    default:
        break;
    }
    xSemaphoreGive(s_mx);
}

static void on_wake_event(void *arg, esp_event_base_t base, int32_t id,
                          void *data)
{
    (void)arg;
    (void)base;
    if (id != LN_WAKE_EVT_DETECTED) {
        return;
    }
    const ln_wake_detect_evt_t *d = data;
    xSemaphoreTake(s_mx, portMAX_DELAY);
    if (s_state == LN_STATE_IDLE) {
        char attrs[112];
        snprintf(attrs, sizeof(attrs), "{\"model\":\"%s\",\"manual\":%s}",
                 (d != NULL) ? d->model : "",
                 (d != NULL && d->word_index == 0) ? "true" : "false");
        ln_iot_publish_telemetry("wake_word_detected", NULL, attrs);
        session_begin_listening(false);
    } else if (s_state == LN_STATE_SPEAKING) {
        /* Wake word during playback = local barge-in. */
        ln_realtime_barge_in();
        ln_iot_publish_telemetry("barge_in", NULL, "{\"source\":\"wake\"}");
        session_begin_listening(true);
    }
    xSemaphoreGive(s_mx);
}

static void on_rt_event(void *arg, esp_event_base_t base, int32_t id,
                        void *data)
{
    (void)arg;
    (void)base;
    xSemaphoreTake(s_mx, portMAX_DELAY);
    switch (id) {
    case LN_RT_EVENT_SPEECH_STARTED:
        /* Server VAD heard the user; barge-in already executed internally. */
        if (s_state == LN_STATE_SPEAKING || s_state == LN_STATE_THINKING) {
            s_resp_done_pending = false;
            set_state(LN_STATE_LISTENING);
        } else if (s_state == LN_STATE_LISTENING) {
            s_state_since_us = esp_timer_get_time(); /* reset idle timeout */
        }
        break;
    case LN_RT_EVENT_SPEECH_STOPPED:
        if (s_state == LN_STATE_LISTENING) {
            set_state(LN_STATE_THINKING);
        }
        break;
    case LN_RT_EVENT_RESPONSE_STARTED:
        s_resp_done_pending = false;
        if (s_state == LN_STATE_LISTENING) {
            set_state(LN_STATE_THINKING);
        }
        break;
    case LN_RT_EVENT_RESPONSE_DONE:
        s_resp_done_pending = true; /* tick moves to Idle once drained */
        break;
    case LN_RT_EVENT_DISCONNECTED:
        if (in_session(s_state)) {
            ln_ui_error_show("Session ended",
                             "The voice link closed unexpectedly.",
                             "ERR_RT_CLOSED");
            set_state(LN_STATE_ERROR);
        }
        break;
    case LN_RT_EVENT_ERROR:
        if (data != NULL && ((const ln_rt_error_info_t *)data)->fatal &&
            in_session(s_state)) {
            set_state(LN_STATE_ERROR); /* ln_ui already showed the detail */
        }
        break;
    default:
        break;
    }
    xSemaphoreGive(s_mx);
}

static void on_iot_event(void *arg, esp_event_base_t base, int32_t id,
                         void *data)
{
    (void)arg;
    (void)base;
    switch (id) {
    case LN_IOT_EVENT_PROVISIONED:
    case LN_IOT_EVENT_CONNECTED:
        ln_ui_set_device_info(NULL, ln_iot_thing_name(), NULL, NULL);
        if (id == LN_IOT_EVENT_CONNECTED) {
            xSemaphoreTake(s_mx, portMAX_DELAY);
            shadow_report_current(); /* boot/reconnect report (shadow.md) */
            xSemaphoreGive(s_mx);
        }
        break;
    case LN_IOT_EVENT_CONFIG_DELTA:
        if (data != NULL) {
            const ln_iot_config_delta_t *d = data;
            xSemaphoreTake(s_mx, portMAX_DELAY);
            if (d->settings_version <= s_cfg.settings_version) {
                /* Stale/duplicate delta: re-report current state unchanged
                 * (self-healing rule, contracts/shadow.md §2). */
                shadow_report_current();
            } else {
                if (d->has_voice) {
                    strlcpy(s_cfg.voice, d->voice, sizeof(s_cfg.voice));
                }
                if (d->has_sensitivity && d->sensitivity >= 0.0f &&
                    d->sensitivity <= 1.0f) {
                    s_cfg.sensitivity = d->sensitivity;
                }
                if (d->has_wake_word) {
                    strlcpy(s_cfg.wake_word, d->wake_word,
                            sizeof(s_cfg.wake_word));
                    /* Model asset swap (wakeword-manifest fetch + SHA verify
                     * + ln_wake_set_model) is the M6 shadow-driven-model
                     * task; M5 keeps the flashed WakeNet model. */
                }
                if (d->has_wake_engine) {
                    strlcpy(s_cfg.wake_engine, d->wake_engine,
                            sizeof(s_cfg.wake_engine));
                }
                if (d->has_turn_detection) {
                    strlcpy(s_cfg.turn_detection, d->turn_detection,
                            sizeof(s_cfg.turn_detection));
                }
                if (d->has_voice_engine) {
                    strlcpy(s_cfg.voice_engine, d->voice_engine,
                            sizeof(s_cfg.voice_engine));
                }
                s_cfg.settings_version = d->settings_version;
                cfg_save();
                shadow_report_current();
                ESP_LOGI(TAG, "applied config shadow v%ld",
                         (long)s_cfg.settings_version);
            }
            xSemaphoreGive(s_mx);
        }
        break;
    case LN_IOT_EVENT_CONTROL_DOWN:
        if (data != NULL) {
            ln_iot_buf_t *buf = data;
            /* No M5-scope control/down actions are defined beyond the
             * pairing bind (handled inside ln_net); log + release. */
            ESP_LOGI(TAG, "control/down: %.*s", (int)buf->len, buf->data);
            free(buf->data);
        }
        break;
    case LN_IOT_EVENT_OTA_STARTED:
        ln_ui_set_cloud_status("Updating firmware…");
        break;
    case LN_IOT_EVENT_OTA_FAILED:
        ln_ui_set_cloud_status("Connected");
        break;
    case LN_IOT_EVENT_PROVISION_FAILED:
        ESP_LOGE(TAG, "IoT fleet provisioning failed (voice still works; "
                      "will retry on next connect)");
        break;
    default:
        break;
    }
}

static void factory_reset(void)
{
    ESP_LOGW(TAG, "factory reset requested — wiping credentials + settings");
    ln_realtime_stop();
    ln_iot_factory_reset_credentials(true);
    ln_net_clear_provisioning();
    nvs_handle_t h;
    if (nvs_open(LN_CTRL_NVS_NS, NVS_READWRITE, &h) == ESP_OK) {
        nvs_erase_all(h);
        nvs_commit(h);
        nvs_close(h);
    }
    vTaskDelay(pdMS_TO_TICKS(500)); /* let NVS/MQTT settle */
    esp_restart();
}

static void push_config_values(void)
{
    ln_ui_config_t ui_cfg = {
        .volume_pct = ln_audio_get_volume(),
        .brightness_pct = s_cfg.brightness,
        .sensitivity = s_cfg.sensitivity,
    };
    strlcpy(ui_cfg.voice, s_cfg.voice, sizeof(ui_cfg.voice));
    strlcpy(ui_cfg.device_name, s_cfg.device_name, sizeof(ui_cfg.device_name));
    ln_ui_config_values(&ui_cfg);
}

static void on_ui_event(void *arg, esp_event_base_t base, int32_t id,
                        void *data)
{
    (void)arg;
    (void)base;
    xSemaphoreTake(s_mx, portMAX_DELAY);
    switch (id) {
    case LN_UI_STOP_TAPPED:
        if (s_state == LN_STATE_SPEAKING) {
            ln_realtime_barge_in();
            ln_iot_publish_telemetry("barge_in", NULL, "{\"source\":\"touch\"}");
            session_begin_listening(true);
        } else if (s_state == LN_STATE_LISTENING ||
                   s_state == LN_STATE_THINKING) {
            ln_realtime_barge_in();
            set_state(LN_STATE_IDLE);
        }
        break;
    case LN_UI_CANCEL_TAPPED:
        if (in_session(s_state)) {
            ln_realtime_barge_in(); /* cancels any response + flushes audio */
            set_state(LN_STATE_IDLE);
        }
        break;
    case LN_UI_FINISH_TAPPED:
        if (s_state == LN_STATE_LISTENING) {
            set_state(LN_STATE_THINKING); /* server VAD completes the turn */
        }
        break;
    case LN_UI_SETTINGS_OPEN_REQUESTED:
        if (s_state == LN_STATE_IDLE) {
            push_config_values();
            set_state(LN_STATE_CONFIG);
        }
        break;
    case LN_UI_SETTINGS_CLOSED:
        if (s_state == LN_STATE_CONFIG) {
            cfg_save();
            shadow_report_current();
            set_state(LN_STATE_IDLE);
        }
        break;
    case LN_UI_RETRY_TAPPED:
        if (s_state == LN_STATE_ERROR && ln_net_is_online() &&
            ln_net_is_paired()) {
            set_state(LN_STATE_IDLE);
        }
        break;
    case LN_UI_VOICE_SELECTED:
        if (data != NULL) {
            const ln_evt_voice_sel_t *v = data;
            strlcpy(s_cfg.voice, v->voice, sizeof(s_cfg.voice));
            cfg_save();
            shadow_report_current();
        }
        break;
    case LN_UI_VOLUME_CHANGED:
        if (data != NULL) {
            ln_audio_set_volume(((const ln_evt_pct_t *)data)->pct);
        }
        break;
    case LN_UI_BRIGHTNESS_CHANGED:
        if (data != NULL) {
            uint8_t pct = ((const ln_evt_pct_t *)data)->pct;
            if (pct < 5) {
                pct = 5; /* never fully dark — the touch UI is the way back */
            }
            s_cfg.brightness = pct;
            bsp_display_brightness_set(pct);
            cfg_save();
        }
        break;
    case LN_UI_SENSITIVITY_CHANGED:
        if (data != NULL) {
            float v = ((const ln_evt_float_t *)data)->value;
            if (v >= 0.0f && v <= 1.0f) {
                s_cfg.sensitivity = v;
                cfg_save();
                shadow_report_current();
            }
        }
        break;
    case LN_UI_DEVICE_NAME_CHANGED:
        if (data != NULL) {
            const ln_evt_name_t *n = data;
            if (n->name[0] != '\0') {
                strlcpy(s_cfg.device_name, n->name, sizeof(s_cfg.device_name));
                cfg_save();
            }
        }
        break;
    case LN_UI_WIFI_SETUP_REQUESTED:
        ln_realtime_stop();
        ln_net_reprovision_wifi(); /* pairing kept; portal comes back up */
        break;
    case LN_UI_FACTORY_RESET_REQUESTED:
        xSemaphoreGive(s_mx);
        factory_reset(); /* does not return */
        return;
    default:
        break;
    }
    xSemaphoreGive(s_mx);
}

/* ------------------------------------------------------------ audio taps */

/* Raw-capture tap (audio_rx task): peak level -> LN_AUDIO_MIC_LEVEL posts. */
static void mic_level_cb(const int16_t *mic, const int16_t *ref,
                         size_t samples, void *ctx)
{
    (void)ref;
    (void)ctx;
    static int16_t peak;
    static int frames;
    for (size_t i = 0; i < samples; i++) {
        int16_t v = (mic[i] < 0) ? (int16_t)-mic[i] : mic[i];
        if (v > peak) {
            peak = v;
        }
    }
    if (++frames < LN_CTRL_MIC_LEVEL_FRAMES) {
        return;
    }
    frames = 0;
    if (s_state == LN_STATE_LISTENING) {
        /* sqrt curve reads naturally for speech dynamics */
        ln_evt_pct_t p = {
            .pct = (uint8_t)(sqrtf((float)peak / 32768.0f) * 100.0f),
        };
        esp_event_post(LN_AUDIO_EVENT, LN_AUDIO_MIC_LEVEL, &p, sizeof(p), 0);
    }
    peak = 0;
}

/* AFE-processed tap (ww_infer task): uplink to OpenAI while in a session. */
static void uplink_cb(const int16_t *pcm, size_t samples, bool speech,
                      void *ctx)
{
    (void)speech;
    (void)ctx;
    ln_app_state_t st = s_state;
    if (in_session(st) && ln_realtime_is_connected()) {
        ln_realtime_send_audio(pcm, samples);
    }
}

/* ----------------------------------------------------------------- tick */

static void tick_cb(void *arg)
{
    (void)arg;
    if (xSemaphoreTake(s_mx, 0) != pdTRUE) {
        return; /* busy handler; catch up next tick */
    }
    int64_t elapsed = esp_timer_get_time() - s_state_since_us;
    switch (s_state) {
    case LN_STATE_THINKING:
        if (ln_audio_is_playing()) {
            set_state(LN_STATE_SPEAKING);
        } else if (s_resp_done_pending && elapsed > 500 * 1000) {
            /* text/tool-only response — nothing to play */
            s_resp_done_pending = false;
            set_state(LN_STATE_IDLE);
        } else if (elapsed > LN_CTRL_THINK_TIMEOUT_US) {
            ESP_LOGW(TAG, "thinking timed out");
            set_state(LN_STATE_IDLE);
        }
        break;
    case LN_STATE_SPEAKING:
        if (s_resp_done_pending && !ln_audio_is_playing()) {
            s_resp_done_pending = false;
            set_state(LN_STATE_IDLE); /* Speaking -> Idle: response done */
        }
        break;
    case LN_STATE_LISTENING:
        if (elapsed > LN_CTRL_LISTEN_TIMEOUT_US) {
            ESP_LOGI(TAG, "listening timed out; back to idle");
            set_state(LN_STATE_IDLE);
        }
        break;
    case LN_STATE_IDLE:
        /* Keep the WSS session warm briefly for a follow-up wake, then drop
         * it (cost: an ephemeral re-mint on next wake). */
        if (ln_realtime_is_running() && elapsed > LN_CTRL_SESSION_LINGER_US) {
            ln_realtime_stop();
        }
        break;
    case LN_STATE_ERROR:
        if (elapsed > LN_CTRL_ERROR_RETRY_US && ln_net_is_online() &&
            ln_net_is_paired()) {
            set_state(LN_STATE_IDLE); /* Error -> Idle: reconnected */
        } else {
            /* Drive the "Auto-retry in N s" line once per second. When the
             * wait has expired but the network still isn't back, hide it —
             * the retry is then gated on connectivity, not time. */
            int remain = (int)((LN_CTRL_ERROR_RETRY_US - elapsed + 999999)
                               / 1000000);
            if (remain < 0) {
                remain = -1;
            }
            if (remain != s_err_cd_last) {
                s_err_cd_last = remain;
                ln_ui_error_countdown(remain);
            }
        }
        break;
    default:
        break;
    }
    xSemaphoreGive(s_mx);
}

/* ----------------------------------------------------------------- init */

esp_err_t ln_ctrl_start(void)
{
    if (s_mx != NULL) {
        return ESP_ERR_INVALID_STATE;
    }
    s_mx = xSemaphoreCreateMutex();
    if (s_mx == NULL) {
        return ESP_ERR_NO_MEM;
    }

    cfg_load();
    bsp_display_brightness_set(s_cfg.brightness);

    /* Identity for the Config > About panel. */
    const esp_app_desc_t *app = esp_app_get_description();
    uint8_t mac[6] = {0};
    esp_read_mac(mac, ESP_MAC_EFUSE_FACTORY); /* P4 eFuse MAC (no radio here) */
    char mac_str[18];
    snprintf(mac_str, sizeof(mac_str), "%02X:%02X:%02X:%02X:%02X:%02X",
             mac[0], mac[1], mac[2], mac[3], mac[4], mac[5]);
    ln_ui_set_device_info(app->version, ln_iot_thing_name(), mac_str, NULL);
    push_config_values();

    struct {
        esp_event_base_t base;
        esp_event_handler_t fn;
    } subs[] = {
        { LN_NET_EVENT,  on_net_event  },
        { LN_WAKE_EVENT, on_wake_event },
        { LN_RT_EVENT,   on_rt_event   },
        { LN_IOT_EVENT,  on_iot_event  },
        { LN_UI_EVENT,   on_ui_event   },
    };
    for (size_t i = 0; i < sizeof(subs) / sizeof(subs[0]); i++) {
        ESP_ERROR_CHECK(esp_event_handler_register(subs[i].base,
                                                   ESP_EVENT_ANY_ID,
                                                   subs[i].fn, NULL));
    }

    /* Audio taps: raw capture for the level meter, AFE output for uplink. */
    esp_err_t err = ln_audio_capture_subscribe(mic_level_cb, NULL);
    if (err != ESP_OK) {
        ESP_LOGW(TAG, "mic level tap unavailable: %s", esp_err_to_name(err));
    }
    err = ln_wake_audio_subscribe(uplink_cb, NULL);
    if (err != ESP_OK) {
        ESP_LOGW(TAG, "uplink tap unavailable: %s", esp_err_to_name(err));
    }

    const esp_timer_create_args_t targs = {
        .callback = tick_cb,
        .name = "ln_ctrl_tick",
    };
    ESP_ERROR_CHECK(esp_timer_create(&targs, &s_tick));
    ESP_ERROR_CHECK(esp_timer_start_periodic(s_tick, LN_CTRL_TICK_US));

    s_state_since_us = esp_timer_get_time();
    ln_iot_set_app_state("boot");
    ln_ui_set_state(LN_UI_STATE_BOOT);
    ESP_LOGI(TAG, "ctrl up (settings v%ld, voice %s, wake \"%s\")",
             (long)s_cfg.settings_version, s_cfg.voice, s_cfg.wake_word);
    return ESP_OK;
}
