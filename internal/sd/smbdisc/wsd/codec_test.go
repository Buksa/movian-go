package wsd

import (
	"encoding/xml"
	"slices"
	"strings"
	"testing"

	"github.com/czz/movian-go/internal/sd/smbdisc/wsd/wsdtest"
)

// The WS-Discovery wire-format parsers, over the sanitized captures and
// edge-case variants of them.

const (
	devprofNamespace = "http://schemas.xmlsoap.org/ws/2006/02/devprof"
	windowsXAddr     = "http://192.0.2.10:5357/da5d9f02-f616-4bb9-a3df-6373ec52ec2b/"
)

func TestParseWSDMessage(t *testing.T) {
	windows := wsdtest.WindowsProbeMatch
	matchesOf := func(t *testing.T, b string) []probeMatch {
		t.Helper()
		msg, err := parseMessage([]byte(b))
		if err != nil {
			t.Fatalf("parseMessage: %v", err)
		}
		return msg.probeMatches
	}

	t.Run("Windows capture", func(t *testing.T) {
		msg, err := parseMessage([]byte(windows))
		if err != nil {
			t.Fatal(err)
		}
		if msg.action != probeMatchesAction {
			t.Errorf("action = %q", msg.action)
		}
		if msg.relatesTo != wsdtest.CaptureRelatesTo {
			t.Errorf("relatesTo = %q, want %q", msg.relatesTo, wsdtest.CaptureRelatesTo)
		}
		if len(msg.probeMatches) != 1 {
			t.Fatalf("%d ProbeMatches, want 1", len(msg.probeMatches))
		}
		m := msg.probeMatches[0]
		if m.epr != wsdtest.CaptureEPR {
			t.Errorf("epr = %q", m.epr)
		}
		wantTypes := []xml.Name{{Space: devprofNamespace, Local: "Device"}, {Space: pubNamespace, Local: "Computer"}}
		if !slices.Equal(m.types, wantTypes) {
			t.Errorf("types = %v, want %v", m.types, wantTypes)
		}
		if !slices.Equal(m.xaddrs, []string{windowsXAddr}) {
			t.Errorf("xaddrs = %v", m.xaddrs)
		}
		if m.metadataVersion != "168" {
			t.Errorf("metadataVersion = %q, want 168", m.metadataVersion)
		}
		if !m.isComputer() {
			t.Error("a Windows PC is not a pub:Computer")
		}
	})

	t.Run("no XAddrs, as wsdd sends", func(t *testing.T) {
		b := strings.Replace(windows, "<wsd:XAddrs>"+windowsXAddr+"</wsd:XAddrs>", "", 1)
		m := matchesOf(t, b)
		if len(m) != 1 || len(m[0].xaddrs) != 0 || !m[0].isComputer() {
			t.Fatalf("got %+v, want one pub:Computer match without XAddrs", m)
		}
	})

	t.Run("wsdd capture", func(t *testing.T) {
		msg, err := parseMessage([]byte(wsdtest.WSDDProbeMatch))
		if err != nil {
			t.Fatal(err)
		}
		if msg.action != probeMatchesAction || msg.relatesTo != wsdtest.WSDDProbeRelatesTo {
			t.Errorf("action %q, relatesTo %q", msg.action, msg.relatesTo)
		}
		m := msg.probeMatches
		if len(m) != 1 || m[0].epr != wsdtest.WSDDEPR || len(m[0].xaddrs) != 0 || m[0].metadataVersion != "1" || !m[0].isComputer() {
			t.Fatalf("got %+v, want one pub:Computer match %s without XAddrs, MetadataVersion 1", m, wsdtest.WSDDEPR)
		}
	})

	t.Run("several XAddrs", func(t *testing.T) {
		b := strings.Replace(windows, windowsXAddr, "http://[2001:db8::10]:5357/x/ "+windowsXAddr, 1)
		m := matchesOf(t, b)
		if len(m) != 1 || !slices.Equal(m[0].xaddrs, []string{"http://[2001:db8::10]:5357/x/", windowsXAddr}) {
			t.Fatalf("got %+v", m)
		}
	})

	for _, tc := range []struct {
		name, types string
		xmlns       string // extra declaration on the Types element
		computer    bool
	}{
		{name: "printer", types: "wsdp:Device wprt:PrintDeviceType", computer: false},
		{name: "other prefix for pub", types: "wsdp:Device p:Computer",
			xmlns: ` xmlns:p="` + pubNamespace + `"`, computer: true},
		{name: "pub prefix rebound", types: "wsdp:Device pub:Computer",
			xmlns: ` xmlns:pub="urn:example:other"`, computer: false},
		{name: "undeclared prefix", types: "wsdp:Device x:Computer", computer: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := strings.Replace(windows, "<wsd:Types>wsdp:Device pub:Computer</wsd:Types>",
				"<wsd:Types"+tc.xmlns+">"+tc.types+"</wsd:Types>", 1)
			m := matchesOf(t, b)
			if len(m) != 1 || m[0].isComputer() != tc.computer {
				t.Fatalf("got %+v, want isComputer %v", m, tc.computer)
			}
		})
	}

	for _, tc := range []struct{ name, data string }{
		{"truncated", windows[:len(windows)/2]},
		{"mismatched tags", strings.Replace(windows, "</wsd:ProbeMatch>", "</wsd:ProbeMatchX>", 1)},
		{"garbage", "\x00\x01\x02 not xml at all <<<"},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg, err := parseMessage([]byte(tc.data))
			if err == nil && len(msg.probeMatches) != 0 {
				t.Fatalf("parsed %+v from a broken message", msg)
			}
		})
	}

	// No WS-Discovery message nests deeper than a dozen elements. A deep
	// one is refused rather than walked, which would cost a
	// datagram-sized document about a second of CPU.
	t.Run("nested too deep", func(t *testing.T) {
		deep := strings.Repeat("<a>", 8000) + strings.Repeat("</a>", 8000)
		if _, err := parseMessage([]byte(deep)); err == nil {
			t.Fatal("a document 8000 elements deep parsed without error")
		}
		if _, err := parseGetResponse([]byte(deep)); err == nil {
			t.Fatal("a GetResponse 8000 elements deep parsed without error")
		}
	})
}

func TestParseResolveMatch(t *testing.T) {
	wsdd := wsdtest.WSDDResolveMatch
	wantTypes := []xml.Name{{Space: devprofNamespace, Local: "Device"}, {Space: pubNamespace, Local: "Computer"}}

	t.Run("wsdd capture", func(t *testing.T) {
		msg, err := parseMessage([]byte(wsdd))
		if err != nil {
			t.Fatal(err)
		}
		if msg.action != resolveMatchesAction {
			t.Errorf("action = %q", msg.action)
		}
		if msg.relatesTo != wsdtest.WSDDResolveRelatesTo {
			t.Errorf("relatesTo = %q, want %q", msg.relatesTo, wsdtest.WSDDResolveRelatesTo)
		}
		if len(msg.probeMatches) != 0 || len(msg.resolveMatches) != 1 {
			t.Fatalf("%d ProbeMatches and %d ResolveMatches, want 0 and 1",
				len(msg.probeMatches), len(msg.resolveMatches))
		}
		m := msg.resolveMatches[0]
		if m.epr != wsdtest.WSDDEPR {
			t.Errorf("epr = %q", m.epr)
		}
		if !slices.Equal(m.types, wantTypes) {
			t.Errorf("types = %v, want %v", m.types, wantTypes)
		}
		if !slices.Equal(m.xaddrs, []string{wsdtest.WSDDXAddr}) {
			t.Errorf("xaddrs = %v", m.xaddrs)
		}
		if m.metadataVersion != "1" {
			t.Errorf("metadataVersion = %q, want 1", m.metadataVersion)
		}
	})

	match := wsdd[strings.Index(wsdd, "<wsd:ResolveMatch>"):strings.Index(wsdd, "</wsd:ResolveMatches>")]
	for _, tc := range []struct {
		name, data string
		want       []probeMatch // only the EPR and XAddrs are compared
	}{
		{"no match", strings.Replace(wsdd, match, "", 1), nil},
		{"no XAddrs", strings.Replace(wsdd, "<wsd:XAddrs>"+wsdtest.WSDDXAddr+"</wsd:XAddrs>", "", 1),
			[]probeMatch{{epr: wsdtest.WSDDEPR}}},
		{"several XAddrs", strings.Replace(wsdd, wsdtest.WSDDXAddr, "http://[2001:db8::20]:5357/x "+wsdtest.WSDDXAddr, 1),
			[]probeMatch{{epr: wsdtest.WSDDEPR, xaddrs: []string{"http://[2001:db8::20]:5357/x", wsdtest.WSDDXAddr}}}},
		{"two matches", strings.Replace(wsdd, match, match+strings.ReplaceAll(match, wsdtest.WSDDEPR, "urn:uuid:other"), 1),
			[]probeMatch{{epr: wsdtest.WSDDEPR, xaddrs: []string{wsdtest.WSDDXAddr}}, {epr: "urn:uuid:other", xaddrs: []string{wsdtest.WSDDXAddr}}}},
		{"match outside ResolveMatches",
			strings.Replace(strings.Replace(wsdd, "<wsd:ResolveMatches>", "<wsd:Other>", 1),
				"</wsd:ResolveMatches>", "</wsd:Other>", 1), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg, err := parseMessage([]byte(tc.data))
			if err != nil {
				t.Fatal(err)
			}
			var got []probeMatch
			for _, m := range msg.resolveMatches {
				got = append(got, probeMatch{epr: m.epr, xaddrs: m.xaddrs})
			}
			if !slices.EqualFunc(got, tc.want, func(a, b probeMatch) bool {
				return a.epr == b.epr && slices.Equal(a.xaddrs, b.xaddrs)
			}) {
				t.Fatalf("ResolveMatches %+v, want %+v", got, tc.want)
			}
			if len(msg.probeMatches) != 0 {
				t.Fatalf("ProbeMatches %+v in a ResolveMatches reply", msg.probeMatches)
			}
		})
	}

	for _, tc := range []struct{ name, data string }{
		{"truncated", wsdd[:len(wsdd)/2]},
		{"mismatched tags", strings.Replace(wsdd, "</wsd:ResolveMatch>", "</wsd:ResolveMatchX>", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg, err := parseMessage([]byte(tc.data))
			if err == nil && len(msg.resolveMatches) != 0 {
				t.Fatalf("parsed %+v from a broken message", msg)
			}
		})
	}
}

func TestParseGetResponse(t *testing.T) {
	windows := wsdtest.WindowsGetResponse
	for _, tc := range []struct {
		name, data             string
		computer, friendlyName string
	}{
		{"Windows capture", windows,
			"OFFICE-PC/Workgroup:WORKGROUP", "Microsoft Publication Service Device Host"},
		{"wsdd capture", wsdtest.WSDDGetResponse,
			"FILESERVER/Workgroup:WORKGROUP", "WSD Device fileserver"},
		{"no pub:Computer", strings.Replace(windows, wsdtest.CaptureComputer, "", 1),
			"", "Microsoft Publication Service Device Host"},
		{"Computer outside the pub namespace",
			strings.Replace(windows, wsdtest.CaptureComputer, `<x:Computer xmlns:x="urn:example:other">EVIL/NotJoined</x:Computer>`, 1),
			"", "Microsoft Publication Service Device Host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md, err := parseGetResponse([]byte(tc.data))
			if err != nil {
				t.Fatal(err)
			}
			if md.computer != tc.computer || md.friendlyName != tc.friendlyName {
				t.Fatalf("got computer %q, friendlyName %q; want %q, %q",
					md.computer, md.friendlyName, tc.computer, tc.friendlyName)
			}
		})
	}

	if _, err := parseGetResponse([]byte(windows[:len(windows)-20])); err == nil {
		t.Error("a truncated GetResponse parsed without error")
	}
}

func TestSplitComputer(t *testing.T) {
	for _, tc := range []struct{ in, name, membership string }{
		{"OFFICE-PC/Workgroup:WORKGROUP", "OFFICE-PC", "Workgroup:WORKGROUP"},
		{`OFFICE-PC\Workgroup:WORKGROUP`, "OFFICE-PC", "Workgroup:WORKGROUP"},
		{"OFFICE-PC/Domain:CORP.EXAMPLE", "OFFICE-PC", "Domain:CORP.EXAMPLE"},
		{`OFFICE-PC\NotJoined`, "OFFICE-PC", "NotJoined"},
		{"OFFICE-PC/NotJoined", "OFFICE-PC", "NotJoined"},
		{"  OFFICE-PC/Workgroup:WORKGROUP\n", "OFFICE-PC", "Workgroup:WORKGROUP"},
		{`OFFICE-PC/Workgroup:A\B`, "OFFICE-PC", `Workgroup:A\B`},
		{"OFFICE-PC", "OFFICE-PC", ""},
		{"/Workgroup:WORKGROUP", "", "Workgroup:WORKGROUP"},
		{"", "", ""},
	} {
		name, membership := splitComputer(tc.in)
		if name != tc.name || membership != tc.membership {
			t.Errorf("splitComputer(%q) = %q, %q; want %q, %q", tc.in, name, membership, tc.name, tc.membership)
		}
	}
}
