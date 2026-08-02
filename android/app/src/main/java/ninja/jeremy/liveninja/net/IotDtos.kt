package ninja.jeremy.liveninja.net

import kotlinx.serialization.Serializable

/**
 * Wire DTO for `GET /api/v1/iot/credentials` (internal/webapp/iot_routes.go).
 *
 * [token] is deliberately NOT the app's access token: it carries a separate
 * audience that only the IoT custom authorizer accepts, so a leaked one can
 * subscribe to its owner's own event stream and nothing else.
 */
@Serializable
data class IotCredentials(
    /** ATS data endpoint host, e.g. `xxxx-ats.iot.us-east-1.amazonaws.com`. */
    val endpoint: String = "",
    /** Custom authorizer name, sent in the connect URL's query string. */
    val authorizerName: String = "",
    /** MQTT client id; must be unique per connection or the two evict each other. */
    val clientId: String = "",
    /**
     * The value the SERVER stamps as `actorDeviceId` on events this client
     * causes. Compared directly rather than derived locally — a locally
     * generated device id is not guaranteed to be the same string, and a
     * mismatch makes a device announce its OWN edits back to the user.
     */
    val actorDeviceId: String = "",
    /** Narrow, short-lived MQTT credential (goes in the CONNECT user-name field). */
    val token: String = "",
    val expiresInSeconds: Int = 900,
    /** Everything this user may subscribe to, supplied so the filter lives in one place. */
    val topicFilter: String = "",
    /** This device's presence slot; also the Last Will topic. */
    val presenceTopic: String = "",
)
