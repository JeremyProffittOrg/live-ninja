/*
 * ln_pairing.c — device pairing against contracts/api.md:
 *
 *   1. Generate a PKCE code_verifier on-chip (32 random bytes, base64url)
 *      and its S256 code_challenge.
 *   2. POST /auth/device/pair/start {codeChallenge} -> {nonce, claimUrl,
 *      pollIntervalSeconds}. The claim URL goes to the LCD (QR) and the
 *      SoftAP portal page; a human opens it in a real browser and signs in
 *      with Amazon (backend runs the confidential LWA leg + owner/allowlist
 *      gate, binds the device, mints the 10-yr family identity).
 *   3. POST /auth/device/pair/poll {nonce, codeVerifier} until the backend
 *      answers status "bound" — that single response carries the one-time
 *      plaintext 10-year refresh token, deviceId, first access JWT, and the
 *      IoT provisioning claim. Persisted immediately via ln_auth_store_claim.
 *
 * Expired/consumed nonces (404 / 410 / 403) restart registration with a
 * fresh verifier — the device never reuses PKCE material across nonces.
 */
#include <stdio.h>
#include <string.h>

#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"
#include "freertos/task.h"

#include "cJSON.h"
#include "esp_log.h"
#include "esp_random.h"
#include "mbedtls/sha256.h"

#include "ln_iot.h"
#include "ln_net_priv.h"

static const char *TAG = "ln_pairing";

#define PAIR_DEFAULT_POLL_S 3
#define PAIR_TTL_S          900   /* backend PairTTL (15 min) */

/* Response buffer is LN_BACKEND_RSP_MAX (8KB) — file-static, used only by
 * the single ln_net task that runs ln_pairing_run(). */
static ln_backend_rsp_t s_rsp;

static SemaphoreHandle_t s_lock;
static char s_claim_url[256];

static void claim_url_set(const char *url)
{
    xSemaphoreTake(s_lock, portMAX_DELAY);
    strlcpy(s_claim_url, url ? url : "", sizeof(s_claim_url));
    xSemaphoreGive(s_lock);
}

void ln_pairing_get_claim_url(char *buf, size_t len)
{
    if (s_lock == NULL) {
        if (len) {
            buf[0] = '\0';
        }
        return;
    }
    xSemaphoreTake(s_lock, portMAX_DELAY);
    strlcpy(buf, s_claim_url, len);
    xSemaphoreGive(s_lock);
}

/* verifier: 43-char base64url of 32 random bytes; challenge: S256. */
static esp_err_t make_pkce(char *verifier, size_t vlen,
                           char *challenge, size_t clen)
{
    uint8_t raw[32];
    esp_fill_random(raw, sizeof(raw));
    esp_err_t err = ln_b64url_encode(raw, sizeof(raw), verifier, vlen);
    if (err != ESP_OK) {
        return err;
    }
    uint8_t digest[32];
    if (mbedtls_sha256((const unsigned char *)verifier, strlen(verifier),
                       digest, 0) != 0) {
        return ESP_FAIL;
    }
    return ln_b64url_encode(digest, sizeof(digest), challenge, clen);
}

/* POST /auth/device/pair/start -> nonce + claimUrl + poll interval. */
static esp_err_t pair_register(const char *challenge, char *nonce, size_t nlen,
                               int *poll_s)
{
    char body[192];
    snprintf(body, sizeof(body), "{\"codeChallenge\":\"%s\"}", challenge);

    esp_err_t err = ln_backend_request("/auth/device/pair/start", body, NULL, &s_rsp);
    if (err != ESP_OK) {
        return err;
    }
    if (s_rsp.status != 200 && s_rsp.status != 201) {
        ESP_LOGW(TAG, "pair/start -> HTTP %d", s_rsp.status);
        return ESP_FAIL;
    }

    cJSON *root = cJSON_Parse(s_rsp.body);
    if (root == NULL) {
        return ESP_FAIL;
    }
    err = ESP_FAIL;
    const cJSON *jn = cJSON_GetObjectItemCaseSensitive(root, "nonce");
    const cJSON *ju = cJSON_GetObjectItemCaseSensitive(root, "claimUrl");
    const cJSON *jp = cJSON_GetObjectItemCaseSensitive(root, "pollIntervalSeconds");
    if (cJSON_IsString(jn) && jn->valuestring[0] != '\0' && cJSON_IsString(ju)) {
        strlcpy(nonce, jn->valuestring, nlen);
        claim_url_set(ju->valuestring);
        *poll_s = (cJSON_IsNumber(jp) && jp->valueint > 0)
                      ? jp->valueint : PAIR_DEFAULT_POLL_S;
        err = ESP_OK;
    }
    cJSON_Delete(root);
    return err;
}

/* One poll/claim round. Returns:
 *   ESP_OK             — claimed; credentials persisted
 *   ESP_ERR_NOT_FOUND  — nonce expired/consumed; caller re-registers
 *   ESP_ERR_TIMEOUT    — still pending; keep polling
 *   other              — transport/server error; caller backs off        */
static esp_err_t pair_poll_once(const char *nonce, const char *verifier)
{
    char body[256];
    snprintf(body, sizeof(body),
             "{\"nonce\":\"%s\",\"codeVerifier\":\"%s\"}", nonce, verifier);

    esp_err_t err = ln_backend_request("/auth/device/pair/poll", body, NULL, &s_rsp);
    if (err != ESP_OK) {
        return err;
    }
    switch (s_rsp.status) {
    case 200:
        break;
    case 404:   /* expired */
    case 410:   /* already_claimed */
    case 403:   /* verifier_mismatch — not our pairing anymore */
        ESP_LOGW(TAG, "pair/poll -> HTTP %d; re-registering", s_rsp.status);
        return ESP_ERR_NOT_FOUND;
    default:
        ESP_LOGW(TAG, "pair/poll -> HTTP %d", s_rsp.status);
        return ESP_FAIL;
    }

    cJSON *root = cJSON_Parse(s_rsp.body);
    if (root == NULL) {
        return ESP_FAIL;
    }

    esp_err_t ret = ESP_FAIL;
    const cJSON *jstatus = cJSON_GetObjectItemCaseSensitive(root, "status");
    if (cJSON_IsString(jstatus) && strcmp(jstatus->valuestring, "pending") == 0) {
        ret = ESP_ERR_TIMEOUT;
    } else if (cJSON_IsString(jstatus) && strcmp(jstatus->valuestring, "bound") == 0) {
        const cJSON *jdev  = cJSON_GetObjectItemCaseSensitive(root, "deviceId");
        const cJSON *jref  = cJSON_GetObjectItemCaseSensitive(root, "refreshToken");
        const cJSON *jacc  = cJSON_GetObjectItemCaseSensitive(root, "accessToken");
        const cJSON *jexp  = cJSON_GetObjectItemCaseSensitive(root, "expiresAt");
        const cJSON *jthg  = cJSON_GetObjectItemCaseSensitive(root, "thingName");
        const cJSON *jcert = cJSON_GetObjectItemCaseSensitive(root, "certArn");
        if (cJSON_IsString(jdev) && cJSON_IsString(jref) && cJSON_IsString(jacc)) {
            ret = ln_auth_store_claim(
                jdev->valuestring,
                jref->valuestring,
                jacc->valuestring,
                cJSON_IsNumber(jexp) ? (int64_t)jexp->valuedouble : 0,
                cJSON_IsString(jthg) ? jthg->valuestring : "",
                cJSON_IsString(jcert) ? jcert->valuestring : "");
            if (ret == ESP_OK) {
                /* Hand IoT bootstrap material to ln_iot (ln_iot.h contract:
                 * NULL fields keep stored values). Today the backend's
                 * ProvisionIoT seam returns no claim material yet — the
                 * deviceId (and any fields that do appear) are stored so
                 * fleet provisioning starts the moment the backend ships
                 * them in this same response. */
                const cJSON *jep  = cJSON_GetObjectItemCaseSensitive(root, "iotEndpoint");
                const cJSON *jcc  = cJSON_GetObjectItemCaseSensitive(root, "claimCertificatePem");
                const cJSON *jck  = cJSON_GetObjectItemCaseSensitive(root, "claimPrivateKeyPem");
                const cJSON *jtpl = cJSON_GetObjectItemCaseSensitive(root, "provisioningTemplate");
                const cJSON *juid = cJSON_GetObjectItemCaseSensitive(root, "userId");
                ln_iot_bootstrap_t bs = {
                    .iot_endpoint   = cJSON_IsString(jep) ? jep->valuestring : NULL,
                    .claim_cert_pem = cJSON_IsString(jcc) ? jcc->valuestring : NULL,
                    .claim_key_pem  = cJSON_IsString(jck) ? jck->valuestring : NULL,
                    .template_name  = cJSON_IsString(jtpl) ? jtpl->valuestring : NULL,
                    .device_id      = jdev->valuestring,
                    .user_id        = cJSON_IsString(juid) ? juid->valuestring : NULL,
                };
                esp_err_t iot_err = ln_iot_store_bootstrap(&bs);
                if (iot_err != ESP_OK) {
                    ESP_LOGW(TAG, "ln_iot bootstrap store failed: %s",
                             esp_err_to_name(iot_err));
                }
                char device_id[48];
                strlcpy(device_id, jdev->valuestring, sizeof(device_id));
                esp_event_post(LN_NET_EVENT, LN_NET_EVENT_PAIRED,
                               device_id, sizeof(device_id), 0);
                esp_event_post(LN_NET_EVENT, LN_NET_EVENT_AUTH_READY, NULL, 0, 0);
            }
        }
    }
    cJSON_Delete(root);
    return ret;
}

esp_err_t ln_pairing_run(void)
{
    if (s_lock == NULL) {
        s_lock = xSemaphoreCreateMutex();
        if (s_lock == NULL) {
            return ESP_ERR_NO_MEM;
        }
    }

    char verifier[64], challenge[64], nonce[96];
    int poll_s = PAIR_DEFAULT_POLL_S;
    bool registered = false;
    int backoff_s = 2;
    int64_t polls_left = 0;

    while (ln_net_is_online()) {
        if (!registered) {
            esp_err_t err = make_pkce(verifier, sizeof(verifier),
                                      challenge, sizeof(challenge));
            if (err != ESP_OK) {
                return err;
            }
            err = pair_register(challenge, nonce, sizeof(nonce), &poll_s);
            if (err != ESP_OK) {
                ESP_LOGW(TAG, "register failed; retry in %ds", backoff_s);
                vTaskDelay(pdMS_TO_TICKS(backoff_s * 1000));
                backoff_s = (backoff_s < 60) ? backoff_s * 2 : 60;
                continue;
            }
            backoff_s = 2;
            registered = true;
            polls_left = PAIR_TTL_S / poll_s;

            ln_net_pairing_info_t info = {0};
            ln_pairing_get_claim_url(info.claim_url, sizeof(info.claim_url));
            info.expires_in_s = PAIR_TTL_S;
            esp_event_post(LN_NET_EVENT, LN_NET_EVENT_PAIRING_STARTED,
                           &info, sizeof(info), 0);
            ESP_LOGI(TAG, "pairing registered; claim at %s", info.claim_url);
        }

        vTaskDelay(pdMS_TO_TICKS(poll_s * 1000));
        esp_err_t err = pair_poll_once(nonce, verifier);
        if (err == ESP_OK) {
            claim_url_set("");
            return ESP_OK;
        }
        if (err == ESP_ERR_NOT_FOUND || --polls_left <= 0) {
            registered = false;     /* fresh nonce + fresh PKCE material */
        }
        /* ESP_ERR_TIMEOUT (pending) and transient failures just loop. */
    }
    claim_url_set("");
    return ESP_ERR_INVALID_STATE;   /* link dropped mid-pairing */
}
