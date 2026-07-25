package ninja.jeremy.liveninja.realtime

import android.content.Context
import android.media.AudioManager
import dagger.hilt.android.qualifiers.ApplicationContext
import javax.inject.Inject
import javax.inject.Singleton
import kotlin.math.roundToInt
import org.json.JSONException
import org.json.JSONObject

/** Server-advertised name intercepted locally before [ToolCallRouter]. */
const val DEVICE_VOLUME_TOOL_NAME = "set_volume"

/**
 * Every public AudioManager stream addressable by the app's API level.
 *
 * The wire names exactly match internal/tools/devicevolume.go. MEDIA is the
 * first/default entry by owner decision; a caller must explicitly name any
 * other stream.
 */
internal enum class DeviceVolumeStream(
    val wireName: String,
    val audioManagerStream: Int,
) {
    MEDIA("media", AudioManager.STREAM_MUSIC),
    RING("ring", AudioManager.STREAM_RING),
    NOTIFICATION("notification", AudioManager.STREAM_NOTIFICATION),
    ALARM("alarm", AudioManager.STREAM_ALARM),
    SYSTEM("system", AudioManager.STREAM_SYSTEM),
    VOICE_CALL("voice_call", AudioManager.STREAM_VOICE_CALL),
    DTMF("dtmf", AudioManager.STREAM_DTMF),
    ACCESSIBILITY("accessibility", AudioManager.STREAM_ACCESSIBILITY),
    ;

    companion object {
        fun fromWireName(name: String): DeviceVolumeStream? =
            entries.firstOrNull { it.wireName == name }
    }
}

internal enum class DeviceVolumeAction(val wireName: String) {
    SET("set"),
    INCREASE("increase"),
    DECREASE("decrease"),
    MUTE("mute"),
    UNMUTE("unmute"),
    ;

    companion object {
        fun fromWireName(name: String): DeviceVolumeAction? =
            entries.firstOrNull { it.wireName == name }
    }
}

/**
 * Android-backed executor for the device-local [DEVICE_VOLUME_TOOL_NAME] tool.
 *
 * The server owns the schema and advertises the capability, but only this
 * process owns AudioManager. Returning the backend Result shape keeps the
 * function-call round trip identical to a server-routed tool.
 */
@Singleton
class DeviceVolumeToolExecutor @Inject constructor(
    @ApplicationContext context: Context,
) {
    private val handler = DeviceVolumeToolHandler(
        AndroidAudioVolumeGateway(
            requireNotNull(context.getSystemService(AudioManager::class.java)) {
                "AudioManager is unavailable"
            },
        ),
    )

    fun execute(callId: String, argumentsJson: String): String =
        handler.execute(callId, argumentsJson)
}

/**
 * Narrow framework seam: the command/validation/result logic remains a JVM
 * unit test while production delegates the actual mutation to AudioManager.
 */
internal interface AudioVolumeGateway {
    val isVolumeFixed: Boolean

    fun minimum(stream: Int): Int
    fun maximum(stream: Int): Int
    fun current(stream: Int): Int
    fun isMuted(stream: Int): Boolean
    fun set(stream: Int, index: Int)
    fun adjust(stream: Int, direction: Int)
}

private class AndroidAudioVolumeGateway(
    private val audioManager: AudioManager,
) : AudioVolumeGateway {
    override val isVolumeFixed: Boolean
        get() = audioManager.isVolumeFixed

    override fun minimum(stream: Int): Int = audioManager.getStreamMinVolume(stream)

    override fun maximum(stream: Int): Int = audioManager.getStreamMaxVolume(stream)

    override fun current(stream: Int): Int = audioManager.getStreamVolume(stream)

    override fun isMuted(stream: Int): Boolean = audioManager.isStreamMute(stream)

    override fun set(stream: Int, index: Int) {
        audioManager.setStreamVolume(stream, index, AudioManager.FLAG_SHOW_UI)
    }

    override fun adjust(stream: Int, direction: Int) {
        audioManager.adjustStreamVolume(stream, direction, AudioManager.FLAG_SHOW_UI)
    }
}

internal class DeviceVolumeToolHandler(
    private val audio: AudioVolumeGateway,
) {
    fun execute(callId: String, argumentsJson: String): String {
        val command = try {
            parseCommand(argumentsJson)
        } catch (e: InvalidVolumeArguments) {
            return errorOutput(callId, "invalid_args", e.message ?: "invalid volume arguments")
        }

        return try {
            if (audio.isVolumeFixed) {
                return errorOutput(
                    callId,
                    "not_supported",
                    "Volume is fixed on this device and cannot be changed by the app.",
                )
            }

            val stream = command.stream.audioManagerStream
            val before = snapshot(stream)
            when (command.action) {
                DeviceVolumeAction.SET -> {
                    val requested = requireNotNull(command.level)
                    audio.set(stream, percentToIndex(requested, before.minimum, before.maximum))
                }

                DeviceVolumeAction.INCREASE ->
                    audio.adjust(stream, AudioManager.ADJUST_RAISE)

                DeviceVolumeAction.DECREASE ->
                    audio.adjust(stream, AudioManager.ADJUST_LOWER)

                DeviceVolumeAction.MUTE ->
                    audio.adjust(stream, AudioManager.ADJUST_MUTE)

                DeviceVolumeAction.UNMUTE ->
                    audio.adjust(stream, AudioManager.ADJUST_UNMUTE)
            }
            val after = snapshot(stream)

            if (command.action == DeviceVolumeAction.MUTE && !after.muted) {
                return errorOutput(
                    callId,
                    "not_supported",
                    "Android did not allow the ${displayName(command.stream)} volume to be muted.",
                )
            }
            if (command.action == DeviceVolumeAction.UNMUTE && after.muted) {
                return errorOutput(
                    callId,
                    "not_supported",
                    "Android did not allow the ${displayName(command.stream)} volume to be unmuted.",
                )
            }

            successOutput(callId, command, before, after)
        } catch (_: SecurityException) {
            errorOutput(
                callId,
                "forbidden",
                "Android blocked that volume change. Ring or notification changes may require " +
                    "Do Not Disturb access.",
            )
        } catch (e: RuntimeException) {
            errorOutput(
                callId,
                "device_error",
                e.message?.takeIf { it.isNotBlank() } ?: "Android could not change that volume.",
            )
        }
    }

    private fun parseCommand(argumentsJson: String): VolumeCommand {
        val json = try {
            JSONObject(argumentsJson)
        } catch (_: JSONException) {
            throw InvalidVolumeArguments("arguments must be a JSON object")
        }

        val allowed = setOf("action", "level", "stream")
        val keys = json.keys()
        while (keys.hasNext()) {
            val key = keys.next()
            if (key !in allowed) {
                throw InvalidVolumeArguments("unexpected argument \"$key\" for tool set_volume")
            }
        }

        val actionName = requiredString(json, "action")
        val action = DeviceVolumeAction.fromWireName(actionName)
            ?: throw InvalidVolumeArguments(
                "action must be one of ${DeviceVolumeAction.entries.map { it.wireName }}",
            )

        val stream = if (!json.has("stream") || json.isNull("stream")) {
            DeviceVolumeStream.MEDIA
        } else {
            val streamName = requiredString(json, "stream")
            DeviceVolumeStream.fromWireName(streamName)
                ?: throw InvalidVolumeArguments(
                    "stream must be one of ${DeviceVolumeStream.entries.map { it.wireName }}",
                )
        }

        val level = optionalInteger(json, "level")
        if (level != null && level !in 0..100) {
            throw InvalidVolumeArguments("level must be between 0 and 100")
        }
        if (action == DeviceVolumeAction.SET && level == null) {
            throw InvalidVolumeArguments("level is required when action is set")
        }

        return VolumeCommand(action = action, stream = stream, level = level)
    }

    private fun requiredString(json: JSONObject, name: String): String {
        if (!json.has(name) || json.isNull(name)) {
            throw InvalidVolumeArguments("missing required argument \"$name\"")
        }
        val value = json.get(name)
        if (value !is String || value.isEmpty()) {
            throw InvalidVolumeArguments("$name must be a non-empty string")
        }
        return value
    }

    private fun optionalInteger(json: JSONObject, name: String): Int? {
        if (!json.has(name) || json.isNull(name)) return null
        val number = json.get(name) as? Number
            ?: throw InvalidVolumeArguments("$name must be an integer")
        val double = number.toDouble()
        if (!double.isFinite() || double != double.toInt().toDouble()) {
            throw InvalidVolumeArguments("$name must be a whole number")
        }
        return double.toInt()
    }

    private fun snapshot(stream: Int): VolumeSnapshot {
        val minimum = audio.minimum(stream)
        val maximum = audio.maximum(stream)
        if (maximum < minimum) {
            throw IllegalStateException("Android reported an invalid volume range")
        }
        return VolumeSnapshot(
            minimum = minimum,
            maximum = maximum,
            index = audio.current(stream).coerceIn(minimum, maximum),
            muted = audio.isMuted(stream),
        )
    }

    private fun successOutput(
        callId: String,
        command: VolumeCommand,
        before: VolumeSnapshot,
        after: VolumeSnapshot,
    ): String {
        val previousLevel = indexToPercent(before.index, before.minimum, before.maximum)
        val resultingLevel = indexToPercent(after.index, after.minimum, after.maximum)
        val streamName = displayName(command.stream)
        val instruction = when {
            after.muted ->
                "The $streamName volume is now muted. Confirm that briefly."

            command.action == DeviceVolumeAction.UNMUTE ->
                "The $streamName volume is unmuted at $resultingLevel percent. Confirm that briefly."

            else ->
                "The $streamName volume is now $resultingLevel percent. Confirm that briefly."
        }

        return JSONObject()
            .put("tool", DEVICE_VOLUME_TOOL_NAME)
            .put("callId", callId)
            .put("ok", true)
            .put(
                "output",
                JSONObject()
                    .put("stream", command.stream.wireName)
                    .put("action", command.action.wireName)
                    .put("previousLevel", previousLevel)
                    .put("level", resultingLevel)
                    .put("muted", after.muted)
                    .put(
                        "changed",
                        before.index != after.index || before.muted != after.muted,
                    )
                    .put("instruction", instruction),
            )
            .toString()
    }

    private fun errorOutput(callId: String, code: String, message: String): String =
        JSONObject()
            .put("tool", DEVICE_VOLUME_TOOL_NAME)
            .put("callId", callId)
            .put("ok", false)
            .put(
                "error",
                JSONObject()
                    .put("code", code)
                    .put("message", message),
            )
            .toString()

    private fun percentToIndex(percent: Int, minimum: Int, maximum: Int): Int {
        if (maximum == minimum) return minimum
        return minimum + ((maximum - minimum) * (percent / 100.0)).roundToInt()
    }

    private fun indexToPercent(index: Int, minimum: Int, maximum: Int): Int {
        if (maximum == minimum) return 100
        return (((index - minimum) * 100.0) / (maximum - minimum))
            .roundToInt()
            .coerceIn(0, 100)
    }

    private fun displayName(stream: DeviceVolumeStream): String =
        stream.wireName.replace('_', ' ')

    private data class VolumeCommand(
        val action: DeviceVolumeAction,
        val stream: DeviceVolumeStream,
        val level: Int?,
    )

    private data class VolumeSnapshot(
        val minimum: Int,
        val maximum: Int,
        val index: Int,
        val muted: Boolean,
    )

    private class InvalidVolumeArguments(message: String) : IllegalArgumentException(message)
}
