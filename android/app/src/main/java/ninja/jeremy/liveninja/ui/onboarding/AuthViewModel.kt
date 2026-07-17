package ninja.jeremy.liveninja.ui.onboarding

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import dagger.hilt.android.lifecycle.HiltViewModel
import javax.inject.Inject
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.launch
import ninja.jeremy.liveninja.auth.AuthRepository
import ninja.jeremy.liveninja.auth.AuthState
import ninja.jeremy.liveninja.ui.state.OnboardingStore

/** One-shot UI events from the auth flow. */
sealed interface AuthEvent {
    /** Open the LWA consent page in a Custom Tab. */
    data class OpenCustomTab(val url: String) : AuthEvent
}

/**
 * Exposes app-wide [AuthState] + first-run-wizard completion for the root
 * gate (onboarding wizard vs. login screen vs. main app) and drives the
 * Custom-Tabs sign-in kickoff.
 */
@HiltViewModel
class AuthViewModel @Inject constructor(
    private val authRepository: AuthRepository,
    private val onboardingStore: OnboardingStore,
) : ViewModel() {

    val authState: StateFlow<AuthState> = authRepository.state

    /** True once the first-run onboarding wizard has been completed. */
    val onboardingCompleted: StateFlow<Boolean> = onboardingStore.completed

    /** Wizard's finish action landed — persist and fall through the root gate. */
    fun onOnboardingFinished() = onboardingStore.markCompleted()

    private val _events = MutableSharedFlow<AuthEvent>(extraBufferCapacity = 1)
    val events: SharedFlow<AuthEvent> = _events

    /** "Continue with Amazon" tapped: mint PKCE state and open the Custom Tab. */
    fun onContinueWithAmazon() {
        viewModelScope.launch {
            val url = authRepository.beginLogin()
            _events.emit(AuthEvent.OpenCustomTab(url))
        }
    }
}
