package ninja.jeremy.liveninja.ui.theme

import androidx.compose.material3.ColorScheme
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.darkColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.runtime.Immutable
import androidx.compose.runtime.staticCompositionLocalOf
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.geometry.Size
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.Shader
import androidx.compose.ui.graphics.ShaderBrush
import androidx.compose.ui.graphics.RadialGradientShader

// ---------------------------------------------------------------------------
// HAL 9000 theme (03-theme spec). HAL is the default look; the colorScheme and
// the extended [LiveNinjaColors] are selected by a style registry keyed on the
// persisted `appStyle` setting. Only "hal9000" is populated today; the
// ninja/minimal/terminal slots land in M8 (they follow the light/dark axis;
// HAL pins dark regardless of the theme setting — spec §A).
// ---------------------------------------------------------------------------

/**
 * Live-Ninja–specific colors that have no Material3 slot. Provided via a
 * [staticCompositionLocalOf] so call sites read them with
 * `LocalLiveNinjaColors.current`. Derive tints at the call site with
 * `.copy(alpha = 0.14f)` (spec §A).
 */
@Immutable
data class LiveNinjaColors(
    /** Dimmest readable text (#aeb7c6 on HAL) — captions, secondary meta. */
    val textDim: Color,
    /** Success/affirmative ink (#34e39b). */
    val success: Color,
    /** Warning ink (#ffca4a). */
    val warn: Color,
    /** Slider/progress track (#2a1417). */
    val track: Color,
    /** Orb/red glow tint (accent @ 30%). */
    val accentGlow: Color,
    /** The red orb accent (#e32636) — decorative only, never small text. */
    val orbAccent: Color,
    /** Radial color stops for the HAL orb core (offset -> color). */
    val orbCoreStops: List<Pair<Float, Color>>,
    /** Full-screen background gradient brush (spec §A bg-grad). */
    val backgroundGradient: Brush,
)

val LocalLiveNinjaColors = staticCompositionLocalOf { HalLiveNinjaColors }

// --- HAL token values --------------------------------------------------------

private val HalBackground = Color(0xFF050507)
private val HalTeal = Color(0xFF22E0D0)
private val HalRed = Color(0xFFE32636)
private val HalError = Color(0xFFFF5C72)
private val HalText = Color(0xFFF4F8FB)
private val HalTextMuted = Color(0xFFC6CDD8)
private val HalTextDim = Color(0xFFAEB7C6)

/**
 * Radial background gradient. CSS is `radial-gradient(1100px 650px at 70% -10%,
 * #16080a 0%, #050507 55%)`; approximated as a circle sized to the draw area
 * with its center biased toward the upper-right (spec §A).
 */
private val HalBackgroundGradient: Brush = object : ShaderBrush() {
    override fun createShader(size: Size): Shader {
        val maxDim = maxOf(size.width, size.height).coerceAtLeast(1f)
        return RadialGradientShader(
            center = Offset(size.width * 0.70f, size.height * -0.10f),
            radius = maxDim * 1.15f,
            colors = listOf(Color(0xFF16080A), HalBackground),
            colorStops = listOf(0f, 0.55f),
        )
    }
}

/** Extended (non-Material) HAL colors. */
val HalLiveNinjaColors = LiveNinjaColors(
    textDim = HalTextDim,
    success = Color(0xFF34E39B),
    warn = Color(0xFFFFCA4A),
    track = Color(0xFF2A1417),
    accentGlow = HalRed.copy(alpha = 0.30f),
    orbAccent = HalRed,
    orbCoreStops = listOf(
        0.00f to Color(0xFFFFF8F2),
        0.035f to Color(0xFFFFD9C4),
        0.12f to Color(0xFFFF5A4A),
        0.26f to HalRed,
        0.58f to Color(0xFF7A0F18),
        0.82f to Color(0xFF2A0507),
        1.00f to Color(0xFF0A0102),
    ),
    backgroundGradient = HalBackgroundGradient,
)

/**
 * HAL Material3 dark scheme (spec §A). Red (#e32636 / tertiary) is decorative
 * only — no filled red buttons with normal text, no small text on red (§D), so
 * the container tints below stay dark with light foregrounds.
 */
val HalColorScheme: ColorScheme = darkColorScheme(
    primary = HalTeal,
    onPrimary = HalBackground,
    primaryContainer = Color(0xFF0E3F3B),
    onPrimaryContainer = Color(0xFF9DF4EC),
    secondary = HalTeal,
    onSecondary = HalBackground,
    secondaryContainer = Color(0xFF102A2A),
    onSecondaryContainer = Color(0xFFB8E8E4),
    tertiary = HalRed,
    onTertiary = HalText,
    tertiaryContainer = Color(0xFF3A1216),
    onTertiaryContainer = Color(0xFFFFD9DC),
    background = HalBackground,
    onBackground = HalText,
    surface = HalBackground,
    onSurface = HalText,
    surfaceVariant = Color(0xB8180C0E),
    onSurfaceVariant = HalTextMuted,
    surfaceContainerLowest = Color(0xFF050507),
    surfaceContainerLow = Color(0xFF1A0D10),
    surfaceContainer = Color(0xFF180C0E),
    surfaceContainerHigh = Color(0xD9241012),
    surfaceContainerHighest = Color(0xFF241012),
    outline = Color(0x4DCD7A82),
    outlineVariant = Color(0x29CD7A82),
    error = HalError,
    onError = HalBackground,
    errorContainer = Color(0xFF5C1620),
    onErrorContainer = Color(0xFFFFDAD9),
    inverseSurface = HalText,
    inverseOnSurface = HalBackground,
    inversePrimary = Color(0xFF0C8F8C),
    scrim = Color(0xFF000000),
)

// --- Style registry ---------------------------------------------------------

/**
 * Material color scheme for [appStyle]. Only "hal9000" is populated today; the
 * ninja/minimal/terminal styles fall back to HAL until M8 ports their token
 * sets from web/static/css/app.css.
 */
fun liveNinjaColorScheme(appStyle: String): ColorScheme = when (appStyle) {
    "hal9000" -> HalColorScheme
    else -> HalColorScheme
}

/** Extended (non-Material) colors for [appStyle]. */
fun liveNinjaColors(appStyle: String): LiveNinjaColors = when (appStyle) {
    "hal9000" -> HalLiveNinjaColors
    else -> HalLiveNinjaColors
}

/**
 * @param appStyle persisted style key (SettingsStore.appStyle). Defaults to
 *   "hal9000" so HAL renders without any change at the call site.
 * @param darkTheme retained for the future light/dark styles; ignored under HAL
 *   (HAL pins dark).
 * @param dynamicColor retained for API compatibility; ignored under HAL
 *   (dynamicColor=false when HAL active — spec §A).
 */
@Composable
fun LiveNinjaTheme(
    appStyle: String = "hal9000",
    @Suppress("UNUSED_PARAMETER") darkTheme: Boolean = true,
    @Suppress("UNUSED_PARAMETER") dynamicColor: Boolean = false,
    content: @Composable () -> Unit,
) {
    val colorScheme = liveNinjaColorScheme(appStyle)
    val extended = liveNinjaColors(appStyle)
    CompositionLocalProvider(LocalLiveNinjaColors provides extended) {
        MaterialTheme(
            colorScheme = colorScheme,
            content = content,
        )
    }
}
