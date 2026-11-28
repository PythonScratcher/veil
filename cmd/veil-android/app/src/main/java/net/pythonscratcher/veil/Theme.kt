package net.pythonscratcher.veil

import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.ui.graphics.Color

// A small, deliberately calm palette - deep indigo/slate rather than default Material purple, so
// the app reads as a security tool rather than a generic template.
private val VeilBlue = Color(0xFFA78BFA)
private val VeilBlueDark = Color(0xFF6A4BE0)
private val VeilGreen = Color(0xFF69DB9C)
private val VeilAmber = Color(0xFFE8B339)
private val VeilRed = Color(0xFFE5676B)

private val DarkScheme = darkColorScheme(
    primary = VeilBlue,
    onPrimary = Color(0xFF1A1033),
    primaryContainer = Color(0xFF2C2357),
    onPrimaryContainer = Color(0xFFE9E2FF),
    secondary = VeilGreen,
    tertiary = VeilAmber,
    error = VeilRed,
    errorContainer = Color(0xFF4A1D20),
    onErrorContainer = Color(0xFFFFD9DA),
    background = Color(0xFF0F1117),
    onBackground = Color(0xFFE7E9F1),
    surface = Color(0xFF171A23),
    onSurface = Color(0xFFE7E9F1),
    surfaceVariant = Color(0xFF1E2230),
    onSurfaceVariant = Color(0xFF8D93AB),
    outline = Color(0xFF2A2F42),
)

private val LightScheme = lightColorScheme(
    primary = VeilBlueDark,
    onPrimary = Color.White,
    primaryContainer = Color(0xFFE9E2FF),
    onPrimaryContainer = Color(0xFF24135E),
    secondary = Color(0xFF1F8F5C),
    tertiary = Color(0xFF9C6F00),
    error = Color(0xFFBA1A1A),
    background = Color(0xFFF7F8FA),
    onBackground = Color(0xFF1A1C1E),
    surface = Color.White,
    onSurface = Color(0xFF1A1C1E),
    surfaceVariant = Color(0xFFEAEDF2),
    onSurfaceVariant = Color(0xFF444950),
    outline = Color(0xFFCACED6),
)

enum class ThemeMode { SYSTEM, LIGHT, DARK }

@Composable
fun VeilTheme(mode: ThemeMode, content: @Composable () -> Unit) {
    val dark = when (mode) {
        ThemeMode.SYSTEM -> isSystemInDarkTheme()
        ThemeMode.DARK -> true
        ThemeMode.LIGHT -> false
    }
    MaterialTheme(
        colorScheme = if (dark) DarkScheme else LightScheme,
        content = content,
    )
}
