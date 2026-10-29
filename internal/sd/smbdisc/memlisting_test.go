package smbdisc

import (
	"slices"
	"strings"
	"testing"
)

// memListing is a listing that keeps its entries in memory. Each entry
// is its own handle, so a test can tell an entry whose URL moved in place
// from one created anew by comparing the handles it gets from handleOf.
//
// It holds the host table to the order the registry needs: an entry is
// removed before another takes its id, and only listed entries change.
type memListing struct {
	t       *testing.T
	entries []*memEntry
}

type memEntry struct{ entry }

func newMemListing(t *testing.T) *memListing { return &memListing{t: t} }

func (l *memListing) add(id, title, url string) listingHandle {
	l.t.Helper()
	if l.handleOf(id) != nil {
		l.t.Errorf("%s is listed while another entry takes its id", id)
	}
	e := &memEntry{entry{id, title, url}}
	l.entries = append(l.entries, e)
	return e
}

func (l *memListing) setURL(h listingHandle, url string) {
	l.t.Helper()
	e := h.(*memEntry)
	if !slices.Contains(l.entries, e) {
		l.t.Errorf("URL set of %s, which is not listed", e.id)
		return
	}
	e.url = url
}

func (l *memListing) remove(h listingHandle) {
	l.t.Helper()
	e := h.(*memEntry)
	i := slices.Index(l.entries, e)
	if i < 0 {
		l.t.Errorf("%s removed, which is not listed", e.id)
		return
	}
	l.entries = slices.Delete(l.entries, i, i+1)
}

// listed returns the entries as Local network shows them, by id.
func (l *memListing) listed() []entry {
	var out []entry
	for _, e := range l.entries {
		out = append(out, e.entry)
	}
	slices.SortFunc(out, func(a, b entry) int { return strings.Compare(a.id, b.id) })
	return out
}

// handleOf returns the handle of the entry with id, or nil.
func (l *memListing) handleOf(id string) listingHandle {
	for _, e := range l.entries {
		if e.id == id {
			return e
		}
	}
	return nil
}

// expect checks that the listing shows exactly want.
func (l *memListing) expect(want ...entry) {
	l.t.Helper()
	if got := l.listed(); !slices.Equal(got, want) {
		l.t.Fatalf("Local network shows %v, want %v", got, want)
	}
}

// expectSame checks that the entry with id is h, not a new one.
func (l *memListing) expectSame(id string, h listingHandle) {
	l.t.Helper()
	if l.handleOf(id) != h {
		l.t.Fatalf("%s was created anew, want it kept", id)
	}
}
