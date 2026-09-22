package ninja.jeremy.liveninja.net

import java.net.URLEncoder

/**
 * WebSocket URL for the AWS IoT custom authorizer.
 * The token stays in the MQTT CONNECT user-name field. When [signingRequired]
 * is true, AWS IoT also requires that same token on the query parameter
 * named token, plus the URL-encoded signature.
 */
fun iotSocketUrl(
    endpoint: String,
    authorizerName: String,
    token: String,
    tokenSignature: String,
    signingRequired: Boolean,
): String {
    val base = "wss://$endpoint/mqtt?x-amz-customauthorizer-name=$authorizerName"
    if (!signingRequired || tokenSignature.isEmpty() || token.isEmpty()) return base
    val sig = URLEncoder.encode(tokenSignature, Charsets.UTF_8.name())
    val tok = URLEncoder.encode(token, Charsets.UTF_8.name())
    return "$base&token=$tok&x-amz-customauthorizer-signature=$sig"
}
