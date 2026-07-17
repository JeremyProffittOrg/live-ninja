/*
 * ln_iot_shadow — the `config` named shadow (contracts/shadow.md).
 *
 * Device side of the settings-sync loop:
 *  - on connect: subscribe delta/get/rejected topics, then publish an empty
 *    /get so a full document arrives even if the delta was consumed before a
 *    reboot;
 *  - apply a desired delta ONLY if desired.settingsVersion > the version we
 *    last reported (higher-version-wins, shadow.md rule 2); stale deltas are
 *    ignored and the cached reported doc is re-published (self-healing);
 *  - ln_iot_shadow_report() publishes the full reported state and caches it
 *    (RAM JSON + NVS settingsVersion) for the stale-delta path.
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "esp_app_desc.h"
#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "cJSON.h"
#include "sdkconfig.h"

#include "ln_iot.h"
#include "ln_iot_priv.h"

static const char *TAG = "ln_iot_shadow";

static int32_t s_reported_version = -1;   /* last settingsVersion we reported */
static char *s_reported_json = NULL;      /* cached full reported publish body */
static bool s_loaded = false;

static void load_reported_version(void)
{
    if (!s_loaded) {
        if (ln_iot_nvs_get_i32(LN_IOT_K_SET_VER, &s_reported_version) != ESP_OK) {
            s_reported_version = -1;
        }
        s_loaded = true;
    }
}

static void shadow_topic(char *buf, size_t len, const char *suffix)
{
    snprintf(buf, len, "$aws/things/%s/shadow/name/config/%s", ln_iot_thing(), suffix);
}

void ln_iot_shadow_on_connected(void)
{
    load_reported_version();
    char topic[192];
    shadow_topic(topic, sizeof(topic), "update/delta");
    ln_iot_subscribe(topic, 1);
    shadow_topic(topic, sizeof(topic), "update/rejected");
    ln_iot_subscribe(topic, 1);
    shadow_topic(topic, sizeof(topic), "get/accepted");
    ln_iot_subscribe(topic, 1);
    shadow_topic(topic, sizeof(topic), "get/rejected");
    ln_iot_subscribe(topic, 1);
    /* Request the full document (boot/reconnect, shadow.md topic table). */
    shadow_topic(topic, sizeof(topic), "get");
    ln_iot_publish(topic, "", 0);
}

/* Copy a string field out of `state` into the delta struct. */
static bool take_str(const cJSON *state, const char *key, char *dst, size_t dstlen)
{
    const cJSON *item = cJSON_GetObjectItemCaseSensitive(state, key);
    if (cJSON_IsString(item) && item->valuestring != NULL) {
        strlcpy(dst, item->valuestring, dstlen);
        return true;
    }
    return false;
}

static bool parse_desired(const cJSON *state, ln_iot_config_delta_t *out)
{
    memset(out, 0, sizeof(*out));
    const cJSON *ver = cJSON_GetObjectItemCaseSensitive(state, "settingsVersion");
    if (!cJSON_IsNumber(ver)) {
        return false; /* contract: deltas without settingsVersion are dropped */
    }
    out->settings_version = (int32_t)ver->valuedouble;
    out->has_wake_word = take_str(state, "wakeWord", out->wake_word, sizeof(out->wake_word));
    out->has_wake_engine =
        take_str(state, "wakeEngine", out->wake_engine, sizeof(out->wake_engine));
    out->has_voice = take_str(state, "voice", out->voice, sizeof(out->voice));
    out->has_turn_detection =
        take_str(state, "turnDetection", out->turn_detection, sizeof(out->turn_detection));
    out->has_voice_engine =
        take_str(state, "voiceEngine", out->voice_engine, sizeof(out->voice_engine));
    const cJSON *sens = cJSON_GetObjectItemCaseSensitive(state, "sensitivity");
    if (cJSON_IsNumber(sens)) {
        out->has_sensitivity = true;
        out->sensitivity = (float)sens->valuedouble;
    }
    const cJSON *privacy = cJSON_GetObjectItemCaseSensitive(state, "privacy");
    if (cJSON_IsObject(privacy)) {
        const cJSON *sa = cJSON_GetObjectItemCaseSensitive(privacy, "storeAudio");
        if (cJSON_IsBool(sa)) {
            out->has_privacy_store_audio = true;
            out->privacy_store_audio = cJSON_IsTrue(sa);
        }
        const cJSON *st = cJSON_GetObjectItemCaseSensitive(privacy, "storeTranscripts");
        if (cJSON_IsBool(st)) {
            out->has_privacy_store_transcripts = true;
            out->privacy_store_transcripts = cJSON_IsTrue(st);
        }
        const cJSON *rd = cJSON_GetObjectItemCaseSensitive(privacy, "retentionDays");
        if (cJSON_IsNumber(rd)) {
            out->has_privacy_retention_days = true;
            out->privacy_retention_days = (int32_t)rd->valuedouble;
        }
    }
    return true;
}

static void republish_cached_reported(void)
{
    if (s_reported_json == NULL) {
        /* Nothing cached (fresh boot): ctrl reports at boot from its own NVS
         * settings, so the backend converges on the next update/accepted. */
        return;
    }
    char topic[192];
    shadow_topic(topic, sizeof(topic), "update");
    ln_iot_publish(topic, s_reported_json, 1);
}

static void handle_desired(const cJSON *state)
{
    ln_iot_config_delta_t delta;
    if (!parse_desired(state, &delta)) {
        ESP_LOGW(TAG, "desired state without settingsVersion — dropped");
        return;
    }
    load_reported_version();
    if (delta.settings_version <= s_reported_version) {
        ESP_LOGI(TAG, "stale desired v%ld (reported v%ld) — ignoring, re-reporting",
                 (long)delta.settings_version, (long)s_reported_version);
        republish_cached_reported();
        return;
    }
    ESP_LOGI(TAG, "config delta v%ld -> apply", (long)delta.settings_version);
    ln_iot_post_event(LN_IOT_EVENT_CONFIG_DELTA, &delta, sizeof(delta));
}

bool ln_iot_shadow_handle(const char *topic, const char *data, int len)
{
    (void)len;
    if (strstr(topic, "/shadow/name/config/") == NULL) {
        return false;
    }
    if (strstr(topic, "update/delta") != NULL) {
        cJSON *root = cJSON_Parse(data);
        const cJSON *state = cJSON_GetObjectItemCaseSensitive(root, "state");
        if (cJSON_IsObject(state)) {
            handle_desired(state);
        }
        cJSON_Delete(root);
        return true;
    }
    if (strstr(topic, "get/accepted") != NULL) {
        cJSON *root = cJSON_Parse(data);
        const cJSON *state = cJSON_GetObjectItemCaseSensitive(root, "state");
        const cJSON *desired = cJSON_GetObjectItemCaseSensitive(state, "desired");
        if (cJSON_IsObject(desired)) {
            handle_desired(desired);
        }
        cJSON_Delete(root);
        return true;
    }
    if (strstr(topic, "get/rejected") != NULL) {
        /* 404 = shadow not created yet (pre-first-bind); backend seeds desired
         * at bind time (shadow.md "Provisioning-time bootstrap"). */
        ESP_LOGW(TAG, "shadow get rejected: %.*s", len, data);
        return true;
    }
    if (strstr(topic, "update/rejected") != NULL) {
        ESP_LOGE(TAG, "shadow update rejected: %.*s", len, data);
        ln_iot_publish_telemetry("device_error", NULL,
                                 "{\"code\":\"shadow_update_rejected\",\"stateAtError\":\"config-sync\"}");
        return true;
    }
    return true; /* other config-shadow topics: consumed, nothing to do */
}

esp_err_t ln_iot_shadow_report(const ln_iot_shadow_reported_t *rep)
{
    if (rep == NULL || rep->wake_word == NULL || rep->wake_engine == NULL ||
        rep->voice == NULL || rep->turn_detection == NULL || rep->voice_engine == NULL) {
        return ESP_ERR_INVALID_ARG;
    }
    if (ln_iot_thing() == NULL) {
        return ESP_ERR_INVALID_STATE;
    }
    char ts[32];
    ln_iot_iso8601_now(ts, sizeof(ts));
    const esp_app_desc_t *app = esp_app_get_description();

    cJSON *root = cJSON_CreateObject();
    cJSON *state = cJSON_AddObjectToObject(root, "state");
    cJSON *reported = cJSON_AddObjectToObject(state, "reported");
    cJSON_AddNumberToObject(reported, "settingsVersion", rep->settings_version);
    cJSON_AddStringToObject(reported, "wakeWord", rep->wake_word);
    cJSON_AddStringToObject(reported, "wakeEngine", rep->wake_engine);
    cJSON_AddNumberToObject(reported, "sensitivity", rep->sensitivity);
    cJSON_AddStringToObject(reported, "voice", rep->voice);
    cJSON_AddStringToObject(reported, "turnDetection", rep->turn_detection);
    cJSON_AddStringToObject(reported, "voiceEngine", rep->voice_engine);
    if (rep->wake_model_sha256_applied != NULL) {
        cJSON_AddStringToObject(reported, "wakeModelSha256Applied",
                                rep->wake_model_sha256_applied);
    }
    cJSON_AddStringToObject(reported, "firmwareVersion", app->version);
    cJSON_AddStringToObject(reported, "deviceReportedAt", ts);

    char *payload = cJSON_PrintUnformatted(root);
    cJSON_Delete(root);
    if (payload == NULL) {
        return ESP_ERR_NO_MEM;
    }

    char topic[192];
    shadow_topic(topic, sizeof(topic), "update");
    int id = ln_iot_publish(topic, payload, 1);

    /* Cache for the stale-delta self-heal path; persist the version. */
    free(s_reported_json);
    s_reported_json = payload; /* takes ownership (cJSON uses malloc) */
    s_reported_version = rep->settings_version;
    s_loaded = true;
    ln_iot_nvs_set_i32(LN_IOT_K_SET_VER, rep->settings_version);

    return (id < 0) ? ESP_FAIL : ESP_OK;
}
