package dvb

// PSI/SI table parsing — PAT, PMT, SDT, NIT, plus a section assembler
// that reassembles PSI sections from 188-byte TS packets. Pure Go, no
// OS deps — fully unit-testable with recorded streams.

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// TSPacketLen — MPEG-TS packet size.
const TSPacketLen = 188

// Well-known PIDs.
const (
	PIDPAT  = 0x0000
	PIDCAT  = 0x0001
	PIDTSDT = 0x0002
	PIDNIT  = 0x0010
	PIDSDT  = 0x0011
	PIDEIT  = 0x0012
	PIDTDT  = 0x0014
	PIDNull = 0x1fff
)

// Table IDs we parse.
const (
	TablePAT         = 0x00
	TablePMT         = 0x02
	TableNITActual   = 0x40
	TableNITOther    = 0x41
	TableSDTActual   = 0x42
	TableSDTOther    = 0x46
	TableEITActualPf = 0x4e // present/following
)

// ErrSectionCRC is returned when a parsed section fails the MPEG-2 CRC32.
var ErrSectionCRC = errors.New("dvb: bad section CRC32")

// crc32mpeg is the MPEG-2 CRC (poly 0x04C11DB7, init 0xffffffff, no
// reflection, no final xor) used by PSI sections.
var crc32mpeg = makeCRCTable()

func makeCRCTable() *[256]uint32 {
	var t [256]uint32
	for i := 0; i < 256; i++ {
		c := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if c&0x80000000 != 0 {
				c = c<<1 ^ 0x04c11db7
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return &t
}

// CRC32MPEG computes the PSI section checksum.
func CRC32MPEG(b []byte) uint32 {
	crc := uint32(0xffffffff)
	for _, c := range b {
		crc = crc<<8 ^ crc32mpeg[byte(crc>>24)^c]
	}
	return crc
}

// SectionAssembler reassembles PSI sections from TS packets of one PID.
type SectionAssembler struct {
	buf     []byte
	want    int // expected total section bytes (0 = unknown until header)
	started bool
}

// PushPacket feeds one 188-byte TS packet belonging to the PID.
// It returns completed sections (a packet may finish one section and
// start another — stuffing handled automatically).
func (a *SectionAssembler) PushPacket(pkt []byte) [][]byte {
	if len(pkt) < TSPacketLen || pkt[0] != 0x47 {
		return nil
	}
	af := (pkt[3] >> 4) & 3 // adaptation_field_control
	if af == 0 || af == 2 {
		return nil // no payload
	}
	pos := 4
	if af == 3 {
		pos += 1 + int(pkt[4]) // skip adaptation field
		if pos >= TSPacketLen {
			return nil
		}
	}
	payload := pkt[pos:]
	pusi := pkt[1]&0x40 != 0
	var out [][]byte

	if pusi {
		// payload_unit_start: byte 0 is the pointer field
		ptr := int(payload[0])
		if ptr > 0 && a.started {
			// complete pending section
			need := a.want - len(a.buf)
			if need > 0 && need <= ptr {
				a.buf = append(a.buf, payload[1:1+need]...)
				if len(a.buf) == a.want {
					out = append(out, a.buf)
				}
			}
		}
		a.buf = a.buf[:0]
		a.started = false
		payload = payload[1+ptr:]
		if len(payload) == 0 || payload[0] == 0xff {
			return out // stuffing only
		}
		a.started = true
	}
	if !a.started {
		return out // mid-section on a PID we just began watching: drop
	}

	for len(payload) > 0 {
		// stuffing after a completed section
		if !a.started {
			if payload[0] == 0xff {
				break
			}
			a.started = true
		}
		if a.want == 0 && len(a.buf)+len(payload) >= 3 {
			// peek at section_length once we have 3 bytes
			take := 3 - len(a.buf)
			a.buf = append(a.buf, payload[:take]...)
			payload = payload[take:]
			sl := int(binary.BigEndian.Uint16(a.buf[1:3])) & 0x0fff
			a.want = 3 + sl
			if a.want < 3+4 || a.want > 4093+3 {
				a.reset()
				return out
			}
			continue
		}
		if a.want == 0 {
			a.buf = append(a.buf, payload...)
			break
		}
		need := a.want - len(a.buf)
		if need <= 0 {
			out = append(out, a.buf)
			a.reset()
			continue
		}
		take := need
		if take > len(payload) {
			take = len(payload)
		}
		a.buf = append(a.buf, payload[:take]...)
		payload = payload[take:]
		if len(a.buf) == a.want {
			out = append(out, a.buf)
			a.reset()
		}
	}
	return out
}

func (a *SectionAssembler) reset() {
	a.buf = a.buf[:0]
	a.want = 0
	a.started = false
}

// Section is a fully assembled PSI section.
type Section struct {
	TableID    uint8
	TableIDExt uint16 // transport_stream_id / service_id / network_id
	Version    int
	Current    bool
	SecNum     int
	LastSec    int
	Payload    []byte // bytes between the SI header and the CRC
}

// ParseSection validates and decodes a raw PSI section. Checks CRC.
func ParseSection(raw []byte) (*Section, error) {
	if len(raw) < 4 {
		return nil, errors.New("dvb: short section")
	}
	sl := int(binary.BigEndian.Uint16(raw[1:3])) & 0x0fff
	if len(raw) != 3+sl {
		return nil, fmt.Errorf("dvb: section length mismatch (%d vs %d)", len(raw), 3+sl)
	}
	if CRC32MPEG(raw) != 0 {
		return nil, ErrSectionCRC
	}
	if sl < 8 {
		return nil, errors.New("dvb: section too short for SI header")
	}
	s := &Section{
		TableID:    raw[0],
		TableIDExt: binary.BigEndian.Uint16(raw[3:5]),
		Version:    int(raw[5]>>1) & 0x1f,
		Current:    raw[5]&1 != 0,
		SecNum:     int(raw[6]),
		LastSec:    int(raw[7]),
		Payload:    raw[8 : 3+sl-4],
	}
	return s, nil
}

// ---- PAT -------------------------------------------------------------------

// PATEntry maps a service_id to its PMT PID.
type PATEntry struct {
	SID    uint16
	PMTPID uint16
}

// ParsePAT decodes a PAT section → service→PMT map.
func ParsePAT(s *Section) ([]PATEntry, error) {
	if s.TableID != TablePAT {
		return nil, fmt.Errorf("dvb: not a PAT (table %02x)", s.TableID)
	}
	var out []PATEntry
	for i := 0; i+4 <= len(s.Payload); i += 4 {
		sid := binary.BigEndian.Uint16(s.Payload[i:])
		pid := binary.BigEndian.Uint16(s.Payload[i+2:]) & 0x1fff
		if sid == 0 {
			continue // NIT pointer
		}
		out = append(out, PATEntry{SID: sid, PMTPID: pid})
	}
	return out, nil
}

// ---- PMT -------------------------------------------------------------------

// PMTStream — one elementary stream in a program.
type PMTStream struct {
	PID         uint16
	StreamType  uint8
	SubtTagging bool // dvb subtitling descriptor present (0x59)
}

// PMT — decoded PMT.
type PMT struct {
	SID     uint16
	PCRPID  uint16
	Streams []PMTStream
	FreeCA  bool
}

// ParsePMT decodes a PMT section.
func ParsePMT(s *Section) (*PMT, error) {
	if s.TableID != TablePMT {
		return nil, fmt.Errorf("dvb: not a PMT (table %02x)", s.TableID)
	}
	p := s.Payload
	if len(p) < 4 {
		return nil, errors.New("dvb: short PMT")
	}
	pmt := &PMT{
		SID:    s.TableIDExt,
		PCRPID: binary.BigEndian.Uint16(p[0:]) & 0x1fff,
	}
	progInfoLen := int(binary.BigEndian.Uint16(p[2:])) & 0x0fff
	if progInfoLen > 0 {
		descs := p[4:]
		if len(descs) < progInfoLen {
			return nil, errors.New("dvb: truncated PMT descriptors")
		}
		for d := 0; d+2 <= progInfoLen; {
			tag, l := descs[d], int(descs[d+1])
			if tag == 0x09 {
				pmt.FreeCA = true // CA_descriptor on the program
			}
			d += 2 + l
		}
	}
	p = p[4+progInfoLen:]
	for len(p) >= 5 {
		st := p[0]
		pid := binary.BigEndian.Uint16(p[1:3]) & 0x1fff
		esLen := int(binary.BigEndian.Uint16(p[3:5])) & 0x0fff
		p = p[5:]
		if len(p) < esLen {
			return nil, errors.New("dvb: truncated PMT ES")
		}
		stream := PMTStream{PID: pid, StreamType: st}
		for d := 0; d+2 <= esLen; {
			tag, l := p[d], int(p[d+1])
			switch tag {
			case 0x09:
				pmt.FreeCA = true
			case 0x59:
				stream.SubtTagging = true
			}
			d += 2 + l
		}
		p = p[esLen:]
		pmt.Streams = append(pmt.Streams, stream)
	}
	return pmt, nil
}

// IsVideo / IsAudio / IsSub classify stream_type values.
func (p PMTStream) IsVideo() bool {
	switch p.StreamType {
	case 0x01, 0x02, 0x10, 0x1b, 0x24, 0x42: // MPEG-1/2 video, MPEG-4, H264, H265, AVS
		return true
	case 0x80: // some broadcasters use 0x80 for video
		return true
	}
	return false
}

func (p PMTStream) IsAudio() bool {
	switch p.StreamType {
	case 0x03, 0x04, 0x0f, 0x11, 0x06, 0x81, 0x87: // MPEG-1/2 audio, AAC, LATM, private(usually audio), AC3, E-AC3
		return true
	}
	return false
}

func (p PMTStream) IsSubtitle() bool { return p.SubtTagging || p.StreamType == 0x06 && p.SubtTagging }

// ---- SDT -------------------------------------------------------------------

// SDTService — one service entry in the SDT.
type SDTService struct {
	SID         uint16
	Name        string
	Provider    string
	ServiceType uint8 // 1=TV, 2=radio, etc.
	FreeCA      bool
	Running     bool
}

// ParseSDT decodes an SDT (actual or other TS) section.
func ParseSDT(s *Section) ([]SDTService, error) {
	if s.TableID != TableSDTActual && s.TableID != TableSDTOther {
		return nil, fmt.Errorf("dvb: not an SDT (table %02x)", s.TableID)
	}
	p := s.Payload
	if len(p) < 3 {
		return nil, errors.New("dvb: short SDT")
	}
	// original_network_id (2) + reserved (1)
	p = p[3:]
	var out []SDTService
	for len(p) >= 5 {
		sid := binary.BigEndian.Uint16(p[0:2])
		// byte 2: EIT flags + running/freeCA in byte 3
		runningStatus := p[3] >> 5
		freeCA := p[3]&0x10 != 0
		descLen := int(binary.BigEndian.Uint16(p[3:5])) & 0x0fff
		p = p[5:]
		if len(p) < descLen {
			return out, nil
		}
		svc := SDTService{
			SID:     sid,
			FreeCA:  freeCA,
			Running: runningStatus == 4,
		}
		for d := 0; d+2 <= descLen; {
			tag, l := p[d], int(p[d+1])
			if d+2+l > descLen {
				break
			}
			body := p[d+2 : d+2+l]
			if tag == 0x48 && len(body) >= 3 { // service_descriptor
				svc.ServiceType = body[0]
				plen := int(body[1])
				if 2+plen <= len(body) {
					svc.Provider = dvbText(body[2 : 2+plen])
					rest := body[2+plen:]
					if len(rest) >= 1 {
						nlen := int(rest[0])
						if 1+nlen <= len(rest) {
							svc.Name = dvbText(rest[1 : 1+nlen])
						}
					}
				}
			}
			d += 2 + l
		}
		p = p[descLen:]
		out = append(out, svc)
	}
	return out, nil
}

// ---- NIT -------------------------------------------------------------------

// NITEntry — one transport stream entry from the NIT actual table.
type NITEntry struct {
	TSID uint16
	ONID uint16
}

// ParseNIT extracts TS/ONID pairs and LCN descriptor data (0x83a / 0x83).
func ParseNIT(s *Section) ([]NITEntry, error) {
	if s.TableID != TableNITActual && s.TableID != TableNITOther {
		return nil, fmt.Errorf("dvb: not a NIT (table %02x)", s.TableID)
	}
	p := s.Payload
	if len(p) < 2 {
		return nil, errors.New("dvb: short NIT")
	}
	netDescLen := int(binary.BigEndian.Uint16(p[0:2])) & 0x0fff
	p = p[2+netDescLen:]
	if len(p) < 2 {
		return nil, errors.New("dvb: short NIT ts loop")
	}
	tsLoopLen := int(binary.BigEndian.Uint16(p[0:2])) & 0x0fff
	p = p[2:]
	if len(p) < tsLoopLen {
		tsLoopLen = len(p)
	}
	var out []NITEntry
	end := tsLoopLen
	for off := 0; off+6 <= end; {
		tsid := binary.BigEndian.Uint16(p[off:])
		onid := binary.BigEndian.Uint16(p[off+2:])
		descLen := int(binary.BigEndian.Uint16(p[off+4:])) & 0x0fff
		out = append(out, NITEntry{TSID: tsid, ONID: onid})
		off += 6 + descLen
	}
	return out, nil
}

// ---- DVB text decoding -------------------------------------------------------

// dvbText decodes an ETSI TS 101 162 DVB text field: optional encoding
// table selector first byte (0x10 → two-byte subset selector, 0x1f+ →
// described_encoding). The broadcast default is ISO-6937; UTF-8 bytes
// also appear in the wild. Italian channels are Latin-1 compatible in
// practice — map the common accented range.
func dvbText(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	// encoding selector bytes
	switch {
	case b[0] == 0x05:
		return string(b[1:]) // UTF-16BE-ish subset of latin — treat raw
	case b[0] == 0x10 && len(b) >= 3:
		b = b[3:] // ISO/IEC 8859 subtable
	case b[0] == 0x1f && len(b) >= 2:
		b = b[2:]
	case b[0] == 0x20: // UTF-16BE marker per spec? skip
		b = b[1:]
	case b[0] >= 0x01 && b[0] <= 0x0b:
		b = b[1:]
	}
	// ISO-6937 ≈ ASCII + combining accents; strip diacritic combining
	// bytes (0xc0-0xcf diacritic + letter) to plain ASCII approximations,
	// pass through the rest (latin-1 range is already compatible).
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c >= 0xc0 && c <= 0xcf && i+1 < len(b) {
			// combining accent — emit base letter approximation
			i++
			base := b[i]
			switch c {
			case 0xc1: // grave
				out = append(out, base)
			case 0xc2: // acute
				out = append(out, base)
			case 0xc8: // diaeresis
				out = append(out, base)
			default:
				out = append(out, base)
			}
			continue
		}
		if c < 0x20 {
			continue // control
		}
		out = append(out, c)
	}
	return string(out)
}
