package ninja.jeremy.liveninja.ui.state

import android.content.Context
import dagger.hilt.android.qualifiers.ApplicationContext
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow

/** Persists whether the first-run onboarding wizard has been completed. */
@Singleton
class OnboardingStore @Inject constructor(
    @ApplicationContext context: Context,
) {
    private val prefs = context.getSharedPreferences(PREFS_NAME, Context.MODE_PRIVATE)

    private val _completed = MutableStateFlow(prefs.getBoolean(KEY_COMPLETED, false))
    val completed: StateFlow<Boolean> = _completed

    fun markCompleted() {
        prefs.edit().putBoolean(KEY_COMPLETED, true).apply()
        _completed.value = true
    }

    /**
     * `internal`, not `private`: the instrumented sign-in-gate test seeds this flag directly, and
     * referencing these constants means renaming either one breaks the build instead of quietly
     * making that test vacuous (it would seed a key nothing reads and still "pass").
     */
    internal companion object {
        const val PREFS_NAME = "liveninja_onboarding"
        const val KEY_COMPLETED = "onboarding_completed_v1"
    }
}
