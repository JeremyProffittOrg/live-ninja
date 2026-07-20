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
 *   - gemini-flash-live -> GEMINI_DIRECT (client-direct WSS to Gemini Live
 *     with a config-constrained ephemeral token in the URL — M13).
 */
typedef enum {
    LN_RT_ENGINE_OPENAI_DIRECT = 0, /**< default: wss://api.openai.com, Bearer ephemeral. */
    LN_RT_ENGINE_NOVA_BRIDGE,       /**< wss to nova.live.jeremy.ninja bridge (token in URL). */
    LN_RT_ENGINE_GEMINI_DIRECT,     /**< wss to generativelanguage.googleapis.com
                                         (?access_token=..., URL-escaped — M13). */
} ln_rt_engine_mode_t;

/** Result of GET {backend}/api/v1/realtime/session (contracts/api.md). */
typedef struct {
    ln_rt_engine_mode_t mode; /**< which transport the broker selected. */
    char token[512]; /**< OPENAI_DIRECT: ephemeral secret ("ek_..."). NOVA_BRIDGE:
                          single-use bridge token when returned as a separate
                          field (usually already embedded in ws_url; may be "").
                          GEMINI_DIRECT: ephemeral access token
                          ("auth_tokens/<id>", ~76 chars; carried URL-escaped in
                          the access_token query param). */
    char model[96];  /**< Realtime model for the WSS URL query param (OPENAI_DIRECT);
                          informational for GEMINI_DIRECT (locked at mint). */
    char ws_url[640]; /**< NOVA_BRIDGE: full bridge WSS URL to connect
                           (wss://nova.live.jeremy.ninja/... — normally already
                           carries the single-use token query param).
                           GEMINI_DIRECT: the BidiGenerateContentConstrained WSS
                           endpoint (110 chars, no query; ln_realtime appends
                           ?access_token=). Never populated from a wsUrl-family
                           field for Gemini — see gemini-plan.md §3.4. */
    const char *setup_frame; /**< GEMINI_DIRECT only: the complete {"setup":{...}}
                                  text frame to send on WSS connect (sessionConfig
                                  from the broker, resumption handle injected on
                                  reconnects). Points into a reused PSRAM buffer
                                  owned by ln_rt_session.c — valid until the next
                                  ln_rt_session_fetch(). NULL for other engines. */
} ln_rt_session_info_t;

/**
 * Fetch the broker's session bootstrap from the backend using the device JWT
 * (via ln_auth_get_jwt). Blocking; call from the worker task only.
 *
 * Resolves the OpenAI-direct ephemeral shape (out->mode ==
 * LN_RT_ENGINE_OPENAI_DIRECT, out->token/out->model), the Nova Sonic bridge
 * shape (out->mode == LN_RT_ENGINE_NOVA_BRIDGE, out->ws_url [+ out->token],
 * response {"mode":"nova-bridge","wsUrl":...,"token":...}), or the Gemini
 * Live direct shape (out->mode == LN_RT_ENGINE_GEMINI_DIRECT,
 * out->ws_url/out->token/out->setup_frame, response {"mode":"gemini-direct",
 * "geminiEndpoint":...,"accessToken":{"value":...},"sessionConfig":{...}}).
 *
 * On failure fills err_out (code/message/fatal) for the caller to post as an
 * LN_RT_EVENT_ERROR. fatal=true for auth (401/403) and quota (402/429) — the
 * caller must not retry-loop those.
 */
esp_err_t ln_rt_session_fetch(ln_rt_session_info_t *out, ln_rt_error_info_t *err_out);

/**
 * Latest Gemini session-resumption handle ("" when there is none), captured
 * by ln_realtime.c from sessionResumptionUpdate server messages. Read by
 * ln_rt_session_fetch() when it rebuilds the setup frame for a reconnect, so
 * the fresh token resumes the SAME Gemini conversation across the ~10-min
 * connection recycles (goAway). Safe to call from the worker task while no
 * WSS session is live (the only time a fetch runs).
 */
const char *ln_rt_resumption_handle(void);

#ifdef __cplusplus
}
#endif
