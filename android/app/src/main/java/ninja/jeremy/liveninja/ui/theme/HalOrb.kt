package ninja.jeremy.liveninja.ui.theme

import android.provider.Settings
import androidx.compose.animation.core.FastOutSlowInEasing
import androidx.compose.animation.core.LinearEasing
import androidx.compose.animation.core.LinearOutSlowInEasing
import androidx.compose.animation.core.RepeatMode
import androidx.compose.animation.core.StartOffset
import androidx.compose.animation.core.animateFloat
import androidx.compose.animation.core.infiniteRepeatable
import androidx.compose.animation.core.rememberInfiniteTransition
import androidx.compose.animation.core.tween
import androidx.compose.foundation.Canvas
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.ui.Modifier
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.drawscope.DrawScope
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.graphics.drawscope.rotate
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.dp
import androidx.compose.ui.util.lerp

/**
 * Visual state of the HAL orb. Distinguished from one another by MOTION and by
 * the accompanying MicStateBanner text (which is an ARIA live region), never by
 * color alone (spec §D). Mapping from the conversation mic state lives in
 * ConversationScreen.
 */
enum class OrbState { IDLE, LISTENING, THINKING, SPEAKING, TOOLCALL, ERROR }

/**
 * The HAL 9000 "eye": an incandescent red core, a soft red glow, and animated
 * rings, per 03-theme §B. The core radial gradient, glow, ring color and all
 * animation timings come straight from the spec.
 *
 * Honors reduced motion: when `Settings.Global.ANIMATOR_DURATION_SCALE == 0`
 * every rotation/pulse is frozen and the pulsing SPEAKING rings collapse to a
 * single static ring; the per-state core alpha and the static base ring still
 * change so states stay visually distinct without animation.
 *
 * Decorative only — the composable exposes no semantics; the caller owns the
 * tap target and the state is announced by the MicStateBanner live region.
 */
@Composable
fun HalOrb(
    state: OrbState,
    modifier: Modifier = Modifier,
) {
    val colors = LocalLiveNinjaColors.current
    val accent = colors.orbAccent
    val ringColor = accent.copy(alpha = 0.30f)
    val errorRing = Color(0xFFFF5C72)

    val context = LocalContext.current
    val reducedMotion = remember {
        Settings.Global.getFloat(
            context.contentResolver,
            Settings.Global.ANIMATOR_DURATION_SCALE,
            1f,
        ) == 0f
    }

    val transition = rememberInfiniteTransition(label = "hal-orb")

    // Rotation: 24s idle spin; 2.2s while thinking; 1.4s reverse for tool calls.
    val spinDurationMs = when (state) {
        OrbState.THINKING -> 2_200
        OrbState.TOOLCALL -> 1_400
        else -> 24_000
    }
    val spin by transition.animateFloat(
        initialValue = 0f,
        targetValue = 360f,
        animationSpec = infiniteRepeatable(
            animation = tween(spinDurationMs, easing = LinearEasing),
            repeatMode = RepeatMode.Restart,
        ),
        label = "spin",
    )

    // TOOLCALL scale pulse .95 -> 1.08.
    val toolPulse by transition.animateFloat(
        initialValue = 0.95f,
        targetValue = 1.08f,
        animationSpec = infiniteRepeatable(
            animation = tween(1_400, easing = FastOutSlowInEasing),
            repeatMode = RepeatMode.Reverse,
        ),
        label = "toolPulse",
    )

    // SPEAKING: 3 emanating rings, staggered 0 / 350 / 700 ms, 2.4s each.
    val ring0 by transition.animateFloat(
        0f, 1f,
        infiniteRepeatable(
            tween(2_400, easing = LinearOutSlowInEasing),
            RepeatMode.Restart,
            initialStartOffset = StartOffset(0),
        ),
        label = "ring0",
    )
    val ring1 by transition.animateFloat(
        0f, 1f,
        infiniteRepeatable(
            tween(2_400, easing = LinearOutSlowInEasing),
            RepeatMode.Restart,
            initialStartOffset = StartOffset(350),
        ),
        label = "ring1",
    )
    val ring2 by transition.animateFloat(
        0f, 1f,
        infiniteRepeatable(
            tween(2_400, easing = LinearOutSlowInEasing),
            RepeatMode.Restart,
            initialStartOffset = StartOffset(700),
        ),
        label = "ring2",
    )
    val speakingRings = listOf(ring0, ring1, ring2)

    // Reverse spin for tool calls; frozen when reduced-motion or ERROR (static).
    val rotation = when {
        reducedMotion || state == OrbState.ERROR -> 0f
        state == OrbState.TOOLCALL -> -spin
        else -> spin
    }
    val pulseScale = if (state == OrbState.TOOLCALL && !reducedMotion) toolPulse else 1f
    val coreAlpha = if (state == OrbState.IDLE) 0.7f else 1.0f
    val baseRingAlpha = when (state) {
        OrbState.IDLE -> 0.30f
        OrbState.LISTENING -> 0.50f
        OrbState.THINKING, OrbState.TOOLCALL -> 0.45f
        else -> 0.0f // SPEAKING uses emanating rings; ERROR uses its own ring
    }

    Canvas(modifier = modifier) {
        val minDim = size.minDimension
        // Reserve the outer ~38% of the radius for glow and emanating rings.
        val coreRadius = (minDim / 2f) * 0.62f * pulseScale
        val center = Offset(size.width / 2f, size.height / 2f)
        val strokePx = 2.dp.toPx()

        // 1. Soft outer glow (accent @32% -> transparent by 65%), ~1.3x core.
        drawCircle(
            brush = Brush.radialGradient(
                colorStops = arrayOf(
                    0.0f to accent.copy(alpha = 0.32f),
                    0.65f to Color.Transparent,
                ),
                center = center,
                radius = coreRadius * 1.45f,
            ),
            radius = coreRadius * 1.45f,
            center = center,
        )

        // 2. Rings.
        when (state) {
            OrbState.SPEAKING -> {
                if (reducedMotion) {
                    // Static single ring at a representative alpha.
                    drawCircle(
                        color = ringColor.copy(alpha = 0.5f),
                        radius = coreRadius * 1.15f,
                        center = center,
                        style = Stroke(width = strokePx),
                    )
                } else {
                    speakingRings.forEach { p ->
                        val r = coreRadius * lerp(0.9f, 1.35f, p)
                        val a = lerp(0.7f, 0f, p)
                        drawCircle(
                            color = accent.copy(alpha = a.coerceIn(0f, 1f)),
                            radius = r,
                            center = center,
                            style = Stroke(width = strokePx),
                        )
                    }
                }
            }

            OrbState.ERROR -> {
                drawCircle(
                    color = errorRing,
                    radius = coreRadius * 1.28f,
                    center = center,
                    style = Stroke(width = strokePx),
                )
            }

            else -> {
                if (baseRingAlpha > 0f) {
                    drawCircle(
                        color = ringColor.copy(alpha = baseRingAlpha),
                        radius = coreRadius * 1.15f,
                        center = center,
                        style = Stroke(width = strokePx),
                    )
                }
            }
        }

        // 3. Core: rotate so the off-center specular highlight visibly spins.
        rotate(degrees = rotation, pivot = center) {
            drawCore(center, coreRadius, colors.orbCoreStops, coreAlpha)
        }
    }
}

/**
 * The incandescent core: a radial gradient whose bright specular point sits at
 * 50%,42% of the circle (upper area), plus an inset dark vignette for depth
 * (spec §B core brush + inset shadow).
 */
private fun DrawScope.drawCore(
    center: Offset,
    radius: Float,
    stops: List<Pair<Float, Color>>,
    alpha: Float,
) {
    // Highlight at 42% vertically -> 8% of the diameter above the geometric
    // center = 0.16 * radius upward.
    val gradientCenter = Offset(center.x, center.y - radius * 0.16f)
    val colorStops = stops.map { it.first to it.second }.toTypedArray()
    drawCircle(
        brush = Brush.radialGradient(
            colorStops = colorStops,
            center = gradientCenter,
            radius = radius,
        ),
        radius = radius,
        center = center,
        alpha = alpha,
    )
    // Inset shadow: transparent core -> black @55% at the rim (span 0.7..1).
    drawCircle(
        brush = Brush.radialGradient(
            colorStops = arrayOf(
                0.70f to Color.Transparent,
                1.0f to Color.Black.copy(alpha = 0.55f),
            ),
            center = center,
            radius = radius,
        ),
        radius = radius,
        center = center,
        alpha = alpha,
    )
}
