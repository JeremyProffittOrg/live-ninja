package ninja.jeremy.liveninja.update

import android.content.Context
import android.content.Intent
import android.content.SharedPreferences
import android.content.pm.PackageInstaller
import io.mockk.*
import kotlinx.coroutines.test.runTest
import ninja.jeremy.liveninja.BuildConfig
import org.junit.After
import org.junit.Assert.*
import org.junit.Test

class UpdateRecoveryTest {
    private val values = mutableMapOf<String, Any>()
    private val prefs = mockk<SharedPreferences>()
    private val editor = mockk<SharedPreferences.Editor>()
    private val context = mockk<Context>(relaxed = true)

    init {
        every { context.applicationContext } returns context
        every { context.getSharedPreferences(any(), any()) } returns prefs
        every { prefs.getLong(any(), any()) } answers { values[firstArg()] as? Long ?: secondArg() }
        every { prefs.getInt(any(), any()) } answers { values[firstArg()] as? Int ?: secondArg() }
        every { prefs.edit() } returns editor
        every { editor.putLong(any(), any()) } answers { values[firstArg()] = secondArg<Long>(); editor }
        every { editor.putInt(any(), any()) } answers { values[firstArg()] = secondArg<Int>(); editor }
        every { editor.apply() } just Runs
        every { editor.commit() } returns true
        every { context.packageManager.canRequestPackageInstalls() } returns true
    }

    @After fun cleanup() { unmockkAll(); AppUpdateCoordinator.publish(AppUpdateState.Idle) }

    @Test fun lateTerminalCallbackCannotClearReplacementOrRecordItsDecline() {
        val first = AppUpdateStore(context)
        first.recordPendingSession(10, 20)
        val afterRecreation = AppUpdateStore(context)
        assertEquals(10, afterRecreation.pendingSessionId)
        afterRecreation.recordPendingSession(11, 21)
        assertFalse(first.clearPendingSession(10, 20, 20))
        assertFalse(first.clearPendingSession(11, 20, 20))
        assertEquals(11, afterRecreation.pendingSessionId)
        assertEquals(0L, afterRecreation.declinedVersionCode)
        assertTrue(first.clearPendingSession(11, 21, 21))
        assertEquals(-1, afterRecreation.pendingSessionId)
        assertEquals(21L, afterRecreation.declinedVersionCode)
    }

    @Test fun pendingSessionSurvivesRecreationAndOnlyManualRetryReplacesIt() = runTest {
        AppUpdateStore(context).recordPendingSession(10, 20) // Also models crash before commit.
        val repository = mockk<AppUpdateRepository>(relaxed = true)
        val installer = mockk<AppUpdateInstaller>()
        var pending = true
        every { installer.hasPendingSession() } answers { pending }
        every { installer.abandonPendingSession() } answers { pending = false }
        val sha = "a".repeat(64)
        val latest = AndroidLatestDto(1, AndroidReleasePolicy.PACKAGE_NAME, "current", BuildConfig.VERSION_CODE.toLong(),
            "https://live.jeremy.ninja/static/models/downloads/liveninja-$sha.apk", sha, 100, "b".repeat(64))
        coEvery { repository.fetchLatest() } returns latest
        mockkObject(AppUpdateNotifier)
        every { AppUpdateNotifier.cancelAll(any()) } just Runs
        var coordinator = AppUpdateCoordinator(context, repository, installer)
        assertNull(coordinator.check(UpdateTrigger.FOREGROUND))
        assertTrue(coordinator.state.value is AppUpdateState.Failed)
        assertTrue((coordinator.state.value as AppUpdateState.Failed).message.contains("Retry"))
        coordinator = AppUpdateCoordinator(context, repository, installer)
        assertNull(coordinator.check(UpdateTrigger.FOREGROUND))
        verify(exactly = 0) { installer.abandonPendingSession() }
        coVerify(exactly = 0) { repository.fetchLatest() }
        assertEquals(UpdateDecision.UpToDate, coordinator.check(UpdateTrigger.MANUAL))
        verify(exactly = 1) { installer.abandonPendingSession() }
        coVerify(exactly = 1) { repository.fetchLatest() }
    }

    @Test fun missingConfirmationIntentLeavesAnActionableRetryInsteadOfInstallingForever() {
        val store = AppUpdateStore(context)
        store.recordPendingSession(10, 20)
        val intent = callback()
        UpdateInstallReceiver().onReceive(context, intent)
        assertEquals(10, store.pendingSessionId)
        assertTrue(AppUpdateCoordinator(context, mockk(relaxed = true), mockk(relaxed = true)).state.value is AppUpdateState.Failed)
        verify(exactly = 0) { context.startActivity(any()) }
    }

    @Suppress("DEPRECATION")
    @Test fun backgroundConfirmationWithNoNotificationStillOffersForegroundRecovery() {
        AppUpdateStore(context).recordPendingSession(10, 20)
        val confirm = mockk<Intent>(relaxed = true)
        every { confirm.addFlags(any()) } returns confirm
        val intent = callback()
        every { intent.getParcelableExtra<Intent>(Intent.EXTRA_INTENT) } returns confirm
        mockkObject(AppUpdateNotifier)
        // A denied POST_NOTIFICATIONS grant makes this a no-op in production.
        every { AppUpdateNotifier.postConfirmInstall(any(), any()) } just Runs
        UpdateInstallReceiver().onReceive(context, intent)
        verify(exactly = 0) { context.startActivity(any()) }
        val state = AppUpdateCoordinator(context, mockk(relaxed = true), mockk(relaxed = true)).state.value
        assertTrue(state is AppUpdateState.Failed)
        assertTrue((state as AppUpdateState.Failed).message.contains("Retry"))
    }

    @Suppress("DEPRECATION")
    private fun callback(): Intent = mockk<Intent> {
        every { action } returns UpdateInstallReceiver.ACTION
        every { getIntExtra(PackageInstaller.EXTRA_STATUS, any()) } returns PackageInstaller.STATUS_PENDING_USER_ACTION
        every { getIntExtra(PackageInstaller.EXTRA_SESSION_ID, any()) } returns 10
        every { getLongExtra(UpdateInstallReceiver.EXTRA_VERSION_CODE, any()) } returns 20
        every { getParcelableExtra<Intent>(Intent.EXTRA_INTENT) } returns null
    }
}
