package smbdisc

import (
	"fmt"
	"net/netip"
	"os"
	"reflect"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// The mDNS response parser, over the sanitized TrueNAS captures and
// malformed variants of them.

// captureID is the ID of the query the TrueNAS captures answer.
const captureID = 0xd797

func readBinCapture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Records of the sanitized TrueNAS host, for building variants of its
// answer.
var (
	storagePTR = ptrRR(smbService, "storage._smb._tcp.local.")
	storageSRV = srvRR("storage._smb._tcp.local.", "storage.local.", 445)
	storageTXT = txtRR("storage._smb._tcp.local.", "")
	storageA   = aRR("storage.local.", "192.0.2.20")
	// storageRecords is what the parser reads from any of them.
	storageRecords = mdnsResponse{
		id:   captureID,
		ptrs: []string{"storage._smb._tcp.local."},
		srvs: []mdnsSRV{{instance: "storage._smb._tcp.local.", target: "storage.local.", port: 445}},
		txts: []mdnsTXT{{instance: "storage._smb._tcp.local.", txt: []string{""}}},
		as:   []mdnsA{{host: "storage.local.", addr: netip.MustParseAddr("192.0.2.20")}},
	}
)

func rrHeader(name string, typ dnsmessage.Type) dnsmessage.ResourceHeader {
	return dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(name), Type: typ, Class: dnsmessage.ClassINET, TTL: 10}
}

func ptrRR(name, ptr string) dnsmessage.Resource {
	return dnsmessage.Resource{Header: rrHeader(name, dnsmessage.TypePTR),
		Body: &dnsmessage.PTRResource{PTR: dnsmessage.MustNewName(ptr)}}
}

func srvRR(name, target string, port uint16) dnsmessage.Resource {
	return dnsmessage.Resource{Header: rrHeader(name, dnsmessage.TypeSRV),
		Body: &dnsmessage.SRVResource{Target: dnsmessage.MustNewName(target), Port: port}}
}

func txtRR(name string, txt ...string) dnsmessage.Resource {
	return dnsmessage.Resource{Header: rrHeader(name, dnsmessage.TypeTXT), Body: &dnsmessage.TXTResource{TXT: txt}}
}

func aRR(name, addr string) dnsmessage.Resource {
	return dnsmessage.Resource{Header: rrHeader(name, dnsmessage.TypeA),
		Body: &dnsmessage.AResource{A: netip.MustParseAddr(addr).As4()}}
}

// mdnsPacket packs an mDNS response with ID id. Packing valid records
// cannot fail.
func mdnsPacket(id uint16, answers, additionals []dnsmessage.Resource) []byte {
	m := dnsmessage.Message{
		Header:      dnsmessage.Header{ID: id, Response: true, Authoritative: true},
		Answers:     answers,
		Additionals: additionals,
	}
	b, err := m.Pack()
	if err != nil {
		panic(err)
	}
	return b
}

func TestParseMDNSResponse(t *testing.T) {
	truenas := readBinCapture(t, "truenas-mdns-smb.bin")

	t.Run("TrueNAS capture, every record in the answer section", func(t *testing.T) {
		got, err := parseMDNSResponse(truenas)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, storageRecords) {
			t.Fatalf("got  %+v\nwant %+v", got, storageRecords)
		}
	})

	t.Run("TrueNAS capture of another service type", func(t *testing.T) {
		got, err := parseMDNSResponse(readBinCapture(t, "truenas-mdns-device-info.bin"))
		if err != nil {
			t.Fatal(err)
		}
		want := mdnsResponse{id: 0xe297, as: storageRecords.as}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got  %+v\nwant %+v", got, want)
		}
	})

	withClass := func(rr dnsmessage.Resource, class dnsmessage.Class) dnsmessage.Resource {
		rr.Header.Class = class
		return rr
	}
	onlyA := func(as ...mdnsA) mdnsResponse { return mdnsResponse{id: captureID, as: as} }
	for _, tc := range []struct {
		name        string
		answers     []dnsmessage.Resource
		additionals []dnsmessage.Resource
		want        mdnsResponse
	}{
		{
			name:        "records in the additional section",
			answers:     []dnsmessage.Resource{storagePTR},
			additionals: []dnsmessage.Resource{storageSRV, storageTXT, storageA},
			want:        storageRecords,
		},
		{
			name: "service type in another case",
			answers: []dnsmessage.Resource{
				ptrRR("_SMB._TCP.LOCAL.", "Storage._SMB._tcp.local."),
				srvRR("Storage._SMB._tcp.local.", "Storage.LOCAL.", 445),
			},
			want: mdnsResponse{
				id:   captureID,
				ptrs: []string{"Storage._SMB._tcp.local."},
				srvs: []mdnsSRV{{instance: "Storage._SMB._tcp.local.", target: "Storage.LOCAL.", port: 445}},
			},
		},
		{
			name: "cache-flush bit set",
			answers: []dnsmessage.Resource{withClass(storageSRV, dnsmessage.ClassINET|0x8000),
				withClass(storageA, dnsmessage.ClassINET|0x8000)},
			want: mdnsResponse{id: captureID, srvs: storageRecords.srvs, as: storageRecords.as},
		},
		{
			name:    "records of another class",
			answers: []dnsmessage.Resource{withClass(storagePTR, dnsmessage.ClassCHAOS), withClass(storageSRV, dnsmessage.ClassCHAOS), withClass(storageA, dnsmessage.ClassANY)},
			want:    mdnsResponse{id: captureID},
		},
		{
			name: "records of other services",
			answers: []dnsmessage.Resource{
				ptrRR("_http._tcp.local.", "web._http._tcp.local."),
				ptrRR(smbService, "web._http._tcp.local."),
				srvRR("web._http._tcp.local.", "web.local.", 80),
				txtRR("web._http._tcp.local.", "path=/"),
			},
			want: mdnsResponse{id: captureID},
		},
		{
			name:    "SRV with port 0",
			answers: []dnsmessage.Resource{srvRR("storage._smb._tcp.local.", "storage.local.", 0)},
			want:    mdnsResponse{id: captureID},
		},
		{
			name: "A records no server can have",
			answers: []dnsmessage.Resource{aRR("a.local.", "0.0.0.0"), aRR("b.local.", "224.0.0.251"),
				aRR("c.local.", "255.255.255.255"), aRR("d.local.", "169.254.7.8"), aRR("e.local.", "127.0.0.2")},
			want: onlyA(mdnsA{"d.local.", netip.MustParseAddr("169.254.7.8")}, mdnsA{"e.local.", netip.MustParseAddr("127.0.0.2")}),
		},
		{
			name: "AAAA and NSEC records",
			answers: []dnsmessage.Resource{
				{Header: rrHeader("storage.local.", dnsmessage.TypeAAAA),
					Body: &dnsmessage.AAAAResource{AAAA: netip.MustParseAddr("2001:db8::20").As16()}},
				{Header: rrHeader("storage.local.", 47), Body: &dnsmessage.UnknownResource{Type: 47, Data: []byte{0xc0, 0x0c, 0, 1, 0x40}}},
				storageA,
			},
			want: onlyA(storageRecords.as...),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseMDNSResponse(mdnsPacket(captureID, tc.answers, tc.additionals))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got  %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

func TestParseMDNSResponseRejects(t *testing.T) {
	truenas := readBinCapture(t, "truenas-mdns-smb.bin")
	patched := func(off int, b ...byte) []byte {
		out := append([]byte(nil), truenas...)
		copy(out[off:], b)
		return out
	}
	packet := func(h dnsmessage.Header) []byte {
		m := dnsmessage.Message{Header: h, Answers: []dnsmessage.Resource{storagePTR, storageSRV, storageA}}
		b, err := m.Pack()
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	cases := []struct {
		name string
		b    []byte
	}{
		{"a query", packet(dnsmessage.Header{ID: captureID})},
		{"opcode other than 0", packet(dnsmessage.Header{ID: captureID, Response: true, OpCode: 2})},
		{"response code other than 0", packet(dnsmessage.Header{ID: captureID, Response: true, RCode: dnsmessage.RCodeServerFailure})},
		{"empty", nil},
		{"garbage", []byte("\x00\xff garbage that is no DNS message")},
		{"more answers than it holds", patched(7, 0x40)},
		// The PTR record's owner, a pointer at 0x21, points at itself.
		{"compression loop", patched(0x21, 0xc0, 0x21)},
		{"compression pointer past the end", patched(0x21, 0xc0, 0xff)},
		{"reserved label type", patched(0x21, 0x80)},
		// The PTR names "s.orage", one label with a dot in it.
		{"dot inside a label", patched(0x2f, '.')},
	}
	for n := 12; n < len(truenas); n++ {
		cases = append(cases, struct {
			name string
			b    []byte
		}{fmt.Sprintf("capture cut to %d bytes", n), truenas[:n]})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := parseMDNSResponse(tc.b); err == nil {
				t.Fatalf("parsed as %+v, want an error", got)
			}
		})
	}
}

func TestMDNSHostName(t *testing.T) {
	for _, tc := range []struct{ target, want string }{
		{"storage.local.", "storage"},
		{"Storage.LOCAL.", "Storage"},
		{"nas-1.local.", "nas-1"},
		{"storage.", "storage"},
		{"nas.home.arpa.", "nas"},
		{".", ""},
		{"my nas.local.", ""},   // not a NetBIOS name
		{"../x.local.", ""},     // first label empty
		{"\xff\xfe.local.", ""}, // not UTF-8
		{"a-very-long-host-name-that-is-longer-than-sixty-three-characters-x.local.", ""},
	} {
		if got := mdnsHostName(tc.target); got != tc.want {
			t.Errorf("mdnsHostName(%q) = %q, want %q", tc.target, got, tc.want)
		}
	}
}
