package smb

import (
	"encoding/binary"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/czz/movian-go/internal/asyncio"
	netcore "github.com/czz/movian-go/internal/networking/core"
	propcore "github.com/czz/movian-go/internal/prop"
	"github.com/czz/movian-go/internal/task"
	"github.com/czz/movian-go/internal/trace"
)

// fakeNBNS is a NetBIOS name server on loopback. It records the name
// of every query and answers name queries for knownName with ip. Made
// a master browser, it also answers __MSBROWSE__ queries.
type fakeNBNS struct {
	conn      *net.UDPConn
	knownName string
	ip        [4]byte

	mu      sync.Mutex
	names   []string
	last    *net.UDPAddr // where the latest query came from
	master  [4]byte      // non-zero: the master browser's address
	msb     []byte       // the latest __MSBROWSE__ query
	msbFrom *net.UDPAddr // where it came from
}

func startFakeNBNS(t *testing.T, knownName string, ip [4]byte) *fakeNBNS {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	f := &fakeNBNS{conn: conn, knownName: knownName, ip: ip}
	go f.serve()
	return f
}

func (f *fakeNBNS) addr() *netcore.NetAddr {
	ua := f.conn.LocalAddr().(*net.UDPAddr)
	na := &netcore.NetAddr{Family: 4, Port: uint16(ua.Port)}
	copy(na.Addr[:4], ua.IP.To4())
	return na
}

func (f *fakeNBNS) serve() {
	buf := make([]byte, 576)
	for {
		n, from, err := f.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if n < nmbpktQuestionLen {
			continue
		}
		name, typ := decodeNBName(buf[13:45])
		msb := typ == 0x01 && strings.Contains(name, "__MSBROWSE__")
		f.mu.Lock()
		f.names = append(f.names, name)
		f.last = from
		if msb {
			f.msb, f.msbFrom = slices.Clone(buf[:nmbpktQuestionLen]), from
		}
		master := f.master
		f.mu.Unlock()
		switch {
		case typ == 0x20 && name == f.knownName:
			f.conn.WriteToUDP(nbAnswer(buf[0:2], buf[12:46], f.ip), from)
		case msb && master != [4]byte{}:
			f.conn.WriteToUDP(nbAnswer(buf[0:2], buf[12:46], master), from)
		}
	}
}

// startFakeMasterBrowser starts a fakeNBNS that answers __MSBROWSE__
// queries as the master browser at ip.
func startFakeMasterBrowser(t *testing.T, ip [4]byte) *fakeNBNS {
	t.Helper()
	f := startFakeNBNS(t, "", [4]byte{})
	f.mu.Lock()
	f.master = ip
	f.mu.Unlock()
	return f
}

// masterAnswersAgain has the master browser answer the latest
// __MSBROWSE__ query once more, as it does each time NetBIOS asks.
func (f *fakeNBNS) masterAnswersAgain(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	q, to, master := f.msb, f.msbFrom, f.master
	f.mu.Unlock()
	if q == nil {
		t.Fatal("no __MSBROWSE__ query seen yet")
	}
	f.conn.WriteToUDP(nbAnswer(q[0:2], q[12:46], master), to)
}

func (f *fakeNBNS) sawName(sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range f.names {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}

// sendStray sends an unsolicited positive response with txid to the
// socket the latest query came from.
func (f *fakeNBNS) sendStray(t *testing.T, txid uint16) {
	t.Helper()
	f.mu.Lock()
	to := f.last
	f.mu.Unlock()
	if to == nil {
		t.Fatal("no query seen yet")
	}
	var id [2]byte
	binary.BigEndian.PutUint16(id[:], txid)
	q := make([]byte, 34)
	encodeName("STRAY", q, 0x20)
	f.conn.WriteToUDP(nbAnswer(id[:], q, [4]byte{192, 0, 2, 99}), to)
}

func waitFor(cond func() bool) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

// decodeNBName reverses NetBIOS first-level encoding of the 32
// encoded bytes: the 15-character name (spaces trimmed) and the type.
func decodeNBName(enc []byte) (string, byte) {
	var raw [16]byte
	for i := range raw {
		raw[i] = (enc[2*i]-'A')<<4 | (enc[2*i+1] - 'A')
	}
	return strings.TrimRight(string(raw[:15]), " "), raw[15]
}

// nbAnswer builds a positive name query response (RFC 1002 4.2.13)
// echoing txid and the question name, with one NB address.
func nbAnswer(txid, qname []byte, ip [4]byte) []byte {
	pkt := make([]byte, nmbpktAnswerLen)
	copy(pkt[0:2], txid)
	binary.BigEndian.PutUint16(pkt[2:4], 0x8500) // response, authoritative, RD
	binary.BigEndian.PutUint16(pkt[6:8], 1)      // answer_count
	copy(pkt[12:46], qname)
	binary.BigEndian.PutUint16(pkt[46:48], 0x20) // type NB
	binary.BigEndian.PutUint16(pkt[48:50], 0x01) // class IN
	binary.BigEndian.PutUint32(pkt[50:54], 300)  // ttl
	binary.BigEndian.PutUint16(pkt[54:56], 6)    // rdlength
	copy(pkt[58:62], ip[:])
	return pkt
}

// startNMB runs an NMBManager on a live asyncio loop, sending its
// name-service queries to dst. With a sink it starts host discovery,
// reporting into sink, on the asyncio thread, as the nmb_init helper
// does unless --disable-sd is given; nil leaves discovery off. setup
// runs before anything starts.
func startNMB(t *testing.T, dst *netcore.NetAddr, sink ServerSink, setup ...func(*NMBManager)) *NMBManager {
	t.Helper()
	aio := asyncio.NewAsyncIO()
	aio.SetTraceSystem(trace.NewTraceSystem(propcore.NewPropManager()))
	aio.StartEarly()
	aio.Start()

	m := NewNMBManager(&System{})
	m.nbnsDest = dst
	for _, f := range setup {
		f(m)
	}
	m.NMBResolverStart(aio, task.NewTaskSystem())
	// The loop outlives the test, but its socket must not: asyncio UDP
	// sockets set SO_REUSEPORT, so a later test's socket can be bound
	// to the same ephemeral port and lose replies to this one.
	t.Cleanup(func() {
		closed := make(chan struct{})
		aio.RunTask(func(any) {
			if m.udpFd != nil {
				m.udpFd.Close()
			}
			close(closed)
		}, nil)
		<-closed
	})
	started := make(chan struct{})
	aio.RunTask(func(any) {
		if sink != nil {
			m.NMBStart(sink)
		}
		close(started)
	}, nil)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("NMBStart did not run on the asyncio loop")
	}
	return m
}

// ignoreServers is a ServerSink that drops every report.
type ignoreServers struct{}

func (ignoreServers) NetBIOSFound(name, workgroup string) {}
func (ignoreServers) NetBIOSLost(name, workgroup string)  {}

func TestNetBIOSNameResolvesWithDiscoveryOff(t *testing.T) {
	nbns := startFakeNBNS(t, "FILESERVER", [4]byte{192, 0, 2, 10})
	m := startNMB(t, nbns.addr(), nil)

	ip, err := m.NMBResolve("FILESERVER")
	if err != nil {
		t.Fatalf("NMBResolve(FILESERVER) with discovery off: %v", err)
	}
	if !ip.Equal(net.IPv4(192, 0, 2, 10)) {
		t.Fatalf("NMBResolve(FILESERVER) = %v, want 192.0.2.10", ip)
	}
	if nbns.sawName("__MSBROWSE__") {
		t.Fatal("discovery is off, but a __MSBROWSE__ query was sent")
	}
}

func TestMasterBrowserQueryIsSentWithDiscoveryOn(t *testing.T) {
	nbns := startFakeNBNS(t, "FILESERVER", [4]byte{192, 0, 2, 10})
	startNMB(t, nbns.addr(), ignoreServers{})

	if !waitFor(func() bool { return nbns.sawName("__MSBROWSE__") }) {
		t.Fatal("discovery is on, but no __MSBROWSE__ query was sent at start")
	}
}

func TestStrayReplyStartsNoServerListingWithDiscoveryOff(t *testing.T) {
	nbns := startFakeNBNS(t, "FILESERVER", [4]byte{192, 0, 2, 10})
	listed := make(chan string, 4)
	m := startNMB(t, nbns.addr(), nil, func(m *NMBManager) {
		m.enumServers = func(host string) []string {
			listed <- host
			return nil
		}
	})
	if _, err := m.NMBResolve("FILESERVER"); err != nil {
		t.Fatalf("NMBResolve(FILESERVER): %v", err)
	}

	// txid 0 is what msbTxid holds when no master-browser query was sent.
	nbns.sendStray(t, 0)
	if _, err := m.NMBResolve("FILESERVER"); err != nil {
		t.Fatalf("NMBResolve(FILESERVER) after the stray reply: %v", err)
	}
	select {
	case host := <-listed:
		t.Fatalf("discovery is off, but a stray reply listed servers on %s", host)
	case <-time.After(300 * time.Millisecond):
	}
}
