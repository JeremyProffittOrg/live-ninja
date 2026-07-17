package ninja.jeremy.liveninja.ui.overlay

import android.content.Context
import android.content.Intent
import android.graphics.PixelFormat
import android.os.Handler
import android.os.Looper
import android.provider.Settings
import android.view.Gravity
import android.view.WindowManager
import androidx.compose.foundation.background
import androidx.compose.foundation.gestures.detectDragGestures
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.GraphicEq
import androidx.compose.material.icons.filled.Mic
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.input.pointer.pointerInput
import androidx.compose.ui.platform.ComposeView
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.unit.dp
import androidx.compose.foundation.clickable
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.LifecycleOwner
import androidx.lifecycle.LifecycleRegistry
import androidx.lifecycle.setViewTreeLifecycleOwner
import androidx.savedstate.SavedStateRegistry
import androidx.savedstate.SavedStateRegistryController
import androidx.savedstate.SavedStateRegistryOwner
import androidx.savedstate.setViewTreeSavedStateRegistryOwner
import dagger.hilt.android.qualifiers.ApplicationContext
import javax.inject.Inject
import javax.inject.Singleton
import kotlin.math.roundToInt
import kotlinx.coroutines.flow.MutableStateFlow
import ninja.jeremy.liveninja.MainActivity
import ninja.jeremy.liveninja.ui.theme.LiveNinjaTheme

/** Visual state the overlay bubble reflects. */
enum class OverlayMicState { LISTENING, SPEAKING }

/**
 * Floating "session live" bubble (plan.md M4 "Live overlay"): a draggable
 * TYPE_APPLICATION_OVERLAY chip shown while a realtime session is active and
 * the app is backgrounded. Tap returns to the conversation. No-ops when the
 * user has not granted SYSTEM_ALERT_WINDOW (requested as the optional step in
 * onboarding — Settings.canDrawOverlays gate).
 */
@Singleton
class LiveOverlayController @Inject constructor(
    @ApplicationContext private val context: Context,
) {
    private val mainHandler = Handler(Looper.getMainLooper())
    private val micState = MutableStateFlow(OverlayMicState.LISTENING)

    private var overlayView: ComposeView? = null
    private var lifecycleOwner: OverlayLifecycleOwner? = null
    private var layoutParams: WindowManager.LayoutParams? = null

    private val windowManager: WindowManager
        get() = context.getSystemService(Context.WINDOW_SERVICE) as WindowManager

    val isShowing: Boolean get() = overlayView != null

    /** Show the bubble (idempotent). Silently no-ops without overlay permission. */
    fun show() = runOnMain {
        if (overlayView != null) return@runOnMain
        if (!Settings.canDrawOverlays(context)) return@runOnMain

        val params = WindowManager.LayoutParams(
            WindowManager.LayoutParams.WRAP_CONTENT,
            WindowManager.LayoutParams.WRAP_CONTENT,
            WindowManager.LayoutParams.TYPE_APPLICATION_OVERLAY,
            WindowManager.LayoutParams.FLAG_NOT_FOCUSABLE,
            PixelFormat.TRANSLUCENT,
        ).apply {
            gravity = Gravity.TOP or Gravity.START
            x = 24
            y = 240
        }

        val owner = OverlayLifecycleOwner().also { it.performCreate() }
        val view = ComposeView(context).apply {
            setViewTreeLifecycleOwner(owner)
            setViewTreeSavedStateRegistryOwner(owner)
            setContent {
                LiveNinjaTheme {
                    OverlayBubble(
                        onTap = ::openConversation,
                        onDrag = { dx, dy -> moveBy(dx, dy) },
                    )
                }
            }
        }
        try {
            windowManager.addView(view, params)
            overlayView = view
            lifecycleOwner = owner
            layoutParams = params
        } catch (_: Exception) {
            owner.performDestroy()
        }
    }

    fun update(state: OverlayMicState) {
        micState.value = state
    }

    /** Remove the bubble (idempotent). */
    fun hide() = runOnMain {
        val view = overlayView ?: return@runOnMain
        try {
            windowManager.removeView(view)
        } catch (_: Exception) {
            // Already detached — fine.
        }
        lifecycleOwner?.performDestroy()
        overlayView = null
        lifecycleOwner = null
        layoutParams = null
    }

    private fun moveBy(dx: Float, dy: Float) {
        val view = overlayView ?: return
        val params = layoutParams ?: return
        params.x += dx.roundToInt()
        params.y += dy.roundToInt()
        try {
            windowManager.updateViewLayout(view, params)
        } catch (_: Exception) {
            // View detached mid-drag — ignore.
        }
    }

    private fun openConversation() {
        context.startActivity(
            Intent(context, MainActivity::class.java).apply {
                addFlags(Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_SINGLE_TOP)
            },
        )
    }

    private fun runOnMain(block: () -> Unit) {
        if (Looper.myLooper() == Looper.getMainLooper()) block() else mainHandler.post(block)
    }

    @androidx.compose.runtime.Composable
    private fun OverlayBubble(
        onTap: () -> Unit,
        onDrag: (Float, Float) -> Unit,
    ) {
        val state by micState.collectAsState()
        val container = when (state) {
            OverlayMicState.LISTENING -> MaterialTheme.colorScheme.primaryContainer
            OverlayMicState.SPEAKING -> MaterialTheme.colorScheme.tertiaryContainer
        }
        val content = when (state) {
            OverlayMicState.LISTENING -> MaterialTheme.colorScheme.onPrimaryContainer
            OverlayMicState.SPEAKING -> MaterialTheme.colorScheme.onTertiaryContainer
        }
        val description = when (state) {
            OverlayMicState.LISTENING -> "Live Ninja session active, listening. Tap to open the conversation."
            OverlayMicState.SPEAKING -> "Live Ninja session active, assistant speaking. Tap to open the conversation."
        }
        Surface(
            shape = CircleShape,
            color = container,
            shadowElevation = 6.dp,
            modifier = Modifier
                .size(56.dp)
                .semantics { contentDescription = description }
                .pointerInput(Unit) {
                    detectDragGestures { change, dragAmount ->
                        change.consume()
                        onDrag(dragAmount.x, dragAmount.y)
                    }
                },
        ) {
            Box(
                modifier = Modifier
                    .background(container, CircleShape)
                    .clickable(onClick = onTap),
                contentAlignment = Alignment.Center,
            ) {
                Icon(
                    imageVector = when (state) {
                        OverlayMicState.LISTENING -> Icons.Filled.Mic
                        OverlayMicState.SPEAKING -> Icons.Filled.GraphicEq
                    },
                    contentDescription = null,
                    tint = content,
                )
            }
        }
    }
}

/**
 * Minimal lifecycle/saved-state owner so a ComposeView hosted directly in a
 * WindowManager overlay window (no Activity/Fragment tree) can compose.
 */
private class OverlayLifecycleOwner : LifecycleOwner, SavedStateRegistryOwner {
    private val lifecycleRegistry = LifecycleRegistry(this)
    private val savedStateRegistryController = SavedStateRegistryController.create(this)

    override val lifecycle: Lifecycle get() = lifecycleRegistry
    override val savedStateRegistry: SavedStateRegistry
        get() = savedStateRegistryController.savedStateRegistry

    fun performCreate() {
        savedStateRegistryController.performRestore(null)
        lifecycleRegistry.handleLifecycleEvent(Lifecycle.Event.ON_CREATE)
        lifecycleRegistry.handleLifecycleEvent(Lifecycle.Event.ON_START)
        lifecycleRegistry.handleLifecycleEvent(Lifecycle.Event.ON_RESUME)
    }

    fun performDestroy() {
        lifecycleRegistry.handleLifecycleEvent(Lifecycle.Event.ON_PAUSE)
        lifecycleRegistry.handleLifecycleEvent(Lifecycle.Event.ON_STOP)
        lifecycleRegistry.handleLifecycleEvent(Lifecycle.Event.ON_DESTROY)
    }
}
