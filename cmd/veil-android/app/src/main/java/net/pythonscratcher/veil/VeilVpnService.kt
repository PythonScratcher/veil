package net.pythonscratcher.veil

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Intent
import android.net.VpnService
import android.os.Build
import android.os.ParcelFileDescriptor
import androidx.core.app.NotificationCompat
import mobile.Client
import org.json.JSONObject
import java.util.concurrent.atomic.AtomicBoolean

/**
 * The actual tunnel. Android's VpnService is what is allowed to create the virtual network
 * interface and install it as the device's default route; once that's set up (establish()
 * below), the raw file descriptor is handed to the same Go engine the Windows/Linux client uses
 * (mobile.Client, bound from cmd/veil-client's carrier/transport/TUN logic via gomobile) which
 * does the actual handshake and packet forwarding.
 */
class VeilVpnService : VpnService() {

    companion object {
        const val ACTION_CONNECT = "net.pythonscratcher.veil.CONNECT"
        const val ACTION_DISCONNECT = "net.pythonscratcher.veil.DISCONNECT"
        const val EXTRA_CONFIG_JSON = "config_json"
        const val CHANNEL_ID = "veil_vpn"
        const val NOTIF_ID = 1

        @Volatile
        var client: Client? = null
            private set
    }

    private var tunFd: ParcelFileDescriptor? = null
    private val running = AtomicBoolean(false)

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        when (intent?.action) {
            ACTION_DISCONNECT -> {
                stopTunnel()
                return START_NOT_STICKY
            }
            ACTION_CONNECT -> {
                val cfgJson = intent.getStringExtra(EXTRA_CONFIG_JSON) ?: return START_NOT_STICKY
                startTunnel(cfgJson)
                return START_STICKY
            }
        }
        return START_NOT_STICKY
    }

    private fun startTunnel(cfgJson: String) {
        if (running.getAndSet(true)) return
        startForeground(NOTIF_ID, buildNotification("Connecting…"))

        val mtu = 1400
        val dns = runCatching { JSONObject(cfgJson).optString("tun_dns", "") }.getOrDefault("")

        val builder = Builder()
            .setSession("Veil")
            .setMtu(mtu)
            .addAddress("10.77.0.2", 24)
            .addAddress("fd77::2", 64)
            .addRoute("0.0.0.0", 0)
            .addRoute("::", 0)
            .addDisallowedApplication(packageName)

        val dnsServers = dns.split(",").map { it.trim() }.filter { it.isNotEmpty() }
        if (dnsServers.isNotEmpty()) {
            dnsServers.forEach { runCatching { builder.addDnsServer(it) } }
        } else {
            builder.addDnsServer("1.1.1.1")
            builder.addDnsServer("1.0.0.1")
        }

        val fd = builder.establish()
        if (fd == null) {
            running.set(false)
            stopSelf()
            return
        }
        tunFd = fd

        val c = Client()
        client = c
        Thread {
            try {
                c.start(cfgJson, fd.fd.toLong(), mtu.toLong())
                updateNotification("Connected")
            } catch (e: Exception) {
                updateNotification("Error: ${e.message}")
                stopTunnel()
            }
        }.start()
    }

    private fun stopTunnel() {
        if (!running.getAndSet(false)) {
            stopSelf()
            return
        }
        Thread {
            client?.stop()
            client = null
            tunFd?.close()
            tunFd = null
            stopForeground(STOP_FOREGROUND_REMOVE)
            stopSelf()
        }.start()
    }

    override fun onDestroy() {
        stopTunnel()
        super.onDestroy()
    }

    override fun onRevoke() {
        stopTunnel()
        super.onRevoke()
    }

    private fun buildNotification(text: String): Notification {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            val mgr = getSystemService(NotificationManager::class.java)
            if (mgr.getNotificationChannel(CHANNEL_ID) == null) {
                mgr.createNotificationChannel(
                    NotificationChannel(CHANNEL_ID, "Veil VPN", NotificationManager.IMPORTANCE_LOW)
                )
            }
        }
        val openIntent = Intent(this, MainActivity::class.java)
        val pi = PendingIntent.getActivity(
            this, 0, openIntent,
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT
        )
        return NotificationCompat.Builder(this, CHANNEL_ID)
            .setContentTitle("Veil")
            .setContentText(text)
            .setSmallIcon(android.R.drawable.ic_lock_lock)
            .setContentIntent(pi)
            .setOngoing(true)
            .build()
    }

    private fun updateNotification(text: String) {
        val mgr = getSystemService(NotificationManager::class.java)
        mgr.notify(NOTIF_ID, buildNotification(text))
    }
}
