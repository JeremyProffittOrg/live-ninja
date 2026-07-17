/*
 * ln_ctrl — Live Ninja Tab5 control state machine (plan.md M5 state diagram).
 *
 * Owns the app-level state (Boot / Provisioning / Idle / Listening / Thinking /
 * Speaking / Config / Error), consumes LN_NET / LN_WAKE / LN_RT / LN_IOT /
 * LN_UI events on the default loop and drives ln_ui (via LN_CTRL_STATE_CHANGED
 * posts), ln_realtime (session start/stop/barge-in), ln_audio (uplink gating +
 * mic level), and ln_iot (shadow apply/report, app-state heartbeat string).
 */
#pragma once

#include "esp_err.h"

#ifdef __cplusplus
extern "C" {
#endif

/*
 * Start the control layer. Call AFTER: NVS + default event loop +
 * bsp_display_start() + ln_ui_init() + ln_audio/ln_wake/ln_realtime/ln_iot
 * init, but BEFORE ln_net_start() (so no provisioning event is missed).
 * Loads persisted settings ("ln_ctrl" NVS namespace), applies brightness,
 * registers all event handlers, starts the ctrl tick timer and posts the
 * initial Boot state.
 */
esp_err_t ln_ctrl_start(void);

#ifdef __cplusplus
}
#endif
