package smbdisc

import (
	"maps"
	"testing"
)

// The registry adapter is tried on a real service registry wired to real
// settings, as at Movian start, with no LAN.

func TestListedEntryHasTheManagedServiceSettings(t *testing.T) {
	r := newRig(t)
	registryListing{r.ss}.add("smb/OFFICE-PC", "OFFICE-PC", "smb://127.0.0.2")
	r.waitListing(entry{"smb/OFFICE-PC", "OFFICE-PC", "smb://127.0.0.2"})

	// The entry is listed a moment before its settings are made.
	want := map[string]string{"Enabled on home screen": "0", "Name": "OFFICE-PC", "Type": "server"}
	if !waitFor(func() bool { return maps.Equal(r.settingsOf("OFFICE-PC"), want) }) {
		t.Fatalf("settings of OFFICE-PC = %v, want %v", r.settingsOf("OFFICE-PC"), want)
	}
}

func TestRemovedEntryLeavesNoSettings(t *testing.T) {
	r := newRig(t)
	l := registryListing{r.ss}
	h := l.add("smb/OFFICE-PC", "OFFICE-PC", "smb://127.0.0.2")
	if !waitFor(func() bool { return r.settingsOf("OFFICE-PC") != nil }) {
		t.Fatal("no settings made for OFFICE-PC")
	}

	l.remove(h)
	r.waitListing()
	if !waitFor(func() bool { return r.settingsOf("OFFICE-PC") == nil }) {
		t.Fatalf("settings of OFFICE-PC are left: %v", r.settingsOf("OFFICE-PC"))
	}
}

func TestEntryURLChangeKeepsTheSameService(t *testing.T) {
	r := newRig(t)
	l := registryListing{r.ss}
	h := l.add("smb/OFFICE-PC", "OFFICE-PC", "smb://127.0.0.2")

	l.setURL(h, "smb://127.0.0.3")
	r.waitListing(entry{"smb/OFFICE-PC", "OFFICE-PC", "smb://127.0.0.3"})
	if r.ss.GetServiceByID("smb/OFFICE-PC") != h {
		t.Fatal("the entry was created anew, want its URL updated in place")
	}
}
