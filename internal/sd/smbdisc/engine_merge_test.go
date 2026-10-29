package smbdisc

import (
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// The A record at the address the SRV record came from can arrive in a
// later datagram than one at another of the host's addresses.
func TestMDNSPreferredARecordInALaterDatagramRelistsTheHost(t *testing.T) {
	lan := startTestLAN(t)
	nas := lan.mdns.addResponder("127.0.0.2", "storage")
	const instance, target = "storage._smb._tcp.local.", "storage.local."
	nas.set(func(r *fakeResponder) {
		r.replies = func(id uint16) [][]byte {
			return [][]byte{
				mdnsPacket(id, []dnsmessage.Resource{ptrRR(smbService, instance),
					srvRR(instance, target, 445), aRR(target, "127.0.0.9")}, nil),
				mdnsPacket(id, []dnsmessage.Resource{aRR(target, "127.0.0.2")}, nil),
			}
		}
	})
	r := startEngine(t, lan)
	want := entry{"smb/STORAGE", "storage", "smb://127.0.0.2"}
	r.waitListing(want)
	r.expectListingStays(want)
}

// WS-Discovery's address for a host is the host's own answer from there:
// a preferred mDNS address that comes later does not move the entry or
// list a second one, round after round.
func TestMDNSPreferredARecordDoesNotMoveAHostWSDiscoverySawAtItsAddress(t *testing.T) {
	lan := startTestLAN(t)
	lan.AddHost("127.0.0.9", "STORAGE-NAS/Workgroup:WORKGROUP")
	nas := lan.mdns.addResponder("127.0.0.2", "storage")
	const instance, target = "storage._smb._tcp.local.", "storage.local."
	nas.set(func(r *fakeResponder) {
		r.silent = true
		r.replies = func(id uint16) [][]byte {
			return [][]byte{
				mdnsPacket(id, []dnsmessage.Resource{ptrRR(smbService, instance),
					srvRR(instance, target, 445), aRR(target, "127.0.0.9")}, nil),
				mdnsPacket(id, []dnsmessage.Resource{aRR(target, "127.0.0.2")}, nil),
			}
		}
	})
	r := startEngine(t, lan)
	want := entry{"smb/STORAGE-NAS", "STORAGE-NAS", "smb://127.0.0.9"}
	r.waitListing(want)
	nas.set(func(r *fakeResponder) { r.silent = false })
	for round := 2; round <= 5; round++ {
		r.clock.Advance(roundInterval)
		lan.WaitProbes(round)
		r.expectListingStays(want)
	}
}
