package ninja.jeremy.liveninja.realtime

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class RealtimeEventParserTest {

    @Test
    fun `speech started maps to barge-in trigger`() {
        val event = RealtimeEventParser.parse("""{"type":"input_audio_buffer.speech_started"}""")
        assertEquals(RealtimeEvent.SpeechStarted, event)
    }

    @Test
    fun `session created carries id`() {
        val event = RealtimeEventParser.parse(
            """{"type":"session.created","session":{"id":"sess_123"}}""",
        )
        assertEquals(RealtimeEvent.SessionCreated("sess_123"), event)
    }

    @Test
    fun `assistant transcript delta - both beta and GA names`() {
        val beta = RealtimeEventParser.parse(
            """{"type":"response.audio_transcript.delta","item_id":"item_1","delta":"Hel"}""",
        )
        assertEquals(RealtimeEvent.AssistantTranscriptDelta("item_1", "Hel"), beta)

        val ga = RealtimeEventParser.parse(
            """{"type":"response.output_audio_transcript.delta","item_id":"item_1","delta":"lo"}""",
        )
        assertEquals(RealtimeEvent.AssistantTranscriptDelta("item_1", "lo"), ga)
    }

    @Test
    fun `user transcript completed`() {
        val event = RealtimeEventParser.parse(
            """{"type":"conversation.item.input_audio_transcription.completed","item_id":"item_9","transcript":"what time is it"}""",
        )
        assertEquals(RealtimeEvent.UserTranscriptCompleted("item_9", "what time is it"), event)
    }

    @Test
    fun `function call arguments done`() {
        val event = RealtimeEventParser.parse(
            """{"type":"response.function_call_arguments.done","call_id":"call_7","name":"memory.search","arguments":"{\"query\":\"dentist\"}"}""",
        )
        assertEquals(
            RealtimeEvent.FunctionCall("call_7", "memory.search", """{"query":"dentist"}"""),
            event,
        )
    }

    @Test
    fun `function call with empty arguments defaults to empty object`() {
        val event = RealtimeEventParser.parse(
            """{"type":"response.function_call_arguments.done","call_id":"call_8","name":"time.now","arguments":""}""",
        ) as RealtimeEvent.FunctionCall
        assertEquals("{}", event.argumentsJson)
    }

    @Test
    fun `output audio buffer stopped and cleared both map to audio stopped`() {
        assertEquals(
            RealtimeEvent.AssistantAudioStopped,
            RealtimeEventParser.parse("""{"type":"output_audio_buffer.stopped"}"""),
        )
        assertEquals(
            RealtimeEvent.AssistantAudioStopped,
            RealtimeEventParser.parse("""{"type":"output_audio_buffer.cleared"}"""),
        )
    }

    @Test
    fun `error event carries code and message`() {
        val event = RealtimeEventParser.parse(
            """{"type":"error","error":{"code":"invalid_request","message":"bad event"}}""",
        )
        assertEquals(RealtimeEvent.ServerError("invalid_request", "bad event"), event)
    }

    @Test
    fun `unknown event surfaces as Other`() {
        val event = RealtimeEventParser.parse(
            """{"type":"rate_limits.updated","rate_limits":[]}""",
        )
        assertTrue(event is RealtimeEvent.Other)
        assertEquals("rate_limits.updated", (event as RealtimeEvent.Other).type)
    }

    @Test
    fun `non-json and typeless payloads return null`() {
        assertNull(RealtimeEventParser.parse("not json"))
        assertNull(RealtimeEventParser.parse("""{"no_type":true}"""))
    }
}
