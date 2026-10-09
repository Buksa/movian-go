//go:build linux

package dvb

// Channel scan: for each transponder in a plan, tune, wait lock, pull
// PAT (→PMT pids), PMTs, SDT (names), NIT (LCN) and build the channel
// list. Runs entirely on section filters — no TS parsing needed for SI.

import (
	"context"
	"time"
)

// Scanner holds scan-time device handles. One adapter at a time — a
// frontend can only be tuned to one transponder.
type Scanner struct {
	Adapter int
	Fe      *Frontend
}

// OpenScanner opens adapter N frontend 0.
func OpenScanner(adapter int) (*Scanner, error) {
	fe, err := OpenFrontend(adapter, 0)
	if err != nil {
		return nil, err
	}
	return &Scanner{Adapter: adapter, Fe: fe}, nil
}

func (s *Scanner) Close() error { return s.Fe.Close() }

// TerrestrialPlanIT returns the DVB-T/T2 scan plan for Europe/Italy:
// UHF channels 21–69 (474–858 MHz step 8 MHz) plus VHF-III channels
// 5–12 (174–230 MHz step 7 MHz). delsys is set per transponder attempt —
// callers should try SysDVBT2 first then SysDVBT on modern tuners (the
// driver falls back per delivery system capability).
func TerrestrialPlanIT() []TuneParams {
	var plan []TuneParams
	// VHF-III (ch 5–12): 177.5 + 7*(ch-5) MHz
	for ch := 5; ch <= 12; ch++ {
		freq := uint32(177500000 + (ch-5)*7000000)
		plan = append(plan, TuneParams{
			DeliverySys: SysDVBT, Frequency: freq, Bandwidth: 7000000,
			Inversion: InversionAuto, InnerFEC: FECAuto, TxMode: TmAuto, Guard: GuardAuto,
		})
	}
	// UHF (ch 21–69): 474 + 8*(ch-21) MHz
	for ch := 21; ch <= 69; ch++ {
		freq := uint32(474000000 + (ch-21)*8000000)
		plan = append(plan, TuneParams{
			DeliverySys: SysDVBT2, Frequency: freq, Bandwidth: 8000000,
			Inversion: InversionAuto, InnerFEC: FECAuto, TxMode: TmAuto, Guard: GuardAuto,
		})
	}
	return plan
}

// CablePlan returns a DVB-C plan placeholder — cable lineups differ per
// operator; callers supply the operator's transponder list. Kept minimal:
// a few common center freqs in 8 MHz steps for when no plan is known.
func CablePlan() []TuneParams {
	var plan []TuneParams
	for f := uint32(114000000); f <= 858000000; f += 8000000 {
		plan = append(plan, TuneParams{
			DeliverySys: SysDVBCAnnexA, Frequency: f, SymbolRate: 6900000,
			Modulation: ModQAM256, InnerFEC: FECNone, Inversion: InversionAuto,
		})
	}
	return plan
}

// readSection opens a fresh demux on the pid with a table-id filter and
// collects sections until timeout or maxSections. Sections arrive fully
// assembled by the kernel section filter.
func (s *Scanner) readSections(pid uint16, tableID byte, timeout time.Duration, maxSections int) [][]byte {
	dmx, err := OpenDemux(s.Adapter, 0)
	if err != nil {
		return nil
	}
	defer dmx.Close()
	// filter: match table_id byte
	if err := dmx.SetSectionFilter(pid, []byte{tableID}, []byte{0xff}, uint32(timeout.Milliseconds())); err != nil {
		return nil
	}
	var out [][]byte
	deadline := time.Now().Add(timeout)
	buf := make([]byte, 4096)
	for len(out) < maxSections {
		remain := time.Until(deadline)
		if remain <= 0 {
			break
		}
		ok, err := dmx.Poll(remain)
		if err != nil || !ok {
			break
		}
		n, err := dmx.ReadSection(buf)
		if err != nil || n <= 0 {
			break
		}
		sec := append([]byte(nil), buf[:n]...)
		out = append(out, sec)
	}
	return out
}

// ScanTransponder tunes to tp and returns the channels found. On a
// physical box each tune+lock takes 100–800 ms; we give the PSI reads
// a fixed short window since tables repeat every ~0.5–2 s.
func (s *Scanner) ScanTransponder(tp *TuneParams) ([]*Channel, error) {
	if err := s.Fe.Tune(tp); err != nil {
		return nil, err
	}
	locked, err := s.Fe.WaitLock(1500 * time.Millisecond)
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, nil
	}
	return s.collect(tp, 4*time.Second)
}

// collect reads PAT→PMTs→SDT→NIT on the currently tuned transponder.
func (s *Scanner) collect(tp *TuneParams, budget time.Duration) ([]*Channel, error) {
	patSecs := s.readSections(PIDPAT, TablePAT, 1500*time.Millisecond, 1)
	if len(patSecs) == 0 {
		return nil, nil // locked but no PAT — noise lock
	}
	pat, err := ParseSection(patSecs[0])
	if err != nil {
		return nil, err
	}
	entries, err := ParsePAT(pat)
	if err != nil || len(entries) == 0 {
		return nil, err
	}
	tsid := pat.TableIDExt

	// PMTs — one section filter read per PMT pid.
	chans := make([]*Channel, 0, len(entries))
	bySID := map[uint16]*Channel{}
	for _, e := range entries {
		pmSecs := s.readSections(e.PMTPID, TablePMT, 1200*time.Millisecond, 1)
		if len(pmSecs) == 0 {
			continue
		}
		sec, err := ParseSection(pmSecs[0])
		if err != nil {
			continue
		}
		pmt, err := ParsePMT(sec)
		if err != nil {
			continue
		}
		ch := &Channel{
			Adapter:   s.Adapter,
			Transport: tp,
			TSID:      tsid,
			SID:       e.SID,
			PMTPID:    e.PMTPID,
			PCRPID:    pmt.PCRPID,
			FreeCA:    pmt.FreeCA,
		}
		for _, st := range pmt.Streams {
			switch {
			case st.IsVideo():
				if ch.VideoPID == 0 {
					ch.VideoPID = st.PID
				}
			case st.IsAudio():
				ch.AudioPIDs = append(ch.AudioPIDs, st.PID)
			case st.IsSubtitle():
				ch.SubPIDs = append(ch.SubPIDs, st.PID)
			}
		}
		chans = append(chans, ch)
		bySID[e.SID] = ch
	}

	// SDT actual — channel names. Table 0x42 (this TS).
	for _, raw := range s.readSections(PIDSDT, TableSDTActual, 1200*time.Millisecond, 4) {
		sec, err := ParseSection(raw)
		if err != nil {
			continue
		}
		if sec.TableIDExt != tsid {
			continue
		}
		svcs, err := ParseSDT(sec)
		if err != nil {
			continue
		}
		for _, sv := range svcs {
			if ch, ok := bySID[sv.SID]; ok {
				ch.Name = sv.Name
				if sv.FreeCA {
					ch.FreeCA = true
				}
			}
		}
	}

	// NIT actual — LCN descriptors for numbering (optional).
	for _, raw := range s.readSections(PIDNIT, TableNITActual, 1500*time.Millisecond, 4) {
		sec, err := ParseSection(raw)
		if err != nil {
			continue
		}
		if err := s.applyLCN(sec, tsid, bySID); err != nil {
			continue
		}
	}
	return chans, nil
}

// applyLCN scans the NIT ts_loop for the entry matching this TSID and
// reads logical_channel_descriptors (tag 0x83) assigning LCNs.
func (s *Scanner) applyLCN(sec *Section, tsid uint16, bySID map[uint16]*Channel) error {
	p := sec.Payload
	if len(p) < 2 {
		return nil
	}
	netLen := int(binary16(p[0:])) & 0x0fff
	p = p[2+netLen:]
	if len(p) < 2 {
		return nil
	}
	tsLen := int(binary16(p[0:])) & 0x0fff
	p = p[2:]
	end := tsLen
	if len(p) < end {
		end = len(p)
	}
	for off := 0; off+6 <= end; {
		ts := binary16(p[off:])
		onid := binary16(p[off+2:])
		dLen := int(binary16(p[off+4:])) & 0x0fff
		if ts == tsid {
			for _, ch := range bySID {
				if ch.ONID == 0 {
					ch.ONID = onid
				}
			}
			descs := p[off+6:]
			for d := 0; d+2 <= dLen && d+2 <= len(descs); {
				tag, l := descs[d], int(descs[d+1])
				if d+2+l > len(descs) {
					break
				}
				body := descs[d+2 : d+2+l]
				// logical_channel_descriptor (0x83)
				if tag == 0x83 && l%4 == 0 {
					for k := 0; k+4 <= len(body); k += 4 {
						sid := binary16(body[k:])
						lcn := int(binary16(body[k+2:])) & 0x03ff
						if ch, ok := bySID[sid]; ok && ch.LCN == 0 {
							ch.LCN = lcn
						}
					}
				}
				d += 2 + l
			}
		}
		off += 6 + dLen
	}
	return nil
}

// Scan iterates the plan; progressCb(current index, total, found count).
// Cancellable via ctx.
func (s *Scanner) Scan(ctx context.Context, plan []TuneParams,
	progressCb func(done, total, found int)) ([]*Channel, error) {

	var all []*Channel
	for i, tp := range plan {
		select {
		case <-ctx.Done():
			return all, ctx.Err()
		default:
		}
		chans, err := s.ScanTransponder(&tp)
		if err == nil {
			// dedupe by (onid, tsid, sid)
			for _, c := range chans {
				dup := false
				for _, e := range all {
					if e.TSID == c.TSID && e.SID == c.SID {
						dup = true
						break
					}
				}
				if !dup {
					all = append(all, c)
				}
			}
		}
		if progressCb != nil {
			progressCb(i+1, len(plan), len(all))
		}
	}
	return all, nil
}

func binary16(b []byte) uint16 { return uint16(b[0])<<8 | uint16(b[1]) }
