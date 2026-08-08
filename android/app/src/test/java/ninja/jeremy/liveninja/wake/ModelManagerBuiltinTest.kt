package ninja.jeremy.liveninja.wake

import android.content.Context
import io.mockk.every
import io.mockk.mockk
import io.mockk.verify
import java.io.File
import java.util.Optional
import kotlinx.coroutines.test.runTest
import okhttp3.OkHttpClient
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder

/**
 * A builtin wake phrase must never be fetched from the server.
 *
 * `GET /v1/wakeword/hey-jarvis/model` answers **404 `builtin_model`** on purpose — "built-in
 * models ship with the client and are not downloadable" (internal/webapp/wakeword_routes.go).
 * The client used to treat every 404 as a failed fetch, so choosing Hey Jarvis reported
 * "Couldn't fetch this phrase's model — the previous model keeps listening" and left the
 * previous phrase active. That is the one phrase whose bytes are always present, and measured
 * against recorded speech it is also the best performing of the lot
 * (see WakePhraseDiscriminationTest), so the failure was maximally misleading.
 *
 * Owner report, 2026-08-08: "i keep getting couldn't fetch this phrase's model".
 */
class ModelManagerBuiltinTest {

    @get:Rule
    val tmp = TemporaryFolder()

    @Test
    fun `selecting the builtin phrase succeeds without any network call`() = runTest {
        val http = mockk<OkHttpClient>() // strict: any call at all fails the test
        val manager = ModelManager(context = contextWithFilesDir(), http = http, tokenProvider = Optional.empty())

        val result = manager.sync(ModelManager.DEFAULT_ASSET_WAKE_WORD_ID)

        assertTrue("expected Builtin, got $result", result is ModelSyncResult.Builtin)
        assertEquals(
            ModelManager.DEFAULT_ASSET_WAKE_WORD_ID,
            (result as ModelSyncResult.Builtin).ref.wakeWordId,
        )
        verify(exactly = 0) { http.newCall(any()) }
    }

    /**
     * And the head the engine reads must follow, or the selection would be cosmetic: the picker
     * would show Hey Jarvis while the previously loaded model kept listening.
     */
    @Test
    fun `selecting the builtin phrase points the head model at the packaged asset`() = runTest {
        val manager = ModelManager(
            context = contextWithFilesDir(),
            http = mockk(),
            tokenProvider = Optional.empty(),
        )

        manager.sync(ModelManager.DEFAULT_ASSET_WAKE_WORD_ID)

        val head = manager.headModel.value
        assertTrue("expected an Asset ref, got $head", head is WakeModelRef.Asset)
        assertEquals(ModelManager.ASSET_DEFAULT_HEAD, (head as WakeModelRef.Asset).assetPath)
    }

    /**
     * Signed out is still NoAuth for a non-builtin phrase — the builtin short-circuit must sit
     * ABOVE the token check without swallowing it for everything else.
     */
    @Test
    fun `a non-builtin phrase while signed out is still reported as signed out`() = runTest {
        val manager = ModelManager(
            context = contextWithFilesDir(),
            http = mockk(),
            tokenProvider = Optional.empty(),
        )

        assertEquals(ModelSyncResult.NoAuth, manager.sync("hey-assistant-pro"))
    }

    private fun contextWithFilesDir(): Context {
        val dir: File = tmp.newFolder("files")
        return mockk<Context>(relaxed = true).also { every { it.filesDir } returns dir }
    }
}
