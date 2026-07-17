/*
 * ln_iot_provision — AWS IoT Fleet Provisioning by Claim.
 *
 * Security model: the operational private key is generated ON DEVICE
 * (EC P-256 via mbedtls + hardware RNG) and never leaves it — the claim
 * session uses CreateCertificateFromCsr (not CreateKeysAndCertificate), so
 * AWS only ever sees the CSR. RegisterThing binds the signed cert to a Thing
 * via the account's provisioning template; the per-device policy is scoped by
 * ${iot:Connection.Thing.ThingName} template-side. The signed cert + key are
 * persisted to NVS (namespace "ln_iot") for the operational session.
 *
 * (Migrating the key into the DS peripheral / an on-chip protected slot is
 * the M5 security-hardening task; the storage API here is already
 * blob-in-encrypted-NVS shaped so only the keygen/persist step changes.)
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "freertos/event_groups.h"
#include "cJSON.h"
#include "mbedtls/ctr_drbg.h"
#include "mbedtls/ecp.h"
#include "mbedtls/entropy.h"
#include "mbedtls/pk.h"
#include "mbedtls/x509_csr.h"
#include "mqtt_client.h"
#include "sdkconfig.h"

#include "ln_iot_priv.h"

static const char *TAG = "ln_iot_prov";

#define PEM_BUF_SIZE 2048
#define STAGE_TIMEOUT_MS 30000

#define BIT_CONNECTED  BIT0
#define BIT_SUBSCRIBED BIT1
#define BIT_CSR_OK     BIT2
#define BIT_REG_OK     BIT3
#define BIT_FAILED     BIT4

#define PROV_SUB_COUNT 4

typedef struct {
    EventGroupHandle_t eg;
    int subacks;
    /* Results (heap, freed by caller). */
    char *cert_pem;
    char *ownership_token;
    char *thing_name;
    /* Chunk accumulator. */
    char topic[192];
    char *buf;
    int total;
    int filled;
} prov_ctx_t;

/* ------------------------------------------------ key + CSR generation */

static esp_err_t generate_key_and_csr(const char *cn, char **key_pem_out, char **csr_pem_out)
{
    esp_err_t ret = ESP_FAIL;
    mbedtls_pk_context key;
    mbedtls_entropy_context entropy;
    mbedtls_ctr_drbg_context drbg;
    mbedtls_x509write_csr csr;
    unsigned char *key_pem = calloc(1, PEM_BUF_SIZE);
    unsigned char *csr_pem = calloc(1, PEM_BUF_SIZE);

    mbedtls_pk_init(&key);
    mbedtls_entropy_init(&entropy);
    mbedtls_ctr_drbg_init(&drbg);
    mbedtls_x509write_csr_init(&csr);

    if (key_pem == NULL || csr_pem == NULL) {
        goto out;
    }
    const char *pers = "ln_iot_prov";
    if (mbedtls_ctr_drbg_seed(&drbg, mbedtls_entropy_func, &entropy,
                              (const unsigned char *)pers, strlen(pers)) != 0) {
        ESP_LOGE(TAG, "drbg seed failed");
        goto out;
    }
    if (mbedtls_pk_setup(&key, mbedtls_pk_info_from_type(MBEDTLS_PK_ECKEY)) != 0 ||
        mbedtls_ecp_gen_key(MBEDTLS_ECP_DP_SECP256R1, mbedtls_pk_ec(key),
                            mbedtls_ctr_drbg_random, &drbg) != 0) {
        ESP_LOGE(TAG, "EC keygen failed");
        goto out;
    }
    if (mbedtls_pk_write_key_pem(&key, key_pem, PEM_BUF_SIZE) != 0) {
        ESP_LOGE(TAG, "key PEM write failed");
        goto out;
    }
    char subject[96];
    snprintf(subject, sizeof(subject), "CN=%s", cn);
    mbedtls_x509write_csr_set_md_alg(&csr, MBEDTLS_MD_SHA256);
    mbedtls_x509write_csr_set_key(&csr, &key);
    if (mbedtls_x509write_csr_set_subject_name(&csr, subject) != 0) {
        ESP_LOGE(TAG, "CSR subject failed");
        goto out;
    }
    int err = mbedtls_x509write_csr_pem(&csr, csr_pem, PEM_BUF_SIZE,
                                        mbedtls_ctr_drbg_random, &drbg);
    if (err != 0) {
        ESP_LOGE(TAG, "CSR PEM write failed: -0x%04x", -err);
        goto out;
    }
    *key_pem_out = strdup((char *)key_pem);
    *csr_pem_out = strdup((char *)csr_pem);
    ret = (*key_pem_out != NULL && *csr_pem_out != NULL) ? ESP_OK : ESP_ERR_NO_MEM;

out:
    mbedtls_x509write_csr_free(&csr);
    mbedtls_ctr_drbg_free(&drbg);
    mbedtls_entropy_free(&entropy);
    mbedtls_pk_free(&key);
    if (key_pem != NULL) {
        memset(key_pem, 0, PEM_BUF_SIZE); /* scrub key material */
    }
    free(key_pem);
    free(csr_pem);
    return ret;
}

/* --------------------------------------------------- MQTT claim session */

static void prov_handle_message(prov_ctx_t *ctx, const char *topic, const char *data)
{
    cJSON *root = cJSON_Parse(data);
    if (root == NULL) {
        ESP_LOGE(TAG, "bad JSON on %s", topic);
        xEventGroupSetBits(ctx->eg, BIT_FAILED);
        return;
    }
    if (strstr(topic, "create-from-csr/json/accepted") != NULL) {
        const cJSON *pem = cJSON_GetObjectItemCaseSensitive(root, "certificatePem");
        const cJSON *tok = cJSON_GetObjectItemCaseSensitive(root, "certificateOwnershipToken");
        if (cJSON_IsString(pem) && cJSON_IsString(tok)) {
            ctx->cert_pem = strdup(pem->valuestring);
            ctx->ownership_token = strdup(tok->valuestring);
            xEventGroupSetBits(ctx->eg, BIT_CSR_OK);
        } else {
            xEventGroupSetBits(ctx->eg, BIT_FAILED);
        }
    } else if (strstr(topic, "/provision/json/accepted") != NULL) {
        const cJSON *thing = cJSON_GetObjectItemCaseSensitive(root, "thingName");
        if (cJSON_IsString(thing)) {
            ctx->thing_name = strdup(thing->valuestring);
            xEventGroupSetBits(ctx->eg, BIT_REG_OK);
        } else {
            xEventGroupSetBits(ctx->eg, BIT_FAILED);
        }
    } else if (strstr(topic, "/rejected") != NULL) {
        char *msg = cJSON_PrintUnformatted(root);
        ESP_LOGE(TAG, "provisioning rejected on %s: %s", topic, msg ? msg : "?");
        cJSON_free(msg);
        xEventGroupSetBits(ctx->eg, BIT_FAILED);
    }
    cJSON_Delete(root);
}

static void prov_mqtt_handler(void *arg, esp_event_base_t base, int32_t event_id,
                              void *event_data)
{
    (void)base;
    prov_ctx_t *ctx = arg;
    esp_mqtt_event_handle_t event = event_data;
    switch ((esp_mqtt_event_id_t)event_id) {
    case MQTT_EVENT_CONNECTED:
        xEventGroupSetBits(ctx->eg, BIT_CONNECTED);
        break;
    case MQTT_EVENT_SUBSCRIBED:
        if (++ctx->subacks >= PROV_SUB_COUNT) {
            xEventGroupSetBits(ctx->eg, BIT_SUBSCRIBED);
        }
        break;
    case MQTT_EVENT_DATA:
        if (event->current_data_offset == 0) {
            free(ctx->buf);
            ctx->buf = malloc((size_t)event->total_data_len + 1);
            ctx->total = event->total_data_len;
            ctx->filled = 0;
            int tlen = event->topic_len < (int)sizeof(ctx->topic) - 1
                           ? event->topic_len
                           : (int)sizeof(ctx->topic) - 1;
            memcpy(ctx->topic, event->topic, (size_t)tlen);
            ctx->topic[tlen] = '\0';
        }
        if (ctx->buf == NULL) {
            break;
        }
        memcpy(ctx->buf + event->current_data_offset, event->data, (size_t)event->data_len);
        ctx->filled += event->data_len;
        if (ctx->filled >= ctx->total) {
            ctx->buf[ctx->total] = '\0';
            prov_handle_message(ctx, ctx->topic, ctx->buf);
            free(ctx->buf);
            ctx->buf = NULL;
        }
        break;
    case MQTT_EVENT_ERROR:
        ESP_LOGW(TAG, "claim-session MQTT error");
        break;
    default:
        break;
    }
}

static bool wait_bit(prov_ctx_t *ctx, EventBits_t bit)
{
    EventBits_t got = xEventGroupWaitBits(ctx->eg, bit | BIT_FAILED, pdFALSE, pdFALSE,
                                          pdMS_TO_TICKS(STAGE_TIMEOUT_MS));
    return (got & bit) != 0 && (got & BIT_FAILED) == 0;
}

esp_err_t ln_iot_provision_run(const char *endpoint, const char *claim_cert_pem,
                               const char *claim_key_pem, const char *template_name,
                               const char *serial_number)
{
    ESP_LOGI(TAG, "fleet provisioning: template=%s serial=%s", template_name, serial_number);

    char *key_pem = NULL;
    char *csr_pem = NULL;
    esp_err_t err = generate_key_and_csr(serial_number, &key_pem, &csr_pem);
    if (err != ESP_OK) {
        return err;
    }

    prov_ctx_t ctx = { .eg = xEventGroupCreate() };
    if (ctx.eg == NULL) {
        err = ESP_ERR_NO_MEM;
        goto out_keys;
    }

    char uri[192];
    char client_id[64];
    snprintf(uri, sizeof(uri), "mqtts://%s:8883", endpoint);
    snprintf(client_id, sizeof(client_id), "ln-claim-%s", serial_number);
    const esp_mqtt_client_config_t cfg = {
        .broker = {
            .address.uri = uri,
            .verification.certificate = ln_iot_aws_root_ca_pem,
        },
        .credentials = {
            .client_id = client_id,
            .authentication = { .certificate = claim_cert_pem, .key = claim_key_pem },
        },
        .session = { .keepalive = 60, .protocol_ver = MQTT_PROTOCOL_V_3_1_1 },
        .buffer = { .size = CONFIG_LN_IOT_MQTT_BUF_SIZE,
                    .out_size = CONFIG_LN_IOT_MQTT_BUF_SIZE },
        .network = { .disable_auto_reconnect = true },
    };
    esp_mqtt_client_handle_t client = esp_mqtt_client_init(&cfg);
    if (client == NULL) {
        err = ESP_FAIL;
        goto out_eg;
    }
    esp_mqtt_client_register_event(client, ESP_EVENT_ANY_ID, prov_mqtt_handler, &ctx);
    err = esp_mqtt_client_start(client);
    if (err != ESP_OK) {
        goto out_client;
    }

    err = ESP_FAIL;
    if (!wait_bit(&ctx, BIT_CONNECTED)) {
        ESP_LOGE(TAG, "claim connect failed/timed out (cert revoked or endpoint wrong?)");
        goto out_client;
    }

    char topic[192];
    esp_mqtt_client_subscribe(client, "$aws/certificates/create-from-csr/json/accepted", 1);
    esp_mqtt_client_subscribe(client, "$aws/certificates/create-from-csr/json/rejected", 1);
    snprintf(topic, sizeof(topic), "$aws/provisioning-templates/%s/provision/json/accepted",
             template_name);
    esp_mqtt_client_subscribe(client, topic, 1);
    snprintf(topic, sizeof(topic), "$aws/provisioning-templates/%s/provision/json/rejected",
             template_name);
    esp_mqtt_client_subscribe(client, topic, 1);
    if (!wait_bit(&ctx, BIT_SUBSCRIBED)) {
        ESP_LOGE(TAG, "claim SUBACKs missing (claim policy too narrow?)");
        goto out_client;
    }

    /* Stage 1: CSR -> signed operational certificate + ownership token. */
    {
        cJSON *req = cJSON_CreateObject();
        cJSON_AddStringToObject(req, "certificateSigningRequest", csr_pem);
        char *payload = cJSON_PrintUnformatted(req);
        cJSON_Delete(req);
        if (payload == NULL) {
            err = ESP_ERR_NO_MEM;
            goto out_client;
        }
        esp_mqtt_client_publish(client, "$aws/certificates/create-from-csr/json", payload, 0,
                                1, 0);
        cJSON_free(payload);
    }
    if (!wait_bit(&ctx, BIT_CSR_OK)) {
        ESP_LOGE(TAG, "CreateCertificateFromCsr failed");
        goto out_client;
    }

    /* Stage 2: RegisterThing against the provisioning template. */
    {
        cJSON *req = cJSON_CreateObject();
        cJSON_AddStringToObject(req, "certificateOwnershipToken", ctx.ownership_token);
        cJSON *params = cJSON_AddObjectToObject(req, "parameters");
        cJSON_AddStringToObject(params, "SerialNumber", serial_number);
        char *payload = cJSON_PrintUnformatted(req);
        cJSON_Delete(req);
        if (payload == NULL) {
            err = ESP_ERR_NO_MEM;
            goto out_client;
        }
        snprintf(topic, sizeof(topic), "$aws/provisioning-templates/%s/provision/json",
                 template_name);
        esp_mqtt_client_publish(client, topic, payload, 0, 1, 0);
        cJSON_free(payload);
    }
    if (!wait_bit(&ctx, BIT_REG_OK)) {
        ESP_LOGE(TAG, "RegisterThing failed");
        goto out_client;
    }

    /* Persist the operational identity atomically enough for our purposes:
     * cert+key first, thing name last (ln_iot_is_provisioned keys off all 3). */
    if (ln_iot_nvs_set_blob_str(LN_IOT_K_OP_CERT, ctx.cert_pem) != ESP_OK ||
        ln_iot_nvs_set_blob_str(LN_IOT_K_OP_KEY, key_pem) != ESP_OK ||
        ln_iot_nvs_set_str(LN_IOT_K_THING, ctx.thing_name) != ESP_OK) {
        err = ESP_FAIL;
        goto out_client;
    }
    ESP_LOGI(TAG, "provisioned as thing \"%s\"", ctx.thing_name);
    err = ESP_OK;

out_client:
    esp_mqtt_client_stop(client);
    esp_mqtt_client_destroy(client);
out_eg:
    vEventGroupDelete(ctx.eg);
    free(ctx.buf);
    free(ctx.cert_pem);
    free(ctx.ownership_token);
    free(ctx.thing_name);
out_keys:
    if (key_pem != NULL) {
        memset(key_pem, 0, strlen(key_pem));
    }
    free(key_pem);
    free(csr_pem);
    return err;
}
