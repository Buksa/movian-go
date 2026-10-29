package core

import (
	"testing"

	propcore "github.com/czz/movian-go/internal/prop"
)

// The Local network page tells the SMB discovery engine it is open, and
// that it closed once the page is destroyed.
func TestDiscoveredPageWatchesDiscoveryWhileOpen(t *testing.T) {
	pm := propcore.NewPropManager()
	bs := NewBackendSystem(pm, nil, nil, nil)
	opened, released := 0, 0
	bs.registerDiscoveredBackend()
	bs.SetDiscoveredWatch(func() func() {
		opened++
		return func() { released++ }
	})

	page := pm.CreateRootEx("page", false)
	if err := bs.Open(page, "discovered:", false); err != nil {
		t.Fatal(err)
	}
	if opened != 1 || released != 0 {
		t.Fatalf("after open: opened %d, released %d; want 1, 0", opened, released)
	}

	pm.Destroy(page)
	propcore.GlobalDispatchBarrier()
	if released != 1 {
		t.Fatalf("after destroy: released %d, want 1", released)
	}
}

// A page the navigator keeps in its history is not a page the user
// looks at: discovery watches only while the page is the selected one.
func TestDiscoveredPageWatchesOnlyWhileSelected(t *testing.T) {
	pm := propcore.NewPropManager()
	bs := NewBackendSystem(pm, nil, nil, nil)
	opened, released := 0, 0
	bs.registerDiscoveredBackend()
	bs.SetDiscoveredWatch(func() func() {
		opened++
		return func() { released++ }
	})

	pages := pm.CreateRootEx("pages", false)
	page := pm.CreateEx(pages, "", nil, false, false)
	other := pm.CreateEx(pages, "", nil, false, false)
	pm.SelectChildProp(page, nil)
	propcore.GlobalDispatchBarrier()
	propcore.GlobalDispatchBarrier()
	if err := bs.Open(page, "discovered:", false); err != nil {
		t.Fatal(err)
	}
	if opened != 1 || released != 0 {
		t.Fatalf("selected on open: opened %d, released %d; want 1, 0", opened, released)
	}

	pm.SelectChildProp(other, nil) // another page opens on top
	propcore.GlobalDispatchBarrier()
	if opened != 1 || released != 1 {
		t.Fatalf("after another page: opened %d, released %d; want 1, 1", opened, released)
	}
	pm.SelectChildProp(other, nil) // reselecting changes nothing
	propcore.GlobalDispatchBarrier()
	if released != 1 {
		t.Fatalf("released %d, want 1", released)
	}

	pm.SelectChildProp(page, nil) // Back onto Local network
	propcore.GlobalDispatchBarrier()
	if opened != 2 || released != 1 {
		t.Fatalf("after Back: opened %d, released %d; want 2, 1", opened, released)
	}

	pm.Destroy(page) // destroyed while selected
	propcore.GlobalDispatchBarrier()
	if released != 2 {
		t.Fatalf("after destroy: released %d, want 2", released)
	}
	pm.SelectChildProp(other, nil)
	propcore.GlobalDispatchBarrier()
	if opened != 2 || released != 2 {
		t.Fatalf("a destroyed page watched again: opened %d, released %d", opened, released)
	}
}

// A page opened behind the selected one does not watch until selected.
func TestDiscoveredPageNotSelectedOnOpenDoesNotWatch(t *testing.T) {
	pm := propcore.NewPropManager()
	bs := NewBackendSystem(pm, nil, nil, nil)
	opened := 0
	bs.registerDiscoveredBackend()
	bs.SetDiscoveredWatch(func() func() {
		opened++
		return func() {}
	})
	pages := pm.CreateRootEx("pages", false)
	page := pm.CreateEx(pages, "", nil, false, false)
	other := pm.CreateEx(pages, "", nil, false, false)
	pm.SelectChildProp(other, nil)
	propcore.GlobalDispatchBarrier()
	propcore.GlobalDispatchBarrier()
	if err := bs.Open(page, "discovered:", false); err != nil {
		t.Fatal(err)
	}
	if opened != 0 {
		t.Fatalf("opened %d, want 0", opened)
	}
	pm.SelectChildProp(page, nil)
	propcore.GlobalDispatchBarrier()
	if opened != 1 {
		t.Fatalf("opened %d after select, want 1", opened)
	}
}

// A destroyed page leaves no subscriber on the long-lived pages parent.
func TestDestroyedDiscoveredPageUnsubscribesFromItsParent(t *testing.T) {
	pm := propcore.NewPropManager()
	bs := NewBackendSystem(pm, nil, nil, nil)
	bs.registerDiscoveredBackend()
	bs.SetDiscoveredWatch(func() func() { return func() {} })

	pages := pm.CreateRootEx("pages", false)
	before := pages.SubCount()
	for range 3 {
		page := pm.CreateEx(pages, "", nil, false, false)
		if err := bs.Open(page, "discovered:", false); err != nil {
			t.Fatal(err)
		}
		if got := pages.SubCount(); got != before+1 {
			t.Fatalf("open page: %d subscribers on pages, want %d", got, before+1)
		}
		pm.Destroy(page)
		propcore.GlobalDispatchBarrier()
		if got := pages.SubCount(); got != before {
			t.Fatalf("destroyed page: %d subscribers on pages, want %d", got, before)
		}
	}
}
