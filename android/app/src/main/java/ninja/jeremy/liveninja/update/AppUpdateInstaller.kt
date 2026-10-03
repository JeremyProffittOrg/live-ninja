package ninja.jeremy.liveninja.update

import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.content.pm.PackageInstaller
import android.content.pm.PackageInfo
import android.content.pm.PackageManager
import android.os.Build
import dagger.hilt.android.qualifiers.ApplicationContext
import java.io.File
import java.security.MessageDigest
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
     * API 31+ explicitly requires Android's confirmation, even when this app
     * is its own installer of record. Older supported versions use the normal
     * PackageInstaller confirmation. No silent-install authority is requested.
     */
    suspend fun install(apk: File, release: AndroidLatestDto) = withContext(Dispatchers.IO) {
        verifyArchive(apk, release)
        check(!hasPendingSession()) { "An Android install confirmation is already pending." }
        val installer = context.packageManager.packageInstaller
        val params = installSessionParams(apk.length())
        val sessionId = installer.createSession(params)
        try {
            installer.openSession(sessionId).use { session ->
                session.openWrite("app", 0, apk.length()).use { out ->
                    apk.inputStream().use { it.copyTo(out) }
                    session.fsync(out)
                }
                val callback = Intent(context, UpdateInstallReceiver::class.java)
                    .setAction(UpdateInstallReceiver.ACTION)
                    .putExtra(UpdateInstallReceiver.EXTRA_VERSION_CODE, release.versionCode)
                    .putExtra(PackageInstaller.EXTRA_SESSION_ID, sessionId)
                // FLAG_MUTABLE is required: the system fills EXTRA_STATUS / EXTRA_INTENT in.
                val flags = PendingIntent.FLAG_UPDATE_CURRENT or
                    if (Build.VERSION.SDK_INT >= 31) PendingIntent.FLAG_MUTABLE else 0
                val pending = PendingIntent.getBroadcast(context, sessionId, callback, flags)
                AppUpdateStore(context).recordPendingSession(sessionId, release.versionCode)
                session.commit(pending.intentSender)
                LNLog.i(LogCategory.GENERAL, TAG, "committed install session $sessionId for versionCode ${release.versionCode}")
            }
        } catch (t: Throwable) {
            runCatching { installer.abandonSession(sessionId) }
            AppUpdateStore(context).clearPendingSession(sessionId, release.versionCode)
            throw t
        }
    }

    fun hasPendingSession(): Boolean {
        val store = AppUpdateStore(context)
        val id = store.pendingSessionId
        val version = store.committedVersionCode
        if (id < 0) return false
        if (context.packageManager.packageInstaller.getSessionInfo(id) != null) return true
        store.clearPendingSession(id, version)
        return false
    }

    /** Only a deliberate Retry can replace a still-pending Android session. */
    fun abandonPendingSession() {
        val store = AppUpdateStore(context)
        val id = store.pendingSessionId
        val version = store.committedVersionCode
        if (id < 0) return
        val installer = context.packageManager.packageInstaller
        if (installer.getSessionInfo(id) != null) installer.abandonSession(id)
        check(store.clearPendingSession(id, version)) { "The pending update changed. Try again." }
        AppUpdateNotifier.cancelAll(context)
    }

    @Suppress("DEPRECATION") // int overload works on all supported API 29+ devices.
    internal fun verifyArchive(apk: File, release: AndroidLatestDto) {
        AndroidReleasePolicy.metadataError(release)?.let { error(it) }
        check(apk.length() == release.sizeBytes) { "APK size mismatch" }
        val digest = MessageDigest.getInstance("SHA-256")
        apk.inputStream().use { input ->
            val buffer = ByteArray(64 * 1024)
            while (true) {
                val count = input.read(buffer)
                if (count < 0) break
                digest.update(buffer, 0, count)
            }
        }
        check(digest.digest().joinToString("") { "%02x".format(it) }.equals(release.sha256, true)) { "APK SHA-256 mismatch" }
        val pm = context.packageManager
        val archive = pm.getPackageArchiveInfo(apk.absolutePath, PackageManager.GET_SIGNING_CERTIFICATES)
            ?: error("Android could not verify the APK archive")
        val installed = pm.getPackageInfo(context.packageName, PackageManager.GET_SIGNING_CERTIFICATES)
        check(archive.longVersionCode > installed.longVersionCode) { "APK is not newer than this install" }
        AndroidReleasePolicy.archiveError(release, archive.packageName, archive.longVersionCode,
            signerDigests(archive), signerDigests(installed))?.let { error(it) }
    }

    private fun signerDigests(info: PackageInfo): Set<String> =
        info.signingInfo?.apkContentsSigners?.map { signature ->
            MessageDigest.getInstance("SHA-256").digest(signature.toByteArray())
                .joinToString("") { "%02x".format(it) }
        }?.toSet().orEmpty()

    private companion object {
        const val TAG = "AppUpdateInstaller"
    }
}

internal fun installSessionParams(size: Long, sdk: Int = Build.VERSION.SDK_INT): PackageInstaller.SessionParams =
    PackageInstaller.SessionParams(PackageInstaller.SessionParams.MODE_FULL_INSTALL).apply {
        setAppPackageName(AndroidReleasePolicy.PACKAGE_NAME)
        setSize(size)
        if (sdk >= 31) setRequireUserAction(PackageInstaller.SessionParams.USER_ACTION_REQUIRED)
    }
