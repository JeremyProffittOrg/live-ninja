idf_component_register(
    SRCS "ln_realtime.c" "ln_rt_session.c"
    INCLUDE_DIRS "include"
    REQUIRES esp_event
    PRIV_REQUIRES esp_http_client esp_websocket_client json mbedtls esp-tls esp_app_format
)
