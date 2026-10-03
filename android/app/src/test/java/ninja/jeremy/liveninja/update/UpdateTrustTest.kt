package ninja.jeremy.liveninja.update

import android.content.pm.PackageInstaller
import io.mockk.*
import org.junit.Assert.*
import org.junit.Test

class UpdateTrustTest {
    private val hash = "a".repeat(64)
    private val cert = "b".repeat(64)
    private val release = AndroidLatestDto(1, AndroidReleasePolicy.PACKAGE_NAME, "1.0", 2,
        "https://live.jeremy.ninja/static/models/downloads/liveninja-1.0-2-$hash.apk", hash, 1234, cert)

    @Test fun malformedAndRedirectLikeMetadataCannotReachDownload() {
        assertNull(AndroidReleasePolicy.metadataError(release))
        listOf(release.copy(schemaVersion = 2), release.copy(sizeBytes = 0),
            release.copy(sizeBytes = AndroidReleasePolicy.MAX_APK_BYTES + 1),
            release.copy(certificateSha256 = ""), release.copy(versionName = ""),
            release.copy(url = release.url.replace("live.jeremy.ninja", "live.jeremy.ninja:8443")),
            release.copy(url = release.url.replace("downloads/", "downloads/../")),
            release.copy(url = release.url.replace("downloads/", "downloads/%2e%2e/")),
            release.copy(url = release.url + "#"), release.copy(url = release.url + "?"),
        ).forEach { assertNotNull(it.toString(), AndroidReleasePolicy.metadataError(it)) }
    }

    @Test fun archiveMustMatchPackageVersionAndBothPublishedAndInstalledSigner() {
        fun verify(pkg: String = release.packageName, version: Long = 2,
                   signers: Set<String> = setOf(cert), installed: Set<String> = setOf(cert)) =
            AndroidReleasePolicy.archiveError(release, pkg, version, signers, installed)
        assertNull(verify())
        assertNotNull(verify(pkg = "foreign.app"))
        assertNotNull(verify(version = 1))
        assertNotNull(verify(version = 3))
        assertNotNull(verify(signers = emptySet()))
        assertNotNull(verify(signers = setOf(hash)))
        assertNotNull(verify(installed = setOf(hash)))
        assertNotNull(verify(signers = setOf(cert, hash)))
    }

    @Test fun installSessionExplicitlyRequiresAndroidConfirmationOnModernAndroid() {
        mockkConstructor(PackageInstaller.SessionParams::class)
        try {
            every { anyConstructed<PackageInstaller.SessionParams>().setAppPackageName(any()) } just Runs
            every { anyConstructed<PackageInstaller.SessionParams>().setSize(any()) } just Runs
            every { anyConstructed<PackageInstaller.SessionParams>().setRequireUserAction(any()) } just Runs
            installSessionParams(1234, 35)
            verify(exactly = 1) { anyConstructed<PackageInstaller.SessionParams>().setRequireUserAction(PackageInstaller.SessionParams.USER_ACTION_REQUIRED) }
            verify(exactly = 0) { anyConstructed<PackageInstaller.SessionParams>().setRequireUserAction(PackageInstaller.SessionParams.USER_ACTION_NOT_REQUIRED) }
            clearConstructorMockk(PackageInstaller.SessionParams::class)
            installSessionParams(1234, 29)
            verify(exactly = 0) { anyConstructed<PackageInstaller.SessionParams>().setRequireUserAction(any()) }
        } finally { unmockkConstructor(PackageInstaller.SessionParams::class) }
    }

    @Test fun onlyTheRecordedInstallSessionCanChangeUpdateState() {
        assertTrue(InstallCallbackPolicy.matches(5, 2, 5, 2))
        assertFalse(InstallCallbackPolicy.matches(-1, 2, -1, 2))
        assertFalse(InstallCallbackPolicy.matches(5, 2, 6, 2))
        assertFalse(InstallCallbackPolicy.matches(5, 2, 5, 3))
    }
}
