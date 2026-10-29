//go:build !windows

package lan

import (
	"net/netip"
	"testing"
)

// An interface found without netlink has no index; its socket names the
// multicast interface by address.
func TestListenOnWithoutAnInterfaceIndex(t *testing.T) {
	conn, err := Listen(Interface{Name: "lo", Addr: netip.MustParseAddr("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
}
