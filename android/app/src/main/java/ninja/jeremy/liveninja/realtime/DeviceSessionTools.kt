package ninja.jeremy.liveninja.realtime

import org.json.JSONObject

/**
 * The device-local half of the `stop_listening` / `start_new_conversation` tools
 * (owner request 2026-07-25: "close the app when asked — stop listening", and
 * "ask the application to start a new conversation").
 *
 * Both are declared server-side so the manifest advertises them
 * (internal/tools/devicesession.go, `DeviceLocal = true`), but their work is an
 * action on *this* device's microphone and session — the backend has no reach
 * into either. The client therefore intercepts them before they would reach
 * `POST /api/v1/tools/invoke`.
 *
 * Kept as a pure mapping so the decision is unit-tested without a live session.
 */
enum class DeviceSessionTool {
    /** Stop the wake service and end the live session. */
    STOP_LISTENING,

    /** End this session and immediately begin a fresh one, so History gets a separate row. */
    START_NEW_CONVERSATION,
    ;

    companion object {
        const val NAME_STOP_LISTENING = "stop_listening"
        const val NAME_START_NEW_CONVERSATION = "start_new_conversation"

        /** The device-local tool for [name], or null if it belongs to the backend router. */
        fun forName(name: String): DeviceSessionTool? = when (name) {
            NAME_STOP_LISTENING -> STOP_LISTENING
            NAME_START_NEW_CONVERSATION -> START_NEW_CONVERSATION
            else -> null
        }
    }
}

/**
 * The `function_call_output` payload for a device-local tool.
 *
 * Mirrors the backend `Result` shape ({tool, callId, ok, output}) so the model cannot tell
 * the difference between a locally-handled tool and a routed one — anything else would
 * make the two paths behave differently for no reason the model can reason about.
 *
 * `output.acknowledged` is deliberately not `done`: at the moment this is sent the action has
 * NOT happened yet. It runs once the assistant has finished speaking its confirmation, because
 * stopping the session the instant the tool fires cuts off the very reply that tells the user
 * what happened.
 */
fun deviceToolOutput(tool: DeviceSessionTool, callId: String): String {
    val spoken = when (tool) {
        DeviceSessionTool.STOP_LISTENING ->
            "Listening will stop as soon as you finish this reply. Say it briefly, and tell " +
                "the user they can start listening again from the app."

        DeviceSessionTool.START_NEW_CONVERSATION ->
            "A fresh conversation will start as soon as you finish this reply. Acknowledge " +
                "briefly; do not summarise the previous conversation."
    }
    return JSONObject()
        .put("tool", toolName(tool))
        .put("callId", callId)
        .put("ok", true)
        .put(
            "output",
            JSONObject()
                .put("acknowledged", true)
                .put("instruction", spoken),
        )
        .toString()
}

private fun toolName(tool: DeviceSessionTool): String = when (tool) {
    DeviceSessionTool.STOP_LISTENING -> DeviceSessionTool.NAME_STOP_LISTENING
    DeviceSessionTool.START_NEW_CONVERSATION -> DeviceSessionTool.NAME_START_NEW_CONVERSATION
}
