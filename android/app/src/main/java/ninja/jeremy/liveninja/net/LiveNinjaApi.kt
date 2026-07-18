package ninja.jeremy.liveninja.net

import retrofit2.http.Body
import retrofit2.http.DELETE
import retrofit2.http.GET
import retrofit2.http.POST
import retrofit2.http.PUT
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

    // ---- Memory Layer + Guide Entities (M10, FR-MEM-05/07/09) ----

    /**
     * List memory entities for the memory browser — backend Query on
     * PK=USER#{uid}, SK begins_with ENT# (or ENT#<type># when [type] is set;
     * type ∈ person|place|info|project|task|plan), never Scan. [type] omitted
     * = all types.
     */
    @GET("api/v1/entities")
    suspend fun listEntities(
        @Query("type") type: String? = null,
        @Query("cursor") cursor: String? = null,
        @Query("limit") limit: Int? = null,
    ): EntityListResponse

    /**
     * "Forget" one memory entity — deletes the ENT# item AND its EMB#
     * embedding so it can never be recalled again (FR-MEM-05, both stores).
     */
    @DELETE("api/v1/memory/{id}")
    suspend fun forgetMemory(@Path("id") id: String): MemoryAck

    /**
     * List the caller's Guide Entities (FR-MEM-09). The backend seeds the
     * default "AI is an emerging technology" guide on first list, so this is
     * never empty for a fresh user.
     */
    @GET("api/v1/guides")
    suspend fun listGuides(): GuideListResponse

    /**
     * Create/edit a guide — Android uses it for the enable toggle and
     * priority stepper (full replace semantics, so the whole guide is sent).
     */
    @PUT("api/v1/guides/{id}")
    suspend fun putGuide(@Path("id") id: String, @Body body: GuidePutRequest): GuideDto

    // ---- Conversation history + topics (M11, FR-TOP-04/05) ----

    /**
     * List/filter conversation history (FR-TOP-04) — Query-only server side
     * (CONV# range for date, TREF# per-topic refs for topic, FilterExpression
     * for device; never Scan). [topic] carries comma-joined stable topicIds
     * for the multi-select filter; [from]/[to] are ISO-8601 instants.
     */
    @GET("api/v1/conversations")
    suspend fun listConversations(
        @Query("topic") topic: String? = null,
        @Query("device") device: String? = null,
        @Query("from") from: String? = null,
        @Query("to") to: String? = null,
        @Query("cursor") cursor: String? = null,
        @Query("limit") limit: Int? = null,
    ): ConversationListResponse

    /** Fetch one conversation with its transcript turns. */
    @GET("api/v1/conversations/{id}")
    suspend fun getConversation(@Path("id") id: String): ConversationDetailDto

    /** List the caller's topic taxonomy (populates the topic filter chips). */
    @GET("api/v1/topics")
    suspend fun listTopics(): TopicListResponse

    /** List the caller's registered devices (populates the device filter). */
    @GET("api/v1/devices")
    suspend fun listDevices(): DeviceListResponse
}
