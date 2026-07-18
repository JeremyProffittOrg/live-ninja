package ninja.jeremy.liveninja.net

import retrofit2.http.Body
import retrofit2.http.DELETE
import retrofit2.http.GET
import retrofit2.http.POST
import retrofit2.http.Path
import retrofit2.http.Query

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

    // ---- Deliverables Store (M9, FR-DLV-04/05) ----

    /**
     * List the caller's deliverables — backend Query on PK=USER#{uid},
     * SK begins_with DELIV# (never Scan), paginated via [cursor].
     */
    @GET("api/v1/deliverables")
    suspend fun listDeliverables(
        @Query("cursor") cursor: String? = null,
        @Query("limit") limit: Int? = null,
    ): DeliverableListResponse

    /**
     * Bundle existing deliverables into one ZIP (async zipper Lambda —
     * the resulting deliverable may list as pending until it finishes).
     */
    @POST("api/v1/deliverables/zip")
    suspend fun zipDeliverables(@Body body: DeliverableZipRequest): DeliverableZipResponse

    /** Delete one deliverable (item + S3 object). */
    @DELETE("api/v1/deliverables/{id}")
    suspend fun deleteDeliverable(@Path("id") id: String): DeliverableAck

    // NOTE: GET api/v1/deliverables/{id}/download is intentionally NOT declared
    // here — it answers with a 302 presigned-URL redirect, and OkHttp would
    // otherwise follow it and forward our Authorization header toward S3
    // (which rejects mixed header+query auth). DeliverablesRepository resolves
    // it with a followRedirects(false) client instead.

    // ---- Custom wake words (M6, FR-K03) ----

    /**
     * Create a custom wake-word training job {phrase, engine}. Backend
     * validates length/phonemes/profanity/collision and enforces ≤3/day/user
     * (429) + job concurrency ≤2.
     */
    @POST("api/v1/wakewords")
    suspend fun createWakeWord(@Body body: WakeWordCreateRequest): WakeWordJobDto

    /** Poll a training job: status pending|training|ready|failed. */
    @GET("api/v1/wakewords/{id}")
    suspend fun getWakeWord(@Path("id") id: String): WakeWordJobDto
}
