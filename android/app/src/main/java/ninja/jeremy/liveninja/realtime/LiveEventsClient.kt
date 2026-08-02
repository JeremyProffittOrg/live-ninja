package ninja.jeremy.liveninja.realtime

import android.os.SystemClock
import java.security.SecureRandom
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.delay
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import ninja.jeremy.liveninja.log.LNLog
import ninja.jeremy.liveninja.log.LogCategory
import ninja.jeremy.liveninja.net.IotCredentials
import ninja.jeremy.liveninja.net.LiveNinjaApi
import ninja.jeremy.liveninja.ui.settings.SettingsViewModel
import ninja.jeremy.liveninja.ui.state.SettingsStore
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import okio.ByteString
import org.json.JSONObject

/**
 * Cross-device change notifications, presence and the turn-taking lock over
 * MQTT (plan.md §6 WS-4, WS-5).
 *
 * The Android half of what liveevents.mjs does on web: subscribe to this
 * user's event topic so a document, memory entity or plan changed on another
 * device is known here immediately.
 *
 * Built on OkHttp's WebSocket plus [MqttCodec] rather than the AWS IoT SDK —
 * see MqttCodec's own note on why another native dependency is the wrong trade
 * for this device family.
 *
 * Three rules this class exists to hold:
 *  - **It never reports the user's own edits.** The server stamps each event
 *    with the device that caused it, and the credential response tells this
 *    client what that value will be for itself.
 *  - **It is not always-on.** The connection is opened while the app is
 *    actually in use, or while a session is live, and closed otherwise.
 *    Samsung's One UI kills long-lived background sockets anyway, and the
 *    reconnect churn costs more battery than the notification latency is worth.
 *  - **Only one device answers an unprompted change.** Three signed-in
 *    surfaces learn about an edit in the same millisecond; [claimSpeakingTurn]
 *    is what stops all three saying so at once.
 *
 * Everything on the wire is subscribed through the single `topicFilter` the
 * credential response carries. A narrower SUBSCRIBE would be refused by the
 * authorizer, and AWS IoT signals a refused SUBSCRIBE by closing the socket —
 * which [scheduleReconnect] would turn into a silent reconnect loop, not an
 * error anybody could see.
 */
@Singleton
class LiveEventsClient @Inject constructor(
    private val api: LiveNinjaApi,
    private val http: OkHttpClient,
    private val settingsStore: SettingsStore,
) {
    /** One change another device made. */
    data class Change(
        val type: String,
        val id: String,
        val actorDeviceId: String,
        val actorPersona: String,
        val summary: String,
    )

    /**
     * One of this user's signed-in devices, as it last described itself.
     *
     * Includes THIS device: the `#` subscription delivers our own retained
     * presence straight back, and a caller that wants "the others" filters on
     * [clientId] rather than this class guessing which it wanted.
     */
    data class Peer(
        val deviceId: String,
        val actorDeviceId: String,
        val persona: String,
        val state: String,
    )

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    private var socket: WebSocket? = null
    private var job: Job? = null
    private var reader = MqttCodec.Reader()
    private var creds: IotCredentials? = null
    private var pingJob: Job? = null
    private var running = false
    private val random = SecureRandom()

    private val _changes = MutableStateFlow<Change?>(null)

    /** Latest change from ANOTHER device; null until one arrives. */
    val changes: StateFlow<Change?> = _changes

    private val _peers = MutableStateFlow<List<Peer>>(emptyList())

    /** Every device of this account currently publishing presence (§6 WS-5 M5.1). */
    val peers: StateFlow<List<Peer>> = _peers

    /**
     * Packets seen on the speaking topic.
     *
     * A SharedFlow, deliberately, where [changes] is a StateFlow: a StateFlow
     * drops a value equal to the one it already holds, and back-to-back
     * releases — or a re-claim by the same holder — are real events rather than
     * duplicates. The replay buffer is not a convenience either: arbitration
     * reads it, and subscribing only after publishing a claim would race the
     * broker's echo of that very claim.
     */
    private val _speaking =
        MutableSharedFlow<SpeakingSignal>(replay = SPEAKING_REPLAY, extraBufferCapacity = 64)

    internal val speaking: SharedFlow<SpeakingSignal> = _speaking

    /** This device's MQTT client id — the roster key and lock holder id — or null while offline. */
    val clientId: String? get() = creds?.clientId

    private var presenceState = "idle"
    private var lastPresenceAtMs = 0L
    private var presenceTrailingJob: Job? = null

    /** A claim of ours is published and inside its settle window. */
    private var claiming = false

    /** We won a turn and have not given it back yet. */
    private var holding = false
    private var holdJob: Job? = null

    /**
     * Persona id → display name from `GET /api/v1/realtime/personas`, empty
     * until the fetch lands. See [personaLabel] for why the presence payload
     * cannot be resolved from the offline preset list.
     */
    private var personaNames: Map<String, String> = emptyMap()

    init {
        // Presence carries the persona label, and the persona is changed in
        // Settings while a session is live. A roster that still names the
        // persona a device stopped running is worse than no roster at all.
        scope.launch {
            settingsStore.document
                .map { it.personaPresetId }
                .distinctUntilChanged()
                .collect { publishPresence() }
        }
    }

    /** Idempotent: a second start() while connected does nothing. */
    fun start() {
        if (running) return
        running = true
        job = scope.launch { connect() }
    }

    /** Clears presence deliberately, so peers see a departure rather than a crash. */
    fun stop() {
        running = false
        pingJob?.cancel()
        presenceTrailingJob?.cancel()
        // Give the lock back before the socket goes: a holder that just vanishes
        // costs every other device the full 30s expiry before any of them speaks.
        releaseSpeakingTurn()
        job?.cancel()
        creds?.let { c ->
            runCatching {
                socket?.send(ByteString.of(*MqttCodec.encodePublish(c.presenceTopic, "", retain = true)))
                socket?.send(ByteString.of(*MqttCodec.encodeDisconnect()))
            }
        }
        socket?.close(1000, null)
        socket = null
        creds = null
        // The roster is only true while we are receiving updates for it.
        _peers.value = emptyList()
    }

    /**
     * What this device tells its peers it is doing: one of `idle`,
     * `connecting`, `listening`, `thinking` or `speaking` (§6 WS-5 M5.1).
     *
     * Pushed in rather than read out of the conversation layer. This class is a
     * `@Singleton` and the ViewModel that knows the mic state dies with the
     * Activity, so a screen-off wake session — the device most likely to speak
     * unprompted — has to be able to keep the roster honest without one.
     */
    fun setState(state: String) {
        if (state == presenceState) return
        presenceState = state
        publishPresence()
    }

    /**
     * Claim the right to answer an unprompted change, arbitrate against every
     * other device that claimed at the same instant, and report whether this
     * device won (§6 WS-5 M5.2).
     *
     * Claim, settle for [SpeakingLock.SETTLE_MS], lowest holder wins. The
     * winner is a pure function of the claims observed, so every device reaches
     * the same answer no matter what order QoS 0 delivered them in — "whoever
     * arrived first" would pick a different winner on each device, which is the
     * one outcome that must never happen.
     *
     * Suspends for the settle window. Returns false when another device already
     * holds the lock, when it holds it after the window, or when THIS device is
     * already settling a claim or speaking on one it won; the caller defers.
     */
    suspend fun claimSpeakingTurn(): Boolean {
        val c = creds
        val ws = socket
        if (c == null || ws == null || c.speakingTopic.isEmpty()) {
            // Not on the event stream: signed out, offline, or a server build
            // that predates the lock. There is nobody to collide with AND
            // nobody to hear a claim, so refusing here would delete the feature
            // rather than prevent an overlap.
            return true
        }
        val startedAt = SystemClock.elapsedRealtime()
        val standing = SpeakingLock.lockWinner(SpeakingLock.liveClaims(_speaking.replayCache, startedAt))
        if (!SpeakingLock.mayClaim(claiming, holding, standing, c.clientId)) return false

        val claimId = newClaimId()
        // Record our own claim locally as well as publishing it. The echo comes
        // back through the `#` subscription, but a claim that never reaches the
        // broker would otherwise leave this device out of its own arbitration.
        _speaking.tryEmit(
            SpeakingSignal.Claimed(LockClaim(c.clientId, claimId, SpeakingLock.TTL_MS, startedAt)),
        )
        runCatching {
            ws.send(
                ByteString.of(
                    *MqttCodec.encodePublish(
                        c.speakingTopic,
                        SpeakingLock.claimPayload(c.clientId, claimId),
                    ),
                ),
            )
        }
        // Set across the suspend, not just around the publish: the window this
        // flag closes is precisely the 400ms this device is invisible to its own
        // second caller. Cleared in a `finally` so a cancelled delivery — the
        // ViewModel's lockJob being replaced, or the scope dying — cannot leave
        // the flag stuck true and mute this device for the rest of the session.
        claiming = true
        try {
            delay(SpeakingLock.SETTLE_MS)
        } finally {
            claiming = false
        }

        val settledAt = SystemClock.elapsedRealtime()
        val winner = SpeakingLock.lockWinner(SpeakingLock.liveClaims(_speaking.replayCache, settledAt))
        val won = winner != null && winner.holder == c.clientId && winner.claimId == claimId
        if (won) {
            holding = true
            holdJob?.cancel()
            holdJob = scope.launch {
                // The holder's own expiry is a courtesy — a crashed holder runs
                // nothing, and it is the readers' timers that actually free the
                // lock. It still matters for the case this device stays up but
                // never produces the response it claimed the turn for.
                delay(SpeakingLock.TTL_MS)
                releaseSpeakingTurn()
            }
        }
        return won
    }

    /**
     * Give the lock back. A no-op unless this device is holding it, so it is
     * safe to call from every path that ends a turn.
     */
    fun releaseSpeakingTurn() {
        if (!holding) return
        holding = false
        holdJob?.cancel()
        holdJob = null
        _speaking.tryEmit(SpeakingSignal.Released(SystemClock.elapsedRealtime()))
        val c = creds ?: return
        val ws = socket ?: return
        runCatching {
            ws.send(ByteString.of(*MqttCodec.encodePublish(c.speakingTopic, "")))
        }
    }

    private suspend fun connect() {
        val c = runCatching { api.iotCredentials() }.getOrElse {
            // Signed out, offline, or the feature is not configured. This is a
            // convenience layer: it goes quiet rather than retrying forever.
            LNLog.i(LogCategory.NET, TAG, "iot credentials unavailable; cross-device events off")
            running = false
            return
        }
        if (c.endpoint.isEmpty() || c.token.isEmpty()) {
            running = false
            return
        }
        creds = c
        reader = MqttCodec.Reader()
        // Credentials succeeded, so this device is signed in and the persona
        // catalog is fetchable. Kicked off here rather than at construction for
        // that reason alone.
        refreshPersonaNames()

        // AWS IoT takes the authorizer name from the query string and the token
        // from the MQTT CONNECT user-name field.
        val url = "wss://${c.endpoint}/mqtt?x-amz-customauthorizer-name=${c.authorizerName}"
        val request = Request.Builder()
            .url(url)
            // The subprotocol AWS IoT requires on the handshake.
            .addHeader("Sec-WebSocket-Protocol", "mqtt")
            .build()

        socket = http.newWebSocket(request, object : WebSocketListener() {
            override fun onOpen(webSocket: WebSocket, response: Response) {
                webSocket.send(
                    ByteString.of(
                        *MqttCodec.encodeConnect(
                            clientId = c.clientId,
                            username = c.token,
                            will = MqttCodec.Will(c.presenceTopic),
                        ),
                    ),
                )
            }

            override fun onMessage(webSocket: WebSocket, bytes: ByteString) {
                for (pkt in reader.push(bytes)) onPacket(webSocket, pkt, c)
            }

            override fun onFailure(webSocket: WebSocket, t: Throwable, response: Response?) {
                LNLog.w(LogCategory.NET, TAG, "live events socket failed", t)
                scheduleReconnect()
            }

            override fun onClosed(webSocket: WebSocket, code: Int, reason: String) {
                scheduleReconnect()
            }
        })
    }

    private fun onPacket(ws: WebSocket, pkt: MqttCodec.Packet, c: IotCredentials) {
        when (pkt.type) {
            MqttCodec.CONNACK -> {
                val code = pkt.body.getOrNull(1)?.toInt() ?: -1
                if (code != 0) {
                    LNLog.w(LogCategory.NET, TAG, "live events connection refused (code $code)")
                    running = false
                    ws.close(1000, null)
                    return
                }
                ws.send(ByteString.of(*MqttCodec.encodeSubscribe(1, listOf(c.topicFilter))))
                // A fresh connection owes its peers a presence publish immediately,
                // ahead of any throttle a previous connection left behind.
                lastPresenceAtMs = 0L
                sendPresence(ws, c)
                startPing(ws)
                scheduleRefresh(c)
            }

            MqttCodec.PUBLISH -> {
                val pub = MqttCodec.decodePublish(pkt.body)
                when (LiveTopics.route(pub.topic, c.speakingTopic)) {
                    LiveTopics.Route.SPEAKING ->
                        SpeakingLock.parse(pub.payload, SystemClock.elapsedRealtime())
                            ?.let { _speaking.tryEmit(it) }

                    LiveTopics.Route.PRESENCE ->
                        _peers.update { PresenceRoster.reduce(it, pub.topic, pub.payload) }

                    LiveTopics.Route.CHANGE -> {
                        val json = runCatching { JSONObject(pub.payload) }.getOrNull() ?: return
                        val actor = json.optString("actorDeviceId")
                        // The comparison that stops this device announcing its own edit.
                        if (actor.isNotEmpty() && actor == c.actorDeviceId) return
                        _changes.value = Change(
                            type = json.optString("type"),
                            id = json.optString("id"),
                            actorDeviceId = actor,
                            actorPersona = json.optString("actorPersona"),
                            summary = json.optString("summary"),
                        )
                    }
                }
            }
        }
    }

    /**
     * Republish presence, at most once per [PRESENCE_MIN_INTERVAL_MS] with a
     * guaranteed trailing publish. The trailing half is the important half: the
     * LAST state is the one peers have to end up with, and dropping it leaves a
     * device showing "speaking" on every other screen until it next changes.
     */
    private fun publishPresence() {
        val c = creds ?: return
        val ws = socket ?: return
        val since = SystemClock.elapsedRealtime() - lastPresenceAtMs
        if (since >= PRESENCE_MIN_INTERVAL_MS) {
            presenceTrailingJob?.cancel()
            presenceTrailingJob = null
            sendPresence(ws, c)
            return
        }
        if (presenceTrailingJob?.isActive == true) return
        presenceTrailingJob = scope.launch {
            delay(PRESENCE_MIN_INTERVAL_MS - since)
            val currentCreds = creds ?: return@launch
            val currentSocket = socket ?: return@launch
            sendPresence(currentSocket, currentCreds)
        }
    }

    private fun sendPresence(ws: WebSocket, c: IotCredentials) {
        lastPresenceAtMs = SystemClock.elapsedRealtime()
        runCatching {
            ws.send(
                ByteString.of(
                    *MqttCodec.encodePublish(
                        c.presenceTopic,
                        PresenceRoster.payload(
                            clientId = c.clientId,
                            actorDeviceId = c.actorDeviceId,
                            persona = personaLabel(),
                            state = presenceState,
                        ),
                        retain = true,
                    ),
                ),
            )
        }
    }

    private fun personaLabel(): String = PersonaLabels.label(
        settingsStore.document.value.personaPresetId,
        personaNames,
    )

    /**
     * Fetch the persona catalog once per process, so [personaLabel] has display
     * names to resolve through.
     *
     * Done here rather than read out of SettingsViewModel: that ViewModel dies
     * with the Activity, and the device most likely to publish presence — a
     * screen-off wake session — never builds one.
     *
     * Failure is left alone rather than retried. The label degrades to the
     * persona id, which is what shipped before this existed, and the next
     * connection tries again.
     */
    private fun refreshPersonaNames() {
        if (personaNames.isNotEmpty()) return
        scope.launch {
            val catalog = runCatching { api.listPersonas() }.getOrNull() ?: return@launch
            val names = catalog.personas.mapNotNull { p -> p.name?.let { p.id to it } }.toMap()
            if (names.isEmpty()) return@launch
            personaNames = names
            // This connection's first presence went out before the catalog
            // landed, and presence is retained: without this republish the
            // roster keeps whatever that first payload said until the next
            // state change, which for an idle device is never.
            publishPresence()
        }
    }

    /** 16 lowercase hex characters, fresh per claim — the arbitration tiebreak. */
    private fun newClaimId(): String {
        val bytes = ByteArray(8)
        random.nextBytes(bytes)
        return bytes.joinToString("") { "%02x".format(it) }
    }

    private fun startPing(ws: WebSocket) {
        pingJob?.cancel()
        pingJob = scope.launch {
            // Half the 60s keep-alive: the broker disconnects at 1.5x, so this
            // leaves room for one lost ping without losing the session.
            while (running) {
                delay(30_000)
                runCatching { ws.send(ByteString.of(*MqttCodec.encodePingreq())) }
            }
        }
    }

    private fun scheduleRefresh(c: IotCredentials) {
        scope.launch {
            // Reconnect BEFORE the token expires. Closing routes through the
            // normal reconnect path, so there is one reconnect implementation
            // rather than two.
            delay(maxOf(30_000L, c.expiresInSeconds * 1000L - 60_000L))
            if (running) socket?.close(1000, "token refresh")
        }
    }

    private fun scheduleReconnect() {
        if (!running) return
        scope.launch {
            delay(2_000)
            if (running) connect()
        }
    }

    private companion object {
        const val TAG = "LiveEventsClient"

        /** §1.3: at most one presence publish per second, trailing guaranteed. */
        const val PRESENCE_MIN_INTERVAL_MS = 1_000L

        /**
         * How many speaking-topic packets arbitration can look back over. Only
         * has to cover one settle window plus whatever else a small fleet
         * published in it; the TTL filter discards anything older regardless.
         */
        const val SPEAKING_REPLAY = 16
    }
}

/**
 * Which of the three things a packet arriving on this user's topic tree is.
 *
 * A pure function rather than a `when` buried in the socket callback because
 * the ORDER of the tests is the contract, and an order is invisible in a diff:
 * the lock topic is `liveninja/user/<uid>/presence/speaking`, so it matches the
 * presence test as well as its own. Presence-first would file the fleet's lock
 * as a peer device literally named "speaking" — a phantom row on every roster,
 * and a lock nothing observes, so every device answers every change at once.
 *
 * The lock lives under `/presence/` deliberately: a browser tab left open
 * across the deploy is still running the old module graph, which had no branch
 * for the lock topic at all. It would parse a claim as an event, find no
 * `actorDeviceId` to match itself against, and announce "[Automatic update]
 * Another device just changed something shared" for an edit that never
 * happened — on every claim, until that tab is reloaded. Both old clients
 * ignore anything containing `/presence/`, so the move makes the rollout silent.
 */
internal object LiveTopics {
    enum class Route { SPEAKING, PRESENCE, CHANGE }

    /**
     * [speakingTopic] is [IotCredentials.speakingTopic] — this client never
     * builds that string itself. The trailing `/speaking` match behind it is
     * not belt-and-braces: a credential response from a server that predates
     * the lock carries an empty `speakingTopic`, and without it a claim from an
     * updated peer would be filed as a roster entry (or, before the topic
     * moved, announced as an edit).
     */
    fun route(topic: String, speakingTopic: String): Route = when {
        speakingTopic.isNotEmpty() && topic == speakingTopic -> Route.SPEAKING
        topic.endsWith("/speaking") -> Route.SPEAKING
        topic.contains("/presence/") -> Route.PRESENCE
        else -> Route.CHANGE
    }
}

/** A claim seen on the speaking topic, stamped with the LOCAL time it arrived. */
internal data class LockClaim(
    val holder: String,
    val claimId: String,
    val ttlMs: Long,
    val receivedAtMs: Long,
)

/** What a packet on the speaking topic meant. */
internal sealed interface SpeakingSignal {
    data class Claimed(val claim: LockClaim) : SpeakingSignal

    /** A zero-length payload: whoever was holding the lock is done with it. */
    data class Released(val atMs: Long) : SpeakingSignal
}

/**
 * The turn-taking lock, as pure functions (§6 WS-5 M5.2).
 *
 * Expiry is measured on each receiver's own clock from the moment a claim
 * arrives, and there is deliberately no timestamp on the wire. An `expiresAt`
 * epoch would only mean anything if every device agreed on "now", and a tablet
 * with a wrong date — a device class this repo already carries workarounds for —
 * would publish claims that are either already expired (the lock never binds)
 * or effectively permanent (one device mutes the fleet). Neither failure is
 * visible in the payload. "For the next 30 seconds" is a duration, and every
 * device can measure a duration without trusting anybody.
 *
 * Nothing here touches a socket or a clock of its own, so all of it is testable
 * on the JVM — same bargain [MqttCodec] makes.
 */
internal object SpeakingLock {
    /** Always this on the wire; receivers clamp what they read. */
    const val TTL_MS = 30_000L

    /**
     * Long enough to comfortably clear a device→broker→device round trip
     * through one regional IoT endpoint (typically 30–120 ms), short enough to
     * stay well under the ~1s at which a spoken interjection stops feeling
     * connected to the thing that caused it.
     */
    const val SETTLE_MS = 400L

    private const val MIN_TTL_MS = 1_000L

    fun clampTtl(ttlMs: Long): Long = ttlMs.coerceIn(MIN_TTL_MS, TTL_MS)

    /**
     * Whether a fresh claim may be published at all (mirrors web's
     * `claimTurn`, which opens `if (claiming || holding) return false`).
     *
     * [claiming] and [holding] are two distinct states and both have to be
     * here. Arbitration is keyed by holder, so a second claim from this device
     * replaces the first and then wins against it — this device beats itself,
     * and the caller gets two turns for what the user experienced as one
     * moment. Worse on the [holding] side: the release published when the first
     * response ends is a fleet-wide zero-length payload, so it would free the
     * lock for everybody while this device is still mid-way through the second.
     *
     * [standing] is the currently binding claim, if any. Our own is not a
     * blocker — with [claiming] and [holding] both false it can only be a stale
     * echo of a turn already finished — but anybody else's is.
     */
    fun mayClaim(claiming: Boolean, holding: Boolean, standing: LockClaim?, self: String): Boolean =
        !claiming && !holding && (standing == null || standing.holder == self)

    fun claimPayload(holder: String, claimId: String): String =
        JSONObject()
            .put("holder", holder)
            .put("claimId", claimId)
            .put("ttlMs", TTL_MS)
            .toString()

    /** Null for a payload that is neither a release nor a usable claim. */
    fun parse(payload: String, receivedAtMs: Long): SpeakingSignal? {
        if (payload.isEmpty()) return SpeakingSignal.Released(receivedAtMs)
        val json = runCatching { JSONObject(payload) }.getOrNull() ?: return null
        val holder = json.optString("holder")
        val claimId = json.optString("claimId")
        if (holder.isEmpty() || claimId.isEmpty()) return null
        return SpeakingSignal.Claimed(
            LockClaim(
                holder = holder,
                claimId = claimId,
                ttlMs = clampTtl(json.optLong("ttlMs", TTL_MS)),
                receivedAtMs = receivedAtMs,
            ),
        )
    }

    /**
     * The claims still binding at [nowMs]. A release frees the whole lock
     * rather than one holder's share of it — there is one lock on one topic,
     * and an empty payload is the only thing that can be said about it.
     */
    fun liveClaims(signals: List<SpeakingSignal>, nowMs: Long): List<LockClaim> {
        val held = LinkedHashMap<String, LockClaim>()
        for (signal in signals) when (signal) {
            is SpeakingSignal.Claimed -> held[signal.claim.holder] = signal.claim
            is SpeakingSignal.Released -> held.clear()
        }
        return held.values.filter { nowMs - it.receivedAtMs < it.ttlMs }
    }

    /**
     * Lowest holder wins, ties broken by the lowest claim id. Deliberately a
     * total order over the payloads and nothing else: every receiver has to
     * reach the same answer from a set of packets QoS 0 delivered to each of
     * them in a different order.
     */
    fun lockWinner(claims: List<LockClaim>): LockClaim? =
        claims.minWithOrNull(compareBy({ it.holder }, { it.claimId }))
}

/**
 * What goes in the presence payload's `persona` field.
 *
 * The rule is parity with web's `personaLabelFor` (conversation.mjs): a human
 * display NAME, resolved through the catalog the server serves at
 * `GET /api/v1/realtime/personas`. This used to resolve through
 * [SettingsViewModel.PERSONA_PRESETS] and fall back to the id, which is wrong
 * in the ordinary case rather than the exotic one — that list is a deliberate
 * two-entry offline fallback (`default`, `custom`) and the real catalog is
 * server-side, so a device running `pirate-captain` published the slug and the
 * web roster rendered "pirate-captain · Listening" next to properly named rows.
 */
internal object PersonaLabels {
    /** The preset every account has, and the one the server falls back to. */
    const val DEFAULT_ID = "default"

    /**
     * [catalog] is persona id → display name; empty before the fetch lands, or
     * on a server that serves no catalog.
     *
     * `default` publishes the EMPTY string, exactly as web does. Both rosters
     * substitute the plain "Live Ninja" label for an empty persona at render
     * time (conversation.mjs renderPeers, ConversationScreen), and doing the
     * substitution on the wire instead would put a name there the user never
     * chose — indistinguishable, on the receiving side, from a device actually
     * running a persona called "Live Ninja".
     */
    fun label(id: String, catalog: Map<String, String>): String {
        if (id.isEmpty() || id == DEFAULT_ID) return ""
        return catalog[id]
            // `custom` is a client-side concept the schema defines and the
            // server catalog therefore never lists; the preset list is where
            // its label lives.
            ?: SettingsViewModel.PERSONA_PRESETS.firstOrNull { it.id == id }?.label
            // A persona the catalog does not know. The id is a poor label but
            // it is honest, and it is what web falls back to as well.
            ?: id
    }
}

/**
 * The device roster (§6 WS-5 M5.1), as pure functions.
 *
 * Retained per device and self-clearing through the Last Will, so an empty
 * payload is the only "gone" signal there is — and it arrives both from the
 * broker on an unclean death and from [LiveEventsClient.stop] on a clean one.
 */
internal object PresenceRoster {
    /**
     * The five values a device may publish. A normalised vocabulary rather than
     * either client's enum: web's MicState has nine values and Android's
     * MicUiState has seven and no `thinking` at all, so putting a raw enum on
     * the wire would make the roster untranslatable between them.
     */
    val STATES = setOf("idle", "connecting", "listening", "thinking", "speaking")

    fun payload(clientId: String, actorDeviceId: String, persona: String, state: String): String =
        JSONObject()
            // The MQTT client id, never actorDeviceId: this is the roster key,
            // and it has to be the same string as the presence topic's last
            // segment or an entry can never be matched to the device that wrote it.
            .put("deviceId", clientId)
            .put("actorDeviceId", actorDeviceId)
            .put("persona", persona)
            .put("state", normalizeState(state))
            .toString()

    /**
     * Apply one presence packet. Keyed by the TOPIC's last segment, never by
     * the body's `deviceId`: the topic is what the authorizer scoped and what
     * the Last Will will clear, so a body that disagrees with it is a body that
     * would otherwise leave an entry nothing can ever remove.
     */
    fun reduce(peers: List<LiveEventsClient.Peer>, topic: String, payload: String): List<LiveEventsClient.Peer> {
        val key = topic.substringAfterLast('/')
        if (key.isEmpty()) return peers
        if (payload.isEmpty()) return peers.filterNot { it.deviceId == key }
        val json = runCatching { JSONObject(payload) }.getOrNull() ?: return peers
        val next = LiveEventsClient.Peer(
            deviceId = key,
            actorDeviceId = json.optString("actorDeviceId"),
            persona = json.optString("persona"),
            state = normalizeState(json.optString("state")),
        )
        // Replace in place rather than append: a roster that reshuffles every
        // time a device starts speaking is unreadable.
        return if (peers.any { it.deviceId == key }) {
            peers.map { if (it.deviceId == key) next else it }
        } else {
            peers + next
        }
    }

    private fun normalizeState(state: String): String = if (state in STATES) state else "idle"
}

/**
 * How several held changes become one thing to say (§6 WS-5).
 *
 * Pure functions over [LiveEventsClient.Change] rather than methods on the
 * conversation ViewModel, so the merge — the part with the actual rules in it —
 * is unit-testable without an Activity.
 */
internal object NudgeMerge {
    /**
     * How many changes a device holds before the oldest is dropped. It used to
     * be one, silently: a second change overwrote the first and the user was
     * never told about either. Bounded because an unbounded backlog of edits
     * nobody has heard about is its own bug.
     */
    const val CAP = 5

    /** How many get named individually before the rest become a count. */
    private const val MENTIONED = 3

    /** One entry per thing changed, keeping what it last looked like. */
    fun dedupe(changes: List<LiveEventsClient.Change>): List<LiveEventsClient.Change> {
        val byThing = LinkedHashMap<String, LiveEventsClient.Change>()
        for (change in changes) byThing["${change.type}\u0000${change.id}"] = change
        return byThing.values.toList()
    }

    /**
     * The injected turn. One `sendUserText` call is one turn, so everything
     * held collapses into this single prompt rather than one interruption each.
     */
    fun prompt(changes: List<LiveEventsClient.Change>): String {
        val merged = dedupe(changes)
        if (merged.isEmpty()) return ""
        val body = clauses(merged) { "${who(it)} just ${what(it)}" }
        return if (merged.size <= 1) {
            "[Automatic update] $body. Mention this to the user in one short " +
                "sentence, then carry on with what you were doing. Re-read the shared file or " +
                "memory before saying anything about its contents."
        } else {
            "[Automatic update] $body. Mention these to the user in ONE short sentence " +
                "covering all of them, then carry on with what you were doing. Re-read the " +
                "shared files or memories before saying anything about their contents."
        }
    }

    /** The on-screen wording, used when there is no session to speak through. */
    fun notice(changes: List<LiveEventsClient.Change>): String {
        val merged = dedupe(changes)
        if (merged.isEmpty()) return ""
        return clauses(merged) { "${who(it)} ${what(it)}" } + "."
    }

    private fun clauses(
        merged: List<LiveEventsClient.Change>,
        render: (LiveEventsClient.Change) -> String,
    ): String {
        val named = merged.take(MENTIONED)
        val rest = merged.size - named.size
        val tail = when {
            rest <= 0 -> ""
            rest == 1 -> ", and 1 other change"
            else -> ", and $rest other changes"
        }
        return named.joinToString("; ", transform = render) + tail
    }

    private fun who(change: LiveEventsClient.Change): String =
        change.actorPersona.ifEmpty { "Another device" }

    private fun what(change: LiveEventsClient.Change): String =
        change.summary.ifEmpty { "changed something shared" }
}
