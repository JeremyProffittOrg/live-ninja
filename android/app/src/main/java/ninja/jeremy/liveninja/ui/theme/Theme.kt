package ninja.jeremy.liveninja.ui.theme

import androidx.compose.material3.ColorScheme
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.lightColorScheme
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
import androidx.compose.ui.graphics.SolidColor

// ---------------------------------------------------------------------------
// Style registry (03-theme spec + M8.1, web/static/css/app.css [data-ln-style]
// blocks + web/static/js/theme.js STYLE_ACCENTS). The colorScheme and the
// extended [LiveNinjaColors] are selected by a registry keyed on the
// persisted `appStyle` setting. HAL 9000 is the default look and pins dark
// regardless of the theme setting (spec §A); ninja/minimal/terminal follow
// the light/dark/system theme setting like the web's --ln-base-* tokens do.
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

// --- Shared helpers for the light/dark styles --------------------------------

/** Linear channel-wise mix toward [other] by [fraction] (0 = this, 1 = other). */
private fun Color.mix(other: Color, fraction: Float): Color = Color(
    red = red + (other.red - red) * fraction,
    green = green + (other.green - green) * fraction,
    blue = blue + (other.blue - blue) * fraction,
    alpha = alpha,
)

/**
 * Approximates a CSS `radial-gradient(NxM at 70% -10%, top 0%, bottom 55%)`
 * background (same construction HAL uses — spec §A bg-grad).
 */
private fun radialBackgroundGradient(top: Color, bottom: Color): Brush = object : ShaderBrush() {
    override fun createShader(size: Size): Shader {
        val maxDim = maxOf(size.width, size.height).coerceAtLeast(1f)
        return RadialGradientShader(
            center = Offset(size.width * 0.70f, size.height * -0.10f),
            radius = maxDim * 1.15f,
            colors = listOf(top, bottom),
            colorStops = listOf(0f, 0.55f),
        )
    }
}

/**
 * CSS-token-equivalent values for one (style, brightness) combination — ported
 * from app.css's `:root`/`[data-theme="light"]` base tokens (ninja/minimal
 * reset to these) and the `[data-ln-style="terminal"]` block. [primary] is the
 * style's STYLE_ACCENTS accent (theme.js) in dark mode — the web overrides
 * `--ln-teal` to that accent per zone, and dark-mode `--ln-accent-ink`
 * resolves through `var(--ln-teal)`, so the raw accent IS the ink there. Light
 * mode's `--ln-base-accent-ink` is a hand-tuned fixed hex in the CSS
 * (independent of the accent picker, because the bright brand hues fail AA as
 * text on light surfaces) — [primary] here is the same kind of AA-safe
 * same-hue shade (ninja's #0a706b matches the CSS literal exactly; minimal's
 * cyan and terminal's green shades are derived the same way, since the CSS
 * only ever hand-tuned the teal case).
 */
private class StyleTokens(
    val bg: Color,
    val bgGradTop: Color,
    val hasGradient: Boolean = true,
    val surface: Color,
    val surface2: Color,
    val surfaceRaised: Color,
    val inputBg: Color,
    val track: Color,
    val border: Color,
    val borderStrong: Color,
    val text: Color,
    val textMuted: Color,
    val textDim: Color,
    val primary: Color,
    val onPrimary: Color,
    val success: Color,
    val warn: Color,
    val error: Color,
    val errorBorder: Color,
    val orbCoreStops: List<Pair<Float, Color>>,
)

/** Builds a full Material3 [ColorScheme] from [t] (spec §A token → M3 slot mapping). */
private fun buildColorScheme(t: StyleTokens, isDark: Boolean): ColorScheme {
    val containerMix = if (isDark) Color.Black else Color.White
    val onContainerMix = if (isDark) Color.White else Color.Black
    val primaryContainer = t.primary.mix(containerMix, 0.70f)
    val onPrimaryContainer = t.primary.mix(onContainerMix, 0.55f)
    val errorContainer = t.error.mix(containerMix, 0.70f)
    val onErrorContainer = t.error.mix(onContainerMix, 0.55f)
    val surfaceContainerHighest = t.surfaceRaised.mix(onContainerMix, 0.07f)
    val inversePrimary = t.primary.mix(containerMix, 0.40f)
    return if (isDark) {
        darkColorScheme(
            primary = t.primary, onPrimary = t.onPrimary,
            primaryContainer = primaryContainer, onPrimaryContainer = onPrimaryContainer,
            secondary = t.primary, onSecondary = t.onPrimary, // single brand hue per style
            secondaryContainer = primaryContainer, onSecondaryContainer = onPrimaryContainer,
            tertiary = t.primary, onTertiary = t.onPrimary, // no HAL-style decorative-only 2nd hue
            tertiaryContainer = primaryContainer, onTertiaryContainer = onPrimaryContainer,
            background = t.bg, onBackground = t.text,
            surface = t.bg, onSurface = t.text,
            surfaceVariant = t.surface2, onSurfaceVariant = t.textMuted,
            surfaceTint = t.primary,
            inverseSurface = t.text, inverseOnSurface = t.bg,
            inversePrimary = inversePrimary,
            outline = t.borderStrong, outlineVariant = t.border,
            scrim = Color(0xFF000000),
            error = t.error, onError = t.onPrimary,
            errorContainer = errorContainer, onErrorContainer = onErrorContainer,
            surfaceBright = t.surfaceRaised, surfaceDim = t.surface,
            surfaceContainer = t.surfaceRaised,
            surfaceContainerHigh = t.surface2,
            surfaceContainerHighest = surfaceContainerHighest,
            surfaceContainerLow = t.inputBg,
            surfaceContainerLowest = t.bg,
        )
    } else {
        lightColorScheme(
            primary = t.primary, onPrimary = t.onPrimary,
            primaryContainer = primaryContainer, onPrimaryContainer = onPrimaryContainer,
            secondary = t.primary, onSecondary = t.onPrimary,
            secondaryContainer = primaryContainer, onSecondaryContainer = onPrimaryContainer,
            tertiary = t.primary, onTertiary = t.onPrimary,
            tertiaryContainer = primaryContainer, onTertiaryContainer = onPrimaryContainer,
            background = t.bg, onBackground = t.text,
            surface = t.bg, onSurface = t.text,
            surfaceVariant = t.surface2, onSurfaceVariant = t.textMuted,
            surfaceTint = t.primary,
            inverseSurface = t.text, inverseOnSurface = t.bg,
            inversePrimary = inversePrimary,
            outline = t.borderStrong, outlineVariant = t.border,
            scrim = Color(0xFF000000),
            error = t.error, onError = t.onPrimary,
            errorContainer = errorContainer, onErrorContainer = onErrorContainer,
            surfaceBright = t.surfaceRaised, surfaceDim = t.surface,
            surfaceContainer = t.surfaceRaised,
            surfaceContainerHigh = t.surface2,
            surfaceContainerHighest = surfaceContainerHighest,
            surfaceContainerLow = t.inputBg,
            surfaceContainerLowest = t.bg,
        )
    }
}

private fun StyleTokens.toLiveNinjaColors(): LiveNinjaColors = LiveNinjaColors(
    textDim = textDim,
    success = success,
    warn = warn,
    track = track,
    accentGlow = primary.copy(alpha = 0.30f),
    orbAccent = primary,
    orbCoreStops = orbCoreStops,
    backgroundGradient = if (hasGradient) radialBackgroundGradient(bgGradTop, bg) else SolidColor(bg),
)

// --- Ninja tokens (app default; STYLE_ACCENTS.ninja = teal, same as base) ---

/** Ninja's default (unstyled-orb) core gradient — app.css .ln-orb__core base radial, teal-hued. */
private val NinjaOrbCoreStops = listOf(
    0.00f to Color(0xFFEAFFFB),
    0.12f to Color(0xFFB8F5EA),
    0.30f to Color(0xFF22E0D0),
    0.55f to Color(0xFF0C8F8C), // --ln-teal-700
    0.80f to Color(0xFF0A2A28),
    1.00f to Color(0xFF050B0A),
)

private val NinjaDarkTokens = StyleTokens(
    bg = Color(0xFF060D18), bgGradTop = Color(0xFF12294A),
    surface = Color(0xB8142642), surface2 = Color(0xD91C3254), surfaceRaised = Color(0xFF142544),
    inputBg = Color(0xFF0F1E35), track = Color(0xFF16294A),
    border = Color(0x297AA0CD), borderStrong = Color(0x4D7AA0CD),
    text = Color(0xFFF4F8FB), textMuted = Color(0xFF9FB0C4), textDim = Color(0xFF8296AE),
    primary = Color(0xFF22E0D0), onPrimary = Color(0xFF052220),
    success = Color(0xFF34E39B), warn = Color(0xFFFFCA4A), error = Color(0xFFFF5C72),
    errorBorder = Color(0x80FF5C72),
    orbCoreStops = NinjaOrbCoreStops,
)

private val NinjaLightTokens = StyleTokens(
    bg = Color(0xFFEEF3F8), bgGradTop = Color(0xFFDCE9F5),
    surface = Color(0xD9FFFFFF), surface2 = Color(0xFFF5F9FD), surfaceRaised = Color(0xFFFFFFFF),
    inputBg = Color(0xFFFFFFFF), track = Color(0xFFC9D6E4),
    border = Color(0x24102A4A), borderStrong = Color(0x47102A4A),
    text = Color(0xFF0C1A2E), textMuted = Color(0xFF3D4F66), textDim = Color(0xFF55677E),
    primary = Color(0xFF0A706B), onPrimary = Color(0xFFFFFFFF), // literal web accent-ink (5.3:1 on bg)
    success = Color(0xFF0A7A4F), warn = Color(0xFF7A5B00), error = Color(0xFFC81E3E),
    errorBorder = Color(0x73C81E3E),
    orbCoreStops = NinjaOrbCoreStops,
)

val NinjaDarkColorScheme: ColorScheme = buildColorScheme(NinjaDarkTokens, isDark = true)
val NinjaLightColorScheme: ColorScheme = buildColorScheme(NinjaLightTokens, isDark = false)
val NinjaDarkColors: LiveNinjaColors = NinjaDarkTokens.toLiveNinjaColors()
val NinjaLightColors: LiveNinjaColors = NinjaLightTokens.toLiveNinjaColors()

// --- Minimal tokens (STYLE_ACCENTS.minimal = cyan; same base surfaces as ninja) ---

private val MinimalOrbCoreStops = listOf(
    0.00f to Color(0xFFF2FBFF),
    0.12f to Color(0xFFBEEAFF),
    0.30f to Color(0xFF38D0FF),
    0.55f to Color(0xFF0B6A8F),
    0.80f to Color(0xFF0A2230),
    1.00f to Color(0xFF050B10),
)

private val MinimalDarkTokens = StyleTokens(
    bg = NinjaDarkTokens.bg, bgGradTop = NinjaDarkTokens.bgGradTop,
    surface = NinjaDarkTokens.surface, surface2 = NinjaDarkTokens.surface2, surfaceRaised = NinjaDarkTokens.surfaceRaised,
    inputBg = NinjaDarkTokens.inputBg, track = NinjaDarkTokens.track,
    border = NinjaDarkTokens.border, borderStrong = NinjaDarkTokens.borderStrong,
    text = NinjaDarkTokens.text, textMuted = NinjaDarkTokens.textMuted, textDim = NinjaDarkTokens.textDim,
    primary = Color(0xFF38D0FF), onPrimary = Color(0xFF052220),
    success = NinjaDarkTokens.success, warn = NinjaDarkTokens.warn, error = NinjaDarkTokens.error,
    errorBorder = NinjaDarkTokens.errorBorder,
    orbCoreStops = MinimalOrbCoreStops,
)

private val MinimalLightTokens = StyleTokens(
    bg = NinjaLightTokens.bg, bgGradTop = NinjaLightTokens.bgGradTop,
    surface = NinjaLightTokens.surface, surface2 = NinjaLightTokens.surface2, surfaceRaised = NinjaLightTokens.surfaceRaised,
    inputBg = NinjaLightTokens.inputBg, track = NinjaLightTokens.track,
    border = NinjaLightTokens.border, borderStrong = NinjaLightTokens.borderStrong,
    text = NinjaLightTokens.text, textMuted = NinjaLightTokens.textMuted, textDim = NinjaLightTokens.textDim,
    primary = Color(0xFF0A6B8F), onPrimary = Color(0xFFFFFFFF), // derived AA-safe cyan ink (5.4:1 on bg)
    success = NinjaLightTokens.success, warn = NinjaLightTokens.warn, error = NinjaLightTokens.error,
    errorBorder = NinjaLightTokens.errorBorder,
    orbCoreStops = MinimalOrbCoreStops,
)

val MinimalDarkColorScheme: ColorScheme = buildColorScheme(MinimalDarkTokens, isDark = true)
val MinimalLightColorScheme: ColorScheme = buildColorScheme(MinimalLightTokens, isDark = false)
val MinimalDarkColors: LiveNinjaColors = MinimalDarkTokens.toLiveNinjaColors()
val MinimalLightColors: LiveNinjaColors = MinimalLightTokens.toLiveNinjaColors()

// --- Terminal tokens (STYLE_ACCENTS.terminal = green; CRT phosphor, app.css literal) ---

/** Literal `[data-ln-style="terminal"]` core radial: `circle at 50% 45%, #eaffea 0%, accent 18%, #0a3a16 70%, #010401 100%`. */
private val TerminalOrbCoreStops = listOf(
    0.00f to Color(0xFFEAFFEA),
    0.18f to Color(0xFF33FF66),
    0.70f to Color(0xFF0A3A16),
    1.00f to Color(0xFF010401),
)

private val TerminalDarkTokens = StyleTokens(
    bg = Color(0xFF000000), bgGradTop = Color(0xFF000000), hasGradient = false, // CSS --ln-bg-grad: none
    surface = Color(0xD9081008), surface2 = Color(0xE60C180C), surfaceRaised = Color(0xFF071007),
    inputBg = Color(0xFF061006), track = Color(0xFF12331A),
    border = Color(0x3850C86E), borderStrong = Color(0x6650C86E),
    text = Color(0xFFEAFFEA), textMuted = Color(0xFF9FD8AB), textDim = Color(0xFF86BF93),
    primary = Color(0xFF33FF66), onPrimary = Color(0xFF052220),
    success = Color(0xFF34E39B), warn = Color(0xFFFFCA4A), error = Color(0xFFFF5C72),
    errorBorder = Color(0x80FF5C72),
    orbCoreStops = TerminalOrbCoreStops,
)

/**
 * app.css never gives terminal a light variant (its block is unconditional,
 * pinned dark like HAL) — M8.1 pins terminal to the light/dark AXIS regardless
 * (android-revamp-plan.md M8.1), so this reuses ninja/minimal's light base
 * surfaces (the only light palette the design system defines) with terminal's
 * own AA-safe green ink, same derivation as minimal's cyan ink above.
 */
private val TerminalLightTokens = StyleTokens(
    bg = NinjaLightTokens.bg, bgGradTop = NinjaLightTokens.bgGradTop, hasGradient = false,
    surface = NinjaLightTokens.surface, surface2 = NinjaLightTokens.surface2, surfaceRaised = NinjaLightTokens.surfaceRaised,
    inputBg = NinjaLightTokens.inputBg, track = NinjaLightTokens.track,
    border = NinjaLightTokens.border, borderStrong = NinjaLightTokens.borderStrong,
    text = NinjaLightTokens.text, textMuted = NinjaLightTokens.textMuted, textDim = NinjaLightTokens.textDim,
    primary = Color(0xFF0A7038), onPrimary = Color(0xFFFFFFFF), // derived AA-safe green ink (5.6:1 on bg)
    success = NinjaLightTokens.success, warn = NinjaLightTokens.warn, error = NinjaLightTokens.error,
    errorBorder = NinjaLightTokens.errorBorder,
    orbCoreStops = TerminalOrbCoreStops,
)

val TerminalDarkColorScheme: ColorScheme = buildColorScheme(TerminalDarkTokens, isDark = true)
val TerminalLightColorScheme: ColorScheme = buildColorScheme(TerminalLightTokens, isDark = false)
val TerminalDarkColors: LiveNinjaColors = TerminalDarkTokens.toLiveNinjaColors()
val TerminalLightColors: LiveNinjaColors = TerminalLightTokens.toLiveNinjaColors()

// --- Style registry ---------------------------------------------------------

/**
 * Material color scheme for [appStyle] at the given brightness. HAL ignores
 * [darkTheme] (always dark — spec §A); ninja/minimal/terminal honor it.
 * Unrecognized styles fall back to HAL.
 */
fun liveNinjaColorScheme(appStyle: String, darkTheme: Boolean): ColorScheme = when (appStyle) {
    "hal9000" -> HalColorScheme
    "ninja" -> if (darkTheme) NinjaDarkColorScheme else NinjaLightColorScheme
    "minimal" -> if (darkTheme) MinimalDarkColorScheme else MinimalLightColorScheme
    "terminal" -> if (darkTheme) TerminalDarkColorScheme else TerminalLightColorScheme
    else -> HalColorScheme
}

/** Extended (non-Material) colors for [appStyle] at the given brightness. */
fun liveNinjaColors(appStyle: String, darkTheme: Boolean): LiveNinjaColors = when (appStyle) {
    "hal9000" -> HalLiveNinjaColors
    "ninja" -> if (darkTheme) NinjaDarkColors else NinjaLightColors
    "minimal" -> if (darkTheme) MinimalDarkColors else MinimalLightColors
    "terminal" -> if (darkTheme) TerminalDarkColors else TerminalLightColors
    else -> HalLiveNinjaColors
}

/**
 * @param appStyle persisted style key (SettingsStore.appStyle). Defaults to
 *   "hal9000" so HAL renders without any change at the call site.
 * @param darkTheme the resolved light/dark/system theme setting. Ignored under
 *   HAL, which always pins dark (spec §A); honored by ninja/minimal/terminal.
 * @param dynamicColor retained for API compatibility; ignored under HAL
 *   (dynamicColor=false when HAL active — spec §A).
 */
@Composable
fun LiveNinjaTheme(
    appStyle: String = "hal9000",
    darkTheme: Boolean = true,
    @Suppress("UNUSED_PARAMETER") dynamicColor: Boolean = false,
    content: @Composable () -> Unit,
) {
    val effectiveDark = if (appStyle == "hal9000") true else darkTheme
    val colorScheme = liveNinjaColorScheme(appStyle, effectiveDark)
    val extended = liveNinjaColors(appStyle, effectiveDark)
    CompositionLocalProvider(LocalLiveNinjaColors provides extended) {
        MaterialTheme(
            colorScheme = colorScheme,
            content = content,
        )
    }
}
