package ninja.jeremy.liveninja.auth

import android.net.Uri
import android.util.Log
import androidx.lifecycle.DefaultLifecycleObserver
import androidx.lifecycle.LifecycleOwner
import androidx.lifecycle.ProcessLifecycleOwner
import java.io.IOException
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext
import ninja.jeremy.liveninja.BuildConfig
import ninja.jeremy.liveninja.config.BackendConfig
import ninja.jeremy.liveninja.net.LiveNinjaApi
import ninja.jeremy.liveninja.net.LwaExchangeRequest
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

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    private val refreshMutex = Mutex()

    private val _state = MutableStateFlow<AuthState>(AuthState.SignedOut())

    /** App-wide auth state. UI gates on this (login screen vs. main app). */
    val state: StateFlow<AuthState> = _state

    /**
     * The LWA return URI for this build. Release rides the App Link
     * (assetlinks.json served by the backend, M8); debug uses the
     * custom-scheme fallback, which needs no domain verification.
     */
    val redirectUri: String =
        if (BuildConfig.DEBUG) BackendConfig.LWA_CUSTOM_SCHEME_REDIRECT
        else BackendConfig.LWA_APP_LINK_REDIRECT

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
        ProcessLifecycleOwner.get().lifecycle.addObserver(this)
    }

    /** Silent sliding refresh whenever the app returns to the foreground. */
    override fun onStart(owner: LifecycleOwner) {
        scope.launch { refreshIfNeeded() }
    }

    // ---- sign-in (Custom Tabs + PKCE) ----

    /**
     * Begin a sign-in attempt: mint state + PKCE verifier, persist them for
     * the round trip, and return the LWA authorize URL to open in a Custom
     * Tab. Any previous pending attempt is superseded.
     */
    suspend fun beginLogin(): String = withContext(Dispatchers.IO) {
        val state = Pkce.newState()
        val verifier = Pkce.newCodeVerifier()
        tokenStore.savePendingLogin(
            PendingLogin(
                state = state,
                codeVerifier = verifier,
                redirectUri = redirectUri,
                createdAt = System.currentTimeMillis() / 1000,
            ),
        )
        Uri.parse(BackendConfig.LWA_AUTHORIZE_URL).buildUpon()
            .appendQueryParameter("client_id", BackendConfig.LWA_CLIENT_ID)
            .appendQueryParameter("scope", BackendConfig.LWA_SCOPE)
            .appendQueryParameter("response_type", "code")
            .appendQueryParameter("redirect_uri", redirectUri)
            .appendQueryParameter("state", state)
            .appendQueryParameter("code_challenge", Pkce.codeChallengeS256(verifier))
            .appendQueryParameter("code_challenge_method", "S256")
            .build()
            .toString()
    }

    /** True when [uri] is one of our two registered LWA return URIs. */
    fun isAuthRedirect(uri: Uri): Boolean {
        val appLink = Uri.parse(BackendConfig.LWA_APP_LINK_REDIRECT)
        val custom = Uri.parse(BackendConfig.LWA_CUSTOM_SCHEME_REDIRECT)
        return (uri.scheme == appLink.scheme && uri.host == appLink.host && uri.path == appLink.path) ||
            (uri.scheme == custom.scheme && uri.host == custom.host)
    }

    /**
     * Complete the round trip: validate `state` (single-use), then exchange
     * `code` + verifier at the backend for the first token grant.
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
                Log.w(TAG, "LWA returned error=$error")
                _state.value = AuthState.SignedOut(AuthError.LWA_DENIED)
                return@launch
            }
            if (pending == null || code.isNullOrEmpty() || returnedState != pending.state ||
                nowSeconds() - pending.createdAt > PENDING_LOGIN_TTL_SECONDS
            ) {
                Log.w(TAG, "LWA redirect state validation failed")
                _state.value = AuthState.SignedOut(AuthError.STATE_MISMATCH)
                return@launch
            }

            _state.value = AuthState.Authorizing
            try {
                val grant = api.exchangeLwaCode(
                    LwaExchangeRequest(
                        code = code,
                        codeVerifier = pending.codeVerifier,
                        redirectURI = pending.redirectUri,
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
                Log.w(TAG, "Code exchange rejected: HTTP ${e.code()}")
                _state.value = AuthState.SignedOut(
                    if (e.code() == 403) AuthError.NOT_ALLOWED else AuthError.EXCHANGE_FAILED,
                )
            } catch (e: IOException) {
                Log.w(TAG, "Code exchange network failure", e)
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
            Log.w(TAG, "Server-side logout rejected: HTTP ${e.code()} (clearing locally anyway)")
        } catch (e: IOException) {
            Log.w(TAG, "Server-side logout unreachable (clearing locally anyway)", e)
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
            Log.w(TAG, "logout-all rejected: HTTP ${e.code()}")
            false
        } catch (e: IOException) {
            Log.w(TAG, "logout-all unreachable", e)
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
