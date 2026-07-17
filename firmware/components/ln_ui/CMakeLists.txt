idf_component_register(
    SRCS
        "ln_events.c"
        "ln_ui.c"
        "ln_ui_widgets.c"
        "ln_ui_scr_idle.c"
        "ln_ui_scr_session.c"
        "ln_ui_scr_config.c"
        "ln_ui_scr_onboarding.c"
        "ln_ui_scr_error.c"
    INCLUDE_DIRS "include"
    REQUIRES esp_event
    PRIV_REQUIRES esp_timer esp_app_format esp_hw_support log heap
                  ln_net ln_realtime ln_iot ln_wake
)
