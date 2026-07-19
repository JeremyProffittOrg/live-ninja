idf_component_register(
    SRCS "app_main.c" "ln_ctrl.c"
    PRIV_REQUIRES
        ln_ui ln_audio ln_wake ln_net ln_iot ln_realtime
        nvs_flash esp_event esp_app_format esp_timer esp_hw_support
)
