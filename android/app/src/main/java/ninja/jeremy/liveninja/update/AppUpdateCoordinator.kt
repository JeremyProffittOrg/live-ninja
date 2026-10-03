package ninja.jeremy.liveninja.update

import android.content.Context
import dagger.hilt.android.qualifiers.ApplicationContext
import java.util.concurrent.atomic.AtomicBoolean
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.async
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.launch
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.sync.Mutex
import ninja.jeremy.liveninja.BuildConfig
import ninja.jeremy.liveninja.log.LNLog
import ninja.jeremy.liveninja.log.LogCategory

/** What the update UI shows. Shared by the foreground check, the worker and the receiver. */
sealed class AppUpdateState {
    data object Idle : AppUpdateState()
    data class Available(
        val versionName: String,
        val versionCode: Long,
        val sizeBytes: Long,
    ) : AppUpdateState()
    data class NeedsInstallPermission(val versionName: String) : AppUpdateState()
    data class Downloading(val receivedBytes: Long, val totalBytes: Long) : AppUpdateState()
    data object Installing : AppUpdateState()
    data class Failed(val message: String) : AppUpdateState()
}

/**
 * The one self-update pipeline: throttle → fetch pointer → [UpdateDecider] →
 * verified download ([AppUpdateRepository.download], SHA-256 + size) →
 * PackageInstaller commit ([AppUpdateInstaller]). Single-flight: a check
 * already running makes a concurrent one a no-op.
 */
@Singleton
class AppUpdateCoordinator @Inject constructor(
    @ApplicationContext private val context: Context,
    private val repository: AppUpdateRepository,
    private val installer: AppUpdateInstaller,
) {
    private val store = AppUpdateStore(context)
    private val running = Mutex()
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    private val workerScheduled = AtomicBoolean(false)
    private val foregroundLoop = ForegroundUpdateLoop(scope) {
        // This sibling operation keeps an already-started verified download
        // alive across onStop. Cancelling the timer prevents any NEW checks.
        scope.async {
            try { check(UpdateTrigger.FOREGROUND) }
            catch (t: Throwable) {
                if (t is CancellationException) throw t
                store.lastCheckAtMs = System.currentTimeMillis()
                store.consecutiveFetchFailures += 1
                LNLog.w(LogCategory.GENERAL, TAG, "foreground update check failed", t)
            }
        }.await()
        UpdateRetrySchedule.remainingMs(System.currentTimeMillis(), store.lastCheckAtMs,
            store.consecutiveFetchFailures).coerceAtLeast(60_000L)
    }

    /** True after a check stopped only because "install unknown apps" was off. */
    @Volatile
    private var blockedOnPermission = false
    @Volatile private var reportPendingOnForeground = true

    val state: StateFlow<AppUpdateState> get() = sharedState.asStateFlow()

    /** Clear a finished dialog (Later / dismiss). */
    fun dismiss() {
        if (sharedState.value !is AppUpdateState.Downloading) {
            sharedState.value = AppUpdateState.Idle
        }
    }

    /**
     * Fire-and-forget [check] on the coordinator's own process-lifetime scope, so
     * a download is not cancelled when the activity or a ViewModel goes away.
     */
    fun launchCheck(trigger: UpdateTrigger) {
        scope.launch {
            runCatching { check(trigger) }
                .onFailure { LNLog.w(LogCategory.GENERAL, TAG, "update check ($trigger) crashed", it) }
        }
    }

    /**
     * MainActivity.onStart hook: starts one hourly loop while visible and
     * retains the existing six-hour background fallback.
     */
    fun onAppForegrounded() {
        reportPendingOnForeground = true
        foregroundLoop.start()
        scope.launch {
            if (workerScheduled.compareAndSet(false, true)) {
                runCatching { AppUpdateWorker.enqueue(context) }
                    .onFailure {
                        workerScheduled.set(false)
                        LNLog.w(LogCategory.GENERAL, TAG, "could not schedule AppUpdateWorker", it)
                    }
            }
        }
    }

    /** MainActivity.onStop hook. Existing downloads may finish; no new timer checks run. */
    fun onAppBackgrounded() = foregroundLoop.stop()

    /** Returns the decision taken, or null when throttled, busy, or the fetch failed. */
    suspend fun check(trigger: UpdateTrigger): UpdateDecision? {
        if (!running.tryLock()) return null
        try {
            val canInstall = context.packageManager.canRequestPackageInstalls()
            val now = System.currentTimeMillis()
            if (installer.hasPendingSession()) {
                if (trigger == UpdateTrigger.MANUAL) {
                    try { installer.abandonPendingSession() }
                    catch (t: Throwable) {
                        if (t is CancellationException) throw t
                        publish(AppUpdateState.Failed("Android still has the previous update request. Retry when it has finished."))
                        return null
                    }
                } else {
                    if (trigger == UpdateTrigger.FOREGROUND && reportPendingOnForeground) {
                        publish(AppUpdateState.Failed(PENDING_RECOVERY_MESSAGE))
                    }
                    reportPendingOnForeground = false
                    return null
                }
            }
            reportPendingOnForeground = false
            // Returning from the explicit install permission screen may retry immediately.
            val permissionJustGranted = canInstall && blockedOnPermission
            if (trigger != UpdateTrigger.MANUAL && !permissionJustGranted &&
                UpdateRetrySchedule.remainingMs(now, store.lastCheckAtMs, store.consecutiveFetchFailures) > 0) {
                return null
            }
            store.lastCheckAtMs = now
            val latest = try {
                repository.fetchLatest()
            } catch (t: Throwable) {
                if (t is CancellationException) throw t
                store.consecutiveFetchFailures += 1
                LNLog.w(LogCategory.NET, TAG, "update check ($trigger) failed", t)
                if (trigger == UpdateTrigger.MANUAL) publish(AppUpdateState.Failed(t.message?.take(200) ?: "Couldn't reach the update server."))
                return null
            }
            store.consecutiveFetchFailures = 0
            val decision = UpdateDecider.decide(
                latest = latest,
                installedVersionCode = BuildConfig.VERSION_CODE,
                canInstallPackages = canInstall,
                declinedVersionCode = store.declinedVersionCode,
                trigger = trigger,
            )
            LNLog.i(
                LogCategory.GENERAL,
                TAG,
                "update check ($trigger): installed=${BuildConfig.VERSION_CODE} remote=${latest.versionCode} -> ${decision::class.simpleName}",
            )
            blockedOnPermission = decision is UpdateDecision.NeedsInstallPermission
            when (decision) {
                UpdateDecision.UpToDate -> {
                    repository.clearCache()
                    AppUpdateNotifier.cancelAll(context)
                    if (sharedState.value is AppUpdateState.NeedsInstallPermission) publish(AppUpdateState.Idle)
                }
                is UpdateDecision.Rejected -> {
                    LNLog.w(LogCategory.GENERAL, TAG, "refusing published APK: ${decision.reason}")
                    if (trigger == UpdateTrigger.MANUAL) publish(AppUpdateState.Failed("The published update could not be verified."))
                }
                is UpdateDecision.Offer -> publish(
                    AppUpdateState.Available(
                        versionName = latest.versionName,
                        versionCode = latest.versionCode,
                        sizeBytes = latest.sizeBytes,
                    ),
                )
                is UpdateDecision.NeedsInstallPermission -> {
                    publish(AppUpdateState.NeedsInstallPermission(latest.versionName))
                    if (store.installPermissionNotifiedVersionCode != latest.versionCode) {
                        store.installPermissionNotifiedVersionCode = latest.versionCode
                        AppUpdateNotifier.postInstallPermissionNeeded(context)
                    }
                }
                is UpdateDecision.Install -> downloadAndInstall(decision.release)
            }
            return decision
        } finally {
            running.unlock()
        }
    }

    private suspend fun downloadAndInstall(release: AndroidLatestDto) {
        try {
            publish(AppUpdateState.Downloading(0, release.sizeBytes))
            val apk = repository.download(release) { copied ->
                sharedState.value = AppUpdateState.Downloading(copied, release.sizeBytes)
            }
            publish(AppUpdateState.Installing)
            installer.install(apk, release)
            // The receiver moves the state on (confirm screen, success, or failure).
        } catch (t: Throwable) {
            if (t is CancellationException) throw t
            LNLog.w(LogCategory.NET, TAG, "update install failed", t)
            publish(AppUpdateState.Failed(t.message?.take(200) ?: "Couldn't install this update."))
        }
    }

    companion object {
        private const val TAG = "AppUpdate"
        internal const val PENDING_RECOVERY_MESSAGE = "An update is waiting for Android confirmation. Tap Retry to cancel that request and open a new verified install confirmation."

        /** Process-wide so [UpdateInstallReceiver] (no Hilt) can move the UI on. */
        private val sharedState = MutableStateFlow<AppUpdateState>(AppUpdateState.Idle)

        fun publish(state: AppUpdateState) {
            sharedState.value = state
        }
    }
}
