package net.pythonscratcher.veil

import android.content.Intent
import android.net.VpnService
import android.os.Build
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.BackHandler
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.compose.setContent
import androidx.activity.result.contract.ActivityResultContracts
import com.journeyapps.barcodescanner.ScanContract
import com.journeyapps.barcodescanner.ScanOptions
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.*
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.delay
import org.json.JSONObject
import kotlin.math.max

class MainActivity : ComponentActivity() {

    private var pendingConfigJson: String? = null

    private val vpnPermissionLauncher =
        registerForActivityResult(ActivityResultContracts.StartActivityForResult()) { result ->
            if (result.resultCode == RESULT_OK) {
                pendingConfigJson?.let { startTunnelService(it) }
            }
        }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContent {
            VeilApp(
                onConnect = { configJson -> requestVpnPermissionAndConnect(configJson) },
                onDisconnect = { stopTunnelService() },
            )
        }
    }

    private fun requestVpnPermissionAndConnect(configJson: String) {
        pendingConfigJson = configJson
        val intent = VpnService.prepare(this)
        if (intent != null) {
            vpnPermissionLauncher.launch(intent)
        } else {
            startTunnelService(configJson)
        }
    }

    private fun startTunnelService(configJson: String) {
        val intent = Intent(this, VeilVpnService::class.java).apply {
            action = VeilVpnService.ACTION_CONNECT
            putExtra(VeilVpnService.EXTRA_CONFIG_JSON, configJson)
        }
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            startForegroundService(intent)
        } else {
            startService(intent)
        }
    }

    private fun stopTunnelService() {
        val intent = Intent(this, VeilVpnService::class.java).apply {
            action = VeilVpnService.ACTION_DISCONNECT
        }
        startService(intent)
    }
}

private sealed class Screen {
    data object Home : Screen()
    data class Manage(val profileId: String) : Screen()
}

@Composable
fun VeilApp(onConnect: (String) -> Unit, onDisconnect: () -> Unit) {
    val ctx = androidx.compose.ui.platform.LocalContext.current
    var themeMode by remember { mutableStateOf(AppPrefs.loadThemeMode(ctx)) }
    var profiles by remember { mutableStateOf(ProfileStore.loadAll(ctx)) }
    var activeId by remember { mutableStateOf(ProfileStore.getActiveId(ctx)) }
    var screen by remember { mutableStateOf<Screen>(Screen.Home) }
    var showImport by remember { mutableStateOf(false) }
    var statusJson by remember { mutableStateOf<JSONObject?>(null) }
    var rates by remember { mutableStateOf(Rates()) }
    val version = remember { runCatching { ctx.packageManager.getPackageInfo(ctx.packageName, 0).versionName }.getOrNull() ?: "" }

    fun refreshProfiles() { profiles = ProfileStore.loadAll(ctx) }

    LaunchedEffect(Unit) {
        var lastUp = 0L; var lastDown = 0L; var lastT = 0L
        while (true) {
            val c = VeilVpnService.client
            val st = if (c != null) runCatching { JSONObject(c.status()) }.getOrNull() else null
            val now = System.currentTimeMillis()
            if (st != null && st.optString("state") == "connected" && lastT > 0) {
                val dt = (now - lastT) / 1000.0
                val up = max(0L, st.optLong("up") - lastUp) / dt
                val down = max(0L, st.optLong("down") - lastDown) / dt
                rates = Rates(0.6 * rates.up + 0.4 * up, 0.6 * rates.down + 0.4 * down)
            } else if (st?.optString("state") != "connected") rates = Rates()
            lastUp = st?.optLong("up") ?: 0; lastDown = st?.optLong("down") ?: 0; lastT = now
            statusJson = st
            delay(1000)
        }
    }

    VeilTheme(themeMode) {
        Surface(modifier = Modifier.fillMaxSize().systemBarsPadding(), color = MaterialTheme.colorScheme.background) {
            when (val s = screen) {
                is Screen.Home -> {
                    val active = profiles.find { it.id == activeId } ?: profiles.firstOrNull()
                    val busy = statusJson?.optString("state").let { it == "connected" || it == "connecting" }
                    HomeScreen(
                        profiles = profiles,
                        active = active,
                        status = statusJson,
                        rates = rates,
                        version = version,
                        themeMode = themeMode,
                        onCycleTheme = {
                            val next = when (themeMode) {
                                ThemeMode.SYSTEM -> ThemeMode.DARK
                                ThemeMode.DARK -> ThemeMode.LIGHT
                                ThemeMode.LIGHT -> ThemeMode.SYSTEM
                            }
                            themeMode = next
                            AppPrefs.saveThemeMode(ctx, next)
                        },
                        onSelect = { id ->
                            if (busy && id != active?.id) {
                                android.widget.Toast.makeText(ctx, "Disconnect first to switch servers", android.widget.Toast.LENGTH_SHORT).show()
                            } else {
                                activeId = id
                                ProfileStore.setActiveId(ctx, id)
                            }
                        },
                        onAddServer = { showImport = true },
                        onConnect = { active?.let { onConnect(it.effectiveConfigJson()) } },
                        onDisconnect = onDisconnect,
                        onSave = { updated -> ProfileStore.update(ctx, updated); refreshProfiles() },
                        onDelete = {
                            active?.let {
                                if (busy) onDisconnect()
                                ProfileStore.delete(ctx, it.id)
                            }
                            refreshProfiles()
                            activeId = ProfileStore.getActiveId(ctx)
                        },
                        onManage = { active?.let { screen = Screen.Manage(it.id) } },
                    )
                }
                is Screen.Manage -> {
                    val profile = profiles.find { it.id == s.profileId }
                    if (profile == null) {
                        screen = Screen.Home
                    } else {
                        BackHandler { screen = Screen.Home }
                        ManageServerScreen(
                            profile = profile,
                            onBack = { screen = Screen.Home },
                            onSaveProfile = { updated ->
                                ProfileStore.update(ctx, updated)
                                refreshProfiles()
                            },
                            onImportKey = { json ->
                                val p = ServerProfile.fromImport(json)
                                if (p != null) {
                                    ProfileStore.add(ctx, p)
                                    refreshProfiles()
                                    android.widget.Toast.makeText(ctx, "Key added to this phone", android.widget.Toast.LENGTH_SHORT).show()
                                }
                            },
                        )
                    }
                }
            }
        }

        if (showImport) {
            ImportKeyDialog(
                onDismiss = { showImport = false },
                onImport = { json ->
                    val p = ServerProfile.fromImport(json)
                    if (p != null) {
                        ProfileStore.add(ctx, p)
                        refreshProfiles()
                        activeId = p.id
                        ProfileStore.setActiveId(ctx, p.id)
                    }
                    showImport = false
                },
            )
        }
    }
}

fun fmtBytes(n: Long): String {
    val units = listOf("B", "KB", "MB", "GB", "TB")
    var v = n.toDouble()
    var i = 0
    while (v >= 1024 && i < units.size - 1) {
        v /= 1024; i++
    }
    return if (i == 0) "$n B" else "%.1f %s".format(v, units[i])
}

fun fmtTimeLeft(sec: Long): String {
    if (sec <= 0) return "expired"
    val d = sec / 86400
    val h = (sec % 86400) / 3600
    val m = (sec % 3600) / 60
    return when {
        d > 0 -> "${d}d ${h}h left"
        h > 0 -> "${h}h ${m}m left"
        m > 0 -> "${m}m left"
        else -> "less than a minute left"
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun ImportKeyDialog(onDismiss: () -> Unit, onImport: (String) -> Unit) {
    var text by remember { mutableStateOf("") }
    var error by remember { mutableStateOf<String?>(null) }
    val scanner = rememberLauncherForActivityResult(ScanContract()) { res ->
        val t = res.contents?.trim()
        if (t != null) {
            val parsed = runCatching { JSONObject(t) }.getOrNull()
            if (parsed == null || !parsed.has("server_pub") || !parsed.has("psk") || !parsed.has("server")) {
                error = "That QR code is not a Veil key."
            } else {
                onImport(t)
            }
        }
    }
    AlertDialog(
        onDismissRequest = onDismiss,
        icon = { Icon(Icons.Filled.VpnKey, contentDescription = null) },
        title = { Text("Import key") },
        text = {
            Column {
                Button(
                    onClick = {
                        scanner.launch(
                            ScanOptions()
                                .setDesiredBarcodeFormats(ScanOptions.QR_CODE)
                                .setPrompt("Scan the key's QR code from the admin panel")
                                .setBeepEnabled(false)
                                .setOrientationLocked(false)
                        )
                    },
                    modifier = Modifier.fillMaxWidth(),
                ) {
                    Icon(Icons.Filled.QrCodeScanner, contentDescription = null)
                    Spacer(Modifier.width(8.dp))
                    Text("Scan QR code")
                }
                Spacer(Modifier.height(12.dp))
                Text(
                    "Or paste the client.json key exported from the server's admin panel.",
                    style = MaterialTheme.typography.bodySmall,
                )
                Spacer(Modifier.height(8.dp))
                OutlinedTextField(
                    value = text,
                    onValueChange = { text = it; error = null },
                    modifier = Modifier.fillMaxWidth().height(180.dp),
                    label = { Text("Key JSON") },
                )
                if (error != null) {
                    Spacer(Modifier.height(6.dp))
                    Text(error!!, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall)
                }
            }
        },
        confirmButton = {
            TextButton(onClick = {
                val t = text.trim()
                val parsed = runCatching { JSONObject(t) }.getOrNull()
                if (parsed == null || !parsed.has("server_pub") || !parsed.has("psk") || !parsed.has("server")) {
                    error = "That doesn't look like a valid key (missing server/server_pub/psk)."
                } else {
                    onImport(t)
                }
            }) { Text("Import") }
        },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
    )
}

