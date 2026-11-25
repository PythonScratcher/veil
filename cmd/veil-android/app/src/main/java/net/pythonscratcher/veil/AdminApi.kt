package net.pythonscratcher.veil

import android.graphics.Bitmap
import android.graphics.Color
import com.google.zxing.BarcodeFormat
import com.google.zxing.EncodeHintType
import com.google.zxing.qrcode.QRCodeWriter
import com.google.zxing.qrcode.decoder.ErrorCorrectionLevel
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import org.json.JSONArray
import org.json.JSONObject
import java.io.ByteArrayInputStream
import java.net.HttpURLConnection
import java.net.URL
import java.security.KeyStore
import java.security.SecureRandom
import java.security.cert.CertificateFactory
import java.security.cert.X509Certificate
import javax.net.ssl.HostnameVerifier
import javax.net.ssl.HttpsURLConnection
import javax.net.ssl.SSLContext
import javax.net.ssl.TrustManager
import javax.net.ssl.TrustManagerFactory
import javax.net.ssl.X509TrustManager

/** One key as the server's admin API reports it. */
data class AdminKey(
    val id: String,
    val name: String,
    val note: String,
    val enabled: Boolean,
    val status: String,
    val expiresAt: Long,
    val dataCap: Long,
    val usedUp: Long,
    val usedDown: Long,
    val rateMbps: Double,
    val maxDevices: Int,
    val resetDays: Int,
    val conns: Int,
    val lastSeen: Long,
) {
    val used: Long get() = usedUp + usedDown

    companion object {
        fun from(o: JSONObject) = AdminKey(
            id = o.getString("id"),
            name = o.optString("name"),
            note = o.optString("note"),
            enabled = o.optBoolean("enabled", true),
            status = o.optString("status", "active"),
            expiresAt = o.optLong("expires_at"),
            dataCap = o.optLong("data_cap_bytes"),
            usedUp = o.optLong("used_up"),
            usedDown = o.optLong("used_down"),
            rateMbps = o.optDouble("rate_mbps", 0.0),
            maxDevices = o.optInt("max_devices"),
            resetDays = o.optInt("reset_days"),
            conns = o.optInt("conns"),
            lastSeen = o.optLong("last_seen"),
        )
    }
}

class AdminException(message: String, val code: Int = 0) : Exception(message)

/**
 * Client for the Veil server's admin web API (the same one the browser panel uses): log in with the
 * admin password, then list and edit keys. [adminUrl] is the full panel address including its secret
 * path, e.g. https://example.com:8088/console-abc123/ .
 *
 * Certificates: if the key carries its own CA ([caPem], from a self-signed server) that CA is
 * trusted; otherwise normal system roots apply. [trustAny] turns checking off (not recommended).
 */
class AdminApi(adminUrl: String, private val caPem: String?, private val trustAny: Boolean) {
    private val base = if (adminUrl.endsWith("/")) adminUrl else "$adminUrl/"
    private var cookie: String? = null
    private var csrf: String = ""

    private val sslContext: SSLContext? by lazy {
        when {
            trustAny -> SSLContext.getInstance("TLS").apply {
                val tm = object : X509TrustManager {
                    override fun checkClientTrusted(chain: Array<X509Certificate>, authType: String) {}
                    override fun checkServerTrusted(chain: Array<X509Certificate>, authType: String) {}
                    override fun getAcceptedIssuers(): Array<X509Certificate> = arrayOf()
                }
                init(null, arrayOf<TrustManager>(tm), SecureRandom())
            }
            !caPem.isNullOrBlank() -> runCatching {
                val cert = CertificateFactory.getInstance("X.509").generateCertificate(ByteArrayInputStream(caPem!!.toByteArray()))
                val ks = KeyStore.getInstance(KeyStore.getDefaultType()).apply { load(null); setCertificateEntry("veil", cert) }
                val tmf = TrustManagerFactory.getInstance(TrustManagerFactory.getDefaultAlgorithm()).apply { init(ks) }
                SSLContext.getInstance("TLS").apply { init(null, tmf.trustManagers, SecureRandom()) }
            }.getOrNull()
            else -> null
        }
    }

    private fun call(method: String, path: String, body: JSONObject? = null): String {
        val c = URL(base + path).openConnection() as HttpURLConnection
        if (c is HttpsURLConnection) {
            sslContext?.let { c.sslSocketFactory = it.socketFactory }
            if (trustAny) c.hostnameVerifier = HostnameVerifier { _, _ -> true }
        }
        c.requestMethod = method
        c.connectTimeout = 10_000
        c.readTimeout = 20_000
        c.setRequestProperty("Accept", "application/json")
        cookie?.let { c.setRequestProperty("Cookie", it) }
        if (csrf.isNotEmpty()) c.setRequestProperty("X-CSRF-Token", csrf)
        if (body != null) {
            c.doOutput = true
            c.setRequestProperty("Content-Type", "application/json")
            c.outputStream.use { it.write(body.toString().toByteArray()) }
        }
        try {
            val code = c.responseCode
            val text = (if (code in 200..299) c.inputStream else c.errorStream)?.bufferedReader()?.use { it.readText() } ?: ""
            if (code !in 200..299) {
                val msg = runCatching { JSONObject(text).optString("error") }.getOrNull().orEmpty()
                throw AdminException(msg.ifBlank { "HTTP $code" }, code)
            }
            c.headerFields["Set-Cookie"]?.firstOrNull { it.startsWith("veil_admin=") }?.let { cookie = it.substringBefore(';') }
            return text
        } finally {
            c.disconnect()
        }
    }

    private suspend fun <T> io(f: () -> T): T = withContext(Dispatchers.IO) {
        try {
            f()
        } catch (e: AdminException) {
            throw e
        } catch (e: javax.net.ssl.SSLException) {
            throw AdminException("Certificate problem: ${e.message}. If this server uses a self-signed or replaced certificate, enable \"Trust any certificate\".")
        } catch (e: java.io.IOException) {
            throw AdminException("Cannot reach the admin panel: ${e.message}")
        }
    }

    suspend fun login(password: String) = io {
        val r = JSONObject(call("POST", "api/login", JSONObject().put("password", password)))
        csrf = r.getString("csrf")
    }

    suspend fun logout() = io { runCatching { call("POST", "api/logout") }; Unit }
    suspend fun serverInfo(): JSONObject = io { JSONObject(call("GET", "api/server")) }

    suspend fun listKeys(): List<AdminKey> = io {
        val a = JSONArray(call("GET", "api/users"))
        (0 until a.length()).map { AdminKey.from(a.getJSONObject(it)) }
    }

    suspend fun createKey(fields: JSONObject): AdminKey = io { AdminKey.from(JSONObject(call("POST", "api/users", fields))) }
    suspend fun updateKey(id: String, fields: JSONObject): AdminKey = io { AdminKey.from(JSONObject(call("POST", "api/users/$id/update", fields))) }
    suspend fun deleteKey(id: String) = io { call("DELETE", "api/users/$id"); Unit }
    suspend fun resetUsage(id: String) = io { call("POST", "api/users/$id/reset-usage"); Unit }
    suspend fun regenKey(id: String) = io { call("POST", "api/users/$id/regen-key"); Unit }
    suspend fun clientConfig(id: String): String = io { call("GET", "api/users/$id/config") }

    suspend fun changePassword(current: String, new: String) = io {
        call("POST", "api/password", JSONObject().put("current", current).put("new", new)); Unit
    }

    companion object {
        /** Keeps only the fields a client needs, so the QR code stays small. */
        fun compactKey(configJson: String): String {
            val o = JSONObject(configJson)
            val keep = listOf("server", "sni", "path", "server_pub", "psk", "ca_pem", "ws_addr", "connect_ip")
            val out = JSONObject()
            for (k in keep) if (o.has(k) && o.optString(k).isNotEmpty()) out.put(k, o.get(k))
            return out.toString()
        }

        fun qrBitmap(text: String, size: Int = 720): Bitmap? = runCatching {
            val hints = mapOf(EncodeHintType.ERROR_CORRECTION to ErrorCorrectionLevel.L, EncodeHintType.MARGIN to 2)
            val m = QRCodeWriter().encode(text, BarcodeFormat.QR_CODE, size, size, hints)
            val px = IntArray(size * size) { i -> if (m[i % size, i / size]) Color.BLACK else Color.WHITE }
            Bitmap.createBitmap(px, size, size, Bitmap.Config.ARGB_8888)
        }.getOrNull()
    }
}
