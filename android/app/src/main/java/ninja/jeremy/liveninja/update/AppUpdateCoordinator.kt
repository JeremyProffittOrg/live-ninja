package ninja.jeremy.liveninja.update

import android.content.Context
import dagger.hilt.android.qualifiers.ApplicationContext
import java.util.concurrent.atomic.AtomicBoolean
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
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

    /** True after a check stopped only because "install unknown apps" was off. */
    @Volatile
    private var blockedOnPermission = false

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
     * MainActivity.onStart hook: make sure the 6-hourly [AppUpdateWorker] is
     * scheduled (once per process, off the main thread so WorkManager's
     * database never opens on the launch path), then run a throttled check.
     */
    fun onAppForegrounded() {
        scope.launch {
            if (workerScheduled.compareAndSet(false, true)) {
                runCatching { AppUpdateWorker.enqueue(context) }
                    .onFailure {
                        workerScheduled.set(false)
                        LNLog.w(LogCategory.GENERAL, TAG, "could not schedule AppUpdateWorker", it)
                    }
            }
            runCatching { check(UpdateTrigger.FOREGROUND) }
                .onFailure { LNLog.w(LogCategory.GENERAL, TAG, "foreground update check crashed", it) }
        }
    }

    /** Returns the decision taken, or null when throttled, busy, or the fetch failed. */
    suspend fun check(trigger: UpdateTrigger): UpdateDecision? {
        val canInstall = context.packageManager.canRequestPackageInstalls()
        val now = System.currentTimeMillis()
        // A check that was blocked only on "install unknown apps" re-runs as soon as
        // the grant appears, instead of waiting out the hour.
        val permissionJustGranted = canInstall && blockedOnPermission
        if (!permissionJustGranted && !UpdateCheckThrottle.isDue(trigger, now, store.lastCheckAtMs)) {
            return null
        }
        if (!running.tryLock()) return null
        try {
            store.lastCheckAtMs = now
            val latest = try {
                repository.fetchLatest()
            } catch (t: Throwable) {
                LNLog.w(LogCategory.NET, TAG, "update check ($trigger) failed", t)
                if (trigger == UpdateTrigger.MANUAL) publish(AppUpdateState.Failed(t.message?.take(200) ?: "Couldn't reach the update server."))
                return null
            }
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
                is UpdateDecision.Rejected ->
                    LNLog.w(LogCategory.GENERAL, TAG, "refusing published APK: ${decision.reason}")
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
            store.committedVersionCode = release.versionCode
            installer.install(apk, release.versionCode)
            // The receiver moves the state on (confirm screen, success, or failure).
        } catch (t: Throwable) {
            LNLog.w(LogCategory.NET, TAG, "update install failed", t)
            publish(AppUpdateState.Failed(t.message?.take(200) ?: "Couldn't install this update."))
        }
    }

    companion object {
        private const val TAG = "AppUpdate"

        /** Process-wide so [UpdateInstallReceiver] (no Hilt) can move the UI on. */
        private val sharedState = MutableStateFlow<AppUpdateState>(AppUpdateState.Idle)

        fun publish(state: AppUpdateState) {
            sharedState.value = state
        }
    }
}
