package ninja.jeremy.liveninja.realtime

import android.Manifest
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.content.pm.ServiceInfo
import android.net.Uri
import android.os.Handler
import android.os.IBinder
import android.os.Looper
import androidx.core.app.NotificationCompat
import androidx.core.app.ServiceCompat
import androidx.core.content.ContextCompat
import java.util.concurrent.CopyOnWriteArrayList
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.withTimeoutOrNull
import ninja.jeremy.liveninja.MainActivity
import ninja.jeremy.liveninja.R
import ninja.jeremy.liveninja.log.LNLog
import ninja.jeremy.liveninja.log.LogCategory

/*
 * Session-owned foreground lifecycle for opt-in phone car audio.
 *
 * A car conversation started from the visible app first runs the short-lived
 * [CarAudioForegroundService] (microphone|mediaPlayback, ongoing notification
 * with End) and waits, bounded, until it is really foreground. Only then is
 * car focus/route acquired and only then may mic/network work begin. Every
 * service command and callback carries the session token; a stale token can
 * never stop, end or mute a replacement session. The service is never sticky,
 * never started at boot or on a Bluetooth connection.
 */

/** One foreground-service window for one car session. */
class CarAudioForegroundTicket internal constructor(
    val token: Long,
    internal val onEnd: () -> Unit,
) {
    internal val ready = CompletableDeferred<Boolean>()

    @Volatile
    internal var foreground = false

    @Volatile
    var endRequested = false
        internal set
}

/** Generation broker between the coordinator and the service instance. Thread-safe. */
class CarAudioForegroundBroker {
    private val lock = Any()
    private var nextToken = 0L
    private var current: CarAudioForegroundTicket? = null
    private val stopListeners = CopyOnWriteArrayList<(Long) -> Unit>()

    /** Opens a new window; any previous one is superseded and asked to stop. */
    fun open(onEnd: () -> Unit): CarAudioForegroundTicket {
        val previous: CarAudioForegroundTicket?
        val ticket: CarAudioForegroundTicket
        synchronized(lock) {
            previous = current
            ticket = CarAudioForegroundTicket(++nextToken, onEnd)
            current = ticket
        }
        previous?.let {
            it.ready.complete(false)
            notifyStop(it.token)
        }
        return ticket
    }

    fun isCurrent(token: Long): Boolean = synchronized(lock) { current?.token == token }

    /** The verified session-owned service is foreground for this live token. */
    fun isForeground(token: Long): Boolean = synchronized(lock) {
        val t = current
        t != null && t.token == token && t.foreground && !t.endRequested
    }

    internal fun markForeground(token: Long): Boolean {
        val ticket = synchronized(lock) {
            val t = current?.takeIf { it.token == token && !it.endRequested } ?: return false
            t.foreground = true
            t
        }
        ticket.ready.complete(true)
        return true
    }

    internal fun markDenied(token: Long) {
        val ticket = synchronized(lock) { current?.takeIf { it.token == token } } ?: return
        ticket.ready.complete(false)
    }

    /** End from the notification / task removal / service death. Fires the handler at most once. */
    fun requestEnd(token: Long): Boolean {
        val ticket = synchronized(lock) {
            val t = current?.takeIf { it.token == token && !it.endRequested } ?: return false
            t.endRequested = true
            t
        }
        ticket.ready.complete(false)
        runCatching { ticket.onEnd() }
        return true
    }

    /** Session finished (after transport shutdown and lease release). Idempotent. */
    fun close(token: Long): Boolean {
        val ticket = synchronized(lock) {
            val t = current?.takeIf { it.token == token } ?: return false
            current = null
            t
        }
        ticket.ready.complete(false)
        notifyStop(token)
        return true
    }

    suspend fun awaitForeground(ticket: CarAudioForegroundTicket, timeoutMs: Long): Boolean {
        val ok = withTimeoutOrNull(timeoutMs) { ticket.ready.await() } ?: false
        return ok && isForeground(ticket.token)
    }

    fun addStopListener(listener: (Long) -> Unit) {
        stopListeners += listener
    }

    fun removeStopListener(listener: (Long) -> Unit) {
        stopListeners -= listener
    }

    private fun notifyStop(token: Long) {
        stopListeners.forEach { runCatching { it(token) } }
    }

    companion object {
        val process = CarAudioForegroundBroker()
    }
}

/** Starts the session service for a token. False when Android refused the start. */
fun interface CarAudioForegroundLauncher {
    fun start(token: Long): Boolean
}

/** Launch + bounded wait. Null when the service is verified foreground for [ticket]. */
internal suspend fun awaitCarAudioForeground(
    broker: CarAudioForegroundBroker,
    ticket: CarAudioForegroundTicket,
    launcher: CarAudioForegroundLauncher,
    timeoutMs: Long,
): CarAudioFailure? {
    if (!broker.isCurrent(ticket.token)) {
        return if (ticket.endRequested) CarAudioFailure.ENDED_BY_USER else CarAudioFailure.SUPERSEDED
    }
    if (!launcher.start(ticket.token)) {
        broker.markDenied(ticket.token)
        return CarAudioFailure.FOREGROUND_DENIED
    }
    if (broker.awaitForeground(ticket, timeoutMs)) return null
    return when {
        ticket.endRequested -> CarAudioFailure.ENDED_BY_USER
        !broker.isCurrent(ticket.token) -> CarAudioFailure.SUPERSEDED
        else -> CarAudioFailure.FOREGROUND_DENIED
    }
}

/** A started car session: foreground window + car lease. [release] is idempotent. */
class CarAudioSessionHandle internal constructor(
    val ticket: CarAudioForegroundTicket,
    private val lease: CarAudioLease,
    private val broker: CarAudioForegroundBroker,
) {
    val device: CarAudioDevice get() = lease.device
    val interruption: CarAudioFailure? get() = lease.interruption

    /** Call only after the transport is shut down: route/focus first, then the service. */
    fun release() {
        try {
            lease.release()
        } finally {
            broker.close(ticket.token)
        }
    }
}

/**
 * Ordered car-session start: visible-app readiness, session service verified
 * foreground, then focus/route. Any failure or cancellation releases what was
 * taken synchronously (no suspension in cleanup) and stops the service.
 */
class CarAudioSessionStarter(
    private val core: CarAudioSessionCore,
    private val broker: CarAudioForegroundBroker,
    private val launcher: CarAudioForegroundLauncher,
    private val readyTimeoutMs: Long = FOREGROUND_READY_TIMEOUT_MS,
) {
    suspend fun start(
        onEnd: () -> Unit,
        onInterrupted: (CarAudioFailure) -> Unit,
    ): CarAudioSessionHandle {
        // Never start the service or touch car audio from the background.
        core.readiness(requireForeground = true)?.let { throw CarAudioException(it) }
        val ticket = broker.open(onEnd)
        var lease: CarAudioLease? = null
        var handle: CarAudioSessionHandle? = null
        try {
            awaitCarAudioForeground(broker, ticket, launcher, readyTimeoutMs)?.let { throw CarAudioException(it) }
            lease = try {
                core.acquire({ broker.isForeground(ticket.token) }, onInterrupted)
            } catch (e: CarAudioException) {
                if (ticket.endRequested) throw CarAudioException(CarAudioFailure.ENDED_BY_USER)
                throw e
            }
            when {
                ticket.endRequested -> throw CarAudioException(CarAudioFailure.ENDED_BY_USER)
                !broker.isCurrent(ticket.token) -> throw CarAudioException(CarAudioFailure.SUPERSEDED)
                !broker.isForeground(ticket.token) -> throw CarAudioException(CarAudioFailure.FOREGROUND_DENIED)
            }
            handle = CarAudioSessionHandle(ticket, lease, broker)
            return handle
        } finally {
            if (handle == null) {
                try {
                    lease?.release()
                } finally {
                    broker.close(ticket.token)
                }
            }
        }
    }

    companion object {
        const val FOREGROUND_READY_TIMEOUT_MS = 5_000L
    }
}

/** Identity gate: [withCurrent] runs only while [expected] is still the current value. */
internal class SessionIdentityGuard<T : Any> {
    private val lock = Any()

    @Volatile
    var current: T? = null
        private set

    fun set(value: T?) {
        synchronized(lock) { current = value }
    }

    fun <R> withCurrent(expected: T, block: () -> R): R? = synchronized(lock) {
        if (current === expected) block() else null
    }
}

/**
 * Pure command logic of [CarAudioForegroundService]; all calls on the main
 * thread. A stale token never stops, ends or demotes the live session.
 */
internal class CarAudioServiceController(
    private val broker: CarAudioForegroundBroker,
    private val actions: Actions,
) {
    interface Actions {
        fun hasMicPermission(): Boolean
        /** startForeground(microphone|mediaPlayback); false on refusal. */
        fun enterForeground(token: Long): Boolean
        /** Best-effort startForeground so a refused/stale foreground start can stop cleanly. */
        fun satisfyForegroundContract()
        fun leaveForeground()
        fun stopSelf(startId: Int)
    }

    var ownedToken: Long? = null
        private set
    private var lastStartId = 0

    fun onStartRequest(token: Long?, startId: Int) {
        lastStartId = startId
        if (token == null || !broker.isCurrent(token)) {
            finishUnowned(contract = true)
            return
        }
        if (ownedToken == token) return
        if (!actions.hasMicPermission() || !actions.enterForeground(token)) {
            broker.markDenied(token)
            finishUnowned(contract = true)
            return
        }
        ownedToken = token
        if (!broker.markForeground(token)) stopOwned()
    }

    fun onEndRequest(token: Long?, startId: Int) {
        lastStartId = startId
        if (token != null) broker.requestEnd(token)
        finishUnowned(contract = false)
    }

    fun onUnknownCommand(startId: Int) {
        lastStartId = startId
        finishUnowned(contract = false)
    }

    /** Broker closed [token] (posted to the main thread). */
    fun onStopRequested(token: Long) {
        if (ownedToken == token) stopOwned()
    }

    fun onTaskRemoved() {
        ownedToken?.let { broker.requestEnd(it) }
    }

    fun onDestroy() {
        val owned = ownedToken
        ownedToken = null
        if (owned != null && broker.isCurrent(owned)) broker.requestEnd(owned)
    }

    private fun stopOwned() {
        ownedToken = null
        actions.leaveForeground()
        actions.stopSelf(lastStartId)
    }

    private fun finishUnowned(contract: Boolean) {
        val owned = ownedToken
        // Already foreground for the live session: never stop it for a stale command.
        if (owned != null && broker.isCurrent(owned)) return
        ownedToken = null
        if (contract) actions.satisfyForegroundContract()
        actions.leaveForeground()
        actions.stopSelf(lastStartId)
    }
}

/*
 * Notification End identity. PendingIntent identity (Intent.filterEquals)
 * ignores extras, so a token carried only in an extra let a newer session's
 * FLAG_UPDATE_CURRENT overwrite an older notification's End to end the NEW
 * session. The token is therefore part of the intent's data URI: every
 * session's End is a distinct immutable PendingIntent, delivered only to this
 * explicit, non-exported service (no intent filter, no external URL handler).
 */
private const val END_URI_PREFIX = "liveninja-caraudio://session/"
private const val END_URI_SUFFIX = "/end"

internal fun carAudioEndDataUri(token: Long): String = "$END_URI_PREFIX$token$END_URI_SUFFIX"

internal fun carAudioTokenFromEndDataUri(uri: String?): Long? {
    if (uri == null || uri.length <= END_URI_PREFIX.length + END_URI_SUFFIX.length) return null
    if (!uri.startsWith(END_URI_PREFIX) || !uri.endsWith(END_URI_SUFFIX)) return null
    return uri.substring(END_URI_PREFIX.length, uri.length - END_URI_SUFFIX.length)
        .toLongOrNull()
        ?.takeIf { it > 0 }
}

/** The End token: from the data URI, and the extra (when present) must agree. */
internal fun resolveCarAudioEndToken(dataUri: String?, extraToken: Long?): Long? {
    val fromData = carAudioTokenFromEndDataUri(dataUri) ?: return null
    if (extraToken != null && extraToken != fromData) return null
    return fromData
}

/** Android launcher; refusal (background start, missing eligibility) is reported, not thrown. */
internal class AndroidCarAudioForegroundLauncher(private val context: Context) : CarAudioForegroundLauncher {
    override fun start(token: Long): Boolean =
        try {
            ContextCompat.startForegroundService(context, CarAudioForegroundService.startIntent(context, token))
            true
        } catch (e: IllegalStateException) {
            LNLog.w(LogCategory.AUDIO, "CarAudioFgsLauncher", "car audio service start refused", e)
            false
        } catch (e: SecurityException) {
            LNLog.w(LogCategory.AUDIO, "CarAudioFgsLauncher", "car audio service start denied", e)
            false
        }
}

/** Short-lived, non-exported session service for a car-audio conversation. */
class CarAudioForegroundService : Service() {
    private val broker = CarAudioForegroundBroker.process
    private val mainHandler = Handler(Looper.getMainLooper())
    private lateinit var controller: CarAudioServiceController
    private val stopListener: (Long) -> Unit = { token ->
        mainHandler.post { if (::controller.isInitialized) controller.onStopRequested(token) }
    }

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onCreate() {
        super.onCreate()
        createChannel()
        controller = CarAudioServiceController(broker, AndroidActions())
        broker.addStopListener(stopListener)
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        val token = intent?.getLongExtra(EXTRA_TOKEN, NO_TOKEN)?.takeIf { it > 0 }
        when (intent?.action) {
            ACTION_START -> controller.onStartRequest(token, startId)
            ACTION_END -> controller.onEndRequest(resolveCarAudioEndToken(intent?.dataString, token), startId)
            else -> controller.onUnknownCommand(startId)
        }
        return START_NOT_STICKY
    }

    override fun onTaskRemoved(rootIntent: Intent?) {
        controller.onTaskRemoved()
        super.onTaskRemoved(rootIntent)
    }

    override fun onDestroy() {
        broker.removeStopListener(stopListener)
        mainHandler.removeCallbacksAndMessages(null)
        controller.onDestroy()
        super.onDestroy()
    }

    private inner class AndroidActions : CarAudioServiceController.Actions {
        private val service get() = this@CarAudioForegroundService

        override fun hasMicPermission(): Boolean =
            ContextCompat.checkSelfPermission(service, Manifest.permission.RECORD_AUDIO) ==
                PackageManager.PERMISSION_GRANTED

        override fun enterForeground(token: Long): Boolean =
            try {
                ServiceCompat.startForeground(service, NOTIFICATION_ID, buildNotification(token), SESSION_TYPES)
                true
            } catch (e: SecurityException) {
                LNLog.e(LogCategory.AUDIO, TAG, "car audio foreground denied", e)
                false
            } catch (e: IllegalStateException) {
                LNLog.e(LogCategory.AUDIO, TAG, "car audio foreground not allowed", e)
                false
            }

        override fun satisfyForegroundContract() {
            try {
                ServiceCompat.startForeground(
                    service,
                    NOTIFICATION_ID,
                    buildEndingNotification(),
                    ServiceInfo.FOREGROUND_SERVICE_TYPE_MEDIA_PLAYBACK,
                )
            } catch (e: RuntimeException) {
                LNLog.w(LogCategory.AUDIO, TAG, "car audio stop-path foreground refused", e)
            }
        }

        override fun leaveForeground() {
            runCatching { ServiceCompat.stopForeground(service, ServiceCompat.STOP_FOREGROUND_REMOVE) }
        }

        override fun stopSelf(startId: Int) {
            service.stopSelf(startId)
        }
    }

    private fun createChannel() {
        getSystemService(NotificationManager::class.java).createNotificationChannel(
            NotificationChannel(
                CHANNEL_ID,
                getString(R.string.car_audio_notification_channel),
                NotificationManager.IMPORTANCE_LOW,
            ).apply {
                description = getString(R.string.car_audio_notification_channel_desc)
                setShowBadge(false)
            },
        )
    }

    private fun contentIntent(): PendingIntent =
        PendingIntent.getActivity(
            this,
            REQUEST_OPEN,
            Intent(this, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )

    private fun buildNotification(token: Long): Notification {
        // Unique identity per token (data URI); see carAudioEndDataUri.
        val end = PendingIntent.getService(
            this,
            REQUEST_END,
            endIntent(this, token),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        // Posted before focus/route are acquired: neutral wording, never
        // claiming the car route is already established.
        return NotificationCompat.Builder(this, CHANNEL_ID)
            .setSmallIcon(R.drawable.ic_launcher_foreground)
            .setContentTitle(getString(R.string.car_audio_notification_title))
            .setContentText(getString(R.string.car_audio_notification_text))
            .setContentIntent(contentIntent())
            .setOngoing(true)
            .setSilent(true)
            .setPriority(NotificationCompat.PRIORITY_LOW)
            .setCategory(NotificationCompat.CATEGORY_SERVICE)
            .setForegroundServiceBehavior(NotificationCompat.FOREGROUND_SERVICE_IMMEDIATE)
            .addAction(NotificationCompat.Action(null, getString(R.string.car_audio_notification_end), end))
            .build()
    }

    private fun buildEndingNotification(): Notification =
        NotificationCompat.Builder(this, CHANNEL_ID)
            .setSmallIcon(R.drawable.ic_launcher_foreground)
            .setContentTitle(getString(R.string.car_audio_notification_title))
            .setContentText(getString(R.string.car_audio_notification_ending))
            .setContentIntent(contentIntent())
            .setSilent(true)
            .setPriority(NotificationCompat.PRIORITY_LOW)
            .build()

    companion object {
        private const val TAG = "CarAudioFgs"
        const val ACTION_START = "ninja.jeremy.liveninja.caraudio.START"
        const val ACTION_END = "ninja.jeremy.liveninja.caraudio.END"
        const val EXTRA_TOKEN = "car_audio_token"
        private const val NO_TOKEN = -1L
        private const val CHANNEL_ID = "live_ninja_car_audio"
        private const val NOTIFICATION_ID = 4211
        private const val REQUEST_OPEN = 61
        private const val REQUEST_END = 62
        private const val SESSION_TYPES = ServiceInfo.FOREGROUND_SERVICE_TYPE_MICROPHONE or
            ServiceInfo.FOREGROUND_SERVICE_TYPE_MEDIA_PLAYBACK

        fun startIntent(context: Context, token: Long): Intent =
            Intent(context, CarAudioForegroundService::class.java)
                .setAction(ACTION_START)
                .putExtra(EXTRA_TOKEN, token)

        /** Explicit End intent whose identity includes the session token. */
        fun endIntent(context: Context, token: Long): Intent =
            Intent(context, CarAudioForegroundService::class.java)
                .setAction(ACTION_END)
                .setData(Uri.parse(carAudioEndDataUri(token)))
                .putExtra(EXTRA_TOKEN, token)
    }
}
