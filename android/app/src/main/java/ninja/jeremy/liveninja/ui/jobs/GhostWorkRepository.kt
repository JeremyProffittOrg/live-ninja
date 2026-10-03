package ninja.jeremy.liveninja.ui.jobs

import java.io.IOException
import javax.inject.Inject
import ninja.jeremy.liveninja.auth.AuthRepository
import ninja.jeremy.liveninja.auth.AuthState
import ninja.jeremy.liveninja.net.*

class GhostWorkRepository @Inject constructor(private val api: LiveNinjaApi, private val boundSessions: BoundJobsSession, private val auth: AuthRepository) {
    private suspend fun bearer(expected: String): String {
        requireCurrent(expected)
        return "Bearer ${boundSessions.credentials(expected).accessToken}"
    }
    private fun requireCurrent(expected: String) {
        if ((auth.state.value as? AuthState.SignedIn)?.sessionId != expected) throw JobsSessionException()
    }
    suspend fun nodes(account: String): GhostNodesResponse {
        val result = api.ghostNodes(bearer(account), AuthBoundRequest()); requireCurrent(account)
        if (result.nodes == null || result.nodes.any { it.node_id.isBlank() }) throw IOException("Invalid node inventory.")
        return result
    }
    suspend fun jobs(account: String): GhostJobsResponse {
        val result = api.ghostJobs(bearer(account), AuthBoundRequest()); requireCurrent(account)
        if (result.events == null || result.events.any { it.event_id.isBlank() }) throw IOException("Invalid scheduled-job inventory.")
        return result
    }
    suspend fun sessions(account: String, node: String, cursor: String?): GhostSessionsResponse {
        val result = api.ghostSessions(node, cursor, bearer(account), AuthBoundRequest()); requireCurrent(account)
        if (result.version != 1 || result.node_id != node || result.coverage != "retained_only" || result.sessions == null || result.sessions.any { it.session_id.isBlank() }) throw IOException("Unverified session archive scope.")
        return result
    }
    suspend fun events(scope: GhostHistoryScope, cursor: String?): GhostEventsResponse {
        val result = api.ghostEvents(scope.nodeId, scope.providerSessionId, cursor, bearer(scope.accountSessionId), AuthBoundRequest()); requireCurrent(scope.accountSessionId)
        if (result.version != 1 || result.node_id != scope.nodeId || result.session_id != scope.providerSessionId || result.coverage != "retained_only" || result.events == null || result.events.any { it.id.isBlank() || it.sequence.isBlank() }) throw IOException("Unverified archive event scope.")
        return result
    }
}
