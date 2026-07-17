/*
 * ln_rt_internal.h — private shared declarations for ln_realtime.
 */
#pragma once

#include <stdbool.h>
#include <stddef.h>

#include "esp_err.h"
#include "ln_realtime.h"

#ifdef __cplusplus
extern "C" {
#endif

/** Result of GET {backend}/v1/realtime/session (contracts/api.md). */
typedef struct {
    char token[512]; /**< OpenAI ephemeral client secret ("ek_..."). */
    char model[96];  /**< Realtime model for the WSS URL query param. */
} ln_rt_session_info_t;

/**
 * Fetch a broker-minted ephemeral token from the backend using the device JWT
 * (via ln_auth_get_jwt). Blocking; call from the worker task only.
 *
 * On failure fills err_out (code/message/fatal) for the caller to post as an
 * LN_RT_EVENT_ERROR. fatal=true for auth (401/403), quota (402/429) and
 * unsupported-engine responses — the caller must not retry-loop those.
 */
esp_err_t ln_rt_session_fetch(ln_rt_session_info_t *out, ln_rt_error_info_t *err_out);

#ifdef __cplusplus
}
#endif
