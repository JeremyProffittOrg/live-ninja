package ninja.jeremy.liveninja

import android.app.Application
import dagger.hilt.android.HiltAndroidApp
import javax.inject.Inject
import ninja.jeremy.liveninja.auth.AuthRepository
import ninja.jeremy.liveninja.log.LNLog
import ninja.jeremy.liveninja.log.LogCategory
import ninja.jeremy.liveninja.log.LogSink

@HiltAndroidApp
class LiveNinjaApplication : Application() {

    @Inject lateinit var authRepository: AuthRepository

    /**
     * Eagerly constructed so file logging is live from process start (M6.4).
     * [LogSink]'s `init {}` self-registers into [LNLog.sink] the moment Hilt
     * builds this singleton — member injection happens in [super.onCreate], so
     * the sink is online before any of our own bootstrap logging below runs
     * (before this, [LNLog] is logcat-passthrough only).
     */
    @Inject lateinit var logSink: LogSink

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
