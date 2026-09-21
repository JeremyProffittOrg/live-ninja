package ninja.jeremy.liveninja.net

import java.net.URLEncoder

/**
 * WebSocket URL for the AWS IoT custom authorizer.
 * The token stays in the MQTT CONNECT user-name field. The signature query
 * parameter is added only when [signingRequired] is true.
 */
fun iotSocketUrl(
    endpoint: String,
    authorizerName: String,
    tokenSignature: String,
    signingRequired: Boolean,
): String {
    val base = "wss://$endpoint/mqtt?x-amz-customauthorizer-name=$authorizerName"
    if (!signingRequired || tokenSignature.isEmpty()) return base
    val sig = URLEncoder.encode(tokenSignature, Charsets.UTF_8.name())
    return "$base&x-amz-customauthorizer-signature=$sig"
}
