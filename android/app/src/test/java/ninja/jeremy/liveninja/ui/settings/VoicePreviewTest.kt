package ninja.jeremy.liveninja.ui.settings

import android.content.Context
import android.media.AudioManager
import android.os.PowerManager
import io.mockk.coEvery
import io.mockk.coVerify
import io.mockk.every
import io.mockk.mockk
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.test.UnconfinedTestDispatcher
import kotlinx.coroutines.test.advanceUntilIdle
import kotlinx.coroutines.test.resetMain
import kotlinx.coroutines.test.runTest
import kotlinx.coroutines.test.setMain
import ninja.jeremy.liveninja.log.LogExporter
import ninja.jeremy.liveninja.log.LogSink
import ninja.jeremy.liveninja.net.LiveNinjaApi
import ninja.jeremy.liveninja.net.VoicePreviewRequest
import ninja.jeremy.liveninja.ui.state.SettingsDocument
import ninja.jeremy.liveninja.ui.state.SettingsStore
import ninja.jeremy.liveninja.ui.state.WakeWordCatalogRepository
import ninja.jeremy.liveninja.ui.state.WakeWordOption
import ninja.jeremy.liveninja.wake.ModelManager
import ninja.jeremy.liveninja.wake.WakeModelRef
import ninja.jeremy.liveninja.wake.WakePreferences
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.ResponseBody.Companion.toResponseBody
import org.json.JSONObject
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Before
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder
import java.util.Optional


@OptIn(ExperimentalCoroutinesApi::class)
class VoicePreviewTest {

    @get:Rule
    val tmp = TemporaryFolder()

    @Before
    fun setUp() {
        Dispatchers.setMain(UnconfinedTestDispatcher())
    }

    @After
    fun tearDown() {
        Dispatchers.resetMain()
    }

    @Test
    fun `preview posts the selected voice to fallback tts`() = runTest {
        val api = mockk<LiveNinjaApi>()
        coEvery { api.previewVoice(any()) } returns
            "ID3".toByteArray().toResponseBody("audio/mpeg".toMediaType())
        val vm = buildViewModel(api)

        vm.onVoicePreviewRequested("cedar")
        advanceUntilIdle()

        coVerify {
            api.previewVoice(
                VoicePreviewRequest(
                    text = SettingsViewModel.PREVIEW_SAMPLE,
                    voice = "cedar",
                    accent = "",
                ),
            )
        }
        assertEquals("Hi, I'm Live Ninja. This is how I sound.", SettingsViewModel.PREVIEW_SAMPLE)
    }

    private fun buildViewModel(api: LiveNinjaApi): SettingsViewModel {
        val context = mockk<Context>(relaxed = true)
        every { context.packageName } returns "ninja.jeremy.liveninja"
        every { context.cacheDir } returns tmp.root
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
            api = api,
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
