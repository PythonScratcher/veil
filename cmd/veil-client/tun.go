package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/tun"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"

	"veil/proto"
)

const (
	tunMTU    = 1400
	tunOffset = 16
	nicID     = 1
)

// runTUN turns the whole machine into a Veil client: every IP packet the OS
// routes into the virtual adapter is terminated in a userspace TCP/IP stack,
// and each TCP connection / UDP flow becomes one Veil stream.
func runTUN(ctx context.Context, t *Transport, cfg *Config) error {
	dev, name, err := createTUN(tunMTU)
	if err != nil {
		return fmt.Errorf("create adapter (Administrator? wintun.dll next to the exe?): %w", err)
	}
	defer dev.Close()
	cleanup, err := configureRoutes(name, cfg)
	if err != nil {
		return fmt.Errorf("routes: %w", err)
	}
	defer cleanup()

	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4, icmp.NewProtocol6},
	})
	ep := channel.New(1024, tunMTU, "")
	if e := s.CreateNIC(nicID, ep); e != nil {
		return fmt.Errorf("nic: %v", e)
	}
	s.SetPromiscuousMode(nicID, true)
	s.SetSpoofing(nicID, true)
	s.SetRouteTable([]tcpip.Route{
		{Destination: header.IPv4EmptySubnet, NIC: nicID},
		{Destination: header.IPv6EmptySubnet, NIC: nicID},
	})
	// big buffers for throughput
	sb := tcpip.TCPReceiveBufferSizeRangeOption{Min: 4096, Default: 1 << 20, Max: 4 << 20}
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &sb)
	ssb := tcpip.TCPSendBufferSizeRangeOption{Min: 4096, Default: 1 << 20, Max: 4 << 20}
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &ssb)

	tcpF := tcp.NewForwarder(s, 0, 4096, func(r *tcp.ForwarderRequest) {
		id := r.ID()
		// Dial the far side BEFORE answering the SYN. If the destination is unreachable from the
		// server (e.g. an IPv6 site while the server has no IPv6), we answer with a RST so the app
		// fails over to IPv4 at once, instead of seeing an "open" connection that then just closes.
		go func() {
			host := ipString(id.LocalAddress)
			rc, err := dialTCP(context.Background(), t, host, id.LocalPort)
			if err != nil {
				log.Printf("tcp %s:%d: %v", host, id.LocalPort, err)
				r.Complete(true)
				return
			}
			var wq waiter.Queue
			e, terr := r.CreateEndpoint(&wq)
			if terr != nil {
				rc.Close()
				r.Complete(true)
				return
			}
			r.Complete(false)
			proto.Pipe(gonet.NewTCPConn(&wq, e), rc)
		}()
	})
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpF.HandlePacket)

	udpF := udp.NewForwarder(s, func(r *udp.ForwarderRequest) bool {
		id := r.ID()
		var wq waiter.Queue
		e, err := r.CreateEndpoint(&wq)
		if err != nil {
			return false
		}
		local := gonet.NewUDPConn(&wq, e)
		go handleUDPFlow(ctx, t, local, ipString(id.LocalAddress), id.LocalPort)
		return true
	})
	s.SetTransportProtocolHandler(udp.ProtocolNumber, udpF.HandlePacket)

	var wg sync.WaitGroup
	wg.Add(2)
	// TUN -> stack
	go func() {
		defer wg.Done()
		bufs := make([][]byte, dev.BatchSize())
		sizes := make([]int, len(bufs))
		for i := range bufs {
			bufs[i] = make([]byte, tunOffset+tunMTU+64)
		}
		for ctx.Err() == nil {
			n, err := dev.Read(bufs, sizes, tunOffset)
			if err != nil {
				return
			}
			for i := 0; i < n; i++ {
				pkt := bufs[i][tunOffset : tunOffset+sizes[i]]
				if len(pkt) == 0 {
					continue
				}
				var proto tcpip.NetworkProtocolNumber
				switch pkt[0] >> 4 {
				case 4:
					proto = header.IPv4ProtocolNumber
				case 6:
					proto = header.IPv6ProtocolNumber
				default:
					continue
				}
				pb := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(append([]byte(nil), pkt...))})
				ep.InjectInbound(proto, pb)
				pb.DecRef()
			}
		}
	}()
	// stack -> TUN
	go func() {
		defer wg.Done()
		for {
			pb := ep.ReadContext(ctx)
			if pb == nil {
				return
			}
			view := pb.ToView()
			data := view.AsSlice()
			buf := make([]byte, tunOffset+len(data))
			copy(buf[tunOffset:], data)
			view.Release()
			pb.DecRef()
			if _, err := dev.Write([][]byte{buf}, tunOffset); err != nil {
				log.Printf("tun write: %v", err)
			}
		}
	}()
	log.Printf("VPN up: all traffic now flows through the Veil tunnel (Ctrl+C to stop)")
	go func() { // apps keep pre-existing connections outside the VPN: reset them so they reconnect through it
		time.Sleep(600 * time.Millisecond)
		if n := resetExistingConnections(cfg); n > 0 {
			log.Printf("reset %d existing connection(s) so apps reconnect through the VPN", n)
		}
	}()
	<-ctx.Done()
	dev.Close()
	s.Close()
	wg.Wait()
	return nil
}

func ipString(a tcpip.Address) string { return net.IP(a.AsSlice()).String() }

func handleUDPFlow(ctx context.Context, t *Transport, local *gonet.UDPConn, host string, port uint16) {
	defer local.Close()
	rc, err := t.Dial(ctx, proto.Target{Net: proto.NetUDP, Host: host, Port: port})
	if err != nil {
		log.Printf("udp %s:%d: %v", host, port, err)
		return
	}
	defer rc.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, proto.MaxData)
		for {
			n, err := rc.Read(buf)
			if err != nil {
				return
			}
			local.Write(buf[:n])
		}
	}()
	buf := make([]byte, 65535)
	for {
		local.SetReadDeadline(time.Now().Add(60 * time.Second))
		n, err := local.Read(buf)
		if err != nil {
			break
		}
		if n > proto.MaxData {
			continue
		}
		if _, err := rc.Write(buf[:n]); err != nil {
			break
		}
	}
	rc.Close()
	<-done
}

func createTUN(mtu int) (tun.Device, string, error) {
	dev, err := tun.CreateTUN(tunName, mtu)
	if err != nil {
		return nil, "", err
	}
	name, err := dev.Name()
	if err != nil {
		name = tunName
	}
	return dev, name, nil
}
