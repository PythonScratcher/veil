package net.pythonscratcher.veil

import android.content.Context
import org.json.JSONArray
import org.json.JSONObject
import java.util.UUID

/**
 * One imported server key plus the in-app settings for it (transport/frag/cover/rotate/DNS - the
 * same knobs the Windows GUI's Settings panel exposes, but kept per-profile here since different
 * servers may want different transports or may be more/less aggressively DPI-filtered).
 */
data class ServerProfile(
    val id: String,
    val name: String,
    val configJson: String, // raw imported key JSON: server/sni/path/server_pub/psk/connect_ip/ca_pem
    val transport: String = "stream",
    val frag: String = "both",
    val cover: Boolean = true,
    val rotateMinutes: Int = 20,
    val dns: String = "1.1.1.1,1.0.0.1",
    val carrier: String = "tls", // "tls" = HTTPS (default), "ws" = plain HTTP WebSocket on port 80
    val adminUrl: String = "", // admin panel address incl. secret path, for "Manage server"
    val adminTrustAny: Boolean = false,
    val insecureTls: Boolean = false, // skip outer-cert check (for networks whose DPI swaps the certificate)
) {
    val serverHost: String
        get() = runCatching { JSONObject(configJson).optString("server", "") }.getOrDefault("")

    /** The JSON actually handed to the tunnel: the imported key with this profile's settings merged in. */
    fun effectiveConfigJson(): String {
        val o = JSONObject(configJson)
        o.put("transport", transport)
        o.put("carrier", carrier)
        o.put("frag", frag)
        o.put("cover", cover)
        o.put("rotate_minutes", rotateMinutes)
        if (insecureTls) o.put("insecure_tls", true) else o.remove("insecure_tls")
        if (dns.isNotBlank()) o.put("tun_dns", dns) else o.remove("tun_dns")
        return o.toString()
    }

    fun toJson(): JSONObject = JSONObject().apply {
        put("id", id)
        put("name", name)
        put("configJson", configJson)
        put("transport", transport)
        put("carrier", carrier)
        put("frag", frag)
        put("cover", cover)
        put("rotateMinutes", rotateMinutes)
        put("dns", dns)
        put("insecureTls", insecureTls)
        put("adminUrl", adminUrl)
        put("adminTrustAny", adminTrustAny)
    }

    companion object {
        fun fromJson(o: JSONObject) = ServerProfile(
            id = o.getString("id"),
            name = o.getString("name"),
            configJson = o.getString("configJson"),
            transport = o.optString("transport", "stream"),
            carrier = o.optString("carrier", "tls"),
            frag = o.optString("frag", "both"),
            cover = o.optBoolean("cover", true),
            rotateMinutes = o.optInt("rotateMinutes", 20),
            dns = o.optString("dns", "1.1.1.1,1.0.0.1"),
            insecureTls = o.optBoolean("insecureTls", false),
            adminUrl = o.optString("adminUrl", ""),
            adminTrustAny = o.optBoolean("adminTrustAny", false),
        )

        /** Validates a pasted/scanned key and builds a fresh profile for it, named after its host. */
        fun fromImport(rawJson: String): ServerProfile? {
            val o = runCatching { JSONObject(rawJson) }.getOrNull() ?: return null
            if (!o.has("server") || !o.has("server_pub") || !o.has("psk")) return null
            val host = o.optString("server", "").substringBefore(":")
            return ServerProfile(
                id = UUID.randomUUID().toString(),
                name = if (host.isNotBlank()) host else "Server",
                configJson = o.toString(),
            )
        }
    }
}

/** Persists the list of server profiles and which one is currently selected. */
object ProfileStore {
    private const val PREFS = "veil_profiles"
    private const val KEY_LIST = "list"
    private const val KEY_ACTIVE = "active_id"

    fun loadAll(ctx: Context): List<ServerProfile> {
        val raw = ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE).getString(KEY_LIST, null) ?: return emptyList()
        val arr = runCatching { JSONArray(raw) }.getOrNull() ?: return emptyList()
        return (0 until arr.length()).mapNotNull { i -> runCatching { ServerProfile.fromJson(arr.getJSONObject(i)) }.getOrNull() }
    }

    private fun saveAll(ctx: Context, profiles: List<ServerProfile>) {
        val arr = JSONArray()
        profiles.forEach { arr.put(it.toJson()) }
        ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE).edit().putString(KEY_LIST, arr.toString()).apply()
    }

    fun add(ctx: Context, profile: ServerProfile) {
        val list = loadAll(ctx) + profile
        saveAll(ctx, list)
        if (getActiveId(ctx) == null) setActiveId(ctx, profile.id)
    }

    fun update(ctx: Context, profile: ServerProfile) {
        saveAll(ctx, loadAll(ctx).map { if (it.id == profile.id) profile else it })
    }

    fun delete(ctx: Context, id: String) {
        val remaining = loadAll(ctx).filterNot { it.id == id }
        saveAll(ctx, remaining)
        if (getActiveId(ctx) == id) {
            setActiveId(ctx, remaining.firstOrNull()?.id)
        }
    }

    fun getActiveId(ctx: Context): String? =
        ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE).getString(KEY_ACTIVE, null)

    fun setActiveId(ctx: Context, id: String?) {
        ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE).edit().putString(KEY_ACTIVE, id).apply()
    }
}
