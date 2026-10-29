package smbdisc

import (
	"testing"
)

func TestNetBIOSNameIsHowNamesMerge(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"OFFICE-PC", "OFFICE-PC"},
		{"office-pc", "OFFICE-PC"},
		{"nas.local", "NAS"},
		{"nas.local.", "NAS"},
		{"nas.example.com", "NAS"},
		{"FIFTEEN-CHARS-X", "FIFTEEN-CHARS-X"},
		{"MEDIASERVER-LIVINGROOM", "MEDIASERVER-LIV"},
		{"mediaserver-livingroom.local", "MEDIASERVER-LIV"},
		{"büro-rechner-zwei", "BÜRO-RECHNER-ZW"}, // characters, not bytes
		{"", ""},
	} {
		if got := netbiosName(tc.in); got != tc.want {
			t.Errorf("netbiosName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The NetBIOS manager reports into the engine through NetBIOSFound and
// NetBIOSLost; these tests make those reports themselves and run the
// WS-Discovery side on the fake LAN and clock.

func TestNetBIOSReportAfterStopListsNothing(t *testing.T) {
	r := startEngine(t, startTestLAN(t))
	r.engine.Stop()
	r.engine.NetBIOSFound("FILESERVER", "WORKGROUP")
	r.expectListingStays()
}
