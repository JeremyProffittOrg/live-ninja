idf_component_register(
    SRCS "ln_audio.c" "ln_resample.c"
    INCLUDE_DIRS "include"
    PRIV_INCLUDE_DIRS "."
    REQUIRES esp_event
    PRIV_REQUIRES nvs_flash esp_driver_i2s
)
