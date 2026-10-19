package server

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

const (
	mdnsServiceType = "_talkify-agent._tcp.local."
	mdnsGroup       = "224.0.0.251"
	mdnsPort        = 5353
	mdnsRecordTTL   = 120
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
	host = mdnsTargetHost(host)
	service := sanitizeDNSLabel(displayName) + " " + shortServerID(serverID) + "." + mdnsServiceType
	conn, err := net.ListenMulticastUDP("udp4", nil, &net.UDPAddr{IP: net.ParseIP(mdnsGroup), Port: mdnsPort})
	if err != nil {
		return nil, fmt.Errorf("bind Bonjour mDNS: %w", err)
	}
	m := &mdnsAdvertiser{conn: conn, addr: &net.UDPAddr{IP: net.ParseIP(mdnsGroup), Port: mdnsPort}, service: service, target: host, port: port, serverID: serverID, displayName: displayName, stop: make(chan struct{})}
	go m.serve()
	if err := m.announce(mdnsRecordTTL); err != nil {
		m.Close()
		return nil, err
	}
	return m, nil
}

func (m *mdnsAdvertiser) serve() {
	buf := make([]byte, 9000)
	nextAnnouncement := time.Now().Add(mdnsRecordTTL * time.Second / 2)
	for {
		if time.Now().After(nextAnnouncement) {
			_ = m.announce(mdnsRecordTTL)
			nextAnnouncement = time.Now().Add(mdnsRecordTTL * time.Second / 2)
		}
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
		packet, unicast := m.responseToQuery(buf[:n])
		if packet == nil {
			continue
		}
		destination := m.addr
		if unicast || from.Port != mdnsPort {
			destination = from
			// Legacy unicast DNS queries use the original transaction ID.
			copy(packet[:2], buf[:2])
		}
		_, _ = m.conn.WriteToUDP(packet, destination)
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
	p.rr(name, 33, 0x8001, ttl, r.data)
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
	p.rr(name, 16, 0x8001, ttl, r)
}
func (p *dnsPacket) a(name string, ip net.IP, ttl uint32) { p.rr(name, 1, 0x8001, ttl, ip.To4()) }
func (p *dnsPacket) bytes() []byte                        { return p.data }

// A normal mDNS query has flags == 0. Only QR-marked responses are
// excluded. TTL zero is reserved for Close's goodbye, never a live answer.
func (m *mdnsAdvertiser) responseToQuery(packet []byte) ([]byte, bool) {
	var parser dnsmessage.Parser
	header, err := parser.Start(packet)
	if err != nil || header.Response || header.OpCode != 0 {
		return nil, false
	}
	questions, err := parser.AllQuestions()
	if err != nil {
		return nil, false
	}
	matched, unicast := false, false
	for _, question := range questions {
		if uint16(question.Class)&0x7fff != uint16(dnsmessage.ClassINET) {
			continue
		}
		name := question.Name.String()
		relevant := strings.EqualFold(name, mdnsServiceType) && (question.Type == dnsmessage.TypePTR || question.Type == dnsmessage.TypeALL) ||
			strings.EqualFold(name, m.service) && (question.Type == dnsmessage.TypeSRV || question.Type == dnsmessage.TypeTXT || question.Type == dnsmessage.TypeALL) ||
			strings.EqualFold(name, m.target) && (question.Type == dnsmessage.TypeA || question.Type == dnsmessage.TypeALL)
		if relevant {
			matched = true
			unicast = unicast || uint16(question.Class)&0x8000 != 0
		}
	}
	if !matched {
		return nil, false
	}
	return m.records(mdnsRecordTTL), unicast
}

func mdnsTargetHost(host string) string {
	host = strings.TrimSuffix(strings.TrimSpace(host), ".")
	if !strings.HasSuffix(strings.ToLower(host), ".local") {
		host += ".local"
	}
	return host + "."
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
