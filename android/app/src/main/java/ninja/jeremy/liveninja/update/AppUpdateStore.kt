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
        const val PREFS = "app_update"
        const val KEY_LAST_CHECK = "last_check_at_ms"
        const val KEY_DECLINED = "declined_version_code"
        const val KEY_COMMITTED = "committed_version_code"
        const val KEY_PERM_NOTIFIED = "install_permission_notified_version_code"
    }
}
