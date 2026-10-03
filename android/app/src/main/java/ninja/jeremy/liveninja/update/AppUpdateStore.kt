package ninja.jeremy.liveninja.update

import android.content.Context
import android.content.SharedPreferences

/**
 * Small persisted state for the self-updater (plain SharedPreferences: none
 * of it is sensitive). Constructed directly from a [Context] so the
 * PackageInstaller [UpdateInstallReceiver] can use it without Hilt.
 */
class AppUpdateStore(context: Context) {
    private val prefs: SharedPreferences =
        context.applicationContext.getSharedPreferences(PREFS, Context.MODE_PRIVATE)

    /** Wall-clock ms of the last pointer fetch attempt (0 = never). */
    var lastCheckAtMs: Long
        get() = prefs.getLong(KEY_LAST_CHECK, 0L)
        set(value) = prefs.edit().putLong(KEY_LAST_CHECK, value).apply()

    var consecutiveFetchFailures: Int
        get() = prefs.getInt("fetch_failures", 0).coerceIn(0, 4)
        set(value) = prefs.edit().putInt("fetch_failures", value.coerceIn(0, 4)).apply()

    var pendingSessionId: Int
        get() = prefs.getInt("pending_session_id", -1)
        private set(value) = prefs.edit().putInt("pending_session_id", value).apply()

    /** Written synchronously before handing the session to Android. */
    fun recordPendingSession(sessionId: Int, versionCode: Long) = synchronized(INSTALL_LOCK) {
        check(prefs.edit().putInt("pending_session_id", sessionId)
            .putLong(KEY_COMMITTED, versionCode).commit()) { "Could not record install session" }
    }

    /** A late result for the old session must never clear its replacement. */
    fun clearPendingSession(expectedSessionId: Int, expectedVersion: Long,
                            declinedVersion: Long? = null): Boolean = synchronized(INSTALL_LOCK) {
        if (pendingSessionId != expectedSessionId || committedVersionCode != expectedVersion) return@synchronized false
        val edit = prefs.edit().putInt("pending_session_id", -1)
        if (declinedVersion != null) edit.putLong(KEY_DECLINED, declinedVersion)
        check(edit.commit()) { "Could not clear install session" }
        true
    }

    /** versionCode whose system confirm the user cancelled (0 = none). */
    var declinedVersionCode: Long
        get() = prefs.getLong(KEY_DECLINED, 0L)
        set(value) = prefs.edit().putLong(KEY_DECLINED, value).apply()

    /** versionCode of the session most recently committed (0 = none). */
    var committedVersionCode: Long
        get() = prefs.getLong(KEY_COMMITTED, 0L)
        set(value) = prefs.edit().putLong(KEY_COMMITTED, value).apply()

    /** versionCode for which the "allow install unknown apps" notification was already posted. */
    var installPermissionNotifiedVersionCode: Long
        get() = prefs.getLong(KEY_PERM_NOTIFIED, 0L)
        set(value) = prefs.edit().putLong(KEY_PERM_NOTIFIED, value).apply()

    private companion object {
        val INSTALL_LOCK = Any()
        const val PREFS = "app_update"
        const val KEY_LAST_CHECK = "last_check_at_ms"
        const val KEY_DECLINED = "declined_version_code"
        const val KEY_COMMITTED = "committed_version_code"
        const val KEY_PERM_NOTIFIED = "install_permission_notified_version_code"
    }
}
