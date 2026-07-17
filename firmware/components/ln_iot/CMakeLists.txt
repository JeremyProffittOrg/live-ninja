# ln_iot — AWS IoT Core (mTLS MQTT), fleet provisioning by claim, config shadow,
# telemetry heartbeat, IoT-Jobs A/B OTA.
#
# Optional build-time claim material (fallback when the pairing flow hasn't
# injected claim credentials into NVS): set env vars LN_IOT_CLAIM_CERT and
# LN_IOT_CLAIM_KEY to PEM file paths before `idf.py build`. The files are
# embedded into the app image and used only when NVS has no claim material.
# NEVER commit claim PEMs to the repo (deploy.md credential policy).

set(ln_iot_srcs
    "src/ln_iot.c"
    "src/ln_iot_nvs.c"
    "src/ln_iot_provision.c"
    "src/ln_iot_shadow.c"
    "src/ln_iot_ota.c"
    "src/ln_iot_root_ca.c")

set(ln_iot_embed "")
if(DEFINED ENV{LN_IOT_CLAIM_CERT} AND DEFINED ENV{LN_IOT_CLAIM_KEY})
    configure_file("$ENV{LN_IOT_CLAIM_CERT}" "${CMAKE_CURRENT_BINARY_DIR}/ln_claim_cert.pem" COPYONLY)
    configure_file("$ENV{LN_IOT_CLAIM_KEY}" "${CMAKE_CURRENT_BINARY_DIR}/ln_claim_key.pem" COPYONLY)
    list(APPEND ln_iot_embed
        "${CMAKE_CURRENT_BINARY_DIR}/ln_claim_cert.pem"
        "${CMAKE_CURRENT_BINARY_DIR}/ln_claim_key.pem")
endif()

idf_component_register(
    SRCS ${ln_iot_srcs}
    INCLUDE_DIRS "include"
    PRIV_INCLUDE_DIRS "src"
    REQUIRES esp_event
    PRIV_REQUIRES mqtt nvs_flash json esp_timer esp_app_format app_update
                  esp_https_ota esp_http_client esp_partition mbedtls esp-tls
    EMBED_TXTFILES ${ln_iot_embed})

if(ln_iot_embed)
    target_compile_definitions(${COMPONENT_LIB} PRIVATE LN_IOT_EMBEDDED_CLAIM=1)
    message(STATUS "ln_iot: embedding build-time claim certificate from $ENV{LN_IOT_CLAIM_CERT}")
endif()
