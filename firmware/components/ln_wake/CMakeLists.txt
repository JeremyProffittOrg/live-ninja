idf_component_register(
    SRCS "ln_wake.c"
    INCLUDE_DIRS "include"
    REQUIRES esp_event ln_audio
    PRIV_REQUIRES nvs_flash
)
