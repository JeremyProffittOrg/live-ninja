package ninja.jeremy.liveninja.auth

import android.content.Context
import android.content.SharedPreferences
import android.util.Log
import androidx.core.content.edit
import androidx.security.crypto.EncryptedSharedPreferences
import androidx.security.crypto.MasterKey
import dagger.hilt.android.qualifiers.ApplicationContext
import java.io.IOException
import java.security.GeneralSecurityException
import java.security.KeyStore
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow

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
 *
 * **Self-healing (01-platform §A1):** security-crypto throws
 * [GeneralSecurityException]/[IOException] when the keyset or master key is
 * corrupt (KeyStoreException / AEADBadTagException / InvalidProtocolBufferException).
 * Left uncaught in [AuthRepository]'s unsupervised coroutine this killed the
 * process — the "crash on load". Instead the first open wipes the corrupt store
 * (prefs file + master key) and retries once; a persistent failure drops the
 * store into a null mode where reads return null and writes no-op, so the app
 * degrades to a forced re-login rather than crashing. Any corruption wipe raises
 * [storeReset] so the auth layer can surface the reset and re-login prompt (M1.4).
 */
@Singleton
class TokenStore @Inject constructor(
    @ApplicationContext private val context: Context,
) {

    /**
     * Test seam (01-platform §A1): the factory that opens the encrypted prefs.
     * Production builds the [EncryptedSharedPreferences]; tests inject a factory
     * that can throw (corruption) or return a fake. Read once, lazily.
     */
    internal var prefsFactory: (Context) -> SharedPreferences = ::createEncryptedPrefs

    private val _storeReset = MutableStateFlow(false)

    /**
     * True once a corruption-triggered wipe has forced the local session to be
     * discarded. The auth layer observes this to sign out and explain the reset
     * on the login screen (M1.4). Latches on — it reflects "a reset happened".
     */
    val storeReset: StateFlow<Boolean> = _storeReset

    /**
     * The encrypted prefs, or null when the store is unusable even after a heal
     * attempt (null mode). Opened lazily off the first access.
     */
    private val prefs: SharedPreferences? by lazy { openPrefsSelfHealing() }

    private fun openPrefsSelfHealing(): SharedPreferences? =
        try {
            prefsFactory(context)
        } catch (e: GeneralSecurityException) {
            healAndRetry(e)
        } catch (e: IOException) {
            healAndRetry(e)
        }

    /** Wipe the corrupt store, flag the reset, and retry the open exactly once. */
    private fun healAndRetry(cause: Exception): SharedPreferences? {
        Log.w(TAG, "Encrypted credential store unusable; wiping and retrying once", cause)
        wipeCorruptStore()
        _storeReset.value = true
        return try {
            prefsFactory(context)
        } catch (e: GeneralSecurityException) {
            Log.e(TAG, "Credential store still unusable after wipe; entering null mode", e)
            null
        } catch (e: IOException) {
            Log.e(TAG, "Credential store still unusable after wipe; entering null mode", e)
            null
        }
    }

    /** Delete the corrupt prefs file and the (possibly unusable) master key. */
    private fun wipeCorruptStore() {
        runCatching { context.deleteSharedPreferences(PREFS_NAME) }
            .onFailure { Log.w(TAG, "Failed to delete corrupt prefs file", it) }
        runCatching {
            val keyStore = KeyStore.getInstance(ANDROID_KEYSTORE).apply { load(null) }
            if (keyStore.containsAlias(MASTER_KEY_ALIAS)) keyStore.deleteEntry(MASTER_KEY_ALIAS)
        }.onFailure { Log.w(TAG, "Failed to delete corrupt master key", it) }
    }

    private fun createEncryptedPrefs(context: Context): SharedPreferences {
        val masterKey = MasterKey.Builder(context)
            .setKeyScheme(MasterKey.KeyScheme.AES256_GCM)
            .build()
        return EncryptedSharedPreferences.create(
            context,
            PREFS_NAME,
            masterKey,
            EncryptedSharedPreferences.PrefKeyEncryptionScheme.AES256_SIV,
            EncryptedSharedPreferences.PrefValueEncryptionScheme.AES256_GCM,
        )
    }

    // ---- session credentials ----

    @Synchronized
    fun session(): StoredSession? {
        val prefs = this.prefs ?: return null
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
        val prefs = this.prefs ?: return
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
        val prefs = this.prefs ?: return
        prefs.edit {
            putString(KEY_ACCESS, accessToken)
            putLong(KEY_ACCESS_EXP, accessExpiresAt)
            if (refreshToken != null) putString(KEY_REFRESH, refreshToken)
            if (refreshExpiresAt != null) putLong(KEY_REFRESH_EXP, refreshExpiresAt)
            if (sessionId != null) putString(KEY_SESSION_ID, sessionId)
        }
    }

    /** Current access token, or null when signed out (or the store is unusable). */
    @Synchronized
    fun accessToken(): String? = prefs?.getString(KEY_ACCESS, null)

    @Synchronized
    fun clearSession() {
        val prefs = this.prefs ?: return
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
        val prefs = this.prefs ?: return
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
        val prefs = this.prefs ?: return null
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
        const val TAG = "TokenStore"
        const val PREFS_NAME = "liveninja_auth"
        const val ANDROID_KEYSTORE = "AndroidKeyStore"
        const val MASTER_KEY_ALIAS = "_androidx_security_master_key_"

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
