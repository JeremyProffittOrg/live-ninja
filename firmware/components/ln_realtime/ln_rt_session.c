/*
 * ln_rt_session.c — realtime session bootstrap against the Live Ninja backend.
 *
 * GET {CONFIG_LN_RT_BACKEND_BASE_URL}/v1/realtime/session
 *   Authorization: Bearer <device JWT from ln_auth_get_jwt (ln_net)>
 *   X-LN-Client: m5stack/<semver>+<build>          (contracts/headers.md)
 *
 * The broker (plan.md M2) returns a config-bound OpenAI ephemeral token. The
 * response shape is parsed defensively: the OpenAI client_secrets passthrough
 * shape ({"value":"ek_...","session":{"model":...}}) plus common wrapper
 * spellings ({"client_secret":{"value":...}}, {"token":...},
 * {"ephemeralToken":...}) all resolve. A Nova Sonic bridge response (M12,
 * "bridgeUrl" without a token) is rejected as unsupported on this firmware.
 */
#include <stdlib.h>
#include <string.h>

#include "cJSON.h"
#include "esp_app_desc.h"
#include "esp_crt_bundle.h"
#include "esp_heap_caps.h"
#include "esp_http_client.h"
#include "esp_log.h"

#include "ln_rt_internal.h"
#include "ln_rt_ports.h"

static const char *TAG = "ln_rt_sess";

#define LN_RT_SESSION_URL   CONFIG_LN_RT_BACKEND_BASE_URL "/v1/realtime/session"
#define LN_RT_JWT_MAX       2048
#define LN_RT_HTTP_BODY_CAP (16 * 1024)
#define LN_RT_HTTP_TIMEOUT_MS 10000

/* Single worker task calls into this file, so static buffers are safe and
 * keep both task-stack usage and heap churn down. Body buffer lives in PSRAM. */
static char s_jwt[LN_RT_JWT_MAX];
static char s_auth_hdr[LN_RT_JWT_MAX + 8]; /* "Bearer " + JWT */
static char s_client_hdr[96];
static char *s_body; /* PSRAM, LN_RT_HTTP_BODY_CAP */

static void set_err(ln_rt_error_info_t *e, const char *code, const char *msg, bool fatal)
{
    if (e == NULL) {
        return;
    }
    memset(e, 0, sizeof(*e));
    strlcpy(e->code, code, sizeof(e->code));
    strlcpy(e->message, msg, sizeof(e->message));
    e->fatal = fatal;
}

/** Fetch obj.a (or obj.a.b when b != NULL) as a string, else NULL. */
static const char *json_str_at(const cJSON *root, const char *a, const char *b)
{
    const cJSON *o = cJSON_GetObjectItemCaseSensitive(root, a);
    if (o == NULL) {
        return NULL;
    }
    if (b != NULL) {
        o = cJSON_GetObjectItemCaseSensitive(o, b);
        if (o == NULL) {
            return NULL;
        }
    }
    return cJSON_IsString(o) ? o->valuestring : NULL;
}

static const char *ln_rt_client_header(void)
{
    if (s_client_hdr[0] == '\0') {
        const esp_app_desc_t *app = esp_app_get_description();
        snprintf(s_client_hdr, sizeof(s_client_hdr), "m5stack/%s+%s",
                 CONFIG_LN_RT_CLIENT_SEMVER, app->version);
    }
    return s_client_hdr;
}

static esp_err_t parse_session_body(const char *body, size_t len,
                                    ln_rt_session_info_t *out,
                                    ln_rt_error_info_t *err_out)
{
    cJSON *root = cJSON_ParseWithLength(body, len);
    if (root == NULL) {
        set_err(err_out, "bad_response", "Broker response is not valid JSON", false);
        return ESP_ERR_INVALID_RESPONSE;
    }

    const char *token = json_str_at(root, "value", NULL);
    if (token == NULL) {
        token = json_str_at(root, "client_secret", "value");
    }
    if (token == NULL) {
        token = json_str_at(root, "token", NULL);
    }
    if (token == NULL) {
        token = json_str_at(root, "ephemeralToken", NULL);
    }
    if (token == NULL) {
        token = json_str_at(root, "ephemeral_token", NULL);
    }

    if (token == NULL) {
        /* M12 Nova-pinned device? This firmware build only speaks the
         * OpenAI-direct WSS path; surface a clear fatal error. */
        if (json_str_at(root, "bridgeUrl", NULL) != NULL ||
            json_str_at(root, "bridge_url", NULL) != NULL) {
            set_err(err_out, "engine_unsupported",
                    "Device is pinned to a bridged voice engine this firmware does not support",
                    true);
        } else {
            set_err(err_out, "bad_response", "No ephemeral token in broker response", false);
        }
        cJSON_Delete(root);
        return ESP_ERR_INVALID_RESPONSE;
    }
    if (strlen(token) >= sizeof(out->token)) {
        set_err(err_out, "bad_response", "Ephemeral token longer than expected", false);
        cJSON_Delete(root);
        return ESP_ERR_INVALID_RESPONSE;
    }
    strlcpy(out->token, token, sizeof(out->token));

    const char *model = json_str_at(root, "model", NULL);
    if (model == NULL) {
        model = json_str_at(root, "session", "model");
    }
    strlcpy(out->model, (model != NULL && model[0] != '\0') ? model : CONFIG_LN_RT_DEFAULT_MODEL,
            sizeof(out->model));

    cJSON_Delete(root);
    return ESP_OK;
}

esp_err_t ln_rt_session_fetch(ln_rt_session_info_t *out, ln_rt_error_info_t *err_out)
{
    if (out == NULL) {
        return ESP_ERR_INVALID_ARG;
    }
    memset(out, 0, sizeof(*out));

    if (s_body == NULL) {
        s_body = heap_caps_malloc(LN_RT_HTTP_BODY_CAP, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
        if (s_body == NULL) {
            s_body = malloc(LN_RT_HTTP_BODY_CAP);
        }
        if (s_body == NULL) {
            set_err(err_out, "no_mem", "Out of memory for HTTP body buffer", true);
            return ESP_ERR_NO_MEM;
        }
    }

    esp_err_t err = ln_auth_get_jwt(s_jwt, sizeof(s_jwt));
    if (err != ESP_OK) {
        ESP_LOGW(TAG, "ln_auth_get_jwt failed: %s", esp_err_to_name(err));
        set_err(err_out, "auth",
                "No device credential available (not paired or refresh failed)",
                err == ESP_ERR_INVALID_STATE);
        return err;
    }
    snprintf(s_auth_hdr, sizeof(s_auth_hdr), "Bearer %s", s_jwt);

    esp_http_client_config_t cfg = {
        .url = LN_RT_SESSION_URL,
        .method = HTTP_METHOD_GET,
        .crt_bundle_attach = esp_crt_bundle_attach,
        .timeout_ms = LN_RT_HTTP_TIMEOUT_MS,
        .buffer_size = 2048,
        .buffer_size_tx = 4096, /* request headers carry the ~1-2 KB JWT */
        .disable_auto_redirect = false,
    };
    esp_http_client_handle_t http = esp_http_client_init(&cfg);
    if (http == NULL) {
        set_err(err_out, "http_init", "esp_http_client_init failed", false);
        return ESP_FAIL;
    }

    esp_http_client_set_header(http, "Authorization", s_auth_hdr);
    esp_http_client_set_header(http, "Accept", "application/json");
    esp_http_client_set_header(http, "X-LN-Client", ln_rt_client_header());

    err = esp_http_client_open(http, 0);
    if (err != ESP_OK) {
        ESP_LOGW(TAG, "HTTP open failed: %s", esp_err_to_name(err));
        esp_http_client_cleanup(http);
        set_err(err_out, "network", "Could not reach the backend broker", false);
        return err;
    }

    (void)esp_http_client_fetch_headers(http); /* -1 for chunked is fine; read() handles it */
    int status = esp_http_client_get_status_code(http);

    size_t body_len = 0;
    while (body_len < LN_RT_HTTP_BODY_CAP - 1) {
        int r = esp_http_client_read(http, s_body + body_len,
                                     (int)(LN_RT_HTTP_BODY_CAP - 1 - body_len));
        if (r <= 0) {
            break;
        }
        body_len += (size_t)r;
    }
    s_body[body_len] = '\0';
    esp_http_client_close(http);
    esp_http_client_cleanup(http);

    if (status < 200 || status >= 300) {
        ESP_LOGW(TAG, "broker HTTP %d: %.*s", status, (int)(body_len > 200 ? 200 : body_len), s_body);
        switch (status) {
        case 401:
        case 403:
            set_err(err_out, "auth", "Broker rejected the device credential", true);
            return ESP_ERR_INVALID_STATE;
        case 402:
        case 429:
            set_err(err_out, "quota", "Usage cap or rate limit reached", true);
            return ESP_ERR_INVALID_STATE;
        default:
            set_err(err_out, "broker", "Broker returned a server error", false);
            return ESP_FAIL;
        }
    }

    err = parse_session_body(s_body, body_len, out, err_out);
    if (err == ESP_OK) {
        ESP_LOGI(TAG, "ephemeral session minted (model=%s)", out->model);
    }
    /* Best-effort scrub of the JWT copy between sessions. */
    memset(s_jwt, 0, sizeof(s_jwt));
    memset(s_auth_hdr, 0, sizeof(s_auth_hdr));
    return err;
}
