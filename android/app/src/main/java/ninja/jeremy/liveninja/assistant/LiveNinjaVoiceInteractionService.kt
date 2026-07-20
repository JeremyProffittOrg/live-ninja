package ninja.jeremy.liveninja.assistant

import android.content.ComponentName
import android.content.Context
import android.service.voice.VoiceInteractionService
import ninja.jeremy.liveninja.log.LNLog
import ninja.jeremy.liveninja.log.LogCategory

/**
 * Live Ninja's [VoiceInteractionService] — the anchor component for
 * `ROLE_ASSISTANT` (plan.md M4, Android §2).
 *
 * The system binds this service while Live Ninja is the device's default
 * digital assistant. Sessions themselves are created by [LiveNinjaSessionService]
 * (declared via `res/xml/interaction_service.xml`); this class's job is the
 * service lifecycle plus a couple of static helpers other layers use to know
 * whether the role is actually active.
 *
 * Continuous "Hey Live Ninja" detection deliberately does NOT run here: it runs
 * in the app's own microphone foreground service so wake detection keeps working
 * even when the assistant role is held by another app (Definition of Done:
 * "works even without it"). When the role IS held, the system additionally
 * routes assist gestures (long-press home / power-button assist) to us, which
 * shows a [LiveNinjaSession].
 */
class LiveNinjaVoiceInteractionService : VoiceInteractionService() {

    override fun onReady() {
        super.onReady()
        LNLog.i(LogCategory.GENERAL, TAG, "Voice interaction service ready (assistant role active)")
    }

    override fun onShutdown() {
        LNLog.i(LogCategory.GENERAL, TAG, "Voice interaction service shut down (assistant role released)")
        super.onShutdown()
    }

    companion object {
        private const val TAG = "LiveNinjaVIS"

        /** Component name of this service, used in role checks. */
        fun componentName(context: Context): ComponentName =
            ComponentName(context, LiveNinjaVoiceInteractionService::class.java)

        /**
         * True when the platform currently treats this service as the active
         * voice-interaction service. Secondary signal alongside
         * `RoleManager.isRoleHeld` — some OEM builds update this before the
         * role state is observable.
         */
        fun isActive(context: Context): Boolean =
            VoiceInteractionService.isActiveService(context, componentName(context))
    }
}
