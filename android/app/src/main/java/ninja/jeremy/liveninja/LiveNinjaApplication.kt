package ninja.jeremy.liveninja

import android.app.Application
import android.util.Log
import dagger.hilt.android.HiltAndroidApp
import javax.inject.Inject
import ninja.jeremy.liveninja.auth.AuthRepository

@HiltAndroidApp
class LiveNinjaApplication : Application() {

    @Inject lateinit var authRepository: AuthRepository

    override fun onCreate() {
        super.onCreate()
        // Restore the persisted session and hook the foreground observer for the
        // silent sliding token refresh. AuthRepository.start() only launches
        // supervised coroutines (the credential-store corruption path self-heals
        // in TokenStore and its scope carries a CoroutineExceptionHandler), but
        // guard the bootstrap itself so no unforeseen failure here can kill the
        // process on load (01-platform §A1).
        runCatching { authRepository.start() }
            .onFailure { Log.e(TAG, "Auth bootstrap failed; continuing signed-out", it) }
    }

    private companion object {
        const val TAG = "LiveNinjaApplication"
    }
}
