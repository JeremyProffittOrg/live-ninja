package ninja.jeremy.liveninja.net

import java.io.IOException
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import ninja.jeremy.liveninja.auth.StoredSession
import ninja.jeremy.liveninja.auth.TokenStore

/** Proactive refresh preserves the UI's identity; dispatch itself never authenticates again. */
@Singleton
class BoundJobsSession @Inject constructor(private val tokens: TokenStore, private val refresher: TokenRefresher) {
    suspend fun credentials(expectedSessionId: String): StoredSession = withContext(Dispatchers.IO) {
        require(expectedSessionId.isNotBlank())
        var session = tokens.session() ?: throw JobsSessionException()
        if (session.sessionId != expectedSessionId) throw JobsSessionException()
        val now = System.currentTimeMillis() / 1000
        if (session.accessExpiresAt <= now + 60) {
            refresher.refreshBoundBlocking(expectedSessionId, session.accessToken)
            session = tokens.session() ?: throw JobsSessionException()
        }
        if (session.sessionId != expectedSessionId || session.accessExpiresAt <= now) throw JobsSessionException()
        session
    }
}

class JobsSessionException : IOException("The original sign-in session is no longer available. Sign in again and retry.")
