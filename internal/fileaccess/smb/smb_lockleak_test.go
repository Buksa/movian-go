package smb

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	facore "github.com/czz/movian-go/internal/fileaccess"
)

// Regression tests: an SMB operation that returns while still
// holding System.mu (the C client's smb_global_mutex) freezes every later
// smb:// operation. Each test drives one early return against a loopback
// fake SMB1 server, then checks that a later operation on the same host
// still completes.

// fakeSMB1 is a loopback SMB1 server: negotiate (share-level security),
// guest session setup, tree connect and NT create always succeed; the
// requests named in silent are read and never answered.
type fakeSMB1 struct {
	ln     net.Listener
	port   int
	silent map[string]bool

	// enumNames are the servers a NetServerEnum2 reply lists, when that
	// request is not silent.
	enumNames []string

	mu    sync.Mutex
	conns []net.Conn
}

func newFakeSMB1(t *testing.T, silent ...string) *fakeSMB1 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMB1{
		ln:     ln,
		port:   ln.Addr().(*net.TCPAddr).Port,
		silent: map[string]bool{},
	}
	for _, s := range silent {
		f.silent[s] = true
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.conns = append(f.conns, c)
			f.mu.Unlock()
			go f.serve(c)
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		f.mu.Lock()
		for _, c := range f.conns {
			c.Close()
		}
		f.mu.Unlock()
	})
	return f
}

func (f *fakeSMB1) serve(c net.Conn) {
	for {
		var hdr [4]byte
		if _, err := io.ReadFull(c, hdr[:]); err != nil {
			return
		}
		n := int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])
		req := make([]byte, 4+n)
		copy(req, hdr[:])
		if _, err := io.ReadFull(c, req[4:]); err != nil {
			return
		}
		if len(req) < 4+smbHdrLen {
			continue
		}
		if rep := f.respond(req); rep != nil {
			c.Write(rep)
		}
	}
}

// reply builds a response to req with an n-byte SMB payload; set fills
// the payload (offsets are the same as the client's rbuf).
func reply(req []byte, n int, set func(p []byte)) []byte {
	rep := make([]byte, 4+n)
	copy(rep[4:4+smbHdrLen], req[4:4+smbHdrLen])
	rep[4+smbFlagsOff] |= 0x80
	binary.LittleEndian.PutUint32(rep[4+smbErrorcodeOff:], 0)
	rep[1], rep[2], rep[3] = byte(n>>16), byte(n>>8), byte(n)
	if set != nil {
		set(rep[4:])
	}
	return rep
}

func (f *fakeSMB1) respond(req []byte) []byte {
	le := binary.LittleEndian
	switch req[4+smbCmdOff] {
	case smbNegProtocol:
		if f.silent["negotiate"] {
			return nil
		}
		return reply(req, smbNegProtoReplyLen+8, func(p []byte) {
			p[32] = 17
			p[35] = 0 // share-level security
			le.PutUint16(p[36:], 1)
			le.PutUint32(p[40:], 4356)
			le.PutUint32(p[52:], serverCapNtSmbs)
		})
	case smbSetupAndx:
		if f.silent["setup"] {
			return nil
		}
		return reply(req, smbSetupAndXReplyLen+1, func(p []byte) {
			p[32] = 3
			le.PutUint16(p[37:], 1) // guest
			le.PutUint16(p[39:], 1) // bytecount: pad only
			le.PutUint16(p[smbUidOff:], 100)
		})
	case smbTreecAndx:
		if f.silent["treeconnect"] {
			return nil
		}
		return reply(req, 36, func(p []byte) {
			le.PutUint16(p[smbTidOff:], 7)
		})
	case smbNtCreateAndx:
		if f.silent["opensrvsvc"] {
			return nil
		}
		return reply(req, smbNTCreateAndXRespLen, func(p []byte) {
			le.PutUint16(p[38:], 0x42)
		})
	case smbTransaction:
		// A DCE/RPC request has two setup words and its PDU type at
		// byte 90 of the request; NetServerEnum2 has no setup words.
		switch {
		case req[63] == 2 && req[90] == 0xb:
			if f.silent["dcerpc_bind"] {
				return nil
			}
			return reply(req, 36, nil)
		case req[63] == 2:
			if f.silent["enumshares"] {
				return nil
			}
			return reply(req, 36, nil) // too short: the client bails out
		default:
			if f.silent["NetServerEnum2"] {
				return nil
			}
			dataOff := transReplyLen + 8
			return reply(req, dataOff+26*len(f.enumNames), func(p []byte) {
				le.PutUint16(p[39:], 8)               // param_count
				le.PutUint16(p[41:], transReplyLen)   // param_offset
				le.PutUint16(p[47:], uint16(dataOff)) // data_offset
				le.PutUint16(p[transReplyLen+6:], uint16(len(f.enumNames)))
				for i, n := range f.enumNames {
					copy(p[dataOff+26*i:], n)
				}
			})
		}
	}
	return nil
}

func (f *fakeSMB1) url(path string) string {
	return fmt.Sprintf("smb://127.0.0.1:%d%s", f.port, path)
}

func newTestSystem() *System {
	sys := NewSystem()
	sys.nbtTimeoutOverrideMs = 200
	return sys
}

// requireOperationCompletes fails the test when fn does not return in
// time: it is blocked on a System.mu left locked by an earlier operation.
func requireOperationCompletes(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s hung: System.mu was left locked by an earlier operation", what)
	}
}

// requireLaterStatCompletes is the user-visible check: opening the host
// again does not hang.
func requireLaterStatCompletes(t *testing.T, sys *System, f *fakeSMB1) {
	t.Helper()
	requireOperationCompletes(t, "a later Stat on the same host", func() {
		if _, err := sys.smb1StatFAP(f.url("/"), 0); err != nil {
			t.Errorf("later Stat: %v", err)
		}
	})
}

// A master browser that answers the NetBIOS query but never answers the
// SMB1 NetServerEnum2 must not freeze later smb:// access.
func TestEnumServersUnansweredNetServerEnum2LaterOperationDoesNotHang(t *testing.T) {
	f := newFakeSMB1(t, "NetServerEnum2")
	sys := newTestSystem()

	requireOperationCompletes(t, "SMBEnumServers", func() {
		if got := sys.smbEnumServers("127.0.0.1", f.port); got != nil {
			t.Errorf("servers = %v, want none", got)
		}
	})
	requireLaterStatCompletes(t, sys, f)
}

// An empty (but answered) server list is not an error, and must not
// freeze later smb:// access either.
func TestEnumServersEmptyListLaterOperationDoesNotHang(t *testing.T) {
	f := newFakeSMB1(t)
	sys := newTestSystem()

	requireOperationCompletes(t, "SMBEnumServers", func() {
		if got := sys.smbEnumServers("127.0.0.1", f.port); got != nil {
			t.Errorf("servers = %v, want none", got)
		}
	})
	requireLaterStatCompletes(t, sys, f)
}

// A successful enumeration is unchanged: it lists the servers and a later
// operation still completes.
func TestEnumServersReturnsListedServers(t *testing.T) {
	f := newFakeSMB1(t)
	f.enumNames = []string{"NAS", "PC2"}
	sys := newTestSystem()

	requireOperationCompletes(t, "SMBEnumServers", func() {
		got := sys.smbEnumServers("127.0.0.1", f.port)
		want := []string{"NAS", "WORKGROUP", "PC2", "WORKGROUP"}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("servers = %v, want %v", got, want)
		}
	})
	requireLaterStatCompletes(t, sys, f)
}

// A keyring that rejects the share login while the client holds the
// connection (before any tree exists) must not freeze later access.
func TestTreeConnectRejectedByKeyringLaterOperationDoesNotHang(t *testing.T) {
	f := newFakeSMB1(t)
	sys := newTestSystem()
	sys.SetKeyringLookup(func(id string, username, password, domain *string,
		rememberMe *int, source, reason string, flags int) int {
		return -1
	})

	requireOperationCompletes(t, "SMBEnumServers", func() {
		if got := sys.smbEnumServers("127.0.0.1", f.port); got != nil {
			t.Errorf("servers = %v, want none", got)
		}
	})
	sys.SetKeyringLookup(nil)
	requireLaterStatCompletes(t, sys, f)
}

// Listing the shares of a host whose srvsvc pipe stops answering must not
// freeze later smb:// access.
func TestScandirHostOpenSrvsvcUnansweredLaterOperationDoesNotHang(t *testing.T) {
	testScandirHostUnanswered(t, "opensrvsvc")
}

func TestScandirHostDcerpcBindUnansweredLaterOperationDoesNotHang(t *testing.T) {
	testScandirHostUnanswered(t, "dcerpc_bind")
}

func TestScandirHostEnumSharesUnansweredLaterOperationDoesNotHang(t *testing.T) {
	testScandirHostUnanswered(t, "enumshares")
}

func testScandirHostUnanswered(t *testing.T, silent string) {
	f := newFakeSMB1(t, silent)
	sys := newTestSystem()

	requireOperationCompletes(t, "SMBScandir", func() {
		if err := sys.SMBScandir(new(facore.Dir), f.url(""), 0); err == nil {
			t.Error("SMBScandir: want an error")
		}
	})
	requireLaterStatCompletes(t, sys, f)
}

// Deleting a bare host URL is invalid; it must fail without freezing
// later smb:// access.
func TestDeleteHostURLLaterOperationDoesNotHang(t *testing.T) {
	f := newFakeSMB1(t)
	sys := newTestSystem()

	requireOperationCompletes(t, "cifsDelete", func() {
		if err := sys.cifsDelete(f.url(""), false); err == nil {
			t.Error("cifsDelete: want an error for a host URL")
		}
	})
	requireLaterStatCompletes(t, sys, f)
}
