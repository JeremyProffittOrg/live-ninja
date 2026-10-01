package ninja.jeremy.liveninja.update

import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.content.pm.PackageInstaller
import android.os.Build
import dagger.hilt.android.qualifiers.ApplicationContext
import java.io.File
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import ninja.jeremy.liveninja.log.LNLog
import ninja.jeremy.liveninja.log.LogCategory

@Singleton
class AppUpdateInstaller @Inject constructor(
    @ApplicationContext private val context: Context,
) {
    /**
     * Stream a verified [apk] into a PackageInstaller session and commit it.
     * The result arrives asynchronously in [UpdateInstallReceiver].
     *
     * On API 31+ the session asks for USER_ACTION_NOT_REQUIRED. Android honours
     * that only when this app is already the installer of record for itself
     * (i.e. the build being replaced was itself installed by this updater) and
     * "install unknown apps" is granted; otherwise the system still returns
     * STATUS_PENDING_USER_ACTION and the receiver shows the confirm screen.
     */
    suspend fun install(apk: File, versionCode: Long) = withContext(Dispatchers.IO) {
        val installer = context.packageManager.packageInstaller
        val params = PackageInstaller.SessionParams(PackageInstaller.SessionParams.MODE_FULL_INSTALL)
        params.setAppPackageName(AndroidReleasePolicy.PACKAGE_NAME)
        params.setSize(apk.length())
        if (Build.VERSION.SDK_INT >= 31) {
            params.setRequireUserAction(PackageInstaller.SessionParams.USER_ACTION_NOT_REQUIRED)
        }
        val sessionId = installer.createSession(params)
        try {
            installer.openSession(sessionId).use { session ->
                session.openWrite("app", 0, apk.length()).use { out ->
                    apk.inputStream().use { it.copyTo(out) }
                    session.fsync(out)
                }
                val callback = Intent(context, UpdateInstallReceiver::class.java)
                    .setAction(UpdateInstallReceiver.ACTION)
                    .putExtra(UpdateInstallReceiver.EXTRA_VERSION_CODE, versionCode)
                // FLAG_MUTABLE is required: the system fills EXTRA_STATUS / EXTRA_INTENT in.
                val flags = PendingIntent.FLAG_UPDATE_CURRENT or
                    if (Build.VERSION.SDK_INT >= 31) PendingIntent.FLAG_MUTABLE else 0
                val pending = PendingIntent.getBroadcast(context, sessionId, callback, flags)
                session.commit(pending.intentSender)
                LNLog.i(LogCategory.GENERAL, TAG, "committed install session $sessionId for versionCode $versionCode")
            }
        } catch (t: Throwable) {
            runCatching { installer.abandonSession(sessionId) }
            throw t
        }
    }

    private companion object {
        const val TAG = "AppUpdateInstaller"
    }
}
