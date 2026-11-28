package net.pythonscratcher.veil

import androidx.compose.animation.AnimatedVisibility
import androidx.compose.animation.core.*
import androidx.compose.foundation.BorderStroke
import androidx.compose.foundation.Canvas
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.interaction.MutableInteractionSource
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.*
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.draw.drawBehind
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.geometry.Size
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.Path
import androidx.compose.ui.graphics.StrokeCap
import androidx.compose.ui.graphics.StrokeJoin
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import org.json.JSONObject
import kotlin.math.max

/** Colours shared with the Windows/macOS app. */
object VeilColors {
    val Accent = Color(0xFFA78BFA)
    val Accent2 = Color(0xFF7C5CF0)
    val Ok = Color(0xFF4ADE80)
    val Warn = Color(0xFFFBBF24)
    val Bad = Color(0xFFF87171)
}

data class Rates(val up: Double = 0.0, val down: Double = 0.0)

private fun stateLabel(status: JSONObject?): Pair<String, Color> {
    val st = status?.optString("state") ?: "disconnected"
    return when (st) {
        "connected" -> if (status?.optBoolean("carrier", true) == false) "Reconnecting" to VeilColors.Warn else "Connected" to VeilColors.Ok
        "connecting" -> "Connecting" to VeilColors.Warn
        "disconnecting" -> "Disconnecting" to VeilColors.Warn
        "error" -> "Error" to VeilColors.Bad
        else -> "Disconnected" to Color(0xFF8D93AB)
    }
}

@Composable
fun HomeScreen(
    profiles: List<ServerProfile>,
    active: ServerProfile?,
    status: JSONObject?,
    rates: Rates,
    version: String,
    themeMode: ThemeMode,
    onCycleTheme: () -> Unit,
    onSelect: (String) -> Unit,
    onAddServer: () -> Unit,
    onConnect: () -> Unit,
    onDisconnect: () -> Unit,
    onSave: (ServerProfile) -> Unit,
    onDelete: () -> Unit,
    onManage: () -> Unit,
) {
    val state = status?.optString("state") ?: "disconnected"
    var pickerOpen by remember { mutableStateOf(false) }
    Column(
        Modifier.fillMaxSize().verticalScroll(rememberScrollState()).padding(horizontal = 18.dp, vertical = 14.dp),
        verticalArrangement = Arrangement.spacedBy(12.dp),
    ) {
        // header
        Row(verticalAlignment = Alignment.CenterVertically) {
            VeilLogo(Modifier.size(28.dp))
            Spacer(Modifier.width(10.dp))
            Text("Veil", fontSize = 21.sp, fontWeight = FontWeight.Bold, modifier = Modifier.weight(1f))
            val (lbl, col) = stateLabel(status)
            StatusPill(lbl, col)
        }

        if (active == null) {
            EmptyCard(onAddServer)
        } else {
            ServerPicker(profiles, active, pickerOpen, { pickerOpen = !pickerOpen }, { pickerOpen = false; onSelect(it) }, { pickerOpen = false; onAddServer() })

            // hero
            Column(Modifier.fillMaxWidth(), horizontalAlignment = Alignment.CenterHorizontally) {
                Spacer(Modifier.height(6.dp))
                PowerButton(status) {
                    when (state) {
                        "connected", "connecting" -> onDisconnect()
                        "disconnecting" -> {}
                        else -> onConnect()
                    }
                }
                Spacer(Modifier.height(12.dp))
                val (lbl, col) = stateLabel(status)
                Text(lbl, fontSize = 24.sp, fontWeight = FontWeight.SemiBold, color = if (lbl == "Connected") VeilColors.Ok else MaterialTheme.colorScheme.onBackground)
                val since = status?.optLong("since", 0) ?: 0
                var now by remember { mutableLongStateOf(System.currentTimeMillis() / 1000) }
                LaunchedEffect(state) { while (true) { now = System.currentTimeMillis() / 1000; kotlinx.coroutines.delay(1000) } }
                val sub = when (state) {
                    "connected" -> if (status?.optBoolean("carrier", true) == false) "Lost the server, reconnecting automatically…" else if (since > 0) "Connected for " + fmtDur(now - since) else ""
                    "connecting" -> "Setting up the tunnel…"
                    "error" -> ""
                    else -> "Tap to connect"
                }
                Text(sub, color = MaterialTheme.colorScheme.onSurfaceVariant, fontSize = 13.sp)
                val err = status?.optString("error")
                if (state == "error" && !err.isNullOrBlank()) {
                    Spacer(Modifier.height(10.dp))
                    Surface(color = Color(0x332A1618), shape = RoundedCornerShape(10.dp), border = BorderStroke(1.dp, Color(0xFF6B2A2A)), modifier = Modifier.fillMaxWidth()) {
                        Text(err, color = VeilColors.Bad, fontSize = 13.sp, modifier = Modifier.padding(12.dp))
                    }
                }
            }

            // speeds
            Row(horizontalArrangement = Arrangement.spacedBy(10.dp)) {
                StatBox("Download", rates.down, status?.optLong("down", 0) ?: 0, Modifier.weight(1f))
                StatBox("Upload", rates.up, status?.optLong("up", 0) ?: 0, Modifier.weight(1f))
            }

            status?.optJSONObject("limits")?.let { PlanCard(it) }

            Section("Settings", Icons.Filled.Tune) { SettingsForm(active, onSave) }
            Section("Manage server (admin)", Icons.Filled.AdminPanelSettings) {
                Text(
                    "Create and edit keys, add time, data and speed limits, show QR codes, straight from your phone. You log in with the admin password.",
                    fontSize = 13.sp, color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
                Spacer(Modifier.height(10.dp))
                Button(onClick = onManage, colors = ButtonDefaults.buttonColors(containerColor = VeilColors.Accent2, contentColor = Color.White), shape = RoundedCornerShape(10.dp)) {
                    Icon(Icons.Filled.OpenInNew, null, Modifier.size(18.dp)); Spacer(Modifier.width(8.dp)); Text("Open server management")
                }
            }
            Section("This server", Icons.Filled.Dns) { ServerSection(active, onSave, onDelete) }
        }

        // footer
        Row(verticalAlignment = Alignment.CenterVertically, modifier = Modifier.fillMaxWidth().padding(top = 4.dp)) {
            TextButton(onClick = onCycleTheme) {
                Icon(
                    when (themeMode) { ThemeMode.DARK -> Icons.Filled.DarkMode; ThemeMode.LIGHT -> Icons.Filled.LightMode; ThemeMode.SYSTEM -> Icons.Filled.BrightnessAuto },
                    null, Modifier.size(16.dp),
                )
                Spacer(Modifier.width(6.dp))
                Text(when (themeMode) { ThemeMode.DARK -> "Dark"; ThemeMode.LIGHT -> "Light"; ThemeMode.SYSTEM -> "System theme" }, fontSize = 13.sp)
            }
            Spacer(Modifier.weight(1f))
            Text("v$version", fontSize = 12.sp, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
    }
}

@Composable
fun VeilLogo(modifier: Modifier) {
    Canvas(modifier) {
        val s = size.minDimension
        drawRoundRect(color = Color(0xFF7656EE), cornerRadius = androidx.compose.ui.geometry.CornerRadius(s * 0.26f))
        val p = Path().apply {
            moveTo(s * 0.29f, s * 0.31f); lineTo(s * 0.5f, s * 0.72f); lineTo(s * 0.71f, s * 0.31f)
        }
        drawPath(p, Color.White, style = Stroke(width = s * 0.125f, cap = StrokeCap.Round, join = StrokeJoin.Round))
    }
}

@Composable
fun StatusPill(label: String, color: Color) {
    Surface(shape = RoundedCornerShape(50), color = MaterialTheme.colorScheme.surface, border = BorderStroke(1.dp, color.copy(alpha = 0.35f))) {
        Row(Modifier.padding(horizontal = 11.dp, vertical = 5.dp), verticalAlignment = Alignment.CenterVertically) {
            Box(Modifier.size(8.dp).clip(CircleShape).background(color))
            Spacer(Modifier.width(7.dp))
            Text(label, color = color, fontSize = 12.sp, fontWeight = FontWeight.Medium)
        }
    }
}

@Composable
fun VeilCard(modifier: Modifier = Modifier, highlight: Boolean = false, onClick: (() -> Unit)? = null, content: @Composable ColumnScope.() -> Unit) {
    val border = if (highlight) VeilColors.Accent2 else MaterialTheme.colorScheme.outline
    val m = modifier.fillMaxWidth().clip(RoundedCornerShape(14.dp)).let { if (onClick != null) it.clickable(onClick = onClick) else it }
    Surface(m, shape = RoundedCornerShape(14.dp), color = MaterialTheme.colorScheme.surface, border = BorderStroke(1.dp, border)) {
        Column(Modifier.padding(14.dp), content = content)
    }
}

@Composable
private fun EmptyCard(onAdd: () -> Unit) {
    VeilCard {
        Text("Add a server", fontSize = 18.sp, fontWeight = FontWeight.SemiBold)
        Spacer(Modifier.height(4.dp))
        Text("Scan the QR code from the admin panel, or paste the key (client.json) your server admin gave you.", fontSize = 13.sp, color = MaterialTheme.colorScheme.onSurfaceVariant)
        Spacer(Modifier.height(14.dp))
        Button(onClick = onAdd, colors = ButtonDefaults.buttonColors(containerColor = VeilColors.Accent2, contentColor = Color.White), shape = RoundedCornerShape(10.dp)) {
            Icon(Icons.Filled.Add, null); Spacer(Modifier.width(6.dp)); Text("Add server")
        }
    }
}

@Composable
private fun ServerBadge() {
    Box(Modifier.size(36.dp).clip(CircleShape).background(VeilColors.Accent2.copy(alpha = 0.18f)), contentAlignment = Alignment.Center) {
        Icon(Icons.Filled.Public, null, tint = VeilColors.Accent, modifier = Modifier.size(20.dp))
    }
}

@Composable
private fun ServerPicker(
    profiles: List<ServerProfile>, active: ServerProfile, open: Boolean,
    onToggle: () -> Unit, onSelect: (String) -> Unit, onAdd: () -> Unit,
) {
    Column {
        VeilCard(highlight = open, onClick = onToggle) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                ServerBadge()
                Spacer(Modifier.width(12.dp))
                Column(Modifier.weight(1f)) {
                    Text(active.name, fontSize = 16.sp, fontWeight = FontWeight.SemiBold, maxLines = 1)
                    Text(active.serverHost, fontSize = 13.sp, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1)
                }
                Icon(if (open) Icons.Filled.ExpandLess else Icons.Filled.ExpandMore, null, tint = MaterialTheme.colorScheme.onSurfaceVariant)
            }
        }
        AnimatedVisibility(open) {
            VeilCard(Modifier.padding(top = 6.dp)) {
                profiles.forEach { p ->
                    val sel = p.id == active.id
                    Row(
                        Modifier.fillMaxWidth().clip(RoundedCornerShape(9.dp))
                            .background(if (sel) VeilColors.Accent2.copy(alpha = 0.16f) else Color.Transparent)
                            .clickable { onSelect(p.id) }.padding(10.dp),
                        verticalAlignment = Alignment.CenterVertically,
                    ) {
                        Column(Modifier.weight(1f)) {
                            Text(p.name, fontWeight = FontWeight.Medium)
                            Text(p.serverHost, fontSize = 12.sp, color = MaterialTheme.colorScheme.onSurfaceVariant)
                        }
                        if (sel) Icon(Icons.Filled.Check, null, tint = VeilColors.Accent, modifier = Modifier.size(18.dp))
                    }
                }
                Spacer(Modifier.height(6.dp))
                OutlinedButton(onClick = onAdd, shape = RoundedCornerShape(10.dp), modifier = Modifier.fillMaxWidth()) {
                    Icon(Icons.Filled.Add, null, Modifier.size(18.dp)); Spacer(Modifier.width(6.dp)); Text("Add server")
                }
            }
        }
    }
}

@Composable
private fun PowerButton(status: JSONObject?, onClick: () -> Unit) {
    val state = status?.optString("state") ?: "disconnected"
    val on = state == "connected" && status?.optBoolean("carrier", true) != false
    val busy = state == "connecting" || state == "disconnecting" || (state == "connected" && !on)
    val pulse by rememberInfiniteTransition(label = "p").animateFloat(0.45f, 1f, infiniteRepeatable(tween(700), RepeatMode.Reverse), label = "a")
    val outline = MaterialTheme.colorScheme.outline
    val surface = MaterialTheme.colorScheme.surface
    val mute = MaterialTheme.colorScheme.onSurfaceVariant
    val (ring, fill, icon) = when {
        on -> Triple(VeilColors.Ok, Color(0xFF163626), Color.White)
        busy -> Triple(VeilColors.Warn.copy(alpha = pulse), surface, VeilColors.Warn)
        state == "error" -> Triple(VeilColors.Bad, surface, VeilColors.Bad)
        else -> Triple(outline, surface, mute)
    }
    Box(
        Modifier.size(196.dp).drawBehind {
            if (on) drawCircle(Brush.radialGradient(listOf(VeilColors.Ok.copy(alpha = 0.35f), Color.Transparent), radius = size.minDimension / 2), radius = size.minDimension / 2)
        },
        contentAlignment = Alignment.Center,
    ) {
        Canvas(
            Modifier.size(150.dp).clip(CircleShape)
                .clickable(interactionSource = remember { MutableInteractionSource() }, indication = androidx.compose.material3.ripple(color = ring), onClick = onClick),
        ) {
            val s = size.minDimension
            drawCircle(fill)
            drawCircle(ring, style = Stroke(width = 2.dp.toPx()))
            val r = s * 0.2f
            val c = Offset(s / 2, s / 2 + s * 0.02f)
            drawArc(icon, startAngle = -90f + 43f, sweepAngle = 360f - 86f, useCenter = false,
                topLeft = Offset(c.x - r, c.y - r), size = Size(r * 2, r * 2), style = Stroke(width = s * 0.045f, cap = StrokeCap.Round))
            drawLine(icon, Offset(s / 2, s / 2 - r * 1.15f), Offset(s / 2, s / 2 - r * 0.15f), strokeWidth = s * 0.045f, cap = StrokeCap.Round)
        }
    }
}

@Composable
private fun StatBox(title: String, rate: Double, total: Long, modifier: Modifier) {
    VeilCard(modifier) {
        Text(title, fontSize = 12.sp, color = MaterialTheme.colorScheme.onSurfaceVariant)
        Text(fmtRate(rate), fontSize = 18.sp, fontWeight = FontWeight.SemiBold)
        Text(fmtBytes(total) + " total", fontSize = 12.sp, color = MaterialTheme.colorScheme.onSurfaceVariant)
    }
}

@Composable
private fun KV(k: String, v: String, color: Color = MaterialTheme.colorScheme.onSurface) {
    Row(Modifier.fillMaxWidth().padding(vertical = 3.dp)) {
        Text(k, fontSize = 13.sp, color = MaterialTheme.colorScheme.onSurfaceVariant, modifier = Modifier.weight(1f))
        Text(v, fontSize = 13.sp, fontWeight = FontWeight.Medium, color = color, textAlign = TextAlign.End)
    }
}

@Composable
private fun PlanCard(l: JSONObject) {
    val expiresAt = l.optLong("expires_at", 0)
    val cap = l.optLong("data_cap", 0)
    val used = l.optLong("used_up", 0) + l.optLong("used_down", 0)
    val rate = l.optDouble("rate_mbps", 0.0)
    var refreshing by remember { mutableStateOf(false) }
    VeilCard {
        Row(verticalAlignment = Alignment.CenterVertically) {
            Text("Your plan", fontWeight = FontWeight.SemiBold, modifier = Modifier.weight(1f))
            Text(
                if (refreshing) "Refreshing…" else "Refresh limits", color = VeilColors.Accent, fontSize = 13.sp,
                modifier = Modifier.clip(RoundedCornerShape(6.dp)).clickable(enabled = !refreshing) {
                    refreshing = true
                    Thread { runCatching { VeilVpnService.client?.refreshLimits() }; refreshing = false }.start()
                }.padding(4.dp),
            )
        }
        Spacer(Modifier.height(6.dp))
        val now = System.currentTimeMillis() / 1000
        when {
            expiresAt == 0L -> KV("Time", "No expiry")
            expiresAt <= now -> KV("Time", "Expired", VeilColors.Bad)
            else -> KV("Time", fmtTimeLeft(expiresAt - now), if (expiresAt - now < 3 * 86400) VeilColors.Warn else MaterialTheme.colorScheme.onSurface)
        }
        KV("Data", if (cap > 0) "${fmtBytes(used)} of ${fmtBytes(cap)}" else "${fmtBytes(used)} used")
        if (cap > 0) {
            val frac = (used.toDouble() / cap).coerceIn(0.0, 1.0).toFloat()
            LinearProgressIndicator(
                progress = { frac }, modifier = Modifier.fillMaxWidth().padding(vertical = 5.dp).height(6.dp).clip(RoundedCornerShape(3.dp)),
                color = when { frac > 0.9f -> VeilColors.Bad; frac > 0.75f -> VeilColors.Warn; else -> VeilColors.Accent2 },
                trackColor = MaterialTheme.colorScheme.surfaceVariant,
            )
        }
        KV("Speed", if (rate <= 0) "Unlimited" else if (rate % 1.0 == 0.0) "${rate.toInt()} Mbps" else "%.1f Mbps".format(rate))
    }
}

@Composable
private fun Section(title: String, icon: androidx.compose.ui.graphics.vector.ImageVector, body: @Composable ColumnScope.() -> Unit) {
    var open by rememberSaveable(title) { mutableStateOf(false) }
    VeilCard {
        Row(
            Modifier.fillMaxWidth().clip(RoundedCornerShape(8.dp)).clickable { open = !open }.padding(vertical = 2.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Icon(icon, null, tint = VeilColors.Accent, modifier = Modifier.size(19.dp))
            Spacer(Modifier.width(10.dp))
            Text(title, fontWeight = FontWeight.SemiBold, modifier = Modifier.weight(1f))
            Icon(if (open) Icons.Filled.ExpandLess else Icons.Filled.ExpandMore, null, tint = MaterialTheme.colorScheme.onSurfaceVariant)
        }
        AnimatedVisibility(open) { Column(Modifier.padding(top = 12.dp), content = body) }
    }
}

@Composable
private fun Label(t: String) = Text(t, fontSize = 12.5.sp, fontWeight = FontWeight.Medium, color = MaterialTheme.colorScheme.onSurfaceVariant, modifier = Modifier.padding(bottom = 2.dp))

@Composable
private fun Radios(value: String, opts: List<Pair<String, String>>, onChange: (String) -> Unit) {
    Column {
        opts.forEach { (k, l) ->
            Row(Modifier.fillMaxWidth().clip(RoundedCornerShape(8.dp)).clickable { onChange(k) }, verticalAlignment = Alignment.CenterVertically) {
                RadioButton(selected = value == k, onClick = { onChange(k) }, colors = RadioButtonDefaults.colors(selectedColor = VeilColors.Accent))
                Text(l, fontSize = 14.sp)
            }
        }
    }
}

@Composable
private fun SwitchRow(title: String, desc: String, checked: Boolean, onChange: (Boolean) -> Unit) {
    Row(Modifier.fillMaxWidth().padding(vertical = 4.dp), verticalAlignment = Alignment.CenterVertically) {
        Column(Modifier.weight(1f)) {
            Text(title, fontSize = 14.sp, fontWeight = FontWeight.Medium)
            Text(desc, fontSize = 12.sp, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
        Switch(checked = checked, onCheckedChange = onChange, colors = SwitchDefaults.colors(checkedTrackColor = VeilColors.Accent2))
    }
}

@Composable
private fun SettingsForm(p: ServerProfile, onSave: (ServerProfile) -> Unit) {
    var carrier by remember(p.id) { mutableStateOf(p.carrier) }
    var transport by remember(p.id) { mutableStateOf(p.transport) }
    var frag by remember(p.id) { mutableStateOf(p.frag) }
    var rotate by remember(p.id) { mutableStateOf(p.rotateMinutes.toString()) }
    var dns by remember(p.id) { mutableStateOf(p.dns) }
    var cover by remember(p.id) { mutableStateOf(p.cover) }
    var insecure by remember(p.id) { mutableStateOf(p.insecureTls) }
    Label("Connection method")
    Radios(carrier, listOf("tls" to "HTTPS (default)", "ws" to "Plain HTTP / WebSocket (port 80)", "poll" to "Plain HTTP, no upgrade (works through proxies)", "quic" to "QUIC (UDP 443)")) { carrier = it }
    Spacer(Modifier.height(8.dp))
    Label("Transport")
    Radios(transport, listOf("stream" to "Stream (fastest)", "split" to "Split (CDN-friendly)")) { transport = it }
    Spacer(Modifier.height(8.dp))
    Label("ClientHello fragmentation")
    Radios(frag, listOf("both" to "Both (default)", "record" to "TLS records", "tcp" to "TCP segments", "none" to "Off")) { frag = it }
    Spacer(Modifier.height(8.dp))
    Row(horizontalArrangement = Arrangement.spacedBy(10.dp)) {
        OutlinedTextField(rotate, { rotate = it.filter(Char::isDigit) }, label = { Text("Rotate (min)") }, singleLine = true,
            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Number), modifier = Modifier.weight(1f))
        OutlinedTextField(dns, { dns = it }, label = { Text("Tunnel DNS") }, singleLine = true, modifier = Modifier.weight(1.4f))
    }
    Spacer(Modifier.height(8.dp))
    SwitchRow("Cover traffic", "Looks like an update client", cover) { cover = it }
    SwitchRow("Allow replaced certificates", "For networks whose filter swaps the TLS certificate; the tunnel stays protected by its own keys", insecure) { insecure = it }
    Spacer(Modifier.height(8.dp))
    val ctx = androidx.compose.ui.platform.LocalContext.current
    Button(
        onClick = {
            onSave(p.copy(carrier = carrier, transport = transport, frag = frag, rotateMinutes = rotate.toIntOrNull() ?: 20, dns = dns, cover = cover, insecureTls = insecure))
            android.widget.Toast.makeText(ctx, "Saved - applies the next time you connect", android.widget.Toast.LENGTH_SHORT).show()
        },
        colors = ButtonDefaults.buttonColors(containerColor = VeilColors.Accent2, contentColor = Color.White), shape = RoundedCornerShape(10.dp),
    ) { Text("Save settings") }
}

@Composable
private fun ServerSection(p: ServerProfile, onSave: (ServerProfile) -> Unit, onDelete: () -> Unit) {
    var name by remember(p.id) { mutableStateOf(p.name) }
    var confirm by remember { mutableStateOf(false) }
    val cfg = remember(p.configJson) { runCatching { JSONObject(p.configJson) }.getOrNull() }
    KV("Address", p.serverHost)
    KV("Disguised as", cfg?.optString("sni")?.ifBlank { "-" } ?: "-")
    KV("Certificate", if (cfg?.optString("ca_pem").isNullOrBlank()) "Public (system trusted)" else "Pinned by your key")
    Spacer(Modifier.height(10.dp))
    Row(verticalAlignment = Alignment.CenterVertically) {
        OutlinedTextField(name, { name = it }, label = { Text("Name") }, singleLine = true, modifier = Modifier.weight(1f))
        Spacer(Modifier.width(8.dp))
        OutlinedButton(onClick = { if (name.isNotBlank()) onSave(p.copy(name = name.trim())) }, shape = RoundedCornerShape(10.dp)) { Text("Rename") }
    }
    Spacer(Modifier.height(10.dp))
    OutlinedButton(
        onClick = { confirm = true }, shape = RoundedCornerShape(10.dp), border = BorderStroke(1.dp, VeilColors.Bad.copy(alpha = 0.5f)),
        colors = ButtonDefaults.outlinedButtonColors(contentColor = VeilColors.Bad),
    ) { Icon(Icons.Filled.Delete, null, Modifier.size(18.dp)); Spacer(Modifier.width(6.dp)); Text("Remove this server") }
    if (confirm) {
        AlertDialog(
            onDismissRequest = { confirm = false },
            title = { Text("Remove ${p.name}?") },
            text = { Text("This removes the saved key from this phone. You can import it again later.") },
            confirmButton = { TextButton(onClick = { confirm = false; onDelete() }) { Text("Remove", color = VeilColors.Bad) } },
            dismissButton = { TextButton(onClick = { confirm = false }) { Text("Cancel") } },
        )
    }
}

fun fmtRate(bytesPerSec: Double): String {
    val bits = bytesPerSec * 8
    return when {
        bits >= 1e9 -> "%.2f Gbps".format(bits / 1e9)
        bits >= 1e6 -> "%.1f Mbps".format(bits / 1e6)
        bits >= 1e3 -> "%.0f kbps".format(bits / 1e3)
        else -> "0 kbps"
    }
}

fun fmtDur(sec: Long): String {
    val s = max(0L, sec)
    val d = s / 86400; val h = (s % 86400) / 3600; val m = (s % 3600) / 60
    return when {
        d > 0 -> "${d}d ${h}h"
        h > 0 -> "${h}h ${m}m"
        m > 0 -> "${m}m ${s % 60}s"
        else -> "${s}s"
    }
}
