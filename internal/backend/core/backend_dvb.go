package core

// DVB live TV state owned by BackendSystem. The type is platform-
// neutral (internal/dvb compiles everywhere); the scan/tune/protocol
// code lives in platform_dvb_linux.go and runs only on linux&&!android.

import (
	"encoding/json"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/czz/movian-go/internal/db/kvstore"
	"github.com/czz/movian-go/internal/dvb"
)

// dvbStoreURL is the kvstore pseudo-URL for the scanned channel list.
const dvbStoreURL = "dvb:store"

// dvbStore — persisted channel list (kvstore, JSON blob).
type dvbStore struct {
	Channels []*dvb.Channel `json:"channels"`
	Scanned  int64          `json:"scanned"` // unixtime
}

// dvbService is the runtime DVB state. One per BackendSystem, created
// only when adapters exist — a frontend is a shared single resource so
// scan and playback serialize on scanMu/tuneMu.
type dvbService struct {
	bs    *BackendSystem
	mu    sync.Mutex // guards store
	store *dvbStore

	scanning atomic.Bool // a full scan is running

	scanMu sync.Mutex // serializes scans (frontend in use)
	tuneMu sync.Mutex // serializes playback tuning

	// Broadcast EPG collected from EIT sections while a channel is
	// tuned. Keyed by onid<<32|tsid<<16|sid so "other-TS" tables land
	// on their own channels too.
	eitMu sync.Mutex
	eit   map[uint64][]dvb.EITEvent
}

// eitKey — map key for one service's EPG: onid.tsid.sid.
func eitKey(onid, tsid, sid uint16) uint64 {
	return uint64(onid)<<32 | uint64(tsid)<<16 | uint64(sid)
}

// eitMerge folds freshly parsed EIT events into the store: same
// event_id replaces (broadcasters resend updated rows), finished
// events expire after 1 h, lists stay sorted by start.
func (s *dvbService) eitMerge(evs []dvb.EITEvent) {
	if len(evs) == 0 {
		return
	}
	now := time.Now().Unix()
	s.eitMu.Lock()
	defer s.eitMu.Unlock()
	if s.eit == nil {
		s.eit = map[uint64][]dvb.EITEvent{}
	}
	touched := map[uint64]bool{}
	for _, e := range evs {
		k := eitKey(e.ONID, e.TSID, e.SID)
		lst := s.eit[k]
		replaced := false
		for i := range lst {
			if lst[i].EventID == e.EventID {
				lst[i] = e
				replaced = true
				break
			}
		}
		if !replaced {
			lst = append(lst, e)
		}
		s.eit[k] = lst
		touched[k] = true
	}
	for k := range touched {
		lst := s.eit[k]
		out := lst[:0]
		for _, x := range lst {
			if x.Start+int64(x.Duration) > now-3600 {
				out = append(out, x)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
		s.eit[k] = out
	}
}

// eitEvents — sorted EPG events for a channel in [from,to) (unix).
func (s *dvbService) eitEvents(ch *dvb.Channel, from, to int64) []dvb.EITEvent {
	s.eitMu.Lock()
	defer s.eitMu.Unlock()
	var out []dvb.EITEvent
	for _, e := range s.eit[eitKey(ch.ONID, ch.TSID, ch.SID)] {
		if e.Start >= from && e.Start < to {
			out = append(out, e)
		}
	}
	return out
}

func (s *dvbService) loadChannels() {
	kvs := s.bs.kvstore
	if kvs == nil {
		return
	}
	raw, ok := kvs.UrlOptGetStringOK(dvbStoreURL, kvstore.DomainPlugin, "channels")
	if !ok || raw == "" {
		return
	}
	var st dvbStore
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return
	}
	s.store = &st
}

func (s *dvbService) saveChannels() {
	kvs := s.bs.kvstore
	if kvs == nil {
		return
	}
	s.mu.Lock()
	raw, err := json.Marshal(s.store)
	s.mu.Unlock()
	if err != nil {
		return
	}
	kvs.UrlOptSet(dvbStoreURL, kvstore.DomainPlugin, "channels",
		kvstore.SetString, string(raw))
}
