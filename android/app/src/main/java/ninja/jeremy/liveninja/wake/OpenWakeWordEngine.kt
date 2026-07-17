package ninja.jeremy.liveninja.wake

import ai.onnxruntime.OnnxTensor
import ai.onnxruntime.OrtEnvironment
import ai.onnxruntime.OrtSession
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
import java.nio.FloatBuffer
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
import kotlinx.coroutines.withContext
import ninja.jeremy.liveninja.audio.WakeWordDetection
import ninja.jeremy.liveninja.audio.WakeWordEngine

/**
 * Default [WakeWordEngine]: the real openWakeWord three-model ONNX pipeline
 * (melspectrogram → speech-embedding → phrase head) on onnxruntime-android, fed by a 16 kHz
 * mono `AudioRecord` behind an [EnergyVad] pre-gate (plan.md M4, Android §3.1/§3.3).
 *
 * Battery posture: the VAD gate means the three ONNX sessions only run while there is
 * speech-level energy (plus hangover); a quiet room costs one RMS loop per 80 ms chunk. All
 * sessions are created with a single intra-op thread — the models are tiny (~3.7 MB total)
 * and single-core inference keeps the scheduler off the big cores.
 *
 * The phrase-head model hot-swaps live: while running, the engine collects
 * [ModelManager.headModel] and atomically replaces the head session when a newly verified
 * model lands (contracts/wakeword-manifest.md "no listening gap" swap rule). Feature models
 * are phrase-independent and always load from packaged assets.
 */
@Singleton
class OpenWakeWordEngine @Inject constructor(
    @ApplicationContext private val context: Context,
    private val modelManager: ModelManager,
    private val prefs: WakePreferences,
) : WakeWordEngine {

    companion object {
        private const val TAG = "OpenWakeWordEngine"

        /** Suppress re-triggers for this long after a detection. */
        private const val REFRACTORY_MS = 2_500L

        /** Chunks of pre-roll replayed into the pipeline when the VAD gate opens. */
        private const val PRE_ROLL_CHUNKS = 3
    }

    private val _detections = MutableSharedFlow<WakeWordDetection>(extraBufferCapacity = 8)
    override val detections: Flow<WakeWordDetection> = _detections

    @Volatile
    override var isRunning: Boolean = false
        private set

    private val lifecycleMutex = Mutex()
    private val captureDispatcher = Executors.newSingleThreadExecutor { r ->
        Thread(r, "oww-capture").apply { isDaemon = true }
    }.asCoroutineDispatcher()
    private val scope = CoroutineScope(SupervisorJob() + captureDispatcher)

    private var captureJob: Job? = null
    private var swapJob: Job? = null
    private var audioRecord: AudioRecord? = null

    // ONNX state — touched from the capture thread; head session swapped via @Volatile.
    private val env: OrtEnvironment get() = OrtEnvironment.getEnvironment()
    private var melSession: OrtSession? = null
    private var embSession: OrtSession? = null

    @Volatile
    private var headSession: OrtSession? = null

    @Volatile
    private var activeModelRef: WakeModelRef? = null

    override suspend fun start(): Unit = lifecycleMutex.withLock {
        if (isRunning) return
        cleanupLocked() // release any leftovers from a capture loop that died uncleanly
        check(
            ContextCompat.checkSelfPermission(context, Manifest.permission.RECORD_AUDIO) ==
                PackageManager.PERMISSION_GRANTED,
        ) { "RECORD_AUDIO permission not granted" }

        withContext(captureDispatcher) { loadSessions() }

        val minBuf = AudioRecord.getMinBufferSize(
            OwwPipeline.SAMPLE_RATE,
            AudioFormat.CHANNEL_IN_MONO,
            AudioFormat.ENCODING_PCM_16BIT,
        )
        val chunkBytes = OwwPipeline.CHUNK_SAMPLES * 2
        val record = AudioRecord(
            MediaRecorder.AudioSource.VOICE_RECOGNITION,
            OwwPipeline.SAMPLE_RATE,
            AudioFormat.CHANNEL_IN_MONO,
            AudioFormat.ENCODING_PCM_16BIT,
            maxOf(minBuf, chunkBytes * 4),
        )
        if (record.state != AudioRecord.STATE_INITIALIZED) {
            record.release()
            closeSessionsQuietly()
            error("AudioRecord failed to initialize (mic busy or restricted)")
        }
        audioRecord = record
        record.startRecording()
        if (record.recordingState != AudioRecord.RECORDSTATE_RECORDING) {
            record.release()
            audioRecord = null
            closeSessionsQuietly()
            // Android 14+/15 blocks background mic starts for FGS-mic launched from boot until
            // the app is next foregrounded — surface as a start failure the service can report.
            error("Microphone capture blocked (background start restriction)")
        }

        isRunning = true
        captureJob = scope.launch { captureLoop(record) }
        swapJob = scope.launch {
            modelManager.headModel.collect { ref ->
                if (ref != activeModelRef) swapHead(ref)
            }
        }
        Log.i(TAG, "started (model=${modelManager.headModel.value.wakeWordId})")
    }

    override suspend fun stop(): Unit = lifecycleMutex.withLock {
        if (!isRunning && captureJob == null) return
        cleanupLocked()
        Log.i(TAG, "stopped")
    }

    /** Tear down jobs, mic, and sessions. Caller must hold [lifecycleMutex]. */
    private suspend fun cleanupLocked() {
        isRunning = false
        captureJob?.cancelAndJoin()
        captureJob = null
        swapJob?.cancelAndJoin()
        swapJob = null
        audioRecord?.let { rec ->
            runCatching { rec.stop() }
            rec.release()
        }
        audioRecord = null
        withContext(captureDispatcher) { closeSessionsQuietly() }
    }

    // ---- capture + inference loop (single dedicated thread) ----

    private fun captureLoop(record: AudioRecord) {
        Process.setThreadPriority(Process.THREAD_PRIORITY_AUDIO)
        val vad = EnergyVad()
        val pipeline = OwwPipeline(::runMelspec, ::runEmbedding, ::runHead)
        pipeline.reset()

        val chunk = ShortArray(OwwPipeline.CHUNK_SAMPLES)
        val preRoll = ArrayDeque<ShortArray>(PRE_ROLL_CHUNKS)
        var refractoryUntil = 0L

        while (scope.isActive && isRunning) {
            var offset = 0
            while (offset < chunk.size && isRunning) {
                val n = record.read(chunk, offset, chunk.size - offset, AudioRecord.READ_BLOCKING)
                if (n < 0) {
                    Log.w(TAG, "AudioRecord.read error $n; capture loop dead")
                    // Mark not-running so the service supervisor notices and restarts us.
                    isRunning = false
                    return
                }
                offset += n
            }
            if (!isRunning) return

            val now = SystemClock.elapsedRealtime()
            if (now < refractoryUntil) continue

            val gateOpen = vad.accept(chunk, now)
            if (!gateOpen) {
                // Keep a short pre-roll so the first phonemes that *triggered* the gate are
                // not lost — they are replayed when the gate opens.
                if (preRoll.size == PRE_ROLL_CHUNKS) preRoll.removeFirst()
                preRoll.addLast(chunk.copyOf())
                continue
            }
            if (vad.gateJustOpened) {
                pipeline.reset()
                for (buffered in preRoll) pipeline.process(buffered)
                preRoll.clear()
            }

            val score = try {
                pipeline.process(chunk)
            } catch (e: Exception) {
                Log.e(TAG, "inference failure; resetting pipeline", e)
                pipeline.reset()
                continue
            }

            val threshold = (1f - prefs.sensitivityFlow.value).coerceIn(0.05f, 0.95f)
            if (score >= threshold) {
                val phrase = (activeModelRef?.wakeWordId ?: ModelManager.DEFAULT_ASSET_WAKE_WORD_ID)
                    .replace('-', ' ')
                Log.i(TAG, "wake detected: \"$phrase\" score=%.3f thr=%.2f".format(score, threshold))
                _detections.tryEmit(WakeWordDetection(phrase, score, now))
                refractoryUntil = now + REFRACTORY_MS
                pipeline.reset()
                vad.reset()
                preRoll.clear()
            }
        }
    }

    // ---- ONNX plumbing ----

    private fun loadSessions() {
        val opts = OrtSession.SessionOptions().apply {
            setIntraOpNumThreads(1)
            setInterOpNumThreads(1)
        }
        melSession = env.createSession(modelManager.readAsset(ModelManager.ASSET_MELSPECTROGRAM), opts)
        embSession = env.createSession(modelManager.readAsset(ModelManager.ASSET_EMBEDDING), opts)
        val ref = modelManager.headModel.value
        headSession = env.createSession(headBytes(ref), opts)
        activeModelRef = ref
    }

    private fun headBytes(ref: WakeModelRef): ByteArray = when (ref) {
        is WakeModelRef.Asset -> modelManager.readAsset(ref.assetPath)
        is WakeModelRef.Downloaded -> ref.file.readBytes()
    }

    /** Verified new head model landed: build the new session first, then swap atomically. */
    private fun swapHead(ref: WakeModelRef) {
        try {
            val opts = OrtSession.SessionOptions().apply {
                setIntraOpNumThreads(1)
                setInterOpNumThreads(1)
            }
            val fresh = env.createSession(headBytes(ref), opts)
            val old = headSession
            headSession = fresh
            activeModelRef = ref
            old?.close()
            Log.i(TAG, "hot-swapped head model -> ${ref.wakeWordId}")
        } catch (e: Exception) {
            // Contract: a bad new model never takes down the active one.
            Log.e(TAG, "head model swap failed; keeping previous model", e)
        }
    }

    private fun closeSessionsQuietly() {
        runCatching { melSession?.close() }
        runCatching { embSession?.close() }
        runCatching { headSession?.close() }
        melSession = null
        embSession = null
        headSession = null
        activeModelRef = null
    }

    /** Raw samples → mel frames, including oWW's `x/10 + 2` post-transform. */
    private fun runMelspec(input: FloatArray): Array<FloatArray> {
        val session = melSession ?: error("melspectrogram session not loaded")
        OnnxTensor.createTensor(
            env,
            FloatBuffer.wrap(input),
            longArrayOf(1, input.size.toLong()),
        ).use { tensor ->
            session.run(mapOf(session.inputNames.first() to tensor)).use { result ->
                val flat = flattenFloats(result[0].value)
                val frames = flat.size / OwwPipeline.MEL_BINS
                return Array(frames) { f ->
                    FloatArray(OwwPipeline.MEL_BINS) { b ->
                        flat[f * OwwPipeline.MEL_BINS + b] / 10f + 2f
                    }
                }
            }
        }
    }

    /** 76 mel frames → one 96-d speech embedding. Input layout [1, 76, 32, 1]. */
    private fun runEmbedding(melWindow: Array<FloatArray>): FloatArray {
        val session = embSession ?: error("embedding session not loaded")
        val flat = FloatArray(OwwPipeline.MEL_WINDOW * OwwPipeline.MEL_BINS)
        for (f in 0 until OwwPipeline.MEL_WINDOW) {
            melWindow[f].copyInto(flat, f * OwwPipeline.MEL_BINS)
        }
        OnnxTensor.createTensor(
            env,
            FloatBuffer.wrap(flat),
            longArrayOf(1, OwwPipeline.MEL_WINDOW.toLong(), OwwPipeline.MEL_BINS.toLong(), 1),
        ).use { tensor ->
            session.run(mapOf(session.inputNames.first() to tensor)).use { result ->
                val out = flattenFloats(result[0].value)
                check(out.size == OwwPipeline.EMB_DIM) {
                    "embedding output ${out.size}, expected ${OwwPipeline.EMB_DIM}"
                }
                return out
            }
        }
    }

    /** 16 embeddings → wake probability. Input layout [1, 16, 96]. */
    private fun runHead(embWindow: Array<FloatArray>): Float {
        val session = headSession ?: error("head session not loaded")
        val flat = FloatArray(OwwPipeline.EMB_WINDOW * OwwPipeline.EMB_DIM)
        for (i in 0 until OwwPipeline.EMB_WINDOW) {
            embWindow[i].copyInto(flat, i * OwwPipeline.EMB_DIM)
        }
        OnnxTensor.createTensor(
            env,
            FloatBuffer.wrap(flat),
            longArrayOf(1, OwwPipeline.EMB_WINDOW.toLong(), OwwPipeline.EMB_DIM.toLong()),
        ).use { tensor ->
            session.run(mapOf(session.inputNames.first() to tensor)).use { result ->
                val out = flattenFloats(result[0].value)
                check(out.isNotEmpty()) { "empty head output" }
                return out[0]
            }
        }
    }

    /** Flatten an arbitrarily nested array tree (ORT returns e.g. [1][1][F][32]) to floats. */
    private fun flattenFloats(value: Any?): FloatArray {
        val out = ArrayList<Float>(256)
        fun walk(v: Any?) {
            when (v) {
                is FloatArray -> for (x in v) out.add(x)
                is Array<*> -> for (child in v) walk(child)
                is Float -> out.add(v)
                else -> error("unexpected ONNX output type: ${v?.javaClass}")
            }
        }
        walk(value)
        return out.toFloatArray()
    }
}
