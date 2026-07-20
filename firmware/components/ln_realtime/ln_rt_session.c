/*
 * ln_rt_session.c — realtime session bootstrap against the Live Ninja backend.
 *
 * GET {CONFIG_LN_RT_BACKEND_BASE_URL}/api/v1/realtime/session
 *   Authorization: Bearer <device JWT from ln_auth_get_jwt (ln_net)>
 *   X-LN-Client: m5stack/<semver>+<build>          (contracts/headers.md)
 *
 * The broker (plan.md M2) resolves the device's per-device `voiceEngine` pin
 * (settings.schema.json) and returns ONE of three shapes:
 *
 *   1. OpenAI-direct (default) — a config-bound OpenAI ephemeral token. Parsed
 *      defensively: the client_secrets passthrough shape
 *      ({"value":"ek_...","session":{"model":...}}) plus common wrapper
 *      spellings ({"client_secret":{"value":...}}, {"token":...},
 *      {"ephemeralToken":...}) all resolve.
 *
 *   2. Nova Sonic bridge (M12, FR-VE-03) — for a device pinned to `nova-sonic`:
 *      {"mode":"nova-bridge","wsUrl":"wss://nova.live.jeremy.ninja/...","token":...}.
 *      No OpenAI ephemeral is minted; ln_realtime connects its WSS to wsUrl
 *      (the single-use bridge token is normally already embedded in wsUrl; a
 *      separate "token" field, when present, is carried for URL composition).
 *      The backend bridge holds the Bedrock InvokeModelWithBidirectionalStream
 *      session and speaks the same pcm16 event framing this client already
 *      uses, so only the transport URL + auth differ.
 *
 *   3. Gemini Live direct (M13) — for a device pinned to `gemini-flash-live`:
 *      {"mode":"gemini-direct","geminiEndpoint":"wss://generativelanguage...",
 *       "accessToken":{"value":"auth_tokens/..."},"sessionConfig":{...},...}.
 *      Client-direct like OpenAI, but auth rides the URL (?access_token=,
 *      URL-escaped) and the client must send sessionConfig as its `setup`
 *      frame on connect. sessionConfig can be tens of KB (full persona
 *      instructions + the whole tool manifest as functionDeclarations), so
 *      the composed {"setup":...} frame is staged in a reused PSRAM buffer.
 *      On reconnects the stored session-resumption handle (ln_realtime.c)
 *      is injected as sessionResumption.handle so the fresh token resumes
 *      the same conversation.
 *
 * Nova detection is by an explicit {"mode":"nova-bridge"} OR the presence of
 * a bridge URL field; both are recognized so the exact broker spelling can be
 * finalized independently. The gemini-direct branch keys on mode ONLY and is
 * checked FIRST — the broker deliberately never uses a wsUrl-family field
 * name for Gemini so a legacy parser fails closed instead of misrouting into
 * its Nova branch (gemini-plan.md §3.4). HIL-unverified on real Tab5
 * hardware (M12 Nova, M13 Gemini).
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

/* The mint route lives under the shared /api prefix (internal/webapp
 * api_routes.go). The bare /v1/... spelling 404s — it was masked for months
 * by the version-gate 426 that fired before routing. */
#define LN_RT_SESSION_URL   CONFIG_LN_RT_BACKEND_BASE_URL "/api/v1/realtime/session"
#define LN_RT_JWT_MAX       2048
/* The mint response carries the whole resolved session: rates, sessionConfig
 * (persona instructions + memory directive) and the ~20-tool manifest — well
 * over 16 KB, which silently truncated the JSON ("not valid JSON" parse
 * failures while the mint burned broker slots). PSRAM buffer, so size is
 * cheap. */
#define LN_RT_HTTP_BODY_CAP (64 * 1024)
#define LN_RT_HTTP_TIMEOUT_MS 10000
/* Staging for the composed Gemini {"setup":...} frame. sessionConfig is the
 * largest member of the (≤64 KB) mint body — persona instructions plus the
 * ~20-tool manifest as functionDeclarations — so 48 KB leaves real headroom
 * while staying a rounding error in 32 MB PSRAM. */
#define LN_RT_GEMINI_SETUP_CAP (48 * 1024)

/* Single worker task calls into this file, so static buffers are safe and
 * keep both task-stack usage and heap churn down. Body buffer lives in PSRAM. */
static char s_jwt[LN_RT_JWT_MAX];
static char s_auth_hdr[LN_RT_JWT_MAX + 8]; /* "Bearer " + JWT */
static char s_client_hdr[96];
static char *s_body;         /* PSRAM, LN_RT_HTTP_BODY_CAP */
static char *s_gemini_setup; /* PSRAM, LN_RT_GEMINI_SETUP_CAP ({"setup":...} frame) */

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

/* --- Gemini Live direct shape (M13) ------------------------------------- *
 * {"mode":"gemini-direct","geminiEndpoint":...,"accessToken":{"value":...},
 *  "sessionConfig":{...},...}. Composes the {"setup":<sessionConfig>} frame
 * ln_realtime must send on WSS connect into the reused PSRAM staging buffer,
 * injecting the stored resumption handle (if any) so a reconnect resumes the
 * same conversation. Mutates root's sessionConfig subtree in place before
 * printing — root is parse-scratch owned by parse_session_body. */
static esp_err_t parse_gemini_session(cJSON *root, ln_rt_session_info_t *out,
                                      ln_rt_error_info_t *err_out)
{
    const char *endpoint = json_str_at(root, "geminiEndpoint", NULL);
    if (endpoint == NULL || endpoint[0] == '\0') {
        set_err(err_out, "bad_response", "gemini-direct response missing geminiEndpoint", false);
        return ESP_ERR_INVALID_RESPONSE;
    }
    if (strlen(endpoint) >= sizeof(out->ws_url)) {
        set_err(err_out, "bad_response", "Gemini endpoint longer than expected", false);
        return ESP_ERR_INVALID_RESPONSE;
    }
    const char *token = json_str_at(root, "accessToken", "value");
    if (token == NULL || token[0] == '\0') {
        set_err(err_out, "bad_response", "gemini-direct response missing accessToken.value", false);
        return ESP_ERR_INVALID_RESPONSE;
    }
    if (strlen(token) >= sizeof(out->token)) {
        set_err(err_out, "bad_response", "Gemini access token longer than expected", false);
        return ESP_ERR_INVALID_RESPONSE;
    }
    cJSON *cfg = cJSON_GetObjectItemCaseSensitive(root, "sessionConfig");
    if (!cJSON_IsObject(cfg)) {
        set_err(err_out, "bad_response", "gemini-direct response missing sessionConfig", false);
        return ESP_ERR_INVALID_RESPONSE;
    }

    if (s_gemini_setup == NULL) {
        s_gemini_setup = heap_caps_malloc(LN_RT_GEMINI_SETUP_CAP,
                                          MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
        if (s_gemini_setup == NULL) {
            s_gemini_setup = malloc(LN_RT_GEMINI_SETUP_CAP);
        }
        if (s_gemini_setup == NULL) {
            set_err(err_out, "no_mem", "Out of memory for Gemini setup buffer", true);
            return ESP_ERR_NO_MEM;
        }
    }

    /* Reconnect? Resume the SAME Gemini conversation: replace the broker's
     * (empty) sessionResumption config with {"handle":<stored>}. Best-effort —
     * a failed injection just starts a fresh conversation. */
    const char *handle = ln_rt_resumption_handle();
    if (handle != NULL && handle[0] != '\0') {
        cJSON *res = cJSON_CreateObject();
        if (res == NULL || cJSON_AddStringToObject(res, "handle", handle) == NULL) {
            cJSON_Delete(res);
            ESP_LOGW(TAG, "resumption handle injection failed — starting fresh");
        } else {
            cJSON_DeleteItemFromObjectCaseSensitive(cfg, "sessionResumption");
            cJSON_AddItemToObject(cfg, "sessionResumption", res);
            ESP_LOGI(TAG, "resuming gemini session (handle %u chars)",
                     (unsigned)strlen(handle));
        }
    }

    /* Compose {"setup":<sessionConfig>} verbatim into the PSRAM stage. */
    static const char k_setup_prefix[] = "{\"setup\":";
    size_t pos = sizeof(k_setup_prefix) - 1;
    memcpy(s_gemini_setup, k_setup_prefix, pos);
    if (!cJSON_PrintPreallocated(cfg, s_gemini_setup + pos,
                                 (int)(LN_RT_GEMINI_SETUP_CAP - pos - 2), 0)) {
        set_err(err_out, "bad_response", "Gemini sessionConfig larger than expected", false);
        return ESP_ERR_INVALID_RESPONSE;
    }
    pos += strlen(s_gemini_setup + pos);
    s_gemini_setup[pos++] = '}';
    s_gemini_setup[pos] = '\0';

    out->mode = LN_RT_ENGINE_GEMINI_DIRECT;
    strlcpy(out->ws_url, endpoint, sizeof(out->ws_url));
    strlcpy(out->token, token, sizeof(out->token));
    const char *model = json_str_at(root, "model", NULL);
    if (model != NULL && model[0] != '\0') {
        strlcpy(out->model, model, sizeof(out->model));
    }
    out->setup_frame = s_gemini_setup;
    return ESP_OK;
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

    const char *mode = json_str_at(root, "mode", NULL);

    /* --- Gemini Live direct shape (M13)? ---------------------------------- *
     * MUST be decided before the Nova wsUrl-presence heuristic below: that
     * heuristic keys on field PRESENCE, and while the gemini-direct shape
     * deliberately avoids wsUrl-family names (gemini-plan.md §3.4), mode is
     * the contract — check it explicitly and first. */
    if (mode != NULL && strcmp(mode, "gemini-direct") == 0) {
        esp_err_t gerr = parse_gemini_session(root, out, err_out);
        cJSON_Delete(root);
        return gerr;
    }

    /* --- Nova Sonic bridge shape (M12, FR-VE-03)? ------------------------- *
     * Selected when the broker resolved this device's voiceEngine pin to
     * nova-sonic. Detected by mode=="nova-bridge" or the presence of a bridge
     * WSS URL. ln_realtime then connects to ws_url instead of OpenAI and skips
     * the ephemeral step entirely. */
    const char *ws_url = json_str_at(root, "wsUrl", NULL);
    if (ws_url == NULL) {
        ws_url = json_str_at(root, "ws_url", NULL);
    }
    if (ws_url == NULL) {
        ws_url = json_str_at(root, "bridgeUrl", NULL);
    }
    if (ws_url == NULL) {
        ws_url = json_str_at(root, "bridge_url", NULL);
    }
    bool nova = (mode != NULL && strcmp(mode, "nova-bridge") == 0) || ws_url != NULL;
    if (nova) {
        if (ws_url == NULL) {
            set_err(err_out, "bad_response", "nova-bridge response missing wsUrl", false);
            cJSON_Delete(root);
            return ESP_ERR_INVALID_RESPONSE;
        }
        if (strlen(ws_url) >= sizeof(out->ws_url)) {
            set_err(err_out, "bad_response", "Bridge wsUrl longer than expected", false);
            cJSON_Delete(root);
            return ESP_ERR_INVALID_RESPONSE;
        }
        out->mode = LN_RT_ENGINE_NOVA_BRIDGE;
        strlcpy(out->ws_url, ws_url, sizeof(out->ws_url));

        /* Optional separate single-use token; usually already embedded in
         * ws_url, in which case ln_realtime uses ws_url verbatim. */
        const char *btok = json_str_at(root, "token", NULL);
        if (btok == NULL) {
            btok = json_str_at(root, "bridgeToken", NULL);
        }
        if (btok == NULL) {
            btok = json_str_at(root, "bridge_token", NULL);
        }
        if (btok != NULL) {
            if (strlen(btok) >= sizeof(out->token)) {
                set_err(err_out, "bad_response", "Bridge token longer than expected", false);
                cJSON_Delete(root);
                return ESP_ERR_INVALID_RESPONSE;
            }
            strlcpy(out->token, btok, sizeof(out->token));
        }
        cJSON_Delete(root);
        return ESP_OK;
    }

    /* --- OpenAI-direct shape (default) ----------------------------------- */
    out->mode = LN_RT_ENGINE_OPENAI_DIRECT;
    const char *token = json_str_at(root, "value", NULL);
    if (token == NULL) {
        /* The deployed broker shape (internal/webapp api_routes.go
         * brokerResponse): {"clientSecret":{"value":"ek_..."},"model":...}.
         * Missing this spelling burned a broker concurrency slot per retry
         * (mint 2xx server-side, token unread client-side) until the user
         * hit the 3-session 429 lock. */
        token = json_str_at(root, "clientSecret", "value");
    }
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
        set_err(err_out, "bad_response", "No ephemeral token in broker response", false);
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
    if (err != ESP_OK) {
        /* A 2xx we couldn't parse still consumed a broker concurrency slot —
         * make the mismatch loud instead of silently re-minting. */
        ESP_LOGE(TAG, "broker 2xx but unusable body (%s): %.*s",
                 (err_out != NULL) ? err_out->message : "?",
                 (int)(body_len > 300 ? 300 : body_len), s_body);
    }
    if (err == ESP_OK) {
        if (out->mode == LN_RT_ENGINE_NOVA_BRIDGE) {
            ESP_LOGI(TAG, "nova-bridge session bootstrapped");
        } else if (out->mode == LN_RT_ENGINE_GEMINI_DIRECT) {
            ESP_LOGI(TAG, "gemini-direct session bootstrapped (model=%s, setup %u B)",
                     out->model, (unsigned)strlen(out->setup_frame));
        } else {
            ESP_LOGI(TAG, "ephemeral session minted (model=%s)", out->model);
        }
    }
    /* Best-effort scrub of the JWT copy between sessions. */
    memset(s_jwt, 0, sizeof(s_jwt));
    memset(s_auth_hdr, 0, sizeof(s_auth_hdr));
    return err;
}
