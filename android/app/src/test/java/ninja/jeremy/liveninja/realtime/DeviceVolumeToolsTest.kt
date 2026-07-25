package ninja.jeremy.liveninja.realtime

import android.media.AudioManager
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class DeviceVolumeToolsTest {

    private class FakeAudioVolumeGateway : AudioVolumeGateway {
        override var isVolumeFixed: Boolean = false
        val ranges = mutableMapOf<Int, IntRange>()
        val levels = mutableMapOf<Int, Int>()
        val muted = mutableMapOf<Int, Boolean>()
        val setCalls = mutableListOf<Pair<Int, Int>>()
        val adjustCalls = mutableListOf<Pair<Int, Int>>()
        var setFailure: RuntimeException? = null

        override fun minimum(stream: Int): Int = range(stream).first

        override fun maximum(stream: Int): Int = range(stream).last

        override fun current(stream: Int): Int = levels[stream] ?: range(stream).first

        override fun isMuted(stream: Int): Boolean = muted[stream] == true

        override fun set(stream: Int, index: Int) {
            setFailure?.let { throw it }
            setCalls += stream to index
            levels[stream] = index.coerceIn(range(stream))
            if (index > range(stream).first) muted[stream] = false
        }

        override fun adjust(stream: Int, direction: Int) {
            adjustCalls += stream to direction
            when (direction) {
                AudioManager.ADJUST_RAISE -> {
                    levels[stream] = (current(stream) + 1).coerceAtMost(maximum(stream))
                    muted[stream] = false
                }

                AudioManager.ADJUST_LOWER ->
                    levels[stream] = (current(stream) - 1).coerceAtLeast(minimum(stream))

                AudioManager.ADJUST_MUTE -> muted[stream] = true
                AudioManager.ADJUST_UNMUTE -> muted[stream] = false
            }
        }

        private fun range(stream: Int): IntRange = ranges[stream] ?: (0..10)
    }

    @Test
    fun everyPublicAddressableStreamHasTheServerWireName() {
        assertEquals(
            listOf(
                "media" to AudioManager.STREAM_MUSIC,
                "ring" to AudioManager.STREAM_RING,
                "notification" to AudioManager.STREAM_NOTIFICATION,
                "alarm" to AudioManager.STREAM_ALARM,
                "system" to AudioManager.STREAM_SYSTEM,
                "voice_call" to AudioManager.STREAM_VOICE_CALL,
                "dtmf" to AudioManager.STREAM_DTMF,
                "accessibility" to AudioManager.STREAM_ACCESSIBILITY,
            ),
            DeviceVolumeStream.entries.map { it.wireName to it.audioManagerStream },
        )
    }

    @Test
    fun omittedStreamDefaultsToMediaAndAbsoluteLevelUsesTheRealRange() {
        val audio = FakeAudioVolumeGateway().apply {
            ranges[AudioManager.STREAM_MUSIC] = 0..20
            levels[AudioManager.STREAM_MUSIC] = 4
        }
        val result = JSONObject(
            DeviceVolumeToolHandler(audio).execute(
                "call-volume",
                """{"action":"set","level":50}""",
            ),
        )

        assertEquals(listOf(AudioManager.STREAM_MUSIC to 10), audio.setCalls)
        assertTrue(result.getBoolean("ok"))
        val output = result.getJSONObject("output")
        assertEquals("media", output.getString("stream"))
        assertEquals(20, output.getInt("previousLevel"))
        assertEquals(50, output.getInt("level"))
        assertEquals("call-volume", result.getString("callId"))
        assertTrue(output.getString("instruction").contains("media volume is now 50 percent"))
    }

    @Test
    fun everyNamedStreamCanBeAdjustedAndNeverFallsThroughToMedia() {
        val audio = FakeAudioVolumeGateway()
        val handler = DeviceVolumeToolHandler(audio)

        for (stream in DeviceVolumeStream.entries) {
            val result = JSONObject(
                handler.execute(
                    "call-${stream.wireName}",
                    """{"action":"increase","stream":"${stream.wireName}"}""",
                ),
            )
            assertTrue("${stream.wireName} should succeed", result.getBoolean("ok"))
        }

        assertEquals(
            DeviceVolumeStream.entries.map {
                it.audioManagerStream to AudioManager.ADJUST_RAISE
            },
            audio.adjustCalls,
        )
    }

    @Test
    fun percentageMappingHonoursAStreamWhoseMinimumIsNotZero() {
        val audio = FakeAudioVolumeGateway().apply {
            ranges[AudioManager.STREAM_VOICE_CALL] = 1..5
            levels[AudioManager.STREAM_VOICE_CALL] = 1
        }
        val handler = DeviceVolumeToolHandler(audio)

        handler.execute(
            "c",
            """{"action":"set","stream":"voice_call","level":50}""",
        )

        assertEquals(AudioManager.STREAM_VOICE_CALL to 3, audio.setCalls.single())
    }

    @Test
    fun relativeMuteAndUnmuteActionsUseFrameworkDirections() {
        val audio = FakeAudioVolumeGateway()
        val handler = DeviceVolumeToolHandler(audio)

        val lower = JSONObject(handler.execute("lower", """{"action":"decrease"}"""))
        val mute = JSONObject(handler.execute("mute", """{"action":"mute"}"""))
        val unmute = JSONObject(handler.execute("unmute", """{"action":"unmute"}"""))

        assertTrue(lower.getBoolean("ok"))
        assertTrue(mute.getJSONObject("output").getBoolean("muted"))
        assertFalse(unmute.getJSONObject("output").getBoolean("muted"))
        assertEquals(
            listOf(
                AudioManager.STREAM_MUSIC to AudioManager.ADJUST_LOWER,
                AudioManager.STREAM_MUSIC to AudioManager.ADJUST_MUTE,
                AudioManager.STREAM_MUSIC to AudioManager.ADJUST_UNMUTE,
            ),
            audio.adjustCalls,
        )
    }

    @Test
    fun malformedOrIncompleteArgumentsFailWithoutChangingVolume() {
        val audio = FakeAudioVolumeGateway()
        val handler = DeviceVolumeToolHandler(audio)

        for (args in listOf(
            "not-json",
            "{}",
            """{"action":"set"}""",
            """{"action":"set","level":101}""",
            """{"action":"mute","stream":"bluetooth"}""",
            """{"action":"increase","surprise":true}""",
        )) {
            val result = JSONObject(handler.execute("bad", args))
            assertFalse(args, result.getBoolean("ok"))
            assertEquals("invalid_args", result.getJSONObject("error").getString("code"))
        }
        assertTrue(audio.setCalls.isEmpty())
        assertTrue(audio.adjustCalls.isEmpty())
    }

    @Test
    fun fixedVolumeAndAndroidPolicyDenialAreReportedHonestly() {
        val fixed = FakeAudioVolumeGateway().apply { isVolumeFixed = true }
        val fixedResult = JSONObject(
            DeviceVolumeToolHandler(fixed).execute("fixed", """{"action":"increase"}"""),
        )
        assertFalse(fixedResult.getBoolean("ok"))
        assertEquals("not_supported", fixedResult.getJSONObject("error").getString("code"))

        val denied = FakeAudioVolumeGateway().apply {
            setFailure = SecurityException("notification policy access denied")
        }
        val deniedResult = JSONObject(
            DeviceVolumeToolHandler(denied).execute(
                "denied",
                """{"action":"set","stream":"ring","level":0}""",
            ),
        )
        assertFalse(deniedResult.getBoolean("ok"))
        assertEquals("forbidden", deniedResult.getJSONObject("error").getString("code"))
    }
}
