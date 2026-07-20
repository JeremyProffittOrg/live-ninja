package ninja.jeremy.liveninja.auth

import android.net.Uri
import androidx.lifecycle.DefaultLifecycleObserver
import androidx.lifecycle.LifecycleOwner
import androidx.lifecycle.ProcessLifecycleOwner
import java.io.IOException
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.CoroutineExceptionHandler
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext
import ninja.jeremy.liveninja.config.BackendConfig
import ninja.jeremy.liveninja.log.LNLog
import ninja.jeremy.liveninja.log.LogCategory
import ninja.jeremy.liveninja.net.LiveNinjaApi
import ninja.jeremy.liveninja.net.LwaAppClaimRequest
import ninja.jeremy.liveninja.net.RefreshOutcome
import ninja.jeremy.liveninja.net.TokenRefresher
import retrofit2.HttpException

/**
 * Owns the LWA Custom-Tabs + PKCE sign-in flow and the session lifecycle:
 * tokens in the Keystore-backed [TokenStore], silent sliding refresh on app
 * foreground, and logout / logout-everywhere.
 *
 * Call [start] once from Application.onCreate — it loads the persisted
 * session and hooks the process-lifecycle foreground observer.
 */
@Singleton
class AuthRepository @Inject constructor(
    private val tokenStore: TokenStore,
    private val api: LiveNinjaApi,
    private val refresher: TokenRefresher,
) : DefaultLifecycleObserver {

    /**
     * Last-resort guard (01-platform §A1 layer 3): any auth coroutine that
     * fails without its own handling logs and drops the app to a clean signed-out
     * state rather than letting the throw reach the process's default handler and
     * kill it on load. The SupervisorJob keeps sibling coroutines alive.
     */
    private val exceptionHandler = CoroutineExceptionHandler { _, throwable ->
        LNLog.e(LogCategory.AUTH, TAG, "Unhandled auth coroutine failure; signing out defensively", throwable)
        _state.value = AuthState.SignedOut()
    }

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO + exceptionHandler)
    private val refreshMutex = Mutex()

    private val _state = MutableStateFlow<AuthState>(AuthState.SignedOut())

    /** App-wide auth state. UI gates on this (login screen vs. main app). */
    val state: StateFlow<AuthState> = _state

    /** Idempotent bootstrap: restore the persisted session + watch foregrounding. */
    fun start() {
        scope.launch {
            val session = tokenStore.session()
            if (session != null) {
                _state.value = AuthState.SignedIn(session.sessionId)
                refreshIfNeeded()
            }
        }
        scope.launch {
            refresher.sessionExpired.collect {
                _state.value = AuthState.SignedOut(AuthError.SESSION_EXPIRED)
            }
        }
        // A corruption wipe (01-platform §A1) discards the local session: surface
        // the forced re-login with an explanation, unless a fresh sign-in already
        // landed. StateFlow replays the current value, so a wipe that happened
        // before this collector attaches is still caught.
        scope.launch {
            tokenStore.storeReset.collect { wiped ->
                if (wiped && _state.value !is AuthState.SignedIn) {
                    _state.value = AuthState.SignedOut(AuthError.STORAGE_RESET)
                }
            }
        }
        ProcessLifecycleOwner.get().lifecycle.addObserver(this)
    }

    /** Silent sliding refresh whenever the app returns to the foreground. */
    override fun onStart(owner: LifecycleOwner) {
        scope.launch { refreshIfNeeded() }
    }

    // ---- sign-in (Custom Tabs + PKCE) ----

    /**
     * Begin a sign-in attempt: mint an app_state nonce + PKCE verifier,
     * persist them for the round trip, and return the backend broker
     * kickoff URL to open in a Custom Tab. The backend runs the LWA
     * round-trip against its own whitelisted callback and hands a one-shot
     * handoff code back to [BackendConfig.LWA_CUSTOM_SCHEME_REDIRECT] — so
     * LWA never sees our custom scheme. Any previous pending attempt is
     * superseded.
     */
    suspend fun beginLogin(): String = withContext(Dispatchers.IO) {
        val state = Pkce.newState()
        val verifier = Pkce.newCodeVerifier()
        tokenStore.savePendingLogin(
            PendingLogin(
                state = state,
                codeVerifier = verifier,
                redirectUri = BackendConfig.LWA_CUSTOM_SCHEME_REDIRECT,
                createdAt = System.currentTimeMillis() / 1000,
            ),
        )
        Uri.parse(BackendConfig.LWA_APP_LOGIN_URL).buildUpon()
            .appendQueryParameter("app_challenge", Pkce.codeChallengeS256(verifier))
            .appendQueryParameter("app_state", state)
            .build()
            .toString()
    }

    /** True when [uri] is our custom-scheme broker return URI. */
    fun isAuthRedirect(uri: Uri): Boolean {
        val custom = Uri.parse(BackendConfig.LWA_CUSTOM_SCHEME_REDIRECT)
        return uri.scheme == custom.scheme && uri.host == custom.host
    }

    /**
     * Complete the round trip: validate `state` (single-use), then claim the
     * one-shot handoff `code` with our PKCE verifier at the backend for the
     * first token grant.
     */
    fun handleRedirect(uri: Uri) {
        if (!isAuthRedirect(uri)) return
        if (_state.value is AuthState.SignedIn) return

        val error = uri.getQueryParameter("error")
        val code = uri.getQueryParameter("code")
        val returnedState = uri.getQueryParameter("state")

        scope.launch {
            val pending = tokenStore.consumePendingLogin()

            if (error != null) {
                LNLog.w(LogCategory.AUTH, TAG, "LWA returned error=$error")
                _state.value = AuthState.SignedOut(AuthError.LWA_DENIED)
                return@launch
            }
            if (pending == null || code.isNullOrEmpty() || returnedState != pending.state ||
                nowSeconds() - pending.createdAt > PENDING_LOGIN_TTL_SECONDS
            ) {
                LNLog.w(LogCategory.AUTH, TAG, "LWA redirect state validation failed")
                _state.value = AuthState.SignedOut(AuthError.STATE_MISMATCH)
                return@launch
            }

            _state.value = AuthState.Authorizing
            try {
                val grant = api.claimLwaAppCode(
                    LwaAppClaimRequest(
                        code = code,
                        codeVerifier = pending.codeVerifier,
                    ),
                )
                tokenStore.saveSession(
                    StoredSession(
                        accessToken = grant.accessToken,
                        accessExpiresAt = grant.expiresAt,
                        refreshToken = grant.refreshToken.orEmpty(),
                        refreshExpiresAt = grant.refreshExpiresAt ?: 0L,
                        sessionId = grant.sessionId.orEmpty(),
                    ),
                )
                _state.value = AuthState.SignedIn(grant.sessionId.orEmpty())
            } catch (e: HttpException) {
                LNLog.w(LogCategory.AUTH, TAG, "Code exchange rejected: HTTP ${e.code()}")
                _state.value = AuthState.SignedOut(
                    if (e.code() == 403) AuthError.NOT_ALLOWED else AuthError.EXCHANGE_FAILED,
                )
            } catch (e: IOException) {
                LNLog.w(LogCategory.AUTH, TAG, "Code exchange network failure", e)
                _state.value = AuthState.SignedOut(AuthError.NETWORK)
            }
        }
    }

    // ---- silent sliding refresh ----

    /**
     * Rotate tokens when useful: always when the access JWT is missing/about
     * to lapse, and at most daily otherwise — each rotation slides the 30-day
     * refresh window forward, so a device opened once a month never falls off.
     */
    suspend fun refreshIfNeeded() {
        refreshMutex.withLock {
            val session = tokenStore.session() ?: return
            val now = nowSeconds()

            val accessStale = session.accessExpiresAt - now < ACCESS_REFRESH_MARGIN_SECONDS
            // refreshExpiresAt - 30d ≈ last rotation time; slide at most daily.
            val lastRotate = session.refreshExpiresAt - REFRESH_WINDOW_SECONDS
            val slideDue = session.refreshExpiresAt > 0 && now - lastRotate > SLIDE_INTERVAL_SECONDS

            if (!accessStale && !slideDue) return

            when (withContext(Dispatchers.IO) { refresher.refreshBlocking(session.accessToken) }) {
                is RefreshOutcome.Refreshed -> {
                    val refreshed = tokenStore.session()
                    if (refreshed != null && _state.value !is AuthState.SignedIn) {
                        _state.value = AuthState.SignedIn(refreshed.sessionId)
                    }
                }
                RefreshOutcome.SessionExpired ->
                    _state.value = AuthState.SignedOut(AuthError.SESSION_EXPIRED)
                RefreshOutcome.Transient -> Unit // stay signed in; retry next foreground
            }
        }
    }

    // ---- logout ----

    /**
     * Sign out this device. Best-effort server-side revoke (the backend
     * identifies the session from the Bearer JWT; the 401 authenticator will
     * refresh first if it expired), then local credentials are cleared
     * unconditionally.
     */
    suspend fun logout() {
        try {
            api.logout()
        } catch (e: HttpException) {
            LNLog.w(LogCategory.AUTH, TAG, "Server-side logout rejected: HTTP ${e.code()} (clearing locally anyway)")
        } catch (e: IOException) {
            LNLog.w(LogCategory.AUTH, TAG, "Server-side logout unreachable (clearing locally anyway)", e)
        }
        tokenStore.clearSession()
        _state.value = AuthState.SignedOut()
    }

    /**
     * Log out everywhere: revokes every session and outstanding JWT across
     * all surfaces. Requires the server round trip to succeed (unlike
     * [logout], a silent local-only fallback would falsely tell the user
     * their other devices were signed out).
     *
     * @return true when the server confirmed the revoke.
     */
    suspend fun logoutAll(): Boolean {
        val ok = try {
            api.logoutAll()
            true
        } catch (e: HttpException) {
            LNLog.w(LogCategory.AUTH, TAG, "logout-all rejected: HTTP ${e.code()}")
            false
        } catch (e: IOException) {
            LNLog.w(LogCategory.AUTH, TAG, "logout-all unreachable", e)
            false
        }
        if (ok) {
            tokenStore.clearSession()
            _state.value = AuthState.SignedOut()
        }
        return ok
    }

    private fun nowSeconds(): Long = System.currentTimeMillis() / 1000

    private companion object {
        const val TAG = "AuthRepository"

        /** Refresh when the 15-min access JWT has less than this left. */
        const val ACCESS_REFRESH_MARGIN_SECONDS = 5 * 60L

        /** Backend refresh window for the Android surface (30 days). */
        const val REFRESH_WINDOW_SECONDS = 30L * 24 * 60 * 60

        /** Slide the refresh window at most once per day. */
        const val SLIDE_INTERVAL_SECONDS = 24 * 60 * 60L

        /** Pending PKCE round trips older than this are rejected (backend uses 10 min). */
        const val PENDING_LOGIN_TTL_SECONDS = 10 * 60L
    }
}
