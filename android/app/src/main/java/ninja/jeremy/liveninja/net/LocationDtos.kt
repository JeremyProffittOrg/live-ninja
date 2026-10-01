package ninja.jeremy.liveninja.net

import kotlinx.serialization.Serializable

/**
 * Body of `POST /api/v1/location/current`: the phone's coarse position,
 * shared with the assistant at session start. [accuracyMeters] is omitted
 * when the platform fix carries no accuracy (NetModule's Json sets
 * explicitNulls = false).
 */
@Serializable
data class CurrentLocationReport(
    val latitude: Double,
    val longitude: Double,
    val accuracyMeters: Float? = null,
)
