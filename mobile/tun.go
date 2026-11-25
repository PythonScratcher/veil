package mobile

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
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

const nicID = 1

// androidTUN wraps the raw file descriptor Android's VpnService.Builder.establish() hands back.
// Unlike the desktop wireguard-go NativeTun, there is no interface to open by name and no ioctls
// to run - Android has already created and configured the virtual interface (addresses, routes,
// MTU, DNS) by the time this fd exists, so this type only ever reads and writes whole IP packets
// on it. Android's TUN fd carries raw IP packets with no additional framing (no 4-byte PI header
// the way a desktop Linux /dev/net/tun can), so Read/Write are a plain, single-packet pass-through.
type androidTUN struct {
	f         *os.File
	mtu       int
	events    chan tun.Event
	closeOnce sync.Once
}

func newAndroidTUN(fd int, mtu int) *androidTUN {
	return &androidTUN{f: os.NewFile(uintptr(fd), "veil-tun"), mtu: mtu, events: make(chan tun.Event, 1)}
}

func (t *androidTUN) File() *os.File { return t.f }

func (t *androidTUN) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	n, err := t.f.Read(bufs[0][offset:])
	if err != nil {
		return 0, err
	}
	sizes[0] = n
	return 1, nil
}

func (t *androidTUN) Write(bufs [][]byte, offset int) (int, error) {
	for i, b := range bufs {
		if _, err := t.f.Write(b[offset:]); err != nil {
			return i, err
		}
	}
	return len(bufs), nil
}

func (t *androidTUN) MTU() (int, error)        { return t.mtu, nil }
func (t *androidTUN) Name() (string, error)    { return "veiltun", nil }
func (t *androidTUN) Events() <-chan tun.Event { return t.events }
func (t *androidTUN) BatchSize() int           { return 1 }

func (t *androidTUN) Close() (err error) {
	t.closeOnce.Do(func() {
		close(t.events)
		err = t.f.Close()
	})
	return err
}

// runTUN terminates every IP packet Android routes into the tunnel interface in a userspace
// TCP/IP stack, turning each TCP connection / UDP flow into one Veil stream. This is the same
// gVisor netstack plumbing the desktop client uses (cmd/veil-client/tun.go) - the only real
// difference on Android is that the device (dev) already exists, handed to us by the OS, instead
// of being created here by opening a system TUN adapter by name.
func runTUN(ctx context.Context, t *Transport, dev tun.Device, mtu int) error {
	defer dev.Close()

	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4, icmp.NewProtocol6},
	})
	ep := channel.New(1024, uint32(mtu), "")
	if e := s.CreateNIC(nicID, ep); e != nil {
		return fmt.Errorf("nic: %v", e)
	}
	s.SetPromiscuousMode(nicID, true)
	s.SetSpoofing(nicID, true)
	s.SetRouteTable([]tcpip.Route{
		{Destination: header.IPv4EmptySubnet, NIC: nicID},
		{Destination: header.IPv6EmptySubnet, NIC: nicID},
	})
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
			bufs[i] = make([]byte, mtu+64)
		}
		for ctx.Err() == nil {
			n, err := dev.Read(bufs, sizes, 0)
			if err != nil {
				return
			}
			for i := 0; i < n; i++ {
				pkt := bufs[i][:sizes[i]]
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
			buf := append([]byte(nil), data...)
			view.Release()
			pb.DecRef()
			if _, err := dev.Write([][]byte{buf}, 0); err != nil {
				log.Printf("tun write: %v", err)
			}
		}
	}()
	log.Printf("VPN up: all traffic now flows through the Veil tunnel")
	<-ctx.Done()
	dev.Close()
	s.Close()
	wg.Wait()
	return nil
}

func ipString(a tcpip.Address) string { return net.IP(a.AsSlice()).String() }

func dialTCP(ctx context.Context, t *Transport, host string, port uint16) (proto.Stream, error) {
	return t.Dial(ctx, proto.Target{Net: proto.NetTCP, Host: host, Port: port})
}

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
