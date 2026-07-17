/* ln_iot_nvs — thin typed accessors over NVS namespace "ln_iot".
 *
 * Certificates/keys are stored as blobs (NUL-terminated PEM text) so they are
 * covered by NVS encryption once the M5 security task enables it (partition
 * table already carries an `nvs_keys` partition). Small identifiers are plain
 * strings. All getters return malloc'd copies the caller frees.
 */
#include <stdlib.h>
#include <string.h>

#include "esp_log.h"
#include "nvs.h"
#include "nvs_flash.h"

#include "ln_iot_priv.h"

static const char *TAG = "ln_iot_nvs";

static esp_err_t open_ns(nvs_open_mode_t mode, nvs_handle_t *h)
{
    esp_err_t err = nvs_open(LN_IOT_NVS_NS, mode, h);
    if (err != ESP_OK && err != ESP_ERR_NVS_NOT_FOUND) {
        ESP_LOGE(TAG, "nvs_open(%s): %s", LN_IOT_NVS_NS, esp_err_to_name(err));
    }
    return err;
}

char *ln_iot_nvs_get_str(const char *key)
{
    nvs_handle_t h;
    if (open_ns(NVS_READONLY, &h) != ESP_OK) {
        return NULL;
    }
    size_t len = 0;
    char *out = NULL;
    if (nvs_get_str(h, key, NULL, &len) == ESP_OK && len > 0) {
        out = malloc(len);
        if (out != NULL && nvs_get_str(h, key, out, &len) != ESP_OK) {
            free(out);
            out = NULL;
        }
    }
    nvs_close(h);
    return out;
}

char *ln_iot_nvs_get_blob_str(const char *key)
{
    nvs_handle_t h;
    if (open_ns(NVS_READONLY, &h) != ESP_OK) {
        return NULL;
    }
    size_t len = 0;
    char *out = NULL;
    if (nvs_get_blob(h, key, NULL, &len) == ESP_OK && len > 0) {
        out = malloc(len + 1);
        if (out != NULL) {
            if (nvs_get_blob(h, key, out, &len) == ESP_OK) {
                out[len] = '\0'; /* blobs are stored NUL-terminated, belt+braces */
            } else {
                free(out);
                out = NULL;
            }
        }
    }
    nvs_close(h);
    return out;
}

static esp_err_t set_common(const char *key, const void *val, size_t len, bool blob)
{
    nvs_handle_t h;
    esp_err_t err = open_ns(NVS_READWRITE, &h);
    if (err != ESP_OK) {
        return err;
    }
    err = blob ? nvs_set_blob(h, key, val, len) : nvs_set_str(h, key, (const char *)val);
    if (err == ESP_OK) {
        err = nvs_commit(h);
    }
    nvs_close(h);
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "set %s: %s", key, esp_err_to_name(err));
    }
    return err;
}

esp_err_t ln_iot_nvs_set_str(const char *key, const char *val)
{
    return set_common(key, val, 0, false);
}

esp_err_t ln_iot_nvs_set_blob_str(const char *key, const char *val)
{
    return set_common(key, val, strlen(val) + 1, true);
}

esp_err_t ln_iot_nvs_erase_key(const char *key)
{
    nvs_handle_t h;
    esp_err_t err = open_ns(NVS_READWRITE, &h);
    if (err != ESP_OK) {
        return err;
    }
    err = nvs_erase_key(h, key);
    if (err == ESP_ERR_NVS_NOT_FOUND) {
        err = ESP_OK;
    }
    if (err == ESP_OK) {
        err = nvs_commit(h);
    }
    nvs_close(h);
    return err;
}

esp_err_t ln_iot_nvs_get_i32(const char *key, int32_t *out)
{
    nvs_handle_t h;
    esp_err_t err = open_ns(NVS_READONLY, &h);
    if (err != ESP_OK) {
        return err;
    }
    err = nvs_get_i32(h, key, out);
    nvs_close(h);
    return err;
}

esp_err_t ln_iot_nvs_set_i32(const char *key, int32_t val)
{
    nvs_handle_t h;
    esp_err_t err = open_ns(NVS_READWRITE, &h);
    if (err != ESP_OK) {
        return err;
    }
    err = nvs_set_i32(h, key, val);
    if (err == ESP_OK) {
        err = nvs_commit(h);
    }
    nvs_close(h);
    return err;
}
