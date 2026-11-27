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
    data object List : Screen()
    data class Detail(val profileId: String) : Screen()
    data class Manage(val profileId: String) : Screen()
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun VeilApp(onConnect: (String) -> Unit, onDisconnect: () -> Unit) {
    val ctx = androidx.compose.ui.platform.LocalContext.current
    var themeMode by remember { mutableStateOf(AppPrefs.loadThemeMode(ctx)) }
    var profiles by remember { mutableStateOf(ProfileStore.loadAll(ctx)) }
    var activeId by remember { mutableStateOf(ProfileStore.getActiveId(ctx)) }
    var screen by remember { mutableStateOf<Screen>(Screen.List) }
    var showImport by remember { mutableStateOf(false) }
    var statusJson by remember { mutableStateOf<JSONObject?>(null) }

    fun refreshProfiles() { profiles = ProfileStore.loadAll(ctx) }

    LaunchedEffect(Unit) {
        while (true) {
            val c = VeilVpnService.client
            statusJson = if (c != null) runCatching { JSONObject(c.status()) }.getOrNull() else null
            delay(1000)
        }
    }

    VeilTheme(themeMode) {
        Surface(modifier = Modifier.fillMaxSize(), color = MaterialTheme.colorScheme.background) {
            when (val s = screen) {
                is Screen.List -> ServerListScreen(
                    profiles = profiles,
                    activeId = activeId,
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
                    onAddServer = { showImport = true },
                    onOpenServer = { id ->
                        activeId = id
                        ProfileStore.setActiveId(ctx, id)
                        screen = Screen.Detail(id)
                    },
                    connectedId = run {
                        val st = statusJson?.optString("state")
                        if (st == "connected" || st == "connecting") activeId else null
                    },
                )
                is Screen.Detail -> {
                    val profile = profiles.find { it.id == s.profileId }
                    if (profile == null) {
                        screen = Screen.List
                    } else {
                        BackHandler { screen = Screen.List }
                        ServerDetailScreen(
                            profile = profile,
                            status = statusJson,
                            onBack = { screen = Screen.List },
                            onConnect = { onConnect(profile.effectiveConfigJson()) },
                            onDisconnect = onDisconnect,
                            onManage = { screen = Screen.Manage(profile.id) },
                            onSave = { updated ->
                                ProfileStore.update(ctx, updated)
                                refreshProfiles()
                            },
                            onDelete = {
                                ProfileStore.delete(ctx, profile.id)
                                refreshProfiles()
                                activeId = ProfileStore.getActiveId(ctx)
                                screen = Screen.List
                            },
                        )
                    }
                }
                is Screen.Manage -> {
                    val profile = profiles.find { it.id == s.profileId }
                    if (profile == null) {
                        screen = Screen.List
                    } else {
                        BackHandler { screen = Screen.Detail(profile.id) }
                        ManageServerScreen(
                            profile = profile,
                            onBack = { screen = Screen.Detail(profile.id) },
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
                        activeId = ProfileStore.getActiveId(ctx)
                    }
                    showImport = false
                },
            )
        }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun ServerListScreen(
    profiles: List<ServerProfile>,
    activeId: String?,
    themeMode: ThemeMode,
    onCycleTheme: () -> Unit,
    onAddServer: () -> Unit,
    onOpenServer: (String) -> Unit,
    connectedId: String?,
) {
    Scaffold(
        containerColor = MaterialTheme.colorScheme.background,
        topBar = {
            TopAppBar(
                title = {
                    Row(verticalAlignment = Alignment.Bottom) {
                        Text("Veil", fontWeight = FontWeight.Bold)
                        Spacer(Modifier.width(8.dp))
                        Text(
                            "v" + BuildConfig.VERSION_NAME,
                            style = MaterialTheme.typography.labelMedium,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                    }
                },
                colors = TopAppBarDefaults.topAppBarColors(containerColor = MaterialTheme.colorScheme.background),
                actions = {
                    IconButton(onClick = onCycleTheme) {
                        Icon(
                            when (themeMode) {
                                ThemeMode.SYSTEM -> Icons.Filled.BrightnessAuto
                                ThemeMode.DARK -> Icons.Filled.DarkMode
                                ThemeMode.LIGHT -> Icons.Filled.LightMode
                            },
                            contentDescription = "Theme: $themeMode",
                        )
                    }
                },
            )
        },
        floatingActionButton = {
            ExtendedFloatingActionButton(onClick = onAddServer, icon = { Icon(Icons.Filled.Add, null) }, text = { Text("Add server") })
        },
    ) { padding ->
        if (profiles.isEmpty()) {
            Column(
                modifier = Modifier.fillMaxSize().padding(padding),
                horizontalAlignment = Alignment.CenterHorizontally,
                verticalArrangement = Arrangement.Center,
            ) {
                Icon(Icons.Filled.VpnKey, contentDescription = null, modifier = Modifier.size(56.dp), tint = MaterialTheme.colorScheme.outline)
                Spacer(Modifier.height(12.dp))
                Text("No servers yet", style = MaterialTheme.typography.titleMedium)
                Spacer(Modifier.height(4.dp))
                Text(
                    "Import a key to get started",
                    style = MaterialTheme.typography.bodyMedium,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
        } else {
            LazyColumn(
                modifier = Modifier.fillMaxSize().padding(padding),
                contentPadding = PaddingValues(16.dp),
                verticalArrangement = Arrangement.spacedBy(10.dp),
            ) {
                items(profiles, key = { it.id }) { p ->
                    ServerRow(
                        profile = p,
                        isActive = p.id == activeId,
                        isConnected = p.id == connectedId,
                        onClick = { onOpenServer(p.id) },
                    )
                }
            }
        }
    }
}

@Composable
fun ServerRow(profile: ServerProfile, isActive: Boolean, isConnected: Boolean, onClick: () -> Unit) {
    Card(
        onClick = onClick,
        colors = CardDefaults.cardColors(
            containerColor = if (isConnected) MaterialTheme.colorScheme.primaryContainer else MaterialTheme.colorScheme.surface,
        ),
        modifier = Modifier.fillMaxWidth(),
    ) {
        Row(
            modifier = Modifier.fillMaxWidth().padding(16.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Icon(
                if (isConnected) Icons.Filled.Shield else Icons.Filled.ShieldMoon,
                contentDescription = null,
                tint = if (isConnected) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.onSurfaceVariant,
                modifier = Modifier.size(28.dp),
            )
            Spacer(Modifier.width(14.dp))
            Column(Modifier.weight(1f)) {
                Text(profile.name, style = MaterialTheme.typography.titleMedium, fontWeight = FontWeight.SemiBold)
                Text(
                    if (isConnected) "Connected" else profile.serverHost,
                    style = MaterialTheme.typography.bodySmall,
                    color = if (isConnected) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
            Icon(Icons.Filled.ChevronRight, contentDescription = null, tint = MaterialTheme.colorScheme.outline)
        }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun ServerDetailScreen(
    profile: ServerProfile,
    status: JSONObject?,
    onBack: () -> Unit,
    onConnect: () -> Unit,
    onDisconnect: () -> Unit,
    onManage: () -> Unit,
    onSave: (ServerProfile) -> Unit,
    onDelete: () -> Unit,
) {
    var showSettings by remember { mutableStateOf(false) }
    var showMenu by remember { mutableStateOf(false) }
    var showDeleteConfirm by remember { mutableStateOf(false) }

    Scaffold(
        containerColor = MaterialTheme.colorScheme.background,
        topBar = {
            TopAppBar(
                title = { Text(profile.name) },
                navigationIcon = {
                    IconButton(onClick = onBack) { Icon(Icons.Filled.ArrowBack, contentDescription = "Back") }
                },
                actions = {
                    IconButton(onClick = { showSettings = true }) { Icon(Icons.Filled.Tune, contentDescription = "Settings") }
                    IconButton(onClick = { showMenu = true }) { Icon(Icons.Filled.MoreVert, contentDescription = "More") }
                    DropdownMenu(expanded = showMenu, onDismissRequest = { showMenu = false }) {
                        DropdownMenuItem(text = { Text("Manage server") }, onClick = {
                            showMenu = false
                            onManage()
                        }, leadingIcon = { Icon(Icons.Filled.AdminPanelSettings, null) })
                        DropdownMenuItem(text = { Text("Delete server") }, onClick = {
                            showMenu = false
                            showDeleteConfirm = true
                        }, leadingIcon = { Icon(Icons.Filled.Delete, null) })
                    }
                },
                colors = TopAppBarDefaults.topAppBarColors(containerColor = MaterialTheme.colorScheme.background),
            )
        },
    ) { padding ->
        Column(
            modifier = Modifier.fillMaxSize().padding(padding).padding(20.dp).verticalScroll(rememberScrollState()),
        ) {
            StatusCard(status)
            Spacer(Modifier.height(20.dp))

            val state = status?.optString("state") ?: "disconnected"
            val busy = state == "connecting" || state == "disconnecting"
            Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                Button(
                    enabled = !busy && state != "connected",
                    onClick = onConnect,
                    modifier = Modifier.weight(1f),
                ) {
                    Icon(Icons.Filled.Bolt, contentDescription = null, modifier = Modifier.size(18.dp))
                    Spacer(Modifier.width(6.dp))
                    Text(if (state == "connecting") "Connecting…" else "Connect")
                }
                OutlinedButton(
                    enabled = state == "connected" || state == "connecting",
                    onClick = onDisconnect,
                    modifier = Modifier.weight(1f),
                ) { Text(if (state == "disconnecting") "Disconnecting…" else "Disconnect") }
            }
        }

        if (showSettings) {
            SettingsDialog(
                initial = profile,
                onDismiss = { showSettings = false },
                onSave = {
                    onSave(it)
                    showSettings = false
                },
            )
        }
        if (showDeleteConfirm) {
            AlertDialog(
                onDismissRequest = { showDeleteConfirm = false },
                title = { Text("Delete ${profile.name}?") },
                text = { Text("This removes the saved key from this device. It can be re-imported later if needed.") },
                confirmButton = {
                    TextButton(onClick = { showDeleteConfirm = false; onDelete() }) { Text("Delete") }
                },
                dismissButton = { TextButton(onClick = { showDeleteConfirm = false }) { Text("Cancel") } },
            )
        }
    }
}

@Composable
fun StatusCard(status: JSONObject?) {
    val state = status?.optString("state") ?: "disconnected"
    val error = status?.optString("error")
    val bgIcon: Pair<androidx.compose.ui.graphics.Color, ImageVector> = when (state) {
        "connected" -> MaterialTheme.colorScheme.primaryContainer to Icons.Filled.Shield
        "error" -> MaterialTheme.colorScheme.errorContainer to Icons.Filled.ErrorOutline
        else -> MaterialTheme.colorScheme.surfaceVariant to Icons.Filled.ShieldMoon
    }
    val (bg, icon) = bgIcon
    Card(colors = CardDefaults.cardColors(containerColor = bg), modifier = Modifier.fillMaxWidth()) {
        Column(Modifier.padding(18.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Icon(icon, contentDescription = null, modifier = Modifier.size(22.dp))
                Spacer(Modifier.width(10.dp))
                Text(
                    when (state) {
                        "connected" -> "Connected"
                        "connecting" -> "Connecting…"
                        "disconnecting" -> "Disconnecting…"
                        "error" -> "Error"
                        else -> "Disconnected"
                    },
                    style = MaterialTheme.typography.titleMedium,
                    fontWeight = FontWeight.Bold,
                )
            }
            if (state == "error" && !error.isNullOrBlank()) {
                Spacer(Modifier.height(6.dp))
                Text(error, style = MaterialTheme.typography.bodySmall)
            }
            if (state == "connected" && status != null) {
                Spacer(Modifier.height(14.dp))
                val up = status.optLong("up", 0)
                val down = status.optLong("down", 0)
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Icon(Icons.Filled.ArrowUpward, contentDescription = null, modifier = Modifier.size(14.dp))
                    Text(" ${fmtBytes(up)}   ", style = MaterialTheme.typography.bodySmall)
                    Icon(Icons.Filled.ArrowDownward, contentDescription = null, modifier = Modifier.size(14.dp))
                    Text(" ${fmtBytes(down)}", style = MaterialTheme.typography.bodySmall)
                }

                val limits = status.optJSONObject("limits")
                if (limits != null) {
                    Spacer(Modifier.height(12.dp))
                    LimitsRows(limits)
                }
                var refreshing by remember { mutableStateOf(false) }
                androidx.compose.material3.TextButton(
                    enabled = !refreshing,
                    onClick = {
                        refreshing = true
                        Thread {
                            runCatching { VeilVpnService.client?.refreshLimits() }
                            refreshing = false
                        }.start()
                    },
                ) { Text(if (refreshing) "Refreshing…" else "Refresh limits") }
            }
        }
    }
}

@Composable
fun LimitsRows(limits: JSONObject) {
    val expiresAt = limits.optLong("expires_at", 0)
    val dataCap = limits.optLong("data_cap", 0)
    val usedUp = limits.optLong("used_up", 0)
    val usedDown = limits.optLong("used_down", 0)
    val rateMbps = limits.optDouble("rate_mbps", 0.0)

    if (expiresAt > 0) {
        val secLeft = max(0L, expiresAt - (System.currentTimeMillis() / 1000))
        Text("Time left: ${fmtTimeLeft(secLeft)}", style = MaterialTheme.typography.bodySmall)
        Spacer(Modifier.height(6.dp))
    }
    if (dataCap > 0) {
        val used = usedUp + usedDown
        val frac = (used.toDouble() / dataCap.toDouble()).coerceIn(0.0, 1.0)
        Text("Data: ${fmtBytes(used)} / ${fmtBytes(dataCap)} used", style = MaterialTheme.typography.bodySmall)
        LinearProgressIndicator(
            progress = { frac.toFloat() },
            modifier = Modifier.fillMaxWidth().padding(top = 4.dp).height(6.dp),
            color = when {
                frac >= 0.9 -> MaterialTheme.colorScheme.error
                frac >= 0.75 -> MaterialTheme.colorScheme.tertiary
                else -> MaterialTheme.colorScheme.primary
            },
        )
        Spacer(Modifier.height(6.dp))
    }
    if (rateMbps > 0) {
        Text("Speed limit: ${"%.1f".format(rateMbps)} Mbps", style = MaterialTheme.typography.bodySmall)
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

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun SettingsDialog(initial: ServerProfile, onDismiss: () -> Unit, onSave: (ServerProfile) -> Unit) {
    var name by remember { mutableStateOf(initial.name) }
    var transport by remember { mutableStateOf(initial.transport) }
    var carrier by remember { mutableStateOf(initial.carrier) }
    var frag by remember { mutableStateOf(initial.frag) }
    var cover by remember { mutableStateOf(initial.cover) }
    var rotate by remember { mutableStateOf(initial.rotateMinutes.toString()) }
    var dns by remember { mutableStateOf(initial.dns) }
    var insecureTls by remember { mutableStateOf(initial.insecureTls) }

    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("Settings") },
        text = {
            Column(Modifier.verticalScroll(rememberScrollState())) {
                OutlinedTextField(
                    value = name,
                    onValueChange = { name = it },
                    label = { Text("Name") },
                    modifier = Modifier.fillMaxWidth(),
                )
                Spacer(Modifier.height(16.dp))

                Text("Connection method", style = MaterialTheme.typography.labelLarge)
                Column {
                    listOf("tls" to "HTTPS (default)", "ws" to "Plain HTTP / WebSocket (port 80)", "poll" to "Plain HTTP, no upgrade (port 80, slower, works through proxies)", "quic" to "QUIC / HTTP3 (UDP 443, fast, often blocked)").forEach { (opt, label) ->
                        Row(verticalAlignment = Alignment.CenterVertically) {
                            RadioButton(selected = carrier == opt, onClick = { carrier = opt })
                            Text(label)
                        }
                    }
                }
                Spacer(Modifier.height(12.dp))

                Text("Transport", style = MaterialTheme.typography.labelLarge)
                Row {
                    listOf("stream", "split").forEach { opt ->
                        Row(verticalAlignment = Alignment.CenterVertically) {
                            RadioButton(selected = transport == opt, onClick = { transport = opt })
                            Text(opt)
                            Spacer(Modifier.width(12.dp))
                        }
                    }
                }
                Spacer(Modifier.height(12.dp))

                Text("ClientHello fragmentation", style = MaterialTheme.typography.labelLarge)
                Column {
                    listOf("none", "tcp", "record", "both").forEach { opt ->
                        Row(verticalAlignment = Alignment.CenterVertically) {
                            RadioButton(selected = frag == opt, onClick = { frag = opt })
                            Text(opt)
                        }
                    }
                }
                Spacer(Modifier.height(12.dp))

                Row(verticalAlignment = Alignment.CenterVertically) {
                    Checkbox(checked = cover, onCheckedChange = { cover = it })
                    Text("Cover traffic (pad frames + decoy page fetches)")
                }
                Spacer(Modifier.height(12.dp))

                Row(verticalAlignment = Alignment.CenterVertically) {
                    Checkbox(checked = insecureTls, onCheckedChange = { insecureTls = it })
                    Text("Allow replaced certificates (use if the network's DPI swaps the TLS cert; the tunnel stays protected by its own keys)")
                }
                Spacer(Modifier.height(12.dp))

                OutlinedTextField(
                    value = rotate,
                    onValueChange = { rotate = it.filter(Char::isDigit) },
                    label = { Text("Carrier rotation (minutes)") },
                    keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Number),
                    modifier = Modifier.fillMaxWidth(),
                )
                Spacer(Modifier.height(12.dp))

                OutlinedTextField(
                    value = dns,
                    onValueChange = { dns = it },
                    label = { Text("Tunnel DNS (comma-separated)") },
                    modifier = Modifier.fillMaxWidth(),
                )
            }
        },
        confirmButton = {
            TextButton(onClick = {
                onSave(
                    initial.copy(
                        name = name.ifBlank { initial.name },
                        transport = transport,
                        carrier = carrier,
                        frag = frag,
                        cover = cover,
                        rotateMinutes = rotate.toIntOrNull() ?: 20,
                        dns = dns,
                        insecureTls = insecureTls,
                    )
                )
            }) { Text("Save") }
        },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
    )
}
