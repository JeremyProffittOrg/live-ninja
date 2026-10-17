package ninja.jeremy.liveninja.ui.files

import android.app.DownloadManager
import android.content.Context
import android.net.Uri
import android.os.Environment
import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import dagger.hilt.android.lifecycle.HiltViewModel
import dagger.hilt.android.qualifiers.ApplicationContext
import javax.inject.Inject
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Job
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import ninja.jeremy.liveninja.net.LiveNinjaApi
import ninja.jeremy.liveninja.net.PhoneRecordingDto
import ninja.jeremy.liveninja.net.verifiedRecordingUrl
import ninja.jeremy.liveninja.realtime.RealtimeSessionCoordinator
import ninja.jeremy.liveninja.wake.WakePreferences

data class PhoneRecordingsState(
    val source: String = "messages",
    val sources: List<String> = listOf("messages"),
    val items: List<PhoneRecordingDto> = emptyList(),
    val cursor: String? = null,
    val loading: Boolean = false,
    val loaded: Boolean = false,
    val message: String? = null,
    val mediaBusy: Boolean = false,
    val playbackUrl: String? = null,
    val playingId: String? = null,
    val captureActive: Boolean = true,
)

@HiltViewModel
class PhoneRecordingsViewModel @Inject constructor(
    private val api: LiveNinjaApi,
    @ApplicationContext private val context: Context,
    coordinator: RealtimeSessionCoordinator,
    wake: WakePreferences,
) : ViewModel() {
    private val mutableState = MutableStateFlow(PhoneRecordingsState())
    val state: StateFlow<PhoneRecordingsState> = mutableState
    private var pageJob: Job? = null
    private var mediaJob: Job? = null
    private var mediaGeneration = 0

    init {
        viewModelScope.launch {
            combine(coordinator.sessionBusy, wake.serviceEnabledFlow) { session, listening -> session || listening }
                .collect { active ->
                    if (active) stopPlayback()
                    mutableState.update { it.copy(captureActive = active) }
                }
        }
    }

    fun load(source: String = state.value.source, more: Boolean = false) {
        if (more && (state.value.loading || state.value.cursor == null)) return
        val cursor = if (more) state.value.cursor else null
        pageJob?.cancel()
        if (!more) stopPlayback()
        mutableState.update {
            it.copy(source = source, loading = true, message = null,
                items = if (more) it.items else emptyList(), loaded = more,
                cursor = if (more) it.cursor else null)
        }
        pageJob = viewModelScope.launch {
            try {
                val page = api.phoneRecordings(source, cursor)
                require(page.source == source)
                mutableState.update {
                    it.copy(loading = false, loaded = true, sources = page.sources,
                        items = (if (more) it.items + page.items else page.items).distinctBy { row -> row.id },
                        cursor = page.nextCursor?.takeIf { next -> next.isNotBlank() && next != cursor })
                }
            } catch (e: CancellationException) { throw e
            } catch (_: Exception) {
                mutableState.update { it.copy(loading = false, message = "Recordings could not be loaded. Owner access is required. Retry to refresh the list.") }
            }
        }
    }

    fun media(item: PhoneRecordingDto, download: Boolean) {
        if (!item.ready || state.value.mediaBusy || (!download && state.value.captureActive)) return
        stopPlayback()
        val generation = mediaGeneration
        val source = state.value.source
        mutableState.update { it.copy(mediaBusy = true, message = null) }
        mediaJob = viewModelScope.launch {
            try {
                val result = api.phoneRecordingMedia(item.callSid, source, if (download) "attachment" else "inline")
                val url = verifiedRecordingUrl(result.url)
                if (generation != mediaGeneration) return@launch
                if (download) {
                    val request = DownloadManager.Request(Uri.parse(url))
                        .setTitle("Phone recording")
                        .setMimeType("audio/mpeg")
                        .setNotificationVisibility(DownloadManager.Request.VISIBILITY_VISIBLE_NOTIFY_COMPLETED)
                        .setDestinationInExternalPublicDir(Environment.DIRECTORY_DOWNLOADS, "${item.callSid}.mp3")
                    context.getSystemService(DownloadManager::class.java).enqueue(request)
                    mutableState.update { it.copy(mediaBusy = false, message = "Download started. Check Downloads for completion.") }
                } else if (!state.value.captureActive) {
                    mutableState.update { it.copy(mediaBusy = false, playbackUrl = url, playingId = item.id) }
                } else {
                    mutableState.update { it.copy(mediaBusy = false) }
                }
            } catch (e: CancellationException) { throw e
            } catch (_: Exception) {
                mutableState.update { it.copy(mediaBusy = false, message = "Recording unavailable or expired. Refresh the list and try again.") }
            }
        }
    }

    fun stopPlayback() {
        mediaGeneration++
        mediaJob?.cancel()
        mutableState.update { it.copy(playbackUrl = null, playingId = null, mediaBusy = false) }
    }

    fun playbackFailed() {
        stopPlayback()
        mutableState.update { it.copy(message = "Playback failed. Tap Play to request a fresh link.") }
    }
}
