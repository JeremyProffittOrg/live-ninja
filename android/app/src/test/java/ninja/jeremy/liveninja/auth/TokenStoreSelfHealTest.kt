package ninja.jeremy.liveninja.auth

import android.content.Context
import android.content.SharedPreferences
import io.mockk.every
import io.mockk.mockk
import io.mockk.verify
import java.security.GeneralSecurityException
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * TokenStore self-healing factory (01-platform §A1): a corrupt encrypted store
 * throws GeneralSecurityException/IOException from the prefs factory. The store
 * must wipe + retry once (heal), and drop to a null mode on persistent failure
 * — never propagate the throw (which previously killed the process on load).
 */
class TokenStoreSelfHealTest {

    private val context = mockk<Context>(relaxed = true)
    private val sample = StoredSession(
        accessToken = "acc",
        accessExpiresAt = 100L,
        refreshToken = "ref",
        refreshExpiresAt = 200L,
        sessionId = "sess-1",
    )

    /** In-memory SharedPreferences fake covering the keys TokenStore touches. */
    private fun fakePrefs(): SharedPreferences {
        val map = HashMap<String, Any?>()
        val editor = mockk<SharedPreferences.Editor>(relaxed = true)
        every { editor.putString(any(), any()) } answers { map[firstArg()] = secondArg<String?>(); editor }
        every { editor.putLong(any(), any()) } answers { map[firstArg()] = secondArg<Long>(); editor }
        every { editor.remove(any()) } answers { map.remove(firstArg()); editor }
        val prefs = mockk<SharedPreferences>(relaxed = true)
        every { prefs.getString(any(), any()) } answers { (map[firstArg()] as String?) ?: secondArg() }
        every { prefs.getLong(any(), any()) } answers { (map[firstArg()] as Long?) ?: secondArg() }
        every { prefs.edit() } returns editor
        return prefs
    }

    @Test
    fun throwOnce_wipesRetriesAndServesStore() {
        val healed = fakePrefs()
        var calls = 0
        val store = TokenStore(context).apply {
            prefsFactory = {
                calls++
                if (calls == 1) throw GeneralSecurityException("corrupt keyset")
                healed
            }
        }

        // First access triggers the corruption → wipe → retry path.
        store.saveSession(sample)

        assertEquals(2, calls)
        assertEquals("acc", store.accessToken())
        assertEquals(sample, store.session())
        assertTrue("wipe must flag a reset", store.storeReset.value)
        verify { context.deleteSharedPreferences("liveninja_auth") }
    }

    @Test
    fun throwAlways_entersNullModeWithoutCrashing() {
        val store = TokenStore(context).apply {
            prefsFactory = { throw GeneralSecurityException("still corrupt after wipe") }
        }

        // Reads return null, writes no-op — no exception escapes.
        assertNull(store.session())
        assertNull(store.accessToken())
        store.saveSession(sample)
        store.updateFromRefresh("a", 1L, "r", 2L, "s")
        store.clearSession()
        store.savePendingLogin(PendingLogin("st", "vf", "uri", 0L))
        assertNull(store.consumePendingLogin())

        assertTrue(store.storeReset.value)
    }

    @Test
    fun healthyStore_neverFlagsReset() {
        val prefs = fakePrefs()
        val store = TokenStore(context).apply { prefsFactory = { prefs } }

        store.saveSession(sample)

        assertEquals(sample, store.session())
        assertFalse(store.storeReset.value)
        verify(exactly = 0) { context.deleteSharedPreferences(any()) }
    }
}
