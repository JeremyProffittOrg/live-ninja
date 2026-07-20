package ninja.jeremy.liveninja.wake

import android.Manifest
import android.app.KeyguardManager
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.content.pm.PackageManager
import android.content.pm.ServiceInfo
import android.os.Build
import android.os.IBinder
import android.os.PowerManager
import androidx.core.app.NotificationCompat
import androidx.core.app.ServiceCompat
import androidx.core.content.ContextCompat
import dagger.hilt.android.AndroidEntryPoint
import javax.inject.Inject
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.awaitCancellation
import kotlinx.coroutines.cancel
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.collectLatest
import kotlinx.coroutines.launch
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeoutOrNull
import ninja.jeremy.liveninja.MainActivity
import ninja.jeremy.liveninja.R
import ninja.jeremy.liveninja.assistant.AssistSource
import ninja.jeremy.liveninja.assistant.LiveNinjaSession
import ninja.jeremy.liveninja.audio.WakeWordEngine
import ninja.jeremy.liveninja.log.LNLog
import ninja.jeremy.liveninja.log.LogCategory
import ninja.jeremy.liveninja.realtime.SessionOrchestrator
import ninja.jeremy.liveninja.ui.state.SettingsStore

/**
 * Always-listening wake-word foreground service (plan.md M4, Android §3.2/§3.3).
 *
 * - `foregroundServiceType="microphone"` with a persistent LOW-importance notification whose
 *   primary action is Mute/Resume (mic stays off while muted, service stays resident).
 * - Battery strategy: the engine's [EnergyVad] gates all ONNX inference; **no wakelock is ever
 *   taken**; under Battery Saver or SEVERE+ thermal throttling (both observed live via
 *   [PowerManager]) listening drops to a duty cycle ([DUTY_LISTEN_MS] on / [DUTY_PAUSE_MS] off)
 *   instead of continuous capture.
 * - Restarts after reboot via [WakeBootReceiver] (subject to the Android 15 rule that a
 *   microphone-type FGS cannot launch straight from BOOT_COMPLETED — the receiver then posts a
 *   tap-to-resume notification instead).
 * - Detections fan out through [WakeEvents] for the realtime layer to open a session on wake.
 *
 * Engine selection is a Hilt multibinding map keyed by `settings.schema.json`'s `wakeEngine`
 * enum values — the optional Porcupine build contributes its own entry without this file
 * changing (see `src/porcupine/`).
 */
@AndroidEntryPoint
class WakeWordService : Service() {

    @Inject lateinit var engines: Map<String, @JvmSuppressWildcards WakeWordEngine>
    @Inject lateinit var prefs: WakePreferences
    @Inject lateinit var wakeEvents: WakeEvents
    @Inject lateinit var modelManager: ModelManager

    /** All session logic lives here; the service only reflects [SessionOrchestrator.sessionActive]. */
    @Inject lateinit var sessionOrchestrator: SessionOrchestrator

    /** Voice-session lifecycle toggles (lockedSessions / wakeScreenOnWake). */
    @Inject lateinit var settingsStore: SettingsStore

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default)
    private val constrained = MutableStateFlow(false)
    private val engineFailure = MutableStateFlow<String?>(null)
    private var controllerStarted = false
    private var thermalListener: PowerManager.OnThermalStatusChangedListener? = null
    private var powerSaveReceiver: BroadcastReceiver? = null

    private val powerManager by lazy { getSystemService(Context.POWER_SERVICE) as PowerManager }
    private val keyguardManager by lazy { getSystemService(Context.KEYGUARD_SERVICE) as KeyguardManager }

    private fun activeEngine(): WakeWordEngine =
        engines[prefs.wakeEngine] ?: engines.getValue(WakePreferences.ENGINE_OPENWAKEWORD)

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onCreate() {
        super.onCreate()
        createChannel()
        watchPowerState()
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        when (intent?.action) {
            ACTION_MUTE -> prefs.muted = true
            ACTION_UNMUTE -> prefs.muted = false
            ACTION_END_SESSION -> scope.launch { runCatching { sessionOrchestrator.stop() } }
            ACTION_STOP -> {
                prefs.serviceEnabled = false
                scope.launch { runCatching { sessionOrchestrator.stop() } }
                stopEngineBlocking()
                stopSelf()
                return START_NOT_STICKY
            }
        }

        // Crash-guard A2: never call startForeground(type=MICROPHONE) without the
        // mic grant (SecurityException on targetSdk 34+), and never crash-loop on
        // a background-start refusal. Degrade to a tap-to-resume notification.
        if (ContextCompat.checkSelfPermission(this, Manifest.permission.RECORD_AUDIO)
            != PackageManager.PERMISSION_GRANTED
        ) {
            LNLog.w(LogCategory.WAKE, TAG, "RECORD_AUDIO not granted — degrading to tap-to-resume")
            postTapToResumeNotification()
            stopSelf()
            return START_NOT_STICKY
        }

        prefs.serviceEnabled = true
        if (!startForegroundGuarded(ServiceInfo.FOREGROUND_SERVICE_TYPE_MICROPHONE)) {
            postTapToResumeNotification()
            stopSelf()
            return START_NOT_STICKY
        }
        if (!controllerStarted) {
            controllerStarted = true
            startController()
        }
        return START_STICKY
    }

    /**
     * [ServiceCompat.startForeground] guarded against the two throws that turn a
     * `START_STICKY` mic FGS into a relaunch loop: [SecurityException] (mic grant
     * revoked) and `ForegroundServiceStartNotAllowedException` (a subclass of
     * [IllegalStateException], API 31+ — caught via the superclass so the catch
     * clause resolves on minSdk 29 too).
     */
    private fun startForegroundGuarded(type: Int): Boolean =
        try {
            ServiceCompat.startForeground(this, NOTIFICATION_ID, buildNotification(), type)
            true
        } catch (e: SecurityException) {
            LNLog.e(LogCategory.WAKE, TAG, "startForeground denied (mic grant)", e)
            false
        } catch (e: IllegalStateException) {
            LNLog.e(LogCategory.WAKE, TAG, "startForeground not allowed from background", e)
            false
        }

    override fun onDestroy() {
        stopEngineBlocking()
        powerSaveReceiver?.let { unregisterReceiver(it) }
        thermalListener?.let { powerManager.removeThermalStatusListener(it) }
        scope.cancel()
        super.onDestroy()
    }

    // ---- controller: mute + power state -> engine run mode ----

    private fun startController() {
        // Fan detections out app-wide, and wake the screen per the toggle.
        scope.launch {
            activeEngine().detections.collect {
                wakeEvents.emit(it)
                maybeWakeScreenOnDetection()
            }
        }
        // Best-effort model sync at service start (no-ops when signed out / offline).
        scope.launch {
            when (val r = modelManager.sync(prefs.wakeWordId, prefs.wakeEngine)) {
                is ModelSyncResult.Active -> LNLog.i(LogCategory.WAKE, TAG, "model sync: active ${r.ref.wakeWordId}")
                else -> LNLog.i(LogCategory.WAKE, TAG, "model sync: $r (packaged/cached model serves)")
            }
        }
        // Run-mode state machine. collectLatest cancels the previous mode's loop on change.
        scope.launch {
            combine(
                prefs.mutedFlow,
                constrained,
                sessionOrchestrator.sessionActive,
            ) { muted, constrainedNow, sessionActive ->
                when {
                    // A live realtime session owns the mic (WebRTC/Gemini capture);
                    // the wake engine must pause and resumes the instant it ends.
                    sessionActive -> Mode.SESSION
                    muted -> Mode.MUTED
                    constrainedNow -> Mode.DUTY_CYCLE
                    else -> Mode.CONTINUOUS
                }
            }.collectLatest { mode ->
                LNLog.i(LogCategory.WAKE, TAG, "run mode -> $mode")
                when (mode) {
                    Mode.SESSION -> {
                        // Stop wake capture and expand the FGS type so the session's
                        // mic + playback are covered on the already-running FGS.
                        stopEngine()
                        promoteForegroundForSession()
                        updateNotification()
                        // Hold until sessionActive flips — collectLatest then cancels
                        // this branch and re-enters CONTINUOUS, which starts the engine
                        // immediately (bypassing the 60 s post-loss retry).
                        awaitCancellation()
                    }
                    Mode.MUTED -> {
                        stopEngine()
                        updateNotification()
                    }
                    Mode.CONTINUOUS -> {
                        updateNotification()
                        while (true) {
                            if (startEngineReportingFailure()) {
                                updateNotification()
                                // Supervise: if the capture loop dies (mic stolen by another
                                // app, read error), clean up and restart it.
                                while (activeEngine().isRunning) delay(SUPERVISE_POLL_MS)
                                LNLog.w(LogCategory.WAKE, TAG, "engine capture died; restarting")
                                stopEngine()
                            } else {
                                updateNotification()
                                delay(ENGINE_RETRY_MS) // mic blocked/busy — retry
                            }
                        }
                    }
                    Mode.DUTY_CYCLE -> {
                        updateNotification()
                        while (true) {
                            if (startEngineReportingFailure()) {
                                updateNotification()
                                delay(DUTY_LISTEN_MS)
                                stopEngine()
                                delay(DUTY_PAUSE_MS)
                            } else {
                                updateNotification()
                                delay(ENGINE_RETRY_MS)
                            }
                        }
                    }
                }
            }
        }
    }

    private enum class Mode { SESSION, MUTED, CONTINUOUS, DUTY_CYCLE }

    /**
     * Re-assert foreground with `microphone|mediaPlayback` when a session begins
     * (02-voice §B1). Re-calling startForeground on the already-running FGS with
     * expanded types is legal even screen-off; on OEM refusal we stay under the
     * mic-only type and continue the session.
     */
    private fun promoteForegroundForSession() {
        val type = ServiceInfo.FOREGROUND_SERVICE_TYPE_MICROPHONE or
            ServiceInfo.FOREGROUND_SERVICE_TYPE_MEDIA_PLAYBACK
        if (!startForegroundGuarded(type)) {
            LNLog.w(LogCategory.WAKE, TAG, "session FGS promote refused; continuing under mic-only type")
        }
    }

    /**
     * Wake-screen path on detection (01-platform §B-ii). Assistant-session
     * showSession() requires the held VoiceInteractionService instance, so this
     * uses the universal full-screen-intent path: launch MainActivity with
     * ACTION_ASSIST (which shows over the keyguard). On API 34+ a full-screen
     * intent is used only when permitted; otherwise a high-priority heads-up
     * notification, with the audio-only session still proceeding via the
     * orchestrator.
     */
    private fun maybeWakeScreenOnDetection() {
        val settings = settingsStore.document.value
        if (!settings.wakeScreenOnWake) return
        val asleepOrLocked = !powerManager.isInteractive || keyguardManager.isKeyguardLocked
        // Respect the locked-session gate: don't wake the screen for a wake the
        // orchestrator will ignore anyway.
        if (!settings.lockedSessions && asleepOrLocked) return

        val activityIntent = Intent(this, MainActivity::class.java).apply {
            action = LiveNinjaSession.ACTION_ASSIST
            putExtra(LiveNinjaSession.EXTRA_SOURCE, AssistSource.WAKE_WORD.name)
            putExtra(LiveNinjaSession.EXTRA_LAUNCHED_WHILE_LOCKED, keyguardManager.isKeyguardLocked)
            addFlags(Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_SINGLE_TOP)
        }
        val pi = PendingIntent.getActivity(
            this,
            3,
            activityIntent,
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        val nm = getSystemService(NotificationManager::class.java)
        val canFsi = Build.VERSION.SDK_INT < Build.VERSION_CODES.UPSIDE_DOWN_CAKE ||
            nm.canUseFullScreenIntent()
        val builder = NotificationCompat.Builder(this, CHANNEL_ID_ALERT)
            .setSmallIcon(R.drawable.ic_launcher_foreground)
            .setContentTitle("Live Ninja")
            .setContentText("Listening…")
            .setContentIntent(pi)
            .setPriority(NotificationCompat.PRIORITY_HIGH)
            .setCategory(NotificationCompat.CATEGORY_CALL)
            .setAutoCancel(true)
        if (canFsi) builder.setFullScreenIntent(pi, true)
        nm.notify(NOTIFICATION_ID_WAKE_SCREEN, builder.build())
    }

    /** Tap-to-resume notification for the degrade paths (no mic grant / FGS refused). */
    private fun postTapToResumeNotification() {
        val intent = Intent(this, MainActivity::class.java).apply {
            putExtra(WakeBootReceiver.EXTRA_START_WAKE_SERVICE, true)
            addFlags(Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_SINGLE_TOP)
        }
        val pi = PendingIntent.getActivity(
            this,
            4,
            intent,
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        val notification = NotificationCompat.Builder(this, CHANNEL_ID)
            .setSmallIcon(R.drawable.ic_launcher_foreground)
            .setContentTitle("Tap to resume Live Ninja")
            .setContentText("Listening was paused. Tap to grant the microphone and resume.")
            .setContentIntent(pi)
            .setAutoCancel(true)
            .setPriority(NotificationCompat.PRIORITY_DEFAULT)
            .build()
        getSystemService(NotificationManager::class.java).notify(NOTIFICATION_ID_WAKE_SCREEN, notification)
    }

    private suspend fun startEngineReportingFailure(): Boolean =
        try {
            activeEngine().start()
            engineFailure.value = null
            true
        } catch (e: Exception) {
            LNLog.w(LogCategory.WAKE, TAG, "engine start failed: ${e.message}")
            engineFailure.value = e.message ?: "microphone unavailable"
            false
        }

    private suspend fun stopEngine() {
        runCatching { activeEngine().stop() }
            .onFailure { LNLog.w(LogCategory.WAKE, TAG, "engine stop failed", it) }
    }

    private fun stopEngineBlocking() {
        runBlocking { withTimeoutOrNull(2_000) { stopEngine() } }
    }

    // ---- power / thermal constraints (battery-saver duty cycle, plan.md §3.3) ----

    private fun watchPowerState() {
        fun recompute(thermalStatus: Int = powerManager.currentThermalStatus) {
            constrained.value =
                powerManager.isPowerSaveMode || thermalStatus >= PowerManager.THERMAL_STATUS_SEVERE
        }
        recompute()
        powerSaveReceiver = object : BroadcastReceiver() {
            override fun onReceive(context: Context, intent: Intent) = recompute()
        }
        registerReceiver(
            powerSaveReceiver,
            IntentFilter(PowerManager.ACTION_POWER_SAVE_MODE_CHANGED),
        )
        thermalListener = PowerManager.OnThermalStatusChangedListener { status ->
            recompute(status)
        }.also { powerManager.addThermalStatusListener(ContextCompat.getMainExecutor(this), it) }
    }

    // ---- notification ----

    private fun createChannel() {
        val manager = getSystemService(NotificationManager::class.java)
        manager.createNotificationChannel(
            NotificationChannel(
                CHANNEL_ID,
                getString(R.string.wake_notification_channel),
                NotificationManager.IMPORTANCE_LOW,
            ).apply {
                description = getString(R.string.wake_notification_channel_description)
                setShowBadge(false)
            },
        )
        // High-importance channel for wake-screen / full-screen-intent alerts.
        manager.createNotificationChannel(
            NotificationChannel(
                CHANNEL_ID_ALERT,
                "Wake alerts",
                NotificationManager.IMPORTANCE_HIGH,
            ).apply {
                description = "Wakes the screen when the wake phrase is heard."
                setShowBadge(false)
            },
        )
    }

    private fun buildNotification(): Notification {
        val sessionLive = sessionOrchestrator.sessionActive.value
        val muted = prefs.muted
        val failure = engineFailure.value
        val phrase = modelManager.headModel.value.wakeWordId.replace('-', ' ')

        val contentIntent = PendingIntent.getActivity(
            this,
            0,
            Intent(this, MainActivity::class.java),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )

        // Session-live: replace the listening notification in place with a
        // "Conversation live — End" surface (single notification, 02-voice §B3).
        if (sessionLive) {
            return NotificationCompat.Builder(this, CHANNEL_ID)
                .setSmallIcon(R.drawable.ic_launcher_foreground)
                .setContentTitle("Conversation live")
                .setContentText("Tap End to close the voice session.")
                .setContentIntent(contentIntent)
                .setOngoing(true)
                .setSilent(true)
                .setPriority(NotificationCompat.PRIORITY_LOW)
                .setCategory(NotificationCompat.CATEGORY_CALL)
                .setForegroundServiceBehavior(NotificationCompat.FOREGROUND_SERVICE_IMMEDIATE)
                .addAction(
                    NotificationCompat.Action(
                        null,
                        "End",
                        servicePendingIntent(ACTION_END_SESSION, 3),
                    ),
                )
                .build()
        }

        val title = when {
            muted -> getString(R.string.wake_notification_muted_title)
            failure != null -> getString(R.string.wake_notification_error_title)
            else -> getString(R.string.wake_notification_listening_title, phrase)
        }
        val text = when {
            muted -> getString(R.string.wake_notification_muted_body)
            failure != null -> getString(R.string.wake_notification_error_body)
            constrained.value -> getString(R.string.wake_notification_duty_body)
            else -> getString(R.string.wake_notification_listening_body)
        }

        val muteAction = if (muted) {
            NotificationCompat.Action(
                null,
                getString(R.string.wake_notification_action_resume),
                servicePendingIntent(ACTION_UNMUTE, 1),
            )
        } else {
            NotificationCompat.Action(
                null,
                getString(R.string.wake_notification_action_mute),
                servicePendingIntent(ACTION_MUTE, 1),
            )
        }

        return NotificationCompat.Builder(this, CHANNEL_ID)
            .setSmallIcon(R.drawable.ic_launcher_foreground)
            .setContentTitle(title)
            .setContentText(text)
            .setContentIntent(contentIntent)
            .setOngoing(true)
            .setSilent(true)
            .setPriority(NotificationCompat.PRIORITY_LOW)
            .setCategory(NotificationCompat.CATEGORY_SERVICE)
            .setForegroundServiceBehavior(NotificationCompat.FOREGROUND_SERVICE_IMMEDIATE)
            .addAction(muteAction)
            .addAction(
                NotificationCompat.Action(
                    null,
                    getString(R.string.wake_notification_action_stop),
                    servicePendingIntent(ACTION_STOP, 2),
                ),
            )
            .build()
    }

    private fun servicePendingIntent(action: String, requestCode: Int): PendingIntent =
        PendingIntent.getService(
            this,
            requestCode,
            Intent(this, WakeWordService::class.java).setAction(action),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )

    private fun updateNotification() {
        getSystemService(NotificationManager::class.java)
            .notify(NOTIFICATION_ID, buildNotification())
    }

    companion object {
        private const val TAG = "WakeWordService"
        const val CHANNEL_ID = "wakeword"
        const val CHANNEL_ID_ALERT = "wakeword_alert"
        const val NOTIFICATION_ID = 1001
        const val NOTIFICATION_ID_WAKE_SCREEN = 1002

        const val ACTION_START = "ninja.jeremy.liveninja.wake.START"
        const val ACTION_MUTE = "ninja.jeremy.liveninja.wake.MUTE"
        const val ACTION_UNMUTE = "ninja.jeremy.liveninja.wake.UNMUTE"
        const val ACTION_STOP = "ninja.jeremy.liveninja.wake.STOP"
        const val ACTION_END_SESSION = "ninja.jeremy.liveninja.wake.END_SESSION"

        /** Battery-saver / thermal duty cycle: 8 s listening, 4 s paused (~33% mic-off). */
        const val DUTY_LISTEN_MS = 8_000L
        const val DUTY_PAUSE_MS = 4_000L

        /** Retry cadence when the mic is blocked (background-start restriction, mic busy). */
        const val ENGINE_RETRY_MS = 60_000L

        /** Poll cadence for detecting a dead capture loop while nominally running. */
        const val SUPERVISE_POLL_MS = 15_000L

        /** Start (or re-assert) the service from a foreground context (UI). */
        fun start(context: Context) {
            ContextCompat.startForegroundService(
                context,
                Intent(context, WakeWordService::class.java).setAction(ACTION_START),
            )
        }

        /** Stop listening entirely and clear the enabled flag. */
        fun stop(context: Context) {
            context.startService(
                Intent(context, WakeWordService::class.java).setAction(ACTION_STOP),
            )
        }
    }
}
