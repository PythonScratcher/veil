package mobile

// Config mirrors the relevant subset of the Windows/Linux client's client.json shape, so the same
// key file exported from the admin panel works unmodified on Android. Fields that only make sense
// for a desktop process (local SOCKS/HTTP proxy ports, system proxy, NetBounce) are left out: the
// Android client is always full-device TUN mode, with the tunnel interface set up by Android's own
// VpnService before Go ever sees a file descriptor.
type Config struct {
	Server    string `json:"server"`
	SNI       string `json:"sni"`
	Path      string `json:"path"`
	ServerPub string `json:"server_pub"`
	PSK       string `json:"psk"`
	ConnectIP string `json:"connect_ip"`
	CAPEM     string `json:"ca_pem"`
	// InsecureTLS skips verifying the OUTER TLS certificate (ignores ca_pem and system roots). Use it
	// on networks whose DPI swaps in its own certificate. Safe by design: the server is authenticated
	// by the inner handshake (server_pub + psk), so a forged outer cert only exposes opaque ciphertext.
	InsecureTLS bool `json:"insecure_tls,omitempty"`
	// WSAddr (host:port, usually :80) is the plain-HTTP address (default <server host>:80). Carrier picks the
	// connection method: tls (default, HTTPS) or ws (sealed WebSocket over plain HTTP, for networks
	// that block HTTPS by default). They never switch automatically.
	WSAddr    string `json:"ws_addr,omitempty"`
	Carrier   string `json:"carrier,omitempty"`
	Transport string `json:"transport"`
	Frag      string `json:"frag"`
	Rotate    int    `json:"rotate_minutes"`
	Cover     *bool  `json:"cover"`
	DNS       string `json:"tun_dns"`
}
