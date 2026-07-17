package ninja.jeremy.liveninja.ui.theme

import android.os.Build
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.dynamicDarkColorScheme
import androidx.compose.material3.dynamicLightColorScheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalContext

private val NinjaPurple = Color(0xFF6750A4)
private val NinjaPurpleLight = Color(0xFFD0BCFF)
private val NinjaTealDark = Color(0xFF03DAC6)

private val DarkColors = darkColorScheme(
    primary = NinjaPurpleLight,
    secondary = NinjaTealDark,
)

private val LightColors = lightColorScheme(
    primary = NinjaPurple,
    secondary = Color(0xFF625B71),
)

@Composable
fun LiveNinjaTheme(
    darkTheme: Boolean = isSystemInDarkTheme(),
    // Dynamic (Material You) color on Android 12+, brand palette below that.
    dynamicColor: Boolean = true,
    content: @Composable () -> Unit,
) {
    val colorScheme = when {
        dynamicColor && Build.VERSION.SDK_INT >= Build.VERSION_CODES.S -> {
            val context = LocalContext.current
            if (darkTheme) dynamicDarkColorScheme(context) else dynamicLightColorScheme(context)
        }
        darkTheme -> DarkColors
        else -> LightColors
    }
    MaterialTheme(
        colorScheme = colorScheme,
        content = content,
    )
}
