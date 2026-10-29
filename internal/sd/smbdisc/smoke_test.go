package smbdisc

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/czz/movian-go/internal/sd/smbdisc/lan"
	"github.com/czz/movian-go/internal/trace"
)

// TestLANSmoke runs the engine on this host's real interfaces against
// the real LAN and prints what Local network then lists. It runs only
// with SMBDISC_SMOKE=1, for SMBDISC_SMOKE_SECONDS (default 30). Run it
// on a LAN host of its own: a VM or WSL instance that shares its host's
// address is answered by that host and proves nothing.
func TestLANSmoke(t *testing.T) {
	if os.Getenv("SMBDISC_SMOKE") != "1" {
		t.Skip("set SMBDISC_SMOKE=1 to probe the LAN")
	}
	d := 30 * time.Second
	if s := os.Getenv("SMBDISC_SMOKE_SECONDS"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			t.Fatalf("SMBDISC_SMOKE_SECONDS=%q, want a positive number", s)
		}
		d = time.Duration(n) * time.Second
	}

	ifs, err := lan.Interfaces()
	if err != nil {
		t.Fatalf("interfaces: %v", err)
	}
	for _, ifc := range ifs {
		t.Logf("interface %s (index %d): %v", ifc.Name, ifc.Index, ifc.Addr)
	}
	if len(ifs) == 0 {
		t.Fatal("no interface to probe through")
	}

	r := newRig(t)
	ts := trace.NewTraceSystem(r.pm)
	ts.SetLevel(trace.TRACE_DEBUG)
	ts.Start(t.TempDir(), "smbdisc-smoke")
	t.Cleanup(ts.Fini)

	ctx, cancel := context.WithCancel(context.Background())
	e := Start(ctx, Config{Services: r.ss, Trace: ts})
	// The engine probes only while a page is current; hold one
	// open for the whole run, as the Local network page does.
	release := e.Watch()
	defer release()
	time.Sleep(d)
	listed := r.listing()
	cancel()
	e.Wait()

	t.Logf("Local network after %v: %d SMB servers", d, len(listed))
	titles := map[string]bool{}
	for _, l := range listed {
		t.Logf("  title %q  id %q  url %q", l.title, l.id, l.url)
		if titles[l.title] {
			t.Errorf("%s is listed more than once", l.title)
		}
		titles[l.title] = true
	}
}
