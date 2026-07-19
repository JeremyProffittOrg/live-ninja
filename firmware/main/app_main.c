/*
 * Live Ninja — M5Stack Tab5 firmware entry point.
 *
 * Boot sequence (plan.md M5): NVS init -> default event loop -> BSP display
 * (MIPI-DSI + GT911 touch via esp_lvgl_port) -> Live Ninja UI -> audio
 * pipeline (ES8388/ES7210 full-duplex I2S) -> wake pipeline (AFE/WakeNet) ->
 * realtime client -> IoT client -> ctrl state machine -> network (C6 WiFi via
 * esp-hosted, portal, pairing, auth). ln_net_start() comes last so the ctrl
 * handlers are already registered when the first provisioning events fire.
 */
#include "esp_app_desc.h"
#include "esp_event.h"
#include "esp_heap_caps.h"
#include "esp_hosted_api_types.h"
#include "esp_log.h"
#include "esp_system.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "nvs_flash.h"

#include "bsp/m5stack_tab5.h"

#include "ln_audio.h"
#include "ln_ctrl.h"
#include "ln_iot.h"
#include "ln_net.h"
#include "ln_realtime.h"
#include "ln_ui.h"
#include "ln_wake.h"

static const char *TAG = "ln_main";

/* ---- TEMPORARY C6 slave OTA (owner-approved 2026-07-19; REMOVE after) ----
 *
 * The C6 runs esp-hosted slave v1.4.1 against our v1.4.7 host driver; the
 * skew is the prime suspect for the recurring SDIO asserts
 * (sdio_rx_get_buffer). This one-shot task waits for WiFi, reads the slave
 * version, and — only if it's older than the host component (1.4.7) —
 * streams the matching slave image (built from the managed component's
 * slave/ project, served from the dev PC over LAN HTTP) to the C6's
 * inactive A/B slot via esp_hosted_slave_ota(). The host auto-restarts on
 * success; the version gate makes the next boot a no-op. */
extern esp_err_t esp_hosted_get_coprocessor_fwversion(esp_hosted_coprocessor_fwver_t *ver_info);
extern esp_err_t esp_hosted_slave_ota(const char *image_url);

#define LN_C6_OTA_URL "http://192.168.1.58:8666/network_adapter.bin"
#define LN_C6_WANT_MAJOR 1
#define LN_C6_WANT_MINOR 4
#define LN_C6_WANT_PATCH 7

static void c6_ota_task(void *arg)
{
    (void)arg;
    while (!ln_net_is_online()) {
        vTaskDelay(pdMS_TO_TICKS(1000));
    }
    vTaskDelay(pdMS_TO_TICKS(3000)); /* let SNTP/auth settle first */

    esp_hosted_coprocessor_fwver_t ver = {0};
    if (esp_hosted_get_coprocessor_fwversion(&ver) != ESP_OK) {
        ESP_LOGE(TAG, "c6-ota: slave version query failed — not flashing blind");
        vTaskDelete(NULL);
        return;
    }
    uint32_t cur = ver.major1 * 10000 + ver.minor1 * 100 + ver.patch1;
    uint32_t want = LN_C6_WANT_MAJOR * 10000 + LN_C6_WANT_MINOR * 100 + LN_C6_WANT_PATCH;
    if (cur >= want) {
        ESP_LOGI(TAG, "c6-ota: slave v%lu.%lu.%lu already current — nothing to do",
                 (unsigned long)ver.major1, (unsigned long)ver.minor1,
                 (unsigned long)ver.patch1);
        vTaskDelete(NULL);
        return;
    }
    ESP_LOGW(TAG, "c6-ota: updating slave v%lu.%lu.%lu -> v%d.%d.%d from %s",
             (unsigned long)ver.major1, (unsigned long)ver.minor1,
             (unsigned long)ver.patch1,
             LN_C6_WANT_MAJOR, LN_C6_WANT_MINOR, LN_C6_WANT_PATCH, LN_C6_OTA_URL);
    esp_err_t err = esp_hosted_slave_ota(LN_C6_OTA_URL);
    /* Success path never returns (host esp_restart()s); reaching here means
     * the OTA failed after its internal retries. */
    ESP_LOGE(TAG, "c6-ota: FAILED (%s) — slave unchanged, rerun after fixing", esp_err_to_name(err));
    vTaskDelete(NULL);
}
/* ---- end TEMPORARY C6 slave OTA ---- */

static void log_internal(const char *stage)
{
    ESP_LOGI(TAG, "[heap] after %s: internal free %u (largest %u)", stage,
             (unsigned)heap_caps_get_free_size(MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT),
             (unsigned)heap_caps_get_largest_free_block(MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT));
}

static void ln_nvs_init(void)
{
    esp_err_t err = nvs_flash_init();
    if (err == ESP_ERR_NVS_NO_FREE_PAGES || err == ESP_ERR_NVS_NEW_VERSION_FOUND) {
        ESP_LOGW(TAG, "NVS needs erase (%s), erasing", esp_err_to_name(err));
        ESP_ERROR_CHECK(nvs_flash_erase());
        err = nvs_flash_init();
    }
    ESP_ERROR_CHECK(err);
}

void app_main(void)
{
    const esp_app_desc_t *app = esp_app_get_description();
    ESP_LOGI(TAG, "Live Ninja Tab5 firmware %s (%s, IDF %s)",
             app->version, app->project_name, app->idf_ver);

    ln_nvs_init();
    ESP_ERROR_CHECK(esp_event_loop_create_default());

    /* Display + touch via BSP (esp_lvgl_port task owns LVGL). */
    log_internal("nvs+eventloop");
    lv_display_t *disp = bsp_display_start();
    if (disp == NULL) {
        ESP_LOGE(TAG, "bsp_display_start failed — no display");
        esp_restart();
    }
    ESP_ERROR_CHECK(bsp_display_backlight_on());
    log_internal("display");
    ESP_ERROR_CHECK(ln_ui_init());
    log_internal("ln_ui");

    /* Audio + wake. A codec/model failure must not brick provisioning —
     * log, show a degraded status, and keep booting. */
    esp_err_t err = ln_audio_init();
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "ln_audio_init failed: %s (voice disabled)",
                 esp_err_to_name(err));
    } else {
        log_internal("ln_audio");
        err = ln_wake_init();
        if (err != ESP_OK) {
            ESP_LOGE(TAG, "ln_wake_init failed: %s (wake disabled)",
                     esp_err_to_name(err));
        }
    }

    log_internal("ln_wake");
    ESP_ERROR_CHECK(ln_realtime_init());
    ESP_ERROR_CHECK(ln_iot_init());
    log_internal("ln_realtime+ln_iot");

    /* Ctrl before net: its handlers must see the first portal/pairing events. */
    ESP_ERROR_CHECK(ln_ctrl_start());

    log_internal("ln_ctrl");
    ESP_ERROR_CHECK(ln_net_init());
    log_internal("ln_net_init");
    ESP_ERROR_CHECK(ln_net_start());

    ESP_LOGI(TAG, "boot complete, free heap %lu (internal %u, largest block %u)",
             (unsigned long)esp_get_free_heap_size(),
             (unsigned)heap_caps_get_free_size(MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT),
             (unsigned)heap_caps_get_largest_free_block(MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT));

    /* TEMPORARY (see c6_ota_task above): one-shot C6 slave update. */
    xTaskCreate(c6_ota_task, "c6_ota", 4096, NULL, 3, NULL);
}
