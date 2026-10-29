package smbdisc

import (
	"maps"
	"slices"
	"sync"
	"testing"
	"time"
)

// fakeNetwork stands in for the app's network-change notification.
type fakeNetwork struct {
	mu     sync.Mutex
	notify map[int]func() // by subscription
	nextID int
}

func (n *fakeNetwork) subscribe(notify func()) (unsubscribe func()) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.notify == nil {
		n.notify = map[int]func(){}
	}
	id := n.nextID
	n.nextID++
	n.notify[id] = notify
	return func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		delete(n.notify, id)
	}
}

func (n *fakeNetwork) subscribers() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.notify)
}

// change tells every subscriber that the network changed.
func (n *fakeNetwork) change() {
	n.mu.Lock()
	notify := slices.Collect(maps.Values(n.notify))
	n.mu.Unlock()
	for _, f := range notify {
		f()
	}
}

func TestNetworkChangeProbesTheNewNetworkAtOnce(t *testing.T) {
	lan := startTestLAN(t)
	var mu sync.Mutex
	current := loopback(t, "127.0.0.1")
	r := startEngineOn(t, lan, func() ([]lanInterface, error) {
		mu.Lock()
		defer mu.Unlock()
		return current, nil
	})
	lan.WaitProbes(1)
	r.clock.Advance(collectWindow) // the first round is over

	// Wi-Fi joins another network, where a NAS answers.
	mu.Lock()
	current = loopback(t, "127.0.0.9")
	mu.Unlock()
	lan.AddHost("127.0.0.2", "NAS/Workgroup:WORKGROUP")
	lan.mdns.addResponder("127.0.0.3", "storage")
	r.network.change()

	// The clock stays short of the next round's tick.
	lan.WaitProbes(2)
	if got, want := lan.ProbeSources(1), []string{"127.0.0.9"}; !slices.Equal(got, want) {
		t.Fatalf("the Probe after the change came from %v, want %v", got, want)
	}
	// The mDNS query goes out with it, from the new interface too.
	if !waitFor(func() bool { return slices.Contains(lan.mdns.ptrQueries(), "127.0.0.9") }) {
		t.Fatalf("no mDNS query from the new interface; saw %v", lan.mdns.ptrQueries())
	}
	r.waitListing(
		entry{"smb/NAS", "NAS", "smb://127.0.0.2"},
		entry{"smb/STORAGE", "storage", "smb://127.0.0.3"},
	)
}

// expectProbesStay checks that the LAN has still seen n Probes in all
// after the engine has had time to send any more.
func (l *testLAN) expectProbesStay(t *testing.T, n int) {
	t.Helper()
	time.Sleep(200 * time.Millisecond)
	if got := l.ProbeCount(); got != n {
		t.Fatalf("saw %d Probes, want %d", got, n)
	}
}

func TestNetworkChangeBurstRunsOneExtraRound(t *testing.T) {
	lan := startTestLAN(t)
	r := startEngine(t, lan)
	lan.WaitProbes(1)

	// The link flaps while the first round collects its replies.
	for range 10 {
		r.network.change()
	}
	lan.expectProbesStay(t, 1)
	r.clock.Advance(collectWindow)
	lan.WaitProbes(2)
	lan.expectProbesStay(t, 2)

	// The burst is spent once that round is over too, and the rounds on
	// the clock go on.
	r.clock.Advance(collectWindow)
	lan.expectProbesStay(t, 2)
	r.clock.Advance(roundInterval - collectWindow)
	lan.WaitProbes(3)
}

func TestStoppedEngineLeavesTheNetworkChangeNotification(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(r *rig)
	}{
		{"Stop", func(r *rig) { r.engine.Stop() }},
		{"context ends", func(r *rig) { r.cancel(); r.engine.Wait() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lan := startTestLAN(t)
			r := startEngine(t, lan)
			if n := r.network.subscribers(); n != 1 {
				t.Fatalf("the running engine has %d network-change subscriptions, want 1", n)
			}
			lan.WaitProbes(1)

			tc.stop(r)
			if n := r.network.subscribers(); n != 0 {
				t.Fatalf("the stopped engine has %d network-change subscriptions, want none", n)
			}
		})
	}
}
