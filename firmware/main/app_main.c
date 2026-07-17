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
#include "esp_log.h"
#include "esp_system.h"
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
}
