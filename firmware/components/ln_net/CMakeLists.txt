idf_component_register(
    SRCS
        "src/ln_net.c"
        "src/ln_portal.c"
        "src/ln_dns.c"
        "src/ln_backend.c"
        "src/ln_pairing.c"
        "src/ln_auth.c"
    INCLUDE_DIRS "include"
    PRIV_INCLUDE_DIRS "src"
    REQUIRES esp_event esp_netif
    PRIV_REQUIRES
        ln_iot
        esp_wifi
        esp_http_server
        esp_http_client
        esp-tls
        mbedtls
        json
        nvs_flash
        lwip
        esp_timer
        esp_app_format
    EMBED_TXTFILES "portal/portal.html"
)
