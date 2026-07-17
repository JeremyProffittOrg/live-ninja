/*
 * ln_iot — Live Ninja AWS IoT Core integration (M5Stack Tab5, ESP32-P4).
 *
 * Owns: esp-mqtt mTLS to the account ATS endpoint, fleet-provisioning-by-claim
 * (on-chip EC keypair + CSR — the operational private key never leaves the
 * device), the `config` named shadow (contracts/shadow.md), 30s telemetry
 * heartbeat + telemetry event publishing (contracts/telemetry.schema.json),
 * control topics, and IoT-Jobs-driven A/B OTA with rollback handling.
 *
 * Lifecycle:
 *   ln_iot_init()      — once, after nvs_flash_init() + default event loop.
 *   ln_iot_start()     — once the C6 WiFi link has an IP. If no operational
 *                        cert exists yet, runs fleet provisioning first (needs
 *                        claim material — see ln_iot_store_bootstrap()).
 *   ln_iot_stop()      — tears the MQTT link down (e.g. WiFi lost).
 *
 * All notifications are posted to the DEFAULT esp_event loop under LN_IOT_EVENT.
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

ESP_EVENT_DECLARE_BASE(LN_IOT_EVENT);

typedef enum {
    LN_IOT_EVENT_PROVISIONED = 0,  /**< Fleet provisioning done; op cert + thing name stored in NVS. No data. */
    LN_IOT_EVENT_CONNECTED,        /**< Operational MQTT session up (subscriptions in place). No data. */
    LN_IOT_EVENT_DISCONNECTED,     /**< Operational MQTT session lost (auto-reconnect keeps running). No data. */
    LN_IOT_EVENT_CONFIG_DELTA,     /**< Shadow `config` desired delta to apply. Data: ln_iot_config_delta_t (by value). */
    LN_IOT_EVENT_CONTROL_DOWN,     /**< Message on liveninja/<thing>/control/down. Data: ln_iot_buf_t; receiver must free(.data). */
    LN_IOT_EVENT_OTA_STARTED,      /**< OTA download begins. Data: ln_iot_ota_info_t (by value). */
    LN_IOT_EVENT_OTA_PROGRESS,     /**< Data: int percent (0-100). */
    LN_IOT_EVENT_OTA_FAILED,       /**< OTA aborted (download/verify/version error). Data: ln_iot_ota_info_t. */
    LN_IOT_EVENT_OTA_REBOOTING,    /**< OTA image verified + boot partition set; esp_restart() follows in ~2s. Data: ln_iot_ota_info_t. */
    LN_IOT_EVENT_PROVISION_FAILED, /**< Fleet provisioning failed (will not retry on its own). No data. */
} ln_iot_event_id_t;

/** Heap-buffer event payload: receiver takes ownership of .data (free() it). */
typedef struct {
    char *data;   /**< NUL-terminated JSON. */
    size_t len;   /**< strlen(data). */
} ln_iot_buf_t;

/** OTA job identity, valid for the duration of the event callback. */
typedef struct {
    char job_id[64];
    char version[32];
} ln_iot_ota_info_t;

/**
 * Parsed `config` shadow desired delta (contracts/shadow.md). Only fields with
 * their has_* flag set were present in the delta. ctrl applies what it can,
 * persists its settings, then calls ln_iot_shadow_report() with the FULL new
 * reported state (including the delta's settings_version).
 */
typedef struct {
    int32_t settings_version;          /**< Always present (deltas without it are dropped). */
    bool has_wake_word;                char wake_word[48];
    bool has_wake_engine;              char wake_engine[24];
    bool has_sensitivity;              float sensitivity;
    bool has_voice;                    char voice[24];
    bool has_turn_detection;           char turn_detection[24];
    bool has_voice_engine;             char voice_engine[32];
    /* privacy.* is shadowed read-only for on-screen disclosure (shadow.md). */
    bool has_privacy_store_audio;      bool privacy_store_audio;
    bool has_privacy_store_transcripts; bool privacy_store_transcripts;
    bool has_privacy_retention_days;   int32_t privacy_retention_days;
} ln_iot_config_delta_t;

/** Full reported state for the `config` shadow (all fields required except sha). */
typedef struct {
    int32_t settings_version;
    const char *wake_word;
    const char *wake_engine;
    float sensitivity;
    const char *voice;
    const char *turn_detection;
    const char *voice_engine;
    const char *wake_model_sha256_applied; /**< NULL until a model has been verified+applied. */
} ln_iot_shadow_reported_t;

/**
 * Claim/bootstrap material handed to the device by the pairing flow
 * (GET /auth/device/pair/claim — "provisioning claim (cert bootstrap
 * material)", contracts/api.md). ln_net calls this once after a successful
 * claim; everything is persisted to NVS namespace "ln_iot". Any field may be
 * NULL to keep the currently stored value.
 */
typedef struct {
    const char *iot_endpoint;    /**< ATS data endpoint hostname, e.g. "xxxx-ats.iot.us-east-1.amazonaws.com". */
    const char *claim_cert_pem;  /**< Claiming certificate (PEM). */
    const char *claim_key_pem;   /**< Claiming certificate private key (PEM). */
    const char *template_name;   /**< Fleet provisioning template name (default: Kconfig LN_IOT_PROV_TEMPLATE). */
    const char *device_id;       /**< Backend deviceId from pairing — used as SerialNumber + telemetry deviceId. */
    const char *user_id;         /**< Internal userId (never the raw LWA id) for telemetry attribution. */
} ln_iot_bootstrap_t;

/** RSSI provider hook — return current STA RSSI in dBm (ln_net registers one
 *  backed by esp_wifi_sta_get_ap_info; unset, heartbeats report rssi=0). */
typedef int (*ln_iot_rssi_fn_t)(void);

esp_err_t ln_iot_init(void);
esp_err_t ln_iot_start(void);
esp_err_t ln_iot_stop(void);

bool ln_iot_is_provisioned(void);   /**< True once an operational cert + thing name are stored. */
bool ln_iot_is_connected(void);     /**< True while the operational MQTT session is up. */
const char *ln_iot_thing_name(void);/**< NULL until provisioned. Stable storage. */
const char *ln_iot_device_id(void); /**< NULL until pairing stored it. Stable storage. */

esp_err_t ln_iot_store_bootstrap(const ln_iot_bootstrap_t *bootstrap);

/** Wipe operational cert/key/thing (device revoked or re-pairing). Claim
 *  material and identity are wiped too iff `full` is true. Disconnects. */
esp_err_t ln_iot_factory_reset_credentials(bool full);

/** Publish full reported state to the `config` shadow (see struct docs). */
esp_err_t ln_iot_shadow_report(const ln_iot_shadow_reported_t *reported);

/**
 * Publish one telemetry event (contracts/telemetry.schema.json envelope is
 * built here: event/deviceId/surface/ts/sessionId/userId are filled in).
 * @param event        Catalog name, e.g. "wake_word_detected".
 * @param session_id   Realtime session id or NULL → "none".
 * @param attrs_json   JSON object string for "attrs" or NULL → "{}".
 */
esp_err_t ln_iot_publish_telemetry(const char *event, const char *session_id,
                                   const char *attrs_json);

/** Publish raw JSON to liveninja/<thing>/control/up (QoS1). */
esp_err_t ln_iot_publish_control_up(const char *json);

/** Set the app-state string carried in device_heartbeat attrs ("idle", "listening", ...). */
void ln_iot_set_app_state(const char *state);

void ln_iot_register_rssi_provider(ln_iot_rssi_fn_t fn);

#ifdef __cplusplus
}
#endif
