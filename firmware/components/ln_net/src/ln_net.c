/*
 * ln_net.c — WiFi bring-up (C6 radio via esp_wifi_remote/esp-hosted) and
 * the network state machine task.
 *
 * Boot decision (plan.md M5 state diagram):
 *   creds in NVS  -> STA connect (retry N times, then raise the portal but
 *                    keep retrying in the background)
 *   no creds      -> SoftAP captive portal immediately
 *   online+unpaired -> pairing loop (ln_pairing.c), portal stays up so the
 *                    phone page can hand the human to the claim URL
 *   online+paired -> auth rotation task owns credentials (ln_auth.c)
 */
#include <string.h>
#include <time.h>

#include "freertos/FreeRTOS.h"
#include "freertos/event_groups.h"
#include "freertos/task.h"

#include "esp_event.h"
#include "esp_log.h"
#include "esp_netif.h"
#include "esp_netif_sntp.h"
#include "esp_wifi.h"
#include "nvs.h"
#include "sdkconfig.h"

#include "bsp/m5stack_tab5.h"

#include "ln_iot.h"
#include "ln_net.h"
#include "ln_net_priv.h"

static const char *TAG = "ln_net";

ESP_EVENT_DEFINE_BASE(LN_NET_EVENT);

/* Event-group bits used to synchronise the state task with WiFi events. */
#define BIT_GOT_IP        BIT0
#define BIT_STA_FAIL      BIT1
#define BIT_CREDS_APPLIED BIT2
#define BIT_LINK_DOWN     BIT3

static EventGroupHandle_t s_bits;
static SemaphoreHandle_t s_lock;          /* guards s_status + s_creds */
static esp_netif_t *s_sta_netif;
static esp_netif_t *s_ap_netif;
static TaskHandle_t s_task;
static bool s_portal_up;
static bool s_online;
static bool s_sntp_started;

static ln_wifi_status_t s_status;         /* portal-visible snapshot */
static char s_ssid[33];
static char s_pass[65];
static bool s_have_creds;

/* ---------------------------------------------------------------- utils */

static void status_set(ln_wifi_state_t st, ln_net_wifi_fail_reason_t why)
{
    xSemaphoreTake(s_lock, portMAX_DELAY);
    s_status.state = st;
    s_status.fail_reason = why;
    strlcpy(s_status.ssid, s_ssid, sizeof(s_status.ssid));
    if (st != LN_WIFI_CONNECTED) {
        s_status.ip[0] = '\0';
    }
    xSemaphoreGive(s_lock);
}

void ln_net_get_wifi_status(ln_wifi_status_t *out)
{
    xSemaphoreTake(s_lock, portMAX_DELAY);
    *out = s_status;
    xSemaphoreGive(s_lock);
}

bool ln_net_is_online(void)
{
    return s_online;
}

bool ln_net_wifi_is_provisioned(void)
{
    return s_have_creds;
}

bool ln_net_is_paired(void)
{
    return ln_auth_is_paired();
}

esp_err_t ln_net_get_claim_url(char *buf, size_t len)
{
    if (buf == NULL || len == 0) {
        return ESP_ERR_INVALID_ARG;
    }
    ln_pairing_get_claim_url(buf, len);
    return ESP_OK;
}

/* ------------------------------------------------------------- NVS creds */

static void creds_load(void)
{
    nvs_handle_t h;
    if (nvs_open(LN_NVS_NS_NET, NVS_READONLY, &h) != ESP_OK) {
        return;
    }
    size_t sl = sizeof(s_ssid), pl = sizeof(s_pass);
    esp_err_t e1 = nvs_get_str(h, "ssid", s_ssid, &sl);
    esp_err_t e2 = nvs_get_str(h, "pass", s_pass, &pl);
    nvs_close(h);
    if (e1 == ESP_OK && s_ssid[0] != '\0') {
        if (e2 != ESP_OK) {
            s_pass[0] = '\0';   /* open network */
        }
        s_have_creds = true;
    }
}

static esp_err_t creds_save(const char *ssid, const char *pass)
{
    nvs_handle_t h;
    esp_err_t err = nvs_open(LN_NVS_NS_NET, NVS_READWRITE, &h);
    if (err != ESP_OK) {
        return err;
    }
    err = nvs_set_str(h, "ssid", ssid);
    if (err == ESP_OK) {
        err = nvs_set_str(h, "pass", pass ? pass : "");
    }
    if (err == ESP_OK) {
        err = nvs_commit(h);
    }
    nvs_close(h);
    return err;
}

esp_err_t ln_net_clear_provisioning(void)
{
    nvs_handle_t h;
    if (nvs_open(LN_NVS_NS_NET, NVS_READWRITE, &h) == ESP_OK) {
        nvs_erase_all(h);
        nvs_commit(h);
        nvs_close(h);
    }
    xSemaphoreTake(s_lock, portMAX_DELAY);
    s_have_creds = false;
    s_ssid[0] = s_pass[0] = '\0';
    xSemaphoreGive(s_lock);
    ln_auth_wipe();
    esp_wifi_disconnect();
    xEventGroupSetBits(s_bits, BIT_LINK_DOWN);
    return ESP_OK;
}

esp_err_t ln_net_reprovision_wifi(void)
{
    nvs_handle_t h;
    if (nvs_open(LN_NVS_NS_NET, NVS_READWRITE, &h) == ESP_OK) {
        nvs_erase_key(h, "ssid");   /* auth lives in LN_NVS_NS_AUTH — kept */
        nvs_erase_key(h, "pass");
        nvs_commit(h);
        nvs_close(h);
    }
    xSemaphoreTake(s_lock, portMAX_DELAY);
    s_have_creds = false;
    s_ssid[0] = s_pass[0] = '\0';
    xSemaphoreGive(s_lock);
    status_set(LN_WIFI_IDLE, LN_NET_WIFI_FAIL_OTHER);
    esp_wifi_disconnect();
    xEventGroupSetBits(s_bits, BIT_LINK_DOWN);
    return ESP_OK;
}

esp_err_t ln_net_apply_wifi_credentials(const char *ssid, const char *pass)
{
    if (ssid == NULL || ssid[0] == '\0' || strlen(ssid) > 32 ||
        (pass != NULL && strlen(pass) > 64)) {
        return ESP_ERR_INVALID_ARG;
    }
    esp_err_t err = creds_save(ssid, pass ? pass : "");
    if (err != ESP_OK) {
        return err;
    }
    xSemaphoreTake(s_lock, portMAX_DELAY);
    strlcpy(s_ssid, ssid, sizeof(s_ssid));
    strlcpy(s_pass, pass ? pass : "", sizeof(s_pass));
    s_have_creds = true;
    xSemaphoreGive(s_lock);
    status_set(LN_WIFI_CONNECTING, LN_NET_WIFI_FAIL_OTHER);
    xEventGroupSetBits(s_bits, BIT_CREDS_APPLIED);
    return ESP_OK;
}

/* ---------------------------------------------------------- WiFi events */

static ln_net_wifi_fail_reason_t classify_reason(uint8_t reason)
{
    switch (reason) {
    case WIFI_REASON_NO_AP_FOUND:
        return LN_NET_WIFI_FAIL_AP_NOT_FOUND;
    case WIFI_REASON_AUTH_EXPIRE:
    case WIFI_REASON_AUTH_FAIL:
    case WIFI_REASON_MIC_FAILURE:
    case WIFI_REASON_4WAY_HANDSHAKE_TIMEOUT:
    case WIFI_REASON_HANDSHAKE_TIMEOUT:
        return LN_NET_WIFI_FAIL_AUTH;
    default:
        return LN_NET_WIFI_FAIL_OTHER;
    }
}

static void wifi_event_handler(void *arg, esp_event_base_t base,
                               int32_t id, void *data)
{
    if (base == WIFI_EVENT && id == WIFI_EVENT_STA_DISCONNECTED) {
        wifi_event_sta_disconnected_t *d = data;
        ln_net_wifi_fail_reason_t why = classify_reason(d->reason);
        ESP_LOGW(TAG, "STA disconnected from \"%s\" (reason %d)",
                 (const char *)d->ssid, d->reason);
        bool was_online = s_online;
        s_online = false;
        status_set(LN_WIFI_FAILED, why);
        xEventGroupSetBits(s_bits, BIT_STA_FAIL | (was_online ? BIT_LINK_DOWN : 0));
    } else if (base == IP_EVENT && id == IP_EVENT_STA_GOT_IP) {
        ip_event_got_ip_t *ip = data;
        xSemaphoreTake(s_lock, portMAX_DELAY);
        snprintf(s_status.ip, sizeof(s_status.ip), IPSTR, IP2STR(&ip->ip_info.ip));
        s_status.state = LN_WIFI_CONNECTED;
        xSemaphoreGive(s_lock);
        s_online = true;
        xEventGroupSetBits(s_bits, BIT_GOT_IP);
    }
}

/* --------------------------------------------------------------- connect */

static void post_wifi_event(ln_net_event_id_t id)
{
    if (id == LN_NET_EVENT_WIFI_CONNECTED) {
        ln_net_wifi_info_t info = {0};
        wifi_ap_record_t ap;
        xSemaphoreTake(s_lock, portMAX_DELAY);
        strlcpy(info.ssid, s_status.ssid, sizeof(info.ssid));
        strlcpy(info.ip, s_status.ip, sizeof(info.ip));
        xSemaphoreGive(s_lock);
        if (esp_wifi_sta_get_ap_info(&ap) == ESP_OK) {
            info.rssi = ap.rssi;
        }
        esp_event_post(LN_NET_EVENT, id, &info, sizeof(info), 0);
    } else {
        esp_event_post(LN_NET_EVENT, id, NULL, 0, 0);
    }
}

/* One connect attempt; true on IP acquired. */
static bool sta_try_connect(int retry_count)
{
    wifi_config_t cfg = {0};
    xSemaphoreTake(s_lock, portMAX_DELAY);
    strlcpy((char *)cfg.sta.ssid, s_ssid, sizeof(cfg.sta.ssid));
    strlcpy((char *)cfg.sta.password, s_pass, sizeof(cfg.sta.password));
    xSemaphoreGive(s_lock);
    cfg.sta.threshold.authmode = s_pass[0] ? WIFI_AUTH_WPA_PSK : WIFI_AUTH_OPEN;
    cfg.sta.scan_method = WIFI_ALL_CHANNEL_SCAN;
    cfg.sta.sort_method = WIFI_CONNECT_AP_BY_SIGNAL;

    char ssid_evt[33];
    strlcpy(ssid_evt, s_ssid, sizeof(ssid_evt));
    esp_event_post(LN_NET_EVENT, LN_NET_EVENT_WIFI_CONNECTING,
                   ssid_evt, sizeof(ssid_evt), 0);
    status_set(LN_WIFI_CONNECTING, LN_NET_WIFI_FAIL_OTHER);

    xEventGroupClearBits(s_bits, BIT_GOT_IP | BIT_STA_FAIL);
    esp_wifi_disconnect();
    esp_err_t err = esp_wifi_set_config(WIFI_IF_STA, &cfg);
    if (err == ESP_OK) {
        err = esp_wifi_connect();
    }
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "connect setup failed: %s", esp_err_to_name(err));
        status_set(LN_WIFI_FAILED, LN_NET_WIFI_FAIL_OTHER);
        return false;
    }

    EventBits_t bits = xEventGroupWaitBits(s_bits, BIT_GOT_IP | BIT_STA_FAIL,
                                           pdTRUE, pdFALSE, pdMS_TO_TICKS(20000));
    if (bits & BIT_GOT_IP) {
        ESP_LOGI(TAG, "connected to \"%s\"", s_ssid);
        post_wifi_event(LN_NET_EVENT_WIFI_CONNECTED);
        return true;
    }

    ln_net_wifi_fail_t fail = {0};
    xSemaphoreTake(s_lock, portMAX_DELAY);
    strlcpy(fail.ssid, s_ssid, sizeof(fail.ssid));
    fail.reason = s_status.fail_reason;
    xSemaphoreGive(s_lock);
    fail.retry_count = retry_count;
    if (!(bits & BIT_STA_FAIL)) {
        /* timed out with no disconnect event */
        esp_wifi_disconnect();
        status_set(LN_WIFI_FAILED, LN_NET_WIFI_FAIL_OTHER);
        fail.reason = LN_NET_WIFI_FAIL_OTHER;
    }
    esp_event_post(LN_NET_EVENT, LN_NET_EVENT_WIFI_DISCONNECTED,
                   &fail, sizeof(fail), 0);
    return false;
}

/* --------------------------------------------------------------- scan */

int ln_net_wifi_scan(void *out_records, int max_records)
{
    wifi_scan_config_t sc = {
        .show_hidden = false,
        .scan_type = WIFI_SCAN_TYPE_ACTIVE,
        .scan_time.active = {.min = 80, .max = 200},
    };
    esp_err_t err = esp_wifi_scan_start(&sc, true /* block */);
    if (err != ESP_OK) {
        ESP_LOGW(TAG, "scan failed: %s", esp_err_to_name(err));
        return -1;
    }
    uint16_t n = (uint16_t)max_records;
    err = esp_wifi_scan_get_ap_records(&n, (wifi_ap_record_t *)out_records);
    if (err != ESP_OK) {
        return -1;
    }
    return (int)n;
}

/* ------------------------------------------------------------ portal glue */

static void portal_ensure_started(void)
{
    if (s_portal_up) {
        return;
    }
    if (ln_portal_start() == ESP_OK) {
        s_portal_up = true;
        ln_net_portal_info_t info = {0};
        strlcpy(info.ap_ssid, CONFIG_LN_PORTAL_AP_SSID, sizeof(info.ap_ssid));
        strlcpy(info.portal_url, "http://" LN_PORTAL_IP_STR, sizeof(info.portal_url));
        esp_event_post(LN_NET_EVENT, LN_NET_EVENT_PORTAL_STARTED,
                       &info, sizeof(info), 0);
    }
}

static void portal_ensure_stopped(void)
{
    if (!s_portal_up) {
        return;
    }
    ln_portal_stop();
    s_portal_up = false;
    esp_event_post(LN_NET_EVENT, LN_NET_EVENT_PORTAL_STOPPED, NULL, 0, 0);
}

esp_netif_t *ln_net_take_ap_netif(void)
{
    if (s_ap_netif == NULL) {
        s_ap_netif = esp_netif_create_default_wifi_ap();
    }
    return s_ap_netif;
}

/* ---------------------------------------------------------------- SNTP */

static void sntp_ensure(void)
{
    if (!s_sntp_started) {
        esp_sntp_config_t cfg = ESP_NETIF_SNTP_DEFAULT_CONFIG("pool.ntp.org");
        cfg.start = true;
        if (esp_netif_sntp_init(&cfg) == ESP_OK) {
            s_sntp_started = true;
        }
    }
    /* TLS cert validation needs a sane clock; wait (bounded) for first sync. */
    time_t now = 0;
    time(&now);
    if (now < 1700000000) { /* clearly unset */
        if (esp_netif_sntp_sync_wait(pdMS_TO_TICKS(15000)) != ESP_OK) {
            ESP_LOGW(TAG, "SNTP sync not confirmed within 15s; HTTPS may retry");
        }
    }
}

/* ------------------------------------------------------------ state task */

static void net_task(void *arg)
{
    int fail_streak = 0;

    for (;;) {
        /* ---------- get online ---------- */
        while (!s_online) {
            if (!s_have_creds) {
                portal_ensure_started();
                /* Wait for the portal to hand us credentials. */
                xEventGroupWaitBits(s_bits, BIT_CREDS_APPLIED, pdTRUE, pdFALSE,
                                    portMAX_DELAY);
                fail_streak = 0;
                continue;
            }

            if (sta_try_connect(fail_streak + 1)) {
                fail_streak = 0;
                break;
            }
            fail_streak++;

            if (fail_streak >= CONFIG_LN_WIFI_CONNECT_RETRIES) {
                /* Stored network unreachable (moved house? changed password?)
                 * — raise the portal but keep retrying in the background. */
                portal_ensure_started();
                EventBits_t b = xEventGroupWaitBits(
                    s_bits, BIT_CREDS_APPLIED, pdTRUE, pdFALSE,
                    pdMS_TO_TICKS(CONFIG_LN_WIFI_RETRY_BACKGROUND_S * 1000));
                if (b & BIT_CREDS_APPLIED) {
                    fail_streak = 0;
                }
            } else {
                /* Small backoff between quick retries; also give a freshly
                 * portal-submitted credential priority. */
                EventBits_t b = xEventGroupWaitBits(s_bits, BIT_CREDS_APPLIED,
                                                    pdTRUE, pdFALSE,
                                                    pdMS_TO_TICKS(2000));
                if (b & BIT_CREDS_APPLIED) {
                    fail_streak = 0;
                }
            }
        }

        /* ---------- online ---------- */
        sntp_ensure();

        if (!ln_auth_is_paired()) {
            /* Blocks until paired or the link drops. Portal (if up) shows
             * the claim URL; the LCD shows it too via PAIRING_STARTED. */
            esp_err_t err = ln_pairing_run();
            if (err == ESP_OK) {
                ESP_LOGI(TAG, "device paired");
            } else if (!s_online) {
                continue;   /* link dropped mid-pairing; reconnect first */
            } else {
                ESP_LOGE(TAG, "pairing failed (%s); retrying in 10s",
                         esp_err_to_name(err));
                vTaskDelay(pdMS_TO_TICKS(10000));
                continue;
            }
        }

        ln_auth_on_online();
        portal_ensure_stopped();

        /* Park until the link drops (or creds are wiped). */
        xEventGroupWaitBits(s_bits, BIT_LINK_DOWN, pdTRUE, pdFALSE,
                            portMAX_DELAY);
        ESP_LOGW(TAG, "link down; reconnecting");
    }
}

/* ----------------------------------------------------------------- init */

/* STA RSSI for ln_iot's device_heartbeat telemetry (ln_iot.h hook). */
static int rssi_provider(void)
{
    wifi_ap_record_t ap;
    if (s_online && esp_wifi_sta_get_ap_info(&ap) == ESP_OK) {
        return ap.rssi;
    }
    return 0;
}

esp_err_t ln_net_init(void)
{
    if (s_lock != NULL) {
        return ESP_ERR_INVALID_STATE;
    }
    s_lock = xSemaphoreCreateMutex();
    s_bits = xEventGroupCreate();
    if (s_lock == NULL || s_bits == NULL) {
        return ESP_ERR_NO_MEM;
    }

    /* Power the ESP32-C6 radio module (IO-expander rail) before the
     * esp-hosted SDIO transport probes it. */
    esp_err_t err = bsp_feature_enable(BSP_FEATURE_WIFI, true);
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "C6 power enable failed: %s", esp_err_to_name(err));
        return err;
    }
    vTaskDelay(pdMS_TO_TICKS(100));

    ESP_ERROR_CHECK(esp_netif_init());
    s_sta_netif = esp_netif_create_default_wifi_sta();
    if (s_sta_netif == NULL) {
        return ESP_FAIL;
    }
    esp_netif_set_hostname(s_sta_netif, "liveninja-tab5");

    wifi_init_config_t wcfg = WIFI_INIT_CONFIG_DEFAULT();
    err = esp_wifi_init(&wcfg);
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "esp_wifi_init (remote/C6) failed: %s", esp_err_to_name(err));
        return err;
    }

    ESP_ERROR_CHECK(esp_event_handler_instance_register(
        WIFI_EVENT, ESP_EVENT_ANY_ID, wifi_event_handler, NULL, NULL));
    ESP_ERROR_CHECK(esp_event_handler_instance_register(
        IP_EVENT, IP_EVENT_STA_GOT_IP, wifi_event_handler, NULL, NULL));

    /* Credentials live in our own NVS namespace, not the WiFi blob. */
    esp_wifi_set_storage(WIFI_STORAGE_RAM);
    ESP_ERROR_CHECK(esp_wifi_set_mode(WIFI_MODE_STA));
    err = esp_wifi_start();
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "esp_wifi_start failed: %s", esp_err_to_name(err));
        return err;
    }

    creds_load();
    ln_iot_register_rssi_provider(rssi_provider);
    return ln_auth_init();
}

esp_err_t ln_net_start(void)
{
    if (s_task != NULL) {
        return ESP_ERR_INVALID_STATE;
    }
    /* Pairing HTTPS runs on this stack — keep it roomy. */
    if (xTaskCreate(net_task, "ln_net", 10240, NULL, 5, &s_task) != pdPASS) {
        return ESP_ERR_NO_MEM;
    }
    return ESP_OK;
}
