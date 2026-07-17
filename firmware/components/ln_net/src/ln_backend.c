/*
 * ln_backend.c — small HTTPS JSON client for the Live Ninja backend
 * (CONFIG_LN_BACKEND_BASE_URL). TLS via the ESP x509 certificate bundle
 * (live.jeremy.ninja terminates on CloudFront/ACM — Amazon roots are in the
 * bundle). Used by ln_pairing.c and ln_auth.c.
 */
#include <stdio.h>
#include <string.h>

#include "esp_app_desc.h"
#include "esp_crt_bundle.h"
#include "esp_http_client.h"
#include "esp_log.h"
#include "mbedtls/base64.h"
#include "sdkconfig.h"

#include "ln_net_priv.h"

static const char *TAG = "ln_backend";

#define LN_HTTP_TIMEOUT_MS 15000

esp_err_t ln_b64url_encode(const uint8_t *in, size_t in_len,
                           char *out, size_t out_len)
{
    size_t olen = 0;
    int rc = mbedtls_base64_encode((unsigned char *)out, out_len, &olen,
                                   in, in_len);
    if (rc != 0) {
        return ESP_ERR_NO_MEM;
    }
    /* base64 -> base64url, strip padding */
    size_t w = 0;
    for (size_t i = 0; i < olen; i++) {
        char c = out[i];
        if (c == '+') {
            c = '-';
        } else if (c == '/') {
            c = '_';
        } else if (c == '=') {
            break;
        }
        out[w++] = c;
    }
    out[w] = '\0';
    return ESP_OK;
}

esp_err_t ln_backend_request(const char *path, const char *json_body,
                             const char *bearer, ln_backend_rsp_t *rsp)
{
    if (path == NULL || rsp == NULL) {
        return ESP_ERR_INVALID_ARG;
    }
    rsp->status = -1;
    rsp->body[0] = '\0';
    rsp->body_len = 0;

    char url[256];
    int n = snprintf(url, sizeof(url), "%s%s", CONFIG_LN_BACKEND_BASE_URL, path);
    if (n < 0 || n >= (int)sizeof(url)) {
        return ESP_ERR_INVALID_ARG;
    }

    esp_http_client_config_t cfg = {
        .url = url,
        .method = (json_body != NULL) ? HTTP_METHOD_POST : HTTP_METHOD_GET,
        .timeout_ms = LN_HTTP_TIMEOUT_MS,
        .crt_bundle_attach = esp_crt_bundle_attach,
        .disable_auto_redirect = false,
        .buffer_size = 2048,
        .buffer_size_tx = 1024,
    };
    esp_http_client_handle_t client = esp_http_client_init(&cfg);
    if (client == NULL) {
        return ESP_ERR_NO_MEM;
    }

    char ua[64];
    const esp_app_desc_t *app = esp_app_get_description();
    snprintf(ua, sizeof(ua), "liveninja-tab5/%s", app->version);
    esp_http_client_set_header(client, "User-Agent", ua);
    esp_http_client_set_header(client, "Accept", "application/json");
    if (json_body != NULL) {
        esp_http_client_set_header(client, "Content-Type", "application/json");
    }
    if (bearer != NULL && bearer[0] != '\0') {
        char auth[2200];
        n = snprintf(auth, sizeof(auth), "Bearer %s", bearer);
        if (n < 0 || n >= (int)sizeof(auth)) {
            esp_http_client_cleanup(client);
            return ESP_ERR_INVALID_ARG;
        }
        esp_http_client_set_header(client, "Authorization", auth);
    }

    size_t body_len = json_body ? strlen(json_body) : 0;
    esp_err_t err = esp_http_client_open(client, body_len);
    if (err != ESP_OK) {
        ESP_LOGW(TAG, "open %s failed: %s", path, esp_err_to_name(err));
        esp_http_client_cleanup(client);
        return err;
    }
    if (body_len > 0 &&
        esp_http_client_write(client, json_body, body_len) != (int)body_len) {
        esp_http_client_cleanup(client);
        return ESP_FAIL;
    }
    if (esp_http_client_fetch_headers(client) < 0) {
        esp_http_client_cleanup(client);
        return ESP_FAIL;
    }
    rsp->status = esp_http_client_get_status_code(client);

    int total = 0;
    while (total < LN_BACKEND_RSP_MAX - 1) {
        int r = esp_http_client_read(client, rsp->body + total,
                                     LN_BACKEND_RSP_MAX - 1 - total);
        if (r <= 0) {
            break;
        }
        total += r;
    }
    rsp->body[total] = '\0';
    rsp->body_len = total;

    esp_http_client_cleanup(client);
    ESP_LOGD(TAG, "%s %s -> %d (%d bytes)",
             json_body ? "POST" : "GET", path, rsp->status, total);
    return ESP_OK;
}
