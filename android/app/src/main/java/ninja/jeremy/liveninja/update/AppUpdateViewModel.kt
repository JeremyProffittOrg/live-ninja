package ninja.jeremy.liveninja.update

import android.content.Context
import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import dagger.hilt.android.lifecycle.HiltViewModel
import dagger.hilt.android.qualifiers.ApplicationContext
import javax.inject.Inject
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.launch
import ninja.jeremy.liveninja.BuildConfig
import ninja.jeremy.liveninja.log.LNLog
import ninja.jeremy.liveninja.log.LogCategory

sealed class AppUpdateState {
    data object Idle : AppUpdateState()
    data class Available(
        val versionName: String,
        val versionCode: Long,
        val sizeBytes: Long,
    ) : AppUpdateState()
    data class Downloading(val receivedBytes: Long, val totalBytes: Long) : AppUpdateState()
    data object Installing : AppUpdateState()
    data class Failed(val message: String) : AppUpdateState()
}

@HiltViewModel
class AppUpdateViewModel @Inject constructor(
    @ApplicationContext private val context: Context,
    private val repository: AppUpdateRepository,
    private val installer: AppUpdateInstaller,
) : ViewModel() {

    private val _state = MutableStateFlow<AppUpdateState>(AppUpdateState.Idle)
    val state: StateFlow<AppUpdateState> = _state

    fun check() {
        if (_state.value !is AppUpdateState.Idle) return
        viewModelScope.launch {
            try {
                val latest = repository.fetchLatest()
                if (latest.packageName != AndroidReleasePolicy.PACKAGE_NAME) return@launch
                if (!AndroidReleasePolicy.isTrustedApkUrl(latest.url)) return@launch
                if (!AndroidReleasePolicy.isNewer(latest.versionCode, BuildConfig.VERSION_CODE)) {
                    return@launch
                }
                pending = latest
                _state.value = AppUpdateState.Available(
                    versionName = latest.versionName,
                    versionCode = latest.versionCode,
                    sizeBytes = latest.sizeBytes,
                )
            } catch (t: Throwable) {
                LNLog.w(LogCategory.NET, TAG, "update check failed", t)
            }
        }
    }

    fun dismiss() {
        pending = null
        _state.value = AppUpdateState.Idle
    }

    fun startInstall() {
        val release = pending ?: return
        viewModelScope.launch {
            try {
                _state.value = AppUpdateState.Downloading(0, release.sizeBytes)
                val apk = repository.download(release) { copied ->
                    _state.value = AppUpdateState.Downloading(copied, release.sizeBytes)
                }
                _state.value = AppUpdateState.Installing
                installer.install(apk)
            } catch (t: Throwable) {
                LNLog.w(LogCategory.NET, TAG, "update install failed", t)
                _state.value = AppUpdateState.Failed(
                    t.message?.take(200) ?: "Couldn't install this update.",
                )
            }
        }
    }

    private var pending: AndroidLatestDto? = null

    private companion object {
        const val TAG = "AppUpdate"
    }
}
