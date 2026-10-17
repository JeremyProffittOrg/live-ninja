package ninja.jeremy.liveninja.ui.screens

import android.media.AudioAttributes
import android.media.AudioFocusRequest
import android.media.AudioManager
import android.media.MediaPlayer
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.dp
import androidx.hilt.navigation.compose.hiltViewModel
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.LifecycleEventObserver
import androidx.lifecycle.compose.LocalLifecycleOwner
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import ninja.jeremy.liveninja.ui.SETTINGS_TAB_SIZE
import ninja.jeremy.liveninja.ui.files.PhoneRecordingsViewModel
import kotlinx.coroutines.delay

@Composable
fun PhoneRecordingsScreen(onBack: () -> Unit, viewModel: PhoneRecordingsViewModel = hiltViewModel()) {
    val state by viewModel.state.collectAsStateWithLifecycle()
    val lifecycle = LocalLifecycleOwner.current.lifecycle
    val context = LocalContext.current
    var sourcesOpen by remember { mutableStateOf(false) }
    var activePlayer by remember { mutableStateOf<MediaPlayer?>(null) }
    var duration by remember { mutableIntStateOf(0) }
    var position by remember { mutableIntStateOf(0) }
    var playing by remember { mutableStateOf(false) }
    LaunchedEffect(state.playbackUrl) {
        while (state.playbackUrl != null) {
            activePlayer?.let { player ->
                runCatching { position = player.currentPosition }
            }
            delay(250)
        }
    }
    LaunchedEffect(Unit) { if (!state.loaded) viewModel.load() }
    DisposableEffect(lifecycle) {
        val observer = LifecycleEventObserver { _, event ->
            if (event == Lifecycle.Event.ON_STOP) viewModel.stopPlayback()
        }
        lifecycle.addObserver(observer)
        onDispose { lifecycle.removeObserver(observer); viewModel.stopPlayback() }
    }
    DisposableEffect(state.playbackUrl) {
        var player: MediaPlayer? = null
        val stopObserver = LifecycleEventObserver { _, event ->
            if (event == Lifecycle.Event.ON_STOP) {
                player?.release()
                player = null
                activePlayer = null
                viewModel.stopPlayback()
            }
        }
        lifecycle.addObserver(stopObserver)
        val audio = context.getSystemService(AudioManager::class.java)
        val attributes = AudioAttributes.Builder().setUsage(AudioAttributes.USAGE_MEDIA)
            .setContentType(AudioAttributes.CONTENT_TYPE_SPEECH).build()
        val focus = AudioFocusRequest.Builder(AudioManager.AUDIOFOCUS_GAIN_TRANSIENT)
            .setAudioAttributes(attributes)
            .setOnAudioFocusChangeListener { change ->
                if (change != AudioManager.AUDIOFOCUS_GAIN) {
                    player?.release()
                    player = null
                    activePlayer = null
                    viewModel.stopPlayback()
                }
            }.build()
        var focusHeld = false
        if (state.playbackUrl != null && lifecycle.currentState.isAtLeast(Lifecycle.State.STARTED)) {
            try {
                focusHeld = audio.requestAudioFocus(focus) == AudioManager.AUDIOFOCUS_REQUEST_GRANTED
                check(focusHeld)
                player = MediaPlayer()
                player?.apply {
                    setAudioAttributes(attributes)
                    setOnPreparedListener {
                        duration = it.duration.coerceAtLeast(0)
                        activePlayer = it
                        it.start()
                        playing = true
                    }
                    setOnCompletionListener { viewModel.stopPlayback() }
                    setOnErrorListener { _, _, _ -> viewModel.playbackFailed(); true }
                    setDataSource(state.playbackUrl)
                    prepareAsync()
                }
            } catch (_: Exception) { viewModel.playbackFailed() }
        }
        onDispose {
            lifecycle.removeObserver(stopObserver)
            activePlayer = null
            player?.release()
            duration = 0
            position = 0
            playing = false
            if (focusHeld) audio.abandonAudioFocusRequest(focus)
        }
    }
    Column(Modifier.fillMaxSize().padding(top = SETTINGS_TAB_SIZE * 2)) {
        Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceBetween) {
            TextButton(onClick = onBack) { Text("Back to Files") }
            TextButton(onClick = { viewModel.load() }, enabled = !state.loading) { Text("Refresh") }
        }
        Text("Phone recordings", style = MaterialTheme.typography.titleLarge, modifier = Modifier.padding(horizontal = 16.dp))
        Box {
            TextButton(onClick = { sourcesOpen = true }) { Text("Source: ${state.source}") }
            DropdownMenu(expanded = sourcesOpen, onDismissRequest = { sourcesOpen = false }) {
                state.sources.forEach { source ->
                    DropdownMenuItem(text = { Text(source) }, onClick = { sourcesOpen = false; viewModel.load(source) })
                }
            }
        }
        if (state.captureActive) Text("End your assistant session and switch off Always listening before playback.", modifier = Modifier.padding(16.dp))
        state.message?.let { Text(it, modifier = Modifier.padding(16.dp)) }
        if (state.loading) LinearProgressIndicator(Modifier.fillMaxWidth())
        if (state.loaded && state.items.isEmpty()) Text("No phone recordings in this source.", modifier = Modifier.padding(16.dp))
        LazyColumn(contentPadding = PaddingValues(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
            items(state.items, key = { it.id }) { item ->
                Card(Modifier.fillMaxWidth()) {
                    Column(Modifier.padding(16.dp)) {
                        Text(item.target.ifBlank { "Phone call" }, style = MaterialTheme.typography.titleMedium)
                        Text("Recording: ${item.recordingStatus}")
                        if (item.providerStatus.isNotBlank()) Text("Call: ${item.providerStatus}")
                        if (item.reason.isNotBlank()) Text(item.reason)
                        if (item.updatedAt > 0) Text(java.time.Instant.ofEpochSecond(item.updatedAt).toString())
                        Row {
                            if (state.playingId == item.id) {
                                TextButton(onClick = {
                                    runCatching {
                                        activePlayer?.let { if (playing) it.pause() else it.start() }
                                        playing = !playing
                                    }.onFailure { viewModel.playbackFailed() }
                                }, enabled = activePlayer != null) { Text(if (playing) "Pause" else "Resume") }
                                TextButton(onClick = viewModel::stopPlayback) { Text("Stop") }
                            } else {
                                TextButton(onClick = { viewModel.media(item, false) }, enabled = item.ready && !state.captureActive && !state.mediaBusy) { Text("Play") }
                            }
                            TextButton(onClick = { viewModel.media(item, true) }, enabled = item.ready && !state.mediaBusy) { Text("Download") }
                        }
                        if (state.playingId == item.id) {
                            Text("${recordingTime(position)} / ${recordingTime(duration)}")
                            Slider(value = position.toFloat().coerceIn(0f, duration.coerceAtLeast(1).toFloat()),
                                onValueChange = { target ->
                                    position = target.toInt()
                                    runCatching { activePlayer?.seekTo(position) }.onFailure { viewModel.playbackFailed() }
                                }, valueRange = 0f..duration.coerceAtLeast(1).toFloat(), enabled = duration > 0)
                        }
                    }
                }
            }
            if (state.cursor != null) item {
                TextButton(onClick = { viewModel.load(more = true) }, enabled = !state.loading) { Text("Load more") }
            }
        }
    }
}

private fun recordingTime(milliseconds: Int): String {
    val seconds = milliseconds / 1000
    return "${seconds / 60}:${(seconds % 60).toString().padStart(2, '0')}"
}
