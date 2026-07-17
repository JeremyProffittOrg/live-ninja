package ninja.jeremy.liveninja.wake

import android.content.Context
import android.content.SharedPreferences
import dagger.hilt.android.qualifiers.ApplicationContext
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow

/**
 * Local persistence for the wake stack: service enabled/muted, selected wake word + engine +
 * sensitivity. Plain (non-encrypted) SharedPreferences — nothing here is a secret; the session
 * tokens live in the auth stack's Keystore-backed storage.
 *
 * These mirror `contracts/settings.schema.json` fields (`wakeWord`, `wakeEngine`,
 * `sensitivity` — same names, same defaults) so the M6 settings-sync layer can write straight
 * through here when a `settings.updated` push arrives.
 */
@Singleton
class WakePreferences @Inject constructor(@ApplicationContext context: Context) {

    private val prefs: SharedPreferences =
        context.getSharedPreferences("wake", Context.MODE_PRIVATE)

    /** User has turned the always-listening service on (drives BOOT_COMPLETED restart). */
    var serviceEnabled: Boolean
        get() = prefs.getBoolean(KEY_SERVICE_ENABLED, false)
        set(value) = prefs.edit().putBoolean(KEY_SERVICE_ENABLED, value).apply()

    /** Mic muted from the persistent notification; service stays up, engine stays stopped. */
    var muted: Boolean
        get() = prefs.getBoolean(KEY_MUTED, false)
        set(value) {
            prefs.edit().putBoolean(KEY_MUTED, value).apply()
            mutedFlow.value = value
        }

    val mutedFlow = MutableStateFlow(prefs.getBoolean(KEY_MUTED, false))

    /** Active wake-word catalog id (settings.schema.json `wakeWord`, default hey-live-ninja). */
    var wakeWordId: String
        get() = prefs.getString(KEY_WAKE_WORD, DEFAULT_WAKE_WORD_ID) ?: DEFAULT_WAKE_WORD_ID
        set(value) = prefs.edit().putString(KEY_WAKE_WORD, value).apply()

    /** Engine id (settings.schema.json `wakeEngine` enum member). */
    var wakeEngine: String
        get() = prefs.getString(KEY_WAKE_ENGINE, ENGINE_OPENWAKEWORD) ?: ENGINE_OPENWAKEWORD
        set(value) = prefs.edit().putString(KEY_WAKE_ENGINE, value).apply()

    /** Detection sensitivity 0..1 (settings.schema.json `sensitivity`, default 0.5). */
    var sensitivity: Float
        get() = prefs.getFloat(KEY_SENSITIVITY, 0.5f)
        set(value) {
            prefs.edit().putFloat(KEY_SENSITIVITY, value.coerceIn(0f, 1f)).apply()
            sensitivityFlow.value = value.coerceIn(0f, 1f)
        }

    val sensitivityFlow: MutableStateFlow<Float> = MutableStateFlow(prefs.getFloat(KEY_SENSITIVITY, 0.5f))

    /**
     * Picovoice AccessKey for the optional Porcupine engine (progressive-disclosure field,
     * settings.schema.json `wakeEngine` = "porcupine"). Null until the user supplies one.
     * Device-local only; never synced to the backend.
     */
    var porcupineAccessKey: String?
        get() = prefs.getString(KEY_PORCUPINE_ACCESS_KEY, null)
        set(value) = prefs.edit().putString(KEY_PORCUPINE_ACCESS_KEY, value).apply()

    /** Observable mute state for the service/notification. */
    fun mutedState(): StateFlow<Boolean> = mutedFlow

    companion object {
        const val ENGINE_OPENWAKEWORD = "openwakeword"
        const val ENGINE_PORCUPINE = "porcupine"
        const val DEFAULT_WAKE_WORD_ID = "hey-live-ninja"

        private const val KEY_SERVICE_ENABLED = "serviceEnabled"
        private const val KEY_MUTED = "muted"
        private const val KEY_WAKE_WORD = "wakeWord"
        private const val KEY_WAKE_ENGINE = "wakeEngine"
        private const val KEY_SENSITIVITY = "sensitivity"
        private const val KEY_PORCUPINE_ACCESS_KEY = "porcupineAccessKey"
    }
}
