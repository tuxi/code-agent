package server

import (
	"encoding/binary"
	"net"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func mdnsTestAdvertiser() *mdnsAdvertiser {
	return &mdnsAdvertiser{service: "Test Mac abc123." + mdnsServiceType, target: "test-mac.local.", port: 49152, serverID: "srv_test", displayName: "Test Mac"}
}

func mdnsTestQuery(t *testing.T, name string, typ dnsmessage.Type, unicast bool) []byte {
	t.Helper()
	class := uint16(dnsmessage.ClassINET)
	if unicast {
		class |= 0x8000
	}
	msg := dnsmessage.Message{Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName(name), Type: typ, Class: dnsmessage.Class(class)}}}
	packet, err := msg.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return packet
}

func TestMDNSAnswersOrdinaryZeroFlagQueriesWithLiveRecords(t *testing.T) {
	m := mdnsTestAdvertiser()
	query := mdnsTestQuery(t, mdnsServiceType, dnsmessage.TypePTR, false)
	if binary.BigEndian.Uint16(query[2:4]) != 0 {
		t.Fatal("test requires an ordinary zero-flag query")
	}
	for _, name := range []string{mdnsServiceType, m.service, m.target} {
		for _, unicast := range []bool{false, true} {
			response, gotUnicast := m.responseToQuery(mdnsTestQuery(t, name, dnsmessage.TypeALL, unicast))
			if gotUnicast != unicast {
				t.Fatalf("QU routing = %v, want %v", gotUnicast, unicast)
			}
			assertLiveMDNSResponse(t, response, uint16(m.port))
		}
	}
}

func assertLiveMDNSResponse(t *testing.T, packet []byte, port uint16) {
	t.Helper()
	var response dnsmessage.Message
	if err := response.Unpack(packet); err != nil {
		t.Fatal(err)
	}
	if !response.Response || !response.Authoritative {
		t.Fatal("expected authoritative DNS response")
	}
	foundPort, foundIdentity := false, false
	for _, answer := range response.Answers {
		if answer.Header.TTL != mdnsRecordTTL {
			t.Fatalf("live response TTL = %d; TTL zero withdraws the service", answer.Header.TTL)
		}
		if answer.Header.Type != dnsmessage.TypePTR && uint16(answer.Header.Class)&0x8000 == 0 {
			t.Fatal("unique record lacks cache flush bit")
		}
		switch body := answer.Body.(type) {
		case *dnsmessage.SRVResource:
			foundPort = body.Port == port
		case *dnsmessage.TXTResource:
			for _, txt := range body.TXT {
				if txt == "server_id=srv_test" {
					foundIdentity = true
				}
			}
		}
	}
	if !foundPort || !foundIdentity {
		t.Fatalf("missing current endpoint or stable identity: %+v", response.Answers)
	}
}

func TestMDNSIgnoresResponsesUnrelatedAndMalformedQueries(t *testing.T) {
	m := mdnsTestAdvertiser()
	query := mdnsTestQuery(t, mdnsServiceType, dnsmessage.TypePTR, false)
	response := append([]byte(nil), query...)
	binary.BigEndian.PutUint16(response[2:4], 0x8400)
	for _, packet := range [][]byte{nil, query[:11], query[:len(query)-1], response, mdnsTestQuery(t, "_other._tcp.local.", dnsmessage.TypePTR, false)} {
		if answer, _ := m.responseToQuery(packet); answer != nil {
			t.Fatalf("answered non-query or unrelated query: %x", packet)
		}
	}
}

func TestMDNSGoodbyeUsesZeroTTLOnlyWhenRequested(t *testing.T) {
	m := mdnsTestAdvertiser()
	var message dnsmessage.Message
	if err := message.Unpack(m.records(0)); err != nil {
		t.Fatal(err)
	}
	for _, answer := range message.Answers {
		if answer.Header.TTL != 0 {
			t.Fatal("goodbye TTL must be zero")
		}
	}
}

func TestMDNSTargetHostAvoidsDuplicateLocalSuffix(t *testing.T) {
	for input, want := range map[string]string{"Mac": "Mac.local.", "Mac.local": "Mac.local.", "Mac.local.": "Mac.local.", "Mac.LOCAL.": "Mac.LOCAL."} {
		if got := mdnsTargetHost(input); got != want {
			t.Fatalf("host(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestMDNSKeepsAnsweringAfterFirstDiscovery(t *testing.T) {
	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	client, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	defer client.Close()
	m := mdnsTestAdvertiser()
	m.conn, m.addr, m.stop = server, client.LocalAddr().(*net.UDPAddr), make(chan struct{})
	go m.serve()
	defer m.Close()
	query := mdnsTestQuery(t, mdnsServiceType, dnsmessage.TypePTR, false)
	for i := 0; i < 2; i++ {
		if _, err := client.WriteToUDP(query, server.LocalAddr().(*net.UDPAddr)); err != nil {
			t.Fatal(err)
		}
		_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
		packet := make([]byte, 9000)
		n, _, err := client.ReadFromUDP(packet)
		if err != nil {
			t.Fatalf("discovery attempt %d: %v", i+1, err)
		}
		assertLiveMDNSResponse(t, packet[:n], uint16(m.port))
	}
}
