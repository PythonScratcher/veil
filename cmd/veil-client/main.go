// veil-client: Windows (and Linux) client for the Veil tunnel.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

type Config struct {
	Server    string `json:"server"`
	SNI       string `json:"sni"`
	Path      string `json:"path"`
	ServerPub string `json:"server_pub"`
	PSK       string `json:"psk"`
	ConnectIP string `json:"connect_ip"`
	CAFile    string `json:"ca_file"`
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
	Mode      string `json:"mode,omitempty"`      // GUI: "proxy" or "tun"
	SysProxy  bool   `json:"sys_proxy,omitempty"` // GUI: also set the Windows system proxy
	Socks     string `json:"listen_socks"`
	HTTP      string `json:"listen_http"`
	TUN       bool   `json:"tun"`
	DNS       string `json:"tun_dns"`
	// NetBounce controls the brief adapter binding flap that makes Chrome (and anything else
	// using long-lived QUIC/UDP connections) pick up the tunnel immediately instead of needing a
	// restart. Defaults to false - opt-in, not opt-on: on at least one real machine this left the
	// IPv4 stack broken badly enough that even a full adapter disable/enable cycle didn't recover
	// it and a netsh winsock/ip stack reset plus reboot was needed. The tunnel itself works fine
	// with this off; apps with existing long-lived connections (Chrome included) just need to be
	// closed and reopened once after connecting, same as before this feature existed.
	NetBounce *bool `json:"net_bounce,omitempty"`
}

func main() {
	cfgPath := flag.String("c", "client.json", "config file")
	tunFlag := flag.Bool("tun", false, "full-device VPN mode (needs Administrator + wintun.dll)")
	cleanFlag := flag.Bool("cleanup", false, "remove leftover Veil firewall rules/routes after a crash (Administrator)")
	guiFlag := flag.Bool("gui", false, "open the graphical app")
	noOpen := flag.Bool("no-open", false, "with -gui: do not open a window, just print the address")
	guardianPID := flag.Int("guardian", 0, "internal: watch this Veil process and restore the network if it dies")
	elevated := flag.Bool("elevated", false, "internal: restarted with Administrator rights")
	flag.Parse()
	if *guardianPID != 0 {
		runGuardian(*guardianPID)
		return
	}
	if *cleanFlag {
		repairNetwork()
		return
	}
	if *guiFlag || *elevated || (guiByDefault && flag.NFlag() == 0) { // double-click on Windows = GUI
		runGUI(*noOpen, *elevated)
		return
	}
	raw, err := os.ReadFile(*cfgPath)
	if err != nil {
		log.Fatal(err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		log.Fatal(err)
	}
	if *tunFlag {
		cfg.TUN = true
	}
	pub, err := base64.StdEncoding.DecodeString(cfg.ServerPub)
	if err != nil || len(pub) != 32 {
		log.Fatal("bad server_pub")
	}
	psk, err := base64.StdEncoding.DecodeString(cfg.PSK)
	if err != nil {
		log.Fatal("bad psk")
	}
	t, err := NewTransport(&cfg, pub, psk)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	wctx, wc := context.WithTimeout(ctx, 20*time.Second)
	if err := t.Start(wctx); err != nil {
		log.Fatalf("cannot reach server: %v", err)
	}
	wc()
	log.Printf("connected to %s (Chrome TLS/h2, %s transport, ClientHello frag=%s)", cfg.Server, t.mode, t.frag)

	go t.WatchLimits(ctx)

	if cfg.Socks != "" {
		if err := startSOCKS(ctx, t, cfg.Socks); err != nil {
			log.Fatal(err)
		}
	}
	if cfg.HTTP != "" {
		if err := startHTTPProxy(ctx, t, cfg.HTTP); err != nil {
			log.Fatal(err)
		}
	}
	if cfg.TUN {
		if err := runTUN(ctx, t, &cfg); err != nil {
			log.Fatalf("tun: %v", err)
		}
		return
	}
	<-ctx.Done()
	fmt.Println("bye")
}
