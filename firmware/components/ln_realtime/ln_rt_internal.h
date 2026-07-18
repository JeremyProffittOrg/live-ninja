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

/**
 * Voice-engine transport the broker resolved for this device (FR-VE-01/03).
 * The per-device `voiceEngine` pin (settings.schema.json) selects it:
 *   - openai-realtime / openai-realtime-mini -> OPENAI_DIRECT (client-direct
 *     WSS straight to OpenAI, the default path).
 *   - nova-sonic -> NOVA_BRIDGE (WSS to the backend Nova Sonic media bridge;
 *     AWS holds the Bedrock bidirectional stream — M12).
 */
typedef enum {
    LN_RT_ENGINE_OPENAI_DIRECT = 0, /**< default: wss://api.openai.com, Bearer ephemeral. */
    LN_RT_ENGINE_NOVA_BRIDGE,       /**< wss to nova.live.jeremy.ninja bridge (token in URL). */
} ln_rt_engine_mode_t;

/** Result of GET {backend}/v1/realtime/session (contracts/api.md). */
typedef struct {
    ln_rt_engine_mode_t mode; /**< which transport the broker selected. */
    char token[512]; /**< OPENAI_DIRECT: ephemeral secret ("ek_..."). NOVA_BRIDGE:
                          single-use bridge token when returned as a separate
                          field (usually already embedded in ws_url; may be ""). */
    char model[96];  /**< Realtime model for the WSS URL query param (OPENAI_DIRECT). */
    char ws_url[640]; /**< NOVA_BRIDGE: full bridge WSS URL to connect
                           (wss://nova.live.jeremy.ninja/... — normally already
                           carries the single-use token query param). */
} ln_rt_session_info_t;

/**
 * Fetch the broker's session bootstrap from the backend using the device JWT
 * (via ln_auth_get_jwt). Blocking; call from the worker task only.
 *
 * Resolves either the OpenAI-direct ephemeral shape (out->mode ==
 * LN_RT_ENGINE_OPENAI_DIRECT, out->token/out->model) or the Nova Sonic bridge
 * shape (out->mode == LN_RT_ENGINE_NOVA_BRIDGE, out->ws_url [+ out->token],
 * response {"mode":"nova-bridge","wsUrl":...,"token":...}).
 *
 * On failure fills err_out (code/message/fatal) for the caller to post as an
 * LN_RT_EVENT_ERROR. fatal=true for auth (401/403) and quota (402/429) — the
 * caller must not retry-loop those.
 */
esp_err_t ln_rt_session_fetch(ln_rt_session_info_t *out, ln_rt_error_info_t *err_out);

#ifdef __cplusplus
}
#endif
