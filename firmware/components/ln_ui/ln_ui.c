/*
 * ln_ui.c — core of the Live Ninja LVGL UI.
 *
 * Owns: screen registry + state switching, transcript accumulation, the
 * esp_event subscriptions (LN_CTRL/LN_NET/LN_AUDIO/LN_WAKE/LN_RT/LN_IOT) and
 * the public setter API. Screen modules render; this file routes data.
 *
 * Locking: every public entry point takes the LVGL port lock via
 * bsp_display_lock() (recursive mutex — safe from LVGL callbacks too).
 * Event handlers run on the default esp_event loop task and call the same
 * public setters.
 */
#include <stdio.h>
#include <string.h>

#include "esp_app_desc.h"
#include "esp_log.h"
#include "esp_mac.h"

#include "bsp/m5stack_tab5.h"

#include "ln_iot.h"
#include "ln_net.h"
#include "ln_realtime.h"
#include "ln_wake.h"

#include "ln_ui_internal.h"

static const char *TAG = "ln_ui";

#define LN_UI_LOCK_MS 1000

static bool s_ready;
static ln_ui_state_t s_state = LN_UI_STATE_BOOT;
static lv_obj_t *s_screens[LN_UI_STATE_ERROR + 1];
static lv_timer_t *s_clock_timer;

/* transcript accumulation (user question / assistant answer) */
static char s_user_txt[1024];
static char s_asst_txt[2048];

/* ------------------------------------------------------------------ */
/* small helpers                                                       */
/* ------------------------------------------------------------------ */

void ln_ui_post(int32_t id, const void *data, size_t size)
{
    esp_err_t err = esp_event_post(LN_UI_EVENT, id, (void *)data, size, 0);
    if (err != ESP_OK && err != ESP_ERR_TIMEOUT) {
        /* no default loop yet (pre-ctrl bring-up) — intent is dropped but
         * the UI itself stays consistent; log at debug to avoid spam */
        ESP_LOGD(TAG, "ln_ui_post(%d) skipped: %s", (int)id,
                 esp_err_to_name(err));
    }
}

static bool ui_lock(void)
{
    if (!s_ready) {
        return false;
    }
    if (!bsp_display_lock(LN_UI_LOCK_MS)) {
        ESP_LOGW(TAG, "LVGL lock timeout");
        return false;
    }
    return true;
}

static void append_bounded(char *dst, size_t cap, const char *frag)
{
    size_t len = strlen(dst);
    size_t add = strlen(frag);
    if (len + add + 1 > cap) {
        /* keep the tail (most recent text) — shift out the oldest half */
        size_t keep = cap / 2;
        if (len > keep) {
            memmove(dst, dst + (len - keep), keep + 1);
            len = keep;
        }
        if (add > cap - 1 - len) {
            add = cap - 1 - len;
        }
    }
    memcpy(dst + len, frag, add);
    dst[len + add] = '\0';
}

static void clock_timer_cb(lv_timer_t *t)
{
    (void)t;
    /* runs in the LVGL task — lock already held by the port */
    if (s_state == LN_UI_STATE_IDLE) {
        ln_scr_idle_tick();
    }
}

/* ------------------------------------------------------------------ */
/* esp_event handlers (default loop task)                              */
/* ------------------------------------------------------------------ */

static void on_ctrl_event(void *arg, esp_event_base_t base, int32_t id,
                          void *data)
{
    (void)arg;
    (void)base;
    if (id == LN_CTRL_STATE_CHANGED && data != NULL) {
        const ln_evt_state_changed_t *ev = data;
        ln_ui_set_state((ln_ui_state_t)ev->state);
    }
}

static const char *rssi_word(int rssi)
{
    if (rssi == 0) {
        return "OK";
    }
    if (rssi >= -60) {
        return "Strong";
    }
    if (rssi >= -75) {
        return "OK";
    }
    return "Weak";
}

/* Contract: ln_net.h (ln_net_event_id_t + payload structs). */
static void on_net_event(void *arg, esp_event_base_t base, int32_t id,
                         void *data)
{
    (void)arg;
    (void)base;
    char buf[160];

    switch (id) {
    case LN_NET_EVENT_WIFI_CONNECTING:
        if (data != NULL) {
            snprintf(buf, sizeof(buf), "Connecting to %s…",
                     (const char *)data);
            ln_ui_set_wifi_status(buf);
        } else {
            ln_ui_set_wifi_status("Connecting…");
        }
        break;
    case LN_NET_EVENT_WIFI_CONNECTED:
        if (data != NULL) {
            const ln_net_wifi_info_t *w = data;
            snprintf(buf, sizeof(buf), "%s · %s", w->ssid,
                     rssi_word(w->rssi));
            ln_ui_set_wifi_status(buf);
            /* Refresh the onboarding QR to the new STA URL (task 5). Pairing,
             * which starts moments later, overrides it with the claim-URL QR. */
            ln_ui_onboarding_connected(w->ip);
            if (ui_lock()) {
                ln_scr_config_set_net(w->ssid, w->ip, rssi_word(w->rssi));
                snprintf(buf, sizeof(buf), "%s · connected", w->ssid);
                ln_scr_error_set_wifi(buf);
                bsp_display_unlock();
            }
        }
        break;
    case LN_NET_EVENT_WIFI_DISCONNECTED: {
        const ln_net_wifi_fail_t *f = data;
        const char *why = "Wi-Fi disconnected";
        if (f != NULL && f->reason == LN_NET_WIFI_FAIL_AUTH) {
            why = "Wi-Fi password rejected";
        } else if (f != NULL && f->reason == LN_NET_WIFI_FAIL_AP_NOT_FOUND) {
            why = "Wi-Fi network not found";
        }
        ln_ui_set_wifi_status("Offline");
        /* The onboarding screen is what's visible during provisioning — its
         * footer previously stayed on "Connecting to <ssid>…" forever on a
         * failed join (HIL: repeated AUTH_FAIL showed nothing on screen). */
        if (f != NULL && f->reason == LN_NET_WIFI_FAIL_AUTH) {
            snprintf(buf, sizeof(buf),
                     "\"%s\" rejected the password — tap \"Join a Wi-Fi network\" "
                     "and try again (use the eye icon to check your typing).",
                     f->ssid);
        } else if (f != NULL && f->reason == LN_NET_WIFI_FAIL_AP_NOT_FOUND) {
            snprintf(buf, sizeof(buf),
                     "Couldn't find \"%s\" — check it's in range, then tap "
                     "\"Join a Wi-Fi network\" to retry.", f->ssid);
        } else {
            snprintf(buf, sizeof(buf),
                     "Wi-Fi connection failed — retrying; or tap \"Join a "
                     "Wi-Fi network\" to pick again.");
        }
        ln_ui_onboarding_status(buf);
        if (ui_lock()) {
            ln_scr_config_set_net("", "", "");
            ln_scr_error_set_wifi(why);
            bsp_display_unlock();
        }
        break;
    }
    case LN_NET_EVENT_PORTAL_STARTED:
        if (data != NULL) {
            const ln_net_portal_info_t *p = data;
            ln_ui_onboarding_portal(p->ap_ssid, p->portal_url);
        }
        break;
    case LN_NET_EVENT_PAIRING_STARTED:
        if (data != NULL) {
            const ln_net_pairing_info_t *p = data;
            ln_ui_onboarding_pairing(p->claim_url, NULL);
        }
        break;
    case LN_NET_EVENT_PAIRED:
        ln_ui_onboarding_status("Device claimed — finishing up…");
        break;
    case LN_NET_EVENT_PORTAL_CLIENT:
        /* A phone joined/left the SoftAP — advance the onboarding footer so it
         * stops saying "Waiting for a device to connect…" (task 4). */
        if (data != NULL) {
            int count = *(const int *)data;
            char line[96];
            if (count > 1) {
                snprintf(line, sizeof(line),
                         "%d devices on the setup hotspot — open the setup page…",
                         count);
            } else {
                strlcpy(line, count == 1
                        ? "Device connected to the hotspot — open the setup page…"
                        : "Waiting for a device to connect…", sizeof(line));
            }
            ln_ui_onboarding_status(line);
        }
        break;
    case LN_NET_EVENT_PORTAL_PAGE_OPENED:
        /* The portal page was actually served — stronger signal than mere
         * AP association. */
        ln_ui_onboarding_status("Setup page open — follow the steps on your phone…");
        break;
    case LN_NET_EVENT_AUTH_INVALID:
        ln_ui_error_show("Sign-in no longer valid",
                         "This device's login was revoked or expired. Run "
                         "setup again to reconnect it to your account.",
                         "ERR_AUTH_INVALID");
        break;
    default: /* PORTAL_STOPPED / AUTH_READY need no UI change */
        break;
    }
}

static void on_audio_event(void *arg, esp_event_base_t base, int32_t id,
                           void *data)
{
    (void)arg;
    (void)base;
    if (id == LN_AUDIO_MIC_LEVEL && data != NULL) {
        const ln_evt_pct_t *p = data;
        ln_ui_mic_level(p->pct);
    }
}

/* Map a WakeNet model id (e.g. "wn9_hiesp") to a human phrase for the hint. */
static const char *wake_model_phrase(const char *model)
{
    static const struct {
        const char *needle;
        const char *phrase;
    } k_map[] = {
        { "hiesp",    "Hi ESP"    },
        { "hilexin",  "Hi Lexin"  },
        { "alexa",    "Alexa"     },
        { "hijason",  "Hi Jason"  },
        { "ninja",    "Hey Live Ninja" },
    };
    if (model == NULL || model[0] == '\0') {
        return NULL;
    }
    for (size_t i = 0; i < sizeof(k_map) / sizeof(k_map[0]); i++) {
        if (strstr(model, k_map[i].needle) != NULL) {
            return k_map[i].phrase;
        }
    }
    return model; /* unknown id: show it verbatim rather than a wrong phrase */
}

/* Contract: ln_wake.h (ln_wake_event_id_t + payload structs). */
static void on_wake_event(void *arg, esp_event_base_t base, int32_t id,
                          void *data)
{
    (void)arg;
    (void)base;
    if ((id == LN_WAKE_EVT_READY || id == LN_WAKE_EVT_FALLBACK) &&
        data != NULL) {
        const ln_wake_ready_evt_t *r = data;
        const char *phrase = wake_model_phrase(r->model);
        if (phrase != NULL) {
            ln_ui_set_wake_phrase(phrase);
        }
    }
}

/* Contract: ln_realtime.h (ln_rt_event_id_t + payload structs). */
static void on_rt_event(void *arg, esp_event_base_t base, int32_t id,
                        void *data)
{
    (void)arg;
    (void)base;
    char buf[96];

    switch (id) {
    case LN_RT_EVENT_CONNECTING:
        ln_ui_set_cloud_status("Connecting…");
        break;
    case LN_RT_EVENT_CONNECTED:
    case LN_RT_EVENT_SESSION_READY:
        ln_ui_set_cloud_status("Connected");
        break;
    case LN_RT_EVENT_RECONNECTING:
        if (data != NULL) {
            const ln_rt_reconnect_info_t *r = data;
            snprintf(buf, sizeof(buf), "Reconnecting %d/%d…", r->attempt,
                     r->max_attempts);
            ln_ui_set_cloud_status(buf);
        } else {
            ln_ui_set_cloud_status("Reconnecting…");
        }
        break;
    case LN_RT_EVENT_DISCONNECTED:
        ln_ui_set_cloud_status("Offline");
        break;
    case LN_RT_EVENT_RESPONSE_STARTED:
        ln_ui_assistant_transcript("", true); /* fresh answer */
        break;
    case LN_RT_EVENT_TRANSCRIPT_DELTA:
        if (data != NULL) {
            const ln_rt_transcript_chunk_t *c = data;
            ln_ui_assistant_transcript(c->text, c->final);
        }
        break;
    case LN_RT_EVENT_ERROR:
        if (data != NULL) {
            const ln_rt_error_info_t *e = data;
            if (e->fatal) {
                snprintf(buf, sizeof(buf), "ERR_RT_%s", e->code);
                ln_ui_error_show("Can't reach Live Ninja cloud", e->message,
                                 buf);
            } else {
                ln_ui_set_cloud_status("Reconnecting…");
            }
        }
        break;
    default: /* SPEECH_STARTED/STOPPED, RESPONSE_AUDIO_DONE: ctrl-only */
        break;
    }
}

/* Contract: ln_iot.h (ln_iot_event_id_t + payload structs). */
static void on_iot_event(void *arg, esp_event_base_t base, int32_t id,
                         void *data)
{
    (void)arg;
    (void)base;
    if (id == LN_IOT_EVENT_CONFIG_DELTA && data != NULL) {
        const ln_iot_config_delta_t *d = data;
        if (ui_lock()) {
            /* reflect shadow-synced fields only; volume/brightness/name are
             * device-local and keep their current control values */
            ln_scr_config_set_shadow(d->has_voice ? d->voice : NULL,
                                     d->has_sensitivity ? d->sensitivity
                                                        : -1.0f);
            bsp_display_unlock();
        }
    }
}

/* ------------------------------------------------------------------ */
/* init                                                                */
/* ------------------------------------------------------------------ */

static void register_handlers(void)
{
    struct {
        esp_event_base_t base;
        esp_event_handler_t fn;
    } subs[] = {
        { LN_CTRL_EVENT,  on_ctrl_event  },
        { LN_NET_EVENT,   on_net_event   },
        { LN_AUDIO_EVENT, on_audio_event },
        { LN_WAKE_EVENT,  on_wake_event  },
        { LN_RT_EVENT,    on_rt_event    },
        { LN_IOT_EVENT,   on_iot_event   },
    };
    for (size_t i = 0; i < sizeof(subs) / sizeof(subs[0]); i++) {
        esp_err_t err = esp_event_handler_register(subs[i].base,
                                                   ESP_EVENT_ANY_ID,
                                                   subs[i].fn, NULL);
        if (err != ESP_OK) {
            ESP_LOGW(TAG, "event subscribe %s failed: %s", subs[i].base,
                     esp_err_to_name(err));
        }
    }
}

esp_err_t ln_ui_init(void)
{
    if (s_ready) {
        return ESP_OK;
    }
    if (!bsp_display_lock(LN_UI_LOCK_MS)) {
        ESP_LOGE(TAG, "could not take LVGL lock");
        return ESP_ERR_TIMEOUT;
    }

    s_screens[LN_UI_STATE_BOOT]         = ln_scr_boot_create();
    s_screens[LN_UI_STATE_PROVISIONING] = ln_scr_onboarding_create();
    s_screens[LN_UI_STATE_IDLE]         = ln_scr_idle_create();
    s_screens[LN_UI_STATE_LISTENING]    = ln_scr_listening_create();
    s_screens[LN_UI_STATE_THINKING]     = ln_scr_thinking_create();
    s_screens[LN_UI_STATE_SPEAKING]     = ln_scr_speaking_create();
    s_screens[LN_UI_STATE_CONFIG]       = ln_scr_config_create();
    s_screens[LN_UI_STATE_ERROR]        = ln_scr_error_create();

    /* default About/identity from the running image */
    const esp_app_desc_t *app = esp_app_get_description();
    char fw[80]; /* app->version + idf_ver are each up to 31 chars */
    snprintf(fw, sizeof(fw), "%s (IDF %s)", app->version, app->idf_ver);
    uint8_t mac[6] = { 0 };
    /* P4 has no radio: the WIFI_STA MAC lives on the C6 and is unknown until
     * esp_wifi_remote is up. The device identity is the P4 eFuse MAC. */
    esp_read_mac(mac, ESP_MAC_EFUSE_FACTORY);
    char macs[18];
    snprintf(macs, sizeof(macs), "%02X:%02X:%02X:%02X:%02X:%02X", mac[0],
             mac[1], mac[2], mac[3], mac[4], mac[5]);
    ln_scr_config_set_about(fw, NULL, macs, NULL);

    s_clock_timer = lv_timer_create(clock_timer_cb, 1000, NULL);
    ln_scr_idle_tick();

    lv_screen_load(s_screens[LN_UI_STATE_BOOT]);
    s_state = LN_UI_STATE_BOOT;
    bsp_display_unlock();

    s_ready = true;
    register_handlers();

    ESP_LOGI(TAG, "UI ready (%ldx%ld)",
             (long)lv_display_get_horizontal_resolution(lv_display_get_default()),
             (long)lv_display_get_vertical_resolution(lv_display_get_default()));
    return ESP_OK;
}

/* ------------------------------------------------------------------ */
/* public setters                                                      */
/* ------------------------------------------------------------------ */

void ln_ui_set_state(ln_ui_state_t state)
{
    if ((unsigned)state > LN_UI_STATE_ERROR) {
        return;
    }
    if (!ui_lock()) {
        return;
    }
    if (state != s_state) {
        switch (state) {
        case LN_UI_STATE_LISTENING:
            /* a new turn starts every time we enter Listening from Idle */
            if (s_state == LN_UI_STATE_IDLE) {
                s_user_txt[0] = '\0';
                ln_scr_listening_reset();
            }
            break;
        case LN_UI_STATE_THINKING:
            ln_scr_thinking_set_request(s_user_txt);
            break;
        case LN_UI_STATE_SPEAKING:
            ln_scr_speaking_set_text(s_asst_txt);
            break;
        case LN_UI_STATE_IDLE:
            ln_scr_idle_tick();
            break;
        default:
            break;
        }
        s_state = state;
        lv_screen_load(s_screens[state]);
    }
    bsp_display_unlock();
}

void ln_ui_set_wifi_status(const char *text)
{
    if (text == NULL || !ui_lock()) {
        return;
    }
    ln_scr_idle_set_wifi(text);
    bsp_display_unlock();
}

void ln_ui_set_cloud_status(const char *text)
{
    if (text == NULL || !ui_lock()) {
        return;
    }
    ln_scr_idle_set_cloud(text);
    bsp_display_unlock();
}

void ln_ui_set_account(const char *name)
{
    if (name == NULL || !ui_lock()) {
        return;
    }
    ln_scr_idle_set_account(name[0] != '\0' ? name : "Not linked");
    bsp_display_unlock();
}

void ln_ui_set_wake_phrase(const char *phrase)
{
    if (phrase == NULL || !ui_lock()) {
        return;
    }
    ln_scr_idle_set_wake_phrase(phrase);
    bsp_display_unlock();
}

void ln_ui_set_device_info(const char *fw_version, const char *thing_name,
                           const char *mac, const char *ip)
{
    if (!ui_lock()) {
        return;
    }
    ln_scr_config_set_about(fw_version, thing_name, mac, ip);
    bsp_display_unlock();
}

void ln_ui_user_transcript(const char *text, bool replace)
{
    if (text == NULL || !ui_lock()) {
        return;
    }
    if (replace) {
        strlcpy(s_user_txt, text, sizeof(s_user_txt));
    } else {
        append_bounded(s_user_txt, sizeof(s_user_txt), text);
    }
    ln_scr_listening_set_transcript(s_user_txt);
    if (s_state == LN_UI_STATE_THINKING) {
        ln_scr_thinking_set_request(s_user_txt);
    }
    bsp_display_unlock();
}

void ln_ui_assistant_transcript(const char *text, bool replace)
{
    if (text == NULL || !ui_lock()) {
        return;
    }
    if (replace) {
        strlcpy(s_asst_txt, text, sizeof(s_asst_txt));
    } else {
        append_bounded(s_asst_txt, sizeof(s_asst_txt), text);
    }
    ln_scr_speaking_set_text(s_asst_txt);
    bsp_display_unlock();
}

void ln_ui_mic_level(uint8_t pct)
{
    if (s_state != LN_UI_STATE_LISTENING || !ui_lock()) {
        return;
    }
    ln_scr_listening_set_level(pct);
    bsp_display_unlock();
}

void ln_ui_config_values(const ln_ui_config_t *cfg)
{
    if (cfg == NULL || !ui_lock()) {
        return;
    }
    ln_scr_config_set_values(cfg);
    bsp_display_unlock();
}

void ln_ui_onboarding_portal(const char *ssid, const char *url)
{
    if (!ui_lock()) {
        return;
    }
    ln_scr_onboarding_portal(ssid, url);
    bsp_display_unlock();
}

void ln_ui_onboarding_pairing(const char *claim_url, const char *code)
{
    if (!ui_lock()) {
        return;
    }
    ln_scr_onboarding_pairing(claim_url, code);
    bsp_display_unlock();
}

void ln_ui_onboarding_connected(const char *ip)
{
    if (ip == NULL || !ui_lock()) {
        return;
    }
    ln_scr_onboarding_connected(ip);
    bsp_display_unlock();
}

void ln_ui_onboarding_status(const char *text)
{
    if (text == NULL || !ui_lock()) {
        return;
    }
    ln_scr_onboarding_status(text);
    bsp_display_unlock();
}

void ln_ui_error_show(const char *title, const char *detail, const char *code)
{
    if (!ui_lock()) {
        return;
    }
    ln_scr_error_set(title, detail, code);
    bsp_display_unlock();
}

void ln_ui_error_countdown(int secs)
{
    if (!ui_lock()) {
        return;
    }
    ln_scr_error_countdown(secs);
    bsp_display_unlock();
}
