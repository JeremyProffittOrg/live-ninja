package ninja.jeremy.liveninja.net

import retrofit2.http.Body
import retrofit2.http.POST

/**
 * Retrofit service for the Live Ninja backend (https://live.jeremy.ninja).
 *
 * This is the shared backend API surface — other features (realtime
 * bootstrap, settings, wake-word manifests, deliverables) should extend this
 * interface (or add sibling services against the same Retrofit instance from
 * [NetModule]) rather than hand-rolling OkHttp calls. Requests through this
 * service automatically carry `Authorization: Bearer <access JWT>` +
 * `X-LN-Client` headers and get one silent refresh-and-retry on 401
 * (see [AuthInterceptor] / [TokenAuthenticator]).
 *
 * Paths are relative to [ninja.jeremy.liveninja.config.BackendConfig.BASE_URL];
 * auth bootstrap routes live under `/auth/` (public at the authorizer layer),
 * resource routes under `/api/v1/` per contracts/api.md.
 */
interface LiveNinjaApi {

    /** Android Custom-Tabs + PKCE code exchange -> first token grant. */
    @POST("auth/lwa/exchange")
    suspend fun exchangeLwaCode(@Body body: LwaExchangeRequest): TokenGrant

    /**
     * Revoke the current session server-side (identified by the Bearer JWT).
     * Idempotent — an already-gone session still returns ok.
     */
    @POST("auth/logout")
    suspend fun logout(): LogoutAck

    /** Log out everywhere: kills every session + outstanding JWT for the user. */
    @POST("api/v1/auth/logout-all")
    suspend fun logoutAll(): LogoutAck
}
