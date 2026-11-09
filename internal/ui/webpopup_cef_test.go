//go:build webpopup && cef

package ui

import (
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"
)

// These tests drive the cookie-query bookkeeping only. They never call
// CEF: no cef_initialize, no browser, no window, no display. (Linking
// needs libcef; nothing from it runs.)

// results maps each test query to the receiving end of its done channel.
var results = map[*cookieQuery]chan []WebpopupCookie{}

func drain(q *cookieQuery) []WebpopupCookie {
	select {
	case c := <-results[q]:
		return c
	default:
		return nil
	}
}

func newTestQuery() *cookieQuery {
	ch := make(chan []WebpopupCookie, 1)
	q := &cookieQuery{url: cookieURL{"https", "site.test", "/"}, done: ch}
	results[q] = ch
	return q
}

func TestCancelCookieQueryBalancesActiveCount(t *testing.T) {
	base := cefActiveN.Load()
	q := newTestQuery()
	key, cancel, ok := startCookieQuery(q)
	if !ok {
		t.Fatal("token allocation failed")
	}
	if got := cefActiveN.Load(); got != base+1 {
		t.Fatalf("active after start = %d, want %d", got, base+1)
	}
	if !recordCookieVisit(key, cookieVisit{name: "a", value: "1", domain: "site.test", path: "/"}) {
		t.Fatal("visit before cancel must be accepted")
	}
	cancel()
	cancel() // idempotent
	if got := cefActiveN.Load(); got != base {
		t.Fatalf("active after cancel = %d, want %d", got, base)
	}
	// Late C callbacks: the visit stops, completion is a no-op, no result.
	if recordCookieVisit(key, cookieVisit{name: "b"}) {
		t.Fatal("visit after cancel must stop")
	}
	cefCookiesDone(key)
	cefCookiesDone(key)
	if got := cefActiveN.Load(); got != base {
		t.Fatalf("active after late done = %d, want %d", got, base)
	}
	if drain(q) != nil {
		t.Fatal("cancelled query delivered a result")
	}
	if q.visits != nil {
		t.Fatal("cancelled query kept its visits")
	}
}

func TestCookieQueryCompletesOnce(t *testing.T) {
	base := cefActiveN.Load()
	q := newTestQuery()
	key, cancel, ok := startCookieQuery(q)
	if !ok {
		t.Fatal("token allocation failed")
	}
	recordCookieVisit(key, cookieVisit{name: "a", value: "1", domain: "site.test", path: "/"})
	cefCookiesDone(key)
	cefCookiesDone(key) // CEF's release after the last cookie
	got := drain(q)
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("result = %+v", got)
	}
	if drain(q) != nil {
		t.Fatal("second result delivered")
	}
	cancel() // after completion: no-op, no second decrement
	if now := cefActiveN.Load(); now != base {
		t.Fatalf("active = %d, want %d", now, base)
	}
}

// A cancel closure must not remove a newer query that got the same token
// address after the first completed.
func TestCancelDoesNotTouchReusedKey(t *testing.T) {
	base := cefActiveN.Load()
	q1 := newTestQuery()
	key, cancel1, ok := startCookieQuery(q1)
	if !ok {
		t.Fatal("token allocation failed")
	}
	cefCookiesDone(key) // q1 completes; the token address is free for reuse
	q2 := newTestQuery()
	cookieQueries.Store(key, q2) // the address, reused
	cefActiveN.Add(1)
	cancel1()
	if v, ok := cookieQueries.Load(key); !ok || v.(*cookieQuery) != q2 {
		t.Fatal("stale cancel removed the newer query")
	}
	if got := cefActiveN.Load(); got != base+1 {
		t.Fatalf("active = %d, want %d", got, base+1)
	}
	cefCookiesDone(key)
	if got := cefActiveN.Load(); got != base {
		t.Fatalf("active = %d, want %d", got, base)
	}
}

func TestBrowserCreateFailureKeepsActiveCount(t *testing.T) {
	old := cefCreateBrowser
	cefCreateBrowser = func(unsafe.Pointer, string) bool { return false }
	defer func() { cefCreateBrowser = old }()

	base := cefActiveN.Load()
	done := make(chan *WebpopupResult, 2)
	s := wpNewSession("https://site.test/", "", nil, &WebpopupResult{}, done)
	if got := cefActiveN.Load(); got != base {
		t.Fatalf("active = %d, want %d", got, base)
	}
	if len(done) != 1 {
		t.Fatalf("completions = %d, want 1", len(done))
	}
	if r := <-done; r.ResultCode != WebpopupLoadError {
		t.Fatalf("result = %d, want WebpopupLoadError", r.ResultCode)
	}
	s.finish(WebpopupLoadError) // a late source must not complete again
	if len(done) != 0 || cefActiveN.Load() != base {
		t.Fatal("second finish completed or changed the count")
	}
}

func TestLateBrowserCreatedAfterFinishIsClosed(t *testing.T) {
	oldCreate, oldClose := cefCreateBrowser, cefCloseBrowser
	defer func() { cefCreateBrowser, cefCloseBrowser = oldCreate, oldClose }()
	var key unsafe.Pointer
	cefCreateBrowser = func(k unsafe.Pointer, _ string) bool { key = k; return true }
	var closed []unsafe.Pointer
	cefCloseBrowser = func(b unsafe.Pointer) { closed = append(closed, b) }

	base := cefActiveN.Load()
	done := make(chan *WebpopupResult, 2)
	// No websurface glue is installed, so the session finishes at once
	// (WebpopupLoadError) while its browser is still being created.
	wpNewSession("https://site.test/", "", nil, &WebpopupResult{}, done)
	if len(done) != 1 || (<-done).ResultCode != WebpopupLoadError {
		t.Fatal("session did not finish once with WebpopupLoadError")
	}
	if key == nil {
		t.Fatal("browser create was not reached")
	}

	late := unsafe.Pointer(new(int))
	cefOnBrowserCreated(late, key) // CEF reports the browser after finish
	if len(closed) != 1 || closed[0] != late {
		t.Fatalf("closed = %v, want exactly the late browser", closed)
	}
	if len(done) != 0 {
		t.Fatal("session completed a second time")
	}
	if got := cefActiveN.Load(); got != base {
		t.Fatalf("active = %d, want %d", got, base)
	}
}

// ---- per-browser session tokens (several browsers alive at once) ----
//
// fakeCEF stands in for libcef. A create builds the real per-browser C
// client and handlers (cl_new) from the token and keeps CEF's reference to
// the client; callbacks are delivered through those real handlers, and the
// browser's end is the release of that client reference.

type fakeCEF struct {
	keys    []unsafe.Pointer                  // token of each create call, in order
	clients map[unsafe.Pointer]unsafe.Pointer // token -> C client
	closed  []unsafe.Pointer                  // browsers passed to cefCloseBrowser
	failing map[int]bool                      // create call numbers (0-based) that fail
}

func (f *fakeCEF) install(t *testing.T, show bool) {
	oldCreate, oldClose, oldFocus := cefCreateBrowser, cefCloseBrowser, cefFocusBrowser
	oldShow, oldHide := wpSurfaceGlueCef.show, wpSurfaceGlueCef.hide
	t.Cleanup(func() {
		cefCreateBrowser, cefCloseBrowser, cefFocusBrowser = oldCreate, oldClose, oldFocus
		wpSurfaceGlueCef.show, wpSurfaceGlueCef.hide = oldShow, oldHide
	})
	cefCreateBrowser = func(k unsafe.Pointer, _ string) bool {
		n := len(f.keys)
		f.keys = append(f.keys, k)
		if f.failing[n] {
			// libcef consumes the client even when creation fails.
			cefTestClientRelease(cefTestClient(k))
			return false
		}
		if f.clients == nil {
			f.clients = map[unsafe.Pointer]unsafe.Pointer{}
		}
		f.clients[k] = cefTestClient(k)
		return true
	}
	cefCloseBrowser = func(b unsafe.Pointer) { f.closed = append(f.closed, b) }
	cefFocusBrowser = func(unsafe.Pointer) {}
	wpSurfaceGlueCef.show = func(any) bool { return show }
	wpSurfaceGlueCef.hide = func() {}
}

// destroyHandlers — CEF dropping its client (and so the handlers) after the
// browser closed.
func (f *fakeCEF) destroyHandlers(k unsafe.Pointer) {
	cefTestClientRelease(f.clients[k])
	delete(f.clients, k)
}

// afterCreated — OnAfterCreated delivered through the browser's own client.
func (f *fakeCEF) afterCreated(k, browser unsafe.Pointer) {
	cefTestAfterCreated(f.clients[k], browser)
}

func newBrowser() unsafe.Pointer { return cefTestBrowser() }

func startSession(t *testing.T) (*cefSession, chan *WebpopupResult) {
	t.Helper()
	done := make(chan *WebpopupResult, 2)
	s := wpNewSession("https://site.test/", "", nil, &WebpopupResult{}, done)
	return s, done
}

func countClosed(f *fakeCEF, b unsafe.Pointer) int {
	n := 0
	for _, c := range f.closed {
		if c == b {
			n++
		}
	}
	return n
}

// Two sessions are created before either OnAfterCreated arrives: each
// browser binds to its own session, none leaks, each closes exactly once.
func TestTwoSessionsBeforeAfterCreated(t *testing.T) {
	f := &fakeCEF{}
	f.install(t, true)
	freed0 := cefTokensFreed()
	a, _ := startSession(t)
	b, _ := startSession(t)
	if len(f.keys) != 2 || f.keys[0] == f.keys[1] {
		t.Fatalf("keys = %v, want two distinct tokens", f.keys)
	}
	brA, brB := newBrowser(), newBrowser()
	f.afterCreated(f.keys[0], brA) // A's callback arrives after B began
	f.afterCreated(f.keys[1], brB)
	if a.browser != brA || b.browser != brB {
		t.Fatalf("browsers bound A=%v B=%v, want %v %v", a.browser, b.browser, brA, brB)
	}
	if len(f.closed) != 0 {
		t.Fatalf("closed %v before any session finished (leak/early close)", f.closed)
	}
	a.finish(WebpopupClosedByUser)
	b.finish(WebpopupClosedByUser)
	if countClosed(f, brA) != 1 || countClosed(f, brB) != 1 || len(f.closed) != 2 {
		t.Fatalf("closed = %v, want each browser once", f.closed)
	}
	f.destroyHandlers(f.keys[0])
	f.destroyHandlers(f.keys[1])
	if got := cefTokensFreed() - freed0; got != 2 {
		t.Fatalf("tokens freed = %d, want 2", got)
	}
}

// B's create fails while A is live: A keeps its callbacks and token, B's
// token is freed on the spot.
func TestCreateFailureWhileOtherBrowserLive(t *testing.T) {
	f := &fakeCEF{failing: map[int]bool{1: true}}
	f.install(t, true)
	freed0 := cefTokensFreed()
	a, _ := startSession(t)
	brA := newBrowser()
	f.afterCreated(f.keys[0], brA)
	b, doneB := startSession(t)
	if r := <-doneB; r.ResultCode != WebpopupLoadError {
		t.Fatalf("B result = %d, want WebpopupLoadError", r.ResultCode)
	}
	if got := cefTokensFreed() - freed0; got != 1 {
		t.Fatalf("tokens freed after B failed = %d, want 1 (B's)", got)
	}
	if cefSessionOf(f.keys[0]) != a || a.finished || a.browser != brA {
		t.Fatal("A lost its session or browser when B's create failed")
	}
	if cefSessionOf(f.keys[1]) != nil || b.key != nil {
		t.Fatal("B's session still reachable")
	}
	if len(f.closed) != 0 {
		t.Fatalf("closed %v, want none yet", f.closed)
	}
	a.finish(WebpopupClosedByUser)
	f.destroyHandlers(f.keys[0])
	if got := cefTokensFreed() - freed0; got != 2 {
		t.Fatalf("tokens freed = %d, want 2", got)
	}
}

// Close order A,B and B,A: a token is freed once, only after the session
// and every handler have let go of it.
func TestCloseOrdersFreeEachTokenOnce(t *testing.T) {
	for _, order := range [][2]int{{0, 1}, {1, 0}} {
		f := &fakeCEF{}
		f.install(t, true)
		freed0 := cefTokensFreed()
		s := make([]*cefSession, 2)
		br := make([]unsafe.Pointer, 2)
		for i := range s {
			s[i], _ = startSession(t)
		}
		for i := range s {
			br[i] = newBrowser()
			f.afterCreated(f.keys[i], br[i])
		}
		first, second := order[0], order[1]
		s[first].finish(WebpopupClosedByUser)
		if got := cefTokensFreed() - freed0; got != 0 {
			t.Fatalf("order %v: freed %d while handlers still hold the token", order, got)
		}
		if cefSessionOf(f.keys[first]) != nil || cefSessionOf(f.keys[second]) != s[second] {
			t.Fatalf("order %v: wrong sessions mapped after first finish", order)
		}
		f.destroyHandlers(f.keys[first])
		if got := cefTokensFreed() - freed0; got != 1 {
			t.Fatalf("order %v: freed %d after first browser ended, want 1", order, got)
		}
		s[second].finish(WebpopupClosedByUser)
		if got := cefTokensFreed() - freed0; got != 1 {
			t.Fatalf("order %v: second token freed before its handlers", order)
		}
		f.destroyHandlers(f.keys[second])
		if got := cefTokensFreed() - freed0; got != 2 {
			t.Fatalf("order %v: freed %d, want 2", order, got)
		}
		for i := range br {
			if countClosed(f, br[i]) != 1 {
				t.Fatalf("order %v: browser %d closed %d times", order, i, countClosed(f, br[i]))
			}
		}
	}
}

// A finishes before its OnAfterCreated while B has started in between:
// A's late browser is closed, and B neither adopts nor loses anything.
func TestLateBrowserWithAnotherSessionInBetween(t *testing.T) {
	f := &fakeCEF{}
	f.install(t, true)
	a, _ := startSession(t)
	a.finish(WebpopupClosedByUser) // before its browser exists
	b, _ := startSession(t)
	brA, brB := newBrowser(), newBrowser()
	f.afterCreated(f.keys[0], brA)
	if countClosed(f, brA) != 1 || len(f.closed) != 1 {
		t.Fatalf("closed = %v, want exactly A's late browser", f.closed)
	}
	if b.browser != nil {
		t.Fatal("B adopted A's browser")
	}
	f.afterCreated(f.keys[1], brB)
	if b.browser != brB || b.finished {
		t.Fatal("B did not get its own browser")
	}
	b.finish(WebpopupClosedByUser)
	f.destroyHandlers(f.keys[0])
	f.destroyHandlers(f.keys[1])
}

// Frames go to the session of the client they came through, also while a
// second session is alive.
func TestPaintRoutedPerClient(t *testing.T) {
	f := &fakeCEF{}
	f.install(t, true)
	a, _ := startSession(t)
	b, _ := startSession(t)
	brA, brB := newBrowser(), newBrowser()
	f.afterCreated(f.keys[0], brA)
	f.afterCreated(f.keys[1], brB)
	cefTestPaint(f.clients[f.keys[0]], brA, []byte{1, 1, 1, 1}, 1, 1)
	cefTestPaint(f.clients[f.keys[1]], brB, []byte{2, 2, 2, 2, 2, 2, 2, 2}, 2, 1)
	if a.fw != 1 || a.frame[0] != 1 || b.fw != 2 || b.frame[0] != 2 {
		t.Fatalf("frames crossed: A %dx%d %v, B %dx%d %v", a.fw, a.fh, a.frame, b.fw, b.fh, b.frame)
	}
	a.finish(WebpopupClosedByUser)
	b.finish(WebpopupClosedByUser)
	f.destroyHandlers(f.keys[0])
	f.destroyHandlers(f.keys[1])
}

// ---- DevTools read of Partitioned cookies ----

const dtTestOwner = "plugin@local"

// dtSession — a cookie-wait session wired to a stub browser, built without
// cef_parse_url so no libcef function runs. The waiter fires finish
// directly (the real one queues it on the main thread).
func dtSession(t *testing.T, f *fakeCEF) (s *cefSession, browser unsafe.Pointer, done chan *WebpopupResult) {
	t.Helper()
	done = make(chan *WebpopupResult, 2)
	wait := &WebpopupCookieWait{Name: "cf_clearance", target: cookieURL{"https", "site.test", "/"}}
	s = &cefSession{wait: wait, wr: &WebpopupResult{owner: dtTestOwner}, done: done}
	s.ownerGen = cefOwnerCtx.begin(dtTestOwner)
	s.key = cefTokenNew()
	cefSessions.Store(s.key, s)
	f.keys = append(f.keys, s.key)
	if f.clients == nil {
		f.clients = map[unsafe.Pointer]unsafe.Pointer{}
	}
	f.clients[s.key] = cefTestClient(s.key)
	s.waiter = startCookieWaiter(time.Minute, 0, nil, func(code int) { s.finish(code) })
	t.Cleanup(func() { s.waiter.Resolve(WebpopupCookieTimeout) })
	cefActiveN.Add(1) // balanced by finish()
	browser = newBrowser()
	f.afterCreated(s.key, browser)
	s.topURL = "https://site.test/login"
	return s, browser, done
}

// dtKey — the token of the only session this fake created.
func dtKey(f *fakeCEF) unsafe.Pointer { return f.keys[len(f.keys)-1] }

func installDT(t *testing.T) *fakeCEF {
	f := &fakeCEF{}
	f.install(t, true)
	return f
}

func dtResult(mod string) string {
	return `{"cookies":[{"name":"cf_clearance","value":"v1","domain":".site.test","path":"/",` +
		`"expires":` + strconv.FormatInt(time.Now().Unix()+3600, 10) + `,"secure":true,"httpOnly":true,` +
		`"session":false,"partitionKey":` + mod + `}]}`
}

const dtFirstPartyKey = `{"topLevelSite":"https://site.test","hasCrossSiteAncestor":false}`

func TestDevToolsReadCompletesWaitAndHoldsCookie(t *testing.T) {
	f := installDT(t)
	freed0 := cefTokensFreed()
	s, br, done := dtSession(t, f)
	if cefTestDevToolsAttached(br) != 1 || !cefTestDevToolsObserverLive(br) {
		t.Fatal("no DevTools observer registered with the browser")
	}
	if !s.startDevToolsRead(time.Now()) {
		t.Fatal("read not started on the wait origin")
	}
	msg, n := cefTestDevToolsSent(br)
	if n != 1 || msg != `{"id":1,"method":"Network.getCookies","params":{"urls":["https://site.test/"]}}` {
		t.Fatalf("sent %d: %s", n, msg)
	}
	if !cefTestDevToolsDeliver(br, 1, true, dtResult(dtFirstPartyKey)) {
		t.Fatal("observer did not receive the result")
	}
	answerEndWithCookie(t, s, br)
	r := <-done
	if r.ResultCode != WebpopupCookieFound {
		t.Fatalf("result = %d, want WebpopupCookieFound", r.ResultCode)
	}
	// The browser is gone, the owner still gets the cookie for its origin.
	c := cefOwnerCtx.firstPartyFor(dtTestOwner, "https://site.test/", "cf_clearance", time.Now())
	if c == nil || c.Value != "v1" || !c.Secure {
		t.Fatalf("held cookie = %+v", c)
	}
	if cefOwnerCtx.firstPartyFor("other@local", "https://site.test/", "cf_clearance", time.Now()) != nil {
		t.Fatal("another owner can read the held cookie")
	}
	// finish released the registration: CEF's observer reference is gone,
	// no later result is delivered, and the token is freed after the last
	// handler (the client) goes.
	if cefTestDevToolsObserverLive(br) || cefTestDevToolsDeliver(br, 1, true, "{}") {
		t.Fatal("observer still registered after finish")
	}
	if got := cefTokensFreed() - freed0; got != 0 {
		t.Fatalf("token freed %d times while the client lives", got)
	}
	f.destroyHandlers(f.keys[0])
	if got := cefTokensFreed() - freed0; got != 1 {
		t.Fatalf("tokens freed = %d, want 1", got)
	}
}

func TestDevToolsRefusedCookieKeepsWaiting(t *testing.T) {
	for name, key := range map[string]string{
		"cross-site":     `{"topLevelSite":"https://other.test","hasCrossSiteAncestor":false}`,
		"cross-ancestor": `{"topLevelSite":"https://site.test","hasCrossSiteAncestor":true}`,
	} {
		f := installDT(t)
		s, br, done := dtSession(t, f)
		s.startDevToolsRead(time.Now())
		cefTestDevToolsDeliver(br, 1, true, dtResult(key))
		if len(done) != 0 || s.finished || s.fpCookie != nil {
			t.Fatalf("%s: a refused cookie completed the wait", name)
		}
		if cefOwnerCtx.firstPartyFor(dtTestOwner, "https://site.test/", "cf_clearance", time.Now()) != nil {
			t.Fatalf("%s: a refused cookie is held", name)
		}
		// A failed or garbled read also just waits.
		s.startDevToolsRead(time.Now())
		cefTestDevToolsDeliver(br, 2, false, `{"code":-32000}`)
		s.startDevToolsRead(time.Now())
		cefTestDevToolsDeliver(br, 3, true, `not json`)
		if len(done) != 0 || s.finished {
			t.Fatalf("%s: an error completed the wait", name)
		}
		s.finish(WebpopupCookieTimeout)
		if cefOwnerCtx.firstPartyFor(dtTestOwner, "https://site.test/", "cf_clearance", time.Now()) != nil {
			t.Fatalf("%s: a timed-out wait holds a cookie", name)
		}
		f.destroyHandlers(dtKey(f))
	}
}

func TestDevToolsReadOnlyOnWaitOrigin(t *testing.T) {
	f := installDT(t)
	s, br, _ := dtSession(t, f)
	s.topURL = "https://other.test/"
	if s.startDevToolsRead(time.Now()) {
		t.Fatal("read started while the main frame is elsewhere")
	}
	s.topURL = ""
	if s.startDevToolsRead(time.Now()) {
		t.Fatal("read started with no page")
	}
	if _, n := cefTestDevToolsSent(br); n != 0 {
		t.Fatalf("%d messages sent", n)
	}
	s.finish(WebpopupClosedByUser)
	f.destroyHandlers(dtKey(f))
}

func TestDevToolsReadBounded(t *testing.T) {
	f := installDT(t)
	s, br, done := dtSession(t, f)
	t0 := time.Now()
	s.startDevToolsRead(t0)
	// Within the limit the read is in flight and no second one is sent.
	if !s.startDevToolsRead(t0.Add(devtoolsReadLimit - time.Millisecond)) {
		t.Fatal("in-flight read not reported")
	}
	if _, n := cefTestDevToolsSent(br); n != 1 {
		t.Fatalf("%d messages sent within the limit, want 1", n)
	}
	// Unanswered past the limit: a new read is sent, the late answer to the
	// first is ignored and the second one counts.
	s.startDevToolsRead(t0.Add(devtoolsReadLimit))
	if _, n := cefTestDevToolsSent(br); n != 2 {
		t.Fatalf("%d messages sent after the limit, want 2", n)
	}
	cefTestDevToolsDeliver(br, 1, true, dtResult(dtFirstPartyKey))
	if len(done) != 0 || s.finished {
		t.Fatal("a stale result completed the wait")
	}
	cefTestDevToolsDeliver(br, 2, true, dtResult(dtFirstPartyKey))
	answerEnd(t, s, br)
	if r := <-done; r.ResultCode != WebpopupCookieFound {
		t.Fatalf("result = %d", r.ResultCode)
	}
	f.destroyHandlers(dtKey(f))
}

func TestAmbiguousHeldNameIsWithheldFromTheVisitorsCookie(t *testing.T) {
	ch := make(chan []WebpopupCookie, 1)
	q := &cookieQuery{url: cookieURL{"https", "site.test", "/api"}, done: ch, firstPartyAmbiguous: "cf_clearance"}
	key, _, ok := startCookieQuery(q)
	if !ok {
		t.Fatal("token allocation failed")
	}
	recordCookieVisit(key, cookieVisit{name: "cf_clearance", value: "u", domain: "site.test", path: "/"})
	cefCookiesDone(key)
	if got := <-ch; len(got) != 0 {
		t.Fatalf("ambiguous name reached the owner: %+v", got)
	}
}

func TestFirstPartyMergedIntoCookieRead(t *testing.T) {
	ch := make(chan []WebpopupCookie, 1)
	q := &cookieQuery{url: cookieURL{"https", "site.test", "/"}, done: ch,
		firstParty: &WebpopupCookie{Name: "cf_clearance", Value: "v1", Secure: true}}
	key, _, ok := startCookieQuery(q)
	if !ok {
		t.Fatal("token allocation failed")
	}
	recordCookieVisit(key, cookieVisit{name: "bb_guid", value: "g", domain: "site.test", path: "/"})
	cefCookiesDone(key)
	got := <-ch
	if len(got) != 2 || got[1].Name != "cf_clearance" || got[1].Value != "v1" {
		t.Fatalf("read = %+v", got)
	}
}

// A read that passed the owner check, and then met a new popup of the same
// owner, delivers nothing: the new popup invalidated the context.
func TestCookieReadSupersededByNewPopupDeliversNothing(t *testing.T) {
	const owner = "reader@local"
	cefOwnerCtx.record(owner, cefOwnerCtx.begin(owner), "https://site.test/")
	gen, ok := cefOwnerCtx.readAllowedGen(owner, "https://site.test/")
	if !ok {
		t.Fatal("read refused")
	}
	ch := make(chan []WebpopupCookie, 1)
	q := &cookieQuery{url: cookieURL{"https", "site.test", "/"}, done: ch, owner: owner, gen: gen,
		firstParty: &WebpopupCookie{Name: "cf_clearance", Value: "v1", Secure: true}}
	key, _, started := startCookieQuery(q)
	if !started {
		t.Fatal("token allocation failed")
	}
	recordCookieVisit(key, cookieVisit{name: "bb_guid", value: "g", domain: "site.test", path: "/"})
	cefOwnerCtx.begin(owner) // the owner opens another popup meanwhile
	cefCookiesDone(key)
	if got := <-ch; len(got) != 0 {
		t.Fatalf("a superseded read delivered %+v", got)
	}
}

// ---- the visitor's cookie against a Partitioned twin ----

func visitorHit() []cookieVisit {
	return []cookieVisit{{name: "cf_clearance", value: "u", domain: ".site.test", path: "/", secure: true, expires: time.Now().Unix() + 3600}}
}

// answerEnd answers the end-of-popup read of a session that is closing.
func answerEnd(t *testing.T, s *cefSession, br unsafe.Pointer) {
	t.Helper()
	if !s.closing {
		t.Fatal("session is not waiting for its end read")
	}
	cefTestDevToolsDeliver(br, s.dtNextID, true, `{"cookies":[]}`)
}

// answerEndWithCookie answers the end read with an inventory that lists the cookie
// the wait found.
func answerEndWithCookie(t *testing.T, s *cefSession, br unsafe.Pointer) {
	t.Helper()
	if !s.closing {
		t.Fatal("session is not waiting for its end read")
	}
	cefTestDevToolsDeliver(br, s.dtNextID, true, dtResult(dtFirstPartyKey))
}

func finishVisitorRead(s *cefSession, visits []cookieVisit) {
	q := &cookieQuery{url: s.wait.target, session: s}
	s.cookieQuery = q
	s.cookieQueryDone(q, visits)
}

const dtUnpartitionedOnly = `{"cookies":[{"name":"cf_clearance","value":"u","domain":".site.test","path":"/","expires":9999999999,"secure":true}]}`

func dtTwins() string {
	return `{"cookies":[{"name":"cf_clearance","value":"u","domain":".site.test","path":"/","expires":9999999999,"secure":true},` +
		`{"name":"cf_clearance","value":"p","domain":".site.test","path":"/","expires":9999999999,"secure":true,` +
		`"partitionKey":` + dtFirstPartyKey + `}]}`
}

func TestVisitorCookieWaitsForDevToolsAndRefusesATwin(t *testing.T) {
	f := installDT(t)
	s, br, done := dtSession(t, f)
	finishVisitorRead(s, visitorHit())
	if len(done) != 0 {
		t.Fatal("the visitor's cookie completed the wait before DevTools could name a twin")
	}
	if _, n := cefTestDevToolsSent(br); n != 1 {
		t.Fatalf("%d DevTools reads, want 1", n)
	}
	cefTestDevToolsDeliver(br, 1, true, dtTwins())
	if len(done) != 0 || s.finished {
		t.Fatal("a partitioned twin did not stop the wait")
	}
	// Still ambiguous on the next poll: still waiting.
	finishVisitorRead(s, visitorHit())
	cefTestDevToolsDeliver(br, 2, true, dtTwins())
	if len(done) != 0 {
		t.Fatal("the wait completed with a twin present")
	}
	// The twin is gone: the visitor's cookie completes it.
	finishVisitorRead(s, visitorHit())
	cefTestDevToolsDeliver(br, 3, true, dtUnpartitionedOnly)
	answerEnd(t, s, br)
	if r := <-done; r.ResultCode != WebpopupCookieFound {
		t.Fatalf("result = %d", r.ResultCode)
	}
	if cefOwnerCtx.firstPartyFor(dtTestOwner, "https://site.test/", "cf_clearance", time.Now()) != nil {
		t.Fatal("a partitioned cookie is held for a wait the visitor completed")
	}
	f.destroyHandlers(dtKey(f))
}

func TestVisitorCookieCompletesWhenDevToolsFailsOrNeverAnswers(t *testing.T) {
	// An error reply: the visitor's cookie stands.
	f := installDT(t)
	s, br, done := dtSession(t, f)
	finishVisitorRead(s, visitorHit())
	cefTestDevToolsDeliver(br, 1, false, `{"code":-32000}`)
	answerEnd(t, s, br)
	if r := <-done; r.ResultCode != WebpopupCookieFound {
		t.Fatalf("result after an error = %d", r.ResultCode)
	}
	f.destroyHandlers(dtKey(f))

	// No answer within the limit: the next visitor read completes it.
	f = installDT(t)
	s, br, done = dtSession(t, f)
	finishVisitorRead(s, visitorHit())
	s.dtUnanswered = time.Now().Add(-devtoolsReadLimit - time.Second)
	finishVisitorRead(s, visitorHit())
	answerEnd(t, s, br)
	if r := <-done; r.ResultCode != WebpopupCookieFound {
		t.Fatalf("result after the limit = %d", r.ResultCode)
	}
	_ = br
	f.destroyHandlers(dtKey(f))

	// No DevTools observer (registration failed): as before, at once.
	f = installDT(t)
	s, _, done = dtSession(t, f)
	s.dtReg = nil
	finishVisitorRead(s, visitorHit())
	if r := <-done; r.ResultCode != WebpopupCookieFound {
		t.Fatalf("result without DevTools = %d", r.ResultCode)
	}
	f.destroyHandlers(dtKey(f))
}

// ---- the end-of-popup DevTools read (a popup without a wait) ----

func dtEndSession(t *testing.T, f *fakeCEF, owner, top string) (s *cefSession, browser unsafe.Pointer, done chan *WebpopupResult) {
	t.Helper()
	done = make(chan *WebpopupResult, 2)
	s = &cefSession{wr: &WebpopupResult{owner: owner}, done: done}
	s.ownerGen = cefOwnerCtx.begin(owner)
	s.key = cefTokenNew()
	cefSessions.Store(s.key, s)
	f.keys = append(f.keys, s.key)
	if f.clients == nil {
		f.clients = map[unsafe.Pointer]unsafe.Pointer{}
	}
	f.clients[s.key] = cefTestClient(s.key)
	cefActiveN.Add(1)
	browser = newBrowser()
	f.afterCreated(s.key, browser)
	s.topURL = top
	return s, browser, done
}

func TestEndReadHoldsFirstPartyCookiesOfADisplayedPage(t *testing.T) {
	f := installDT(t)
	freed0 := cefTokensFreed()
	s, br, done := dtEndSession(t, f, "refresh@local", "https://site.test/login")
	if cefTestDevToolsAttached(br) != 1 {
		t.Fatal("no observer for a popup of an owner without a wait")
	}
	s.finish(WebpopupClosedByUser) // the user presses Back
	if s.finished || len(done) != 0 {
		t.Fatal("finished before the DevTools read answered")
	}
	msg, n := cefTestDevToolsSent(br)
	if n != 1 || msg != `{"id":1,"method":"Storage.getCookies"}` {
		t.Fatalf("sent %d: %s", n, msg)
	}
	if len(f.closed) != 0 {
		t.Fatal("browser closed before the read answered")
	}
	cefTestDevToolsDeliver(br, 1, true, dtResult(dtFirstPartyKey))
	r := <-done
	if r.ResultCode != WebpopupClosedByUser {
		t.Fatalf("result = %d, want the user close", r.ResultCode)
	}
	if got := cefOwnerCtx.firstPartyFor("refresh@local", "https://site.test/", "cf_clearance", time.Now()); got == nil || got.Value != "v1" {
		t.Fatalf("held = %+v", got)
	}
	if cefOwnerCtx.firstPartyFor("other@local", "https://site.test/", "cf_clearance", time.Now()) != nil {
		t.Fatal("another owner can read it")
	}
	if cefOwnerCtx.firstPartyFor("refresh@local", "https://other.test/", "cf_clearance", time.Now()) != nil {
		t.Fatal("held for another origin")
	}
	if countClosed(f, br) != 1 || cefTestDevToolsObserverLive(br) {
		t.Fatal("browser not closed once / observer still registered")
	}
	f.destroyHandlers(dtKey(f))
	if got := cefTokensFreed() - freed0; got != 1 {
		t.Fatalf("tokens freed = %d", got)
	}
	// A second terminal source after the result changes nothing.
	s.finish(WebpopupTrappedURL)
	if len(done) != 0 || s.wr.ResultCode != WebpopupClosedByUser {
		t.Fatal("a later finish changed the result")
	}
}

func dtStorageAt(path, name string) string {
	return `{"cookies":[{"name":"` + name + `","value":"v` + path + `","domain":".site.test","path":"` + path + `",` +
		`"expires":` + strconv.FormatInt(time.Now().Unix()+3600, 10) + `,"secure":true,"httpOnly":true,` +
		`"session":false,"partitionKey":` + dtFirstPartyKey + `}]}`
}

func TestEndReadHoldsACookieOfAnotherPathThanThePage(t *testing.T) {
	f := installDT(t)
	s, br, done := dtEndSession(t, f, "path@local", "https://site.test/login")
	s.finish(WebpopupClosedByUser)
	cefTestDevToolsDeliver(br, 1, true, dtStorageAt("/api", "cf_clearance"))
	<-done
	get := func(path string) *WebpopupCookie {
		return cefOwnerCtx.firstPartyForURL("path@local", "https://site.test/login",
			cookieURL{"https", "site.test", path}, "cf_clearance", time.Now())
	}
	if got := get("/api/resource"); got == nil || got.Value != "v/api" {
		t.Fatalf("/api/resource = %+v", got)
	}
	if get("/other") != nil || get("/login") != nil {
		t.Fatal("served for a path the cookie does not match")
	}
	f.destroyHandlers(dtKey(f))
}

func TestEndReadKeepsAnotherNameAfterASuccessfulWait(t *testing.T) {
	f := installDT(t)
	s, br, done := dtSession(t, f)
	s.startDevToolsRead(time.Now())
	cefTestDevToolsDeliver(br, 1, true, dtResult(dtFirstPartyKey))
	if s.fpCookie == nil && len(done) == 0 {
		t.Fatal("wait did not find the cookie")
	}
	if !s.closing {
		// The wait finished through finish(); the end read is out.
		t.Fatal("no end read after a successful wait")
	}
	two := `{"cookies":[` + dtStorageAt("/", "cf_clearance")[len(`{"cookies":[`):len(dtStorageAt("/", "cf_clearance"))-2] + `,` +
		dtStorageAt("/", "second")[len(`{"cookies":[`):len(dtStorageAt("/", "second"))-2] + `]}`
	cefTestDevToolsDeliver(br, s.dtNextID, true, two)
	<-done
	for _, n := range []string{"cf_clearance", "second"} {
		if cefOwnerCtx.firstPartyFor(dtTestOwner, "https://site.test/", n, time.Now()) == nil {
			t.Errorf("%s not held", n)
		}
	}
	f.destroyHandlers(dtKey(f))
}

func TestEndReadParsesALargeProfileInventory(t *testing.T) {
	f := installDT(t)
	s, br, done := dtEndSession(t, f, "big@local", "https://site.test/login")
	s.finish(WebpopupClosedByUser)
	one := dtStorageAt("/api", "cf_clearance")
	var pad strings.Builder
	for pad.Len() < 3<<20 { // well over 1 MiB of unrelated cookies
		pad.WriteString(`{"name":"junk","value":"` + strings.Repeat("x", 200) + `","domain":".elsewhere.test","path":"/","expires":9999999999,"secure":true},`)
	}
	cefTestDevToolsDeliver(br, 1, true, `{"cookies":[`+pad.String()+one[len(`{"cookies":[`):])
	<-done
	if cefOwnerCtx.firstPartyForURL("big@local", "https://site.test/", cookieURL{"https", "site.test", "/api"}, "cf_clearance", time.Now()) == nil {
		t.Fatal("cookie lost in a large inventory")
	}
	f.destroyHandlers(dtKey(f))
}

func TestEndInventorySupersedesTheWaitResult(t *testing.T) {
	f := installDT(t)
	s, br, done := dtSession(t, f)
	s.startDevToolsRead(time.Now())
	cefTestDevToolsDeliver(br, 1, true, dtResult(dtFirstPartyKey))
	if s.fpCookie == nil || !s.closing {
		t.Fatal("wait did not find the cookie")
	}
	// The cookie is deleted before the final read: a successful, empty
	// inventory must not bring it back.
	cefTestDevToolsDeliver(br, s.dtNextID, true, `{"cookies":[]}`)
	<-done
	if cefOwnerCtx.firstPartyFor(dtTestOwner, "https://site.test/", "cf_clearance", time.Now()) != nil {
		t.Fatal("a cookie missing from the final inventory was held")
	}
	f.destroyHandlers(dtKey(f))
}

// A final inventory that did not answer says nothing about the cookie the
// wait found: the cookie may be gone, so nothing is held for the popup.
func TestUnavailableEndInventoryHoldsNothing(t *testing.T) {
	for name, answer := range map[string]func(s *cefSession, br unsafe.Pointer){
		"error": func(s *cefSession, br unsafe.Pointer) {
			cefTestDevToolsDeliver(br, s.dtNextID, false, ``)
		},
		"malformed": func(s *cefSession, br unsafe.Pointer) {
			cefTestDevToolsDeliver(br, s.dtNextID, true, `{"cookies":`)
		},
		"timeout": func(s *cefSession, br unsafe.Pointer) { s.endTimeout() },
	} {
		t.Run(name, func(t *testing.T) {
			f := installDT(t)
			s, br, done := dtSession(t, f)
			s.startDevToolsRead(time.Now())
			cefTestDevToolsDeliver(br, 1, true, dtResult(dtFirstPartyKey))
			if s.fpCookie == nil || !s.closing {
				t.Fatal("wait did not find the cookie")
			}
			answer(s, br)
			<-done
			if cefOwnerCtx.firstPartyFor(dtTestOwner, "https://site.test/", "cf_clearance", time.Now()) != nil {
				t.Fatal("the wait result was held although the final inventory was unavailable")
			}
			f.destroyHandlers(dtKey(f))
		})
	}
}

func TestSuccessfulEndInventoryHoldsTheCookieItListsAfterTheWait(t *testing.T) {
	f := installDT(t)
	s, br, done := dtSession(t, f)
	s.startDevToolsRead(time.Now())
	cefTestDevToolsDeliver(br, 1, true, dtResult(dtFirstPartyKey))
	cefTestDevToolsDeliver(br, s.dtNextID, true, dtResult(dtFirstPartyKey))
	<-done
	if cefOwnerCtx.firstPartyFor(dtTestOwner, "https://site.test/", "cf_clearance", time.Now()) == nil {
		t.Fatal("a cookie in the final inventory was not held")
	}
	f.destroyHandlers(dtKey(f))
}

func TestEndReadKeepsAmbiguityAcrossPaths(t *testing.T) {
	f := installDT(t)
	s, br, done := dtEndSession(t, f, "amb@local", "https://site.test/login")
	s.finish(WebpopupClosedByUser)
	a, b := dtStorageAt("/", "cf_clearance"), dtStorageAt("/api", "cf_clearance")
	body := a[:len(a)-2] + `,` + b[len(`{"cookies":[`):]
	cefTestDevToolsDeliver(br, 1, true, body)
	<-done
	get := func(path string) *WebpopupCookie {
		return cefOwnerCtx.firstPartyForURL("amb@local", "https://site.test/", cookieURL{"https", "site.test", path}, "cf_clearance", time.Now())
	}
	if get("/api/x") != nil {
		t.Fatal("two cookies of one name served")
	}
	if got := get("/other"); got == nil || got.Value != "v/" {
		t.Fatalf("/other = %+v", got)
	}
	f.destroyHandlers(dtKey(f))
}

func TestEndReadIsBoundedAndKeepsNothingOnSilence(t *testing.T) {
	f := installDT(t)
	s, br, done := dtEndSession(t, f, "refresh@local", "https://site.test/")
	s.finish(WebpopupClosedByUser)
	if s.finished {
		t.Fatal("finished without an answer or the timeout")
	}
	s.finish(WebpopupTrappedURL) // another source while the read is out
	if s.endCode != WebpopupClosedByUser {
		t.Fatalf("the first result must stand, got %d", s.endCode)
	}
	s.endTimeout() // the bound elapsed, no answer came
	r := <-done
	if r.ResultCode != WebpopupClosedByUser || countClosed(f, br) != 1 {
		t.Fatalf("result %d closed %d", r.ResultCode, countClosed(f, br))
	}
	if cefOwnerCtx.firstPartyFor("refresh@local", "https://site.test/", "cf_clearance", time.Now()) != nil {
		t.Fatal("a silent read holds a cookie")
	}
	// The late answer finds nothing to do.
	if cefTestDevToolsDeliver(br, 1, true, dtResult(dtFirstPartyKey)) {
		t.Fatal("observer still registered after the end")
	}
	f.destroyHandlers(dtKey(f))
}

func TestEndReadRefusesForeignAndCrossSiteCookies(t *testing.T) {
	f := installDT(t)
	s, br, done := dtEndSession(t, f, "refresh@local", "https://site.test/")
	s.finish(WebpopupClosedByUser)
	cefTestDevToolsDeliver(br, 1, true, dtResult(`{"topLevelSite":"https://other.test","hasCrossSiteAncestor":false}`))
	<-done
	if cefOwnerCtx.firstPartyFor("refresh@local", "https://site.test/", "cf_clearance", time.Now()) != nil {
		t.Fatal("a foreign partition was held")
	}
	f.destroyHandlers(dtKey(f))
}

func TestEndReadNeedsAnOwnerAndAnHTTPSPage(t *testing.T) {
	for name, tc := range map[string]struct{ owner, top string }{
		"no owner":    {"", "https://site.test/"},
		"http page":   {"refresh@local", "http://site.test/"},
		"no page yet": {"refresh@local", ""},
	} {
		f := installDT(t)
		s, br, done := dtEndSession(t, f, tc.owner, tc.top)
		s.finish(WebpopupClosedByUser)
		if !s.finished || len(done) != 1 {
			t.Fatalf("%s: did not finish at once", name)
		}
		if _, n := cefTestDevToolsSent(br); n != 0 {
			t.Fatalf("%s: a read was sent", name)
		}
		<-done
		f.destroyHandlers(dtKey(f))
	}
	// A timeout or a load error ends nowhere: no read either.
	f := installDT(t)
	s, br, done := dtEndSession(t, f, "refresh@local", "https://site.test/")
	s.finish(WebpopupLoadError)
	if _, n := cefTestDevToolsSent(br); n != 0 || len(done) != 1 {
		t.Fatal("a failed popup read cookies")
	}
	<-done
	f.destroyHandlers(dtKey(f))
}

// ---- deleting first-party Partitioned cookies through DevTools ----

func dtStorageList() string {
	return `{"cookies":[` +
		`{"name":"cf_clearance","value":"v1","domain":".site.test","path":"/","expires":9999999999,"secure":true,"partitionKey":` + dtFirstPartyKey + `},` +
		`{"name":"cf_clearance","value":"f","domain":".site.test","path":"/","expires":9999999999,"secure":true,"partitionKey":{"topLevelSite":"https://other.test","hasCrossSiteAncestor":false}},` +
		`{"name":"plain","value":"p","domain":".site.test","path":"/","expires":9999999999,"secure":true},` +
		`{"name":"elsewhere","value":"e","domain":".evil.test","path":"/","expires":9999999999,"secure":true,"partitionKey":{"topLevelSite":"https://evil.test","hasCrossSiteAncestor":false}}]}`
}

func startDeleteForTest(t *testing.T, f *fakeCEF, names map[string]bool) (s *cefSession, br unsafe.Pointer, got chan int) {
	t.Helper()
	got = make(chan int, 1)
	s = startDeleteJob("site.test", "/", names, func(n int) { got <- n })
	if s == nil || len(f.keys) == 0 {
		t.Fatal("no helper browser was created")
	}
	br = newBrowser()
	f.afterCreated(f.keys[len(f.keys)-1], br)
	return s, br, got
}

func TestDeleteJobRemovesOnlyFirstPartyPartitionedCookies(t *testing.T) {
	f := installDT(t)
	freed0 := cefTokensFreed()
	gen := cefOwnerCtx.begin("holder@local")
	cefOwnerCtx.recordFirstParty("holder@local", gen, "https://site.test/", time.Now(),
		WebpopupCookie{Name: "cf_clearance", Value: "v1", Domain: ".site.test", Path: "/", Secure: true, Expires: time.Now().Unix() + 3600})
	s, br, got := startDeleteForTest(t, f, map[string]bool{"cf_clearance": true})
	if msg, n := cefTestDevToolsSent(br); n != 1 || msg != `{"id":1,"method":"Storage.getCookies"}` {
		t.Fatalf("first message %d: %s", n, msg)
	}
	cefTestDevToolsDeliver(br, 1, true, dtStorageList())
	msg, n := cefTestDevToolsSent(br)
	want := `{"id":2,"method":"Network.deleteCookies","params":{"name":"cf_clearance","domain":".site.test","path":"/","partitionKey":{"topLevelSite":"https://site.test","hasCrossSiteAncestor":false}}}`
	if n != 2 || msg != want {
		t.Fatalf("delete message %d: %s", n, msg)
	}
	if len(got) != 0 {
		t.Fatal("done before the delete answered")
	}
	cefTestDevToolsDeliver(br, 2, true, `{}`)
	if n := <-got; n != 1 {
		t.Fatalf("deleted = %d, want 1 (foreign, plain and other-host cookies stay)", n)
	}
	if cefOwnerCtx.firstPartyFor("holder@local", "https://site.test/", "cf_clearance", time.Now()) != nil {
		t.Fatal("the hold outlived the deletion")
	}
	if countClosed(f, br) != 1 || cefTestDevToolsObserverLive(br) || !s.finished {
		t.Fatal("helper browser not closed once")
	}
	f.destroyHandlers(dtKey(f))
	if freed := cefTokensFreed() - freed0; freed != 1 {
		t.Fatalf("tokens freed = %d", freed)
	}
}

func TestDeleteJobWithNothingToDeleteEndsAtOnce(t *testing.T) {
	f := installDT(t)
	_, br, got := startDeleteForTest(t, f, map[string]bool{"nothing": true})
	cefTestDevToolsDeliver(br, 1, true, dtStorageList())
	if n := <-got; n != 0 {
		t.Fatalf("deleted = %d", n)
	}
	if _, n := cefTestDevToolsSent(br); n != 1 {
		t.Fatalf("%d messages, want only the listing", n)
	}
	f.destroyHandlers(dtKey(f))
}

func TestDeleteJobIsBounded(t *testing.T) {
	f := installDT(t)
	s, br, got := startDeleteForTest(t, f, nil)
	cefTestDevToolsDeliver(br, 1, true, dtStorageList())
	s.deleteTimeout() // the delete never answered
	if n := <-got; n != 0 {
		t.Fatalf("deleted = %d", n)
	}
	if countClosed(f, br) != 1 {
		t.Fatal("helper browser not closed on the bound")
	}
	s.deleteTimeout() // idempotent
	if len(got) != 0 {
		t.Fatal("second completion")
	}
	f.destroyHandlers(dtKey(f))

	// An error reply from the listing, and a failed create, end with zero.
	f = installDT(t)
	_, br, got = startDeleteForTest(t, f, nil)
	cefTestDevToolsDeliver(br, 1, false, `{"code":-32000}`)
	if n := <-got; n != 0 {
		t.Fatalf("deleted after an error = %d", n)
	}
	f.destroyHandlers(dtKey(f))
	f = &fakeCEF{failing: map[int]bool{0: true}}
	f.install(t, true)
	got = make(chan int, 1)
	startDeleteJob("site.test", "/", nil, func(n int) { got <- n })
	if n := <-got; n != 0 {
		t.Fatalf("deleted after a failed create = %d", n)
	}
}

// A navigation that arrives while the end read is out belongs to a hidden
// popup: it neither moves the context nor the origin the cookies are judged
// for.
func TestEndReadFreezesThePopupURL(t *testing.T) {
	f := installDT(t)
	s, br, done := dtEndSession(t, f, "refresh@local", "https://site.test/login")
	s.finish(WebpopupClosedByUser)
	s.noteURL("https://evil.test/landing", true) // after the user closed it
	cefTestDevToolsDeliver(br, 1, true, dtResult(dtFirstPartyKey))
	<-done
	if cefOwnerCtx.firstPartyFor("refresh@local", "https://site.test/", "cf_clearance", time.Now()) == nil {
		t.Fatal("the cookie of the page the user closed was lost")
	}
	if !cefOwnerCtx.readAllowed("refresh@local", "https://site.test/") ||
		cefOwnerCtx.readAllowed("refresh@local", "https://evil.test/") {
		t.Fatal("the context follows a navigation made after the close")
	}
	f.destroyHandlers(dtKey(f))
}
