package smb

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	propcore "github.com/czz/movian-go/internal/prop"
	"github.com/czz/movian-go/internal/sd/smbdisc"
	"github.com/czz/movian-go/internal/sd/smbdisc/lan"
	"github.com/czz/movian-go/internal/sd/smbdisc/wsd/wsdtest"
	"github.com/czz/movian-go/internal/service"
)

// NetBIOS host discovery is driven at its edges: a fake master browser
// on loopback answers the __MSBROWSE__ query and a stub stands in for
// the SMBv1 server enumeration. Its reports go to a real SMB discovery
// engine, beside a fake WS-Discovery host, and the tests read Local
// network back from a real service registry.

// browseList is the master browser's server list, as the stubbed
// enumeration returns it: name/workgroup pairs.
type browseList struct {
	mu      sync.Mutex
	servers []string
}

func (b *browseList) set(servers ...string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.servers = servers
}

// enum stands in for SMBEnumServers. It never returns nil, which would
// mean that the master browser could not be asked.
func (b *browseList) enum(string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string{}, b.servers...)
}

// frozenClock never moves: the engine's first round stays open for the
// whole test and no entry ages.
type frozenClock struct{}

func (frozenClock) Now() time.Time { return time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC) }

func (c frozenClock) After(d time.Duration) <-chan time.Time {
	if d > 0 {
		return nil
	}
	ch := make(chan time.Time, 1)
	ch <- c.Now()
	return ch
}

// loopbackOnly is an interface list holding the loopback interface
// under 127.0.0.1.
func loopbackOnly(t *testing.T) func() ([]lan.Interface, error) {
	t.Helper()
	ifis, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(ifis, func(ifi net.Interface) bool { return ifi.Flags&net.FlagLoopback != 0 })
	if i < 0 {
		t.Fatal("no loopback interface")
	}
	ifs := []lan.Interface{{Name: ifis[i].Name, Index: ifis[i].Index, Addr: netip.MustParseAddr("127.0.0.1")}}
	return func() ([]lan.Interface, error) { return ifs, nil }
}

// entry is one server as Local network shows it.
type entry struct{ id, title, url string }

type discovery struct {
	t      *testing.T
	pm     *propcore.PropManager
	ss     *service.ServiceSystem
	engine *smbdisc.Engine
	wsd    *wsdtest.Host
	list   *browseList
	nbns   *fakeNBNS
	nmb    *NMBManager
}

// startDiscovery starts the SMB discovery engine on loopback, on a
// clock that never moves, with a host at 127.0.0.2 that its one
// WS-Discovery round probes.
func startDiscovery(t *testing.T) *discovery {
	t.Helper()
	pm := propcore.NewPropManager()
	ss := service.NewServiceSystem(pm)
	ss.Start()
	group := wsdtest.StartLAN(t)
	// The host keeps the engine's Probe until a test releases it.
	wsd := group.AddHost("127.0.0.2", "")
	wsd.Hold()
	wsd.Set(func(h *wsdtest.Host) { h.Repeats = 1 })
	e := smbdisc.Start(context.Background(), smbdisc.Config{
		Services:   ss,
		WSDDest:    group.Dest(),
		Interfaces: loopbackOnly(t),
		Clock:      frozenClock{},
	})
	t.Cleanup(e.Stop)
	// The engine probes only while a Local network page is open.
	t.Cleanup(e.Watch())
	return &discovery{t: t, pm: pm, ss: ss, engine: e, wsd: wsd}
}

// startNetBIOS starts NetBIOS host discovery, reporting into the
// engine, with a master browser at 192.0.2.20 that lists servers
// (name/workgroup pairs).
func (d *discovery) startNetBIOS(servers ...string) {
	d.t.Helper()
	d.list = &browseList{servers: servers}
	d.nbns = startFakeMasterBrowser(d.t, [4]byte{192, 0, 2, 20})
	d.nmb = startNMB(d.t, d.nbns.addr(), d.engine, func(m *NMBManager) { m.enumServers = d.list.enum })
}

// masterBrowserDrops waits until NetBIOS lists every server on the
// master browser's list, takes servers (names) off that list, and waits
// until NetBIOS has swept them: its mark-and-sweep drops a server
// missing from two lists in a row, so the master browser answers twice
// more.
func (d *discovery) masterBrowserDrops(names ...string) {
	d.t.Helper()
	var all, kept, left []string
	for i := 0; i+1 < len(d.list.servers); i += 2 {
		all = append(all, d.list.servers[i])
		if !slices.Contains(names, d.list.servers[i]) {
			kept = append(kept, d.list.servers[i:i+2]...)
			left = append(left, d.list.servers[i])
		}
	}
	slices.Sort(all)
	d.waitNetBIOSLists(all...)
	d.list.set(kept...)
	for range 2 {
		d.nbns.masterAnswersAgain(d.t)
	}
	slices.Sort(left)
	d.waitNetBIOSLists(left...)
}

// waitNetBIOSLists waits until NetBIOS has taken names from the master
// browser's list; it has reported them by then.
func (d *discovery) waitNetBIOSLists(names ...string) {
	d.t.Helper()
	listed := func() []string {
		var out []string
		for _, ns := range d.nmb.DiscoverServers() {
			out = append(out, ns.name)
		}
		slices.Sort(out)
		return out
	}
	if !waitFor(func() bool { return slices.Equal(listed(), names) }) {
		d.t.Fatalf("NetBIOS lists %v, want %v", listed(), names)
	}
}

// listing returns the discovered servers on Local network, by id.
func (d *discovery) listing() []entry {
	propcore.GlobalDispatchBarrier()
	var out []entry
	for _, s := range d.ss.GetServicesByOrigin(service.SvcOriginDiscovered) {
		root := s.Root()
		out = append(out, entry{
			id:    service.ServiceGetID(s),
			title: d.pm.GetString(root.FindChild("title"), ""),
			url:   d.pm.GetString(root.FindChild("url"), ""),
		})
	}
	slices.SortFunc(out, func(a, b entry) int { return strings.Compare(a.id, b.id) })
	return out
}

// waitListing waits until Local network shows exactly want.
func (d *discovery) waitListing(want ...entry) {
	d.t.Helper()
	if !waitFor(func() bool { return slices.Equal(d.listing(), want) }) {
		d.t.Fatalf("Local network shows %v, want %v", d.listing(), want)
	}
}

// expectListingStays checks that Local network still shows exactly
// want after the engine has had time to act on any late report.
func (d *discovery) expectListingStays(want ...entry) {
	d.t.Helper()
	time.Sleep(200 * time.Millisecond)
	if got := d.listing(); !slices.Equal(got, want) {
		d.t.Fatalf("Local network shows %v, want %v", got, want)
	}
}

func TestNetBIOSServerIsListedByItsName(t *testing.T) {
	d := startDiscovery(t)
	d.startNetBIOS("FILESERVER", "WORKGROUP")
	d.waitListing(entry{"smb/FILESERVER", "FILESERVER", "smb://FILESERVER"})
}

func TestNetBIOSNameMergesWithTheWSDiscoveryHostOfThatName(t *testing.T) {
	want := entry{"smb/OFFICE-PC", "OFFICE-PC", "smb://127.0.0.2"}
	t.Run("NetBIOS first", func(t *testing.T) {
		d := startDiscovery(t)
		d.startNetBIOS("OFFICE-PC", "WORKGROUP")
		d.waitListing(entry{"smb/OFFICE-PC", "OFFICE-PC", "smb://OFFICE-PC"})

		d.wsd.Release("OFFICE-PC/Workgroup:WORKGROUP")
		d.waitListing(want)
	})
	t.Run("WS-Discovery first", func(t *testing.T) {
		d := startDiscovery(t)
		d.wsd.Release("OFFICE-PC/Workgroup:WORKGROUP")
		d.waitListing(want)

		d.startNetBIOS("OFFICE-PC", "WORKGROUP")
		d.waitNetBIOSLists("OFFICE-PC")
		d.expectListingStays(want)
	})
	t.Run("NetBIOS name cut to 15 characters", func(t *testing.T) {
		d := startDiscovery(t)
		d.startNetBIOS("MEDIASERVER-LIV", "WORKGROUP")
		d.waitListing(entry{"smb/MEDIASERVER-LIV", "MEDIASERVER-LIV", "smb://MEDIASERVER-LIV"})

		d.wsd.Release("MEDIASERVER-LIVINGROOM/Workgroup:WORKGROUP")
		d.waitListing(entry{"smb/MEDIASERVER-LIVINGROOM", "MEDIASERVER-LIVINGROOM", "smb://127.0.0.2"})
	})
}

func TestServerNetBIOSDropsThatNoOtherSourceSeesIsRemoved(t *testing.T) {
	d := startDiscovery(t)
	d.startNetBIOS("FILESERVER", "WORKGROUP", "PRINTSERVER", "WORKGROUP")
	d.waitListing(
		entry{"smb/FILESERVER", "FILESERVER", "smb://FILESERVER"},
		entry{"smb/PRINTSERVER", "PRINTSERVER", "smb://PRINTSERVER"},
	)

	d.masterBrowserDrops("FILESERVER")
	d.waitListing(entry{"smb/PRINTSERVER", "PRINTSERVER", "smb://PRINTSERVER"})
}

func TestServerNetBIOSDropsStaysWhileWSDiscoverySeesIt(t *testing.T) {
	d := startDiscovery(t)
	d.startNetBIOS("OFFICE-PC", "WORKGROUP")
	d.waitListing(entry{"smb/OFFICE-PC", "OFFICE-PC", "smb://OFFICE-PC"})
	d.wsd.Release("OFFICE-PC/Workgroup:WORKGROUP")
	want := entry{"smb/OFFICE-PC", "OFFICE-PC", "smb://127.0.0.2"}
	d.waitListing(want)

	d.masterBrowserDrops("OFFICE-PC")
	d.expectListingStays(want)
}
