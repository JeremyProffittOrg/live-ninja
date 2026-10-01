package ninja.jeremy.liveninja.update

import android.content.Context
import android.content.ActivityNotFoundException
import androidx.lifecycle.ViewModel
import dagger.hilt.android.lifecycle.HiltViewModel
import dagger.hilt.android.qualifiers.ApplicationContext
import javax.inject.Inject
import kotlinx.coroutines.flow.StateFlow
import ninja.jeremy.liveninja.log.LNLog
import ninja.jeremy.liveninja.log.LogCategory

/**
 * UI adapter over [AppUpdateCoordinator]. Automatic checks run from
 * MainActivity.onStart and [AppUpdateWorker]; this only renders their state
 * and carries the manual Update / Retry button.
 */
@HiltViewModel
class AppUpdateViewModel @Inject constructor(
    @ApplicationContext private val context: Context,
    private val coordinator: AppUpdateCoordinator,
) : ViewModel() {

    val state: StateFlow<AppUpdateState> = coordinator.state

    fun dismiss() = coordinator.dismiss()

    /** Manual check + install: bypasses the throttle and a previous decline. */
    fun startInstall() {
        coordinator.launchCheck(UpdateTrigger.MANUAL)
    }

    /** Settings › Install unknown apps for this package. */
    fun openInstallPermissionSettings() {
        try {
            context.startActivity(AppUpdateNotifier.unknownSourcesIntent(context))
        } catch (e: ActivityNotFoundException) {
            LNLog.w(LogCategory.GENERAL, TAG, "no unknown-sources settings screen", e)
        }
    }

    private companion object {
        const val TAG = "AppUpdate"
    }
}
