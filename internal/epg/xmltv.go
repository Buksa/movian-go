// Package epg — XMLTV guide fetch/parse/lookup. Source-agnostic: any
// backend that can't get EPG natively (Android TvProvider locked,
// DVB without EIT) can fill Movian's canonical metadata.events model
// (the HTSP shape: list + current/next links) from a remote XMLTV
// feed such as iptv-org/epg or epgshare01.
package epg

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// Event — one programme in a channel's schedule.
type Event struct {
	Title string
	Desc  string
	Start int64 // unix seconds
	Stop  int64
}

// Guide — parsed XMLTV indexed by normalized channel name.
type Guide struct {
	progs     map[string][]Event // "#id:"+xmltv channel id -> schedule
	alias     map[string]string  // normalized display-name/id -> "#id:"+id
	FetchedAt time.Time
}

// normalizeName — matching key for "Rai 1 HD" vs XMLTV "Rai1" /
// "Rai Uno HD": lowercase, strip diacritics, drop parenthetical
// suffixes ("(provvisorio)"), drop non-alnum, drop the standalone
// quality suffixes (hd, sd, uhd, 4k, plus).
func normalizeName(s string) string {
	if i := strings.IndexByte(s, '('); i >= 0 {
		s = s[:i]
	}
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)),
		norm.NFC)
	out, _, err := transform.String(t, s)
	if err != nil {
		out = s
	}
	out = strings.ToLower(out)
	var b strings.Builder
	for _, r := range out {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	out = b.String()
	for _, suf := range []string{"hd", "sd", "uhd", "4k", "plus"} {
		out = strings.TrimSuffix(out, suf)
	}
	return out
}

// xmltvTime — "20060102150405 -0700" (offset optional, can also be
// "+02:00" or bare with implied UTC).
func xmltvTime(s string) int64 {
	s = strings.TrimSpace(s)
	if len(s) < 14 {
		return 0
	}
	base := s[:14]
	rest := strings.TrimSpace(s[14:])
	layout := "20060102150405"
	if rest != "" {
		rest = strings.ReplaceAll(rest, ":", "")
		if t, err := time.Parse(layout+" -0700", base+" "+rest); err == nil {
			return t.Unix()
		}
	}
	t, err := time.ParseInLocation(layout, base, time.UTC)
	if err != nil {
		return 0
	}
	return t.Unix()
}

type xtvText struct {
	Lang  string `xml:"lang,attr"`
	Value string `xml:",chardata"`
}

type xtvChannel struct {
	ID    string    `xml:"id,attr"`
	Names []xtvText `xml:"display-name"`
}

type xtvProgramme struct {
	Channel string    `xml:"channel,attr"`
	Start   string    `xml:"start,attr"`
	Stop    string    `xml:"stop,attr"`
	Title   []xtvText `xml:"title"`
	Desc    []xtvText `xml:"desc"`
}

// pickText — prefer the first non-empty text (feed ordering usually
// puts the local language first anyway).
func pickText(ts []xtvText) string {
	for _, t := range ts {
		if v := strings.TrimSpace(t.Value); v != "" {
			return v
		}
	}
	return ""
}

// ParseXMLTV — streaming parse; programmes for channels whose
// normalized display-name (or channel id) fails the keep filter are
// skipped, bounding memory for curated feeds.
func ParseXMLTV(r io.Reader, keepFn func(normName string) bool) (*Guide, error) {
	g := &Guide{
		progs: make(map[string][]Event),
		alias: make(map[string]string),
	}
	// Some generators prepend a UTF-8 BOM; encoding/xml rejects it.
	br := bufio.NewReader(r)
	if b, _ := br.Peek(3); bytes.Equal(b, []byte{0xEF, 0xBB, 0xBF}) {
		br.Discard(3)
	}
	dec := xml.NewDecoder(br)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "channel":
			var ch xtvChannel
			if err := dec.DecodeElement(&ch, &se); err != nil {
				return nil, err
			}
			key := "#id:" + ch.ID
			match := keepFn == nil
			if !match {
				if keepFn(normalizeName(ch.ID)) {
					match = true
				} else {
					for _, n := range ch.Names {
						if keepFn(normalizeName(n.Value)) {
							match = true
							break
						}
					}
				}
			}
			if !match {
				continue
			}
			g.alias[normalizeName(ch.ID)] = key
			for _, n := range ch.Names {
				g.alias[normalizeName(n.Value)] = key
			}
		case "programme":
			var p xtvProgramme
			if err := dec.DecodeElement(&p, &se); err != nil {
				return nil, err
			}
			if _, aliased := g.alias[normalizeName(p.Channel)]; !aliased {
				continue
			}
			key := "#id:" + p.Channel
			ev := Event{
				Title: pickText(p.Title),
				Desc:  pickText(p.Desc),
				Start: xmltvTime(p.Start),
				Stop:  xmltvTime(p.Stop),
			}
			if ev.Start == 0 || ev.Title == "" {
				continue
			}
			if ev.Stop == 0 {
				ev.Stop = ev.Start + 3600
			}
			g.progs[key] = append(g.progs[key], ev)
		}
	}
	for _, evs := range g.progs {
		sort.Slice(evs, func(i, j int) bool {
			return evs[i].Start < evs[j].Start
		})
	}
	return g, nil
}

// match — resolve a tuner/channel name to a "#id:" key. Exact
// normalized-name hit first; else the longest alias that is a prefix
// or suffix of the name (covers feed "Italia 2" vs tuner "Mediaset
// Italia 2 HD", "RaiRadio2" vs "Rai Radio 2 Visual HD"). Minimum 4
// chars on the alias to avoid degenerate matches.
func (g *Guide) match(name string) string {
	n := normalizeName(name)
	if k, ok := g.alias[n]; ok {
		return k
	}
	best := ""
	for a := range g.alias {
		if len(a) < 4 || len(a) <= len(best) {
			continue
		}
		if strings.HasPrefix(n, a) || strings.HasSuffix(n, a) {
			best = a
		}
	}
	if best == "" {
		return ""
	}
	return g.alias[best]
}

// NowNext — event covering t and the following one for a channel
// name. Returns nil when unknown.
func (g *Guide) NowNext(name string, t time.Time) (cur, next *Event) {
	if g == nil {
		return nil, nil
	}
	progs := g.progs[g.match(name)]
	u := t.Unix()
	for i := range progs {
		e := &progs[i]
		if e.Stop <= u {
			continue
		}
		if e.Start <= u {
			cur = e
			if i+1 < len(progs) {
				next = &progs[i+1]
			}
			return
		}
		next = e
		return
	}
	return nil, nil
}

// EventsFor — all events for a channel name overlapping [from, to).
// Programmes are sorted by start, so the scan can bail early.
func (g *Guide) EventsFor(name string, from, to time.Time) []Event {
	progs := g.progs[g.match(name)]
	if len(progs) == 0 {
		return nil
	}
	lo, hi := from.Unix(), to.Unix()
	var out []Event
	for _, e := range progs {
		if e.Stop <= lo {
			continue
		}
		if e.Start >= hi {
			break
		}
		out = append(out, e)
	}
	return out
}

// FetchXMLTV — download (gzip-aware) and parse, filtered to the given
// normalized channel names (nil keeps everything).
func FetchXMLTV(ctx context.Context, url string,
	names map[string]bool) (*Guide, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("User-Agent", "movian-go")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("epg fetch %s: %s", url, resp.Status)
	}
	var r io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" ||
		strings.HasSuffix(url, ".gz") {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		r = gz
	}
	keepFn := func(normName string) bool {
		return names == nil || names[normName]
	}
	g, err := ParseXMLTV(r, keepFn)
	if err != nil {
		return nil, err
	}
	g.FetchedAt = time.Now()
	return g, nil
}

// KeepForNames — builds the normalized-name set backends pass to
// FetchXMLTV.
func KeepForNames(names []string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[normalizeName(n)] = true
	}
	return m
}
