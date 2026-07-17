/*
 * ln_iot core — operational MQTT session to AWS IoT Core (mTLS), telemetry
 * heartbeat, control topics, and dispatch into the shadow + jobs/OTA units.
 *
 * Connection model: one esp-mqtt client for the operational identity
 * (client id == thing name, per-device IoT policy scoped by
 * ${iot:Connection.Thing.ThingName}). Fleet provisioning uses its own
 * short-lived client inside ln_iot_provision.c. Auto-reconnect is left to
 * esp-mqtt; ln_iot_start()/ln_iot_stop() bracket network availability.
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/time.h>
#include <time.h>

#include "esp_app_desc.h"
#include "esp_event.h"
#include "esp_heap_caps.h"
#include "esp_log.h"
#include "esp_mac.h"
#include "esp_timer.h"
#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"
#include "freertos/task.h"
#include "cJSON.h"
#include "mqtt_client.h"
#include "sdkconfig.h"

#include "ln_iot.h"
#include "ln_iot_priv.h"

static const char *TAG = "ln_iot";

ESP_EVENT_DEFINE_BASE(LN_IOT_EVENT);

#ifdef LN_IOT_EMBEDDED_CLAIM
/* Build-time claim material embedded from $LN_IOT_CLAIM_CERT / $LN_IOT_CLAIM_KEY. */
extern const char _binary_ln_claim_cert_pem_start[];
extern const char _binary_ln_claim_key_pem_start[];
#endif

#define LN_IOT_TOPIC_MAX 192

typedef struct {
    /* Persistent identity (owned heap copies, loaded from NVS). */
    char *endpoint;
    char *thing;
    char *device_id;
    char *user_id;
    char *op_cert;
    char *op_key;

    esp_mqtt_client_handle_t client;
    bool connected;
    bool started;

    SemaphoreHandle_t lock;
    char app_state[24];
    ln_iot_rssi_fn_t rssi_fn;
    esp_timer_handle_t heartbeat_timer;

    /* Multi-chunk MQTT payload accumulator. */
    char rx_topic[LN_IOT_TOPIC_MAX];
    char *rx_buf;
    int rx_total;
    int rx_filled;
} ln_iot_state_t;

static ln_iot_state_t s_iot;

/* ---------------------------------------------------------------- helpers */

void ln_iot_iso8601_now(char *buf, size_t buflen)
{
    time_t now = time(NULL);
    struct tm tm_utc;
    gmtime_r(&now, &tm_utc);
    strftime(buf, buflen, "%Y-%m-%dT%H:%M:%SZ", &tm_utc);
}

void ln_iot_post_event(int32_t event_id, const void *data, size_t data_len)
{
    esp_err_t err = esp_event_post(LN_IOT_EVENT, event_id, (void *)data, data_len,
                                   pdMS_TO_TICKS(1000));
    if (err != ESP_OK) {
        ESP_LOGW(TAG, "esp_event_post(%ld): %s", (long)event_id, esp_err_to_name(err));
    }
}

esp_mqtt_client_handle_t ln_iot_mqtt(void)
{
    return s_iot.client;
}

const char *ln_iot_thing(void)
{
    return s_iot.thing;
}

int ln_iot_publish(const char *topic, const char *payload, int qos)
{
    if (s_iot.client == NULL || !s_iot.connected) {
        return -1;
    }
    return esp_mqtt_client_publish(s_iot.client, topic, payload, 0, qos, 0);
}

int ln_iot_enqueue(const char *topic, const char *payload, int qos)
{
    if (s_iot.client == NULL || !s_iot.connected) {
        return -1;
    }
    return esp_mqtt_client_enqueue(s_iot.client, topic, payload, 0, qos, 0, true);
}

esp_err_t ln_iot_subscribe(const char *topic, int qos)
{
    if (s_iot.client == NULL) {
        return ESP_ERR_INVALID_STATE;
    }
    int id = esp_mqtt_client_subscribe(s_iot.client, topic, qos);
    if (id < 0) {
        ESP_LOGE(TAG, "subscribe %s failed", topic);
        return ESP_FAIL;
    }
    return ESP_OK;
}

/* ------------------------------------------------------------- telemetry */

esp_err_t ln_iot_publish_telemetry(const char *event, const char *session_id,
                                   const char *attrs_json)
{
    if (event == NULL || s_iot.thing == NULL) {
        return ESP_ERR_INVALID_STATE;
    }
    cJSON *root = cJSON_CreateObject();
    if (root == NULL) {
        return ESP_ERR_NO_MEM;
    }
    char ts[32];
    ln_iot_iso8601_now(ts, sizeof(ts));
    cJSON_AddStringToObject(root, "event", event);
    if (s_iot.device_id != NULL) {
        cJSON_AddStringToObject(root, "deviceId", s_iot.device_id);
    } else {
        cJSON_AddNullToObject(root, "deviceId");
    }
    cJSON_AddStringToObject(root, "surface", "m5stack");
    cJSON_AddStringToObject(root, "ts", ts);
    cJSON_AddStringToObject(root, "sessionId",
                            (session_id != NULL && session_id[0] != '\0') ? session_id : "none");
    if (s_iot.user_id != NULL) {
        cJSON_AddStringToObject(root, "userId", s_iot.user_id);
    } else {
        cJSON_AddNullToObject(root, "userId");
    }
    cJSON *attrs = NULL;
    if (attrs_json != NULL) {
        attrs = cJSON_Parse(attrs_json);
        if (attrs == NULL) {
            ESP_LOGW(TAG, "telemetry %s: attrs_json is not valid JSON, sending {}", event);
        }
    }
    if (attrs == NULL) {
        attrs = cJSON_CreateObject();
    }
    cJSON_AddItemToObject(root, "attrs", attrs);

    char *payload = cJSON_PrintUnformatted(root);
    cJSON_Delete(root);
    if (payload == NULL) {
        return ESP_ERR_NO_MEM;
    }
    char topic[LN_IOT_TOPIC_MAX];
    snprintf(topic, sizeof(topic), CONFIG_LN_IOT_TOPIC_PREFIX "/%s/telemetry", s_iot.thing);
    int id = ln_iot_enqueue(topic, payload, 0);
    cJSON_free(payload);
    return (id < 0) ? ESP_FAIL : ESP_OK;
}

void ln_iot_set_app_state(const char *state)
{
    if (state == NULL) {
        return;
    }
    xSemaphoreTake(s_iot.lock, portMAX_DELAY);
    strlcpy(s_iot.app_state, state, sizeof(s_iot.app_state));
    xSemaphoreGive(s_iot.lock);
}

void ln_iot_register_rssi_provider(ln_iot_rssi_fn_t fn)
{
    s_iot.rssi_fn = fn;
}

static void heartbeat_cb(void *arg)
{
    (void)arg;
    if (!s_iot.connected) {
        return;
    }
    char state[24];
    xSemaphoreTake(s_iot.lock, portMAX_DELAY);
    strlcpy(state, s_iot.app_state, sizeof(state));
    xSemaphoreGive(s_iot.lock);

    int rssi = (s_iot.rssi_fn != NULL) ? s_iot.rssi_fn() : 0;
    const esp_app_desc_t *app = esp_app_get_description();
    char attrs[192];
    snprintf(attrs, sizeof(attrs),
             "{\"firmwareVersion\":\"%s\",\"rssi\":%d,\"freeHeapBytes\":%u,\"state\":\"%s\"}",
             app->version, rssi, (unsigned)esp_get_free_heap_size(), state);
    ln_iot_publish_telemetry("device_heartbeat", NULL, attrs);
}

/* ------------------------------------------------------------ control up */

esp_err_t ln_iot_publish_control_up(const char *json)
{
    if (json == NULL || s_iot.thing == NULL) {
        return ESP_ERR_INVALID_ARG;
    }
    char topic[LN_IOT_TOPIC_MAX];
    snprintf(topic, sizeof(topic), CONFIG_LN_IOT_TOPIC_PREFIX "/%s/control/up", s_iot.thing);
    return (ln_iot_publish(topic, json, 1) < 0) ? ESP_FAIL : ESP_OK;
}

/* --------------------------------------------------------- rx + dispatch */

static void dispatch(const char *topic, const char *data, int len)
{
    ESP_LOGD(TAG, "rx %s (%d bytes)", topic, len);
    if (ln_iot_shadow_handle(topic, data, len)) {
        return;
    }
    if (ln_iot_ota_handle(topic, data, len)) {
        return;
    }
    if (strstr(topic, "/control/down") != NULL) {
        ln_iot_buf_t buf = {
            .data = malloc((size_t)len + 1),
            .len = (size_t)len,
        };
        if (buf.data == NULL) {
            ESP_LOGE(TAG, "control/down: OOM (%d bytes)", len);
            return;
        }
        memcpy(buf.data, data, (size_t)len);
        buf.data[len] = '\0';
        ln_iot_post_event(LN_IOT_EVENT_CONTROL_DOWN, &buf, sizeof(buf));
        return;
    }
    ESP_LOGW(TAG, "unhandled topic %s", topic);
}

/* esp-mqtt splits payloads larger than the rx buffer across several
 * MQTT_EVENT_DATA events; only the first carries the topic. Reassemble. */
static void on_data(esp_mqtt_event_handle_t event)
{
    if (event->current_data_offset == 0) {
        free(s_iot.rx_buf);
        s_iot.rx_buf = malloc((size_t)event->total_data_len + 1);
        s_iot.rx_total = event->total_data_len;
        s_iot.rx_filled = 0;
        int tlen = event->topic_len < LN_IOT_TOPIC_MAX - 1 ? event->topic_len
                                                           : LN_IOT_TOPIC_MAX - 1;
        memcpy(s_iot.rx_topic, event->topic, (size_t)tlen);
        s_iot.rx_topic[tlen] = '\0';
    }
    if (s_iot.rx_buf == NULL) {
        return; /* OOM — drop the message */
    }
    memcpy(s_iot.rx_buf + event->current_data_offset, event->data, (size_t)event->data_len);
    s_iot.rx_filled += event->data_len;
    if (s_iot.rx_filled >= s_iot.rx_total) {
        s_iot.rx_buf[s_iot.rx_total] = '\0';
        dispatch(s_iot.rx_topic, s_iot.rx_buf, s_iot.rx_total);
        free(s_iot.rx_buf);
        s_iot.rx_buf = NULL;
        s_iot.rx_total = s_iot.rx_filled = 0;
    }
}

static void on_connected(void)
{
    ESP_LOGI(TAG, "MQTT connected as %s", s_iot.thing);
    s_iot.connected = true;

    /* A/B OTA: the first successful cloud connect is the self-test that makes
     * this image permanent (plan.md M5 "mark-valid-after-check-in"). */
    ln_iot_ota_check_pending_verify();

    char topic[LN_IOT_TOPIC_MAX];
    snprintf(topic, sizeof(topic), CONFIG_LN_IOT_TOPIC_PREFIX "/%s/control/down", s_iot.thing);
    ln_iot_subscribe(topic, 1);

    ln_iot_shadow_on_connected();
    ln_iot_ota_on_connected();

    esp_timer_stop(s_iot.heartbeat_timer); /* no-op if not running */
    esp_timer_start_periodic(s_iot.heartbeat_timer,
                             (uint64_t)CONFIG_LN_IOT_HEARTBEAT_S * 1000000ULL);
    heartbeat_cb(NULL); /* immediate first heartbeat */

    ln_iot_post_event(LN_IOT_EVENT_CONNECTED, NULL, 0);
}

static void mqtt_event_handler(void *arg, esp_event_base_t base, int32_t event_id,
                               void *event_data)
{
    (void)arg;
    (void)base;
    esp_mqtt_event_handle_t event = event_data;
    switch ((esp_mqtt_event_id_t)event_id) {
    case MQTT_EVENT_CONNECTED:
        on_connected();
        break;
    case MQTT_EVENT_DISCONNECTED:
        if (s_iot.connected) {
            s_iot.connected = false;
            esp_timer_stop(s_iot.heartbeat_timer);
            ln_iot_post_event(LN_IOT_EVENT_DISCONNECTED, NULL, 0);
        }
        break;
    case MQTT_EVENT_DATA:
        on_data(event);
        break;
    case MQTT_EVENT_ERROR:
        if (event->error_handle->error_type == MQTT_ERROR_TYPE_TCP_TRANSPORT) {
            ESP_LOGW(TAG, "transport error: esp-tls 0x%x, sock errno %d",
                     event->error_handle->esp_tls_last_esp_err,
                     event->error_handle->esp_transport_sock_errno);
        }
        break;
    default:
        break;
    }
}

/* ------------------------------------------------------------- lifecycle */

static void load_identity(void)
{
    free(s_iot.endpoint);
    free(s_iot.thing);
    free(s_iot.device_id);
    free(s_iot.user_id);
    free(s_iot.op_cert);
    free(s_iot.op_key);
    s_iot.endpoint = ln_iot_nvs_get_str(LN_IOT_K_ENDPOINT);
    s_iot.thing = ln_iot_nvs_get_str(LN_IOT_K_THING);
    s_iot.device_id = ln_iot_nvs_get_str(LN_IOT_K_DEVICE_ID);
    s_iot.user_id = ln_iot_nvs_get_str(LN_IOT_K_USER_ID);
    s_iot.op_cert = ln_iot_nvs_get_blob_str(LN_IOT_K_OP_CERT);
    s_iot.op_key = ln_iot_nvs_get_blob_str(LN_IOT_K_OP_KEY);
}

esp_err_t ln_iot_init(void)
{
    if (s_iot.lock != NULL) {
        return ESP_ERR_INVALID_STATE;
    }
    s_iot.lock = xSemaphoreCreateMutex();
    if (s_iot.lock == NULL) {
        return ESP_ERR_NO_MEM;
    }
    strlcpy(s_iot.app_state, "boot", sizeof(s_iot.app_state));
    const esp_timer_create_args_t targs = {
        .callback = heartbeat_cb,
        .name = "ln_iot_hb",
        .dispatch_method = ESP_TIMER_TASK,
    };
    esp_err_t err = esp_timer_create(&targs, &s_iot.heartbeat_timer);
    if (err != ESP_OK) {
        return err;
    }
    load_identity();
    /* If this boot is an unverified OTA image, arm the rollback watchdog now —
     * only a successful MQTT connect (ln_iot_ota_check_pending_verify) makes
     * the image permanent. */
    ln_iot_ota_boot_guard();
    ESP_LOGI(TAG, "init: endpoint=%s thing=%s provisioned=%d",
             s_iot.endpoint ? s_iot.endpoint : "(unset)",
             s_iot.thing ? s_iot.thing : "(unset)", ln_iot_is_provisioned());
    return ESP_OK;
}

static esp_err_t start_operational_client(void)
{
    char uri[192];
    snprintf(uri, sizeof(uri), "mqtts://%s:8883", s_iot.endpoint);
    const esp_mqtt_client_config_t cfg = {
        .broker = {
            .address.uri = uri,
            .verification.certificate = ln_iot_aws_root_ca_pem,
        },
        .credentials = {
            .client_id = s_iot.thing,
            .authentication = {
                .certificate = s_iot.op_cert,
                .key = s_iot.op_key,
            },
        },
        .session = {
            .keepalive = 60,
            .protocol_ver = MQTT_PROTOCOL_V_3_1_1,
        },
        .buffer = {
            .size = CONFIG_LN_IOT_MQTT_BUF_SIZE,
            .out_size = CONFIG_LN_IOT_MQTT_BUF_SIZE,
        },
        .task = {
            .stack_size = 6144,
        },
    };
    s_iot.client = esp_mqtt_client_init(&cfg);
    if (s_iot.client == NULL) {
        return ESP_FAIL;
    }
    esp_mqtt_client_register_event(s_iot.client, ESP_EVENT_ANY_ID, mqtt_event_handler, NULL);
    return esp_mqtt_client_start(s_iot.client);
}

static bool get_claim_material(char **cert, char **key)
{
    *cert = ln_iot_nvs_get_blob_str(LN_IOT_K_CLAIM_CERT);
    *key = ln_iot_nvs_get_blob_str(LN_IOT_K_CLAIM_KEY);
    if (*cert != NULL && *key != NULL) {
        return true; /* NVS-injected via pairing (primary path) */
    }
    free(*cert);
    free(*key);
    *cert = *key = NULL;
#ifdef LN_IOT_EMBEDDED_CLAIM
    *cert = strdup(_binary_ln_claim_cert_pem_start);
    *key = strdup(_binary_ln_claim_key_pem_start);
    return (*cert != NULL && *key != NULL);
#else
    return false;
#endif
}

static void start_task(void *arg)
{
    (void)arg;
    if (!ln_iot_is_provisioned()) {
        char *claim_cert = NULL;
        char *claim_key = NULL;
        char *tmpl = ln_iot_nvs_get_str(LN_IOT_K_TEMPLATE);
        if (s_iot.endpoint == NULL || !get_claim_material(&claim_cert, &claim_key)) {
            ESP_LOGE(TAG, "not provisioned and no claim material/endpoint — pair the device first");
            free(tmpl);
            ln_iot_post_event(LN_IOT_EVENT_PROVISION_FAILED, NULL, 0);
            s_iot.started = false;
            vTaskDelete(NULL);
            return;
        }
        char serial[64];
        if (s_iot.device_id != NULL) {
            strlcpy(serial, s_iot.device_id, sizeof(serial));
        } else {
            uint8_t mac[6] = {0};
            esp_read_mac(mac, ESP_MAC_EFUSE_FACTORY);
            snprintf(serial, sizeof(serial), "%02X%02X%02X%02X%02X%02X",
                     mac[0], mac[1], mac[2], mac[3], mac[4], mac[5]);
        }
        esp_err_t err = ln_iot_provision_run(
            s_iot.endpoint, claim_cert, claim_key,
            (tmpl != NULL) ? tmpl : CONFIG_LN_IOT_PROV_TEMPLATE, serial);
        free(claim_cert);
        free(claim_key);
        free(tmpl);
        if (err != ESP_OK) {
            ESP_LOGE(TAG, "fleet provisioning failed: %s", esp_err_to_name(err));
            ln_iot_post_event(LN_IOT_EVENT_PROVISION_FAILED, NULL, 0);
            s_iot.started = false;
            vTaskDelete(NULL);
            return;
        }
        load_identity(); /* pick up op cert/key/thing just stored */
        ln_iot_post_event(LN_IOT_EVENT_PROVISIONED, NULL, 0);
    }
    esp_err_t err = start_operational_client();
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "mqtt client start failed: %s", esp_err_to_name(err));
        s_iot.started = false;
    }
    vTaskDelete(NULL);
}

esp_err_t ln_iot_start(void)
{
    if (s_iot.lock == NULL) {
        return ESP_ERR_INVALID_STATE; /* init not called */
    }
    if (s_iot.started) {
        return ESP_OK;
    }
    s_iot.started = true;
    /* Provisioning does blocking TLS work — run off the caller's task. 8KB
     * stack covers mbedtls keygen + CSR writing. */
    if (xTaskCreate(start_task, "ln_iot_start", 8192, NULL, 5, NULL) != pdPASS) {
        s_iot.started = false;
        return ESP_ERR_NO_MEM;
    }
    return ESP_OK;
}

esp_err_t ln_iot_stop(void)
{
    s_iot.started = false;
    esp_timer_stop(s_iot.heartbeat_timer);
    if (s_iot.client != NULL) {
        esp_mqtt_client_stop(s_iot.client);
        esp_mqtt_client_destroy(s_iot.client);
        s_iot.client = NULL;
    }
    if (s_iot.connected) {
        s_iot.connected = false;
        ln_iot_post_event(LN_IOT_EVENT_DISCONNECTED, NULL, 0);
    }
    return ESP_OK;
}

void ln_iot_prepare_reboot(void)
{
    /* Give QoS1 publishes (job status, telemetry) a moment to flush. */
    vTaskDelay(pdMS_TO_TICKS(2000));
    if (s_iot.client != NULL) {
        esp_mqtt_client_stop(s_iot.client);
    }
}

/* ----------------------------------------------------------- public state */

bool ln_iot_is_provisioned(void)
{
    return s_iot.thing != NULL && s_iot.op_cert != NULL && s_iot.op_key != NULL &&
           s_iot.endpoint != NULL;
}

bool ln_iot_is_connected(void)
{
    return s_iot.connected;
}

const char *ln_iot_thing_name(void)
{
    return s_iot.thing;
}

const char *ln_iot_device_id(void)
{
    return s_iot.device_id;
}

esp_err_t ln_iot_store_bootstrap(const ln_iot_bootstrap_t *bootstrap)
{
    if (bootstrap == NULL) {
        return ESP_ERR_INVALID_ARG;
    }
    esp_err_t err = ESP_OK;
    if (bootstrap->iot_endpoint != NULL) {
        err |= ln_iot_nvs_set_str(LN_IOT_K_ENDPOINT, bootstrap->iot_endpoint);
    }
    if (bootstrap->claim_cert_pem != NULL) {
        err |= ln_iot_nvs_set_blob_str(LN_IOT_K_CLAIM_CERT, bootstrap->claim_cert_pem);
    }
    if (bootstrap->claim_key_pem != NULL) {
        err |= ln_iot_nvs_set_blob_str(LN_IOT_K_CLAIM_KEY, bootstrap->claim_key_pem);
    }
    if (bootstrap->template_name != NULL) {
        err |= ln_iot_nvs_set_str(LN_IOT_K_TEMPLATE, bootstrap->template_name);
    }
    if (bootstrap->device_id != NULL) {
        err |= ln_iot_nvs_set_str(LN_IOT_K_DEVICE_ID, bootstrap->device_id);
    }
    if (bootstrap->user_id != NULL) {
        err |= ln_iot_nvs_set_str(LN_IOT_K_USER_ID, bootstrap->user_id);
    }
    load_identity();
    return (err == ESP_OK) ? ESP_OK : ESP_FAIL;
}

esp_err_t ln_iot_factory_reset_credentials(bool full)
{
    ln_iot_stop();
    ln_iot_nvs_erase_key(LN_IOT_K_OP_CERT);
    ln_iot_nvs_erase_key(LN_IOT_K_OP_KEY);
    ln_iot_nvs_erase_key(LN_IOT_K_THING);
    ln_iot_nvs_erase_key(LN_IOT_K_SET_VER);
    ln_iot_nvs_erase_key(LN_IOT_K_OTA_JOB);
    ln_iot_nvs_erase_key(LN_IOT_K_OTA_VER);
    if (full) {
        ln_iot_nvs_erase_key(LN_IOT_K_CLAIM_CERT);
        ln_iot_nvs_erase_key(LN_IOT_K_CLAIM_KEY);
        ln_iot_nvs_erase_key(LN_IOT_K_ENDPOINT);
        ln_iot_nvs_erase_key(LN_IOT_K_TEMPLATE);
        ln_iot_nvs_erase_key(LN_IOT_K_DEVICE_ID);
        ln_iot_nvs_erase_key(LN_IOT_K_USER_ID);
    }
    load_identity();
    return ESP_OK;
}
