/* ln_net internal interfaces shared between ln_net.c / ln_portal.c /
 * ln_dns.c / ln_backend.c / ln_pairing.c / ln_auth.c. Not installed. */
#pragma once

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#include "esp_err.h"
#include "esp_netif.h"
#include "ln_net.h"

#ifdef __cplusplus
extern "C" {
#endif

/* ---- NVS layout ----
 * namespace "ln_net":  ssid (str), pass (str)
 * namespace "ln_auth": refresh (str, wire "sessionId.secret"),
 *                      device_id (str), thing_name (str), cert_arn (str)
 */
#define LN_NVS_NS_NET   "ln_net"
#define LN_NVS_NS_AUTH  "ln_auth"

/* Default SoftAP gateway (subnet 192.168.4.0/24). The *active* gateway can be
 * changed at runtime via /api/apconfig — use ln_net_ap_gateway() to read the
 * live value; this macro is only the boot default. */
#define LN_PORTAL_IP_DEFAULT "192.168.4.1"

/* ---- WiFi / state machine (ln_net.c) ---- */

typedef enum {
    LN_WIFI_IDLE = 0,      /* no attempt in flight */
    LN_WIFI_CONNECTING,
    LN_WIFI_CONNECTED,     /* got IP */
    LN_WIFI_FAILED,        /* last attempt failed (see fail reason) */
} ln_wifi_state_t;

/* Snapshot of connection progress for the portal /api/status endpoint. */
typedef struct {
    ln_wifi_state_t state;
    ln_net_wifi_fail_reason_t fail_reason;
    char ssid[33];
    char ip[16];
} ln_wifi_status_t;

void ln_net_get_wifi_status(ln_wifi_status_t *out);

/* Store creds to NVS and kick an immediate STA connect attempt (called by
 * the portal's POST /api/wifi handler). */
esp_err_t ln_net_apply_wifi_credentials(const char *ssid, const char *pass);

/* ---- async cached scan (ln_net.c) ----
 * The httpd scan handler MUST NOT block on the radio: a background task runs
 * esp_wifi_scan_start and fills a mutex-protected cache. These two calls only
 * touch the cache / kick the task, so they always return immediately. */

/* Copy the cached scan records into out_records (wifi_ap_record_t[]). Returns
 * the record count (0 before the first scan completes). *scanning is set true
 * while a background scan is in flight; *age_ms is milliseconds since the
 * cache was last filled, or -1 if it never has. Both out-params may be NULL. */
int ln_net_wifi_scan_cached(void *out_records, int max_records,
                            bool *scanning, int64_t *age_ms);

/* Kick a one-off background scan if none is already running (else no-op). */
void ln_net_wifi_scan_trigger(void);

/* Lazily create (and return) the default AP netif for the portal SoftAP,
 * applying the NVS-persisted subnet if one was selected. */
esp_netif_t *ln_net_take_ap_netif(void);

/* ---- SoftAP gateway / subnet (ln_net.c) ---- */

/* Copy the active SoftAP gateway IP (e.g. "192.168.4.1" or "10.0.0.1"). */
void ln_net_ap_gateway(char *buf, size_t len);
/* Fill the active gateway's four octets (for the DNS A-record answer). */
void ln_net_ap_gateway_octets(uint8_t out[4]);

/* Reconfigure the SoftAP subnet. `subnet` is the /24 prefix without the host
 * octet — currently "192.168.4" or "10.0.0". Persists the choice to NVS and,
 * after a short delay (so the HTTP response flushes first), swaps the
 * DHCP-server IP + gateway live. Current clients are dropped and must
 * reconnect to the new gateway. ESP_ERR_INVALID_ARG for an unknown subnet. */
esp_err_t ln_net_apply_ap_subnet(const char *subnet);

/* Portal "keep this device as its own access point" choice: persisted to NVS
 * and surfaced in /api/status. ap_only just records that the user opted to
 * stay on the SoftAP rather than join a Wi-Fi network. */
esp_err_t ln_net_set_ap_only(bool ap_only);
bool      ln_net_is_ap_only(void);

/* ---- Captive portal (ln_portal.c) ---- */
esp_err_t ln_portal_start(void);   /* raises SoftAP + HTTP server + DNS */
void      ln_portal_stop(void);

/* ---- DNS hijack (ln_dns.c) ---- */
esp_err_t ln_dns_start(void);
void      ln_dns_stop(void);

/* ---- Backend HTTPS JSON helper (ln_backend.c) ---- */

/* Large enough for the pairing-claim response once the backend adds the IoT
 * claim material (two PEMs + JWT + JSON overhead). Instances of this struct
 * are big — keep them file-static (one per calling task), never on a task
 * stack. */
#define LN_BACKEND_RSP_MAX 8192

typedef struct {
    int  status;                     /* HTTP status, <0 on transport error */
    char body[LN_BACKEND_RSP_MAX];   /* NUL-terminated response body */
    int  body_len;
} ln_backend_rsp_t;

/* POST (or GET when body==NULL) `path` (e.g. "/auth/refresh") against
 * CONFIG_LN_BACKEND_BASE_URL with a JSON body. bearer may be NULL. */
esp_err_t ln_backend_request(const char *path, const char *json_body,
                             const char *bearer, ln_backend_rsp_t *rsp);

/* base64url (unpadded) encode; returns ESP_OK and NUL-terminates out. */
esp_err_t ln_b64url_encode(const uint8_t *in, size_t in_len,
                           char *out, size_t out_len);

/* ---- Pairing (ln_pairing.c) ---- */

/* Runs the full register->poll->claim loop; blocks until paired (creds in
 * NVS) or a fatal error. Called from the ln_net task while online+unpaired.
 * Returns ESP_OK once paired. */
esp_err_t ln_pairing_run(void);

void ln_pairing_get_claim_url(char *buf, size_t len);

/* ---- Auth (ln_auth.c) ---- */

esp_err_t ln_auth_init(void);            /* load NVS state, create task */
void      ln_auth_on_online(void);       /* nudge: connectivity available */
/* Called by ln_pairing on claim: persists + seeds the auth cache. */
esp_err_t ln_auth_store_claim(const char *device_id, const char *refresh_wire,
                              const char *access_jwt, int64_t access_exp,
                              const char *thing_name, const char *cert_arn);
esp_err_t ln_auth_wipe(void);            /* clear ln_auth namespace + cache */
bool      ln_auth_is_paired(void);

#ifdef __cplusplus
}
#endif
