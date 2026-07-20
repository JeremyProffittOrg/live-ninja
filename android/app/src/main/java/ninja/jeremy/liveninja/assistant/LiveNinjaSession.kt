package ninja.jeremy.liveninja.assistant

import android.app.KeyguardManager
import android.content.Context
import android.content.Intent
import android.os.Bundle
import android.service.voice.VoiceInteractionSession
import ninja.jeremy.liveninja.MainActivity
import ninja.jeremy.liveninja.log.LNLog
import ninja.jeremy.liveninja.log.LogCategory

/**
 * Live Ninja's assistant session (plan.md M4, Android §2).
 *
 * We render no session overlay of our own — the full conversation experience
 * lives in [MainActivity]'s Conversation tab. So the session's whole job is:
 *
 *  1. note whether the keyguard is locked (locked launches must gate sensitive
 *     tools behind [KeyguardGate] downstream),
 *  2. start [MainActivity] with [ACTION_ASSIST] via [startAssistantActivity]
 *     (which is allowed to launch over the keyguard, unlike a plain
 *     `startActivity` from a background service), and
 *  3. finish immediately so no empty system overlay lingers.
 *
 * MainActivity turns the intent into an [AssistTrigger] on [AssistantEvents];
 * `LiveNinjaRoot` navigates to the Conversation tab and the realtime layer
 * starts the WebRTC session from the same trigger.
 */
class LiveNinjaSession(context: Context) : VoiceInteractionSession(context) {

    override fun onCreate() {
        super.onCreate()
        // No session UI: everything happens in the activity we launch.
        setUiEnabled(false)
    }

    override fun onShow(args: Bundle?, showFlags: Int) {
        super.onShow(args, showFlags)
        val keyguard = context.getSystemService(Context.KEYGUARD_SERVICE) as KeyguardManager
        val locked = keyguard.isKeyguardLocked
        LNLog.i(LogCategory.GENERAL, TAG, "Assist session shown (flags=$showFlags, locked=$locked) — launching conversation UI")
        val intent = Intent(context, MainActivity::class.java).apply {
            action = ACTION_ASSIST
            putExtra(EXTRA_LAUNCHED_WHILE_LOCKED, locked)
            putExtra(EXTRA_SOURCE, AssistSource.VOICE_INTERACTION.name)
            addFlags(Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_SINGLE_TOP)
        }
        try {
            startAssistantActivity(intent)
        } catch (e: RuntimeException) {
            // Extremely rare (session torn down mid-show); nothing sane to do
            // beyond logging — the user can relaunch via icon or wake word.
            LNLog.e(LogCategory.GENERAL, TAG, "Failed to start assistant activity", e)
        }
        finish()
    }

    companion object {
        private const val TAG = "LiveNinjaSession"

        /** Intent action MainActivity treats as "assistant invoked — start a realtime session". */
        const val ACTION_ASSIST = "ninja.jeremy.liveninja.action.ASSIST"

        /** Boolean extra: keyguard was locked when the session launched. */
        const val EXTRA_LAUNCHED_WHILE_LOCKED = "ninja.jeremy.liveninja.extra.LAUNCHED_WHILE_LOCKED"

        /** String extra: [AssistSource] name that raised the session. */
        const val EXTRA_SOURCE = "ninja.jeremy.liveninja.extra.SOURCE"
    }
}
