package ninja.jeremy.liveninja.wake

/**
 * Seam between the wake stack and the auth stack: supplies the current session access JWT for
 * the authenticated wake-word manifest endpoint (`GET /v1/wakeword/{id}/model`, Session JWT per
 * contracts/api.md).
 *
 * Bound as a Hilt OPTIONAL dependency (`@BindsOptionalOf` in [WakeModule]): until the auth
 * feature installs a binding, [ModelManager] simply skips backend model sync and serves the
 * packaged/cached model — genuine offline behavior, not a stub. The auth module provides a
 * binding like:
 *
 * ```
 * @Binds fun bind(impl: SessionTokenProviderImpl): WakeTokenProvider
 * ```
 */
fun interface WakeTokenProvider {
    /** Current valid access JWT, or null when signed out / refresh failed. */
    suspend fun accessToken(): String?
}
