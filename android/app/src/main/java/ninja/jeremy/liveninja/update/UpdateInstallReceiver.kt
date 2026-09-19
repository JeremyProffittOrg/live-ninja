package ninja.jeremy.liveninja.update

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.pm.PackageInstaller
import ninja.jeremy.liveninja.log.LNLog
import ninja.jeremy.liveninja.log.LogCategory

/**
 * PackageInstaller status callback. When the system needs a confirmation
 * screen it arrives as STATUS_PENDING_USER_ACTION with an extra Intent.
 */
class UpdateInstallReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        val status = intent.getIntExtra(PackageInstaller.EXTRA_STATUS, PackageInstaller.STATUS_FAILURE)
        when (status) {
            PackageInstaller.STATUS_PENDING_USER_ACTION -> {
                @Suppress("DEPRECATION")
                val confirm = intent.getParcelableExtra<Intent>(Intent.EXTRA_INTENT)
                if (confirm != null) {
                    confirm.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
                    context.startActivity(confirm)
                }
            }
            PackageInstaller.STATUS_SUCCESS ->
                LNLog.i(LogCategory.GENERAL, TAG, "package install succeeded")
            else -> {
                val msg = intent.getStringExtra(PackageInstaller.EXTRA_STATUS_MESSAGE) ?: "status $status"
                LNLog.w(LogCategory.GENERAL, TAG, "package install failed: $msg")
            }
        }
    }

    companion object {
        const val ACTION = "ninja.jeremy.liveninja.UPDATE_INSTALL"
        private const val TAG = "UpdateInstall"
    }
}
