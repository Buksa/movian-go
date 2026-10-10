//go:build linux

package dvb

// Linux DVB API v5 bindings — /dev/dvb/adapterN/{frontend,demux,dvr}M.
// Constants and struct layouts mirror linux/dvb/frontend.h and
// linux/dvb/dmx.h. Everything goes through raw ioctl; no cgo.
//
// dtv_property is __attribute__((packed)) in the kernel and its stride
// differs between 64-bit (76B) and 32-bit (72B) ABIs, so properties are
// marshalled into a byte buffer with explicit offsets instead of a Go
// struct.

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ---- ioctl request codes ---------------------------------------------------
//
// _IOC(dir,'o',nr,size): dir<<30 | 'o'<<8 | nr | size<<16.

func ioc(dir, nr, size uintptr) uintptr {
	return dir<<30 | uintptr('o')<<8 | nr<<0 | size<<16
}

// wire structs (natural alignment, same layout as the kernel ones)

type feInfo struct { // struct dvb_frontend_info
	name     [128]byte
	typ      uint32
	freqMin  uint32
	freqMax  uint32
	freqStep uint32
	freqTol  uint32
	symMin   uint32
	symMax   uint32
	symTol   uint32
	notif    uint32
	caps     uint32
}

type dtvPropertiesHdr struct { // struct dtv_properties
	num   uint32
	props unsafe.Pointer // Go pads num→props to ptr alignment, matching C on both ABIs
}

type dmxSctFilterParams struct { // struct dmx_sct_filter_params
	pid     uint16
	filter  [16]byte // struct dmx_filter { filter, mask, mode }
	mask    [16]byte
	mode    [16]byte
	timeout uint32
	flags   uint32
}

type dmxPesFilterParams struct { // struct dmx_pes_filter_params
	pid     uint16
	input   uint32
	output  uint32
	pesType uint32
	flags   uint32
}

var (
	ioctlFEGetInfo     = ioc(2, 61, unsafe.Sizeof(feInfo{}))
	ioctlFEReadStatus  = ioc(2, 69, unsafe.Sizeof(uint32(0)))
	ioctlFESetProperty = ioc(1, 82, unsafe.Sizeof(dtvPropertiesHdr{}))
	ioctlFEGetProperty = ioc(2, 83, unsafe.Sizeof(dtvPropertiesHdr{}))
	ioctlDMXSetFilter  = ioc(1, 43, unsafe.Sizeof(dmxSctFilterParams{}))
	ioctlDMXSetPesFil  = ioc(1, 44, unsafe.Sizeof(dmxPesFilterParams{}))
	ioctlDMXSetBufSize = ioc(0, 45, 0)
	ioctlDMXStart      = ioc(0, 41, 0)
	ioctlDMXStop       = ioc(0, 42, 0)
)

// ---- packed dtv_property marshalling ----------------------------------------
//
// struct dtv_property { __u32 cmd; __u32 reserved[3]; union u;
//                       int result; } __attribute__((packed))
// union u members:
//   u.data   : __u32  @0
//   u.st     : struct dtv_fe_stats { __u8 len; dtv_stats stat[4]; }  @0
//     dtv_stats = { __u8 scale; union{u64,s64}; } packed → 9 bytes each
//   u.buffer : { __u8 data[32]; __u32 len; __u32 reserved1[3];
//                void *reserved2; } @0 — 56B (64-bit) / 52B (32-bit)
// So: cmd@0, u@16, result@stride-4. Stride = 16 + sizeof(union) + 4.

var propStride, propUnionSize int

func init() {
	if unsafe.Sizeof(uintptr(0)) == 8 {
		propUnionSize = 56
	} else {
		propUnionSize = 52
	}
	propStride = 16 + propUnionSize + 4
}

const propOffData = 16        // u.data / u.st.len / u.buffer.data
const propOffBufLen = 16 + 32 // u.buffer.len

// marshalProps packs props into the kernel wire buffer.
func marshalProps(props []dtvProp) []byte {
	buf := make([]byte, propStride*len(props))
	for i, p := range props {
		off := i * propStride
		binary.LittleEndian.PutUint32(buf[off:], p.cmd)
		binary.LittleEndian.PutUint32(buf[off+propOffData:], p.data)
	}
	return buf
}

// unmarshalBuffer reads u.buffer (data[32] as u32 array + len).
func unmarshalBuffer(buf []byte, off int) ([]uint32, int) {
	n := int(binary.LittleEndian.Uint32(buf[off+propOffBufLen:]))
	if n > 8 {
		n = 8
	}
	out := make([]uint32, n)
	for i := 0; i < n; i++ {
		out[i] = binary.LittleEndian.Uint32(buf[off+16+i*4:])
	}
	return out, n
}

// unmarshalStats reads u.st → (len, stat[0] udata).
func unmarshalStats(buf []byte, off int) (int, []uint64) {
	n := int(buf[off+propOffData])
	if n > 4 {
		n = 4
	}
	out := make([]uint64, n)
	for i := 0; i < n; i++ {
		// dtv_stats: scale u8 @0, udata u64 @1 (packed)
		out[i] = binary.LittleEndian.Uint64(buf[off+propOffData+1+i*9+1:])
	}
	return n, out
}

// dtvProp — one (cmd,data) pair.
type dtvProp struct {
	cmd  uint32
	data uint32
}

// ---- DTV property commands (frontend.h) ------------------------------------

const (
	dtvClear          = 2
	dtvFrequency      = 3
	dtvModulation     = 4
	dtvBandwidthHz    = 5
	dtvInversion      = 6
	dtvSymbolRate     = 8
	dtvInnerFEC       = 9
	dtvDeliverySystem = 17
	dtvCodeRateHP     = 36
	dtvGuardInterval  = 38
	dtvTransmitMode   = 39
	dtvStreamID       = 42 // DVB-T2 PLP id | ISDB-S ts id
	dtvEnumDelsys     = 44
	dtvStatSignal     = 62 // DTV_STAT_SIGNAL_STRENGTH
	dtvStatCNR        = 63 // DTV_STAT_CNR
	dtvTune           = 1
)

const dtvIOCTLMaxMsgs = 64

// ---- demux constants (dmx.h) ------------------------------------------------

const (
	dmxInFrontend     = 0  // DMX_IN_FRONTEND
	dmxOutTsTap       = 2  // DMX_OUT_TS_TAP
	dmxPesOther       = 20 // DMX_PES_OTHER
	dmxCheckCRC       = 1
	dmxImmediateStart = 4
	dvrBufferSize     = 10 * 188 * 348 // ~0.65 MB, kernel may clamp
)

// ---- frontend ---------------------------------------------------------------

// Frontend is an open /dev/dvb/adapterN/frontendM device.
type Frontend struct {
	fd *os.File
}

// OpenFrontend opens adapter N frontend M (usually 0).
func OpenFrontend(adapter, frontend int) (*Frontend, error) {
	path := fmt.Sprintf("/dev/dvb/adapter%d/frontend%d", adapter, frontend)
	fd, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("dvb: open %s: %w", path, err)
	}
	return &Frontend{fd: fd}, nil
}

func (f *Frontend) Close() error {
	if f.fd == nil {
		return nil
	}
	err := f.fd.Close()
	f.fd = nil
	return err
}

func (f *Frontend) getProps(cmds []uint32) ([]byte, error) {
	props := make([]dtvProp, len(cmds))
	for i, c := range cmds {
		props[i].cmd = c
	}
	buf := marshalProps(props)
	h := dtvPropertiesHdr{num: uint32(len(props)), props: unsafe.Pointer(&buf[0])}
	if err := f.ioctl(ioctlFEGetProperty, unsafe.Pointer(&h)); err != nil {
		return nil, err
	}
	return buf, nil
}

func (f *Frontend) setProps(props []dtvProp) error {
	buf := marshalProps(props)
	h := dtvPropertiesHdr{num: uint32(len(props)), props: unsafe.Pointer(&buf[0])}
	return f.ioctl(ioctlFESetProperty, unsafe.Pointer(&h))
}

// Info queries FE_GET_INFO, then DTV_ENUM_DELSYS for the delivery list.
func (f *Frontend) Info() (*FrontendInfo, error) {
	var inf feInfo
	if err := f.ioctl(ioctlFEGetInfo, unsafe.Pointer(&inf)); err != nil {
		return nil, fmt.Errorf("dvb: FE_GET_INFO: %w", err)
	}
	fi := &FrontendInfo{
		Name:          cstr(inf.name[:]),
		FrequencyMin:  inf.freqMin,
		FrequencyMax:  inf.freqMax,
		FrequencyStep: inf.freqStep,
	}
	// Some frontends report kHz (old DVB API units); modern digital
	// frontends report Hz. Values < 1 MHz are almost surely kHz.
	if fi.FrequencyMax > 0 && fi.FrequencyMax < 1000000 {
		fi.FrequencyMin *= 1000
		fi.FrequencyMax *= 1000
	}
	fi.DeliverySystems = f.enumDelsys()
	return fi, nil
}

// enumDelsys runs FE_GET_PROPERTY{DTV_ENUM_DELSYS}.
func (f *Frontend) enumDelsys() []DeliverySystem {
	buf, err := f.getProps([]uint32{dtvEnumDelsys})
	if err != nil {
		return nil
	}
	vals, _ := unmarshalBuffer(buf, 0)
	out := make([]DeliverySystem, len(vals))
	for i, v := range vals {
		out[i] = DeliverySystem(v)
	}
	return out
}

// Tune issues DTV_CLEAR + properties + DTV_TUNE in one FE_SET_PROPERTY.
func (f *Frontend) Tune(tp *TuneParams) error {
	props := []dtvProp{{cmd: dtvClear}}
	add := func(cmd, val uint32) {
		props = append(props, dtvProp{cmd: cmd, data: val})
	}
	add(dtvDeliverySystem, uint32(tp.DeliverySys))
	add(dtvFrequency, tp.Frequency)
	switch {
	case tp.DeliverySys.IsTerrestrial():
		if tp.Bandwidth != 0 {
			add(dtvBandwidthHz, tp.Bandwidth)
		}
		add(dtvCodeRateHP, uint32(tp.InnerFEC))
		add(dtvTransmitMode, uint32(tp.TxMode))
		add(dtvGuardInterval, uint32(tp.Guard))
		if tp.Modulation != 0 {
			add(dtvModulation, uint32(tp.Modulation))
		}
		if tp.PLPID != 0 && tp.PLPID != 0xffffffff && tp.DeliverySys == SysDVBT2 {
			add(dtvStreamID, tp.PLPID)
		}
	case tp.DeliverySys.IsCable():
		add(dtvInversion, uint32(tp.Inversion))
		add(dtvModulation, uint32(tp.Modulation))
		add(dtvSymbolRate, tp.SymbolRate)
		add(dtvInnerFEC, uint32(tp.InnerFEC))
	case tp.DeliverySys.IsSatellite():
		add(dtvSymbolRate, tp.SymbolRate)
		add(dtvInnerFEC, uint32(tp.InnerFEC))
		if tp.Modulation != 0 {
			add(dtvModulation, uint32(tp.Modulation))
		}
		if tp.PLPID != 0 && tp.PLPID != 0xffffffff {
			add(dtvStreamID, tp.PLPID) // MIS/PLP on S2
		}
	}
	add(dtvTune, 0)
	if len(props) > dtvIOCTLMaxMsgs {
		return fmt.Errorf("dvb: too many tune props (%d)", len(props))
	}
	return f.setProps(props)
}

// Status reads FE_READ_STATUS.
func (f *Frontend) Status() (FrontendStatus, error) {
	var st uint32
	if err := f.ioctl(ioctlFEReadStatus, unsafe.Pointer(&st)); err != nil {
		return 0, fmt.Errorf("dvb: FE_READ_STATUS: %w", err)
	}
	return FrontendStatus(st), nil
}

// WaitLock polls FE_READ_STATUS until FE_HAS_LOCK or timeout.
func (f *Frontend) WaitLock(timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		st, err := f.Status()
		if err != nil {
			return false, err
		}
		if st.Locked() {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// SignalStrength reads DTV_STAT_SIGNAL_STRENGTH (first stat udata).
func (f *Frontend) SignalStrength() (uint64, bool) { return f.statProp(dtvStatSignal) }

// CNR reads DTV_STAT_CNR (carrier-to-noise ratio).
func (f *Frontend) CNR() (uint64, bool) { return f.statProp(dtvStatCNR) }

func (f *Frontend) statProp(cmd uint32) (uint64, bool) {
	buf, err := f.getProps([]uint32{cmd})
	if err != nil {
		return 0, false
	}
	n, vals := unmarshalStats(buf, 0)
	if n == 0 {
		return 0, false
	}
	return vals[0], true
}

func (f *Frontend) ioctl(req uintptr, arg unsafe.Pointer) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.fd.Fd(), req, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

// ---- demux / dvr --------------------------------------------------------------

// Demux is an open /dev/dvb/adapterN/demuxM device. Each open instance
// carries one section or PES filter.
type Demux struct {
	fd *os.File
}

// OpenDemux opens adapter N demux M.
func OpenDemux(adapter, demux int) (*Demux, error) {
	path := fmt.Sprintf("/dev/dvb/adapter%d/demux%d", adapter, demux)
	fd, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("dvb: open %s: %w", path, err)
	}
	return &Demux{fd: fd}, nil
}

func (d *Demux) Close() error {
	if d.fd == nil {
		return nil
	}
	err := d.fd.Close()
	d.fd = nil
	return err
}

// SetSectionFilter installs a section filter on pid. filterBytes/mask are
// up to 16 bytes matched at the section header (table_id etc).
// Immediate start; CRC checked by the kernel.
func (d *Demux) SetSectionFilter(pid uint16, filterBytes, mask []byte, timeoutMS uint32) error {
	var p dmxSctFilterParams
	p.pid = pid
	n := len(filterBytes)
	if n > 16 {
		n = 16
	}
	copy(p.filter[:], filterBytes[:n])
	copy(p.mask[:], mask[:n])
	p.timeout = timeoutMS
	p.flags = dmxCheckCRC | dmxImmediateStart
	return d.ioctl(ioctlDMXSetFilter, unsafe.Pointer(&p))
}

// SetPESFilter installs a TS-level PID filter routed to the DVR device.
func (d *Demux) SetPESFilter(pid uint16) error {
	p := dmxPesFilterParams{
		pid:     pid,
		input:   dmxInFrontend,
		output:  dmxOutTsTap,
		pesType: dmxPesOther,
		flags:   dmxImmediateStart,
	}
	return d.ioctl(ioctlDMXSetPesFil, unsafe.Pointer(&p))
}

// SetBufferSize grows the demux/dvr ring buffer (bytes).
// DMX_SET_BUFFER_SIZE is _IO — the size is passed as the arg VALUE.
func (d *Demux) SetBufferSize(size int) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, d.fd.Fd(), ioctlDMXSetBufSize, uintptr(size))
	if errno != 0 {
		return errno
	}
	return nil
}

// Start / Stop toggle the filter.
func (d *Demux) Start() error { return d.ioctl(ioctlDMXStart, nil) }
func (d *Demux) Stop() error  { return d.ioctl(ioctlDMXStop, nil) }

// ReadSection reads one assembled section from the demux fd
// (section filters deliver whole PSI sections on read()).
func (d *Demux) ReadSection(buf []byte) (int, error) {
	return d.fd.Read(buf)
}

// ReadTS reads raw TS packets when the filter output is TS_TAP.
func (d *Demux) ReadTS(buf []byte) (int, error) {
	return d.fd.Read(buf)
}

// Poll waits for demux data (or error) up to timeout. Opened O_RDWR so
// reads block; callers Poll first to keep the scan cancellable.
func (d *Demux) Poll(timeout time.Duration) (bool, error) {
	fds := []unix.PollFd{{Fd: int32(d.fd.Fd()), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, int(timeout.Milliseconds()))
	if err != nil {
		return false, err
	}
	return n > 0 && fds[0].Revents&(unix.POLLIN|unix.POLLERR|unix.POLLHUP) != 0, nil
}

func (d *Demux) ioctl(req uintptr, arg unsafe.Pointer) error {
	var a uintptr
	if arg != nil {
		a = uintptr(arg)
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, d.fd.Fd(), req, a)
	if errno != 0 {
		return errno
	}
	return nil
}

// DVR is an open /dev/dvb/adapterN/dvrM read side — the demux output.
type DVR struct {
	fd *os.File
}

// OpenDVR opens adapter N dvr M for reading (non-blocking; pair with Poll).
func OpenDVR(adapter, dvr int) (*DVR, error) {
	path := fmt.Sprintf("/dev/dvb/adapter%d/dvr%d", adapter, dvr)
	fd, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("dvb: open %s: %w", path, err)
	}
	return &DVR{fd: fd}, nil
}

// Read returns whatever TS bytes are buffered; non-blocking (may return
// EAGAIN — callers should Poll first).
func (d *DVR) Read(buf []byte) (int, error) {
	return d.fd.Read(buf)
}

// Poll waits for data up to timeout.
func (d *DVR) Poll(timeout time.Duration) (bool, error) {
	fds := []unix.PollFd{{Fd: int32(d.fd.Fd()), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, int(timeout.Milliseconds()))
	if err != nil {
		return false, err
	}
	return n > 0 && fds[0].Revents&unix.POLLIN != 0, nil
}

func (d *DVR) Close() error {
	if d.fd == nil {
		return nil
	}
	err := d.fd.Close()
	d.fd = nil
	return err
}

// ---- enumeration ------------------------------------------------------------

// Adapters lists all usable adapters: adapterN with a frontend AND a
// demux AND a dvr node (frontend alone is useless — e.g. locked-down
// vendor kernels expose only frontend0).
func Adapters() ([]*Adapter, error) {
	var out []*Adapter
	for i := 0; ; i++ {
		base := fmt.Sprintf("/dev/dvb/adapter%d", i)
		st, err := os.Stat(base)
		if err != nil || !st.IsDir() {
			break
		}
		if !hasNode(base, "demux0") || !hasNode(base, "dvr0") {
			continue
		}
		ad := &Adapter{ID: i, Path: base}
		for j := 0; ; j++ {
			if !hasNode(base, fmt.Sprintf("frontend%d", j)) {
				break
			}
			fe, err := OpenFrontend(i, j)
			if err != nil {
				continue // busy or permission — skip
			}
			if info, err := fe.Info(); err == nil {
				ad.Frontends = append(ad.Frontends, *info)
			}
			fe.Close()
		}
		if len(ad.Frontends) > 0 {
			out = append(out, ad)
		}
	}
	return out, nil
}

func hasNode(base, name string) bool {
	_, err := os.Stat(filepath.Join(base, name))
	return err == nil
}

// AdapterCount is the cheap probe used at backend-registration time —
// no opens, just the node check. Returns the count of usable adapters.
func AdapterCount() int {
	n := 0
	for i := 0; ; i++ {
		base := fmt.Sprintf("/dev/dvb/adapter%d", i)
		st, err := os.Stat(base)
		if err != nil || !st.IsDir() {
			break
		}
		if hasNode(base, "frontend0") && hasNode(base, "demux0") && hasNode(base, "dvr0") {
			n++
		}
	}
	return n
}

// ---- helpers ----------------------------------------------------------------

func cstr(b []byte) string {
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}
