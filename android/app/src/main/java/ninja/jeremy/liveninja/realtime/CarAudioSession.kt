package ninja.jeremy.liveninja.realtime

import android.Manifest
import android.app.ActivityManager
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.content.pm.PackageManager
import android.media.AudioAttributes
import android.media.AudioDeviceCallback
import android.media.AudioDeviceInfo
import android.media.AudioFocusRequest
import android.media.AudioManager
import android.os.Build
import android.os.Handler
import android.os.Looper
import androidx.annotation.RequiresApi
import androidx.core.content.ContextCompat
import dagger.hilt.android.qualifiers.ApplicationContext
import java.io.IOException
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.Job
import kotlinx.coroutines.async
import kotlinx.coroutines.coroutineScope
import kotlinx.coroutines.ensureActive
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withTimeoutOrNull
import ninja.jeremy.liveninja.log.LNLog
import ninja.jeremy.liveninja.log.LogCategory

/**
 * Phone-based car audio (opt-in): the full assistant runs on the phone and its
 * call audio (mic + playback) is carried by the car's Bluetooth hands-free
 * (HFP/SCO) or a BLE headset. NOT Android Auto projection.
 *
 * Ownership model: one [CarAudioSessionCore] (behind the @Singleton
 * [CarAudioSessionManager]) owns audio focus, MODE_IN_COMMUNICATION and the
 * communication route for the whole session, for every provider. Transports
 * consult the process-wide [CarAudioRouteOwner.process] and skip their own
 * legacy routing while it is claimed. That legacy routing differs by provider:
 * Nova always chose the speaker; WebRTC/Gemini on API 31+ preferred a
 * connected headset with speaker fallback; the API 29/30 legacy paths forced
 * the speaker. Claims and releases carry an explicit generation so a stale
 * lease can never release a replacement session's route.
 */

/** Local, device-only opt-in. Dedicated SharedPreferences file; default false. */
@Singleton
class CarAudioPreferences @Inject constructor(
    @ApplicationContext context: Context,
) {
    private val prefs = context.getSharedPreferences(PREFS_NAME, Context.MODE_PRIVATE)
    private val _enabled = MutableStateFlow(prefs.getBoolean(KEY_ENABLED, false))

    /** Observable opt-in state for the Settings UI. */
    val enabled: StateFlow<Boolean> = _enabled.asStateFlow()

    val isEnabled: Boolean get() = _enabled.value

    fun setEnabled(enabled: Boolean) {
        prefs.edit().putBoolean(KEY_ENABLED, enabled).apply()
        _enabled.value = enabled
    }

    companion object {
        const val PREFS_NAME = "car_audio_prefs"
        private const val KEY_ENABLED = "car_audio_enabled"

        /** Runtime permissions the UI must hold before enabling car audio. */
        fun requiredRuntimePermissions(sdkInt: Int = Build.VERSION.SDK_INT): List<String> =
            buildList {
                add(Manifest.permission.RECORD_AUDIO)
                if (sdkInt >= Build.VERSION_CODES.S) add(Manifest.permission.BLUETOOTH_CONNECT)
            }
    }
}

/** Why car audio could not start or had to end. [userMessage] is actionable. */
enum class CarAudioFailure(val code: String, val userMessage: String) {
    NOT_FOREGROUND(
        "car_audio_not_foreground",
        "Open Live Ninja on your phone and start the conversation from the app to use car audio.",
    ),
    MIC_PERMISSION(
        "car_audio_mic_permission",
        "Live Ninja needs microphone permission to talk through your car. Allow the microphone in Settings.",
    ),
    BLUETOOTH_PERMISSION(
        "car_audio_bluetooth_permission",
        "Allow Live Ninja's Nearby devices (Bluetooth) permission to use your car's microphone and speakers, or turn off Car audio.",
    ),
    CALL_ACTIVE(
        "car_audio_call_active",
        "A phone call or another calling app is using call audio. Try again after it ends.",
    ),
    NO_DEVICE(
        "car_audio_no_device",
        "No Bluetooth car or headset with a microphone is connected. Connect your phone to the car's Bluetooth for calls (hands-free), or turn off Car audio in Settings.",
    ),
    FOCUS_DENIED(
        "car_audio_focus_denied",
        "Another app is holding the audio. Stop it or try again in a moment.",
    ),
    ROUTE_FAILED(
        "car_audio_route_failed",
        "Couldn't switch audio to your car's Bluetooth hands-free. Check that the car is connected for phone calls and try again.",
    ),
    FOCUS_LOST(
        "car_audio_focus_lost",
        "Car audio was interrupted by another app or call, so the conversation ended. Start again when you're ready.",
    ),
    ROUTE_LOST(
        "car_audio_route_lost",
        "Your car or headset disconnected, so the conversation ended to keep audio off the phone.",
    ),
    SUPERSEDED(
        "car_audio_superseded",
        "Car audio was taken over by a newer conversation.",
    ),
    FOREGROUND_DENIED(
        "car_audio_foreground_denied",
        "Android didn't let Live Ninja keep the car conversation running. Open Live Ninja and start the conversation from the app again.",
    ),
    ENDED_BY_USER(
        "car_audio_ended",
        "The car audio conversation was ended.",
    ),
}

class CarAudioException(val failure: CarAudioFailure) : IOException(failure.userMessage)

/** Platform-neutral view of a communication device ([AudioDeviceInfo] id/type). */
data class CarAudioDevice(val id: Int, val type: Int, val name: String)

/** What a transport should do with mode/route when it connects. */
enum class TransportAudioPlan { LEGACY_TRANSPORT_ROUTING, CAR_SESSION_OWNED }

/**
 * Generation-bound record of "the car session owns call audio". Reads are
 * lock-free (per audio frame); mutations are serialized.
 */
class CarAudioRouteOwner {
    private data class State(val generation: Long, val suppressed: Boolean)

    private val lock = Any()

    @Volatile
    private var state: State? = null

    val isCarOwned: Boolean get() = state != null

    /** True after an interruption: transports must drop mic and playback audio. */
    val isAudioSuppressed: Boolean get() = state?.suppressed == true

    fun audioPlan(): TransportAudioPlan =
        if (state != null) TransportAudioPlan.CAR_SESSION_OWNED else TransportAudioPlan.LEGACY_TRANSPORT_ROUTING

    internal fun claim(generation: Long) {
        synchronized(lock) { state = State(generation, suppressed = false) }
    }

    internal fun suppress(generation: Long): Boolean = synchronized(lock) {
        val current = state
        if (current?.generation == generation) {
            state = current.copy(suppressed = true)
            true
        } else {
            false
        }
    }

    internal fun release(generation: Long): Boolean = synchronized(lock) {
        if (state?.generation == generation) {
            state = null
            true
        } else {
            false
        }
    }

    companion object {
        /** The process-wide owner the transports consult. */
        val process = CarAudioRouteOwner()
    }
}

/**
 * Communication devices that carry BOTH mic and playback for a car/headset.
 * A2DP (playback only), BLE speaker, speaker, earpiece and wired are never
 * eligible: no silent fallback while car audio is selected.
 */
internal fun selectCarCommunicationDevice(candidates: List<CarAudioDevice>, sdkInt: Int): CarAudioDevice? {
    candidates.firstOrNull { it.type == AudioDeviceInfo.TYPE_BLUETOOTH_SCO }?.let { return it }
    if (sdkInt >= Build.VERSION_CODES.S) {
        candidates.firstOrNull { it.type == AudioDeviceInfo.TYPE_BLE_HEADSET }?.let { return it }
    }
    return null
}

/** Every loss, including transient MAY_DUCK, ends a car session. Gains never restart one. */
internal fun isCarAudioFocusLoss(change: Int): Boolean =
    change == AudioManager.AUDIOFOCUS_LOSS ||
        change == AudioManager.AUDIOFOCUS_LOSS_TRANSIENT ||
        change == AudioManager.AUDIOFOCUS_LOSS_TRANSIENT_CAN_DUCK

/** Platform effects behind [CarAudioSessionCore]; faked in JVM tests. */
interface CarAudioPlatform {
    val sdkInt: Int
    fun isForegroundAllowed(): Boolean
    fun hasMicPermission(): Boolean
    /** Always true below API 31. */
    fun hasBluetoothConnectPermission(): Boolean
    /** A phone call / other call app already owns call audio. */
    fun isCallAudioBusy(): Boolean
    fun communicationCandidates(): List<CarAudioDevice>
    /** True only for AUDIOFOCUS_REQUEST_GRANTED. [onFocusLoss] fires on any loss. */
    fun requestFocus(onFocusLoss: () -> Unit): Boolean
    fun abandonFocus()
    fun enterCommunicationMode()
    /** Restores only if the mode is still the one this session set. */
    fun restoreCommunicationMode()
    /**
     * Route call audio to [device] and wait (bounded) until the route is
     * observed. [onRouteLost] fires if the route is lost after that.
     */
    suspend fun connectRoute(device: CarAudioDevice, onRouteLost: () -> Unit): Boolean
    /** Idempotent: undo only what [connectRoute] did. */
    fun disconnectRoute()
}

/** Handle on one acquired car session. [release] is idempotent and generation-safe. */
class CarAudioLease internal constructor(
    val generation: Long,
    val device: CarAudioDevice,
    private val interruptionProvider: () -> CarAudioFailure?,
    private val releaser: () -> Unit,
) {
    val interruption: CarAudioFailure? get() = interruptionProvider()
    fun release() = releaser()
}

/**
 * Pure car-audio session state machine. One attempt is active at a time;
 * every platform callback is bound to its attempt object so a stale callback
 * can neither interrupt nor release a replacement session. An attempt stays
 * `active` until its physical cleanup (route, mode, focus) has finished, and
 * the next acquire waits for that, so platform state is never shared by two
 * attempts at once.
 */
class CarAudioSessionCore(
    private val platform: CarAudioPlatform,
    private val owner: CarAudioRouteOwner,
) {
    private enum class Phase { ACQUIRING, ACTIVE, RELEASED }

    private class Attempt(
        val generation: Long,
        val onInterrupted: (CarAudioFailure) -> Unit,
    ) {
        var phase = Phase.ACQUIRING
        var interruption: CarAudioFailure? = null
        var focusRequested = false
        var modeEntered = false
        var routeRequested = false
        var routeJob: Job? = null
        val cleanupDone = CompletableDeferred<Unit>()
    }

    private val lock = Any()
    private val acquireMutex = Mutex()
    private var generation = 0L
    private var active: Attempt? = null

    /**
     * Non-acquiring readiness check (for UI and the first acquire step).
     * [sessionForeground] may vouch for foreground only when a verified
     * session-owned foreground service, started from the visible app, is live.
     */
    fun readiness(requireForeground: Boolean, sessionForeground: () -> Boolean = { false }): CarAudioFailure? {
        if (requireForeground && !platform.isForegroundAllowed() && !sessionForeground()) {
            return CarAudioFailure.NOT_FOREGROUND
        }
        if (!platform.hasMicPermission()) return CarAudioFailure.MIC_PERMISSION
        if (!platform.hasBluetoothConnectPermission()) return CarAudioFailure.BLUETOOTH_PERMISSION
        if (platform.isCallAudioBusy()) return CarAudioFailure.CALL_ACTIVE
        if (selectCarCommunicationDevice(platform.communicationCandidates(), platform.sdkInt) == null) {
            return CarAudioFailure.NO_DEVICE
        }
        return null
    }

    /**
     * Acquire focus + communication mode + an observed car route. Throws
     * [CarAudioException]; on any failure or cancellation everything this
     * attempt changed is restored. [onInterrupted] is invoked at most once,
     * only after a successful acquire, on any focus or route loss.
     */
    suspend fun acquire(
        sessionForeground: () -> Boolean = { false },
        onInterrupted: (CarAudioFailure) -> Unit,
    ): CarAudioLease = acquireMutex.withLock {
        // A lease leaked by a crashed owner must not block car audio forever,
        // and a release still running on another thread must finish its
        // physical cleanup before this attempt touches the same platform state.
        synchronized(lock) { active }?.let { previous ->
            cleanup(previous)
            previous.cleanupDone.await()
        }
        readiness(requireForeground = true, sessionForeground = sessionForeground)?.let { throw CarAudioException(it) }
        val device = selectCarCommunicationDevice(platform.communicationCandidates(), platform.sdkInt)
            ?: throw CarAudioException(CarAudioFailure.NO_DEVICE)
        val attempt = synchronized(lock) {
            Attempt(++generation, onInterrupted).also { active = it }
        }
        var acquired = false
        try {
            step(attempt) { focusRequested = true }
            if (!platform.requestFocus { interrupt(attempt, CarAudioFailure.FOCUS_LOST) }) {
                throw CarAudioException(CarAudioFailure.FOCUS_DENIED)
            }
            step(attempt) {
                owner.claim(generation)
                modeEntered = true
            }
            platform.enterCommunicationMode()
            step(attempt) { routeRequested = true }
            if (!connectRouteInterruptibly(attempt, device)) {
                throw CarAudioException(CarAudioFailure.ROUTE_FAILED)
            }
            synchronized(lock) {
                attempt.interruption?.let { throw CarAudioException(it) }
                if (attempt.phase != Phase.ACQUIRING || active !== attempt) {
                    throw CarAudioException(CarAudioFailure.SUPERSEDED)
                }
                attempt.phase = Phase.ACTIVE
                attempt.routeJob = null
            }
            acquired = true
            CarAudioLease(
                attempt.generation,
                device,
                { synchronized(lock) { attempt.interruption } },
                { cleanup(attempt) },
            )
        } finally {
            if (!acquired) cleanup(attempt)
        }
    }

    private inline fun step(attempt: Attempt, block: Attempt.() -> Unit) {
        synchronized(lock) {
            attempt.interruption?.let { throw CarAudioException(it) }
            if (attempt.phase != Phase.ACQUIRING) throw CarAudioException(CarAudioFailure.SUPERSEDED)
            attempt.block()
        }
    }

    private suspend fun connectRouteInterruptibly(attempt: Attempt, device: CarAudioDevice): Boolean =
        coroutineScope {
            val routeJob = async {
                try {
                    platform.connectRoute(device) { interrupt(attempt, CarAudioFailure.ROUTE_LOST) }
                } catch (e: CancellationException) {
                    throw e
                } catch (e: Exception) {
                    false
                }
            }
            val cancelNow = synchronized(lock) {
                attempt.routeJob = routeJob
                attempt.interruption != null || attempt.phase != Phase.ACQUIRING
            }
            if (cancelNow) routeJob.cancel()
            try {
                routeJob.await()
            } catch (e: CancellationException) {
                ensureActive() // the caller itself was cancelled: propagate
                val failure = synchronized(lock) { attempt.interruption } ?: CarAudioFailure.SUPERSEDED
                throw CarAudioException(failure)
            }
        }

    private fun interrupt(attempt: Attempt, failure: CarAudioFailure) {
        var job: Job? = null
        val callback: ((CarAudioFailure) -> Unit)? = synchronized(lock) {
            if (attempt.phase == Phase.RELEASED || attempt.interruption != null || active !== attempt) return
            attempt.interruption = failure
            owner.suppress(attempt.generation)
            job = attempt.routeJob
            if (attempt.phase == Phase.ACTIVE) attempt.onInterrupted else null
        }
        job?.cancel()
        callback?.invoke(failure)
    }

    private fun cleanup(attempt: Attempt) {
        var route = false
        var mode = false
        var focus = false
        var job: Job? = null
        synchronized(lock) {
            if (attempt.phase == Phase.RELEASED) return
            attempt.phase = Phase.RELEASED
            // `active` stays set until the physical cleanup below is done; the
            // next acquire waits on cleanupDone instead of overlapping it.
            route = attempt.routeRequested
            mode = attempt.modeEntered
            focus = attempt.focusRequested
            job = attempt.routeJob
            attempt.routeJob = null
        }
        try {
            job?.cancel()
            if (route) runCatching { platform.disconnectRoute() }
            if (mode) runCatching { platform.restoreCommunicationMode() }
            if (focus) runCatching { platform.abandonFocus() }
            owner.release(attempt.generation)
        } finally {
            synchronized(lock) { if (active === attempt) active = null }
            attempt.cleanupDone.complete(Unit)
        }
    }
}

/**
 * Route handshake bookkeeping: the route counts only once observed AND still
 * present when the waiter takes it; a later loss is reported exactly once.
 */
internal class RouteHandshake(private val onLost: () -> Unit) {
    private val lock = Any()
    private val outcome = CompletableDeferred<Boolean>()
    private var seen = false
    private var established = false
    private var lostEarly = false
    private var lostReported = false

    fun observed() {
        synchronized(lock) { seen = true }
        outcome.complete(true)
    }

    /** The route ended. Before it was ever seen, [failIfUnseen] fails the handshake. */
    fun routeEnded(failIfUnseen: Boolean) {
        var notify = false
        var fail = false
        synchronized(lock) {
            when {
                established -> if (!lostReported) {
                    lostReported = true
                    notify = true
                }
                seen -> lostEarly = true
                failIfUnseen -> fail = true
            }
        }
        if (fail) outcome.complete(false)
        if (notify) onLost()
    }

    suspend fun awaitEstablished(timeoutMs: Long): Boolean {
        val ok = withTimeoutOrNull(timeoutMs) { outcome.await() } ?: false
        if (!ok) return false
        return synchronized(lock) {
            if (lostEarly) {
                false
            } else {
                established = true
                true
            }
        }
    }
}

/** Android implementation of [CarAudioPlatform]. Never scans or pairs. */
internal class AndroidCarAudioPlatform(private val context: Context) : CarAudioPlatform {
    private val audioManager: AudioManager? = context.getSystemService(AudioManager::class.java)
    private val mainHandler = Handler(Looper.getMainLooper())
    private val lock = Any()

    private var focusRequest: AudioFocusRequest? = null
    private var previousMode: Int? = null
    private var communicationDeviceRequested = false
    private var communicationListener: Any? = null
    private var deviceCallback: AudioDeviceCallback? = null
    private var scoReceiver: BroadcastReceiver? = null
    private var scoStarted = false
    private var previousScoOn: Boolean? = null

    override val sdkInt: Int get() = Build.VERSION.SDK_INT

    override fun isForegroundAllowed(): Boolean {
        val info = ActivityManager.RunningAppProcessInfo()
        ActivityManager.getMyMemoryState(info)
        return info.importance <= ActivityManager.RunningAppProcessInfo.IMPORTANCE_FOREGROUND
    }

    private fun granted(permission: String): Boolean =
        ContextCompat.checkSelfPermission(context, permission) == PackageManager.PERMISSION_GRANTED

    override fun hasMicPermission(): Boolean = granted(Manifest.permission.RECORD_AUDIO)

    override fun hasBluetoothConnectPermission(): Boolean =
        Build.VERSION.SDK_INT < Build.VERSION_CODES.S || granted(Manifest.permission.BLUETOOTH_CONNECT)

    override fun isCallAudioBusy(): Boolean {
        val am = audioManager ?: return false
        return am.mode != AudioManager.MODE_NORMAL
    }

    override fun communicationCandidates(): List<CarAudioDevice> {
        val am = audioManager ?: return emptyList()
        return if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
            am.availableCommunicationDevices.map { it.toCarDevice() }
        } else {
            if (!am.isBluetoothScoAvailableOffCall) return emptyList()
            am.getDevices(AudioManager.GET_DEVICES_OUTPUTS)
                .filter { it.type == AudioDeviceInfo.TYPE_BLUETOOTH_SCO }
                .map { it.toCarDevice() }
        }
    }

    private fun AudioDeviceInfo.toCarDevice() =
        CarAudioDevice(id, type, productName?.toString().orEmpty())

    override fun requestFocus(onFocusLoss: () -> Unit): Boolean {
        val am = audioManager ?: return false
        val request = AudioFocusRequest.Builder(AudioManager.AUDIOFOCUS_GAIN_TRANSIENT)
            .setAudioAttributes(
                AudioAttributes.Builder()
                    .setUsage(AudioAttributes.USAGE_VOICE_COMMUNICATION)
                    .setContentType(AudioAttributes.CONTENT_TYPE_SPEECH)
                    .build(),
            )
            .setAcceptsDelayedFocusGain(false)
            // Deliver MAY_DUCK to us instead of the system ducking silently.
            .setWillPauseWhenDucked(true)
            .setOnAudioFocusChangeListener(
                { change ->
                    LNLog.d(LogCategory.AUDIO, TAG, "car audio focus change: $change")
                    if (isCarAudioFocusLoss(change)) onFocusLoss()
                },
                mainHandler,
            )
            .build()
        synchronized(lock) { focusRequest = request }
        return am.requestAudioFocus(request) == AudioManager.AUDIOFOCUS_REQUEST_GRANTED
    }

    override fun abandonFocus() {
        val request = synchronized(lock) { focusRequest.also { focusRequest = null } } ?: return
        audioManager?.let { am -> runCatching { am.abandonAudioFocusRequest(request) } }
    }

    override fun enterCommunicationMode() {
        val am = audioManager ?: return
        synchronized(lock) { previousMode = am.mode }
        am.mode = AudioManager.MODE_IN_COMMUNICATION
    }

    override fun restoreCommunicationMode() {
        val previous = synchronized(lock) { previousMode.also { previousMode = null } } ?: return
        val am = audioManager ?: return
        // A call that took over has set its own mode; never clobber it.
        if (am.mode == AudioManager.MODE_IN_COMMUNICATION) runCatching { am.mode = previous }
    }

    override suspend fun connectRoute(device: CarAudioDevice, onRouteLost: () -> Unit): Boolean {
        val am = audioManager ?: return false
        val ok = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
            connectCommunicationDevice(am, device, onRouteLost)
        } else {
            connectLegacySco(am, onRouteLost)
        }
        LNLog.i(LogCategory.AUDIO, TAG, "car route to type=${device.type} established=$ok")
        return ok
    }

    @RequiresApi(Build.VERSION_CODES.S)
    private suspend fun connectCommunicationDevice(
        am: AudioManager,
        device: CarAudioDevice,
        onRouteLost: () -> Unit,
    ): Boolean {
        val target = am.availableCommunicationDevices.firstOrNull { it.id == device.id } ?: return false
        val handshake = RouteHandshake(onRouteLost)
        val listener = AudioManager.OnCommunicationDeviceChangedListener { current ->
            if (current?.id == device.id) handshake.observed() else handshake.routeEnded(failIfUnseen = false)
        }
        am.addOnCommunicationDeviceChangedListener(ContextCompat.getMainExecutor(context), listener)
        synchronized(lock) {
            communicationListener = listener
            communicationDeviceRequested = true
        }
        if (!am.setCommunicationDevice(target)) return false
        if (am.communicationDevice?.id == device.id) handshake.observed()
        if (!handshake.awaitEstablished(ROUTE_TIMEOUT_MS)) return false
        registerRemovalCallback(am, onRouteLost) { it.id == device.id }
        return am.communicationDevice?.id == device.id
    }

    @Suppress("DEPRECATION")
    private suspend fun connectLegacySco(am: AudioManager, onRouteLost: () -> Unit): Boolean {
        val handshake = RouteHandshake(onRouteLost)
        val receiver = object : BroadcastReceiver() {
            private var sawConnecting = false
            override fun onReceive(c: Context, intent: Intent) {
                if (isInitialStickyBroadcast) return
                when (intent.getIntExtra(AudioManager.EXTRA_SCO_AUDIO_STATE, AudioManager.SCO_AUDIO_STATE_ERROR)) {
                    AudioManager.SCO_AUDIO_STATE_CONNECTING -> sawConnecting = true
                    AudioManager.SCO_AUDIO_STATE_CONNECTED -> handshake.observed()
                    AudioManager.SCO_AUDIO_STATE_DISCONNECTED -> handshake.routeEnded(failIfUnseen = sawConnecting)
                    else -> handshake.routeEnded(failIfUnseen = true)
                }
            }
        }
        ContextCompat.registerReceiver(
            context,
            receiver,
            IntentFilter(AudioManager.ACTION_SCO_AUDIO_STATE_UPDATED),
            null,
            mainHandler,
            ContextCompat.RECEIVER_NOT_EXPORTED,
        )
        synchronized(lock) {
            scoReceiver = receiver
            scoStarted = true
        }
        am.startBluetoothSco()
        if (!handshake.awaitEstablished(SCO_TIMEOUT_MS)) return false
        synchronized(lock) { previousScoOn = am.isBluetoothScoOn }
        am.isBluetoothScoOn = true
        registerRemovalCallback(am, onRouteLost) { it.type == AudioDeviceInfo.TYPE_BLUETOOTH_SCO }
        return true
    }

    private fun registerRemovalCallback(
        am: AudioManager,
        onRouteLost: () -> Unit,
        matches: (AudioDeviceInfo) -> Boolean,
    ) {
        val callback = object : AudioDeviceCallback() {
            override fun onAudioDevicesRemoved(removedDevices: Array<out AudioDeviceInfo>?) {
                if (removedDevices?.any(matches) == true) onRouteLost()
            }
        }
        synchronized(lock) { deviceCallback = callback }
        am.registerAudioDeviceCallback(callback, mainHandler)
    }

    @Suppress("DEPRECATION")
    override fun disconnectRoute() {
        val am = audioManager ?: return
        val callback: AudioDeviceCallback?
        val listener: Any?
        val requested: Boolean
        val receiver: BroadcastReceiver?
        val started: Boolean
        val scoOn: Boolean?
        synchronized(lock) {
            callback = deviceCallback
            deviceCallback = null
            listener = communicationListener
            communicationListener = null
            requested = communicationDeviceRequested
            communicationDeviceRequested = false
            receiver = scoReceiver
            scoReceiver = null
            started = scoStarted
            scoStarted = false
            scoOn = previousScoOn
            previousScoOn = null
        }
        callback?.let { runCatching { am.unregisterAudioDeviceCallback(it) } }
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
            listener?.let { removeCommunicationListener(am, it) }
            // Per-client request in AudioService: clears only this app's route.
            if (requested) runCatching { am.clearCommunicationDevice() }
        }
        receiver?.let { runCatching { context.unregisterReceiver(it) } }
        if (scoOn != null && am.mode != AudioManager.MODE_IN_CALL) runCatching { am.isBluetoothScoOn = scoOn }
        if (started) runCatching { am.stopBluetoothSco() }
    }

    @RequiresApi(Build.VERSION_CODES.S)
    private fun removeCommunicationListener(am: AudioManager, listener: Any) {
        (listener as? AudioManager.OnCommunicationDeviceChangedListener)?.let {
            runCatching { am.removeOnCommunicationDeviceChangedListener(it) }
        }
    }

    private companion object {
        const val TAG = "CarAudioPlatform"
        const val ROUTE_TIMEOUT_MS = 5_000L
        const val SCO_TIMEOUT_MS = 8_000L
    }
}

/** App-wide car-audio owner shared by the coordinator for all providers. */
@Singleton
class CarAudioSessionManager @Inject constructor(
    @ApplicationContext context: Context,
    private val preferences: CarAudioPreferences,
) {
    private val core = CarAudioSessionCore(AndroidCarAudioPlatform(context), CarAudioRouteOwner.process)
    private val starter = CarAudioSessionStarter(
        core,
        CarAudioForegroundBroker.process,
        AndroidCarAudioForegroundLauncher(context),
    )

    /** The user explicitly opted in on this phone. */
    val isSelected: Boolean get() = preferences.isEnabled

    /** UI pre-flight; does not take focus or route. Null when ready. */
    fun readiness(): CarAudioFailure? = core.readiness(requireForeground = false)

    /**
     * Full car start for an explicit start from the visible app: the session
     * foreground service is verified foreground (bounded wait) before focus
     * and route are taken. [onEnd] fires once for notification End, task
     * removal or service death; [onInterrupted] once for focus/route loss.
     */
    suspend fun startSession(
        onEnd: () -> Unit,
        onInterrupted: (CarAudioFailure) -> Unit,
    ): CarAudioSessionHandle = starter.start(onEnd, onInterrupted)

    /** Bare acquire without the session service; requires the visible app. */
    suspend fun acquire(onInterrupted: (CarAudioFailure) -> Unit): CarAudioLease =
        core.acquire(onInterrupted = onInterrupted)
}
