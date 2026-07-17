package ninja.jeremy.liveninja.wake

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.content.pm.ServiceInfo
import android.os.IBinder
import android.os.PowerManager
import android.util.Log
import androidx.core.app.NotificationCompat
import androidx.core.app.ServiceCompat
import androidx.core.content.ContextCompat
import dagger.hilt.android.AndroidEntryPoint
import javax.inject.Inject
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
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
import ninja.jeremy.liveninja.audio.WakeWordEngine

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

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default)
    private val constrained = MutableStateFlow(false)
    private val engineFailure = MutableStateFlow<String?>(null)
    private var controllerStarted = false
    private var thermalListener: PowerManager.OnThermalStatusChangedListener? = null
    private var powerSaveReceiver: BroadcastReceiver? = null

    private val powerManager by lazy { getSystemService(Context.POWER_SERVICE) as PowerManager }

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
            ACTION_STOP -> {
                prefs.serviceEnabled = false
                stopEngineBlocking()
                stopSelf()
                return START_NOT_STICKY
            }
        }
        prefs.serviceEnabled = true
        ServiceCompat.startForeground(
            this,
            NOTIFICATION_ID,
            buildNotification(),
            ServiceInfo.FOREGROUND_SERVICE_TYPE_MICROPHONE,
        )
        if (!controllerStarted) {
            controllerStarted = true
            startController()
        }
        return START_STICKY
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
        // Fan detections out app-wide.
        scope.launch {
            activeEngine().detections.collect { wakeEvents.emit(it) }
        }
        // Best-effort model sync at service start (no-ops when signed out / offline).
        scope.launch {
            when (val r = modelManager.sync(prefs.wakeWordId, prefs.wakeEngine)) {
                is ModelSyncResult.Active -> Log.i(TAG, "model sync: active ${r.ref.wakeWordId}")
                else -> Log.i(TAG, "model sync: $r (packaged/cached model serves)")
            }
        }
        // Run-mode state machine. collectLatest cancels the previous mode's loop on change.
        scope.launch {
            combine(prefs.mutedFlow, constrained) { muted, constrainedNow ->
                when {
                    muted -> Mode.MUTED
                    constrainedNow -> Mode.DUTY_CYCLE
                    else -> Mode.CONTINUOUS
                }
            }.collectLatest { mode ->
                Log.i(TAG, "run mode -> $mode")
                when (mode) {
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
                                Log.w(TAG, "engine capture died; restarting")
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

    private enum class Mode { MUTED, CONTINUOUS, DUTY_CYCLE }

    private suspend fun startEngineReportingFailure(): Boolean =
        try {
            activeEngine().start()
            engineFailure.value = null
            true
        } catch (e: Exception) {
            Log.w(TAG, "engine start failed: ${e.message}")
            engineFailure.value = e.message ?: "microphone unavailable"
            false
        }

    private suspend fun stopEngine() {
        runCatching { activeEngine().stop() }
            .onFailure { Log.w(TAG, "engine stop failed", it) }
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
    }

    private fun buildNotification(): Notification {
        val muted = prefs.muted
        val failure = engineFailure.value
        val phrase = modelManager.headModel.value.wakeWordId.replace('-', ' ')
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

        val contentIntent = PendingIntent.getActivity(
            this,
            0,
            Intent(this, MainActivity::class.java),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
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
        const val NOTIFICATION_ID = 1001

        const val ACTION_START = "ninja.jeremy.liveninja.wake.START"
        const val ACTION_MUTE = "ninja.jeremy.liveninja.wake.MUTE"
        const val ACTION_UNMUTE = "ninja.jeremy.liveninja.wake.UNMUTE"
        const val ACTION_STOP = "ninja.jeremy.liveninja.wake.STOP"

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
