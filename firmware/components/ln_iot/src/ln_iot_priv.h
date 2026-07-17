/* ln_iot internals — shared between the core, provisioning, shadow and OTA units. */
#pragma once

#include <stdbool.h>
#include <stdint.h>

#include "esp_err.h"
#include "mqtt_client.h"

#ifdef __cplusplus
extern "C" {
#endif

#define LN_IOT_NVS_NS "ln_iot"

/* NVS keys (<=15 chars). Strings unless noted. */
#define LN_IOT_K_ENDPOINT   "endpoint"   /* ATS endpoint hostname */
#define LN_IOT_K_CLAIM_CERT "claim_cert" /* blob: claim cert PEM (NUL-terminated) */
#define LN_IOT_K_CLAIM_KEY  "claim_key"  /* blob: claim key PEM */
#define LN_IOT_K_TEMPLATE   "tmpl"       /* provisioning template name */
#define LN_IOT_K_OP_CERT    "op_cert"    /* blob: operational cert PEM */
#define LN_IOT_K_OP_KEY     "op_key"     /* blob: operational private key PEM (device-generated) */
#define LN_IOT_K_THING      "thing"      /* thing name from RegisterThing */
#define LN_IOT_K_DEVICE_ID  "device_id"  /* backend deviceId from pairing */
#define LN_IOT_K_USER_ID    "user_id"    /* internal userId */
#define LN_IOT_K_OTA_JOB    "ota_job"    /* IoT Jobs jobId of an in-flight OTA (set right before reboot) */
#define LN_IOT_K_OTA_VER    "ota_ver"    /* target firmware version of that OTA */
#define LN_IOT_K_SET_VER    "set_ver"    /* i32: last reported settingsVersion */

/* Amazon Root CA 1 (public), used to verify the ATS endpoint. */
extern const char ln_iot_aws_root_ca_pem[];

/* ---- NVS helpers (ln_iot_nvs.c) ---- */
char *ln_iot_nvs_get_str(const char *key);            /* malloc'd or NULL */
char *ln_iot_nvs_get_blob_str(const char *key);       /* malloc'd NUL-terminated or NULL */
esp_err_t ln_iot_nvs_set_str(const char *key, const char *val);
esp_err_t ln_iot_nvs_set_blob_str(const char *key, const char *val); /* stores strlen+1 bytes */
esp_err_t ln_iot_nvs_erase_key(const char *key);
esp_err_t ln_iot_nvs_get_i32(const char *key, int32_t *out);
esp_err_t ln_iot_nvs_set_i32(const char *key, int32_t val);

/* ---- Core services used by the sub-units (ln_iot.c) ---- */
esp_mqtt_client_handle_t ln_iot_mqtt(void);           /* operational client or NULL */
const char *ln_iot_thing(void);                       /* thing name or NULL */
int ln_iot_publish(const char *topic, const char *payload, int qos);      /* blocking publish */
int ln_iot_enqueue(const char *topic, const char *payload, int qos);      /* non-blocking */
esp_err_t ln_iot_subscribe(const char *topic, int qos);
void ln_iot_iso8601_now(char *buf, size_t buflen);
void ln_iot_post_event(int32_t event_id, const void *data, size_t data_len);
/* Called by the OTA unit right before esp_restart() so the core can flush. */
void ln_iot_prepare_reboot(void);

/* ---- Provisioning (ln_iot_provision.c) ----
 * Blocking: generates an EC P-256 keypair + CSR, connects with the claim cert,
 * runs CreateCertificateFromCsr + RegisterThing, persists op cert/key/thing.
 * Returns ESP_OK once NVS holds the operational identity. */
esp_err_t ln_iot_provision_run(const char *endpoint,
                               const char *claim_cert_pem,
                               const char *claim_key_pem,
                               const char *template_name,
                               const char *serial_number);

/* ---- Shadow (ln_iot_shadow.c) ---- */
void ln_iot_shadow_on_connected(void);   /* subscribe + publish /get */
bool ln_iot_shadow_handle(const char *topic, const char *data, int len); /* true if consumed */

/* ---- Jobs / OTA (ln_iot_ota.c) ---- */
void ln_iot_ota_on_connected(void);      /* subscribe, resolve pending job, query $next */
bool ln_iot_ota_handle(const char *topic, const char *data, int len);    /* true if consumed */
void ln_iot_ota_boot_guard(void);        /* at init: arm rollback watchdog if image unverified */
void ln_iot_ota_check_pending_verify(void); /* on connect: cancel watchdog + mark app valid */

#ifdef __cplusplus
}
#endif
