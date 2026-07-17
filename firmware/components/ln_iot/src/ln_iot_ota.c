/*
 * ln_iot_ota — IoT-Jobs-driven A/B OTA (plan.md M5 "OTA" task).
 *
 * Job document contract: {"operation":"ota","url":"https://...","sha256":"<64 hex>",
 * "version":"x.y.z","force":false?}. Flow:
 *   notify-next / $next/get  ->  QUEUED ota job  ->  IN_PROGRESS(downloading)
 *   -> esp_https_ota into the inactive slot -> SHA-256 read-back verify against
 *   the job's pinned hash -> IN_PROGRESS(rebooting) + jobId/version persisted to
 *   NVS -> esp_restart().
 * Next boot runs the new image in ESP_OTA_IMG_PENDING_VERIFY: the first
 * successful MQTT connect marks it valid (esp_ota_mark_app_valid_cancel_rollback)
 * and reports the persisted job SUCCEEDED (+ ota_completed telemetry). If the
 * new image can't reach the cloud within CONFIG_LN_IOT_VERIFY_TIMEOUT_S, a boot
 * guard triggers esp_ota_mark_app_invalid_rollback_and_reboot(); after the
 * bootloader falls back, the old image sees the persisted job with a version
 * mismatch and reports it FAILED (+ ota_rolled_back telemetry).
 *
 * Anti-rollback: version-downgrade jobs are rejected app-side unless
 * "force":true. Hardware anti-rollback (Secure Boot v2 eFuse secure_version)
 * is enabled by the M5 security-hardening task — see components/ln_iot/README.md.
 */
#include <ctype.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "esp_app_desc.h"
#include "esp_crt_bundle.h"
#include "esp_https_ota.h"
#include "esp_log.h"
#include "esp_ota_ops.h"
#include "esp_partition.h"
#include "esp_system.h"
#include "esp_timer.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "cJSON.h"
#include "mbedtls/sha256.h"
#include "sdkconfig.h"

#include "ln_iot.h"
#include "ln_iot_priv.h"

static const char *TAG = "ln_iot_ota";

typedef struct {
    char job_id[64];
    char url[512];
    char sha256_hex[65];
    char version[32];
    bool force;
} ota_job_t;

static volatile bool s_ota_running = false;
static esp_timer_handle_t s_verify_guard;

/* ------------------------------------------------------------- utilities */

static void jobs_topic(char *buf, size_t len, const char *suffix)
{
    snprintf(buf, len, "$aws/things/%s/jobs/%s", ln_iot_thing(), suffix);
}

static void publish_job_status(const char *job_id, const char *status, const char *step)
{
    char topic[192];
    char suffix[96];
    snprintf(suffix, sizeof(suffix), "%s/update", job_id);
    jobs_topic(topic, sizeof(topic), suffix);
    char payload[224];
    snprintf(payload, sizeof(payload),
             "{\"status\":\"%s\",\"statusDetails\":{\"step\":\"%s\",\"fw\":\"%s\"}}",
             status, step, esp_app_get_description()->version);
    ln_iot_publish(topic, payload, 1);
}

/* "v1.2.3" -> 1002003; tolerant of missing components. */
static long version_rank(const char *v)
{
    if (v == NULL) {
        return -1;
    }
    if (*v == 'v' || *v == 'V') {
        v++;
    }
    long parts[3] = {0, 0, 0};
    for (int i = 0; i < 3; i++) {
        parts[i] = strtol(v, (char **)&v, 10);
        if (*v != '.') {
            break;
        }
        v++;
    }
    return parts[0] * 1000000L + parts[1] * 1000L + parts[2];
}

static bool hex_to_bytes(const char *hex, uint8_t *out, size_t outlen)
{
    if (strlen(hex) != outlen * 2) {
        return false;
    }
    for (size_t i = 0; i < outlen; i++) {
        char byte_str[3] = {hex[2 * i], hex[2 * i + 1], '\0'};
        if (!isxdigit((unsigned char)byte_str[0]) || !isxdigit((unsigned char)byte_str[1])) {
            return false;
        }
        out[i] = (uint8_t)strtol(byte_str, NULL, 16);
    }
    return true;
}

/* SHA-256 of the first `len` bytes of the freshly written OTA partition. */
static esp_err_t hash_partition(const esp_partition_t *part, int len, uint8_t out[32])
{
    uint8_t *buf = malloc(4096);
    if (buf == NULL) {
        return ESP_ERR_NO_MEM;
    }
    mbedtls_sha256_context sha;
    mbedtls_sha256_init(&sha);
    mbedtls_sha256_starts(&sha, 0);
    esp_err_t err = ESP_OK;
    for (int off = 0; off < len; off += 4096) {
        int chunk = (len - off) < 4096 ? (len - off) : 4096;
        err = esp_partition_read(part, off, buf, chunk);
        if (err != ESP_OK) {
            break;
        }
        mbedtls_sha256_update(&sha, buf, chunk);
    }
    if (err == ESP_OK) {
        mbedtls_sha256_finish(&sha, out);
    }
    mbedtls_sha256_free(&sha);
    free(buf);
    return err;
}

/* --------------------------------------------------------------- OTA task */

static void ota_task(void *arg)
{
    ota_job_t *job = arg;
    ln_iot_ota_info_t info = {0};
    strlcpy(info.job_id, job->job_id, sizeof(info.job_id));
    strlcpy(info.version, job->version, sizeof(info.version));

    const esp_app_desc_t *running = esp_app_get_description();
    ESP_LOGI(TAG, "OTA job %s: %s -> %s", job->job_id, running->version, job->version);

    uint8_t expected[32];
    if (!hex_to_bytes(job->sha256_hex, expected, sizeof(expected))) {
        publish_job_status(job->job_id, "FAILED", "bad-sha256-field");
        goto fail_no_ota;
    }
    if (!job->force && version_rank(job->version) <= version_rank(running->version)) {
        ESP_LOGE(TAG, "downgrade/same-version rejected (%s <= %s)", job->version,
                 running->version);
        publish_job_status(job->job_id, "FAILED", "downgrade-rejected");
        goto fail_no_ota;
    }

    publish_job_status(job->job_id, "IN_PROGRESS", "downloading");
    {
        char attrs[224]; /* worst case: 2×31B versions + 63B jobId + envelope */
        snprintf(attrs, sizeof(attrs),
                 "{\"fromVersion\":\"%s\",\"toVersion\":\"%s\",\"jobId\":\"%s\"}",
                 running->version, job->version, job->job_id);
        ln_iot_publish_telemetry("ota_started", NULL, attrs);
    }
    ln_iot_post_event(LN_IOT_EVENT_OTA_STARTED, &info, sizeof(info));

    esp_http_client_config_t http = {
        .url = job->url,
        .crt_bundle_attach = esp_crt_bundle_attach,
        .timeout_ms = 30000,
        .buffer_size = 8192,
        .keep_alive_enable = true,
    };
    esp_https_ota_config_t ota_cfg = {
        .http_config = &http,
    };
    esp_https_ota_handle_t handle = NULL;
    esp_err_t err = esp_https_ota_begin(&ota_cfg, &handle);
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "esp_https_ota_begin: %s", esp_err_to_name(err));
        publish_job_status(job->job_id, "FAILED", "download-begin");
        goto fail_no_ota;
    }

    int total = esp_https_ota_get_image_size(handle);
    int last_pct = -1;
    while ((err = esp_https_ota_perform(handle)) == ESP_ERR_HTTPS_OTA_IN_PROGRESS) {
        if (total > 0) {
            int pct = (int)(((int64_t)esp_https_ota_get_image_len_read(handle) * 100) / total);
            if (pct != last_pct && pct % 5 == 0) {
                last_pct = pct;
                ln_iot_post_event(LN_IOT_EVENT_OTA_PROGRESS, &pct, sizeof(pct));
            }
        }
    }
    if (err != ESP_OK || !esp_https_ota_is_complete_data_received(handle)) {
        ESP_LOGE(TAG, "download failed: %s", esp_err_to_name(err));
        esp_https_ota_abort(handle);
        publish_job_status(job->job_id, "FAILED", "download");
        goto fail;
    }

    /* Pinned-hash verify: read the written image back out of flash and compare
     * against the job's sha256 BEFORE the slot becomes bootable. */
    {
        int img_len = esp_https_ota_get_image_len_read(handle);
        const esp_partition_t *part = esp_ota_get_next_update_partition(NULL);
        uint8_t actual[32];
        if (part == NULL || hash_partition(part, img_len, actual) != ESP_OK ||
            memcmp(actual, expected, sizeof(expected)) != 0) {
            ESP_LOGE(TAG, "SHA-256 mismatch — refusing to boot image");
            esp_https_ota_abort(handle);
            publish_job_status(job->job_id, "FAILED", "sha256-mismatch");
            goto fail;
        }
    }

    err = esp_https_ota_finish(handle); /* validates image + sets boot partition */
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "esp_https_ota_finish: %s", esp_err_to_name(err));
        publish_job_status(job->job_id, "FAILED", "finish-validate");
        goto fail_no_ota;
    }

    /* Persist the in-flight job so the next boot (new or rolled-back image)
     * can close it out, then reboot into the new slot. */
    ln_iot_nvs_set_str(LN_IOT_K_OTA_JOB, job->job_id);
    ln_iot_nvs_set_str(LN_IOT_K_OTA_VER, job->version);
    publish_job_status(job->job_id, "IN_PROGRESS", "rebooting");
    ln_iot_post_event(LN_IOT_EVENT_OTA_REBOOTING, &info, sizeof(info));
    ln_iot_prepare_reboot();
    esp_restart();

fail:
    /* handle already aborted above */
fail_no_ota:
    ln_iot_post_event(LN_IOT_EVENT_OTA_FAILED, &info, sizeof(info));
    s_ota_running = false;
    free(job);
    vTaskDelete(NULL);
}

/* ------------------------------------------------------- job dispatching */

static void handle_execution(const cJSON *execution)
{
    const cJSON *job_id = cJSON_GetObjectItemCaseSensitive(execution, "jobId");
    const cJSON *status = cJSON_GetObjectItemCaseSensitive(execution, "status");
    const cJSON *doc = cJSON_GetObjectItemCaseSensitive(execution, "jobDocument");
    if (!cJSON_IsString(job_id) || !cJSON_IsString(status) || !cJSON_IsObject(doc)) {
        return;
    }
    if (strcmp(status->valuestring, "QUEUED") != 0 &&
        strcmp(status->valuestring, "IN_PROGRESS") != 0) {
        return;
    }
    const cJSON *op = cJSON_GetObjectItemCaseSensitive(doc, "operation");
    if (!cJSON_IsString(op) || strcmp(op->valuestring, "ota") != 0) {
        ESP_LOGW(TAG, "job %s: unknown operation, rejecting", job_id->valuestring);
        publish_job_status(job_id->valuestring, "REJECTED", "unknown-operation");
        return;
    }
    const cJSON *url = cJSON_GetObjectItemCaseSensitive(doc, "url");
    const cJSON *sha = cJSON_GetObjectItemCaseSensitive(doc, "sha256");
    const cJSON *ver = cJSON_GetObjectItemCaseSensitive(doc, "version");
    const cJSON *force = cJSON_GetObjectItemCaseSensitive(doc, "force");
    if (!cJSON_IsString(url) || !cJSON_IsString(sha) || !cJSON_IsString(ver)) {
        publish_job_status(job_id->valuestring, "FAILED", "bad-job-document");
        return;
    }
    if (s_ota_running) {
        ESP_LOGW(TAG, "OTA already in progress, leaving job %s queued", job_id->valuestring);
        return;
    }
    ota_job_t *job = calloc(1, sizeof(*job));
    if (job == NULL) {
        return;
    }
    strlcpy(job->job_id, job_id->valuestring, sizeof(job->job_id));
    strlcpy(job->url, url->valuestring, sizeof(job->url));
    strlcpy(job->sha256_hex, sha->valuestring, sizeof(job->sha256_hex));
    strlcpy(job->version, ver->valuestring, sizeof(job->version));
    job->force = cJSON_IsTrue(force);
    s_ota_running = true;
    if (xTaskCreate(ota_task, "ln_iot_ota", 8192, job, 5, NULL) != pdPASS) {
        s_ota_running = false;
        free(job);
        ESP_LOGE(TAG, "OTA task create failed");
    }
}

bool ln_iot_ota_handle(const char *topic, const char *data, int len)
{
    (void)len;
    if (strstr(topic, "/jobs/") == NULL) {
        return false;
    }
    if (strstr(topic, "notify-next") != NULL || strstr(topic, "$next/get/accepted") != NULL) {
        cJSON *root = cJSON_Parse(data);
        const cJSON *execution = cJSON_GetObjectItemCaseSensitive(root, "execution");
        if (cJSON_IsObject(execution)) {
            handle_execution(execution);
        }
        cJSON_Delete(root);
        return true;
    }
    /* $next/get/rejected (no jobs) and <jobId>/update/accepted|rejected. */
    if (strstr(topic, "/update/rejected") != NULL) {
        ESP_LOGW(TAG, "job update rejected: %.*s", len, data);
    }
    return true;
}

/* ---------------------------------------------- boot guard + reconciliation */

static void verify_guard_cb(void *arg)
{
    (void)arg;
    ESP_LOGE(TAG, "no cloud check-in within %ds of an OTA boot — rolling back",
             CONFIG_LN_IOT_VERIFY_TIMEOUT_S);
    esp_ota_mark_app_invalid_rollback_and_reboot();
}

void ln_iot_ota_boot_guard(void)
{
    const esp_partition_t *running = esp_ota_get_running_partition();
    esp_ota_img_states_t state;
    if (esp_ota_get_state_partition(running, &state) == ESP_OK &&
        state == ESP_OTA_IMG_PENDING_VERIFY) {
        ESP_LOGW(TAG, "running unverified OTA image — %ds to check in or roll back",
                 CONFIG_LN_IOT_VERIFY_TIMEOUT_S);
        const esp_timer_create_args_t targs = {
            .callback = verify_guard_cb,
            .name = "ln_ota_guard",
        };
        if (esp_timer_create(&targs, &s_verify_guard) == ESP_OK) {
            esp_timer_start_once(s_verify_guard,
                                 (uint64_t)CONFIG_LN_IOT_VERIFY_TIMEOUT_S * 1000000ULL);
        }
    }
}

void ln_iot_ota_check_pending_verify(void)
{
    if (s_verify_guard != NULL) {
        esp_timer_stop(s_verify_guard);
        esp_timer_delete(s_verify_guard);
        s_verify_guard = NULL;
    }
    const esp_partition_t *running = esp_ota_get_running_partition();
    esp_ota_img_states_t state;
    if (esp_ota_get_state_partition(running, &state) == ESP_OK &&
        state == ESP_OTA_IMG_PENDING_VERIFY) {
        ESP_LOGI(TAG, "first cloud check-in OK — marking app image valid");
        esp_ota_mark_app_valid_cancel_rollback();
    }
}

void ln_iot_ota_on_connected(void)
{
    /* Close out a persisted in-flight OTA job from a previous boot. */
    char *job_id = ln_iot_nvs_get_str(LN_IOT_K_OTA_JOB);
    char *target_ver = ln_iot_nvs_get_str(LN_IOT_K_OTA_VER);
    if (job_id != NULL && target_ver != NULL) {
        const char *fw = esp_app_get_description()->version;
        bool ok = (strcmp(fw, target_ver) == 0);
        publish_job_status(job_id, ok ? "SUCCEEDED" : "FAILED",
                           ok ? "boot-verified" : "rolled-back");
        char attrs[160];
        snprintf(attrs, sizeof(attrs),
                 "{\"fromVersion\":null,\"toVersion\":\"%s\",\"jobId\":\"%s\"}",
                 target_ver, job_id);
        ln_iot_publish_telemetry(ok ? "ota_completed" : "ota_rolled_back", NULL, attrs);
        ln_iot_nvs_erase_key(LN_IOT_K_OTA_JOB);
        ln_iot_nvs_erase_key(LN_IOT_K_OTA_VER);
    }
    free(job_id);
    free(target_ver);

    char topic[192];
    jobs_topic(topic, sizeof(topic), "notify-next");
    ln_iot_subscribe(topic, 1);
    jobs_topic(topic, sizeof(topic), "$next/get/accepted");
    ln_iot_subscribe(topic, 1);
    jobs_topic(topic, sizeof(topic), "$next/get/rejected");
    ln_iot_subscribe(topic, 1);
    jobs_topic(topic, sizeof(topic), "$next/get");
    ln_iot_publish(topic, "{}", 1);
}
