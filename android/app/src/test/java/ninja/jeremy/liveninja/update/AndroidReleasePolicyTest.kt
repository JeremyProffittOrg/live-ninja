package ninja.jeremy.liveninja.update

import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class AndroidReleasePolicyTest {

    @Test
    fun newerVersionCodeWins() {
        assertTrue(AndroidReleasePolicy.isNewer(14, 13))
        assertFalse(AndroidReleasePolicy.isNewer(13, 13))
        assertFalse(AndroidReleasePolicy.isNewer(12, 13))
    }

    @Test
    fun onlyTheLiveNinjaDownloadsHostIsTrusted() {
        val good = "https://live.jeremy.ninja/static/models/downloads/liveninja-0.3.8-14-abc.apk"
        assertTrue(AndroidReleasePolicy.isTrustedApkUrl(good))
        assertFalse(AndroidReleasePolicy.isTrustedApkUrl("http://live.jeremy.ninja/static/models/downloads/x.apk"))
        assertFalse(AndroidReleasePolicy.isTrustedApkUrl("https://evil.example/static/models/downloads/x.apk"))
        assertFalse(AndroidReleasePolicy.isTrustedApkUrl("https://live.jeremy.ninja/static/js/app.apk"))
        assertFalse(AndroidReleasePolicy.isTrustedApkUrl("https://live.jeremy.ninja/static/models/downloads/x.apk?token=1"))
    }

    @Test
    fun urlMustCarryTheSha256() {
        val sha = "e460916d92d1556b26bf377fc32a90064144d1eea30be2b34dd6ddf4f2c58317"
        val url = "https://live.jeremy.ninja/static/models/downloads/liveninja-0.3.6-12-$sha.apk"
        assertTrue(AndroidReleasePolicy.sha256MatchesUrl(url, sha))
        assertFalse(AndroidReleasePolicy.sha256MatchesUrl(url, "deadbeef"))
    }
}
