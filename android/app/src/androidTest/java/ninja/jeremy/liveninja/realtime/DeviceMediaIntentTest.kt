package ninja.jeremy.liveninja.realtime

import android.app.SearchManager
import android.content.Intent
import android.provider.MediaStore
import androidx.test.ext.junit.runners.AndroidJUnit4
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith

/**
 * Verifies the real platform Intent/Uri produced by [buildMediaLaunchIntent].
 * These tests only CONSTRUCT Intents: nothing is resolved or started, no
 * credentials are used and no installed YouTube app is required. They make no
 * claim about actual playback.
 */
@RunWith(AndroidJUnit4::class)
class DeviceMediaIntentTest {

    /** Unicode plus URI-reserved characters, including injected query keys. */
    private val tricky =
        " Beyoncé — AC/DC & \"Live\" #1 ?x=y+z 100% %41 🎵 &q=evil&search_query=evil "

    private fun assertHttpsSearch(
        intent: Intent,
        expectedPackage: String?,
        host: String,
        path: String,
        parameter: String,
        query: String,
    ) {
        assertEquals(Intent.ACTION_VIEW, intent.action)
        assertEquals(expectedPackage, intent.`package`)
        assertNull(intent.component)
        assertEquals(setOf(Intent.CATEGORY_BROWSABLE), intent.categories)
        assertNull(intent.extras)
        val uri = assertNotNullUri(intent)
        assertEquals("https", uri.scheme)
        assertEquals(host, uri.host)
        assertEquals(host, uri.authority)
        assertNull(uri.userInfo)
        assertEquals(-1, uri.port)
        assertEquals(path, uri.path)
        assertNull(uri.fragment)
        assertEquals(setOf(parameter), uri.queryParameterNames)
        assertEquals(listOf(query), uri.getQueryParameters(parameter))
        assertEquals(query, uri.getQueryParameter(parameter))
        val encodedQuery = uri.encodedQuery.orEmpty()
        assertTrue(encodedQuery.startsWith("$parameter="))
        assertFalse("reserved characters are percent-encoded", encodedQuery.contains('#'))
        assertEquals("exactly one key/value pair", 1, encodedQuery.count { it == '=' })
    }

    private fun assertNotNullUri(intent: Intent) = intent.data.also { assertNotNull(it) }!!

    @Test
    fun musicPlayFromSearch_isScopedWithUnstructuredFocusAndExactQuery() {
        val intent = buildMediaLaunchIntent(
            MediaLaunchCandidate.PlayFromSearch(AllowedMediaApp.YOUTUBE_MUSIC, tricky),
        )
        assertEquals(MediaStore.INTENT_ACTION_MEDIA_PLAY_FROM_SEARCH, intent.action)
        assertEquals("com.google.android.apps.youtube.music", intent.`package`)
        assertNull(intent.component)
        assertNull(intent.data)
        assertEquals("vnd.android.cursor.item/*", intent.getStringExtra(MediaStore.EXTRA_MEDIA_FOCUS))
        assertEquals(tricky, intent.getStringExtra(SearchManager.QUERY))
        assertEquals(
            setOf(MediaStore.EXTRA_MEDIA_FOCUS, SearchManager.QUERY),
            intent.extras!!.keySet().toSet(),
        )
    }

    @Test
    fun musicAppSearch_usesFixedYouTubeMusicHttpsEndpointScopedToPackage() {
        val intent = buildMediaLaunchIntent(
            MediaLaunchCandidate.HttpsSearch(AllowedMediaApp.YOUTUBE_MUSIC, MediaSearchSite.YOUTUBE_MUSIC, tricky),
        )
        assertHttpsSearch(
            intent,
            "com.google.android.apps.youtube.music",
            "music.youtube.com",
            "/search",
            "q",
            tricky,
        )
    }

    @Test
    fun musicWebFallback_hasNoPackage() {
        val intent = buildMediaLaunchIntent(
            MediaLaunchCandidate.HttpsSearch(null, MediaSearchSite.YOUTUBE_MUSIC, tricky),
        )
        assertHttpsSearch(intent, null, "music.youtube.com", "/search", "q", tricky)
    }

    @Test
    fun videoAppSearch_usesFixedYouTubeHttpsEndpointScopedToPackage() {
        val intent = buildMediaLaunchIntent(
            MediaLaunchCandidate.HttpsSearch(AllowedMediaApp.YOUTUBE, MediaSearchSite.YOUTUBE, tricky),
        )
        assertHttpsSearch(
            intent,
            "com.google.android.youtube",
            "www.youtube.com",
            "/results",
            "search_query",
            tricky,
        )
    }

    @Test
    fun videoWebFallback_hasNoPackage() {
        val intent = buildMediaLaunchIntent(
            MediaLaunchCandidate.HttpsSearch(null, MediaSearchSite.YOUTUBE, tricky),
        )
        assertHttpsSearch(intent, null, "www.youtube.com", "/results", "search_query", tricky)
    }

    @Test
    fun everyPlannedCandidate_targetsOnlyAllowListedPackagesAndHosts() {
        val allowedPackages = AllowedMediaApp.entries.map { it.packageName }.toSet()
        val allowedHosts = MediaSearchSite.entries.map { it.host }.toSet()
        for (kind in MediaKind.entries) {
            for (candidate in MediaLaunchPlanner.plan(MediaRequest(kind, tricky))) {
                val intent = buildMediaLaunchIntent(candidate)
                assertEquals(candidate.app?.packageName, intent.`package`)
                intent.`package`?.let { assertTrue(it in allowedPackages) }
                assertNull(intent.component)
                when (candidate) {
                    is MediaLaunchCandidate.PlayFromSearch ->
                        assertEquals(tricky, intent.getStringExtra(SearchManager.QUERY))

                    is MediaLaunchCandidate.HttpsSearch -> {
                        val uri = assertNotNullUri(intent)
                        assertEquals("https", uri.scheme)
                        assertTrue(uri.host in allowedHosts)
                        assertEquals(setOf(candidate.site.queryParameter), uri.queryParameterNames)
                        assertEquals(tricky, uri.getQueryParameter(candidate.site.queryParameter))
                    }
                }
            }
        }
    }
}
