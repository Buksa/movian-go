package core

// Shared EPG→prop plumbing — builds metadata.events (list + current/
// next links) in the canonical HTSP shape so items/list/tvchannel.view
// and the TV page sidebars render events from any source (XMLTV on
// Android TIF, EIT on Linux DVB) unchanged.

import (
	"fmt"
	"time"

	"github.com/czz/movian-go/internal/dvb"
	"github.com/czz/movian-go/internal/epg"
	propcore "github.com/czz/movian-go/internal/prop"
)

// tvSetEvents — rebuild metadata.events (full event list + current/
// next links) in the canonical HTSP shape, so every skin feature that
// reads events.* works unchanged for TIF/XMLTV and DVB/EIT data.
// evs must be sorted by start; current/next flags and links are
// derived from `now`. Links are dropped before the old nodes are
// destroyed so subscribers never hold a dead event node.
func tvSetEvents(pm *propcore.PropManager, list, curP, nextP *propcore.Prop,
	evs []epg.Event, now time.Time) {
	pm.Unlink(curP)
	pm.Unlink(nextP)
	pm.DestroyChilds(list)
	u := now.Unix()
	curDone, nextDone := false, false
	for i := range evs {
		e := &evs[i]
		n := pm.CreateEx(list, fmt.Sprintf("e%d", i), nil, false, false)
		pm.SetStringEx(pm.CreateEx(n, "type", nil, false, false),
			nil, "event", propcore.StringUTF8)
		m := pm.CreateEx(n, "metadata", nil, false, false)
		pm.SetStringEx(pm.CreateEx(m, "title", nil, false, false),
			nil, e.Title, propcore.StringUTF8)
		if e.Desc != "" {
			pm.SetStringEx(pm.CreateEx(m, "description", nil,
				false, false), nil, e.Desc, propcore.StringUTF8)
		}
		pm.SetIntEx(pm.CreateEx(m, "start", nil, false, false),
			nil, int(e.Start))
		pm.SetIntEx(pm.CreateEx(m, "stop", nil, false, false),
			nil, int(e.Stop))
		isCur := e.Start <= u && e.Stop > u
		if isCur && !curDone {
			pm.SetIntEx(pm.CreateEx(m, "isCurrent", nil, false,
				false), nil, 1)
			pm.Link(n, curP, nil, false, false)
			curDone = true
		}
		if !isCur && !nextDone && e.Start > u {
			pm.SetIntEx(pm.CreateEx(m, "isNext", nil, false,
				false), nil, 1)
			pm.Link(n, nextP, nil, false, false)
			nextDone = true
		}
	}
}

// eitToEPG adapts broadcast EIT events to the epg.Event shape used by
// the prop layer. Broadcast descriptions come from the short_event
// text first; when absent the extended_event text substitutes.
func eitToEPG(evs []dvb.EITEvent) []epg.Event {
	out := make([]epg.Event, 0, len(evs))
	for _, e := range evs {
		if e.Start == 0 {
			continue
		}
		desc := e.Desc
		if desc == "" {
			desc = e.LongDesc
		}
		out = append(out, epg.Event{
			Title: e.Title,
			Desc:  desc,
			Start: e.Start,
			Stop:  e.Start + int64(e.Duration),
		})
	}
	return out
}
