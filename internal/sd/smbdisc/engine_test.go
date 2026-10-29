package smbdisc

import (
	"context"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/czz/movian-go/internal/htsmsg"
	propcore "github.com/czz/movian-go/internal/prop"
	"github.com/czz/movian-go/internal/sd/smbdisc/lan"
	"github.com/czz/movian-go/internal/sd/smbdisc/wsd/wsdtest"
	"github.com/czz/movian-go/internal/service"
	settings "github.com/czz/movian-go/internal/settings"
)

// The engine is driven only at its network edge. A fake WS-Discovery
// group on loopback hands each Probe to fake hosts that replay the
// sanitized Windows captures, and the tests read Local network back
// from a real service registry on a real prop manager.

// fakeClock moves only when a test advances it.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []fakeTimer
}

type fakeTimer struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	at := c.now.Add(d)
	if !at.After(c.now) {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, fakeTimer{at, ch})
	return ch
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if w.at.After(c.now) {
			kept = append(kept, w)
		} else {
			w.ch <- c.now
		}
	}
	c.waiters = kept
}

// testLAN is the WS-Discovery group of wsdtest beside the LAN's mDNS
// group.
type testLAN struct {
	*wsdtest.LAN
	mdns *fakeMDNS
}

func startTestLAN(t *testing.T) *testLAN {
	t.Helper()
	return &testLAN{LAN: wsdtest.StartLAN(t), mdns: startFakeMDNS(t)}
}

// entry is one server as Local network shows it.
type entry struct{ id, title, url string }

type rig struct {
	t       *testing.T
	pm      *propcore.PropManager
	sm      *settings.SettingsManager
	ss      *service.ServiceSystem
	clock   *fakeClock
	network *fakeNetwork
	engine  *Engine
	cancel  context.CancelFunc
}

// startEngine starts the engine against lan on a fake clock, over a
// service registry wired to real settings as at Movian start. It
// probes through the loopback interface only.
func startEngine(t *testing.T, lan *testLAN) *rig {
	t.Helper()
	return startEngineOn(t, lan, interfaces(loopback(t, "127.0.0.1")...))
}

// startEngineOn is startEngine with the interface list ifs. A Local
// network page is open, as rounds on the clock need one.
func startEngineOn(t *testing.T, lan *testLAN, ifs func() ([]lan.Interface, error)) *rig {
	t.Helper()
	r := startEngineClosed(t, lan, ifs)
	// Opening the page is what starts the first round.
	release := r.engine.Watch()
	t.Cleanup(release)
	return r
}

// startEngineClosed is startEngineOn with no page open.
func startEngineClosed(t *testing.T, lan *testLAN, ifs func() ([]lan.Interface, error)) *rig {
	t.Helper()
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.network = &fakeNetwork{}
	r.engine = Start(ctx, Config{
		Services:       r.ss,
		WSDDest:        lan.Dest(),
		MDNSDest:       lan.mdns.dest(),
		Interfaces:     ifs,
		Clock:          r.clock,
		NetworkChanges: r.network.subscribe,
	})
	t.Cleanup(func() {
		cancel()
		r.engine.Wait()
	})
	return r
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

// interfaces is an interface list that never changes.
// lanInterface is lan.Interface for the tests that name a testLAN lan,
// which hides the package.
type lanInterface = lan.Interface

func interfaces(ifs ...lan.Interface) func() ([]lan.Interface, error) {
	return func() ([]lan.Interface, error) { return ifs, nil }
}

// newRig makes a service registry wired to real settings as at Movian
// start, with no engine yet.
func newRig(t *testing.T) *rig {
	t.Helper()
	pm := propcore.NewPropManager()
	sm := settings.NewSettingsManager(pm)
	sm.SetStore(htsmsg.NewStore(t.TempDir()))
	sm.Start()
	ss := service.NewServiceSystem(pm)
	ss.Start()
	ss.SetSettingsHooks(serviceSettingsHooks(sm))
	return &rig{t: t, pm: pm, sm: sm, ss: ss, clock: newFakeClock()}
}

// serviceSettingsHooks wires the registry's managed-service settings
// to sm, as cmd/movian-go does.
func serviceSettingsHooks(sm *settings.SettingsManager) *service.ServiceSettingsHooks {
	return &service.ServiceSettingsHooks{
		SD: sm.SD,
		P:  sm.P,
		AddDir: func(parent, title *propcore.Prop, subtype, icon string, url string) *propcore.Prop {
			return sm.AddDir(parent, title, subtype, icon, nil, url)
		},
		CreateBoundBool: func(parent, title *propcore.Prop, value int, writeProp *propcore.Prop, store, key string) any {
			return sm.SettingCreate(settings.SettingBool, parent, settings.SettingsInitialUpdate,
				settings.SettingTagTitle, title, settings.SettingTagValue, value,
				settings.SettingTagWriteProp, writeProp, settings.SettingTagStore, store, key)
		},
		CreateBoundString: func(parent, title *propcore.Prop, value string, writeProp *propcore.Prop, store, key string) any {
			return sm.SettingCreate(settings.SettingString, parent, settings.SettingsInitialUpdate,
				settings.SettingTagTitle, title, settings.SettingTagValue, value,
				settings.SettingTagWriteProp, writeProp, settings.SettingTagStore, store, key)
		},
		SetBool: func(s any, v int) { sm.SettingSet(s.(*settings.Setting), settings.SettingBool, v) },
		Destroy: func(s any) { sm.Destroy(s.(*settings.Setting)) },
	}
}

// listing returns the discovered servers on Local network, by id.
func (r *rig) listing() []entry {
	propcore.GlobalDispatchBarrier()
	var out []entry
	for _, s := range r.ss.GetServicesByOrigin(service.SvcOriginDiscovered) {
		root := s.Root()
		out = append(out, entry{
			id:    service.ServiceGetID(s),
			title: r.pm.GetString(root.FindChild("title"), ""),
			url:   r.pm.GetString(root.FindChild("url"), ""),
		})
	}
	slices.SortFunc(out, func(a, b entry) int { return strings.Compare(a.id, b.id) })
	return out
}

// waitListing waits until Local network shows exactly want.
func (r *rig) waitListing(want ...entry) {
	r.t.Helper()
	if !waitFor(func() bool { return slices.Equal(r.listing(), want) }) {
		r.t.Fatalf("Local network shows %v, want %v", r.listing(), want)
	}
}

// expectListingStays checks that Local network still shows exactly
// want after the engine has had time to act on any late replies.
func (r *rig) expectListingStays(want ...entry) {
	r.t.Helper()
	time.Sleep(200 * time.Millisecond)
	if got := r.listing(); !slices.Equal(got, want) {
		r.t.Fatalf("Local network shows %v, want %v", got, want)
	}
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

// settingsOf returns the settings in the Settings dir titled title, as
// setting title → value. A string setting with nothing stored shows
// its defaultValue.
func (r *rig) settingsOf(title string) map[string]string {
	propcore.GlobalDispatchBarrier()
	child := func(p *propcore.Prop, names ...string) *propcore.Prop {
		for _, n := range names {
			if p == nil {
				return nil
			}
			p = p.FindChild(n)
		}
		return p
	}
	children := func(p *propcore.Prop) []*propcore.Prop {
		if p == nil {
			return nil
		}
		return p.GetChildren()
	}
	titleOf := func(p *propcore.Prop) string { return r.pm.GetString(child(p, "metadata", "title"), "") }
	for _, dir := range children(child(r.sm.SD(), "nodes")) {
		if titleOf(dir) != title {
			continue
		}
		out := map[string]string{}
		for _, s := range children(child(dir, "nodes")) {
			v := s.FindChild("value")
			if d := s.FindChild("defaultValue"); d != nil {
				out[titleOf(s)] = r.pm.GetString(v, r.pm.GetString(d, ""))
			} else {
				out[titleOf(s)] = fmt.Sprint(r.pm.GetInt(v, -1))
			}
		}
		return out
	}
	return nil
}

func TestDiscoveredHostHasTheManagedServiceSettings(t *testing.T) {
	lan := startTestLAN(t)
	lan.AddHost("127.0.0.2", "OFFICE-PC/Workgroup:WORKGROUP")
	r := startEngine(t, lan)
	r.waitListing(entry{"smb/OFFICE-PC", "OFFICE-PC", "smb://127.0.0.2"})

	// The entry is listed a moment before its settings are made.
	want := map[string]string{"Enabled on home screen": "0", "Name": "OFFICE-PC", "Type": "server"}
	if !waitFor(func() bool { return maps.Equal(r.settingsOf("OFFICE-PC"), want) }) {
		t.Fatalf("settings of OFFICE-PC = %v, want %v", r.settingsOf("OFFICE-PC"), want)
	}
}

func TestHostDropsOutRightAfterItsThirdMissedRound(t *testing.T) {
	lan := startTestLAN(t)
	stall := make(chan struct{})
	pc := lan.AddHost("127.0.0.2", "OFFICE-PC/Workgroup:WORKGROUP")
	pc.Set(func(h *wsdtest.Host) { h.Stall = stall })
	r := startEngine(t, lan)

	// The host is last seen 2 s into the first round, when its Get ends.
	if !waitFor(func() bool { return pc.GetCount() == 1 }) {
		t.Fatal("the host got no Get")
	}
	r.clock.Advance(2 * time.Second)
	close(stall)
	want := entry{"smb/OFFICE-PC", "OFFICE-PC", "smb://127.0.0.2"}
	r.waitListing(want)

	pc.Set(func(h *wsdtest.Host) { h.Repeats, h.Stall = 0, nil })
	r.clock.Advance(roundInterval - 2*time.Second)
	lan.WaitProbes(2)
	for probes := 3; probes <= 4; probes++ {
		r.clock.Advance(roundInterval)
		lan.WaitProbes(probes)
	}
	r.expectListingStays(want) // the third missed round is still collecting
	r.clock.Advance(collectWindow)
	r.waitListing()
}

// A process that was suspended with the page open wakes up long after a
// round was due: that round starts at the current time with its whole
// collection window, so the hosts stay listed, and the missed rounds
// are not replayed.
func TestOverdueRoundStartsNowAndIsNotReplayed(t *testing.T) {
	lan := startTestLAN(t)
	lan.AddHost("127.0.0.2", "OFFICE-PC/Workgroup:WORKGROUP")
	r := startEngine(t, lan)
	want := entry{"smb/OFFICE-PC", "OFFICE-PC", "smb://127.0.0.2"}
	r.waitListing(want)
	lan.WaitProbes(1)
	r.clock.Advance(collectWindow)
	first := lan.ProbeCount()

	r.clock.Advance(5 * time.Minute)
	lan.WaitProbes(first + 1)
	r.expectListingStays(want)
	if got := lan.ProbeCount(); got != first+1 {
		t.Fatalf("%d Probes after the jump, want %d: one round, not a burst", got, first+1)
	}

	// The round after it is due roundInterval after the one that started
	// at the jump.
	r.clock.Advance(collectWindow)
	r.clock.Advance(roundInterval - collectWindow)
	lan.WaitProbes(first + 2)
	r.expectListingStays(want)
}

func TestNetBIOSPunctuationStaysInTheName(t *testing.T) {
	lan := startTestLAN(t)
	lan.AddHost("127.0.0.2", "ANNA'S-PC/Workgroup:WORKGROUP")
	r := startEngine(t, lan)
	r.waitListing(entry{"smb/ANNA'S-PC", "ANNA'S-PC", "smb://127.0.0.2"})
}

func TestInterfacesAreReadAgainEachRound(t *testing.T) {
	lan := startTestLAN(t)
	var mu sync.Mutex
	current := loopback(t, "127.0.0.1")
	r := startEngineOn(t, lan, func() ([]lanInterface, error) {
		mu.Lock()
		defer mu.Unlock()
		return current, nil
	})
	lan.WaitProbes(1)

	// Wi-Fi comes up beside Ethernet.
	mu.Lock()
	current = loopback(t, "127.0.0.1", "127.0.0.9")
	mu.Unlock()
	r.clock.Advance(roundInterval)
	lan.WaitProbes(3)
	if got, want := lan.ProbeSources(1), []string{"127.0.0.1", "127.0.0.9"}; !slices.Equal(got, want) {
		t.Fatalf("the second round's Probes came from %v, want %v", got, want)
	}
}

// hold returns a channel that blocks until release is called. It is
// released at the latest as the test ends: before the engine is waited
// for when the hold is made after the engine starts, and before the
// servers of the hosts added before it close.
func hold(t *testing.T) (held chan struct{}, release func()) {
	ch := make(chan struct{})
	release = sync.OnceFunc(func() { close(ch) })
	t.Cleanup(release)
	return ch, release
}

// holdFirstSettings makes the next service the registry creates wait
// in its settings hook, with the service listed and the engine's mutex
// held, until release is called. started is closed once it waits.
func (r *rig) holdFirstSettings() (started <-chan struct{}, release func()) {
	held, release := hold(r.t)
	ch := make(chan struct{})
	hooks := serviceSettingsHooks(r.sm)
	addDir := hooks.AddDir
	var once sync.Once
	hooks.AddDir = func(parent, title *propcore.Prop, subtype, icon string, url string) *propcore.Prop {
		once.Do(func() {
			close(ch)
			<-held
		})
		return addDir(parent, title, subtype, icon, url)
	}
	r.ss.SetSettingsHooks(hooks)
	return ch, release
}

func TestSightingStillInFlightWhenTheEngineStopsListsNothing(t *testing.T) {
	lan := startTestLAN(t)
	pc := lan.AddHost("127.0.0.2", "OFFICE-PC/Workgroup:WORKGROUP")
	getHeld, releaseGet := hold(t)
	pc.Set(func(h *wsdtest.Host) { h.Stall = getHeld })
	late := lan.AddHost("127.0.0.3", "LATE/Workgroup:WORKGROUP")
	lateHeld, releaseLate := hold(t)
	late.Set(func(h *wsdtest.Host) {
		h.XAddr = "http://192.0.2.1:5357/late/" // no Get, listed by address
		h.Replies = func(pm string) []string { <-lateHeld; return []string{pm} }
	})
	r := startEngine(t, lan)

	// Making the PC's settings takes a while.
	settingsStarted, releaseSettings := r.holdFirstSettings()

	if !waitFor(func() bool { return pc.GetCount() == 1 }) {
		t.Fatal("the PC got no Get")
	}
	releaseGet()
	select {
	case <-settingsStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("the PC was never listed")
	}
	// The late host's reply is in while the PC is being listed.
	releaseLate()
	time.Sleep(200 * time.Millisecond)

	r.cancel()
	releaseSettings()
	r.engine.Wait()
	want := entry{"smb/OFFICE-PC", "OFFICE-PC", "smb://127.0.0.2"}
	if got := r.listing(); !slices.Equal(got, []entry{want}) {
		t.Fatalf("Local network shows %v once the engine has stopped, want only %v, listed before it stopped", got, want)
	}
	r.expectListingStays(want)
}

func TestStopWaitsForTheListingInProgressAndListsNothingAfter(t *testing.T) {
	lan := startTestLAN(t)
	pc := lan.AddHost("127.0.0.2", "OFFICE-PC/Workgroup:WORKGROUP")
	getHeld, releaseGet := hold(t)
	pc.Set(func(h *wsdtest.Host) { h.Stall = getHeld })
	// Only the engine stopping ends this host's Get.
	stalled := lan.AddHost("127.0.0.3", "STALLED/Workgroup:WORKGROUP")
	stall, _ := hold(t)
	stalled.Set(func(h *wsdtest.Host) { h.Stall = stall })
	r := startEngine(t, lan)
	settingsStarted, releaseSettings := r.holdFirstSettings()

	if !waitFor(func() bool { return pc.GetCount() == 1 && stalled.GetCount() == 1 }) {
		t.Fatalf("the hosts got %d and %d Gets, want 1 each", pc.GetCount(), stalled.GetCount())
	}
	releaseGet()
	select {
	case <-settingsStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("the PC was never listed")
	}

	// The engine is stopped while the PC is being listed.
	stopped := make(chan struct{})
	go func() {
		r.engine.Stop()
		close(stopped)
	}()
	time.Sleep(200 * time.Millisecond)
	select {
	case <-stopped:
		t.Fatal("Stop returned while the PC was still being listed")
	default:
	}
	if _, open := stalled.GetsInFlight(); open != 1 {
		t.Fatal("the engine stopped while the PC was still being listed: the stalled host's Get was dropped")
	}

	releaseSettings()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop did not return within 1 s of the PC being listed")
	}
	want := map[string]string{"Enabled on home screen": "0", "Name": "OFFICE-PC", "Type": "server"}
	if got := r.settingsOf("OFFICE-PC"); !maps.Equal(got, want) {
		t.Errorf("settings of OFFICE-PC = %v as Stop returns, want %v", got, want)
	}
	// The stalled host's Get ended with the stop; it is listed neither
	// by name nor by address.
	r.expectListingStays(entry{"smb/OFFICE-PC", "OFFICE-PC", "smb://127.0.0.2"})
}
