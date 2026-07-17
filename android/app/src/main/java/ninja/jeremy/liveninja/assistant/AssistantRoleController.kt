package ninja.jeremy.liveninja.assistant

import android.app.role.RoleManager
import android.content.ActivityNotFoundException
import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.os.Build
import android.provider.Settings
import android.util.Log
import dagger.hilt.android.qualifiers.ApplicationContext
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.flow

/**
 * `ROLE_ASSISTANT` acquisition flow (plan.md M4, Android §2).
 *
 * The happy path is [requestRoleIntent] — the system role-request dialog via
 * `RoleManager.createRequestRoleIntent`. Many OEM builds refuse to show that
 * dialog for the assistant role (it is settings-managed there), in which case
 * the launcher result comes back CANCELED immediately; callers should then fall
 * back to [openAssistantSettings], which deep-links the user into the closest
 * "default digital assistant" settings screen for the current OEM
 * ([Build.MANUFACTURER] map: samsung / xiaomi / oneplus / google, plus a
 * generic chain) and rely on [pollRoleHeld] to observe the user completing the
 * switch out-of-band.
 *
 * The app must keep working WITHOUT the role (wake word runs in our own FGS),
 * so every consumer treats [roleHeld] as an enhancement flag, never a gate.
 */
@Singleton
class AssistantRoleController @Inject constructor(
    @ApplicationContext private val context: Context,
) {
    private val roleManager: RoleManager? = context.getSystemService(RoleManager::class.java)

    private val _roleHeld = MutableStateFlow(isRoleHeld())

    /** Latest observed role state; refreshed by [refresh] / [pollRoleHeld]. */
    val roleHeld: StateFlow<Boolean> = _roleHeld.asStateFlow()

    /**
     * Live check: RoleManager is authoritative, with the platform's
     * active-voice-interaction-service state as a secondary signal (some OEM
     * builds flip it before the role becomes observable).
     */
    fun isRoleHeld(): Boolean {
        val byRole = roleManager?.isRoleHeld(RoleManager.ROLE_ASSISTANT) == true
        return byRole || LiveNinjaVoiceInteractionService.isActive(context)
    }

    /** Re-check the role and publish to [roleHeld]. Returns the fresh value. */
    fun refresh(): Boolean = isRoleHeld().also { _roleHeld.value = it }

    /**
     * Intent for the system role-request dialog, or null when the role is
     * unavailable on this device (then go straight to [openAssistantSettings]).
     * Launch with an ActivityResultLauncher; RESULT_OK means granted, an
     * immediate RESULT_CANCELED usually means this OEM blocks the dialog for
     * the assistant role — fall back to [openAssistantSettings].
     */
    fun requestRoleIntent(): Intent? {
        val rm = roleManager ?: return null
        if (!rm.isRoleAvailable(RoleManager.ROLE_ASSISTANT)) {
            Log.w(TAG, "ROLE_ASSISTANT unavailable on this device")
            return null
        }
        return rm.createRequestRoleIntent(RoleManager.ROLE_ASSISTANT)
    }

    /**
     * Poll the role while the acquisition UI is visible (collect from a
     * lifecycle-aware scope; cancellation stops the poll). Emits the current
     * state immediately, then every [intervalMillis], publishing each sample to
     * [roleHeld]. Completes as soon as the role is held or after
     * [timeoutMillis].
     */
    fun pollRoleHeld(
        intervalMillis: Long = 1_000L,
        timeoutMillis: Long = 180_000L,
    ): Flow<Boolean> = flow {
        val deadline = System.nanoTime() + timeoutMillis * 1_000_000
        while (true) {
            val held = refresh()
            emit(held)
            if (held || System.nanoTime() >= deadline) break
            delay(intervalMillis)
        }
    }

    /**
     * OEM-aware candidates for the "choose your digital assistant" settings
     * screen, most specific first. Every candidate is resolve-checked before
     * launch, so unresolvable entries just fall through.
     */
    fun fallbackSettingsIntents(manufacturer: String = Build.MANUFACTURER): List<Intent> {
        val voiceInput = Intent(Settings.ACTION_VOICE_INPUT_SETTINGS)
        val defaultApps = Intent(Settings.ACTION_MANAGE_DEFAULT_APPS_SETTINGS)
        // AOSP-derived settings ship a dedicated "manage assist" screen; on
        // MIUI/ColorOS the generic actions sometimes land on a stripped page,
        // so try the explicit component too.
        val manageAssist = Intent().setComponent(
            ComponentName("com.android.settings", "com.android.settings.Settings\$ManageAssistActivity"),
        )
        val oemOrder = when (manufacturer.lowercase()) {
            // One UI: Settings > Apps > Choose default apps > Digital assistant app.
            "samsung" -> listOf(defaultApps, voiceInput)
            // MIUI/HyperOS buries assistant choice under default apps; the AOSP
            // component is often still reachable when the generic action is not.
            "xiaomi", "redmi", "poco" -> listOf(defaultApps, manageAssist, voiceInput)
            // OxygenOS keeps the stock AOSP voice-input screen.
            "oneplus" -> listOf(voiceInput, defaultApps)
            // Pixel: direct "Digital assistant app" screen.
            "google" -> listOf(voiceInput, manageAssist)
            else -> emptyList()
        }
        // Generic chain always appended: voice input -> default apps -> Settings root.
        return (oemOrder + listOf(voiceInput, defaultApps, manageAssist, Intent(Settings.ACTION_SETTINGS)))
            .distinctBy { it.component?.flattenToString() ?: it.action }
    }

    /**
     * Deep-link the user to the assistant-selection settings. Launches the
     * first resolvable candidate from [fallbackSettingsIntents]; returns the
     * launched intent, or null when nothing resolved (never expected — the
     * Settings root always resolves).
     */
    fun openAssistantSettings(activityContext: Context): Intent? {
        for (candidate in fallbackSettingsIntents()) {
            if (candidate.resolveActivity(context.packageManager) == null) continue
            return try {
                activityContext.startActivity(candidate)
                Log.i(TAG, "Opened assistant settings via ${candidate.component ?: candidate.action}")
                candidate
            } catch (e: ActivityNotFoundException) {
                // Resolved but launch-blocked (OEM lockdown) — try the next one.
                Log.w(TAG, "Assistant settings candidate refused to launch: $candidate", e)
                continue
            } catch (e: SecurityException) {
                Log.w(TAG, "Assistant settings candidate not launchable: $candidate", e)
                continue
            }
        }
        Log.e(TAG, "No assistant settings screen resolvable on this device")
        return null
    }

    private companion object {
        const val TAG = "AssistantRole"
    }
}
