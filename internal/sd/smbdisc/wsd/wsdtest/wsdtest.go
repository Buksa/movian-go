// Package wsdtest is a fake WS-Discovery LAN for tests, as httptest is
// a fake HTTP server: Windows PCs and wsdd hosts on loopback that
// answer Probes and Resolves over UDP and serve the Get over HTTP,
// replaying the captures.
package wsdtest

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// LAN stands in for the WS-Discovery multicast group: the code under
// test sends its Probe to Dest, and each host answers from its own
// loopback address, as a host on a real LAN answers a multicast Probe
// by unicast.
type LAN struct {
	t    testing.TB
	conn *net.UDPConn

	mu       sync.Mutex
	hosts    []*Host
	probes   []string // source address of every wsdp:Device Probe seen
	resolves int      // Resolves seen
}

var (
	messageID   = regexp.MustCompile(`<wsa:MessageID>([^<]+)</wsa:MessageID>`)
	resolveEPR  = regexp.MustCompile(`<wsd:Resolve><wsa:EndpointReference><wsa:Address>([^<]+)</wsa:Address>`)
	eprTally    atomic.Int32
	resolveActn = "<wsa:Action>http://schemas.xmlsoap.org/ws/2005/04/discovery/Resolve</wsa:Action>"
)

// StartLAN starts a LAN on loopback that lives until the test ends.
func StartLAN(t testing.TB) *LAN {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	l := &LAN{t: t, conn: conn}
	go l.serve()
	return l
}

// Dest is the address of the group: where the code under test sends
// its Probes and Resolves.
func (l *LAN) Dest() *net.UDPAddr { return l.conn.LocalAddr().(*net.UDPAddr) }

func (l *LAN) serve() {
	buf := make([]byte, 65536)
	for {
		n, from, err := l.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		msg := string(buf[:n])
		m := messageID.FindStringSubmatch(msg)
		if m == nil {
			continue
		}
		if r := resolveEPR.FindStringSubmatch(msg); r != nil && strings.Contains(msg, resolveActn) {
			l.mu.Lock()
			l.resolves++
			hosts := slices.Clone(l.hosts)
			l.mu.Unlock()
			for _, h := range hosts {
				h.resolve(m[1], r[1], from)
			}
			continue
		}
		if !strings.Contains(msg, "<wsd:Types>wsdp:Device</wsd:Types>") {
			continue
		}
		l.mu.Lock()
		l.probes = append(l.probes, from.IP.String())
		hosts := slices.Clone(l.hosts)
		l.mu.Unlock()
		for _, h := range hosts {
			h.probe(m[1], from)
		}
	}
}

// ProbeCount returns how many Probes the LAN has seen.
func (l *LAN) ProbeCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.probes)
}

// ResolveCount returns how many Resolves the LAN has seen.
func (l *LAN) ResolveCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.resolves
}

// ProbeSources returns the source address of each Probe seen from the
// nth on, sorted.
func (l *LAN) ProbeSources(n int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := slices.Clone(l.probes[n:])
	slices.Sort(out)
	return out
}

// WaitProbes waits until the LAN has seen n Probes in all, and fails
// the test if it does not.
func (l *LAN) WaitProbes(n int) {
	l.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for l.ProbeCount() < n && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := l.ProbeCount(); got < n {
		l.t.Fatalf("saw %d Probes, want %d", got, n)
	}
}

// Host is a Windows PC or a wsdd host on the LAN. It answers each Probe
// with the captured ProbeMatch, sent Repeats times as Windows does, and
// serves the captured GetResponse from an HTTP endpoint on its own
// address. A test changes the exported fields through Set.
type Host struct {
	// GetResponse is the captured GetResponse that the host serves.
	GetResponse string
	// ProbeMatch is the captured ProbeMatch that the host sends.
	ProbeMatch string
	// IP is the host's loopback address.
	IP string

	t    testing.TB
	conn *net.UDPConn
	srv  *httptest.Server

	mu       sync.Mutex
	held     chan heldProbe // non-nil holds each Probe until Release
	gets     int
	getsDone int

	// EPR is the endpoint reference UUID, unique per host.
	EPR string
	// Computer is the pub:Computer text; "" leaves the element out.
	Computer string
	// GetStatus, when non-zero, fails the Get with this HTTP status.
	GetStatus int
	// GetBody, when non-empty, is served instead of the GetResponse.
	GetBody string
	// Stall, when non-nil, holds each Get until it is closed.
	Stall chan struct{}
	// XAddr, when non-empty, replaces the host's own XAddr in its replies.
	XAddr string
	// Repeats is how many times the ProbeMatch is sent.
	Repeats int
	// Replies, when non-nil, sends these datagrams instead of the
	// ProbeMatch, which it is given with the host's values filled in.
	Replies func(probeMatch string) []string
	// Pace, when non-nil, runs before reply datagram i goes out.
	Pace func(i int)

	// A wsdd host answers a Resolve for its endpoint with ResolveMatch,
	// sent twice; ResolveReplies, when non-nil, sends these instead.
	ResolveMatch   string
	ResolveReplies func(resolveMatch string) []string

	// Only, when non-empty, makes the host hear only datagrams from this address.
	Only         string
	probedFrom   []string // address:port of each Probe it heard
	resolvedFrom []string // address:port of each Resolve for its endpoint it heard
}

type heldProbe struct {
	messageID string
	from      *net.UDPAddr
}

// AddHost puts a Windows PC on the LAN at ip (a 127.0.0.0/8 address)
// whose metadata names it computer.
func (l *LAN) AddHost(ip, computer string) *Host {
	t := l.t
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(ip)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	h := &Host{
		t:           t,
		IP:          ip,
		conn:        conn,
		ProbeMatch:  WindowsProbeMatch,
		GetResponse: WindowsGetResponse,
		EPR:         fmt.Sprintf("5e1f0000-0000-4000-8000-%012d", eprTally.Add(1)),
		Computer:    computer,
		Repeats:     4,
	}
	h.srv = startHTTPOn(t, ip, http.HandlerFunc(h.serveGet))

	l.mu.Lock()
	l.hosts = append(l.hosts, h)
	l.mu.Unlock()
	return h
}

// AddWSDDHost puts a Samba or NAS host running wsdd on the LAN at ip
// whose metadata names it computer. Its ProbeMatch carries no XAddrs
// and goes out twice, as wsdd sends it; a Resolve for its endpoint gets
// the XAddr of its HTTP endpoint in a ResolveMatch.
func (l *LAN) AddWSDDHost(ip, computer string) *Host {
	l.t.Helper()
	h := l.AddHost(ip, computer)
	h.Set(func(h *Host) {
		h.ProbeMatch = asWindows(WSDDProbeMatch)
		h.ResolveMatch = asWindows(WSDDResolveMatch)
		h.GetResponse = asWindows(WSDDGetResponse)
		h.Repeats = 2
	})
	return h
}

// startHTTPOn serves handler on ip at an ephemeral port.
func startHTTPOn(t testing.TB, ip string, handler http.Handler) *httptest.Server {
	t.Helper()
	ln, err := net.Listen("tcp4", ip+":0")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(handler)
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// Set changes the host's behaviour under its lock.
func (h *Host) Set(f func(h *Host)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f(h)
}

// URL is the base URL of the host's HTTP endpoint.
func (h *Host) URL() string { return h.srv.URL }

// Hold makes the host keep each Probe it hears, up to four, instead of
// answering it, until Release answers one. It lets a test order the
// host's answer against other events.
func (h *Host) Hold() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.held = make(chan heldProbe, 4)
}

// Release answers the oldest Probe that Hold kept, as a host whose
// metadata names it computer ("OFFICE-PC/Workgroup:WORKGROUP" and the
// like). It waits up to two seconds for a Probe to arrive and fails the
// test if none does.
func (h *Host) Release(computer string) {
	h.t.Helper()
	h.mu.Lock()
	held := h.held
	h.Computer = computer
	h.mu.Unlock()
	if held == nil {
		h.t.Fatal("Release without Hold")
	}
	select {
	case p := <-held:
		h.answer(p.messageID, p.from)
	case <-time.After(2 * time.Second):
		h.t.Fatal("the code under test sent no WS-Discovery Probe")
	}
}

// probe hears a Probe that came from from: it answers it, or keeps it
// when the host is held.
func (h *Host) probe(messageID string, from *net.UDPAddr) {
	h.mu.Lock()
	held := h.held
	h.mu.Unlock()
	if held == nil {
		h.answer(messageID, from)
		return
	}
	select {
	case held <- heldProbe{messageID, from}:
	default:
	}
}

func (h *Host) answer(messageID string, to *net.UDPAddr) {
	h.mu.Lock()
	if h.Only != "" && h.Only != to.IP.String() {
		h.mu.Unlock()
		return
	}
	h.probedFrom = append(h.probedFrom, to.String())
	epr, xaddr, repeats, replies, pace := h.EPR, h.XAddr, h.Repeats, h.Replies, h.Pace
	pm := h.ProbeMatch
	h.mu.Unlock()
	pm = h.rewrite(pm, messageID, epr, xaddr)
	datagrams := slices.Repeat([]string{pm}, repeats)
	if replies != nil {
		datagrams = replies(pm)
	}
	for i, d := range datagrams {
		if pace != nil {
			pace(i)
		}
		h.conn.WriteToUDP([]byte(d), to)
	}
}

// resolve answers a Resolve for endpoint epr that came from from, if it
// is this wsdd host's endpoint.
func (h *Host) resolve(messageID, epr string, from *net.UDPAddr) {
	h.mu.Lock()
	own, xaddr, rm, replies := h.EPR, h.XAddr, h.ResolveMatch, h.ResolveReplies
	if rm == "" || epr != "urn:uuid:"+own || h.Only != "" && h.Only != from.IP.String() {
		h.mu.Unlock()
		return
	}
	h.resolvedFrom = append(h.resolvedFrom, from.String())
	h.mu.Unlock()
	rm = h.rewrite(rm, messageID, own, xaddr)
	datagrams := []string{rm, rm}
	if replies != nil {
		datagrams = replies(rm)
	}
	for _, d := range datagrams {
		h.conn.WriteToUDP([]byte(d), from)
	}
}

// rewrite fills a captured reply in for this host: relatesTo, its
// endpoint epr and xaddr, or when xaddr is "" its own HTTP endpoint.
func (h *Host) rewrite(reply, relatesTo, epr, xaddr string) string {
	if xaddr == "" {
		xaddr = h.srv.URL + "/" + epr + "/"
	}
	reply = strings.Replace(reply, CaptureRelatesTo, relatesTo, 1)
	reply = strings.Replace(reply, CaptureXAddr, xaddr, 1)
	return strings.ReplaceAll(reply, CaptureEPR, "urn:uuid:"+epr)
}

func (h *Host) serveGet(w http.ResponseWriter, r *http.Request) {
	io.Copy(io.Discard, r.Body) // lets the server notice the client hanging up
	h.mu.Lock()
	h.gets++
	status, body, stall, computer, getResponse := h.GetStatus, h.GetBody, h.Stall, h.Computer, h.GetResponse
	h.mu.Unlock()
	defer h.Set(func(h *Host) { h.getsDone++ })
	if stall != nil {
		select {
		case <-stall:
		case <-r.Context().Done():
		}
	}
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	if body == "" {
		el := ""
		if computer != "" {
			el = "<pub:Computer>" + computer + "</pub:Computer>"
		}
		body = strings.Replace(getResponse, CaptureComputer, el, 1)
	}
	w.Header().Set("Content-Type", "application/soap+xml")
	io.WriteString(w, body)
}

// GetCount returns how many Gets the host has received.
func (h *Host) GetCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.gets
}

// HeardFrom returns where the Probes and the Resolves the host heard
// came from, as address:port.
func (h *Host) HeardFrom() (probes, resolves []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.probedFrom), slices.Clone(h.resolvedFrom)
}

// GetsInFlight returns how many Gets the host has received and how
// many of those are still open.
func (h *Host) GetsInFlight() (received, open int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.gets, h.gets - h.getsDone
}
