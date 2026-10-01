package ninja.jeremy.liveninja.location

import android.Manifest
import android.content.Context
import android.content.pm.PackageManager
import android.location.Location
import android.location.LocationManager
import android.os.Build
import android.os.CancellationSignal
import android.os.SystemClock
import androidx.core.content.ContextCompat
import androidx.core.location.LocationManagerCompat
import dagger.hilt.android.qualifiers.ApplicationContext
import java.util.concurrent.Executors
import java.util.concurrent.atomic.AtomicLong
import javax.inject.Inject
import javax.inject.Singleton
import kotlin.coroutines.resume
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.launch
import kotlinx.coroutines.suspendCancellableCoroutine
import kotlinx.coroutines.withTimeoutOrNull
import ninja.jeremy.liveninja.auth.TokenStore
import ninja.jeremy.liveninja.log.LNLog
import ninja.jeremy.liveninja.log.LogCategory
import ninja.jeremy.liveninja.net.CurrentLocationReport
import ninja.jeremy.liveninja.net.LiveNinjaApi

/** Pure throttle: at most one report per [MIN_INTERVAL_MS]. Unit-tested. */
object LocationReportThrottle {
    const val MIN_INTERVAL_MS: Long = 15L * 60 * 1000

    /** [lastReportAtMs] <= 0 means never; a clock that went backwards counts as due. */
    fun isDue(nowMs: Long, lastReportAtMs: Long, intervalMs: Long = MIN_INTERVAL_MS): Boolean {
        if (lastReportAtMs <= 0L) return true
        if (lastReportAtMs > nowMs) return true
        return nowMs - lastReportAtMs >= intervalMs
    }

    /** A cached fix younger than this is used as-is instead of asking for a new one. */
    const val MAX_LAST_KNOWN_AGE_MS: Long = 15L * 60 * 1000
}

/**
 * Sends the phone's coarse location to the backend at the start of a live
 * session, at most once per 15 minutes, only when a location permission is
 * granted. Uses android.location.LocationManager (the app carries no Play
 * Services). Every failure is logged and ignored: location is a hint for the
 * assistant, never a reason for a session to fail.
 */
@Singleton
class LocationReporter @Inject constructor(
    @ApplicationContext private val context: Context,
    private val api: LiveNinjaApi,
    private val tokenStore: TokenStore,
) {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    private val lastAttemptAtMs = AtomicLong(0L)

    /** Fire-and-forget hook for SessionOrchestrator's session-start edge. */
    fun onSessionStarted() {
        val now = System.currentTimeMillis()
        val last = lastAttemptAtMs.get()
        if (!LocationReportThrottle.isDue(now, last)) return
        if (!hasLocationPermission()) return
        if (tokenStore.accessToken() == null) return
        // Claim the slot before the network call so two session starts in a
        // row cannot both report.
        if (!lastAttemptAtMs.compareAndSet(last, now)) return
        scope.launch {
            try {
                val location = currentLocation()
                if (location == null) {
                    LNLog.i(LogCategory.NET, TAG, "no location fix available; skipping report")
                    return@launch
                }
                api.reportCurrentLocation(
                    CurrentLocationReport(
                        latitude = location.latitude,
                        longitude = location.longitude,
                        accuracyMeters = if (location.hasAccuracy()) location.accuracy else null,
                    ),
                )
                LNLog.i(LogCategory.NET, TAG, "location reported (provider=${location.provider})")
            } catch (t: Throwable) {
                LNLog.w(LogCategory.NET, TAG, "location report failed (ignored)", t)
            }
        }
    }

    private fun hasLocationPermission(): Boolean =
        granted(Manifest.permission.ACCESS_COARSE_LOCATION) || granted(Manifest.permission.ACCESS_FINE_LOCATION)

    private fun granted(permission: String): Boolean =
        ContextCompat.checkSelfPermission(context, permission) == PackageManager.PERMISSION_GRANTED

    /** Freshest recent last-known fix, else one current fix (bounded wait). */
    private suspend fun currentLocation(): Location? {
        val lm = context.getSystemService(LocationManager::class.java) ?: return null
        val providers = candidateProviders(lm)
        val recent = providers
            .mapNotNull { provider -> runCatching { lm.getLastKnownLocation(provider) }.getOrNull() }
            .filter { ageMs(it) <= LocationReportThrottle.MAX_LAST_KNOWN_AGE_MS }
            .maxByOrNull { it.elapsedRealtimeNanos }
        if (recent != null) return recent
        val provider = providers.firstOrNull { runCatching { lm.isProviderEnabled(it) }.getOrDefault(false) }
            ?: return null
        return withTimeoutOrNull(CURRENT_FIX_TIMEOUT_MS) { requestCurrent(lm, provider) }
    }

    /** Coarse-first: network (cell/Wi-Fi) is enough and cheapest; GPS only as fallback. */
    private fun candidateProviders(lm: LocationManager): List<String> = buildList {
        if (Build.VERSION.SDK_INT >= 31) add(LocationManager.FUSED_PROVIDER)
        add(LocationManager.NETWORK_PROVIDER)
        if (granted(Manifest.permission.ACCESS_FINE_LOCATION)) add(LocationManager.GPS_PROVIDER)
        add(LocationManager.PASSIVE_PROVIDER)
    }.filter { provider -> runCatching { lm.allProviders.contains(provider) }.getOrDefault(false) }

    private fun ageMs(location: Location): Long =
        (SystemClock.elapsedRealtimeNanos() - location.elapsedRealtimeNanos) / 1_000_000

    @Suppress("MissingPermission") // checked in onSessionStarted; SecurityException is caught by the caller
    private suspend fun requestCurrent(lm: LocationManager, provider: String): Location? =
        suspendCancellableCoroutine { cont ->
            val cancel = CancellationSignal()
            cont.invokeOnCancellation { cancel.cancel() }
            LocationManagerCompat.getCurrentLocation(lm, provider, cancel, executor) { location ->
                if (cont.isActive) cont.resume(location)
            }
        }

    private companion object {
        const val TAG = "LocationReporter"
        const val CURRENT_FIX_TIMEOUT_MS = 10_000L
        val executor = Executors.newSingleThreadExecutor()
    }
}
