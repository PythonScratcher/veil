package net.pythonscratcher.veil

import android.content.ClipData
import android.content.ClipboardManager
import android.content.Context
import androidx.compose.foundation.Image
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.*
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.asImageBitmap
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.launch
import org.json.JSONObject

private const val GB = 1_000_000_000L

private fun statusColor(status: String): Color = when (status) {
    "active" -> Color(0xFF3DBE7A)
    "disabled" -> Color(0xFF8A93A3)
    else -> Color(0xFFE5A93D)
}

private fun fmtExpiry(ts: Long): String {
    if (ts <= 0) return "never expires"
    val left = ts - System.currentTimeMillis() / 1000
    return if (left <= 0) "expired" else fmtTimeLeft(left) + " left"
}

/** Manage the server this profile points at: log in to its admin panel, then edit keys. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun ManageServerScreen(
    profile: ServerProfile,
    onBack: () -> Unit,
    onSaveProfile: (ServerProfile) -> Unit,
    onImportKey: (String) -> Unit,
) {
    val scope = rememberCoroutineScope()
    var api by remember { mutableStateOf<AdminApi?>(null) }
    var keys by remember { mutableStateOf<List<AdminKey>>(emptyList()) }
    var info by remember { mutableStateOf<JSONObject?>(null) }
    var loading by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }
    var selected by remember { mutableStateOf<AdminKey?>(null) }
    var editing by remember { mutableStateOf<AdminKey?>(null) }
    var creating by remember { mutableStateOf(false) }
    var showPassword by remember { mutableStateOf(false) }
    var showMenu by remember { mutableStateOf(false) }

    val caPem = remember(profile.configJson) {
        runCatching { JSONObject(profile.configJson).optString("ca_pem") }.getOrNull()?.ifBlank { null }
    }

    fun reload(a: AdminApi) {
        scope.launch {
            loading = true
            try {
                keys = a.listKeys()
                info = a.serverInfo()
                error = null
            } catch (e: AdminException) {
                error = e.message
                if (e.code == 401) api = null
            } finally {
                loading = false
            }
        }
    }

    Scaffold(
        containerColor = MaterialTheme.colorScheme.background,
        topBar = {
            TopAppBar(
                title = { Text("Manage ${profile.name}") },
                navigationIcon = {
                    IconButton(onClick = {
                        api?.let { a -> scope.launch { runCatching { a.logout() } } }
                        onBack()
                    }) { Icon(Icons.Filled.ArrowBack, contentDescription = "Back") }
                },
                actions = {
                    if (api != null) {
                        IconButton(onClick = { api?.let { reload(it) } }) { Icon(Icons.Filled.Refresh, contentDescription = "Refresh") }
                        IconButton(onClick = { showMenu = true }) { Icon(Icons.Filled.MoreVert, contentDescription = "More") }
                        DropdownMenu(expanded = showMenu, onDismissRequest = { showMenu = false }) {
                            DropdownMenuItem(
                                text = { Text("Change admin password") },
                                leadingIcon = { Icon(Icons.Filled.Password, null) },
                                onClick = { showMenu = false; showPassword = true },
                            )
                            DropdownMenuItem(
                                text = { Text("Log out") },
                                leadingIcon = { Icon(Icons.Filled.Logout, null) },
                                onClick = {
                                    showMenu = false
                                    api?.let { a -> scope.launch { runCatching { a.logout() } } }
                                    api = null
                                    keys = emptyList()
                                },
                            )
                        }
                    }
                },
                colors = TopAppBarDefaults.topAppBarColors(containerColor = MaterialTheme.colorScheme.background),
            )
        },
        floatingActionButton = {
            if (api != null) {
                ExtendedFloatingActionButton(
                    onClick = { creating = true },
                    icon = { Icon(Icons.Filled.Add, null) },
                    text = { Text("New key") },
                )
            }
        },
    ) { padding ->
        Box(Modifier.fillMaxSize().padding(padding)) {
            val a = api
            if (a == null) {
                AdminLogin(
                    profile = profile,
                    hasCa = caPem != null,
                    busy = loading,
                    error = error,
                    onLogin = { url, pw, trustAny ->
                        scope.launch {
                            loading = true
                            error = null
                            val candidate = AdminApi(url.trim(), caPem, trustAny)
                            try {
                                candidate.login(pw)
                                onSaveProfile(profile.copy(adminUrl = url.trim(), adminTrustAny = trustAny))
                                api = candidate
                                keys = candidate.listKeys()
                                info = candidate.serverInfo()
                            } catch (e: AdminException) {
                                error = e.message
                            } catch (e: Exception) {
                                error = "Invalid address: ${e.message}"
                            } finally {
                                loading = false
                            }
                        }
                    },
                )
            } else {
                Column(Modifier.fillMaxSize().padding(horizontal = 16.dp)) {
                    info?.let { i ->
                        Card(Modifier.fillMaxWidth(), colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceVariant)) {
                            Row(Modifier.padding(16.dp), horizontalArrangement = Arrangement.SpaceBetween) {
                                Stat("Keys", i.optInt("users").toString())
                                Stat("Connected", i.optInt("conns").toString())
                                Stat("Traffic", fmtBytes(i.optLong("total_up") + i.optLong("total_down")))
                            }
                        }
                        Spacer(Modifier.height(12.dp))
                    }
                    if (error != null) {
                        Text(error!!, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall)
                        Spacer(Modifier.height(8.dp))
                    }
                    if (loading) LinearProgressIndicator(Modifier.fillMaxWidth())
                    LazyColumn(verticalArrangement = Arrangement.spacedBy(10.dp), contentPadding = PaddingValues(bottom = 96.dp)) {
                        items(keys, key = { it.id }) { k -> KeyRow(k) { selected = k } }
                        if (keys.isEmpty() && !loading) {
                            item { Text("No keys yet. Tap “New key” to create one.", color = MaterialTheme.colorScheme.onSurfaceVariant) }
                        }
                    }
                }
            }
        }

        val a = api
        if (a != null) {
            selected?.let { k ->
                KeyDetailDialog(
                    api = a,
                    key = k,
                    onDismiss = { selected = null },
                    onEdit = { selected = null; editing = k },
                    onChanged = { reload(a) },
                    onImport = { json -> onImportKey(json); selected = null },
                    onError = { error = it },
                )
            }
            editing?.let { k ->
                KeyEditDialog(
                    initial = k,
                    onDismiss = { editing = null },
                    onSave = { fields ->
                        scope.launch {
                            try { a.updateKey(k.id, fields); error = null } catch (e: AdminException) { error = e.message }
                            editing = null
                            reload(a)
                        }
                    },
                )
            }
            if (creating) {
                KeyEditDialog(
                    initial = null,
                    onDismiss = { creating = false },
                    onSave = { fields ->
                        scope.launch {
                            try { a.createKey(fields); error = null } catch (e: AdminException) { error = e.message }
                            creating = false
                            reload(a)
                        }
                    },
                )
            }
            if (showPassword) {
                PasswordDialog(
                    onDismiss = { showPassword = false },
                    onSubmit = { cur, new ->
                        a.changePassword(cur, new)
                        showPassword = false
                    },
                )
            }
        }
    }
}

@Composable
private fun Stat(label: String, value: String) {
    Column {
        Text(value, style = MaterialTheme.typography.titleLarge, fontWeight = FontWeight.Bold)
        Text(label, style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
    }
}

@Composable
private fun AdminLogin(
    profile: ServerProfile,
    hasCa: Boolean,
    busy: Boolean,
    error: String?,
    onLogin: (url: String, password: String, trustAny: Boolean) -> Unit,
) {
    val host = remember(profile.serverHost) { profile.serverHost.substringBeforeLast(':') }
    var url by remember { mutableStateOf(profile.adminUrl.ifBlank { "https://$host:8088/" }) }
    var pw by remember { mutableStateOf("") }
    var trustAny by remember { mutableStateOf(profile.adminTrustAny) }
    Column(Modifier.fillMaxSize().padding(20.dp).verticalScroll(rememberScrollState())) {
        Text("Sign in to the admin panel", style = MaterialTheme.typography.titleMedium)
        Spacer(Modifier.height(4.dp))
        Text(
            "Use the full admin address, including the secret path shown when the server was set up (…:8088/console-…/).",
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
        Spacer(Modifier.height(16.dp))
        OutlinedTextField(
            value = url, onValueChange = { url = it }, label = { Text("Admin panel address") },
            singleLine = true, modifier = Modifier.fillMaxWidth(),
            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Uri),
        )
        Spacer(Modifier.height(10.dp))
        OutlinedTextField(
            value = pw, onValueChange = { pw = it }, label = { Text("Admin password") },
            singleLine = true, modifier = Modifier.fillMaxWidth(),
            visualTransformation = PasswordVisualTransformation(),
            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Password),
        )
        Spacer(Modifier.height(6.dp))
        Row(verticalAlignment = Alignment.CenterVertically) {
            Checkbox(checked = trustAny, onCheckedChange = { trustAny = it })
            Text(
                if (hasCa) "Trust any certificate (otherwise this server's own certificate is used)" else "Trust any certificate (not recommended)",
                style = MaterialTheme.typography.bodySmall,
            )
        }
        if (error != null) {
            Spacer(Modifier.height(8.dp))
            Text(error, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall)
        }
        Spacer(Modifier.height(16.dp))
        Button(enabled = !busy && url.isNotBlank() && pw.isNotEmpty(), onClick = { onLogin(url, pw, trustAny) }, modifier = Modifier.fillMaxWidth()) {
            if (busy) CircularProgressIndicator(Modifier.size(18.dp), strokeWidth = 2.dp) else Text("Log in")
        }
        Spacer(Modifier.height(10.dp))
        Text(
            "The admin panel is reached directly, not through the VPN tunnel, so port 8088 must be reachable from this network.",
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }
}

@Composable
private fun KeyRow(k: AdminKey, onClick: () -> Unit) {
    Card(
        Modifier.fillMaxWidth().clickable(onClick = onClick),
        colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceVariant),
    ) {
        Column(Modifier.padding(14.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Box(Modifier.size(10.dp).background(statusColor(k.status), RoundedCornerShape(50)))
                Spacer(Modifier.width(8.dp))
                Text(k.name, fontWeight = FontWeight.SemiBold, modifier = Modifier.weight(1f))
                Text(k.status, style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
            }
            Spacer(Modifier.height(6.dp))
            val cap = if (k.dataCap > 0) fmtBytes(k.dataCap) else "unlimited"
            Text(
                "${fmtBytes(k.used)} of $cap · ${fmtExpiry(k.expiresAt)}" + if (k.conns > 0) " · ${k.conns} online" else "",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            if (k.dataCap > 0) {
                Spacer(Modifier.height(6.dp))
                LinearProgressIndicator(progress = { (k.used.toFloat() / k.dataCap).coerceIn(0f, 1f) }, modifier = Modifier.fillMaxWidth())
            }
        }
    }
}

@Composable
private fun KeyDetailDialog(
    api: AdminApi,
    key: AdminKey,
    onDismiss: () -> Unit,
    onEdit: () -> Unit,
    onChanged: () -> Unit,
    onImport: (String) -> Unit,
    onError: (String) -> Unit,
) {
    val ctx = LocalContext.current
    val scope = rememberCoroutineScope()
    var cfg by remember { mutableStateOf<String?>(null) }
    var cfgError by remember { mutableStateOf<String?>(null) }
    var confirm by remember { mutableStateOf<String?>(null) } // "delete" | "regen" | "reset"
    LaunchedEffect(key.id) {
        try { cfg = api.clientConfig(key.id) } catch (e: AdminException) { cfgError = e.message }
    }
    val compact = remember(cfg) { cfg?.let { runCatching { AdminApi.compactKey(it) }.getOrNull() } }
    val qr = remember(compact) { compact?.let { AdminApi.qrBitmap(it) } }

    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text(key.name) },
        text = {
            Column(Modifier.verticalScroll(rememberScrollState())) {
                val cap = if (key.dataCap > 0) fmtBytes(key.dataCap) else "unlimited"
                Text("Status: ${key.status}", style = MaterialTheme.typography.bodyMedium)
                Text("Used: ${fmtBytes(key.used)} of $cap", style = MaterialTheme.typography.bodyMedium)
                Text("Expiry: ${fmtExpiry(key.expiresAt)}", style = MaterialTheme.typography.bodyMedium)
                Text(
                    "Speed limit: " + (if (key.rateMbps > 0) "${key.rateMbps} Mb/s" else "none") +
                        " · Devices: " + (if (key.maxDevices > 0) key.maxDevices.toString() else "any"),
                    style = MaterialTheme.typography.bodyMedium,
                )
                if (key.note.isNotBlank()) Text("Note: ${key.note}", style = MaterialTheme.typography.bodyMedium)
                Spacer(Modifier.height(12.dp))
                when {
                    qr != null -> {
                        Text("Scan with Veil on another phone:", style = MaterialTheme.typography.labelLarge)
                        Spacer(Modifier.height(6.dp))
                        Image(
                            bitmap = qr.asImageBitmap(),
                            contentDescription = "Key QR code",
                            modifier = Modifier.size(230.dp).background(Color.White, RoundedCornerShape(8.dp)).padding(6.dp).align(Alignment.CenterHorizontally),
                        )
                    }
                    cfgError != null -> Text(cfgError!!, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall)
                    cfg == null -> LinearProgressIndicator(Modifier.fillMaxWidth())
                    else -> Text("This key is too large for a QR code. Use Copy instead.", style = MaterialTheme.typography.bodySmall)
                }
                Spacer(Modifier.height(10.dp))
                Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    OutlinedButton(enabled = cfg != null, onClick = {
                        val cm = ctx.getSystemService(Context.CLIPBOARD_SERVICE) as ClipboardManager
                        cm.setPrimaryClip(ClipData.newPlainText("Veil key", cfg))
                    }) { Text("Copy") }
                    OutlinedButton(enabled = compact != null, onClick = { compact?.let(onImport) }) { Text("Add to this phone") }
                }
                Spacer(Modifier.height(10.dp))
                Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    OutlinedButton(onClick = onEdit) { Text("Edit") }
                    OutlinedButton(onClick = { confirm = "reset" }) { Text("Reset usage") }
                }
                Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    TextButton(onClick = { confirm = "regen" }) { Text("New secret") }
                    TextButton(onClick = { confirm = "delete" }) { Text("Delete", color = MaterialTheme.colorScheme.error) }
                }
            }
        },
        confirmButton = { TextButton(onClick = onDismiss) { Text("Done") } },
    )

    confirm?.let { what ->
        val (title, body) = when (what) {
            "delete" -> "Delete “${key.name}”?" to "Its devices are cut off immediately."
            "regen" -> "Generate a new secret?" to "The old key stops working at once; devices need the new one."
            else -> "Reset usage?" to "Sets this key's used data back to zero."
        }
        AlertDialog(
            onDismissRequest = { confirm = null },
            title = { Text(title) },
            text = { Text(body) },
            confirmButton = {
                TextButton(onClick = {
                    confirm = null
                    scope.launch {
                        try {
                            when (what) {
                                "delete" -> { api.deleteKey(key.id); onDismiss() }
                                "regen" -> { api.regenKey(key.id); cfg = api.clientConfig(key.id) }
                                else -> api.resetUsage(key.id)
                            }
                            onChanged()
                        } catch (e: AdminException) {
                            onError(e.message ?: "failed")
                        }
                    }
                }) { Text("Confirm") }
            },
            dismissButton = { TextButton(onClick = { confirm = null }) { Text("Cancel") } },
        )
    }
}

@Composable
private fun KeyEditDialog(initial: AdminKey?, onDismiss: () -> Unit, onSave: (JSONObject) -> Unit) {
    var name by remember { mutableStateOf(initial?.name ?: "") }
    var note by remember { mutableStateOf(initial?.note ?: "") }
    var enabled by remember { mutableStateOf(initial?.enabled ?: true) }
    var neverExpires by remember { mutableStateOf(initial == null || (initial.expiresAt == 0L)) }
    var days by remember { mutableStateOf("") }
    var capGb by remember { mutableStateOf(initial?.dataCap?.takeIf { it > 0 }?.let { (it.toDouble() / GB).toString().removeSuffix(".0") } ?: "") }
    var rate by remember { mutableStateOf(initial?.rateMbps?.takeIf { it > 0 }?.toString()?.removeSuffix(".0") ?: "") }
    var devices by remember { mutableStateOf(initial?.maxDevices?.takeIf { it > 0 }?.toString() ?: "") }
    var resetDays by remember { mutableStateOf(initial?.resetDays?.takeIf { it > 0 }?.toString() ?: "") }
    var err by remember { mutableStateOf<String?>(null) }
    val num = KeyboardOptions(keyboardType = KeyboardType.Decimal)

    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text(if (initial == null) "New key" else "Edit key") },
        text = {
            Column(Modifier.verticalScroll(rememberScrollState())) {
                OutlinedTextField(value = name, onValueChange = { name = it }, label = { Text("Name") }, singleLine = true, modifier = Modifier.fillMaxWidth())
                Spacer(Modifier.height(8.dp))
                OutlinedTextField(value = note, onValueChange = { note = it }, label = { Text("Note (optional)") }, modifier = Modifier.fillMaxWidth())
                Spacer(Modifier.height(8.dp))
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Switch(checked = enabled, onCheckedChange = { enabled = it })
                    Spacer(Modifier.width(8.dp))
                    Text("Enabled")
                }
                Spacer(Modifier.height(8.dp))
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Checkbox(checked = neverExpires, onCheckedChange = { neverExpires = it })
                    Text("Never expires")
                }
                if (!neverExpires) {
                    OutlinedTextField(
                        value = days, onValueChange = { days = it.filter { c -> c.isDigit() || c == '.' } },
                        label = { Text(if (initial == null) "Valid for (days)" else "Extend by (days)") },
                        singleLine = true, keyboardOptions = num, modifier = Modifier.fillMaxWidth(),
                    )
                }
                Spacer(Modifier.height(8.dp))
                OutlinedTextField(
                    value = capGb, onValueChange = { capGb = it.filter { c -> c.isDigit() || c == '.' } },
                    label = { Text("Data cap in GB (blank = unlimited)") }, singleLine = true, keyboardOptions = num, modifier = Modifier.fillMaxWidth(),
                )
                Spacer(Modifier.height(8.dp))
                OutlinedTextField(
                    value = rate, onValueChange = { rate = it.filter { c -> c.isDigit() || c == '.' } },
                    label = { Text("Speed limit in Mb/s (blank = none)") }, singleLine = true, keyboardOptions = num, modifier = Modifier.fillMaxWidth(),
                )
                Spacer(Modifier.height(8.dp))
                OutlinedTextField(
                    value = devices, onValueChange = { devices = it.filter(Char::isDigit) },
                    label = { Text("Max devices (blank = any)") }, singleLine = true, keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Number), modifier = Modifier.fillMaxWidth(),
                )
                Spacer(Modifier.height(8.dp))
                OutlinedTextField(
                    value = resetDays, onValueChange = { resetDays = it.filter(Char::isDigit) },
                    label = { Text("Reset usage every N days (blank = never)") }, singleLine = true, keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Number), modifier = Modifier.fillMaxWidth(),
                )
                if (err != null) {
                    Spacer(Modifier.height(6.dp))
                    Text(err!!, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall)
                }
            }
        },
        confirmButton = {
            TextButton(onClick = {
                if (name.isBlank()) { err = "Enter a name"; return@TextButton }
                val f = JSONObject()
                f.put("name", name.trim())
                f.put("note", note)
                f.put("enabled", enabled)
                f.put("data_cap_bytes", ((capGb.toDoubleOrNull() ?: 0.0) * GB).toLong())
                f.put("rate_mbps", rate.toDoubleOrNull() ?: 0.0)
                f.put("max_devices", devices.toIntOrNull() ?: 0)
                f.put("reset_days", resetDays.toIntOrNull() ?: 0)
                val d = days.toDoubleOrNull()
                when {
                    neverExpires -> f.put("expires_at", 0)
                    d != null && d > 0 && initial == null -> f.put("expires_at", System.currentTimeMillis() / 1000 + (d * 86400).toLong())
                    d != null && d > 0 -> f.put("add_days", d)
                }
                onSave(f)
            }) { Text("Save") }
        },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
    )
}

@Composable
private fun PasswordDialog(onDismiss: () -> Unit, onSubmit: suspend (String, String) -> Unit) {
    val scope = rememberCoroutineScope()
    var cur by remember { mutableStateOf("") }
    var n1 by remember { mutableStateOf("") }
    var n2 by remember { mutableStateOf("") }
    var err by remember { mutableStateOf<String?>(null) }
    var busy by remember { mutableStateOf(false) }
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("Change admin password") },
        text = {
            Column {
                OutlinedTextField(value = cur, onValueChange = { cur = it }, label = { Text("Current password") }, singleLine = true, visualTransformation = PasswordVisualTransformation(), modifier = Modifier.fillMaxWidth())
                Spacer(Modifier.height(8.dp))
                OutlinedTextField(value = n1, onValueChange = { n1 = it }, label = { Text("New password (min. 10 characters)") }, singleLine = true, visualTransformation = PasswordVisualTransformation(), modifier = Modifier.fillMaxWidth())
                Spacer(Modifier.height(8.dp))
                OutlinedTextField(value = n2, onValueChange = { n2 = it }, label = { Text("Repeat new password") }, singleLine = true, visualTransformation = PasswordVisualTransformation(), modifier = Modifier.fillMaxWidth())
                if (err != null) {
                    Spacer(Modifier.height(6.dp))
                    Text(err!!, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall)
                }
            }
        },
        confirmButton = {
            TextButton(enabled = !busy, onClick = {
                if (n1 != n2) { err = "New passwords do not match"; return@TextButton }
                scope.launch {
                    busy = true
                    try { onSubmit(cur, n1) } catch (e: AdminException) { err = e.message } finally { busy = false }
                }
            }) { Text("Change") }
        },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
    )
}
