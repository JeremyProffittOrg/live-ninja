package ninja.jeremy.liveninja.realtime

import org.json.JSONException
import org.json.JSONObject

/**
 * Typed view of the OpenAI Realtime server events that arrive on the
 * `oai-events` DataChannel (plan.md M4, Android §4). Only the events the
 * client acts on are modeled; everything else surfaces as [Other] so the
 * ViewModel/diagnostics can still observe the raw stream.
 */
sealed interface RealtimeEvent {
    /** `session.created` — the realtime session is live. */
    data class SessionCreated(val sessionId: String?) : RealtimeEvent

    /** `session.updated` — server acknowledged a session.update. */
    data object SessionUpdated : RealtimeEvent

    /** `input_audio_buffer.speech_started` — server VAD heard the user (barge-in trigger). */
    data object SpeechStarted : RealtimeEvent

    /** `input_audio_buffer.speech_stopped` — server VAD end-of-utterance. */
    data object SpeechStopped : RealtimeEvent

    /** `response.created` — the model started producing a response. */
    data class ResponseStarted(val responseId: String?) : RealtimeEvent

    /** `response.done` — the response finished (complete, cancelled, or failed). */
    data class ResponseDone(val responseId: String?) : RealtimeEvent

    /** `output_audio_buffer.started` — assistant audio is now playing (WebRTC-specific). */
    data object AssistantAudioStarted : RealtimeEvent

    /** `output_audio_buffer.stopped` / `.cleared` — assistant audio finished/flushed. */
    data object AssistantAudioStopped : RealtimeEvent

    /** `conversation.item.input_audio_transcription.delta` — partial user transcript. */
    data class UserTranscriptDelta(val itemId: String, val delta: String) : RealtimeEvent

    /** `conversation.item.input_audio_transcription.completed` — final user transcript. */
    data class UserTranscriptCompleted(val itemId: String, val text: String) : RealtimeEvent

    /** `response.[output_]audio_transcript.delta` — partial assistant transcript. */
    data class AssistantTranscriptDelta(val itemId: String, val delta: String) : RealtimeEvent

    /** `response.[output_]audio_transcript.done` — final assistant transcript for one item. */
    data class AssistantTranscriptDone(val itemId: String, val text: String) : RealtimeEvent

    /**
     * `response.function_call_arguments.done` — the model requested a tool call.
     * Routed to the backend tool router (`POST /api/v1/tools/invoke`) by
     * [ToolCallRouter]; the result goes back as a `function_call_output` item.
     */
    data class FunctionCall(
        val callId: String,
        val name: String,
        val argumentsJson: String,
    ) : RealtimeEvent

    /** `error` — server-reported error. */
    data class ServerError(val code: String?, val message: String) : RealtimeEvent

    /** Any event type not explicitly modeled above. */
    data class Other(val type: String, val json: JSONObject) : RealtimeEvent
}

/**
 * Parses raw DataChannel JSON into [RealtimeEvent]s. Pure JVM (org.json only)
 * so it is unit-testable without Robolectric.
 */
object RealtimeEventParser {

    /** Returns the typed event, or null when [raw] is not a JSON object with a `type`. */
    fun parse(raw: String): RealtimeEvent? {
        val json = try {
            JSONObject(raw)
        } catch (_: JSONException) {
            return null
        }
        val type = json.optString("type")
        if (type.isEmpty()) return null

        return when (type) {
            "session.created" ->
                RealtimeEvent.SessionCreated(json.optJSONObject("session")?.optString("id")?.ifEmpty { null })

            "session.updated" -> RealtimeEvent.SessionUpdated

            "input_audio_buffer.speech_started" -> RealtimeEvent.SpeechStarted
            "input_audio_buffer.speech_stopped" -> RealtimeEvent.SpeechStopped

            "response.created" ->
                RealtimeEvent.ResponseStarted(json.optJSONObject("response")?.optString("id")?.ifEmpty { null })

            "response.done" ->
                RealtimeEvent.ResponseDone(json.optJSONObject("response")?.optString("id")?.ifEmpty { null })

            "output_audio_buffer.started" -> RealtimeEvent.AssistantAudioStarted
            "output_audio_buffer.stopped", "output_audio_buffer.cleared" -> RealtimeEvent.AssistantAudioStopped

            "conversation.item.input_audio_transcription.delta" ->
                RealtimeEvent.UserTranscriptDelta(
                    itemId = json.optString("item_id"),
                    delta = json.optString("delta"),
                )

            "conversation.item.input_audio_transcription.completed" ->
                RealtimeEvent.UserTranscriptCompleted(
                    itemId = json.optString("item_id"),
                    text = json.optString("transcript"),
                )

            // Beta name and GA (gpt-realtime) name for the same stream.
            "response.audio_transcript.delta", "response.output_audio_transcript.delta" ->
                RealtimeEvent.AssistantTranscriptDelta(
                    itemId = json.optString("item_id"),
                    delta = json.optString("delta"),
                )

            "response.audio_transcript.done", "response.output_audio_transcript.done" ->
                RealtimeEvent.AssistantTranscriptDone(
                    itemId = json.optString("item_id"),
                    text = json.optString("transcript"),
                )

            "response.function_call_arguments.done" ->
                RealtimeEvent.FunctionCall(
                    callId = json.optString("call_id"),
                    name = json.optString("name"),
                    argumentsJson = json.optString("arguments").ifEmpty { "{}" },
                )

            "error" -> {
                val err = json.optJSONObject("error")
                RealtimeEvent.ServerError(
                    code = err?.optString("code")?.ifEmpty { null },
                    message = err?.optString("message").orEmpty().ifEmpty { "realtime server error" },
                )
            }

            else -> RealtimeEvent.Other(type, json)
        }
    }
}
