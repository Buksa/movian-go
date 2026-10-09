package dvb

// EIT parsing — Event Information Table (ETSI EN 300 468 §5.2.4).
// EIT carries the broadcast EPG: present/following tables (0x4e/0x4f)
// hold now/next, schedule tables (0x50-0x6f) hold up to ~8 days.
// Unlike XMLTV this data comes free with the multiplex — no network.

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// EIT table IDs we parse.
const (
	TableEITOtherPf        = 0x4f // other-TS present/following
	TableEITActualSchedMin = 0x50 // actual-TS schedule, first table
	TableEITActualSchedMax = 0x5f
	TableEITOtherSchedMin  = 0x60 // other-TS schedule
	TableEITOtherSchedMax  = 0x6f
)

// EIT descriptor tags.
const (
	descShortEvent     = 0x4d
	descExtendedEvent  = 0x4e
	descComponent      = 0x50
	descContent        = 0x54
	descParentalRating = 0x55
)

// IsEIT reports whether a table ID is any EIT variant.
func IsEIT(tid uint8) bool {
	switch {
	case tid == TableEITActualPf || tid == TableEITOtherPf:
		return true
	case tid >= TableEITActualSchedMin && tid <= TableEITOtherSchedMax:
		return true
	}
	return false
}

// EITEvent — one programme decoded from an EIT section. Start/Duration
// are UTC; Start is 0 when the broadcaster sent the undefined marker.
type EITEvent struct {
	SID      uint16 // service_id — section TableIDExt
	TSID     uint16 // transport_stream_id — section payload
	ONID     uint16 // original_network_id — section payload
	EventID  uint16
	Title    string  // short_event_descriptor event_name
	Desc     string  // short_event_descriptor text
	LongDesc string  // extended_event_descriptor text (all items)
	Genre    []uint8 // content_descriptor nibbles (level1<<4|level2)
	Parental int     // parental_rating min age, 0 if unrated
	Start    int64   // unix seconds UTC, 0 = undefined
	Duration int     // seconds
	Running  uint8   // running_status (4 = running)
	FreeCA   bool    // scrambled
}

// ParseEIT decodes an EIT section (any of the EIT table IDs) into its
// event list.
func ParseEIT(s *Section) ([]EITEvent, error) {
	if !IsEIT(s.TableID) {
		return nil, fmt.Errorf("dvb: not an EIT (table %02x)", s.TableID)
	}
	p := s.Payload
	if len(p) < 6 {
		return nil, errors.New("dvb: short EIT")
	}
	// transport_stream_id(2) original_network_id(2)
	// segment_last_section_number(1) last_table_id(1)
	tsid := binary.BigEndian.Uint16(p[0:2])
	onid := binary.BigEndian.Uint16(p[2:4])
	p = p[6:]
	sid := s.TableIDExt

	var out []EITEvent
	for len(p) >= 12 {
		ev := EITEvent{
			SID:      sid,
			TSID:     tsid,
			ONID:     onid,
			EventID:  binary.BigEndian.Uint16(p[0:2]),
			Start:    mjdBCDTime(p[2:7]),
			Duration: bcdDuration(p[7:10]),
			Running:  p[10] >> 5,
			FreeCA:   p[10]&0x10 != 0,
		}
		descLen := int(binary.BigEndian.Uint16(p[10:12])) & 0x0fff
		p = p[12:]
		if len(p) < descLen {
			return out, nil
		}
		parseEITDescriptors(p[:descLen], &ev)
		p = p[descLen:]
		out = append(out, ev)
	}
	return out, nil
}

// parseEITDescriptors walks one event's descriptor loop.
func parseEITDescriptors(d []byte, ev *EITEvent) {
	for len(d) >= 2 {
		tag, l := d[0], int(d[1])
		if 2+l > len(d) {
			return
		}
		body := d[2 : 2+l]
		d = d[2+l:]
		switch tag {
		case descShortEvent:
			// ISO-639(3) name_len(1) name text_len(1) text
			if len(body) < 4 {
				continue
			}
			nlen := int(body[3])
			if 4+nlen > len(body) {
				continue
			}
			ev.Title = dvbText(body[4 : 4+nlen])
			rest := body[4+nlen:]
			if len(rest) >= 1 {
				tlen := int(rest[0])
				if 1+tlen <= len(rest) {
					ev.Desc = dvbText(rest[1 : 1+tlen])
				}
			}
		case descExtendedEvent:
			// num(4)|last(4) ISO-639(3) items_len(1) items text_len(1)
			// text — fragments across descriptors concatenate.
			if len(body) < 6 {
				continue
			}
			itemsLen := int(body[4])
			items := body[5:]
			if itemsLen > len(items) {
				itemsLen = len(items)
			}
			items = items[:itemsLen]
			// item loop: desc_len(1) desc item_len(1) item —
			// items describe key/value detail; append item texts.
			for len(items) >= 2 {
				dl := int(items[0])
				items = items[1:]
				if dl > len(items) {
					dl = len(items)
				}
				items = items[dl:]
				if len(items) < 1 {
					break
				}
				il := int(items[0])
				items = items[1:]
				if il > len(items) {
					il = len(items)
				}
				if t := dvbText(items[:il]); t != "" {
					if ev.LongDesc != "" {
						ev.LongDesc += "\n"
					}
					ev.LongDesc += t
				}
				items = items[il:]
			}
			rest := body[5:]
			if len(body) >= 5+itemsLen+1 && len(rest) > itemsLen {
				rest = rest[itemsLen:]
				tlen := int(rest[0])
				if 1+tlen <= len(rest) {
					if t := dvbText(rest[1 : 1+tlen]); t != "" {
						if ev.LongDesc != "" {
							ev.LongDesc += "\n"
						}
						ev.LongDesc += t
					}
				}
			}
		case descContent:
			// pairs: content_nibble_level_1(4)|level_2(4),
			// user_nibble(4)|user_nibble(4)
			for i := 0; i+2 <= len(body); i += 2 {
				ev.Genre = append(ev.Genre, body[i])
			}
		case descParentalRating:
			// per entry: country_code(3) rating(1); rating 0x01-0x0f
			// means min age = rating + 3.
			for i := 0; i+4 <= len(body); i += 4 {
				r := int(body[i+3])
				if r >= 1 && r <= 0x0f {
					if age := r + 3; age > ev.Parental {
						ev.Parental = age
					}
				}
			}
		}
	}
}

// mjdBCDTime decodes a 5-byte DVB time field (MJD u16 + BCD hh:mm:ss)
// to unix seconds. 0xffff / all-ones fields mean "undefined" → 0.
func mjdBCDTime(b []byte) int64 {
	mjd := int64(binary.BigEndian.Uint16(b[0:2]))
	if mjd == 0xffff || mjd == 0 {
		return 0
	}
	// MJD epoch 1858-11-17; unix epoch (1970-01-01) is MJD 40587.
	secs := (mjd - 40587) * 86400
	h, m, s := bcd(b[2]), bcd(b[3]), bcd(b[4])
	return secs + int64(h)*3600 + int64(m)*60 + int64(s)
}

// bcdDuration decodes a 3-byte BCD hh:mm:ss duration to seconds.
func bcdDuration(b []byte) int {
	return bcd(b[0])*3600 + bcd(b[1])*60 + bcd(b[2])
}

func bcd(v uint8) int {
	return int(v>>4)*10 + int(v&0x0f)
}
