package ninja.jeremy.liveninja.wake.porcupine

import ai.picovoice.porcupine.Porcupine
import android.Manifest
import android.content.Context
import android.content.pm.PackageManager
import android.media.AudioFormat
import android.media.AudioRecord
import android.media.MediaRecorder
import android.os.Process
import android.os.SystemClock
import android.util.Log
import androidx.core.content.ContextCompat
import dagger.hilt.android.qualifiers.ApplicationContext
import java.util.concurrent.Executors
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.asCoroutineDispatcher
import kotlinx.coroutines.cancelAndJoin
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import ninja.jeremy.liveninja.audio.WakeWordDetection
import ninja.jeremy.liveninja.audio.WakeWordEngine
import ninja.jeremy.liveninja.wake.EnergyVad
import ninja.jeremy.liveninja.wake.ModelManager
import ninja.jeremy.liveninja.wake.WakeModelRef
import ninja.jeremy.liveninja.wake.WakePreferences

/**
 * Optional Picovoice Porcupine [WakeWordEngine] (plan.md M4, Android §3.1 "Porcupine optional").
 *
 * COMPILED OUT by default: this whole source set (`src/porcupine/`) plus the
 * `ai.picovoice:porcupine-android` dependency only join the build when
 * `-Pliveninja.porcupine=true` is set (see app/build.gradle.kts), because Porcupine requires a
 * per-user Picovoice AccessKey and ships proprietary native libs. [PorcupineModule] contributes
 * the `@StringKey("porcupine")` entry to the same engine multibinding map the service consumes,
 * so no `main` source changes when toggling it.
 *
 * The `.ppn` keyword file comes from the backend wake-word manifest via [ModelManager]
 * (engine `porcupine`, format `ppn-android-v1`, SHA-256 verified). The AccessKey comes from
 * [WakePreferences.porcupineAccessKey] (progressive-disclosure settings field).
 */
@Singleton
class PorcupineWakeWordEngine @Inject constructor(
    @ApplicationContext private val context: Context,
    private val modelManager: ModelManager,
    private val prefs: WakePreferences,
) : WakeWordEngine {

    companion object {
        private const val TAG = "PorcupineEngine"
        private const val REFRACTORY_MS = 2_500L
    }

    private val _detections = MutableSharedFlow<WakeWordDetection>(extraBufferCapacity = 8)
    override val detections: Flow<WakeWordDetection> = _detections

    @Volatile
    override var isRunning: Boolean = false
        private set

    private val lifecycleMutex = Mutex()
    private val captureDispatcher = Executors.newSingleThreadExecutor { r ->
        Thread(r, "porcupine-capture").apply { isDaemon = true }
    }.asCoroutineDispatcher()
    private val scope = CoroutineScope(SupervisorJob() + captureDispatcher)

    private var captureJob: Job? = null
    private var porcupine: Porcupine? = null
    private var audioRecord: AudioRecord? = null
    private var phrase: String = "porcupine"

    override suspend fun start(): Unit = lifecycleMutex.withLock {
        if (isRunning) return
        cleanupLocked() // release leftovers from a capture loop that died uncleanly
        check(
            ContextCompat.checkSelfPermission(context, Manifest.permission.RECORD_AUDIO) ==
                PackageManager.PERMISSION_GRANTED,
        ) { "RECORD_AUDIO permission not granted" }

        val accessKey = prefs.porcupineAccessKey
            ?: error("Porcupine AccessKey not configured (Settings → wake engine → Porcupine)")
        val model = modelManager.activeModel(WakePreferences.ENGINE_PORCUPINE)
            as? WakeModelRef.Downloaded
            ?: error("No verified Porcupine keyword model downloaded yet — sign in and select a Porcupine wake word")
        phrase = model.wakeWordId.replace('-', ' ')

        val pp = Porcupine.Builder()
            .setAccessKey(accessKey)
            .setKeywordPath(model.file.absolutePath)
            .setSensitivity(prefs.sensitivityFlow.value.coerceIn(0f, 1f))
            .build(context)

        val frameLength = pp.frameLength // 512 samples @ 16 kHz
        val minBuf = AudioRecord.getMinBufferSize(
            pp.sampleRate,
            AudioFormat.CHANNEL_IN_MONO,
            AudioFormat.ENCODING_PCM_16BIT,
        )
        val record = AudioRecord(
            MediaRecorder.AudioSource.VOICE_RECOGNITION,
            pp.sampleRate,
            AudioFormat.CHANNEL_IN_MONO,
            AudioFormat.ENCODING_PCM_16BIT,
            maxOf(minBuf, frameLength * 2 * 8),
        )
        if (record.state != AudioRecord.STATE_INITIALIZED) {
            record.release()
            pp.delete()
            error("AudioRecord failed to initialize (mic busy or restricted)")
        }
        record.startRecording()
        if (record.recordingState != AudioRecord.RECORDSTATE_RECORDING) {
            record.release()
            pp.delete()
            error("Microphone capture blocked (background start restriction)")
        }

        porcupine = pp
        audioRecord = record
        isRunning = true
        captureJob = scope.launch { captureLoop(record, pp, frameLength) }
        Log.i(TAG, "started (keyword=${model.wakeWordId})")
    }

    override suspend fun stop(): Unit = lifecycleMutex.withLock {
        if (!isRunning && captureJob == null) return
        cleanupLocked()
        Log.i(TAG, "stopped")
    }

    /** Tear down job, mic, and Porcupine handle. Caller must hold [lifecycleMutex]. */
    private suspend fun cleanupLocked() {
        isRunning = false
        captureJob?.cancelAndJoin()
        captureJob = null
        audioRecord?.let { rec ->
            runCatching { rec.stop() }
            rec.release()
        }
        audioRecord = null
        porcupine?.delete()
        porcupine = null
    }

    private fun captureLoop(record: AudioRecord, pp: Porcupine, frameLength: Int) {
        Process.setThreadPriority(Process.THREAD_PRIORITY_AUDIO)
        val vad = EnergyVad()
        val frame = ShortArray(frameLength)
        var refractoryUntil = 0L

        while (scope.isActive && isRunning) {
            var offset = 0
            while (offset < frame.size && isRunning) {
                val n = record.read(frame, offset, frame.size - offset, AudioRecord.READ_BLOCKING)
                if (n < 0) {
                    Log.w(TAG, "AudioRecord.read error $n; capture loop dead")
                    isRunning = false
                    return
                }
                offset += n
            }
            if (!isRunning) return

            val now = SystemClock.elapsedRealtime()
            if (now < refractoryUntil) continue
            // Porcupine keeps internal streaming state, so unlike the oWW pipeline we cannot
            // skip frames while audio flows — but its per-frame cost is small. The gate still
            // saves work by only *checking* results during energetic audio.
            vad.accept(frame, now)

            val keywordIndex = try {
                pp.process(frame)
            } catch (e: Exception) {
                Log.e(TAG, "porcupine process failed", e)
                continue
            }
            if (keywordIndex >= 0 && vad.isOpen) {
                Log.i(TAG, "wake detected: \"$phrase\"")
                _detections.tryEmit(WakeWordDetection(phrase, 1f, now))
                refractoryUntil = now + REFRACTORY_MS
            }
        }
    }
}
