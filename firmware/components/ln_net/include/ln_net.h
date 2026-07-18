/*
 * ln_net — Live Ninja Tab5 network + provisioning + device auth.
 *
 * Owns (plan.md M5 §2/§6):
 *   - ESP32-C6 WiFi via esp_wifi_remote over the esp-hosted SDIO link
 *     (standard esp_wifi_* API, radio on the C6).
 *   - STA connection from NVS-stored credentials with retry/backoff.
 *   - SoftAP captive portal (esp_http_server + DNS hijack) while the device
 *     is unprovisioned or its network is unreachable: SSID scan-list-select
 *     page + passphrase form + pairing hand-off (rich-UI rules, PRD §M5
 *     portal screens 10-12).
 *   - Device pairing per contracts/api.md: POST /auth/device/pair/start
 *     (device-generated PKCE S256 challenge) -> human completes LWA in a
 *     browser at the claim URL -> POST /auth/device/pair/poll presenting the
 *     code_verifier -> 10-year refresh family + deviceId, persisted in NVS
 *     (NVS encryption arrives with the M5 "10-yr persistence" task).
 *   - Silent 24h rotation via POST /auth/refresh and on-demand access-JWT
 *     mint (ln_auth_get_jwt()).
 *
 * Wiring: call ln_net_init() after nvs_flash_init() + the default event
 * loop + bsp_display_start() (ln_net powers the C6 through the BSP IO
 * expander), then ln_net_start(). Progress is reported on the default event
 * loop under LN_NET_EVENT; the ctrl state machine in main/ maps those onto
 * the plan.md M5 state diagram (Boot/Provisioning/Idle/...).
 */
#pragma once

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#include "esp_err.h"
#include "esp_event.h"

#ifdef __cplusplus
extern "C" {
#endif

ESP_EVENT_DECLARE_BASE(LN_NET_EVENT);

/** Events posted to the default event loop (event base LN_NET_EVENT). */
typedef enum {
    /** SoftAP portal is up. Data: ln_net_portal_info_t. */
    LN_NET_EVENT_PORTAL_STARTED = 0,
    /** Portal (and SoftAP) torn down. No data. */
    LN_NET_EVENT_PORTAL_STOPPED,
    /** STA connect attempt started. Data: char[33] SSID. */
    LN_NET_EVENT_WIFI_CONNECTING,
    /** STA got an IP. Data: ln_net_wifi_info_t. */
    LN_NET_EVENT_WIFI_CONNECTED,
    /** STA lost the link / failed to connect. Data: ln_net_wifi_fail_t. */
    LN_NET_EVENT_WIFI_DISCONNECTED,
    /** Pairing registered with the backend. Data: ln_net_pairing_info_t
     *  (claim URL for the LCD QR / portal hand-off). */
    LN_NET_EVENT_PAIRING_STARTED,
    /** Pairing bound + claimed; 10-yr refresh + deviceId now in NVS.
     *  Data: char[48] deviceId. */
    LN_NET_EVENT_PAIRED,
    /** A valid access JWT is available (also fires after each successful
     *  refresh/rotation). No data. */
    LN_NET_EVENT_AUTH_READY,
    /** The refresh lineage was rejected by the backend (revoked/reused/
     *  invalid). Stored auth has been wiped; device must re-pair
     *  (state diagram: Error -> Provisioning). No data. */
    LN_NET_EVENT_AUTH_INVALID,
    /** A station (phone/laptop) joined or left the provisioning SoftAP.
     *  Data: int (current associated-client count). Lets the LCD onboarding
     *  screen advance its footer from "Waiting for a device to connect…". */
    LN_NET_EVENT_PORTAL_CLIENT,
    /** The portal setup page was served to a connected client (GET /).
     *  Data: int (1). Lets the LCD show "setup page open on your phone". */
    LN_NET_EVENT_PORTAL_PAGE_OPENED,
} ln_net_event_id_t;

typedef struct {
    char ap_ssid[33];   /**< SoftAP SSID, e.g. "LiveNinja-Setup" */
    char portal_url[32];/**< e.g. "http://192.168.4.1" */
} ln_net_portal_info_t;

typedef struct {
    char ssid[33];
    char ip[16];
    int  rssi;
} ln_net_wifi_info_t;

/** Why the last STA connect attempt failed (for UI copy). */
typedef enum {
    LN_NET_WIFI_FAIL_OTHER = 0,
    LN_NET_WIFI_FAIL_AUTH,        /**< wrong passphrase / auth rejected */
    LN_NET_WIFI_FAIL_AP_NOT_FOUND,
} ln_net_wifi_fail_reason_t;

typedef struct {
    char ssid[33];
    ln_net_wifi_fail_reason_t reason;
    int  retry_count;   /**< consecutive failures so far */
} ln_net_wifi_fail_t;

typedef struct {
    char claim_url[256]; /**< https://.../auth/device/claim?nonce=... */
    int  expires_in_s;   /**< seconds until this pairing nonce expires */
} ln_net_pairing_info_t;

/**
 * Bring up netif + esp_wifi (remote/C6), load stored state from NVS and
 * register internal event handlers. Requires: NVS initialised, default
 * event loop created, BSP I2C/IO-expander available (C6 power rail).
 */
esp_err_t ln_net_init(void);

/**
 * Start the network state machine task:
 *  - WiFi creds in NVS -> STA connect (portal raised after
 *    CONFIG_LN_WIFI_CONNECT_RETRIES consecutive failures);
 *  - no creds -> SoftAP captive portal immediately;
 *  - online + unpaired -> backend pairing loop;
 *  - online + paired  -> auth rotation task (24h silent refresh).
 */
esp_err_t ln_net_start(void);

/** True once WiFi credentials exist in NVS. */
bool ln_net_wifi_is_provisioned(void);

/** True once a device refresh token + deviceId exist in NVS. */
bool ln_net_is_paired(void);

/** True while the STA interface holds an IP. */
bool ln_net_is_online(void);

/**
 * Copy the current pairing claim URL (empty string if pairing is not in
 * progress). The ctrl/UI layer renders this as QR + text on the LCD.
 */
esp_err_t ln_net_get_claim_url(char *buf, size_t len);

/**
 * Wipe WiFi credentials AND device auth from NVS (Settings "forget this
 * device" / factory reset path), then restart provisioning from the portal.
 */
esp_err_t ln_net_clear_provisioning(void);

/**
 * Wipe ONLY the WiFi credentials (device pairing/auth kept) and drop the
 * link so the SoftAP portal comes back up — Settings "Wi-Fi setup" path.
 */
esp_err_t ln_net_reprovision_wifi(void);

/**
 * Get a currently-valid access JWT (Authorization: Bearer ...) for backend
 * calls (GET /v1/realtime/session, POST /v1/tools/invoke, ...).
 *
 * If the cached JWT expires within 60 s this blocks (up to ~20 s) while the
 * auth task rotates the refresh token and mints a new one. Safe to call
 * from any task; the HTTP work runs on the auth task's stack, not the
 * caller's.
 *
 * Returns ESP_OK and a NUL-terminated JWT in buf, ESP_ERR_INVALID_STATE if
 * the device is unpaired, ESP_ERR_TIMEOUT if a refresh could not complete,
 * ESP_ERR_NO_MEM if buf is too small.
 */
esp_err_t ln_auth_get_jwt(char *buf, size_t len);

/** Copy the paired deviceId (ESP_ERR_INVALID_STATE if unpaired). */
esp_err_t ln_auth_get_device_id(char *buf, size_t len);

/** Force an immediate refresh-token rotation (fire-and-forget). */
void ln_auth_force_refresh(void);

/* ---- On-device provisioning surface (LCD onboarding screen) ----
 * The same capabilities the SoftAP portal page offers, exposed to ln_ui so
 * setup can be completed entirely on the touchscreen. */

/** One scanned access point (public shape of the cached SSID scan). */
typedef struct {
    char ssid[33];
    int  rssi;
    bool secure;   /**< false only for open networks */
} ln_net_scan_ap_t;

/**
 * Copy the cached SSID scan into out (deduped upstream only by the caller).
 * Never blocks on the radio. Returns the record count; *scanning true while
 * a background scan runs; *age_ms is ms since the cache was filled (-1 if
 * never). Either out-param may be NULL.
 */
int ln_net_scan_results(ln_net_scan_ap_t *out, int max,
                        bool *scanning, int64_t *age_ms);

/** Kick a background SSID rescan (no-op if one is already running).
 *  NOTE: an all-channel scan takes the SoftAP radio off-channel for ~1-2s —
 *  associated portal clients will stall/drop briefly. Only call on explicit
 *  user action (Rescan button / opening the network list). */
void ln_net_scan_request(void);

/** Store credentials + start the STA join (async; progress arrives as
 *  LN_NET_EVENT_WIFI_* events). Also records the join-a-network choice. */
esp_err_t ln_net_join_wifi(const char *ssid, const char *pass);

/** Record the stay-on-the-hotspot choice. subnet is "10.0.0" or
 *  "192.168.4" (NULL keeps the current one); a change re-IPs the SoftAP
 *  ~600ms later (clients must reconnect to the new gateway). */
esp_err_t ln_net_choose_ap_mode(const char *subnet);

/** Copy the live portal URL, e.g. "http://10.0.0.1/". */
void ln_net_portal_url(char *buf, size_t len);

/** Number of stations currently associated with the setup SoftAP. */
int ln_net_ap_client_count(void);

#ifdef __cplusplus
}
#endif
