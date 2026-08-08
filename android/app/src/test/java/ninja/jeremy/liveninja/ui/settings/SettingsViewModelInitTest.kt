package ninja.jeremy.liveninja.ui.settings

import android.content.Context
import android.media.AudioManager
import android.os.PowerManager
import io.mockk.every
import io.mockk.mockk
import java.util.Optional
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.test.UnconfinedTestDispatcher
import kotlinx.coroutines.test.resetMain
import kotlinx.coroutines.test.setMain
import ninja.jeremy.liveninja.log.LogExporter
import ninja.jeremy.liveninja.log.LogSink
import ninja.jeremy.liveninja.net.LiveNinjaApi
import ninja.jeremy.liveninja.ui.state.SettingsDocument
import ninja.jeremy.liveninja.ui.state.SettingsStore
import ninja.jeremy.liveninja.ui.state.WakeWordCatalogRepository
import ninja.jeremy.liveninja.ui.state.WakeWordOption
import ninja.jeremy.liveninja.wake.ModelManager
import ninja.jeremy.liveninja.wake.WakeModelRef
import ninja.jeremy.liveninja.wake.WakePreferences
import org.json.JSONObject
import org.junit.After
import org.junit.Assert.assertNull
import org.junit.Before
import org.junit.Test

/**
 * Constructing SettingsViewModel must not throw.
 *
 * Regression test for the crash that took down the whole Settings screen the moment it was
 * opened (reproduced on a Galaxy S9, Android 10):
 *
 *     java.lang.NullPointerException: Parameter specified as non-null is null:
 *     method SettingsViewModel.buildPersonaPresets, parameter catalog
 *
 * `personaCatalog` was declared *below* the init block that reads it. Kotlin runs property
 * initializers and init blocks strictly top-to-bottom, and viewModelScope dispatches on
 * Dispatchers.Main.immediate — so `settingsStore.document.collect` started synchronously
 * during construction and StateFlow replayed its current value before ever suspending. That
 * first emission reached buildPersonaPresets() while the backing field was still JVM-null,
 * and the non-null parameter intrinsic threw.
 *
 * The bug was invisible to PersonaPresetBuilderTest because that test builds the ViewModel with
 * `mockk(relaxed = true)` and calls buildPersonaPresets() directly — the constructor never runs.
 * This test therefore constructs the REAL object, which is the only way to cover init ordering.
 *
 * UnconfinedTestDispatcher reproduces Main.immediate's eager dispatch, so the init coroutines run
 * synchronously on the test thread. An exception thrown inside `launch` is not rethrown to the
 * caller — it is routed to the thread's uncaught handler (which on Android is what killed the
 * process), so the handler is what this test asserts on.
 */
@OptIn(ExperimentalCoroutinesApi::class)
class SettingsViewModelInitTest {

    private var previousHandler: Thread.UncaughtExceptionHandler? = null
    private val uncaught = mutableListOf<Throwable>()

    @Before
    fun setUp() {
        Dispatchers.setMain(UnconfinedTestDispatcher())
        previousHandler = Thread.getDefaultUncaughtExceptionHandler()
        Thread.setDefaultUncaughtExceptionHandler { _, e -> uncaught += e }
    }

    @After
    fun tearDown() {
        Thread.setDefaultUncaughtExceptionHandler(previousHandler)
        Dispatchers.resetMain()
    }

    @Test
    fun `constructing the view model does not throw`() {
        // Constructor-time NPEs surface here directly; init-coroutine failures land in `uncaught`.
        buildViewModel()

        assertNull(
            "SettingsViewModel init threw ${uncaught.firstOrNull()} — the Settings screen " +
                "crashes on open. A property read by an init block must be declared above it.",
            uncaught.firstOrNull(),
        )
    }

    private fun buildViewModel(): SettingsViewModel {
        val context = mockk<Context>(relaxed = true)
        every { context.packageName } returns "ninja.jeremy.liveninja"
        every { context.getSystemService(Context.AUDIO_SERVICE) } returns
            mockk<AudioManager>(relaxed = true)
        every { context.getSystemService(Context.POWER_SERVICE) } returns
            mockk<PowerManager>(relaxed = true)

        val settingsStore = mockk<SettingsStore>(relaxed = true)
        every { settingsStore.document } returns MutableStateFlow(document())

        val catalog = mockk<WakeWordCatalogRepository>(relaxed = true)
        every { catalog.options } returns MutableStateFlow(emptyList<WakeWordOption>())
        every { catalog.lastFetchFailed } returns MutableStateFlow(false)

        val modelManager = mockk<ModelManager>(relaxed = true)
        every { modelManager.headModel } returns MutableStateFlow(
            WakeModelRef.Asset(wakeWordId = "hey-live-ninja", assetPath = "models/head.onnx"),
        )

        val wakePrefs = mockk<WakePreferences>(relaxed = true)
        every { wakePrefs.serviceEnabledFlow } returns MutableStateFlow(false)

        val customStore = mockk<CustomWakeWordStore>(relaxed = true)
        every { customStore.load() } returns null

        return SettingsViewModel(
            context = context,
            settingsStore = settingsStore,
            settingsRepository = mockk(relaxed = true),
            catalog = catalog,
            api = mockk<LiveNinjaApi>(relaxed = true),
            modelManager = modelManager,
            wakePrefs = wakePrefs,
            customStore = customStore,
            logSink = mockk<LogSink>(relaxed = true),
            logExporter = mockk<LogExporter>(relaxed = true),
            accountActions = Optional.empty(),
            signInLauncher = Optional.empty(),
        )
    }

    private fun document() = SettingsDocument(
        version = 1,
        wakeWord = "hey-live-ninja",
        wakeEngine = "openwakeword",
        sensitivity = 0.5f,
        personaPresetId = "default",
        hiddenPersonas = emptySet(),
        personaSystemInstructions = null,
        voice = "cedar",
        geminiVoice = "",
        turnDetection = "semantic_vad",
        micEagerness = "auto",
        theme = "system",
        micDeviceId = null,
        voiceEngineDefault = "openai-realtime",
        storeAudio = false,
        storeTranscripts = true,
        retentionDays = 30,
        raw = JSONObject(),
    )
}
