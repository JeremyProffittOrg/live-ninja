package ninja.jeremy.liveninja.auth

import dagger.Binds
import dagger.Module
import dagger.hilt.InstallIn
import dagger.hilt.components.SingletonComponent
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import ninja.jeremy.liveninja.net.RefreshOutcome
import ninja.jeremy.liveninja.net.TokenRefresher
import ninja.jeremy.liveninja.wake.WakeTokenProvider

/**
 * Fills the wake stack's optional [WakeTokenProvider] seam (wake/WakeModule.kt)
 * with the real auth session: returns the stored access JWT when it still has
 * >30s of life, otherwise rotates via the single-flight [TokenRefresher].
 * Returns null when signed out or the refresh is rejected — [ModelManager]
 * then skips backend model sync and serves the packaged/cached model.
 */
@Singleton
class SessionWakeTokenProvider @Inject constructor(
    private val tokenStore: TokenStore,
    private val refresher: TokenRefresher,
) : WakeTokenProvider {

    override suspend fun accessToken(): String? = withContext(Dispatchers.IO) {
        val session = tokenStore.session() ?: return@withContext null
        val now = System.currentTimeMillis() / 1000
        if (session.accessExpiresAt > now + 30) return@withContext session.accessToken
        when (val outcome = refresher.refreshBlocking(session.accessToken)) {
            is RefreshOutcome.Refreshed -> outcome.accessToken
            RefreshOutcome.SessionExpired, RefreshOutcome.Transient -> null
        }
    }

    @Module
    @InstallIn(SingletonComponent::class)
    abstract class Binder {
        @Binds
        abstract fun bindWakeTokenProvider(impl: SessionWakeTokenProvider): WakeTokenProvider
    }
}
