package ninja.jeremy.liveninja.auth

import android.app.Activity
import android.net.Uri
import androidx.browser.customtabs.CustomTabsIntent
import dagger.Binds
import dagger.Module
import dagger.hilt.InstallIn
import dagger.hilt.components.SingletonComponent
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.stateIn
import kotlinx.coroutines.launch
import ninja.jeremy.liveninja.ui.state.AccountActions
import ninja.jeremy.liveninja.ui.state.SignInLauncher

/**
 * Auth-workstream implementation of the UI seams declared in
 * [ninja.jeremy.liveninja.ui.state.UiSeamsModule]: Custom-Tabs LWA sign-in
 * for the onboarding wizard and session-teardown actions for Settings.
 */
@Singleton
class AuthUiBridge @Inject constructor(
    private val authRepository: AuthRepository,
) : SignInLauncher, AccountActions {

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main.immediate)

    override val isSignedIn: StateFlow<Boolean> =
        authRepository.state
            .map { it is AuthState.SignedIn }
            .stateIn(
                scope,
                SharingStarted.Eagerly,
                authRepository.state.value is AuthState.SignedIn,
            )

    override fun beginSignIn(activity: Activity) {
        scope.launch {
            val url = authRepository.beginLogin()
            CustomTabsIntent.Builder()
                .setShowTitle(true)
                .build()
                .launchUrl(activity, Uri.parse(url))
        }
    }

    override suspend fun signOut() {
        authRepository.logout()
    }

    override suspend fun signOutEverywhere() {
        authRepository.logoutAll()
    }
}

@Module
@InstallIn(SingletonComponent::class)
abstract class AuthUiBridgeModule {
    @Binds
    abstract fun bindSignInLauncher(impl: AuthUiBridge): SignInLauncher

    @Binds
    abstract fun bindAccountActions(impl: AuthUiBridge): AccountActions
}
