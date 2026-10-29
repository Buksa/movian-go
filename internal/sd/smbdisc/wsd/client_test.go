package wsd

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/czz/movian-go/internal/sd/smbdisc/lan"
	"github.com/czz/movian-go/internal/sd/smbdisc/wsd/wsdtest"
)

// The client is driven at its network edge: a fake WS-Discovery group on
// loopback hands each Probe to fake hosts that replay the sanitized
// captures, a test closes a round's window itself, and the tests read
// the sightings the round reports.

// fixture is a client on a fake LAN, with the sightings it reported.
type fixture struct {
	t      *testing.T
	lan    *wsdtest.LAN
	client *Client
	ifs    []lan.Interface
	ctx    context.Context
	cancel context.CancelFunc

	mu  sync.Mutex
	now time.Time
	got []Sighting
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, lan: wsdtest.StartLAN(t), now: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	f.ifs = loopback(t, "127.0.0.1")
	f.client = &Client{Dest: f.lan.Dest(), Now: f.clock}
	f.ctx, f.cancel = context.WithCancel(context.Background())
	// Registered after the hosts the test adds, so it runs before they
	// close: the Gets in flight end with the context.
	t.Cleanup(func() {
		f.cancel()
		f.client.Wait()
	})
	return f
}

func (f *fixture) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fixture) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// report is the client's report function.
func (f *fixture) report(s Sighting) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, s)
}

// sightings returns what the rounds have reported, sorted.
func (f *fixture) sightings() []Sighting {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := slices.Clone(f.got)
	slices.SortFunc(out, func(a, b Sighting) int {
		if c := a.Addr.Compare(b.Addr); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

// forget drops the sightings reported so far.
func (f *fixture) forget() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = nil
}

// seen is the sighting of the host at ip that name names; "" is a host
// known by address only.
func seen(name, ip string) Sighting {
	return Sighting{Name: name, Addr: netip.MustParseAddr(ip), Port: smbPort}
}

// waitSightings waits until the rounds have reported exactly want.
func (f *fixture) waitSightings(want ...Sighting) {
	f.t.Helper()
	if !waitFor(func() bool { return slices.Equal(f.sightings(), want) }) {
		f.t.Fatalf("sightings %v, want %v", f.sightings(), want)
	}
}

// expectSightingsStay checks that the rounds have reported exactly want
// after the client has had time to act on any late replies.
func (f *fixture) expectSightingsStay(want ...Sighting) {
	f.t.Helper()
	time.Sleep(200 * time.Millisecond)
	if got := f.sightings(); !slices.Equal(got, want) {
		f.t.Fatalf("sightings %v, want %v", got, want)
	}
}

// run is a round in progress.
type run struct {
	window chan time.Time
	done   chan struct{}
	sent   int
}

// round starts a round through ifs, which is the fixture's interfaces
// when none are given, with its window open.
func (f *fixture) round(ifs ...lan.Interface) *run {
	if ifs == nil {
		ifs = f.ifs
	}
	r := &run{window: make(chan time.Time), done: make(chan struct{})}
	go func() {
		r.sent = f.client.Round(f.ctx, r.window, ifs, f.report)
		close(r.done)
	}()
	return r
}

// closeWindow ends the round's collection window and waits for the
// round to return, then returns how many Probes it sent.
func (r *run) closeWindow() int {
	close(r.window)
	<-r.done
	return r.sent
}

// loopback returns the loopback interface once under each of addrs
// (127.0.0.0/8 addresses), as a host with that many LAN interfaces.
func loopback(t *testing.T, addrs ...string) []lan.Interface {
	t.Helper()
	ifis, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(ifis, func(ifi net.Interface) bool { return ifi.Flags&net.FlagLoopback != 0 })
	if i < 0 {
		t.Fatal("no loopback interface")
	}
	var out []lan.Interface
	for _, a := range addrs {
		out = append(out, lan.Interface{Name: ifis[i].Name, Index: ifis[i].Index, Addr: netip.MustParseAddr(a)})
	}
	return out
}

func waitFor(cond func() bool) bool {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

// keptEndpoints returns how many WS-Discovery endpoints the client
// keeps metadata for. Nothing outside the client shows it, short of
// the memory it takes.
func (f *fixture) keptEndpoints() int {
	f.client.mu.Lock()
	defer f.client.mu.Unlock()
	return len(f.client.meta)
}

// endpointReplies rewrites reply pm, the host's ProbeMatch for its
// endpoint epr, into one reply for each of n other endpoints of the
// same host, each with its own XAddr on the host. Each is a datagram of
// its own; see TestHostileRepliesAreIgnoredWithoutHanging.
func endpointReplies(pm, epr string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = strings.ReplaceAll(pm, epr, fmt.Sprintf("f100d000-0000-4000-8000-%012d", i))
	}
	return out
}

func TestWindowsHostRepeatingItsProbeMatchIsListedOnce(t *testing.T) {
	f := newFixture(t)
	pc := f.lan.AddHost("127.0.0.2", "OFFICE-PC/Workgroup:WORKGROUP")
	f.round()

	want := seen("OFFICE-PC", "127.0.0.2")
	f.waitSightings(want)
	f.expectSightingsStay(want)
	if n := pc.GetCount(); n != 1 {
		t.Fatalf("the host got %d metadata Gets for one ProbeMatch sent 4 times, want 1", n)
	}
}

func TestWSDDHostIsResolvedAndReportedByName(t *testing.T) {
	f := newFixture(t)
	nas := f.lan.AddWSDDHost("127.0.0.2", "FILESERVER/Workgroup:WORKGROUP")
	r := f.round()

	want := seen("FILESERVER", "127.0.0.2")
	f.waitSightings(want)
	f.expectSightingsStay(want)
	// wsdd sends its ProbeMatch and its ResolveMatch twice each.
	if n := f.lan.ResolveCount(); n != 1 {
		t.Fatalf("%d Resolves for a ProbeMatch sent twice, want 1", n)
	}
	if n := nas.GetCount(); n != 1 {
		t.Fatalf("the host got %d metadata Gets for a ResolveMatch sent twice, want 1", n)
	}

	// The name is known, so the next round needs neither.
	r.closeWindow()
	f.forget()
	f.round()
	f.lan.WaitProbes(2)
	f.waitSightings(want)
	f.expectSightingsStay(want)
	if n, gets := f.lan.ResolveCount(), nas.GetCount(); n != 1 || gets != 1 {
		t.Fatalf("%d Resolves and %d Gets after a second round, want 1 of each", n, gets)
	}
}

func TestUnansweredResolveReportsTheHostByAddressWithoutHoldingUpTheRound(t *testing.T) {
	f := newFixture(t)
	nas := f.lan.AddWSDDHost("127.0.0.2", "FILESERVER/Workgroup:WORKGROUP")
	nas.Set(func(h *wsdtest.Host) { h.ResolveReplies = func(string) []string { return nil } })
	f.lan.AddHost("127.0.0.3", "OFFICE-PC/Workgroup:WORKGROUP")
	r := f.round()

	// The PC is reported while the NAS's Resolve waits, and the NAS by
	// address once the round's replies are in.
	pc := seen("OFFICE-PC", "127.0.0.3")
	f.waitSightings(pc)
	if !waitFor(func() bool { return f.lan.ResolveCount() == 1 }) {
		t.Fatal("no Resolve for the NAS")
	}
	f.expectSightingsStay(pc)
	r.closeWindow()
	f.waitSightings(seen("", "127.0.0.2"), pc)

	// The next round resolves it again.
	nas.Set(func(h *wsdtest.Host) { h.ResolveReplies = nil })
	f.forget()
	f.round()
	f.waitSightings(seen("FILESERVER", "127.0.0.2"), pc)
}

func TestResolveMatchThatDoesNotAnswerTheResolveIsIgnored(t *testing.T) {
	relatesTo := regexp.MustCompile(`<wsa:RelatesTo>[^<]*</wsa:RelatesTo>`)
	address := regexp.MustCompile(`<wsa:Address>[^<]*</wsa:Address>`)
	never := func(string) []string { return nil }
	for _, tc := range []struct {
		name  string
		setup func(lan *wsdtest.LAN, nas *wsdtest.Host) (others []*wsdtest.Host)
	}{
		{"another RelatesTo", func(_ *wsdtest.LAN, nas *wsdtest.Host) []*wsdtest.Host {
			nas.Set(func(h *wsdtest.Host) {
				h.ResolveReplies = func(rm string) []string {
					return []string{relatesTo.ReplaceAllString(rm, "<wsa:RelatesTo>"+wsdtest.CaptureRelatesTo+"</wsa:RelatesTo>")}
				}
			})
			return nil
		}},
		{"another endpoint", func(_ *wsdtest.LAN, nas *wsdtest.Host) []*wsdtest.Host {
			nas.Set(func(h *wsdtest.Host) {
				h.ResolveReplies = func(rm string) []string {
					return []string{address.ReplaceAllString(rm, "<wsa:Address>urn:uuid:5e1f0000-0000-4000-8000-ffffffffffff</wsa:Address>")}
				}
			})
			return nil
		}},
		{"an XAddr on another host", func(lan *wsdtest.LAN, nas *wsdtest.Host) []*wsdtest.Host {
			victim := lan.AddHost("127.0.0.3", "VICTIM/Workgroup:WORKGROUP")
			victim.Set(func(h *wsdtest.Host) { h.Repeats = 0 })
			nas.Set(func(h *wsdtest.Host) { h.XAddr = victim.URL() + "/" + victim.EPR + "/" })
			return []*wsdtest.Host{victim}
		}},
		{"from another host", func(lan *wsdtest.LAN, nas *wsdtest.Host) []*wsdtest.Host {
			// It answers Resolves for the NAS's endpoint, with an XAddr
			// of its own.
			impostor := lan.AddWSDDHost("127.0.0.4", "IMPOSTOR/Workgroup:WORKGROUP")
			impostor.Set(func(h *wsdtest.Host) { h.Repeats, h.EPR = 0, nas.EPR })
			nas.Set(func(h *wsdtest.Host) { h.ResolveReplies = never })
			return []*wsdtest.Host{impostor}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			nas := f.lan.AddWSDDHost("127.0.0.2", "FILESERVER/Workgroup:WORKGROUP")
			others := tc.setup(f.lan, nas)
			r := f.round()

			if !waitFor(func() bool { return f.lan.ResolveCount() == 1 }) {
				t.Fatal("no Resolve for the NAS")
			}
			time.Sleep(200 * time.Millisecond) // for the ResolveMatch
			r.closeWindow()
			want := seen("", "127.0.0.2")
			f.waitSightings(want)
			f.expectSightingsStay(want)
			for _, h := range append(others, nas) {
				if n := h.GetCount(); n != 0 {
					t.Errorf("%s got %d Gets, want none", h.IP, n)
				}
			}
		})
	}
}

func TestUnansweredResolvesInARoundAreCapped(t *testing.T) {
	f := newFixture(t)
	// It answers for many endpoints, none with XAddrs, and answers no
	// Resolve.
	flood := f.lan.AddWSDDHost("127.0.0.2", "FLOOD/Workgroup:WORKGROUP")
	const endpoints = maxResolves + 4
	flood.Set(func(h *wsdtest.Host) {
		epr := h.EPR
		h.Replies = func(pm string) []string { return endpointReplies(pm, epr, endpoints) }
		h.ResolveReplies = func(string) []string { return nil }
	})
	r := f.round()

	if !waitFor(func() bool { return f.lan.ResolveCount() >= maxResolves }) {
		t.Fatalf("%d Resolves, want %d", f.lan.ResolveCount(), maxResolves)
	}
	time.Sleep(200 * time.Millisecond) // for any more
	if n := f.lan.ResolveCount(); n != maxResolves {
		t.Fatalf("%d Resolves for %d endpoints that answer none, want %d", n, endpoints, maxResolves)
	}
	// The endpoints past the cap are reported by address at once, and
	// the ones with a Resolve as the window closes.
	byAddress := func(n int) []Sighting {
		out := make([]Sighting, n)
		for i := range out {
			out[i] = seen("", "127.0.0.2")
		}
		return out
	}
	f.waitSightings(byAddress(endpoints - maxResolves)...)
	r.closeWindow()
	f.waitSightings(byAddress(endpoints)...)
}

func TestEveryComputerFormNamesTheHost(t *testing.T) {
	for _, computer := range []string{
		"OFFICE-PC/Workgroup:WORKGROUP",
		`OFFICE-PC\Workgroup:WORKGROUP`,
		"OFFICE-PC/Domain:CORP.EXAMPLE",
		`OFFICE-PC\NotJoined`,
	} {
		t.Run(computer, func(t *testing.T) {
			f := newFixture(t)
			f.lan.AddHost("127.0.0.2", computer)
			f.round()
			f.waitSightings(seen("OFFICE-PC", "127.0.0.2"))
		})
	}
}

func TestHostWhoseGetFailsIsReportedByAddressUntilANameArrives(t *testing.T) {
	for _, tc := range []struct {
		name    string
		degrade func(*wsdtest.Host)
	}{
		{"Get fails", func(h *wsdtest.Host) { h.GetStatus = http.StatusInternalServerError }},
		{"Get names nothing", func(h *wsdtest.Host) { h.Computer = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			pc := f.lan.AddHost("127.0.0.2", "OFFICE-PC/Workgroup:WORKGROUP")
			pc.Set(tc.degrade)
			r := f.round()
			f.waitSightings(seen("", "127.0.0.2"))
			r.closeWindow()

			pc.Set(func(h *wsdtest.Host) { h.GetStatus, h.Computer = 0, "OFFICE-PC/Workgroup:WORKGROUP" })
			f.forget()
			f.round()
			f.waitSightings(seen("OFFICE-PC", "127.0.0.2"))
			if n := f.keptEndpoints(); n != 1 {
				t.Errorf("the client keeps %d endpoints once a Get named one, want 1", n)
			}
		})
	}
}

func TestXAddrOnAnotherHostGetsNoGet(t *testing.T) {
	f := newFixture(t)
	victim := f.lan.AddHost("127.0.0.3", "VICTIM/Workgroup:WORKGROUP")
	victim.Set(func(h *wsdtest.Host) { h.Repeats = 0 }) // it never answers a Probe itself
	pc := f.lan.AddHost("127.0.0.2", "OFFICE-PC/Workgroup:WORKGROUP")
	pc.Set(func(h *wsdtest.Host) { h.XAddr = victim.URL() + "/" + victim.EPR + "/" })
	f.round()

	want := seen("", "127.0.0.2")
	f.waitSightings(want)
	f.expectSightingsStay(want)
	if n := victim.GetCount() + pc.GetCount(); n != 0 {
		t.Fatalf("%d Gets sent for an XAddr that names another host, want none", n)
	}
}

func TestHostileRepliesAreIgnoredWithoutHanging(t *testing.T) {
	f := newFixture(t)
	f.lan.AddHost("127.0.0.2", "OFFICE-PC/Workgroup:WORKGROUP")

	// Nothing this device sends is a usable ProbeMatch.
	noisy := f.lan.AddHost("127.0.0.4", "NOISY/Workgroup:WORKGROUP")
	noisy.Set(func(h *wsdtest.Host) {
		h.Replies = func(pm string) []string {
			// Each fits one unfragmented datagram: WSL's loopback drops
			// fragments, and the parser tests cover bigger input.
			return []string{
				"\x00\xff garbage",
				strings.Repeat("<a>", 200) + strings.Repeat("</a>", 200),
				pm[:len(pm)/2],
				strings.Replace(pm, "<wsa:RelatesTo>", "<wsa:RelatesTo>x", 1),
				strings.Replace(pm, "pub:Computer</wsd:Types>", "wprt:PrintDeviceType</wsd:Types>", 1),
				strings.Replace(strings.Replace(pm, "?>", `?><!DOCTYPE e [<!ENTITY a "aaaa">]>`, 1),
					"<wsa:RelatesTo>", "<wsa:RelatesTo>&a;", 1),
			}
		}
	})

	// These answer properly, but their metadata is unusable.
	oversized := f.lan.AddHost("127.0.0.5", "")
	oversized.Set(func(h *wsdtest.Host) {
		h.GetBody = strings.Replace(h.GetResponse, wsdtest.CaptureComputer,
			"<pub:Computer>OVERSIZED/Workgroup:WORKGROUP</pub:Computer>", 1) + strings.Repeat(" ", 1<<20)
	})
	malformed := f.lan.AddHost("127.0.0.6", "")
	malformed.Set(func(h *wsdtest.Host) {
		body := strings.Replace(h.GetResponse, wsdtest.CaptureComputer, "<pub:Computer>MALFORMED/NotJoined</pub:Computer>", 1)
		h.GetBody = body[:len(body)-30]
	})
	f.lan.AddHost("127.0.0.7", "../../etc/Workgroup:WORKGROUP")

	f.round()
	want := []Sighting{
		seen("OFFICE-PC", "127.0.0.2"),
		seen("", "127.0.0.5"),
		seen("", "127.0.0.6"),
		seen("", "127.0.0.7"),
	}
	f.waitSightings(want...)
	f.expectSightingsStay(want...)
	if n := noisy.GetCount(); n != 0 {
		t.Fatalf("the noisy device got %d Gets, want none", n)
	}
}

func TestStalledGetHoldsUpNeitherOtherHostsNorShutdown(t *testing.T) {
	f := newFixture(t)
	stall := make(chan struct{})
	stalled := f.lan.AddHost("127.0.0.3", "STALLED/Workgroup:WORKGROUP")
	t.Cleanup(func() { close(stall) }) // before the host's server closes
	stalled.Set(func(h *wsdtest.Host) { h.Stall = stall })
	f.lan.AddHost("127.0.0.2", "OFFICE-PC/Workgroup:WORKGROUP")
	r := f.round()

	f.waitSightings(seen("OFFICE-PC", "127.0.0.2"))
	if !waitFor(func() bool { return stalled.GetCount() == 1 }) {
		t.Fatal("the stalled host got no Get")
	}
	f.cancel()
	stopped := make(chan struct{})
	go func() {
		<-r.done
		f.client.Wait()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("the round and Wait did not return within 1 s of the context ending, with a Get stalled")
	}
}

func TestProbeGoesOutOnEveryInterface(t *testing.T) {
	f := newFixture(t)
	pc := f.lan.AddHost("127.0.0.2", "OFFICE-PC/Workgroup:WORKGROUP")
	r := f.round(loopback(t, "127.0.0.1", "127.0.0.9")...)

	f.lan.WaitProbes(2)
	if got, want := f.lan.ProbeSources(0), []string{"127.0.0.1", "127.0.0.9"}; !slices.Equal(got, want) {
		t.Fatalf("Probes came from %v, want one from each interface %v", got, want)
	}
	// The PC hears both Probes and answers each 4 times.
	want := seen("OFFICE-PC", "127.0.0.2")
	f.waitSightings(want)
	f.expectSightingsStay(want)
	if n := pc.GetCount(); n != 1 {
		t.Fatalf("the host got %d metadata Gets for a Probe heard through 2 interfaces, want 1", n)
	}
	if sent := r.closeWindow(); sent != 2 {
		t.Fatalf("the round says it sent %d Probes, want 2", sent)
	}
}

func TestResolveGoesOutThroughTheSocketTheProbeMatchCameIn(t *testing.T) {
	f := newFixture(t)
	// The NAS is on the LAN behind the second interface only.
	nas := f.lan.AddWSDDHost("127.0.0.2", "FILESERVER/Workgroup:WORKGROUP")
	nas.Set(func(h *wsdtest.Host) { h.Only = "127.0.0.9" })
	f.round(loopback(t, "127.0.0.1", "127.0.0.9")...)

	f.waitSightings(seen("FILESERVER", "127.0.0.2"))
	probes, resolves := nas.HeardFrom()
	if len(probes) != 1 || !slices.Equal(resolves, probes) {
		t.Fatalf("the NAS heard Probes from %v and Resolves from %v, want one Resolve, from the socket its Probe came from",
			probes, resolves)
	}
}

func TestInterfaceThatCannotBeBoundIsSkipped(t *testing.T) {
	f := newFixture(t)
	f.lan.AddHost("127.0.0.2", "OFFICE-PC/Workgroup:WORKGROUP")
	ifs := loopback(t, "127.0.0.1")
	gone := ifs[0]
	gone.Addr = netip.MustParseAddr("192.0.2.1") // no longer this host's address
	r := f.round(gone, ifs[0])

	f.waitSightings(seen("OFFICE-PC", "127.0.0.2"))
	if got, want := f.lan.ProbeSources(0), []string{"127.0.0.1"}; !slices.Equal(got, want) {
		t.Fatalf("Probes came from %v, want %v", got, want)
	}
	if sent := r.closeWindow(); sent != 1 {
		t.Fatalf("the round says it sent %d Probes, want 1", sent)
	}
}

func TestEndpointFloodQueuesNoGetsAndReportsTheHostByAddress(t *testing.T) {
	f := newFixture(t)
	stall := make(chan struct{})
	flood := f.lan.AddHost("127.0.0.2", "FLOOD/Workgroup:WORKGROUP")
	t.Cleanup(func() { close(stall) }) // before the host's server closes
	const endpoints = 80
	flood.Set(func(h *wsdtest.Host) {
		epr := h.EPR
		h.Stall = stall
		h.Replies = func(pm string) []string { return endpointReplies(pm, epr, endpoints) }
	})
	goroutines := runtime.NumGoroutine()
	f.round()

	if !waitFor(func() bool { n, _ := flood.GetsInFlight(); return n == maxGets }) {
		n, _ := flood.GetsInFlight()
		t.Fatalf("the host got %d Gets, want %d", n, maxGets)
	}
	// Every endpoint past the Get slots is reported by address at once.
	if !waitFor(func() bool { return len(f.sightings()) >= endpoints-maxGets }) {
		t.Fatalf("%d sightings, want %d while the first %d Gets hang", len(f.sightings()), endpoints-maxGets, maxGets)
	}
	if n, open := flood.GetsInFlight(); n != maxGets || open != maxGets {
		t.Errorf("reported after %d Gets, %d still open; want it reported while the first %d hang", n, open, maxGets)
	}
	if grown := runtime.NumGoroutine() - goroutines; grown > endpoints/2 {
		t.Errorf("%d goroutines more than before the round started, for %d endpoints", grown, endpoints)
	}
	if n := f.keptEndpoints(); n > maxGets {
		t.Errorf("the client keeps %d endpoints with %d Gets in flight, want no more than those", n, maxGets)
	}

	// The hung Gets time out and name nothing.
	if !waitFor(func() bool { _, open := flood.GetsInFlight(); return open == 0 && f.keptEndpoints() == 0 }) {
		t.Errorf("the client keeps %d endpoints after every Get failed, want none", f.keptEndpoints())
	}
	for _, s := range f.sightings() {
		if s != seen("", "127.0.0.2") {
			t.Errorf("sighting %v, want the host by address", s)
		}
	}
}

func TestEndpointsKeptAreCapped(t *testing.T) {
	f := newFixture(t)
	flood := f.lan.AddHost("127.0.0.2", "FLOOD/Workgroup:WORKGROUP")
	const endpoints = maxEndpoints + 2
	sent := make(chan struct{})
	flood.Set(func(h *wsdtest.Host) {
		epr := h.EPR
		h.Replies = func(pm string) []string { return endpointReplies(pm, epr, endpoints) }
		// Each endpoint answers once the one before it has had its Get,
		// or 200 ms on when it gets none, so a Get slot is free for
		// every one.
		gets := 0
		h.Pace = func(i int) {
			deadline := time.Now().Add(200 * time.Millisecond)
			for i > 0 && flood.GetCount() == gets && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			gets = flood.GetCount()
			if i == endpoints-1 {
				close(sent)
			}
		}
	})
	f.round()

	select {
	case <-sent:
	case <-time.After(30 * time.Second):
		t.Fatalf("the host answered for %d endpoints in 30 s, want %d", flood.GetCount(), endpoints)
	}
	time.Sleep(200 * time.Millisecond)
	named := false
	for _, s := range f.sightings() {
		switch s {
		case seen("FLOOD", "127.0.0.2"):
			named = true
		case seen("", "127.0.0.2"):
		default:
			t.Errorf("sighting %v, want the host by name or by address", s)
		}
	}
	if !named {
		t.Error("no sighting names the host")
	}
	if n := flood.GetCount(); n > maxEndpoints {
		t.Errorf("the host got %d Gets for %d endpoints, want no more than %d", n, endpoints, maxEndpoints)
	}
	if n := f.keptEndpoints(); n > maxEndpoints {
		t.Errorf("the client keeps %d endpoints, want no more than %d", n, maxEndpoints)
	}
}

func TestEndpointsAnsweredInARoundAreCapped(t *testing.T) {
	f := newFixture(t)
	// Its Gets name nothing, so the client keeps none of its endpoints
	// and the endpoint cap never applies.
	flood := f.lan.AddHost("127.0.0.2", "")
	const endpoints = maxAnswered + 4
	sent := make(chan struct{})
	flood.Set(func(h *wsdtest.Host) {
		epr := h.EPR
		h.Replies = func(pm string) []string { return endpointReplies(pm, epr, endpoints) }
		// As in TestEndpointsKeptAreCapped, each endpoint answers once the
		// one before it has had its Get, or 200 ms on.
		gets := 0
		h.Pace = func(i int) {
			deadline := time.Now().Add(200 * time.Millisecond)
			for i > 0 && flood.GetCount() == gets && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			gets = flood.GetCount()
			if i == endpoints-1 {
				close(sent)
			}
		}
	})
	f.round()

	select {
	case <-sent:
	case <-time.After(30 * time.Second):
		t.Fatalf("the host answered for %d endpoints in 30 s, want %d", flood.GetCount(), endpoints)
	}
	time.Sleep(200 * time.Millisecond)
	if n := flood.GetCount(); n > maxAnswered {
		t.Errorf("the host got %d Gets for %d endpoints answering in one round, want no more than %d",
			n, endpoints, maxAnswered)
	}
	got := f.sightings()
	if len(got) == 0 || len(got) > maxAnswered {
		t.Errorf("%d sightings for %d endpoints answering in one round, want 1 to %d", len(got), endpoints, maxAnswered)
	}
	for _, s := range got {
		if s != seen("", "127.0.0.2") {
			t.Errorf("sighting %v, want the host by address", s)
		}
	}
}

func TestMetadataOfAnEndpointNoRoundSawAgesAfterARoundThatSent(t *testing.T) {
	f := newFixture(t)
	pc := f.lan.AddHost("127.0.0.2", "OFFICE-PC/Workgroup:WORKGROUP")
	r := f.round()
	f.waitSightings(seen("OFFICE-PC", "127.0.0.2"))
	r.closeWindow()
	if n := f.keptEndpoints(); n != 1 {
		t.Fatalf("the client keeps %d endpoints once a Get named one, want 1", n)
	}

	// The host goes quiet, and its metadata goes with the round after the TTL.
	pc.Set(func(h *wsdtest.Host) { h.Repeats = 0 })
	f.advance(metaTTL)
	if sent := f.round().closeWindow(); sent != 1 {
		t.Fatalf("the round says it sent %d Probes, want 1", sent)
	}
	if n := f.keptEndpoints(); n != 0 {
		t.Errorf("the client keeps %d endpoints after a round that sent, with the host unseen for the TTL, want none", n)
	}
}

func TestRoundThatSentNothingAgesNoMetadata(t *testing.T) {
	f := newFixture(t)
	f.lan.AddHost("127.0.0.2", "OFFICE-PC/Workgroup:WORKGROUP")
	r := f.round()
	f.waitSightings(seen("OFFICE-PC", "127.0.0.2"))
	r.closeWindow()

	f.advance(2 * metaTTL)
	gone := f.ifs[0]
	gone.Addr = netip.MustParseAddr("192.0.2.1") // no longer this host's address
	if sent := f.round(gone).closeWindow(); sent != 0 {
		t.Fatalf("the round says it sent %d Probes, want none", sent)
	}
	if n := f.keptEndpoints(); n != 1 {
		t.Errorf("the client keeps %d endpoints after a round that sent nothing, want 1", n)
	}
}
