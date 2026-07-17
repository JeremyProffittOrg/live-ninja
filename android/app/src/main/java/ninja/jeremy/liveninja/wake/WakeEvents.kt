package ninja.jeremy.liveninja.wake

import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.asSharedFlow
import ninja.jeremy.liveninja.audio.WakeWordDetection

/**
 * App-wide fan-out of wake detections. [WakeWordService] forwards every engine detection here;
 * the realtime/session layer collects [detections] to open a GPT-Realtime call on wake without
 * binding to the service or caring which [ninja.jeremy.liveninja.audio.WakeWordEngine] fired.
 */
@Singleton
class WakeEvents @Inject constructor() {
    private val _detections = MutableSharedFlow<WakeWordDetection>(extraBufferCapacity = 8)
    val detections: SharedFlow<WakeWordDetection> = _detections.asSharedFlow()

    internal fun emit(detection: WakeWordDetection) {
        _detections.tryEmit(detection)
    }
}
