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

#define LN_PORTAL_IP_STR "192.168.4.1"

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

/* Blocking scan (APSTA-safe); returns number of records written. */
int ln_net_wifi_scan(void *out_records, int max_records);

/* Lazily create (and return) the default AP netif for the portal SoftAP. */
esp_netif_t *ln_net_take_ap_netif(void);

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
