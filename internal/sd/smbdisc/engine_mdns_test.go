package smbdisc

import (
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// The engine's mDNS source, driven at its network edge as the
// WS-Discovery tests drive the engine: a fake mDNS group on loopback
// hands each query to fake responders that replay the sanitized TrueNAS
// capture or variants of it, and the tests read Local network back from
// a real service registry.

// fakeMDNS stands in for the mDNS multicast group: the engine sends its
// queries to the group socket, and each fake responder answers from its
// own loopback address by unicast to the query's source port, as
// RFC 6762 §6.7 has a responder answer a legacy unicast query.
type fakeMDNS struct {
	t    *testing.T
	conn *net.UDPConn

	mu         sync.Mutex
	responders []*fakeResponder
	queries    []mdnsQuestion
}

// mdnsQuestion is a query the group saw.
type mdnsQuestion struct {
	from  string // source address
	id    uint16
	name  string
	qtype dnsmessage.Type
}

func startFakeMDNS(t *testing.T) *fakeMDNS {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	g := &fakeMDNS{t: t, conn: conn}
	go g.serve()
	return g
}

func (g *fakeMDNS) dest() *net.UDPAddr { return g.conn.LocalAddr().(*net.UDPAddr) }

func (g *fakeMDNS) serve() {
	buf := make([]byte, 65536)
	for {
		n, from, err := g.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		var p dnsmessage.Parser
		h, err := p.Start(buf[:n])
		if err != nil || h.Response {
			continue
		}
		qs, err := p.AllQuestions()
		if err != nil || len(qs) != 1 {
			continue
		}
		q := mdnsQuestion{from: from.IP.String(), id: h.ID, name: qs[0].Name.String(), qtype: qs[0].Type}
		g.mu.Lock()
		g.queries = append(g.queries, q)
		responders := slices.Clone(g.responders)
		g.mu.Unlock()
		for _, r := range responders {
			r.answer(q, from)
		}
	}
}

// questions returns every query the group has seen.
func (g *fakeMDNS) questions() []mdnsQuestion {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.queries)
}

// mdnsLayout is how a responder lays out its answer to a PTR query.
type mdnsLayout int

const (
	// layoutCapture is the TrueNAS capture: every record in the answer
	// section.
	layoutCapture mdnsLayout = iota
	// layoutAdditional has the PTR in the answer section and SRV, TXT
	// and A in the additional section, as RFC 6763 §12 suggests.
	layoutAdditional
	// layoutPTROnly has the PTR alone; SRV and A come only when asked.
	layoutPTROnly
)

// fakeResponder is a NAS that advertises _smb._tcp. It answers a PTR
// query for the service with its records laid out by layout, and SRV
// and A queries for its own instance and host name with that record
// alone.
type fakeResponder struct {
	ip      string
	conn    *net.UDPConn
	capture []byte

	mu      sync.Mutex
	host    string // host label; the instance has the same name
	layout  mdnsLayout
	port    uint16
	as      []string // A records of the host; nil means its own address
	noA     bool     // the host has no A record
	idDelta uint16   // added to the query ID in each reply
	silent  bool     // answers nothing
	// replies, when non-nil, gives the datagrams sent instead of the
	// answer to a PTR query with ID id.
	replies func(id uint16) [][]byte
}

// addResponder puts a NAS called host on the LAN at ip (a 127.0.0.0/8
// address). It replays the TrueNAS capture until set changes it.
func (g *fakeMDNS) addResponder(ip, host string) *fakeResponder {
	t := g.t
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(ip)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	r := &fakeResponder{ip: ip, conn: conn, capture: readBinCapture(t, "truenas-mdns-smb.bin"), host: host, port: 445}
	g.mu.Lock()
	g.responders = append(g.responders, r)
	g.mu.Unlock()
	return r
}

// set changes the responder's behaviour under its lock.
func (r *fakeResponder) set(f func(r *fakeResponder)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f(r)
}

func (r *fakeResponder) answer(q mdnsQuestion, to *net.UDPAddr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.silent {
		return
	}
	instance, target := r.host+"._smb._tcp.local.", r.host+".local."
	ptr := ptrRR(smbService, instance)
	srv := srvRR(instance, target, r.port)
	txt := txtRR(instance, "")
	var as []dnsmessage.Resource
	if !r.noA {
		addrs := r.as
		if addrs == nil {
			addrs = []string{r.ip}
		}
		for _, a := range addrs {
			as = append(as, aRR(target, a))
		}
	}

	id := q.id + r.idDelta
	if r.replies != nil {
		if q.qtype == dnsmessage.TypePTR {
			for _, d := range r.replies(id) {
				r.conn.WriteToUDP(d, to)
			}
		}
		return
	}
	var b []byte
	switch {
	case q.qtype == dnsmessage.TypePTR && strings.EqualFold(q.name, smbService):
		switch r.layout {
		case layoutCapture:
			if r.host != "storage" || r.port != 445 || r.as != nil || r.noA {
				panic("the capture holds host storage on port 445 at one address")
			}
			b = slices.Clone(r.capture)
			b[0], b[1] = byte(id>>8), byte(id)
			a := netip.MustParseAddr(r.ip).As4()
			copy(b[len(b)-4:], a[:])
		case layoutAdditional:
			b = mdnsPacket(id, []dnsmessage.Resource{ptr}, append([]dnsmessage.Resource{srv, txt}, as...))
		case layoutPTROnly:
			b = mdnsPacket(id, []dnsmessage.Resource{ptr}, nil)
		}
	case q.qtype == dnsmessage.TypeSRV && strings.EqualFold(q.name, instance):
		b = mdnsPacket(id, []dnsmessage.Resource{srv}, nil)
	case q.qtype == dnsmessage.TypeA && strings.EqualFold(q.name, target) && len(as) > 0:
		b = mdnsPacket(id, as, nil)
	default:
		return
	}
	r.conn.WriteToUDP(b, to)
}

// count returns how many queries for records of type qtype the group
// has seen.
func (g *fakeMDNS) count(qtype dnsmessage.Type) int {
	n := 0
	for _, q := range g.questions() {
		if q.qtype == qtype {
			n++
		}
	}
	return n
}

// ptrQueries returns the sources of the _smb._tcp PTR queries the group
// has seen, sorted.
func (g *fakeMDNS) ptrQueries() []string {
	var out []string
	for _, q := range g.questions() {
		if q.qtype == dnsmessage.TypePTR && q.name == smbService {
			out = append(out, q.from)
		}
	}
	slices.Sort(out)
	return out
}

func TestMDNSHostIsListedOnceWhereverItsRecordsAre(t *testing.T) {
	ptr := mdnsQuestion{from: "127.0.0.1", name: smbService, qtype: dnsmessage.TypePTR}
	for _, tc := range []struct {
		name   string
		layout mdnsLayout
		want   []mdnsQuestion // the queries the round sends, IDs aside
	}{
		{"records in the answer section", layoutCapture, []mdnsQuestion{ptr}},
		{"records in the additional section", layoutAdditional, []mdnsQuestion{ptr}},
		{"PTR alone, resolved through follow-ups", layoutPTROnly, []mdnsQuestion{
			ptr,
			{from: "127.0.0.1", name: "storage._smb._tcp.local.", qtype: dnsmessage.TypeSRV},
			{from: "127.0.0.1", name: "storage.local.", qtype: dnsmessage.TypeA},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lan := startTestLAN(t)
			nas := lan.mdns.addResponder("127.0.0.2", "storage")
			nas.set(func(r *fakeResponder) { r.layout = tc.layout })
			r := startEngine(t, lan)

			want := entry{"smb/STORAGE", "storage", "smb://127.0.0.2"}
			r.waitListing(want)
			r.expectListingStays(want)
			qs := lan.mdns.questions()
			id := qs[0].id
			if id == 0 {
				t.Error("the query has ID 0, want a random non-zero one")
			}
			for i := range qs {
				if qs[i].id != id {
					t.Errorf("query %d has ID %#x, want the round's ID %#x", i, qs[i].id, id)
				}
				qs[i].id = 0
			}
			if !slices.Equal(qs, tc.want) {
				t.Fatalf("the group saw queries %+v, want %+v", qs, tc.want)
			}
		})
	}
}

func TestMDNSReplyWithAnotherIDIsIgnored(t *testing.T) {
	lan := startTestLAN(t)
	lan.mdns.addResponder("127.0.0.2", "storage")
	stray := lan.mdns.addResponder("127.0.0.3", "stray")
	stray.set(func(r *fakeResponder) { r.layout, r.idDelta = layoutAdditional, 1 })
	r := startEngine(t, lan)

	want := entry{"smb/STORAGE", "storage", "smb://127.0.0.2"}
	r.waitListing(want)
	r.expectListingStays(want)
}

func TestMDNSAddressAndPortComeFromTheRecords(t *testing.T) {
	for _, tc := range []struct {
		name string
		nas  func(r *fakeResponder)
		want entry
	}{
		{
			"SRV port other than 445",
			func(r *fakeResponder) { r.port = 4450 },
			entry{"smb/STORAGE", "storage", "smb://127.0.0.2:4450"},
		},
		{
			"the A record that is the address the answer came from",
			func(r *fakeResponder) { r.as = []string{"192.0.2.30", "127.0.0.2"} },
			entry{"smb/STORAGE", "storage", "smb://127.0.0.2"},
		},
		{
			"else the first A record",
			func(r *fakeResponder) { r.as = []string{"192.0.2.30", "192.0.2.31"} },
			entry{"smb/STORAGE", "storage", "smb://192.0.2.30"},
		},
		{
			"else the address the answer came from",
			func(r *fakeResponder) { r.noA = true },
			entry{"smb/STORAGE", "storage", "smb://127.0.0.2"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lan := startTestLAN(t)
			nas := lan.mdns.addResponder("127.0.0.2", "storage")
			nas.set(func(r *fakeResponder) { r.layout = layoutAdditional })
			nas.set(tc.nas)
			r := startEngine(t, lan)

			// A host with no A record is listed once it has been asked for
			// one and the round's replies end.
			waitFor(func() bool { return len(r.listing()) > 0 || len(lan.mdns.questions()) > 1 })
			r.clock.Advance(collectWindow)
			r.waitListing(tc.want)
			r.expectListingStays(tc.want)
		})
	}
}

func TestMDNSQueryGoesOutOnEveryInterface(t *testing.T) {
	lan := startTestLAN(t)
	lan.mdns.addResponder("127.0.0.2", "storage")
	r := startEngineOn(t, lan, interfaces(loopback(t, "127.0.0.1", "127.0.0.9")...))

	// The NAS hears both queries and answers each.
	want := entry{"smb/STORAGE", "storage", "smb://127.0.0.2"}
	r.waitListing(want)
	r.expectListingStays(want)
	if got, want := lan.mdns.ptrQueries(), []string{"127.0.0.1", "127.0.0.9"}; !slices.Equal(got, want) {
		t.Fatalf("PTR queries came from %v, want one from each interface %v", got, want)
	}
}

func TestMDNSHostAgesOutOnTheEngineWindowNotItsTTL(t *testing.T) {
	// The capture's records carry TTL 10, as legacy unicast answers do.
	lan := startTestLAN(t)
	nas := lan.mdns.addResponder("127.0.0.2", "storage")
	r := startEngine(t, lan)
	want := entry{"smb/STORAGE", "storage", "smb://127.0.0.2"}
	r.waitListing(want)

	nas.set(func(r *fakeResponder) { r.silent = true }) // switched off
	for rounds := 2; rounds <= 3; rounds++ {
		r.clock.Advance(roundInterval)
		if !waitFor(func() bool { return len(lan.mdns.ptrQueries()) == rounds }) {
			t.Fatalf("no mDNS round %d", rounds)
		}
	}
	r.expectListingStays(want) // 40 s of silence
	r.clock.Advance(roundInterval)
	r.waitListing() // 60 s
}

func TestHostileMDNSRepliesAreIgnoredWithoutHanging(t *testing.T) {
	lan := startTestLAN(t)
	lan.mdns.addResponder("127.0.0.2", "storage")
	capture := readBinCapture(t, "truenas-mdns-smb.bin")
	withID := func(b []byte, id uint16) []byte {
		b = slices.Clone(b)
		b[0], b[1] = byte(id>>8), byte(id)
		return b
	}

	// Nothing this device sends names a usable server.
	noisy := lan.mdns.addResponder("127.0.0.4", "noisy")
	noisy.set(func(r *fakeResponder) {
		r.replies = func(id uint16) [][]byte {
			loop := withID(capture, id)
			loop[0x21], loop[0x22] = 0xc0, 0x21
			return [][]byte{
				[]byte("\x00\xff garbage"),
				withID(capture[:len(capture)/2], id),
				loop,
				// A reply that is a query.
				withID(capture, id)[:2:2],
			}
		}
	})

	// It names an instance whose host name is no usable name.
	odd := lan.mdns.addResponder("127.0.0.5", "odd")
	odd.set(func(r *fakeResponder) {
		r.replies = func(id uint16) [][]byte {
			const instance = "odd._smb._tcp.local."
			return [][]byte{mdnsPacket(id, []dnsmessage.Resource{ptrRR(smbService, instance),
				srvRR(instance, "odd host.local.", 445), aRR("odd host.local.", "127.0.0.5")}, nil)}
		}
	})

	// It lists far more instances than any LAN has, none of which
	// answers the SRV query that follows, in datagrams small enough
	// not to fragment.
	const instances = maxInstances + 40
	flood := lan.mdns.addResponder("127.0.0.6", "flood")
	flood.set(func(r *fakeResponder) {
		r.replies = func(id uint16) [][]byte {
			var out [][]byte
			for i := 0; i < instances; i += 40 {
				var ptrs []dnsmessage.Resource
				for j := i; j < i+40 && j < instances; j++ {
					ptrs = append(ptrs, ptrRR(smbService, fmt.Sprintf("i-%03d._smb._tcp.local.", j)))
				}
				out = append(out, mdnsPacket(id, ptrs, nil))
			}
			return out
		}
	})

	r := startEngine(t, lan)
	want := []entry{
		{"smb/127.0.0.5", "127.0.0.5", "smb://127.0.0.5"},
		{"smb/STORAGE", "storage", "smb://127.0.0.2"},
	}
	r.waitListing(want...)
	r.expectListingStays(want...)
	if srvs := lan.mdns.count(dnsmessage.TypeSRV); srvs > maxSourceInstances {
		t.Errorf("%d SRV queries for %d instances one device listed in one round, want no more than %d",
			srvs, instances, maxSourceInstances)
	}
}

func TestMDNSInstancesARoundHandlesAreCapped(t *testing.T) {
	lan := startTestLAN(t)
	// Each device lists its share of instances, none of which answers
	// the SRV query that follows.
	const devices = maxInstances/maxSourceInstances + 2
	for d := range devices {
		flood := lan.mdns.addResponder(fmt.Sprintf("127.0.0.%d", 11+d), "flood")
		flood.set(func(r *fakeResponder) {
			r.replies = func(id uint16) [][]byte {
				var ptrs []dnsmessage.Resource
				for i := range maxSourceInstances {
					ptrs = append(ptrs, ptrRR(smbService, fmt.Sprintf("d%d-%03d._smb._tcp.local.", d, i)))
				}
				return [][]byte{mdnsPacket(id, ptrs, nil)}
			}
		})
	}
	r := startEngine(t, lan)

	srvs := func() int { return lan.mdns.count(dnsmessage.TypeSRV) }
	if !waitFor(func() bool { return srvs() >= maxInstances }) {
		t.Fatalf("%d SRV queries, want %d", srvs(), maxInstances)
	}
	r.expectListingStays()
	if n := srvs(); n != maxInstances {
		t.Errorf("%d SRV queries for %d instances listed in one round, want %d",
			n, devices*maxSourceInstances, maxInstances)
	}
}
