package ninja.jeremy.liveninja.assistant

import android.content.Intent
import android.os.RemoteException
import android.speech.RecognitionService
import android.speech.SpeechRecognizer
import ninja.jeremy.liveninja.log.LNLog
import ninja.jeremy.liveninja.log.LogCategory

/**
 * Minimal-but-complete [RecognitionService] (plan.md M4, Android §2).
 *
 * A voice-interaction service MUST reference a recognition service in its
 * `interaction_service.xml` metadata or the platform refuses to offer the app
 * as a digital assistant. Live Ninja does all speech understanding inside the
 * OpenAI Realtime WebRTC session, so this recognizer intentionally does not
 * offer general-purpose transcription to third-party apps: every request is
 * declined promptly and cleanly with [SpeechRecognizer.ERROR_CLIENT] so callers
 * fail fast instead of hanging on a recognizer that will never produce results.
 * That is this service's full, real contract — not a placeholder for a future
 * transcriber.
 */
class LiveNinjaRecognitionService : RecognitionService() {

    override fun onStartListening(recognizerIntent: Intent?, listener: Callback?) {
        LNLog.i(LogCategory.GENERAL, TAG, "Declining external recognition request (realtime-session-only recognizer)")
        listener?.safeError(SpeechRecognizer.ERROR_CLIENT)
    }

    override fun onCancel(listener: Callback?) {
        // Nothing is ever in flight (requests are declined synchronously),
        // but acknowledge per the RecognitionService contract.
        listener?.safeError(SpeechRecognizer.ERROR_CLIENT)
    }

    override fun onStopListening(listener: Callback?) {
        listener?.safeError(SpeechRecognizer.ERROR_CLIENT)
    }

    private fun Callback.safeError(code: Int) {
        try {
            error(code)
        } catch (e: RemoteException) {
            LNLog.w(LogCategory.GENERAL, TAG, "Caller died before recognition error could be delivered", e)
        }
    }

    private companion object {
        const val TAG = "LiveNinjaRecognition"
    }
}
