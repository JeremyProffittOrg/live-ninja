package ninja.jeremy.liveninja.update

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.pm.PackageInstaller
import android.os.Build
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.ProcessLifecycleOwner
import ninja.jeremy.liveninja.log.LNLog
import ninja.jeremy.liveninja.log.LogCategory

/**
 * PackageInstaller status callback. When the system needs a confirmation
 * screen (always on API 29/30; on 31+ whenever a silent update is not
 * allowed) it arrives as STATUS_PENDING_USER_ACTION with an EXTRA_INTENT.
 *
 * The confirm screen is launched directly; background activity starts are
 * blocked on API 29+, so when the app is not in the foreground a
 * notification that opens the same screen is posted as well.
 */
class UpdateInstallReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        val status = intent.getIntExtra(PackageInstaller.EXTRA_STATUS, PackageInstaller.STATUS_FAILURE)
        val versionCode = intent.getLongExtra(EXTRA_VERSION_CODE, 0L)
        val store = AppUpdateStore(context)
        when (status) {
            PackageInstaller.STATUS_PENDING_USER_ACTION -> {
                val confirm = confirmIntent(intent)
                if (confirm == null) {
                    LNLog.w(LogCategory.GENERAL, TAG, "pending user action without a confirm intent")
                    AppUpdateCoordinator.publish(AppUpdateState.Failed("Android did not provide an install screen."))
                    return
                }
                confirm.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
                val foreground = isAppInForeground()
                runCatching { context.startActivity(confirm) }
                    .onFailure { LNLog.w(LogCategory.GENERAL, TAG, "could not launch install confirm", it) }
                if (!foreground) AppUpdateNotifier.postConfirmInstall(context, confirm)
                LNLog.i(LogCategory.GENERAL, TAG, "install needs user confirmation (foreground=$foreground)")
            }
            PackageInstaller.STATUS_SUCCESS -> {
                // Normally the process is replaced before this is delivered.
                LNLog.i(LogCategory.GENERAL, TAG, "package install succeeded (versionCode $versionCode)")
                store.declinedVersionCode = 0L
                AppUpdateNotifier.cancelAll(context)
                AppUpdateCoordinator.publish(AppUpdateState.Idle)
            }
            PackageInstaller.STATUS_FAILURE_ABORTED -> {
                // The user backed out of the confirm screen: remember it so
                // automatic checks offer the update instead of re-prompting.
                LNLog.i(LogCategory.GENERAL, TAG, "user cancelled install of versionCode $versionCode")
                if (versionCode > 0) store.declinedVersionCode = versionCode
                AppUpdateCoordinator.publish(AppUpdateState.Idle)
            }
            else -> {
                val msg = intent.getStringExtra(PackageInstaller.EXTRA_STATUS_MESSAGE) ?: "status $status"
                LNLog.w(LogCategory.GENERAL, TAG, "package install failed: $msg")
                AppUpdateCoordinator.publish(AppUpdateState.Failed(msg.take(200)))
            }
        }
    }

    private fun confirmIntent(intent: Intent): Intent? =
        if (Build.VERSION.SDK_INT >= 33) {
            intent.getParcelableExtra(Intent.EXTRA_INTENT, Intent::class.java)
        } else {
            @Suppress("DEPRECATION")
            intent.getParcelableExtra(Intent.EXTRA_INTENT)
        }

    private fun isAppInForeground(): Boolean =
        runCatching {
            ProcessLifecycleOwner.get().lifecycle.currentState.isAtLeast(Lifecycle.State.STARTED)
        }.getOrDefault(false)

    companion object {
        const val ACTION = "ninja.jeremy.liveninja.UPDATE_INSTALL"
        const val EXTRA_VERSION_CODE = "ninja.jeremy.liveninja.extra.UPDATE_VERSION_CODE"
        private const val TAG = "UpdateInstall"
    }
}
