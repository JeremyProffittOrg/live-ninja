/*
 * ln_events.c — definitions for the event bases owned by the UI/ctrl layer.
 * LN_NET/LN_RT/LN_IOT/LN_WAKE bases are defined by their producer components
 * (see the ownership table in include/ln_events.h).
 */
#include "ln_events.h"

ESP_EVENT_DEFINE_BASE(LN_CTRL_EVENT);
ESP_EVENT_DEFINE_BASE(LN_UI_EVENT);
ESP_EVENT_DEFINE_BASE(LN_AUDIO_EVENT);
