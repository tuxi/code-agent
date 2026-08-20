package server

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	mdnsServiceType = "_talkify-agent._tcp.local."
	mdnsGroup       = "224.0.0.251"
	mdnsPort        = 5353
)

// mdnsAdvertiser is intentionally small and dependency-free. It answers the
// service/type queries used by Bonjour discovery and sends RFC 6762
// announcements and goodbyes for the daemon-owned listener.
type mdnsAdvertiser struct {
	conn                  *net.UDPConn
	addr                  *net.UDPAddr
	service               string
	target                string
	port                  int
	serverID, displayName string
	stop                  chan struct{}
	once                  sync.Once
}

func newMDNSAdvertiser(serverID, displayName string, port int) (*mdnsAdvertiser, error) {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		host = "codeagent"
	}
	host = strings.TrimSuffix(host, ".")
	service := sanitizeDNSLabel(displayName) + " " + shortServerID(serverID) + "." + mdnsServiceType
	conn, err := net.ListenMulticastUDP("udp4", nil, &net.UDPAddr{IP: net.ParseIP(mdnsGroup), Port: mdnsPort})
	if err != nil {
		return nil, fmt.Errorf("bind Bonjour mDNS: %w", err)
	}
	m := &mdnsAdvertiser{conn: conn, addr: &net.UDPAddr{IP: net.ParseIP(mdnsGroup), Port: mdnsPort}, service: service, target: host + ".local.", port: port, serverID: serverID, displayName: displayName, stop: make(chan struct{})}
	go m.serve()
	if err := m.announce(120); err != nil {
		m.Close()
		return nil, err
	}
	return m, nil
}

func (m *mdnsAdvertiser) serve() {
	buf := make([]byte, 1500)
	for {
		_ = m.conn.SetReadDeadline(time.Now().Add(time.Second))
		n, from, err := m.conn.ReadFromUDP(buf)
		select {
		case <-m.stop:
			return
		default:
		}
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
		if n < 12 || binary.BigEndian.Uint16(buf[2:4]) == 0 {
			continue
		}
		if !hasMDNSQuestion(buf[:n]) {
			continue
		}
		packet := m.records(0)
		_, _ = m.conn.WriteToUDP(packet, from)
	}
}

func (m *mdnsAdvertiser) announce(ttl uint32) error {
	_, err := m.conn.WriteToUDP(m.records(ttl), m.addr)
	return err
}

func (m *mdnsAdvertiser) Close() {
	m.once.Do(func() { _ = m.announce(0); close(m.stop); _ = m.conn.Close() })
}

func (m *mdnsAdvertiser) records(ttl uint32) []byte {
	var b dnsPacket
	b.header(0, 0x8400, 0, 3)
	b.ptr(mdnsServiceType, m.service, ttl)
	b.srv(m.service, m.target, uint16(m.port), ttl)
	b.txt(m.service, []string{"schema=talkify-runtime-share/v1", "server_id=" + m.serverID, "display_name=" + m.displayName, "wire_major=1"}, ttl)
	if ip := firstPrivateIPv4(); ip != nil {
		binary.BigEndian.PutUint16(b.data[6:8], 4)
		b.a(m.target, ip, ttl)
	}
	return b.bytes()
}

type dnsPacket struct{ data []byte }

func (p *dnsPacket) header(id, flags, qd, an uint16) {
	p.data = make([]byte, 12)
	binary.BigEndian.PutUint16(p.data[0:2], id)
	binary.BigEndian.PutUint16(p.data[2:4], flags)
	binary.BigEndian.PutUint16(p.data[4:6], qd)
	binary.BigEndian.PutUint16(p.data[6:8], an)
}
func (p *dnsPacket) name(name string) {
	for _, label := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		if len(label) > 63 {
			label = label[:63]
		}
		p.data = append(p.data, byte(len(label)))
		p.data = append(p.data, []byte(label)...)
	}
	p.data = append(p.data, 0)
}
func (p *dnsPacket) rr(name string, typ, class uint16, ttl uint32, rdata []byte) {
	p.name(name)
	var h [10]byte
	binary.BigEndian.PutUint16(h[0:2], typ)
	binary.BigEndian.PutUint16(h[2:4], class)
	binary.BigEndian.PutUint32(h[4:8], ttl)
	binary.BigEndian.PutUint16(h[8:10], uint16(len(rdata)))
	p.data = append(p.data, h[:]...)
	p.data = append(p.data, rdata...)
}
func (p *dnsPacket) ptr(name, target string, ttl uint32) {
	var r dnsPacket
	r.name(target)
	p.rr(name, 12, 1, ttl, r.data)
}
func (p *dnsPacket) srv(name, target string, port uint16, ttl uint32) {
	var r dnsPacket
	r.data = make([]byte, 6)
	binary.BigEndian.PutUint16(r.data[4:6], port)
	r.name(target)
	p.rr(name, 33, 1, ttl, r.data)
}
func (p *dnsPacket) txt(name string, values []string, ttl uint32) {
	var r []byte
	for _, v := range values {
		if len(v) > 255 {
			v = v[:255]
		}
		r = append(r, byte(len(v)))
		r = append(r, []byte(v)...)
	}
	p.rr(name, 16, 1, ttl, r)
}
func (p *dnsPacket) a(name string, ip net.IP, ttl uint32) { p.rr(name, 1, 1, ttl, ip.To4()) }
func (p *dnsPacket) bytes() []byte                        { return p.data }

func hasMDNSQuestion(packet []byte) bool {
	return len(packet) > 12 && binary.BigEndian.Uint16(packet[4:6]) > 0
}
func firstPrivateIPv4() net.IP {
	ifaces, _ := net.Interfaces()
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip4 := ip.To4(); ip4 != nil {
				return ip4
			}
		}
	}
	return nil
}
func sanitizeDNSLabel(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "CodeAgent"
	}
	v = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '.' {
			return '-'
		}
		return r
	}, v)
	return v
}
func shortServerID(v string) string {
	if len(v) > 6 {
		return v[len(v)-6:]
	}
	return v
}
