// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package resolver

import (
	"bytes"
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dns "golang.org/x/net/dns/dnsmessage"
	"tailscale.com/envknob"
	"tailscale.com/net/netmon"
	"tailscale.com/net/tsdial"
	"tailscale.com/types/dnstype"
)

func (rr resolverAndDelay) String() string {
	return fmt.Sprintf("%v+%v", rr.name, rr.startDelay)
}

func TestResolversWithDelays(t *testing.T) {
	// query
	q := func(ss ...string) (ipps []*dnstype.Resolver) {
		for _, host := range ss {
			ipps = append(ipps, &dnstype.Resolver{Addr: host})
		}
		return
	}
	// output
	o := func(ss ...string) (rr []resolverAndDelay) {
		for _, s := range ss {
			var d time.Duration
			s, durStr, hasPlus := strings.Cut(s, "+")
			if hasPlus {
				var err error
				d, err = time.ParseDuration(durStr)
				if err != nil {
					panic(fmt.Sprintf("parsing duration in %q: %v", s, err))
				}
			}
			rr = append(rr, resolverAndDelay{
				name:       &dnstype.Resolver{Addr: s},
				startDelay: d,
			})
		}
		return
	}

	tests := []struct {
		name string
		in   []*dnstype.Resolver
		want []resolverAndDelay
	}{
		{
			name: "unknown-no-delays",
			in:   q("1.2.3.4", "2.3.4.5"),
			want: o("1.2.3.4", "2.3.4.5"),
		},
		{
			name: "google-all-ipv4",
			in:   q("8.8.8.8", "8.8.4.4"),
			want: o("https://dns.google/dns-query", "8.8.8.8+0.5s", "8.8.4.4+0.7s"),
		},
		{
			name: "google-only-ipv6",
			in:   q("2001:4860:4860::8888", "2001:4860:4860::8844"),
			want: o("https://dns.google/dns-query", "2001:4860:4860::8888+0.5s", "2001:4860:4860::8844+0.7s"),
		},
		{
			name: "google-all-four",
			in:   q("8.8.8.8", "8.8.4.4", "2001:4860:4860::8888", "2001:4860:4860::8844"),
			want: o("https://dns.google/dns-query", "8.8.8.8+0.5s", "8.8.4.4+0.7s", "2001:4860:4860::8888+0.5s", "2001:4860:4860::8844+0.7s"),
		},
		{
			name: "quad9-one-v4-one-v6",
			in:   q("9.9.9.9", "2620:fe::fe"),
			want: o("https://dns.quad9.net/dns-query", "9.9.9.9+0.5s", "2620:fe::fe+0.5s"),
		},
		{
			name: "nextdns-ipv6-expand",
			in:   q("2a07:a8c0::c3:a884"),
			want: o("https://dns.nextdns.io/c3a884"),
		},
		{
			name: "nextdns-doh-input",
			in:   q("https://dns.nextdns.io/c3a884"),
			want: o("https://dns.nextdns.io/c3a884"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolversWithDelays(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v; want %v", got, tt.want)
			}
		})
	}

}

func TestGetRCode(t *testing.T) {
	tests := []struct {
		name   string
		packet []byte
		want   dns.RCode
	}{
		{
			name:   "empty",
			packet: []byte{},
			want:   dns.RCode(5),
		},
		{
			name:   "too-short",
			packet: []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
			want:   dns.RCode(5),
		},
		{
			name:   "noerror",
			packet: []byte{0xC4, 0xFE, 0x81, 0xA0, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01},
			want:   dns.RCode(0),
		},
		{
			name:   "refused",
			packet: []byte{0xee, 0xa1, 0x81, 0x05, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01},
			want:   dns.RCode(5),
		},
		{
			name:   "nxdomain",
			packet: []byte{0x34, 0xf4, 0x81, 0x83, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01},
			want:   dns.RCode(3),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getRCode(tt.packet)
			if got != tt.want {
				t.Errorf("got %d; want %d", got, tt.want)
			}
		})
	}
}

var testDNS = flag.Bool("test-dns", false, "run tests that require a working DNS server")

func TestGetKnownDoHClientForProvider(t *testing.T) {
	var fwd forwarder
	c, ok := fwd.getKnownDoHClientForProvider("https://dns.google/dns-query")
	if !ok {
		t.Fatal("not found")
	}
	if !*testDNS {
		t.Skip("skipping without --test-dns")
	}
	res, err := c.Head("https://dns.google/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	t.Logf("Got: %+v", res)
}

func BenchmarkNameFromQuery(b *testing.B) {
	builder := dns.NewBuilder(nil, dns.Header{})
	builder.StartQuestions()
	builder.Question(dns.Question{
		Name:  dns.MustNewName("foo.example."),
		Type:  dns.TypeA,
		Class: dns.ClassINET,
	})
	msg, err := builder.Finish()
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := nameFromQuery(msg)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// Reproduces https://github.com/tailscale/tailscale/issues/2533
// Fixed by https://github.com/tailscale/tailscale/commit/f414a9cc01f3264912513d07c0244ff4f3e4ba54
//
// NOTE: fuzz tests act like unit tests when run without `-fuzz`
func FuzzClampEDNSSize(f *testing.F) {
	// Empty DNS packet
	f.Add([]byte{
		// query id
		0x12, 0x34,
		// flags: standard query, recurse
		0x01, 0x20,
		// num questions
		0x00, 0x00,
		// num answers
		0x00, 0x00,
		// num authority RRs
		0x00, 0x00,
		// num additional RRs
		0x00, 0x00,
	})

	// Empty OPT
	f.Add([]byte{
		// header
		0xaf, 0x66, 0x01, 0x20, 0x00, 0x01, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x01,
		// query
		0x06, 0x67, 0x6f, 0x6f, 0x67, 0x6c, 0x65, 0x03, 0x63, 0x6f,
		0x6d, 0x00, 0x00, 0x01, 0x00, 0x01,
		// OPT
		0x00,       // name: <root>
		0x00, 0x29, // type: OPT
		0x10, 0x00, // UDP payload size
		0x00,       // higher bits in extended RCODE
		0x00,       // EDNS0 version
		0x80, 0x00, // "Z" field
		0x00, 0x00, // data length
	})

	// Query for "google.com"
	f.Add([]byte{
		// header
		0xaf, 0x66, 0x01, 0x20, 0x00, 0x01, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x01,
		// query
		0x06, 0x67, 0x6f, 0x6f, 0x67, 0x6c, 0x65, 0x03, 0x63, 0x6f,
		0x6d, 0x00, 0x00, 0x01, 0x00, 0x01,
		// OPT
		0x00, 0x00, 0x29, 0x10, 0x00, 0x00, 0x00, 0x80, 0x00, 0x00,
		0x0c, 0x00, 0x0a, 0x00, 0x08, 0x62, 0x18, 0x1a, 0xcb, 0x19,
		0xd7, 0xee, 0x23,
	})

	f.Fuzz(func(t *testing.T, data []byte) {
		clampEDNSSize(data, maxResponseBytes)
	})
}

func runDNSServer(tb testing.TB, response []byte, onRequest func(bool, []byte)) (port uint16) {
	tcpResponse := make([]byte, len(response)+2)
	binary.BigEndian.PutUint16(tcpResponse, uint16(len(response)))
	copy(tcpResponse[2:], response)

	// Repeatedly listen until we can get the same port.
	const tries = 25
	var (
		tcpLn *net.TCPListener
		udpLn *net.UDPConn
		err   error
	)
	for try := 0; try < tries; try++ {
		if tcpLn != nil {
			tcpLn.Close()
			tcpLn = nil
		}

		tcpLn, err = net.ListenTCP("tcp4", &net.TCPAddr{
			IP:   net.IPv4(127, 0, 0, 1),
			Port: 0, // Choose one
		})
		if err != nil {
			tb.Fatal(err)
		}
		udpLn, err = net.ListenUDP("udp4", &net.UDPAddr{
			IP:   net.IPv4(127, 0, 0, 1),
			Port: tcpLn.Addr().(*net.TCPAddr).Port,
		})
		if err == nil {
			break
		}
	}
	if tcpLn == nil || udpLn == nil {
		if tcpLn != nil {
			tcpLn.Close()
		}
		if udpLn != nil {
			udpLn.Close()
		}

		// Skip instead of being fatal to avoid flaking on extremely
		// heavily-loaded CI systems.
		tb.Skipf("failed to listen on same port for TCP/UDP after %d tries", tries)
	}

	port = uint16(tcpLn.Addr().(*net.TCPAddr).Port)

	handleConn := func(conn net.Conn) {
		defer conn.Close()

		// Read the length header, then the buffer
		var length uint16
		if err := binary.Read(conn, binary.BigEndian, &length); err != nil {
			tb.Logf("error reading length header: %v", err)
			return
		}
		req := make([]byte, length)
		n, err := io.ReadFull(conn, req)
		if err != nil {
			tb.Logf("error reading query: %v", err)
			return
		}
		req = req[:n]
		onRequest(true, req)

		// Write response
		if _, err := conn.Write(tcpResponse); err != nil {
			tb.Logf("error writing response: %v", err)
			return
		}
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := tcpLn.Accept()
			if err != nil {
				return
			}
			go handleConn(conn)
		}
	}()

	handleUDP := func(addr netip.AddrPort, req []byte) {
		onRequest(false, req)
		if _, err := udpLn.WriteToUDPAddrPort(response, addr); err != nil {
			tb.Logf("error writing response: %v", err)
		}
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			buf := make([]byte, 65535)
			n, addr, err := udpLn.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			buf = buf[:n]
			go handleUDP(addr, buf)
		}
	}()

	tb.Cleanup(func() {
		tcpLn.Close()
		udpLn.Close()
		tb.Logf("waiting for listeners to finish...")
		wg.Wait()
	})
	return
}

func TestForwarderTCPFallback(t *testing.T) {
	const debugKnob = "TS_DEBUG_DNS_FORWARD_SEND"
	oldVal := os.Getenv(debugKnob)
	envknob.Setenv(debugKnob, "true")
	t.Cleanup(func() { envknob.Setenv(debugKnob, oldVal) })

	const domain = "large-dns-response.tailscale.com."

	// Make a response that's very large, containing a bunch of localhost addresses.
	largeResponse := func() []byte {
		name := dns.MustNewName(domain)

		builder := dns.NewBuilder(nil, dns.Header{})
		builder.StartQuestions()
		builder.Question(dns.Question{
			Name:  name,
			Type:  dns.TypeA,
			Class: dns.ClassINET,
		})
		builder.StartAnswers()
		for i := 0; i < 120; i++ {
			builder.AResource(dns.ResourceHeader{
				Name:  name,
				Class: dns.ClassINET,
				TTL:   300,
			}, dns.AResource{
				A: [4]byte{127, 0, 0, byte(i)},
			})
		}

		msg, err := builder.Finish()
		if err != nil {
			t.Fatal(err)
		}
		return msg
	}()
	if len(largeResponse) <= maxResponseBytes {
		t.Fatalf("got len(largeResponse)=%d, want > %d", len(largeResponse), maxResponseBytes)
	}

	// Our request is a single A query for the domain in the answer, above.
	request := func() []byte {
		builder := dns.NewBuilder(nil, dns.Header{})
		builder.StartQuestions()
		builder.Question(dns.Question{
			Name:  dns.MustNewName(domain),
			Type:  dns.TypeA,
			Class: dns.ClassINET,
		})
		msg, err := builder.Finish()
		if err != nil {
			t.Fatal(err)
		}
		return msg
	}()

	var sawUDPRequest, sawTCPRequest atomic.Bool
	port := runDNSServer(t, largeResponse, func(isTCP bool, gotRequest []byte) {
		if isTCP {
			sawTCPRequest.Store(true)
		} else {
			sawUDPRequest.Store(true)
		}

		if !bytes.Equal(request, gotRequest) {
			t.Errorf("invalid request\ngot: %+v\nwant: %+v", gotRequest, request)
		}
	})

	netMon, err := netmon.New(t.Logf)
	if err != nil {
		t.Fatal(err)
	}

	var dialer tsdial.Dialer
	dialer.SetNetMon(netMon)

	fwd := newForwarder(t.Logf, netMon, nil, &dialer, nil)

	fq := &forwardQuery{
		txid:           getTxID(request),
		packet:         request,
		closeOnCtxDone: new(closePool),
	}
	defer fq.closeOnCtxDone.Close()

	rr := resolverAndDelay{
		name: &dnstype.Resolver{Addr: fmt.Sprintf("127.0.0.1:%d", port)},
	}

	resp, err := fwd.send(context.Background(), fq, rr)
	if err != nil {
		t.Fatalf("error making request: %v", err)
	}
	if !bytes.Equal(resp, largeResponse) {
		t.Errorf("invalid response\ngot: %+v\nwant: %+v", resp, largeResponse)
	}
	if !sawTCPRequest.Load() {
		t.Errorf("DNS server never saw TCP request")
	}
	if !sawUDPRequest.Load() {
		t.Errorf("DNS server never saw UDP request")
	}
}
