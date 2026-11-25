package net.pythonscratcher.veil

import android.content.Context

object AppPrefs {
    private const val PREFS = "veil_app"
    private const val KEY_THEME = "theme_mode"

    fun loadThemeMode(ctx: Context): ThemeMode {
        val raw = ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE).getString(KEY_THEME, null)
        return runCatching { ThemeMode.valueOf(raw ?: "SYSTEM") }.getOrDefault(ThemeMode.SYSTEM)
    }

    fun saveThemeMode(ctx: Context, mode: ThemeMode) {
        ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE).edit().putString(KEY_THEME, mode.name).apply()
    }
}
