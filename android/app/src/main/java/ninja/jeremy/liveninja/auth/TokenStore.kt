package ninja.jeremy.liveninja.auth

import android.content.Context
import android.content.SharedPreferences
import androidx.core.content.edit
import androidx.security.crypto.EncryptedSharedPreferences
import androidx.security.crypto.MasterKey
import dagger.hilt.android.qualifiers.ApplicationContext
import javax.inject.Inject
import javax.inject.Singleton

/**
 * The persisted session credentials, as returned by the backend's
 * exchange/refresh endpoints. Epoch values are unix seconds (matching the
 * backend's `expiresAt`/`refreshExpiresAt` wire fields).
 */
data class StoredSession(
    val accessToken: String,
    val accessExpiresAt: Long,
    val refreshToken: String,
    val refreshExpiresAt: Long,
    val sessionId: String,
)

/**
 * An in-flight LWA login attempt: the PKCE verifier + `state` nonce persisted
 * across the Custom-Tabs round trip (the process may be killed while the tab
 * is foregrounded, so this cannot live only in memory).
 */
data class PendingLogin(
    val state: String,
    val codeVerifier: String,
    val redirectUri: String,
    val createdAt: Long,
)

/**
 * Keystore-backed credential storage: [EncryptedSharedPreferences] with an
 * AES256-GCM master key held in the Android Keystore. All access is
 * synchronized on the instance; callers on the hot path should stay off the
 * main thread for the first access (the master key + prefs file open is I/O).
 */
@Singleton
class TokenStore @Inject constructor(
    @ApplicationContext private val context: Context,
) {

    private val prefs: SharedPreferences by lazy {
        val masterKey = MasterKey.Builder(context)
            .setKeyScheme(MasterKey.KeyScheme.AES256_GCM)
            .build()
        EncryptedSharedPreferences.create(
            context,
            "liveninja_auth",
            masterKey,
            EncryptedSharedPreferences.PrefKeyEncryptionScheme.AES256_SIV,
            EncryptedSharedPreferences.PrefValueEncryptionScheme.AES256_GCM,
        )
    }

    // ---- session credentials ----

    @Synchronized
    fun session(): StoredSession? {
        val access = prefs.getString(KEY_ACCESS, null) ?: return null
        val refresh = prefs.getString(KEY_REFRESH, null) ?: return null
        return StoredSession(
            accessToken = access,
            accessExpiresAt = prefs.getLong(KEY_ACCESS_EXP, 0L),
            refreshToken = refresh,
            refreshExpiresAt = prefs.getLong(KEY_REFRESH_EXP, 0L),
            sessionId = prefs.getString(KEY_SESSION_ID, "") ?: "",
        )
    }

    @Synchronized
    fun saveSession(session: StoredSession) {
        prefs.edit {
            putString(KEY_ACCESS, session.accessToken)
            putLong(KEY_ACCESS_EXP, session.accessExpiresAt)
            putString(KEY_REFRESH, session.refreshToken)
            putLong(KEY_REFRESH_EXP, session.refreshExpiresAt)
            putString(KEY_SESSION_ID, session.sessionId)
        }
    }

    /**
     * Merge a refresh response into the stored session. The backend rotates
     * the refresh token on every refresh, so all fields update together.
     */
    @Synchronized
    fun updateFromRefresh(
        accessToken: String,
        accessExpiresAt: Long,
        refreshToken: String?,
        refreshExpiresAt: Long?,
        sessionId: String?,
    ) {
        prefs.edit {
            putString(KEY_ACCESS, accessToken)
            putLong(KEY_ACCESS_EXP, accessExpiresAt)
            if (refreshToken != null) putString(KEY_REFRESH, refreshToken)
            if (refreshExpiresAt != null) putLong(KEY_REFRESH_EXP, refreshExpiresAt)
            if (sessionId != null) putString(KEY_SESSION_ID, sessionId)
        }
    }

    /** Current access token, or null when signed out. */
    @Synchronized
    fun accessToken(): String? = prefs.getString(KEY_ACCESS, null)

    @Synchronized
    fun clearSession() {
        prefs.edit {
            remove(KEY_ACCESS)
            remove(KEY_ACCESS_EXP)
            remove(KEY_REFRESH)
            remove(KEY_REFRESH_EXP)
            remove(KEY_SESSION_ID)
        }
    }

    // ---- pending login (PKCE round trip) ----

    @Synchronized
    fun savePendingLogin(pending: PendingLogin) {
        prefs.edit {
            putString(KEY_PENDING_STATE, pending.state)
            putString(KEY_PENDING_VERIFIER, pending.codeVerifier)
            putString(KEY_PENDING_REDIRECT, pending.redirectUri)
            putLong(KEY_PENDING_CREATED, pending.createdAt)
        }
    }

    /**
     * Consume (return + delete) the pending login. Single-use by design:
     * a second redirect with the same `state` must not match.
     */
    @Synchronized
    fun consumePendingLogin(): PendingLogin? {
        val state = prefs.getString(KEY_PENDING_STATE, null)
        val verifier = prefs.getString(KEY_PENDING_VERIFIER, null)
        val redirect = prefs.getString(KEY_PENDING_REDIRECT, null)
        val created = prefs.getLong(KEY_PENDING_CREATED, 0L)
        prefs.edit {
            remove(KEY_PENDING_STATE)
            remove(KEY_PENDING_VERIFIER)
            remove(KEY_PENDING_REDIRECT)
            remove(KEY_PENDING_CREATED)
        }
        if (state == null || verifier == null || redirect == null) return null
        return PendingLogin(state, verifier, redirect, created)
    }

    private companion object {
        const val KEY_ACCESS = "accessToken"
        const val KEY_ACCESS_EXP = "accessExpiresAt"
        const val KEY_REFRESH = "refreshToken"
        const val KEY_REFRESH_EXP = "refreshExpiresAt"
        const val KEY_SESSION_ID = "sessionId"
        const val KEY_PENDING_STATE = "pending.state"
        const val KEY_PENDING_VERIFIER = "pending.codeVerifier"
        const val KEY_PENDING_REDIRECT = "pending.redirectUri"
        const val KEY_PENDING_CREATED = "pending.createdAt"
    }
}
