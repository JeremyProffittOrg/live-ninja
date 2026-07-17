package ninja.jeremy.liveninja.assistant

import android.app.Activity
import android.app.KeyguardManager
import android.content.Context
import android.hardware.biometrics.BiometricManager
import android.hardware.biometrics.BiometricPrompt
import android.os.Build
import android.os.CancellationSignal
import android.util.Log
import dagger.hilt.android.qualifiers.ApplicationContext
import java.util.concurrent.Executor
import javax.inject.Inject
import javax.inject.Singleton
import kotlin.coroutines.resume
import kotlinx.coroutines.suspendCancellableCoroutine

/**
 * Keyguard + biometric gating for assistant sessions launched over the lock
 * screen (plan.md M4, Android §2: "locked-screen sessions gate sensitive
 * actions behind biometric").
 *
 * Policy:
 *  - A conversation may proceed over the keyguard (weather, timers, chat).
 *  - Any SENSITIVE tool call (account, purchases, files, home controls, ...)
 *    must pass [gateSensitiveAction] first: on a locked keyguard that means
 *    [requestDismissKeyguard] (the platform prompts the user's credential /
 *    biometric to unlock); on an unlocked device with a secure lock it means an
 *    explicit [confirmBiometric] so a bystander with the unlocked phone still
 *    can't fire sensitive tools mid-session from the lock-screen entry point.
 *
 * Uses the framework [BiometricPrompt] (API 29+, matches minSdk) rather than
 * androidx.biometric so plain ComponentActivity hosts work without a
 * FragmentActivity dependency.
 */
@Singleton
class KeyguardGate @Inject constructor(
    @ApplicationContext private val context: Context,
) {
    private val keyguardManager: KeyguardManager
        get() = context.getSystemService(Context.KEYGUARD_SERVICE) as KeyguardManager

    /** True while the keyguard is showing (locked screen). */
    fun isKeyguardLocked(): Boolean = keyguardManager.isKeyguardLocked

    /** True when the user has any secure lock (PIN/pattern/password/biometric). */
    fun isDeviceSecure(): Boolean = keyguardManager.isDeviceSecure

    /**
     * Ask the platform to dismiss the keyguard, prompting the user's credential
     * on a secure keyguard. Resumes true only when the keyguard is actually
     * gone. Safe to call when already unlocked (returns true immediately).
     */
    suspend fun requestDismissKeyguard(activity: Activity): Boolean {
        if (!isKeyguardLocked()) return true
        return suspendCancellableCoroutine { cont ->
            keyguardManager.requestDismissKeyguard(
                activity,
                object : KeyguardManager.KeyguardDismissCallback() {
                    override fun onDismissSucceeded() {
                        if (cont.isActive) cont.resume(true)
                    }

                    override fun onDismissCancelled() {
                        Log.i(TAG, "Keyguard dismiss cancelled by user")
                        if (cont.isActive) cont.resume(false)
                    }

                    override fun onDismissError() {
                        Log.w(TAG, "Keyguard dismiss error")
                        if (cont.isActive) cont.resume(false)
                    }
                },
            )
        }
    }

    /**
     * Show a biometric confirmation (device credential fallback allowed).
     * Resumes true on successful authentication, false on user cancel/error.
     */
    suspend fun confirmBiometric(
        activity: Activity,
        title: String,
        subtitle: String? = null,
    ): Boolean = suspendCancellableCoroutine { cont ->
        val executor: Executor = activity.mainExecutor
        val cancellation = CancellationSignal()
        cont.invokeOnCancellation { cancellation.cancel() }

        val builder = BiometricPrompt.Builder(activity)
            .setTitle(title)
            .apply { subtitle?.let(::setSubtitle) }
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
            builder.setAllowedAuthenticators(
                BiometricManager.Authenticators.BIOMETRIC_STRONG or
                    BiometricManager.Authenticators.DEVICE_CREDENTIAL,
            )
        } else {
            // API 29 path: device credential as fallback.
            @Suppress("DEPRECATION")
            builder.setDeviceCredentialAllowed(true)
        }

        builder.build().authenticate(
            cancellation,
            executor,
            object : BiometricPrompt.AuthenticationCallback() {
                override fun onAuthenticationSucceeded(result: BiometricPrompt.AuthenticationResult?) {
                    if (cont.isActive) cont.resume(true)
                }

                override fun onAuthenticationError(errorCode: Int, errString: CharSequence?) {
                    Log.i(TAG, "Biometric gate declined ($errorCode): $errString")
                    if (cont.isActive) cont.resume(false)
                }
                // onAuthenticationFailed = one bad attempt; the prompt stays up,
                // so no resume — terminal outcomes arrive via the two above.
            },
        )
    }

    /**
     * Full gate for a sensitive tool call. Returns true when the action may
     * proceed:
     *  - no secure lock configured -> allowed (nothing to authenticate against),
     *  - keyguard locked -> [requestDismissKeyguard] must succeed,
     *  - unlocked but the session began over the keyguard -> [confirmBiometric].
     *
     * @param sessionLaunchedWhileLocked [AssistTrigger.launchedWhileLocked] of
     *   the session driving the tool call.
     */
    suspend fun gateSensitiveAction(
        activity: Activity,
        title: String,
        subtitle: String? = null,
        sessionLaunchedWhileLocked: Boolean,
    ): Boolean {
        if (!isDeviceSecure()) return true
        if (isKeyguardLocked()) return requestDismissKeyguard(activity)
        if (sessionLaunchedWhileLocked) return confirmBiometric(activity, title, subtitle)
        return true
    }

    private companion object {
        const val TAG = "KeyguardGate"
    }
}
