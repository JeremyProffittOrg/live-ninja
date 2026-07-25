package ninja.jeremy.liveninja

import android.app.Application
import android.content.Context
import androidx.hilt.work.HiltWorkerFactory
import androidx.work.Configuration
import dagger.hilt.android.HiltAndroidApp
import javax.inject.Inject
import ninja.jeremy.liveninja.auth.AuthRepository
import ninja.jeremy.liveninja.log.LNLog
import ninja.jeremy.liveninja.log.LogCategory
import ninja.jeremy.liveninja.log.LogSink

@HiltAndroidApp
class LiveNinjaApplication : Application(), Configuration.Provider {

    @Inject lateinit var authRepository: AuthRepository

    /**
     * Eagerly constructed so file logging is live from process start (M6.4).
     * [LogSink]'s `init {}` self-registers into [LNLog.sink] the moment Hilt
     * builds this singleton — member injection happens in [super.onCreate], so
     * the sink is online before any of our own bootstrap logging below runs
     * (before this, [LNLog] is logcat-passthrough only).
     */
    @Inject lateinit var logSink: LogSink

    /** Lets `@HiltWorker`-annotated workers (M8.4 [ninja.jeremy.liveninja.wake.WakeWatchdogWorker]) receive injected deps. */
    @Inject lateinit var hiltWorkerFactory: HiltWorkerFactory

    /**
     * Supplies [HiltWorkerFactory] for WorkManager's genuinely on-demand init
     * (M22.2 cold start): this alone does NOT defer initialization — see the
     * `tools:node="remove"` merge on `androidx.startup.InitializationProvider`
     * in AndroidManifest.xml for the other required half. With both in place,
     * WorkManager.initialize() runs (using this configuration) the first time
     * `WorkManager.getInstance(context)` is called — [ninja.jeremy.liveninja.wake.WakeWatchdogWorker]'s
     * enqueue()/cancel() calls in WakeWordService — instead of eagerly on
     * every process start regardless of whether the wake service ever runs.
     */
    override val workManagerConfiguration: Configuration
        get() = Configuration.Builder()
            .setWorkerFactory(hiltWorkerFactory)
            .build()

    /**
     * Prime the two SharedPreferences files the eager Hilt singleton graph is
     * about to read synchronously on the main thread (M22.2 cold start):
     * `LogSink`'s constructor (below) pulls in [ninja.jeremy.liveninja.ui.state.SettingsStore],
     * whose constructor does a blocking `getString`/parse of "liveninja_settings",
     * and `MainActivity`'s eager `WakePreferences` field-inject does the same for
     * "wake" moments later. `Context.getSharedPreferences()` kicks off its XML
     * parse on a background thread the *first* time a given file name is opened
     * for this process, then caches the loaded instance — calling it here, off
     * the main thread, before Hilt touches either file, gives that parse the
     * maximum possible head start so the later synchronous reads are far more
     * likely to find the file already loaded instead of blocking on first touch.
     * This changes only *when* the file is opened, never what is read from it.
     */
    override fun attachBaseContext(base: Context) {
        super.attachBaseContext(base)
        Thread({
            base.getSharedPreferences("liveninja_settings", Context.MODE_PRIVATE)
            base.getSharedPreferences("wake", Context.MODE_PRIVATE)
        }, "prefs-warmup").start()
    }

    override fun onCreate() {
        super.onCreate()
        // Reference the injected sink so it is definitely instantiated (and its
        // LNLog self-registration has run) — @Inject already forces this, but the
        // explicit touch documents the eager-init contract and silences unused-field lint.
        LNLog.i(LogCategory.GENERAL, TAG, "LiveNinja process start; file logging online (sink=${logSink.hashCode()})")
        // Restore the persisted session and hook the foreground observer for the
        // silent sliding token refresh. AuthRepository.start() only launches
        // supervised coroutines (the credential-store corruption path self-heals
        // in TokenStore and its scope carries a CoroutineExceptionHandler), but
        // guard the bootstrap itself so no unforeseen failure here can kill the
        // process on load (01-platform §A1).
        runCatching { authRepository.start() }
            .onFailure { LNLog.e(LogCategory.AUTH, TAG, "Auth bootstrap failed; continuing signed-out", it) }
    }

    private companion object {
        const val TAG = "LiveNinjaApplication"
    }
}
