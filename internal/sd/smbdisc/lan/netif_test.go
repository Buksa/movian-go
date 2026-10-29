package lan

import (
	"errors"
	"net"
	"net/netip"
	"slices"
	"testing"
)

func TestProbeInterfacesKeepsEveryLANAddress(t *testing.T) {
	const lan = net.FlagUp | net.FlagRunning | net.FlagBroadcast | net.FlagMulticast
	ipv4 := func(s string) net.Addr {
		p := netip.MustParsePrefix(s)
		return &net.IPNet{IP: p.Addr().AsSlice(), Mask: net.CIDRMask(p.Bits(), 32)}
	}
	ipv6 := func(s string) net.Addr {
		p := netip.MustParsePrefix(s)
		return &net.IPNet{IP: p.Addr().AsSlice(), Mask: net.CIDRMask(p.Bits(), 128)}
	}
	for _, tc := range []struct {
		name  string
		flags net.Flags
		addrs []net.Addr
		err   error
		want  []string // kept addresses
	}{
		{"Ethernet", lan, []net.Addr{ipv4("192.0.2.10/24"), ipv6("2001:db8::10/64")}, nil, []string{"192.0.2.10"}},
		{"two IPv4 addresses", lan, []net.Addr{ipv4("192.0.2.10/24"), ipv4("192.0.2.11/24")}, nil, []string{"192.0.2.10", "192.0.2.11"}},
		{"address without a mask", lan, []net.Addr{&net.IPAddr{IP: net.IPv4(192, 0, 2, 10)}}, nil, []string{"192.0.2.10"}},
		{"loopback", net.FlagUp | net.FlagRunning | net.FlagLoopback | net.FlagMulticast, []net.Addr{ipv4("127.0.0.1/8")}, nil, nil},
		{"down", net.FlagBroadcast | net.FlagMulticast, []net.Addr{ipv4("192.0.2.10/24")}, nil, nil},
		{"no carrier", net.FlagUp | net.FlagBroadcast | net.FlagMulticast, []net.Addr{ipv4("192.0.2.10/24")}, nil, nil},
		{"no multicast", net.FlagUp | net.FlagRunning | net.FlagBroadcast, []net.Addr{ipv4("192.0.2.10/24")}, nil, nil},
		{"VPN tunnel", net.FlagUp | net.FlagRunning | net.FlagPointToPoint | net.FlagMulticast, []net.Addr{ipv4("192.0.2.10/32")}, nil, nil},
		{"IPv6 only", lan, []net.Addr{ipv6("2001:db8::10/64")}, nil, nil},
		{"unspecified address", lan, []net.Addr{ipv4("0.0.0.0/0")}, nil, nil},
		{"addresses unreadable", lan, nil, errors.New("no access"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ifis := []net.Interface{{Index: 7, Name: "eth0", Flags: tc.flags}}
			got := probeInterfaces(ifis, func(ifi *net.Interface) ([]net.Addr, error) {
				if ifi.Index != 7 {
					t.Fatalf("addresses asked of interface %d, want 7", ifi.Index)
				}
				return tc.addrs, tc.err
			})
			var want []Interface
			for _, a := range tc.want {
				want = append(want, Interface{Name: "eth0", Index: 7, Addr: netip.MustParseAddr(a)})
			}
			if !slices.Equal(got, want) {
				t.Fatalf("got %v, want %v", got, want)
			}
		})
	}
}

func TestProbeInterfacesGoesThroughEveryInterface(t *testing.T) {
	const lan = net.FlagUp | net.FlagRunning | net.FlagBroadcast | net.FlagMulticast
	ifis := []net.Interface{
		{Index: 1, Name: "lo", Flags: net.FlagUp | net.FlagRunning | net.FlagLoopback},
		{Index: 2, Name: "eth0", Flags: lan},
		{Index: 3, Name: "docker0", Flags: lan},
		{Index: 4, Name: "tailscale0", Flags: net.FlagUp | net.FlagRunning | net.FlagPointToPoint | net.FlagMulticast},
		{Index: 5, Name: "wlan0", Flags: lan},
	}
	addrs := map[int]string{1: "127.0.0.1", 2: "192.0.2.10", 3: "192.0.2.129", 4: "192.0.2.200", 5: "192.0.2.20"}
	got := probeInterfaces(ifis, func(ifi *net.Interface) ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP(addrs[ifi.Index]), Mask: net.CIDRMask(24, 32)}}, nil
	})
	want := []Interface{
		{Name: "eth0", Index: 2, Addr: netip.MustParseAddr("192.0.2.10")},
		{Name: "docker0", Index: 3, Addr: netip.MustParseAddr("192.0.2.129")},
		{Name: "wlan0", Index: 5, Addr: netip.MustParseAddr("192.0.2.20")},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSourceInterfacesUsesTheKernelsSourceAddress(t *testing.T) {
	cause := errors.New("netlinkrib: permission denied")
	udp := func(s string) net.Addr { return &net.UDPAddr{IP: net.ParseIP(s)} }
	for _, tc := range []struct {
		name string
		by   map[string]net.Addr // destination -> local address
		want []string
	}{
		{"one LAN address for every destination",
			map[string]net.Addr{"224.0.0.251:5353": udp("192.0.2.10"), "239.255.255.250:3702": udp("192.0.2.10"), "192.0.2.1:9": udp("192.0.2.10")},
			[]string{"192.0.2.10"}},
		{"two interfaces",
			map[string]net.Addr{"224.0.0.251:5353": udp("192.0.2.10"), "192.0.2.1:9": udp("198.51.100.7")},
			[]string{"192.0.2.10", "198.51.100.7"}},
		{"loopback and unspecified are skipped",
			map[string]net.Addr{"224.0.0.251:5353": udp("127.0.0.1"), "239.255.255.250:3702": udp("0.0.0.0"), "192.0.2.1:9": udp("2001:db8::1")},
			nil},
		{"not a UDP address", map[string]net.Addr{"224.0.0.251:5353": &net.TCPAddr{IP: net.ParseIP("192.0.2.10")}}, nil},
		{"no route", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sourceInterfaces(cause, func(dst string) (net.Addr, error) {
				if a, ok := tc.by[dst]; ok {
					return a, nil
				}
				return nil, errors.New("network is unreachable")
			})
			if tc.want == nil {
				if err == nil || !errors.Is(err, cause) {
					t.Fatalf("got %v, %v; want an error wrapping the netlink error", got, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var want []Interface
			for _, a := range tc.want {
				ad := netip.MustParseAddr(a)
				want = append(want, Interface{Name: a, Addr: ad})
			}
			if !slices.Equal(got, want) {
				t.Fatalf("got %v, want %v", got, want)
			}
		})
	}
}
