package ninja.jeremy.liveninja

import android.app.Application
import dagger.hilt.android.HiltAndroidApp
import javax.inject.Inject
import ninja.jeremy.liveninja.auth.AuthRepository

@HiltAndroidApp
class LiveNinjaApplication : Application() {

    @Inject lateinit var authRepository: AuthRepository

    override fun onCreate() {
        super.onCreate()
        // Restore the persisted session and hook the foreground observer for
        // the silent sliding token refresh.
        authRepository.start()
    }
}
