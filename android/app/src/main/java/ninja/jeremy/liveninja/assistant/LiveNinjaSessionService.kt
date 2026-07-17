package ninja.jeremy.liveninja.assistant

import android.os.Bundle
import android.service.voice.VoiceInteractionSession
import android.service.voice.VoiceInteractionSessionService

/**
 * Creates a [LiveNinjaSession] whenever the system requests an assistant
 * session (assist gesture, `showSession`, keyguard voice-assist). Referenced
 * from `res/xml/interaction_service.xml` (`android:sessionService`).
 */
class LiveNinjaSessionService : VoiceInteractionSessionService() {
    override fun onNewSession(args: Bundle?): VoiceInteractionSession = LiveNinjaSession(this)
}
