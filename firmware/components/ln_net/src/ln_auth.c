/*
 * ln_auth.c — device credential lineage (plan.md M5 §6, Auth §6).
 *
 * Owns the NVS-persisted 10-year refresh token (wire form
 * "sessionId.secret" from the pairing claim) and the in-RAM 15-minute
 * access JWT. Two refresh triggers, one code path (POST /auth/refresh):
 *
 *   - silent rotation task: every CONFIG_LN_AUTH_ROTATE_PERIOD_S (24h) a
 *     device that never misses a check-in effectively never re-pairs;
 *   - on-demand: ln_auth_get_jwt() finds the cached JWT expiring and blocks
 *     while the auth task rotates. The HTTPS work always runs on the auth
 *     task's stack, never the caller's.
 *
 * A 401 from /auth/refresh (invalid/reused/revoked/unavailable) is the
 * canonical "credential lineage dead" signal: stored auth is wiped and
 * LN_NET_EVENT_AUTH_INVALID fires so ctrl can route Error -> Provisioning.
 *
 * The rotated refresh token is committed to NVS *before* the in-memory swap
 * — after the server rotates, presenting the previous token again would
 * trip reuse detection and revoke the whole family, so the persisted copy
 * must always be the newest one we know the server accepted.
 *
 * NVS encryption (flash enc + NVS enc) arrives with the M5 "10-yr
 * persistence" task; the namespace layout here doesn't change for it.
 */
#include <stdio.h>
#include <string.h>
#include <time.h>

#include "freertos/FreeRTOS.h"
#include "freertos/event_groups.h"
#include "freertos/semphr.h"
#include "freertos/task.h"

#include "cJSON.h"
#include "esp_log.h"
#include "nvs.h"
#include "sdkconfig.h"

#include "ln_net_priv.h"

static const char *TAG = "ln_auth";

#define JWT_MAX      2048
#define REFRESH_MAX  192
#define JWT_MARGIN_S 60          /* refresh when this close to expiry */
#define REFRESH_WAIT_MS 20000    /* get_jwt blocking budget */

#define BIT_REQ       BIT0
#define BIT_DONE_OK   BIT1
#define BIT_DONE_FAIL BIT2

static SemaphoreHandle_t s_lock;
static EventGroupHandle_t s_ev;
static TaskHandle_t s_task;

static bool s_paired;
static bool s_online_hint;
static char s_refresh[REFRESH_MAX];
static char s_device_id[48];
static char s_thing_name[64];
static char s_cert_arn[128];
static char s_jwt[JWT_MAX];
static int64_t s_jwt_exp;        /* unix seconds */

bool ln_auth_is_paired(void)
{
    return s_paired;
}

/* ------------------------------------------------------------------ NVS */

static esp_err_t auth_nvs_set_str(nvs_handle_t h, const char *key, const char *v)
{
    return nvs_set_str(h, key, v ? v : "");
}

static esp_err_t auth_persist_refresh(const char *refresh_wire)
{
    nvs_handle_t h;
    esp_err_t err = nvs_open(LN_NVS_NS_AUTH, NVS_READWRITE, &h);
    if (err != ESP_OK) {
        return err;
    }
    err = nvs_set_str(h, "refresh", refresh_wire);
    if (err == ESP_OK) {
        err = nvs_commit(h);
    }
    nvs_close(h);
    return err;
}

static void auth_nvs_load(void)
{
    nvs_handle_t h;
    if (nvs_open(LN_NVS_NS_AUTH, NVS_READONLY, &h) != ESP_OK) {
        return;
    }
    size_t l;
    l = sizeof(s_refresh);
    esp_err_t e1 = nvs_get_str(h, "refresh", s_refresh, &l);
    l = sizeof(s_device_id);
    esp_err_t e2 = nvs_get_str(h, "device_id", s_device_id, &l);
    l = sizeof(s_thing_name);
    nvs_get_str(h, "thing_name", s_thing_name, &l);
    l = sizeof(s_cert_arn);
    nvs_get_str(h, "cert_arn", s_cert_arn, &l);
    nvs_close(h);
    if (e1 == ESP_OK && e2 == ESP_OK &&
        s_refresh[0] != '\0' && s_device_id[0] != '\0') {
        s_paired = true;
    }
}

esp_err_t ln_auth_store_claim(const char *device_id, const char *refresh_wire,
                              const char *access_jwt, int64_t access_exp,
                              const char *thing_name, const char *cert_arn)
{
    if (device_id == NULL || refresh_wire == NULL || access_jwt == NULL ||
        strlen(refresh_wire) >= REFRESH_MAX || strlen(access_jwt) >= JWT_MAX) {
        return ESP_ERR_INVALID_ARG;
    }

    nvs_handle_t h;
    esp_err_t err = nvs_open(LN_NVS_NS_AUTH, NVS_READWRITE, &h);
    if (err != ESP_OK) {
        return err;
    }
    err = auth_nvs_set_str(h, "refresh", refresh_wire);
    if (err == ESP_OK) {
        err = auth_nvs_set_str(h, "device_id", device_id);
    }
    if (err == ESP_OK) {
        err = auth_nvs_set_str(h, "thing_name", thing_name);
    }
    if (err == ESP_OK) {
        err = auth_nvs_set_str(h, "cert_arn", cert_arn);
    }
    if (err == ESP_OK) {
        err = nvs_commit(h);
    }
    nvs_close(h);
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "persisting pairing claim failed: %s", esp_err_to_name(err));
        return err;
    }

    xSemaphoreTake(s_lock, portMAX_DELAY);
    strlcpy(s_refresh, refresh_wire, sizeof(s_refresh));
    strlcpy(s_device_id, device_id, sizeof(s_device_id));
    strlcpy(s_thing_name, thing_name ? thing_name : "", sizeof(s_thing_name));
    strlcpy(s_cert_arn, cert_arn ? cert_arn : "", sizeof(s_cert_arn));
    strlcpy(s_jwt, access_jwt, sizeof(s_jwt));
    s_jwt_exp = access_exp;
    s_paired = true;
    xSemaphoreGive(s_lock);
    ESP_LOGI(TAG, "paired as %s (thing \"%s\")", device_id,
             thing_name ? thing_name : "");
    return ESP_OK;
}

esp_err_t ln_auth_wipe(void)
{
    nvs_handle_t h;
    if (nvs_open(LN_NVS_NS_AUTH, NVS_READWRITE, &h) == ESP_OK) {
        nvs_erase_all(h);
        nvs_commit(h);
        nvs_close(h);
    }
    xSemaphoreTake(s_lock, portMAX_DELAY);
    s_paired = false;
    s_refresh[0] = s_device_id[0] = s_thing_name[0] = s_cert_arn[0] = '\0';
    s_jwt[0] = '\0';
    s_jwt_exp = 0;
    xSemaphoreGive(s_lock);
    return ESP_OK;
}

/* -------------------------------------------------------------- refresh */

/* One rotation round-trip. Runs on the auth task only. */
static esp_err_t do_refresh(void)
{
    char refresh[REFRESH_MAX];
    xSemaphoreTake(s_lock, portMAX_DELAY);
    strlcpy(refresh, s_refresh, sizeof(refresh));
    xSemaphoreGive(s_lock);
    if (refresh[0] == '\0') {
        return ESP_ERR_INVALID_STATE;
    }

    char body[REFRESH_MAX + 32];
    snprintf(body, sizeof(body), "{\"refreshToken\":\"%s\"}", refresh);

    /* File-static: LN_BACKEND_RSP_MAX is far too big for the task stack;
     * do_refresh only ever runs on the single auth task. */
    static ln_backend_rsp_t rsp;
    esp_err_t err = ln_backend_request("/auth/refresh", body, NULL, &rsp);
    if (err != ESP_OK) {
        return err;   /* transport — retry later, lineage intact */
    }

    if (rsp.status == 401) {
        /* invalid_refresh_token / refresh_reused / session_revoked /
         * account_unavailable — the lineage is dead either way. */
        ESP_LOGE(TAG, "refresh rejected (401): %.128s — wiping credentials",
                 rsp.body);
        ln_auth_wipe();
        esp_event_post(LN_NET_EVENT, LN_NET_EVENT_AUTH_INVALID, NULL, 0, 0);
        return ESP_ERR_INVALID_STATE;
    }
    if (rsp.status != 200) {
        ESP_LOGW(TAG, "refresh -> HTTP %d", rsp.status);
        return ESP_FAIL;
    }

    cJSON *root = cJSON_Parse(rsp.body);
    if (root == NULL) {
        return ESP_FAIL;
    }
    err = ESP_FAIL;
    const cJSON *jacc = cJSON_GetObjectItemCaseSensitive(root, "accessToken");
    const cJSON *jexp = cJSON_GetObjectItemCaseSensitive(root, "expiresAt");
    const cJSON *jref = cJSON_GetObjectItemCaseSensitive(root, "refreshToken");
    if (cJSON_IsString(jacc) && strlen(jacc->valuestring) < JWT_MAX &&
        cJSON_IsString(jref) && strlen(jref->valuestring) < REFRESH_MAX) {
        /* Persist the rotated refresh token BEFORE swapping memory (see
         * file header — the old token is burned server-side already). */
        err = auth_persist_refresh(jref->valuestring);
        if (err != ESP_OK) {
            ESP_LOGE(TAG, "NVS persist of rotated refresh failed: %s "
                     "(continuing with in-RAM copy)", esp_err_to_name(err));
        }
        xSemaphoreTake(s_lock, portMAX_DELAY);
        strlcpy(s_refresh, jref->valuestring, sizeof(s_refresh));
        strlcpy(s_jwt, jacc->valuestring, sizeof(s_jwt));
        s_jwt_exp = cJSON_IsNumber(jexp) ? (int64_t)jexp->valuedouble : 0;
        xSemaphoreGive(s_lock);
        err = ESP_OK;
        esp_event_post(LN_NET_EVENT, LN_NET_EVENT_AUTH_READY, NULL, 0, 0);
        ESP_LOGI(TAG, "refresh rotated; access JWT valid until %lld",
                 (long long)s_jwt_exp);
    }
    cJSON_Delete(root);
    return err;
}

static bool jwt_is_fresh(void)
{
    time_t now = time(NULL);
    bool fresh;
    xSemaphoreTake(s_lock, portMAX_DELAY);
    fresh = (s_jwt[0] != '\0') && (s_jwt_exp - JWT_MARGIN_S > (int64_t)now);
    xSemaphoreGive(s_lock);
    return fresh;
}

static void auth_task(void *arg)
{
    int64_t next_rotation = 0;

    for (;;) {
        TickType_t wait = pdMS_TO_TICKS(60 * 1000);
        time_t now = time(NULL);
        bool usable = s_paired && s_online_hint && ln_net_is_online();
        bool due = usable &&
                   (next_rotation == 0 || (int64_t)now >= next_rotation);

        EventBits_t bits = 0;
        if (!due) {
            bits = xEventGroupWaitBits(s_ev, BIT_REQ, pdTRUE, pdFALSE, wait);
            usable = s_paired && s_online_hint && ln_net_is_online();
        }
        if (!usable) {
            continue;
        }
        if (!due && !(bits & BIT_REQ)) {
            continue;
        }

        xEventGroupClearBits(s_ev, BIT_DONE_OK | BIT_DONE_FAIL);
        esp_err_t err = do_refresh();
        if (err == ESP_OK) {
            next_rotation = (int64_t)time(NULL) + CONFIG_LN_AUTH_ROTATE_PERIOD_S;
            xEventGroupSetBits(s_ev, BIT_DONE_OK);
        } else {
            /* Transient failure: retry in 60s via the `due` path. Fatal
             * (wiped) states simply stop being `paired`. */
            if (err != ESP_ERR_INVALID_STATE) {
                next_rotation = (int64_t)time(NULL) + 60;
            }
            xEventGroupSetBits(s_ev, BIT_DONE_FAIL);
        }
    }
}

/* ------------------------------------------------------------ public API */

esp_err_t ln_auth_get_jwt(char *buf, size_t len)
{
    if (buf == NULL || len == 0) {
        return ESP_ERR_INVALID_ARG;
    }
    if (!s_paired) {
        return ESP_ERR_INVALID_STATE;
    }

    if (!jwt_is_fresh()) {
        xEventGroupClearBits(s_ev, BIT_DONE_OK | BIT_DONE_FAIL);
        xEventGroupSetBits(s_ev, BIT_REQ);
        xEventGroupWaitBits(s_ev, BIT_DONE_OK | BIT_DONE_FAIL,
                            pdFALSE, pdFALSE, pdMS_TO_TICKS(REFRESH_WAIT_MS));
        if (!s_paired) {
            return ESP_ERR_INVALID_STATE;   /* wiped mid-wait (401) */
        }
        if (!jwt_is_fresh()) {
            return ESP_ERR_TIMEOUT;
        }
    }

    esp_err_t ret = ESP_OK;
    xSemaphoreTake(s_lock, portMAX_DELAY);
    if (strlen(s_jwt) + 1 > len) {
        ret = ESP_ERR_NO_MEM;
    } else {
        strcpy(buf, s_jwt);
    }
    xSemaphoreGive(s_lock);
    return ret;
}

esp_err_t ln_auth_get_device_id(char *buf, size_t len)
{
    if (buf == NULL || len == 0) {
        return ESP_ERR_INVALID_ARG;
    }
    if (!s_paired) {
        return ESP_ERR_INVALID_STATE;
    }
    xSemaphoreTake(s_lock, portMAX_DELAY);
    strlcpy(buf, s_device_id, len);
    xSemaphoreGive(s_lock);
    return ESP_OK;
}

void ln_auth_force_refresh(void)
{
    if (s_ev != NULL) {
        xEventGroupSetBits(s_ev, BIT_REQ);
    }
}

void ln_auth_on_online(void)
{
    s_online_hint = true;
    /* Rotate promptly on (re)connect so the persisted lineage stays warm
     * and the first realtime-session mint has a fresh JWT waiting. */
    ln_auth_force_refresh();
}

esp_err_t ln_auth_init(void)
{
    if (s_lock != NULL) {
        return ESP_OK;
    }
    s_lock = xSemaphoreCreateMutex();
    s_ev = xEventGroupCreate();
    if (s_lock == NULL || s_ev == NULL) {
        return ESP_ERR_NO_MEM;
    }
    auth_nvs_load();
    /* HTTPS runs on this stack. */
    if (xTaskCreate(auth_task, "ln_auth", 8192, NULL, 5, &s_task) != pdPASS) {
        return ESP_ERR_NO_MEM;
    }
    ESP_LOGI(TAG, "init: %s", s_paired ? "paired (refresh lineage loaded)"
                                       : "unpaired");
    return ESP_OK;
}
