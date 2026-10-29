package smbdisc

import (
	"net/netip"
	"testing"
	"time"
)

// The host table is driven through its four operations, with the time
// passed in, and read back through the listing it changes. No test here
// has a socket, a clock or a sleep.

var t0 = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

func newTestTable(t *testing.T) (*hostTable, *memListing) {
	t.Helper()
	l := newMemListing(t)
	return newHostTable(l, nil), l
}

func ip(s string) netip.Addr { return netip.MustParseAddr(s) }

// wsdSeen and mdns are the sightings the two sources make of a host. An empty
// name is a host WS-Discovery knows by address only.
func wsdSeen(name, addr string) sighting {
	return sighting{from: fromWSD, name: name, addr: ip(addr), port: smbPort}
}

func mdns(name, addr string, port int) sighting {
	return sighting{from: fromMDNS, name: name, addr: ip(addr), port: port}
}

// A computer every source names differently is one entry, whichever
// order the names arrive in.
func TestNetBIOSMatchesAnyNameTheSourcesGaveTheHost(t *testing.T) {
	tab, l := newTestTable(t)
	tab.sight(mdns("storage", "127.0.0.2", smbPort), t0)
	tab.sight(wsdSeen("STORAGE-NAS", "127.0.0.2"), t0)
	final := entry{"smb/STORAGE-NAS", "STORAGE-NAS", "smb://127.0.0.2"}
	l.expect(final)
	h := l.handleOf(final.id)

	tab.netbiosFound("STORAGE", "WORKGROUP")
	l.expect(final)
	l.expectSame(final.id, h)
}

// Another computer taking the address does not leave its predecessor's
// names behind.
func TestNetBIOSIgnoresTheNamesOfAComputerThatLeftTheAddress(t *testing.T) {
	tab, l := newTestTable(t)
	tab.sight(wsdSeen("OLD-PC", "127.0.0.2"), t0)
	tab.sight(wsdSeen("NEW-PC", "127.0.0.2"), t0)
	l.expect(entry{"smb/NEW-PC", "NEW-PC", "smb://127.0.0.2"})

	tab.netbiosFound("OLD-PC", "WORKGROUP")
	l.expect(
		entry{"smb/NEW-PC", "NEW-PC", "smb://127.0.0.2"},
		entry{"smb/OLD-PC", "OLD-PC", "smb://OLD-PC"})
}

// A host NetBIOS lists by name alone gains the address another source
// found it at, and the port that source read off the server.
func TestNameOnlyEntryKeepsThePortOfTheEntryItFoldsIn(t *testing.T) {
	tab, l := newTestTable(t)
	tab.netbiosFound("FILESERVER", "WORKGROUP")
	tab.sight(mdns("nas", "127.0.0.2", 4450), t0)
	l.expect(
		entry{"smb/FILESERVER", "FILESERVER", "smb://FILESERVER"},
		entry{"smb/NAS", "nas", "smb://127.0.0.2:4450"})
	nameOnly := l.handleOf("smb/FILESERVER")

	tab.sight(wsdSeen("FILESERVER", "127.0.0.2"), t0)
	want := entry{"smb/FILESERVER", "FILESERVER", "smb://127.0.0.2:4450"}
	l.expect(want)
	l.expectSame(want.id, nameOnly)

	// The name mDNS gave it still finds it.
	tab.netbiosFound("NAS", "WORKGROUP")
	l.expect(want, entry{"smb/NAS", "NAS", "smb://NAS"})
}

// A computer that moves to the address another computer held does not
// take that computer's port or names.
func TestMovedHostDoesNotInheritFromTheComputerItDisplaced(t *testing.T) {
	tab, l := newTestTable(t)
	tab.sight(mdns("OLD-PC", "127.0.0.2", 4450), t0)
	tab.sight(wsdSeen("NEW-PC", "127.0.0.3"), t0)
	l.expect(
		entry{"smb/NEW-PC", "NEW-PC", "smb://127.0.0.3"},
		entry{"smb/OLD-PC", "OLD-PC", "smb://127.0.0.2:4450"})

	tab.sight(wsdSeen("NEW-PC", "127.0.0.2"), t0)
	l.expect(entry{"smb/NEW-PC", "NEW-PC", "smb://127.0.0.2"})
	tab.netbiosFound("OLD-PC", "WORKGROUP")
	l.expect(
		entry{"smb/NEW-PC", "NEW-PC", "smb://127.0.0.2"},
		entry{"smb/OLD-PC", "OLD-PC", "smb://OLD-PC"})
}

// The same, when NetBIOS lists the name before mDNS gives it to the
// host: the entry NetBIOS made by name alone folds into the host.
func TestNetBIOSNameListedBeforeMDNSGivesItFoldsIntoTheHost(t *testing.T) {
	tab, l := newTestTable(t)
	tab.sight(wsdSeen("STORAGE-NAS", "127.0.0.2"), t0)
	final := entry{"smb/STORAGE-NAS", "STORAGE-NAS", "smb://127.0.0.2"}
	h := l.handleOf(final.id)
	tab.netbiosFound("STORAGE", "WORKGROUP")
	l.expect(entry{"smb/STORAGE", "STORAGE", "smb://STORAGE"}, final)

	tab.sight(mdns("storage", "127.0.0.2", smbPort), t0)
	l.expect(final)
	l.expectSame(final.id, h)

	// NetBIOS stops listing it: the host stays, by address.
	tab.netbiosLost("STORAGE", "WORKGROUP", t0)
	l.expect(final)
	l.expectSame(final.id, h)
}

func TestAddressChangeKeepsTheEntryAndMovesItsURL(t *testing.T) {
	t.Run("same endpoint", func(t *testing.T) {
		tab, l := newTestTable(t)
		tab.sight(wsdSeen("OFFICE-PC", "127.0.0.2"), t0)
		l.expect(entry{"smb/OFFICE-PC", "OFFICE-PC", "smb://127.0.0.2"})
		h := l.handleOf("smb/OFFICE-PC")

		// DHCP hands the PC a new address.
		tab.sight(wsdSeen("OFFICE-PC", "127.0.0.3"), t0.Add(roundInterval))
		l.expect(entry{"smb/OFFICE-PC", "OFFICE-PC", "smb://127.0.0.3"})
		l.expectSame("smb/OFFICE-PC", h)
	})

	t.Run("new endpoint listed by address first", func(t *testing.T) {
		tab, l := newTestTable(t)
		tab.sight(wsdSeen("OFFICE-PC", "127.0.0.2"), t0)
		h := l.handleOf("smb/OFFICE-PC")

		tab.sight(wsdSeen("", "127.0.0.3"), t0.Add(roundInterval))
		l.expect(
			entry{"smb/127.0.0.3", "127.0.0.3", "smb://127.0.0.3"},
			entry{"smb/OFFICE-PC", "OFFICE-PC", "smb://127.0.0.2"})

		tab.sight(wsdSeen("OFFICE-PC", "127.0.0.3"), t0.Add(2*roundInterval))
		l.expect(entry{"smb/OFFICE-PC", "OFFICE-PC", "smb://127.0.0.3"})
		l.expectSame("smb/OFFICE-PC", h)
	})
}

func TestHostNetBIOSListsStaysByNameOnceWSDiscoveryLosesIt(t *testing.T) {
	tab, l := newTestTable(t)
	tab.sight(wsdSeen("OFFICE-PC", "127.0.0.2"), t0)
	tab.netbiosFound("OFFICE-PC", "WORKGROUP")
	addressed := entry{"smb/OFFICE-PC", "OFFICE-PC", "smb://127.0.0.2"}
	l.expect(addressed)
	h := l.handleOf(addressed.id)

	tab.age(t0.Add(hostTTL - time.Second))
	l.expect(addressed)
	tab.age(t0.Add(hostTTL))
	l.expect(entry{"smb/OFFICE-PC", "OFFICE-PC", "smb://OFFICE-PC"})
	l.expectSame(addressed.id, h)

	tab.sight(wsdSeen("OFFICE-PC", "127.0.0.2"), t0.Add(hostTTL+roundInterval))
	l.expect(addressed)
	l.expectSame(addressed.id, h)
}

func TestHostNetBIOSListsStaysByNameWhenAnotherComputerTakesItsAddress(t *testing.T) {
	tab, l := newTestTable(t)
	tab.sight(wsdSeen("OFFICE-PC", "127.0.0.2"), t0)
	tab.sight(wsdSeen("OLD-PC", "127.0.0.3"), t0)
	tab.netbiosFound("OLD-PC", "WORKGROUP")
	l.expect(
		entry{"smb/OFFICE-PC", "OFFICE-PC", "smb://127.0.0.2"},
		entry{"smb/OLD-PC", "OLD-PC", "smb://127.0.0.3"})

	// OLD-PC is switched off, and DHCP hands its address to OFFICE-PC.
	tab.sight(wsdSeen("OFFICE-PC", "127.0.0.3"), t0.Add(roundInterval))
	l.expect(
		entry{"smb/OFFICE-PC", "OFFICE-PC", "smb://127.0.0.3"},
		entry{"smb/OLD-PC", "OLD-PC", "smb://OLD-PC"})

	tab.netbiosLost("OLD-PC", "WORKGROUP", t0.Add(roundInterval))
	l.expect(entry{"smb/OFFICE-PC", "OFFICE-PC", "smb://127.0.0.3"})
}

func TestHostNetBIOSListsStaysByNameWhenAnUnseenComputerTakesItsAddress(t *testing.T) {
	tab, l := newTestTable(t)
	tab.sight(wsdSeen("OLD-PC", "127.0.0.3"), t0)
	tab.netbiosFound("OLD-PC", "WORKGROUP")
	l.expect(entry{"smb/OLD-PC", "OLD-PC", "smb://127.0.0.3"})

	// OLD-PC is switched off, and DHCP hands its address to NEW-PC, which
	// no source has listed before.
	tab.sight(wsdSeen("NEW-PC", "127.0.0.3"), t0.Add(roundInterval))
	want := []entry{
		{"smb/NEW-PC", "NEW-PC", "smb://127.0.0.3"},
		{"smb/OLD-PC", "OLD-PC", "smb://OLD-PC"},
	}
	l.expect(want...)
	h := l.handleOf("smb/NEW-PC")

	// NetBIOS lists NEW-PC too: it is the host at that address, not a
	// second entry.
	tab.netbiosFound("NEW-PC", "WORKGROUP")
	l.expect(want...)
	l.expectSame("smb/NEW-PC", h)

	tab.netbiosLost("OLD-PC", "WORKGROUP", t0.Add(roundInterval))
	l.expect(want[0])
}

func TestHostNetBIOSListsForgetsItsMDNSPortWhenItsAddressExpires(t *testing.T) {
	tab, l := newTestTable(t)
	tab.netbiosFound("STORAGE", "WORKGROUP")
	tab.sight(mdns("storage", "127.0.0.2", 4450), t0)
	tab.sight(wsdSeen("STORAGE", "127.0.0.2"), t0)
	l.expect(entry{"smb/STORAGE", "STORAGE", "smb://127.0.0.2:4450"})

	// Both sources go quiet; NetBIOS still lists the host, by name.
	tab.age(t0.Add(hostTTL))
	l.expect(entry{"smb/STORAGE", "STORAGE", "smb://STORAGE"})

	// WS-Discovery alone sees it again, on the default port.
	tab.sight(wsdSeen("STORAGE", "127.0.0.2"), t0.Add(hostTTL+roundInterval))
	l.expect(entry{"smb/STORAGE", "STORAGE", "smb://127.0.0.2"})
}

func TestUnseenComputerTakingAnAddressDoesNotInheritItsMDNSPort(t *testing.T) {
	tab, l := newTestTable(t)
	tab.sight(wsdSeen("OLD-PC", "127.0.0.3"), t0)
	tab.sight(mdns("old-pc", "127.0.0.3", 4450), t0)
	l.expect(entry{"smb/OLD-PC", "OLD-PC", "smb://127.0.0.3:4450"})

	// OLD-PC is switched off, and DHCP hands its address to NEW-PC, which
	// answers WS-Discovery on the default port.
	tab.sight(wsdSeen("NEW-PC", "127.0.0.3"), t0.Add(roundInterval))
	l.expect(entry{"smb/NEW-PC", "NEW-PC", "smb://127.0.0.3"})
}

func TestNetBIOSNameAndHostListedByAddressMergeOnceTheGetNamesIt(t *testing.T) {
	tab, l := newTestTable(t)
	tab.sight(wsdSeen("", "127.0.0.2"), t0)
	tab.netbiosFound("OFFICE-PC", "WORKGROUP")
	l.expect(
		entry{"smb/127.0.0.2", "127.0.0.2", "smb://127.0.0.2"},
		entry{"smb/OFFICE-PC", "OFFICE-PC", "smb://OFFICE-PC"})
	h := l.handleOf("smb/OFFICE-PC")

	tab.sight(wsdSeen("OFFICE-PC", "127.0.0.2"), t0.Add(roundInterval))
	l.expect(entry{"smb/OFFICE-PC", "OFFICE-PC", "smb://127.0.0.2"})
	l.expectSame("smb/OFFICE-PC", h)
}

// A host's names rank WS-Discovery's pub:Computer, then the NetBIOS
// name, then the mDNS name, then the address.
func TestHostSeenByAllThreeSourcesTakesTheMostPreferredName(t *testing.T) {
	tab, l := newTestTable(t)

	// mDNS names the host first.
	tab.sight(mdns("storage", "127.0.0.2", smbPort), t0)
	l.expect(entry{"smb/STORAGE", "storage", "smb://127.0.0.2"})

	// NetBIOS lists the same name, and prefers its own spelling.
	tab.netbiosFound("STORAGE", "WORKGROUP")
	l.expect(entry{"smb/STORAGE", "STORAGE", "smb://127.0.0.2"})

	// The Get names it last, and WS-Discovery wins.
	tab.sight(wsdSeen("STORAGE-NAS", "127.0.0.2"), t0)
	final := entry{"smb/STORAGE-NAS", "STORAGE-NAS", "smb://127.0.0.2"}
	l.expect(final)
	h := l.handleOf(final.id)

	// All three report again; the name stays.
	later := t0.Add(roundInterval)
	tab.sight(mdns("storage", "127.0.0.2", smbPort), later)
	tab.sight(wsdSeen("STORAGE-NAS", "127.0.0.2"), later)
	tab.netbiosFound("STORAGE", "WORKGROUP")
	l.expect(final)
	l.expectSame(final.id, h)
}

func TestHostSeenByWSDAndMDNSIsListedOnceUnderItsWSDName(t *testing.T) {
	for _, tc := range []struct {
		name     string
		computer string // the WS-Discovery name of the host
		port     int    // the SRV port
		mdnsWins bool   // mDNS lists the host before WS-Discovery names it
		want     entry
	}{
		{"the same name in another case, mDNS first", "STORAGE", smbPort, true,
			entry{"smb/STORAGE", "STORAGE", "smb://127.0.0.2"}},
		{"another name, mDNS first", "NAS-BOX", smbPort, true,
			entry{"smb/NAS-BOX", "NAS-BOX", "smb://127.0.0.2"}},
		{"another name, WS-Discovery first", "NAS-BOX", smbPort, false,
			entry{"smb/NAS-BOX", "NAS-BOX", "smb://127.0.0.2"}},
		// WS-Discovery tells no port, so the SRV port stays.
		{"SRV port other than 445, mDNS first", "STORAGE", 4450, true,
			entry{"smb/STORAGE", "STORAGE", "smb://127.0.0.2:4450"}},
		{"SRV port other than 445, WS-Discovery first", "STORAGE", 4450, false,
			entry{"smb/STORAGE", "STORAGE", "smb://127.0.0.2:4450"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tab, l := newTestTable(t)
			viaMDNS := mdns("storage", "127.0.0.2", tc.port)
			viaWSD := wsdSeen(tc.computer, "127.0.0.2")
			if tc.mdnsWins {
				tab.sight(viaMDNS, t0)
				tab.sight(viaWSD, t0)
			} else {
				tab.sight(viaWSD, t0)
				tab.sight(viaMDNS, t0)
			}
			l.expect(tc.want)
			h := l.handleOf(tc.want.id)

			// Both answer the next round too.
			later := t0.Add(roundInterval)
			tab.sight(viaMDNS, later)
			tab.sight(viaWSD, later)
			l.expect(tc.want)
			l.expectSame(tc.want.id, h)
		})
	}
}

func TestMDNSNameOfAnotherHostLeavesAWSDNamedHostAlone(t *testing.T) {
	tab, l := newTestTable(t)
	tab.sight(wsdSeen("NAS-BOX", "127.0.0.2"), t0)
	tab.sight(mdns("storage", "127.0.0.3", smbPort), t0)
	want := []entry{
		{"smb/NAS-BOX", "NAS-BOX", "smb://127.0.0.2"},
		{"smb/STORAGE", "storage", "smb://127.0.0.3"},
	}
	l.expect(want...)
	h := l.handleOf("smb/NAS-BOX")

	// The WS-Discovery host answers mDNS with a host name the other NAS
	// has.
	tab.sight(mdns("storage", "127.0.0.2", smbPort), t0.Add(roundInterval))
	l.expect(want...)
	l.expectSame("smb/NAS-BOX", h)
}

func TestUnusableNetBIOSNameListsNothing(t *testing.T) {
	tab, l := newTestTable(t)
	for _, name := range []string{"", "BAD/NAME", "TWO WORDS", "\x01\x02", ".LOCAL"} {
		tab.netbiosFound(name, "WORKGROUP")
	}
	l.expect()
}

func TestNetBIOSNameListedInTwoWorkgroupsStaysUntilBothLoseIt(t *testing.T) {
	tab, l := newTestTable(t)
	tab.netbiosFound("FILESERVER", "WORKGROUP")
	tab.netbiosFound("FILESERVER", "OFFICE")
	want := entry{"smb/FILESERVER", "FILESERVER", "smb://FILESERVER"}
	l.expect(want)

	tab.netbiosLost("FILESERVER", "WORKGROUP", t0)
	l.expect(want)
	tab.netbiosLost("FILESERVER", "OFFICE", t0)
	l.expect()
}

func TestHostSilentForTheHostTTLIsRemovedAndComesBack(t *testing.T) {
	tab, l := newTestTable(t)
	want := entry{"smb/OFFICE-PC", "OFFICE-PC", "smb://127.0.0.2"}
	tab.sight(wsdSeen("OFFICE-PC", "127.0.0.2"), t0)
	l.expect(want)

	tab.age(t0.Add(hostTTL - time.Second))
	l.expect(want)
	tab.age(t0.Add(hostTTL))
	l.expect()

	tab.sight(wsdSeen("OFFICE-PC", "127.0.0.2"), t0.Add(hostTTL+roundInterval))
	l.expect(want)
}

// WS-Discovery's address for a host is the host's own answer from there:
// a preferred mDNS address that comes later does not move the entry or
// list a second one, round after round.
func TestMDNSRelistLeavesAHostWSDiscoveryNamedAtItsAddress(t *testing.T) {
	tab, l := newTestTable(t)
	want := entry{"smb/STORAGE-NAS", "STORAGE-NAS", "smb://127.0.0.9"}
	tab.sight(wsdSeen("STORAGE-NAS", "127.0.0.9"), t0)
	l.expect(want)
	h := l.handleOf(want.id)

	// The SRV target has two A records: the one WS-Discovery saw, and one
	// that arrives in a later datagram.
	for round := 1; round <= 4; round++ {
		now := t0.Add(time.Duration(round) * roundInterval)
		tab.sight(wsdSeen("STORAGE-NAS", "127.0.0.9"), now)
		tab.sight(mdns("storage", "127.0.0.9", smbPort), now)
		again := mdns("storage", "127.0.0.2", smbPort)
		again.again = ip("127.0.0.9")
		tab.sight(again, now)
		l.expect(want)
		l.expectSame(want.id, h)
	}
}
