/*
 * ln_portal.c — SoftAP captive provisioning portal (plan.md M5 §6, PRD
 * portal screens). Open AP "LiveNinja-Setup" + esp_http_server on :80 +
 * DNS hijack (ln_dns.c) so phones auto-open the page.
 *
 * Endpoints:
 *   GET  /            single self-contained rich-UI page (embedded)
 *   GET  /api/scan    live SSID scan -> JSON list (dedup, strongest first)
 *   POST /api/wifi    {ssid, pass} -> store + async STA connect (202)
 *   GET  /api/status  {wifi:{state,ssid,ip,reason}, paired, claimUrl}
 *   *                 captive-portal probes + any 404 -> 302 to /
 *
 * The AP runs alongside STA (APSTA) so the phone keeps seeing status while
 * the device tries the selected network and then pairs with the backend.
 */
#include <stdio.h>
#include <string.h>

#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"

#include "cJSON.h"
#include "esp_http_server.h"
#include "esp_log.h"
#include "esp_wifi.h"
#include "sdkconfig.h"

#include "ln_net_priv.h"

static const char *TAG = "ln_portal";

#define SCAN_MAX_RECORDS 24

extern const char portal_html_start[] asm("_binary_portal_html_start");
extern const char portal_html_end[]   asm("_binary_portal_html_end");

static httpd_handle_t s_httpd;

/* ------------------------------------------------------------- helpers */

static esp_err_t redirect_to_root(httpd_req_t *req)
{
    httpd_resp_set_status(req, "302 Found");
    httpd_resp_set_hdr(req, "Location", "http://" LN_PORTAL_IP_STR "/");
    httpd_resp_set_hdr(req, "Cache-Control", "no-store");
    return httpd_resp_send(req, NULL, 0);
}

/* Minimal JSON string escaper (SSIDs can contain quotes/backslashes). */
static void json_escape(const char *in, char *out, size_t out_len)
{
    size_t w = 0;
    for (const unsigned char *p = (const unsigned char *)in; *p != '\0'; p++) {
        if (w + 7 >= out_len) {
            break;
        }
        if (*p == '"' || *p == '\\') {
            out[w++] = '\\';
            out[w++] = (char)*p;
        } else if (*p < 0x20) {
            w += snprintf(&out[w], out_len - w, "\\u%04x", *p);
        } else {
            out[w++] = (char)*p;
        }
    }
    out[w] = '\0';
}

/* -------------------------------------------------------------- handlers */

static esp_err_t root_get(httpd_req_t *req)
{
    httpd_resp_set_type(req, "text/html; charset=utf-8");
    httpd_resp_set_hdr(req, "Cache-Control", "no-store");
    return httpd_resp_send(req, portal_html_start,
                           portal_html_end - portal_html_start - 1);
}

static esp_err_t scan_get(httpd_req_t *req)
{
    static wifi_ap_record_t recs[SCAN_MAX_RECORDS]; /* ~2KB; keep off stack */
    static SemaphoreHandle_t scan_lock;
    if (scan_lock == NULL) {
        scan_lock = xSemaphoreCreateMutex();
    }

    xSemaphoreTake(scan_lock, portMAX_DELAY);
    int n = ln_net_wifi_scan(recs, SCAN_MAX_RECORDS);

    httpd_resp_set_type(req, "application/json");
    httpd_resp_set_hdr(req, "Cache-Control", "no-store");
    if (n < 0) {
        xSemaphoreGive(scan_lock);
        httpd_resp_set_status(req, "503 Service Unavailable");
        return httpd_resp_sendstr(req, "{\"error\":\"scan_busy\"}");
    }

    httpd_resp_sendstr_chunk(req, "{\"networks\":[");
    bool first = true;
    for (int i = 0; i < n; i++) {
        const char *ssid = (const char *)recs[i].ssid;
        if (ssid[0] == '\0') {
            continue;               /* hidden — the page has its own row */
        }
        /* Dedupe: keep only the first (strongest — driver sorts by RSSI)
         * record for each SSID. */
        bool dup = false;
        for (int j = 0; j < i; j++) {
            if (strcmp((const char *)recs[j].ssid, ssid) == 0) {
                dup = true;
                break;
            }
        }
        if (dup) {
            continue;
        }
        char esc[100];
        json_escape(ssid, esc, sizeof(esc));
        char row[160];
        snprintf(row, sizeof(row), "%s{\"ssid\":\"%s\",\"rssi\":%d,\"secure\":%s}",
                 first ? "" : ",", esc, recs[i].rssi,
                 (recs[i].authmode == WIFI_AUTH_OPEN) ? "false" : "true");
        httpd_resp_sendstr_chunk(req, row);
        first = false;
    }
    httpd_resp_sendstr_chunk(req, "]}");
    httpd_resp_sendstr_chunk(req, NULL);
    xSemaphoreGive(scan_lock);
    return ESP_OK;
}

static esp_err_t wifi_post(httpd_req_t *req)
{
    char body[256];
    int total = 0;
    int to_read = req->content_len;
    if (to_read <= 0 || to_read >= (int)sizeof(body)) {
        httpd_resp_set_status(req, "400 Bad Request");
        return httpd_resp_sendstr(req, "{\"error\":\"bad_body\"}");
    }
    while (total < to_read) {
        int r = httpd_req_recv(req, body + total, to_read - total);
        if (r <= 0) {
            return ESP_FAIL;
        }
        total += r;
    }
    body[total] = '\0';

    esp_err_t err = ESP_ERR_INVALID_ARG;
    cJSON *root = cJSON_Parse(body);
    if (root != NULL) {
        const cJSON *jssid = cJSON_GetObjectItemCaseSensitive(root, "ssid");
        const cJSON *jpass = cJSON_GetObjectItemCaseSensitive(root, "pass");
        if (cJSON_IsString(jssid)) {
            err = ln_net_apply_wifi_credentials(
                jssid->valuestring,
                cJSON_IsString(jpass) ? jpass->valuestring : "");
        }
        cJSON_Delete(root);
    }

    httpd_resp_set_type(req, "application/json");
    if (err != ESP_OK) {
        httpd_resp_set_status(req, "400 Bad Request");
        return httpd_resp_sendstr(req, "{\"error\":\"invalid_credentials\"}");
    }
    httpd_resp_set_status(req, "202 Accepted");
    return httpd_resp_sendstr(req, "{\"ok\":true}");
}

static const char *wifi_state_str(const ln_wifi_status_t *st)
{
    switch (st->state) {
    case LN_WIFI_CONNECTING:
        return "connecting";
    case LN_WIFI_CONNECTED:
        return "connected";
    case LN_WIFI_FAILED:
        switch (st->fail_reason) {
        case LN_NET_WIFI_FAIL_AUTH:
            return "wrong_password";
        case LN_NET_WIFI_FAIL_AP_NOT_FOUND:
            return "ap_not_found";
        default:
            return "failed";
        }
    default:
        return "idle";
    }
}

static esp_err_t status_get(httpd_req_t *req)
{
    ln_wifi_status_t st;
    ln_net_get_wifi_status(&st);

    char claim[256];
    ln_pairing_get_claim_url(claim, sizeof(claim));

    char ssid_esc[100];
    json_escape(st.ssid, ssid_esc, sizeof(ssid_esc));

    char out[640];
    snprintf(out, sizeof(out),
             "{\"wifi\":{\"state\":\"%s\",\"ssid\":\"%s\",\"ip\":\"%s\"},"
             "\"paired\":%s,\"claimUrl\":\"%s\"}",
             wifi_state_str(&st), ssid_esc, st.ip,
             ln_net_is_paired() ? "true" : "false", claim);

    httpd_resp_set_type(req, "application/json");
    httpd_resp_set_hdr(req, "Cache-Control", "no-store");
    return httpd_resp_sendstr(req, out);
}

static esp_err_t captive_probe(httpd_req_t *req)
{
    return redirect_to_root(req);
}

static esp_err_t err_404(httpd_req_t *req, httpd_err_code_t err)
{
    return redirect_to_root(req);
}

/* ------------------------------------------------------------ start/stop */

static esp_err_t softap_up(void)
{
    if (ln_net_take_ap_netif() == NULL) {
        return ESP_FAIL;
    }
    esp_err_t err = esp_wifi_set_mode(WIFI_MODE_APSTA);
    if (err != ESP_OK) {
        return err;
    }
    wifi_config_t ap = {0};
    strlcpy((char *)ap.ap.ssid, CONFIG_LN_PORTAL_AP_SSID, sizeof(ap.ap.ssid));
    ap.ap.ssid_len = strlen(CONFIG_LN_PORTAL_AP_SSID);
    ap.ap.channel = CONFIG_LN_PORTAL_AP_CHANNEL;
    ap.ap.max_connection = CONFIG_LN_PORTAL_AP_MAX_CLIENTS;
    ap.ap.authmode = WIFI_AUTH_OPEN;
    return esp_wifi_set_config(WIFI_IF_AP, &ap);
}

esp_err_t ln_portal_start(void)
{
    if (s_httpd != NULL) {
        return ESP_OK;
    }
    esp_err_t err = softap_up();
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "SoftAP up failed: %s", esp_err_to_name(err));
        return err;
    }

    httpd_config_t cfg = HTTPD_DEFAULT_CONFIG();
    cfg.max_uri_handlers = 14;
    cfg.stack_size = 8192;
    cfg.lru_purge_enable = true;
    cfg.uri_match_fn = httpd_uri_match_wildcard;
    err = httpd_start(&s_httpd, &cfg);
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "httpd_start failed: %s", esp_err_to_name(err));
        return err;
    }

    const httpd_uri_t routes[] = {
        {.uri = "/",            .method = HTTP_GET,  .handler = root_get},
        {.uri = "/api/scan",    .method = HTTP_GET,  .handler = scan_get},
        {.uri = "/api/wifi",    .method = HTTP_POST, .handler = wifi_post},
        {.uri = "/api/status",  .method = HTTP_GET,  .handler = status_get},
        /* Captive-portal detection probes (Android/Apple/Windows). */
        {.uri = "/generate_204",             .method = HTTP_GET, .handler = captive_probe},
        {.uri = "/gen_204",                  .method = HTTP_GET, .handler = captive_probe},
        {.uri = "/hotspot-detect.html",      .method = HTTP_GET, .handler = captive_probe},
        {.uri = "/library/test/success.html",.method = HTTP_GET, .handler = captive_probe},
        {.uri = "/connecttest.txt",          .method = HTTP_GET, .handler = captive_probe},
        {.uri = "/ncsi.txt",                 .method = HTTP_GET, .handler = captive_probe},
        {.uri = "/success.txt",              .method = HTTP_GET, .handler = captive_probe},
        {.uri = "/redirect",                 .method = HTTP_GET, .handler = captive_probe},
    };
    for (size_t i = 0; i < sizeof(routes) / sizeof(routes[0]); i++) {
        httpd_register_uri_handler(s_httpd, &routes[i]);
    }
    httpd_register_err_handler(s_httpd, HTTPD_404_NOT_FOUND, err_404);

    err = ln_dns_start();
    if (err != ESP_OK) {
        ESP_LOGW(TAG, "DNS hijack unavailable (%s); portal reachable at "
                 "http://" LN_PORTAL_IP_STR " only", esp_err_to_name(err));
    }
    ESP_LOGI(TAG, "portal up: SSID \"%s\", http://" LN_PORTAL_IP_STR,
             CONFIG_LN_PORTAL_AP_SSID);
    return ESP_OK;
}

void ln_portal_stop(void)
{
    ln_dns_stop();
    if (s_httpd != NULL) {
        httpd_stop(s_httpd);
        s_httpd = NULL;
    }
    esp_wifi_set_mode(WIFI_MODE_STA);
    ESP_LOGI(TAG, "portal stopped");
}
