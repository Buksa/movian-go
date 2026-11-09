package ui

import (
	"testing"
	"time"
)

var dtNow = time.Unix(1_800_000_000, 0)

const dtFirstParty = `{"cookies":[{"name":"cf_clearance","value":"v1","domain":".site.test",
"path":"/","expires":1800003600.5,"size":40,"httpOnly":true,"secure":true,"session":false,
"sameSite":"None","priority":"Medium","sourceScheme":"Secure","sourcePort":443,
"partitionKey":{"topLevelSite":"https://site.test","hasCrossSiteAncestor":false}}]}`

func TestParseDevToolsCookies(t *testing.T) {
	got, err := parseDevToolsCookies([]byte(dtFirstParty))
	if err != nil || len(got) != 1 {
		t.Fatalf("parse: %v %+v", err, got)
	}
	c := got[0]
	if c.Name != "cf_clearance" || c.Value != "v1" || c.Domain != ".site.test" ||
		c.Path != "/" || !c.Secure || !c.HTTPOnly || c.Expires != 1800003600.5 {
		t.Fatalf("fields = %+v", c)
	}
	if c.PartitionKey == nil || c.PartitionKey.TopLevelSite != "https://site.test" ||
		c.PartitionKey.HasCrossSiteAncestor {
		t.Fatalf("partition = %+v", c.PartitionKey)
	}
}

func TestParseDevToolsCookiesShapes(t *testing.T) {
	// No cookies, an empty result and a missing key are an empty list.
	for _, in := range []string{`{"cookies":[]}`, `{}`} {
		got, err := parseDevToolsCookies([]byte(in))
		if err != nil || len(got) != 0 {
			t.Fatalf("%s: %v %+v", in, err, got)
		}
	}
	// Unpartitioned: no partition key at all.
	got, err := parseDevToolsCookies([]byte(`{"cookies":[{"name":"a","value":"1","domain":"h","path":"/"}]}`))
	if err != nil || len(got) != 1 || got[0].PartitionKey != nil {
		t.Fatalf("unpartitioned: %v %+v", err, got)
	}
	// An absent hasCrossSiteAncestor is false.
	got, _ = parseDevToolsCookies([]byte(`{"cookies":[{"name":"a","partitionKey":{"topLevelSite":"https://x.test"}}]}`))
	if len(got) != 1 || got[0].PartitionKey == nil || got[0].PartitionKey.HasCrossSiteAncestor {
		t.Fatalf("absent ancestor flag: %+v", got)
	}
	// The pre-object string form cannot prove the ancestor chain: kept as a
	// key that the selector refuses.
	got, _ = parseDevToolsCookies([]byte(`{"cookies":[{"name":"a","partitionKey":"https://x.test"}]}`))
	if len(got) != 1 || got[0].PartitionKey == nil || !got[0].PartitionKey.Legacy {
		t.Fatalf("legacy string key: %+v", got)
	}
	for _, bad := range []string{``, `not json`, `{"cookies":{}}`, `{"cookies":[1]}`} {
		if _, err := parseDevToolsCookies([]byte(bad)); err == nil {
			t.Fatalf("%q parsed without error", bad)
		}
	}
}

func dtCookie(mod func(*devtoolsCookie)) devtoolsCookie {
	c := devtoolsCookie{
		Name: "cf_clearance", Value: "v1", Domain: ".site.test", Path: "/",
		Expires: float64(dtNow.Unix() + 3600), Secure: true, HTTPOnly: true,
		PartitionKey: &devtoolsPartition{TopLevelSite: "https://site.test"},
	}
	if mod != nil {
		mod(&c)
	}
	return c
}

func selectDT(list []devtoolsCookie) (*WebpopupCookie, string) {
	return selectFirstPartyCookie(list, cookieURL{"https", "site.test", "/login"},
		"https://site.test", "https://site.test", "cf_clearance", dtNow)
}

func TestSelectFirstPartyAccepts(t *testing.T) {
	c, reason := selectDT([]devtoolsCookie{dtCookie(nil)})
	if c == nil {
		t.Fatalf("refused: %s", reason)
	}
	if c.Name != "cf_clearance" || c.Value != "v1" || c.Domain != ".site.test" ||
		c.Path != "/" || !c.Secure || !c.HTTPOnly || c.Expires != dtNow.Unix()+3600 {
		t.Fatalf("cookie = %+v", c)
	}
	// A sibling host of the same site is still the first party.
	c, reason = selectFirstPartyCookie([]devtoolsCookie{dtCookie(nil)},
		cookieURL{"https", "forum.site.test", "/"},
		"https://forum.site.test", "https://forum.site.test", "cf_clearance", dtNow)
	if c == nil {
		t.Fatalf("subdomain target refused: %s", reason)
	}
}

func TestSelectFirstPartyRefuses(t *testing.T) {
	cases := []struct {
		name   string
		list   []devtoolsCookie
		reason string
	}{
		{"cross-site top level", []devtoolsCookie{dtCookie(func(c *devtoolsCookie) {
			c.PartitionKey.TopLevelSite = "https://other.test"
		})}, "cross-site-partition"},
		{"cross-site ancestor", []devtoolsCookie{dtCookie(func(c *devtoolsCookie) {
			c.PartitionKey.HasCrossSiteAncestor = true
		})}, "cross-site-partition"},
		{"legacy string key", []devtoolsCookie{dtCookie(func(c *devtoolsCookie) {
			c.PartitionKey = &devtoolsPartition{TopLevelSite: "https://site.test", Legacy: true}
		})}, "cross-site-partition"},
		{"opaque key", []devtoolsCookie{dtCookie(func(c *devtoolsCookie) { c.PartitionKeyOpaque = true })}, "cross-site-partition"},
		{"top-level site with other scheme", []devtoolsCookie{dtCookie(func(c *devtoolsCookie) {
			c.PartitionKey.TopLevelSite = "http://site.test"
		})}, "cross-site-partition"},
		{"not secure", []devtoolsCookie{dtCookie(func(c *devtoolsCookie) { c.Secure = false })}, "secure-required"},
		{"session cookie", []devtoolsCookie{dtCookie(func(c *devtoolsCookie) { c.Expires = -1; c.Session = true })}, "expires-not-in-future"},
		{"expired", []devtoolsCookie{dtCookie(func(c *devtoolsCookie) { c.Expires = float64(dtNow.Unix() - 1) })}, "expires-not-in-future"},
		{"empty value", []devtoolsCookie{dtCookie(func(c *devtoolsCookie) { c.Value = "" })}, "empty-value"},
		{"other name", []devtoolsCookie{dtCookie(func(c *devtoolsCookie) { c.Name = "other" })}, "no-matching-cookie"},
		{"other host", []devtoolsCookie{dtCookie(func(c *devtoolsCookie) { c.Domain = "evil.test" })}, "no-matching-cookie"},
		{"other path", []devtoolsCookie{dtCookie(func(c *devtoolsCookie) { c.Path = "/admin" })}, "no-matching-cookie"},
		{"empty", nil, "no-matching-cookie"},
		{"unpartitioned only", []devtoolsCookie{dtCookie(func(c *devtoolsCookie) { c.PartitionKey = nil })}, "not-partitioned"},
		{"unpartitioned twin", []devtoolsCookie{dtCookie(nil), dtCookie(func(c *devtoolsCookie) { c.PartitionKey = nil; c.Value = "v2" })}, "ambiguous-cookie"},
		{"two partitioned", []devtoolsCookie{dtCookie(nil), dtCookie(func(c *devtoolsCookie) { c.Path = "/login"; c.Value = "v2" })}, "ambiguous-cookie"},
		{"first party plus foreign partition", []devtoolsCookie{dtCookie(nil), dtCookie(func(c *devtoolsCookie) {
			c.PartitionKey.TopLevelSite = "https://other.test"
		})}, "ambiguous-cookie"},
	}
	for _, tc := range cases {
		c, reason := selectDT(tc.list)
		if c != nil || reason != tc.reason {
			t.Errorf("%s: got %+v, %q; want refusal %q", tc.name, c, reason, tc.reason)
		}
	}
}

func TestSelectFirstPartyOrigins(t *testing.T) {
	list := []devtoolsCookie{dtCookie(nil)}
	target := cookieURL{"https", "site.test", "/"}
	for _, tc := range []struct{ name, targetOrigin, top string }{
		{"popup ended elsewhere", "https://site.test", "https://other.test"},
		{"popup ended nowhere", "https://site.test", ""},
		{"popup ended on another port", "https://site.test", "https://site.test:8443"},
		{"popup ended on http", "https://site.test", "http://site.test"},
		{"no target origin", "", ""},
	} {
		c, reason := selectFirstPartyCookie(list, target, tc.targetOrigin, tc.top, "cf_clearance", dtNow)
		if c != nil || reason != "origin-mismatch" {
			t.Errorf("%s: got %+v, %q", tc.name, c, reason)
		}
	}
}

func TestFirstPartySite(t *testing.T) {
	for host, want := range map[string]string{
		"rutracker.org":       "https://rutracker.org",
		"forum.rutracker.org": "https://rutracker.org",
		"a.b.example.co.uk":   "https://example.co.uk",
		"localhost":           "https://localhost",
		"127.0.0.1":           "https://127.0.0.1",
		"[::1]":               "https://[::1]",
	} {
		if got := firstPartySiteOf(host); got != want {
			t.Errorf("%s: %q, want %q", host, got, want)
		}
	}
}

func TestOwnerContextFirstParty(t *testing.T) {
	var c popupOwnerContexts
	cookie := &WebpopupCookie{Name: "cf_clearance", Value: "v1", Domain: ".site.test", Path: "/",
		Secure: true, Expires: dtNow.Unix() + 3600}
	gen := c.begin("a@local")
	c.recordFirstParty("a@local", gen, "https://site.test/login", dtNow, *cookie)

	if got := c.firstPartyFor("a@local", "https://site.test/x", "cf_clearance", dtNow.Add(time.Second)); got == nil || got.Value != "v1" {
		t.Fatalf("owner read = %+v", got)
	}
	// Another owner, another origin, another name, another scheme.
	for name, got := range map[string]*WebpopupCookie{
		"other owner":  c.firstPartyFor("b@local", "https://site.test/", "cf_clearance", dtNow),
		"other origin": c.firstPartyFor("a@local", "https://other.test/", "cf_clearance", dtNow),
		"other port":   c.firstPartyFor("a@local", "https://site.test:8443/", "cf_clearance", dtNow),
		"other name":   c.firstPartyFor("a@local", "https://site.test/", "bb_guid", dtNow),
		"http":         c.firstPartyFor("a@local", "http://site.test/", "cf_clearance", dtNow),
		"no owner":     c.firstPartyFor("", "https://site.test/", "cf_clearance", dtNow),
		"too late":     c.firstPartyFor("a@local", "https://site.test/", "cf_clearance", dtNow.Add(firstPartyHoldTTL+time.Second)),
		"expired":      c.firstPartyFor("a@local", "https://site.test/", "cf_clearance", time.Unix(cookie.Expires+1, 0)),
	} {
		if got != nil {
			t.Errorf("%s: handed out %+v", name, got)
		}
	}
	// The caller cannot change the stored cookie.
	got := c.firstPartyFor("a@local", "https://site.test/", "cf_clearance", dtNow)
	got.Value = "changed"
	if again := c.firstPartyFor("a@local", "https://site.test/", "cf_clearance", dtNow); again.Value != "v1" {
		t.Fatal("stored cookie was aliased")
	}
	// A new popup of the owner drops it, and so does a superseded recorder.
	c.begin("a@local")
	if c.firstPartyFor("a@local", "https://site.test/", "cf_clearance", dtNow) != nil {
		t.Fatal("kept across a new popup")
	}
	old := c.begin("a@local")
	c.begin("a@local")
	c.recordFirstParty("a@local", old, "https://site.test/", dtNow, *cookie)
	if c.firstPartyFor("a@local", "https://site.test/", "cf_clearance", dtNow) != nil {
		t.Fatal("a superseded popup recorded a cookie")
	}
}

func TestOwnerContextFirstPartyNeedsMatchingEnd(t *testing.T) {
	cookie := &WebpopupCookie{Name: "cf_clearance", Value: "v1", Domain: ".site.test", Path: "/",
		Secure: true, Expires: dtNow.Unix() + 3600}
	var c popupOwnerContexts
	gen := c.begin("a@local")
	// The popup ended on another origin than the cookie's own: nothing.
	c.recordFirstParty("a@local", gen, "https://other.test/", dtNow, *cookie)
	if c.firstPartyFor("a@local", "https://site.test/", "cf_clearance", dtNow) != nil ||
		c.firstPartyFor("a@local", "https://other.test/", "cf_clearance", dtNow) != nil {
		t.Fatal("cookie kept for a popup that ended on another origin")
	}
	// A popup that ended nowhere records nothing either.
	gen = c.begin("a@local")
	c.recordFirstParty("a@local", gen, "", dtNow, *cookie)
	if c.firstPartyFor("a@local", "https://site.test/", "cf_clearance", dtNow) != nil {
		t.Fatal("cookie kept for a popup that ended nowhere")
	}
}

func TestMergeFirstPartyWithholdsAnAmbiguousHeldName(t *testing.T) {
	u := cookieURL{"https", "site.test", "/api"}
	visits := []cookieVisit{
		{name: "cf_clearance", value: "x", domain: "site.test", path: "/"},
		{name: "bb_guid", value: "g", domain: "site.test", path: "/"}}
	got, amb := mergeFirstParty(visits, u, nil, "cf_clearance")
	if amb != 1 || len(got) != 1 || got[0].Name != "bb_guid" {
		t.Fatalf("merge = %+v %d", got, amb)
	}
}

func TestHoldReportsAmbiguity(t *testing.T) {
	var c popupOwnerContexts
	plain := WebpopupCookie{Name: "cf_clearance", Domain: ".site.test", Path: "/", Secure: true}
	holdOf(&c, []WebpopupCookie{fpCk("cf_clearance", "/api")}, []WebpopupCookie{plain})
	hit, amb := c.firstPartyLookup("a@local", "https://site.test/", cookieURL{"https", "site.test", "/api/x"}, "cf_clearance", dtNow)
	if hit != nil || !amb {
		t.Fatalf("hit=%+v amb=%v", hit, amb)
	}
	// Nothing held for the name or one cookie only: not ambiguous.
	if _, amb := c.firstPartyLookup("a@local", "https://site.test/", cookieURL{"https", "site.test", "/api/x"}, "other", dtNow); amb {
		t.Fatal("unknown name ambiguous")
	}
}

func TestMergeFirstParty(t *testing.T) {
	u := cookieURL{"https", "site.test", "/"}
	hold := &WebpopupCookie{Name: "cf_clearance", Value: "v1"}
	got, amb := mergeFirstParty([]cookieVisit{{name: "bb_guid", value: "g", domain: "site.test", path: "/"}}, u, hold, "")
	if amb != 0 || len(got) != 2 || got[1].Name != "cf_clearance" {
		t.Fatalf("merge = %+v %d", got, amb)
	}
	// An unpartitioned cookie of the same name leaves no safe choice.
	got, amb = mergeFirstParty([]cookieVisit{
		{name: "cf_clearance", value: "x", domain: "site.test", path: "/"},
		{name: "bb_guid", domain: "site.test", path: "/"}}, u, hold, "")
	if amb != 1 || len(got) != 1 || got[0].Name != "bb_guid" {
		t.Fatalf("twin merge = %+v %d", got, amb)
	}
	// Twins the jar gained after the wait, which scopeCookies already
	// withholds as ambiguous (host-only plus domain cookie), still block it.
	got, amb = mergeFirstParty([]cookieVisit{
		{name: "cf_clearance", value: "x", domain: "site.test", path: "/"},
		{name: "cf_clearance", value: "y", domain: ".site.test", path: "/"}}, u, hold, "")
	if len(got) != 0 || amb < 1 {
		t.Fatalf("late twins merged: %+v %d", got, amb)
	}
	// A visit that the URL does not match is no twin.
	got, amb = mergeFirstParty([]cookieVisit{{name: "cf_clearance", domain: "other.test", path: "/"}}, u, hold, "")
	if amb != 0 || len(got) != 1 || got[0].Value != "v1" {
		t.Fatalf("unrelated visit blocked the cookie: %+v %d", got, amb)
	}
	if got, amb = mergeFirstParty(nil, u, nil, ""); got == nil || len(got) != 0 || amb != 0 {
		t.Fatalf("nil merge = %#v", got)
	}
}

func TestFirstPartyHoldNeedsMatchingRequestPath(t *testing.T) {
	var c popupOwnerContexts
	cookie := &WebpopupCookie{Name: "cf_clearance", Value: "v1", Domain: ".site.test", Path: "/admin",
		Secure: true, Expires: dtNow.Unix() + 3600}
	gen := c.begin("a@local")
	c.recordFirstParty("a@local", gen, "https://site.test/admin/login", dtNow, *cookie)
	if c.firstPartyFor("a@local", "https://site.test/admin/x", "cf_clearance", dtNow) == nil {
		t.Fatal("cookie not handed out for a path it covers")
	}
	for _, u := range []string{"https://site.test/public", "https://site.test/", "https://site.test/administrator"} {
		if c.firstPartyFor("a@local", u, "cf_clearance", dtNow) != nil {
			t.Errorf("cookie of /admin handed out for %s", u)
		}
	}
}

func TestDropFirstParty(t *testing.T) {
	var c popupOwnerContexts
	mk := func(name string) *WebpopupCookie {
		return &WebpopupCookie{Name: name, Value: "v", Domain: ".site.test", Path: "/", Secure: true, Expires: dtNow.Unix() + 3600}
	}
	hold := func(owner string, ck *WebpopupCookie) {
		c.recordFirstParty(owner, c.begin(owner), "https://site.test/", dtNow, *ck)
	}
	hold("a@local", mk("cf_clearance"))
	hold("b@local", mk("cf_clearance"))
	hold("c@local", mk("other"))
	// Deleting another name, host or path keeps the held cookies.
	c.dropFirstParty("site.test", "/", map[string]bool{"unrelated": true})
	c.dropFirstParty("elsewhere.test", "/", nil)
	if c.firstPartyFor("a@local", "https://site.test/", "cf_clearance", dtNow) == nil {
		t.Fatal("an unrelated deletion dropped the cookie")
	}
	// A deletion by name drops it for every owner, and only that name.
	c.dropFirstParty("forum.site.test", "/", map[string]bool{"cf_clearance": true})
	if c.firstPartyFor("a@local", "https://site.test/", "cf_clearance", dtNow) != nil ||
		c.firstPartyFor("b@local", "https://site.test/", "cf_clearance", dtNow) != nil {
		t.Fatal("deleted cookie still held")
	}
	if c.firstPartyFor("c@local", "https://site.test/", "other", dtNow) == nil {
		t.Fatal("another name was dropped")
	}
	// No name filter drops everything on the host.
	c.dropFirstParty("site.test", "", nil)
	if c.firstPartyFor("c@local", "https://site.test/", "other", dtNow) != nil {
		t.Fatal("unfiltered deletion kept a cookie")
	}
}

func TestCookieDeleteMatches(t *testing.T) {
	names := map[string]bool{"a": true}
	for _, tc := range []struct {
		dom, path, name, host, dpath string
		names                        map[string]bool
		want                         bool
	}{
		{".site.test", "/", "a", "site.test", "/", names, true},
		{"site.test", "/", "a", "forum.site.test", "/", names, true}, // as the deletion always matched
		{".site.test", "/", "a", "forum.site.test", "/x", names, true},
		{".site.test", "/", "b", "site.test", "/", names, false},
		{".site.test", "/", "b", "site.test", "/", nil, true},
		{".site.test", "/admin", "a", "site.test", "/", names, false},
		{".site.test", "/", "a", "other.test", "/", names, false},
		{".site.test", "/", "a", "evilsite.test", "/", names, false},
	} {
		if got := cookieDeleteMatches(tc.dom, tc.path, tc.name, tc.host, tc.dpath, tc.names); got != tc.want {
			t.Errorf("%+v = %v", tc, got)
		}
	}
}

func TestDevToolsReadIsBounded(t *testing.T) {
	var r devtoolsRead
	t0 := time.Unix(100, 0)
	if r.busy(t0) {
		t.Fatal("idle read reports busy")
	}
	r.begin(7, t0)
	if !r.busy(t0.Add(devtoolsReadLimit - time.Millisecond)) {
		t.Fatal("read in flight is not busy")
	}
	// Never answered (agent detached): the next trigger may start another.
	if r.busy(t0.Add(devtoolsReadLimit)) {
		t.Fatal("an unanswered read blocks forever")
	}
	r.begin(8, t0.Add(devtoolsReadLimit))
	// The late answer of the abandoned read is ignored; the live one counts once.
	if r.take(7) {
		t.Fatal("stale result accepted")
	}
	if !r.take(8) || r.take(8) {
		t.Fatal("live result must be accepted exactly once")
	}
	if r.busy(t0.Add(devtoolsReadLimit)) {
		t.Fatal("answered read still busy")
	}
	// A result with no read in flight is ignored.
	if r.take(0) {
		t.Fatal("zero id accepted")
	}
}

func TestDevToolsRequest(t *testing.T) {
	got := devtoolsCookiesRequest(9, "https://site.test/login?x=1")
	want := `{"id":9,"method":"Network.getCookies","params":{"urls":["https://site.test/login?x=1"]}}`
	if got != want {
		t.Fatalf("request = %s", got)
	}
	// A quote in the URL cannot break out of the JSON string.
	if got := devtoolsCookiesRequest(1, `https://a.test/"},{"x":"`); got != `{"id":1,"method":"Network.getCookies","params":{"urls":["https://a.test/\"},{\"x\":\""]}}` {
		t.Fatalf("unescaped: %s", got)
	}
}

func TestReadAllowedGenSupersededByNewPopup(t *testing.T) {
	var c popupOwnerContexts
	c.record("a@local", c.begin("a@local"), "https://site.test/")
	gen, ok := c.readAllowedGen("a@local", "https://site.test/")
	if !ok || !c.stillCurrent("a@local", gen) {
		t.Fatalf("read refused or generation not current: %v", ok)
	}
	c.begin("a@local") // a new popup of the owner while the read ran
	if c.stillCurrent("a@local", gen) {
		t.Fatal("a read from before a new popup is still current")
	}
	if _, ok := c.readAllowedGen("a@local", "https://site.test/"); ok {
		t.Fatal("read allowed while the new popup has no context")
	}
	// The ownerless legacy read has no generation to supersede.
	g, ok := c.readAllowedGen("", "https://site.test/")
	if !ok || !c.stillCurrent("", g) {
		t.Fatal("ownerless read refused")
	}
}

func TestFirstPartyInventoryKeepsEveryPath(t *testing.T) {
	list := []devtoolsCookie{
		dtCookie(func(c *devtoolsCookie) { c.Path = "/api" }), // another path than the page
		dtCookie(func(c *devtoolsCookie) { c.Name = "other_fp" }),
		dtCookie(func(c *devtoolsCookie) { c.Name = "foreign"; c.PartitionKey.TopLevelSite = "https://x.test" }),
		dtCookie(func(c *devtoolsCookie) { c.Name = "plain"; c.PartitionKey = nil }),
		dtCookie(func(c *devtoolsCookie) { c.Name = "opaque"; c.PartitionKey = nil; c.PartitionKeyOpaque = true }),
		dtCookie(func(c *devtoolsCookie) { c.Name = "insecure"; c.Secure = false }),
		dtCookie(func(c *devtoolsCookie) { c.Name = "session"; c.Session = true; c.Expires = -1 }),
		dtCookie(func(c *devtoolsCookie) { c.Name = "elsewhere"; c.Domain = "evil.test" }),
		dtCookie(func(c *devtoolsCookie) {
			c.Name = "otherhost"
			c.Domain = "sub.site.test"
			c.PartitionKey.TopLevelSite = "https://site.test"
		}),
	}
	elig, block := selectFirstPartyInventory(list, "https://site.test", dtNow)
	names := map[string]string{}
	for _, c := range elig {
		names[c.Name] = c.Path
	}
	if len(elig) != 2 || names["cf_clearance"] != "/api" || names["other_fp"] != "/" {
		t.Fatalf("eligible %+v", elig)
	}
	// Ineligible cookies the browser would send to the origin are kept as
	// blockers, so a twin on another path stays visible; foreign hosts are not.
	bn := map[string]bool{}
	for _, c := range block {
		bn[c.Name] = true
		if c.foreign != (c.Name == "foreign" || c.Name == "opaque") {
			t.Fatalf("%s: foreign = %v", c.Name, c.foreign)
		}
		if c.Value != "" {
			t.Fatalf("blocker carries a value: %+v", c)
		}
	}
	for _, n := range []string{"foreign", "opaque", "plain", "insecure", "session"} {
		if !bn[n] {
			t.Errorf("%s not a blocker", n)
		}
	}
	if bn["elsewhere"] || bn["otherhost"] || len(block) != 5 {
		t.Fatalf("blockers %+v", block)
	}
	if e, b := selectFirstPartyInventory(list, "", dtNow); len(e)+len(b) != 0 {
		t.Fatal("selected for no origin")
	}
}

func holdOf(c *popupOwnerContexts, cookies, blockers []WebpopupCookie) {
	var bl []fpBlocker
	for _, b := range blockers {
		bl = append(bl, fpBlocker{WebpopupCookie: b})
	}
	holdOfBlockers(c, cookies, bl)
}

func holdOfBlockers(c *popupOwnerContexts, cookies []WebpopupCookie, blockers []fpBlocker) {
	c.recordFirstPartySet("a@local", c.begin("a@local"), "https://site.test/login", dtNow,
		firstPartySet{cookies: cookies, blockers: blockers})
}

func fpCk(name, path string) WebpopupCookie {
	return WebpopupCookie{Name: name, Value: "v" + path, Domain: ".site.test", Path: path, Secure: true, Expires: dtNow.Unix() + 3600}
}

func TestHoldServesACookieOfAnotherPathByRequestPath(t *testing.T) {
	var c popupOwnerContexts
	holdOf(&c, []WebpopupCookie{fpCk("cf_clearance", "/api")}, nil)
	get := func(path string) *WebpopupCookie {
		return c.firstPartyForURL("a@local", "https://site.test/login",
			cookieURL{"https", "site.test", path}, "cf_clearance", dtNow)
	}
	if got := get("/api/resource"); got == nil || got.Value != "v/api" {
		t.Fatalf("/api/resource = %+v", got)
	}
	if got := get("/api"); got == nil {
		t.Fatal("/api not served")
	}
	if get("/other") != nil || get("/login") != nil || get("/apix") != nil {
		t.Fatal("served for a path the cookie does not match")
	}
}

func TestHoldRefusesAmbiguityAcrossPaths(t *testing.T) {
	get := func(c *popupOwnerContexts, path string) *WebpopupCookie {
		return c.firstPartyForURL("a@local", "https://site.test/login",
			cookieURL{"https", "site.test", path}, "cf_clearance", dtNow)
	}
	// Two eligible cookies of one name: both match /api/x, only one /other.
	var two popupOwnerContexts
	holdOf(&two, []WebpopupCookie{fpCk("cf_clearance", "/"), fpCk("cf_clearance", "/api")}, nil)
	if get(&two, "/api/x") != nil {
		t.Fatal("two matching cookies of one name served")
	}
	if got := get(&two, "/other"); got == nil || got.Path != "/" {
		t.Fatalf("/other = %+v", got)
	}
	// An ineligible twin (unpartitioned, cross-site partition) on a path the
	// request matches makes it ambiguous; on another path it does not.
	var tw popupOwnerContexts
	blocker := WebpopupCookie{Name: "cf_clearance", Domain: ".site.test", Path: "/api", Secure: true}
	holdOf(&tw, []WebpopupCookie{fpCk("cf_clearance", "/")}, []WebpopupCookie{blocker})
	if get(&tw, "/api/x") != nil {
		t.Fatal("served next to a twin that the request also matches")
	}
	if got := get(&tw, "/other"); got == nil {
		t.Fatal("a twin on another path blocked the request")
	}
	// A twin with the very identity of the eligible cookie (another
	// partition) is a cookie of its own: still ambiguous.
	var same popupOwnerContexts
	holdOf(&same, []WebpopupCookie{fpCk("cf_clearance", "/api")},
		[]WebpopupCookie{{Name: "cf_clearance", Domain: ".site.test", Path: "/api", Secure: true}})
	if get(&same, "/api/x") != nil {
		t.Fatal("served next to a twin of the same identity")
	}
	// An expired blocker is gone.
	var ex popupOwnerContexts
	old := blocker
	old.Expires = dtNow.Unix() - 10
	holdOf(&ex, []WebpopupCookie{fpCk("cf_clearance", "/")}, []WebpopupCookie{old})
	if get(&ex, "/api/x") == nil {
		t.Fatal("an expired twin still blocks")
	}
}

func TestDropFirstPartyRemovesTheDeletedTwinsToo(t *testing.T) {
	var c popupOwnerContexts
	plain := WebpopupCookie{Name: "cf_clearance", Domain: ".site.test", Path: "/", Secure: true}
	foreign := fpBlocker{WebpopupCookie: WebpopupCookie{Name: "cf_clearance", Domain: ".site.test", Path: "/", Secure: true}, foreign: true}
	get := func() *WebpopupCookie {
		return c.firstPartyForURL("a@local", "https://site.test/", cookieURL{"https", "site.test", "/api"}, "cf_clearance", dtNow)
	}
	// Deleting the unpartitioned twin at / frees the cookie at /api.
	holdOf(&c, []WebpopupCookie{fpCk("cf_clearance", "/api")}, []WebpopupCookie{plain})
	if get() != nil {
		t.Fatal("served next to the twin")
	}
	c.dropFirstParty("site.test", "/", map[string]bool{"cf_clearance": true})
	if get() == nil {
		t.Fatal("deleted twin still blocks")
	}
	// A cross-site partition survives a deletion, so it keeps blocking.
	holdOfBlockers(&c, []WebpopupCookie{fpCk("cf_clearance", "/api")}, []fpBlocker{foreign})
	c.dropFirstParty("site.test", "/", map[string]bool{"cf_clearance": true})
	if get() != nil {
		t.Fatal("a surviving foreign partition stopped blocking")
	}
}

func TestDropFirstPartyRemovesAnyPathOfTheName(t *testing.T) {
	var c popupOwnerContexts
	holdOf(&c, []WebpopupCookie{fpCk("cf_clearance", "/api"), fpCk("keep", "/api")}, nil)
	c.dropFirstParty("site.test", "/api", map[string]bool{"cf_clearance": true})
	if c.firstPartyForURL("a@local", "https://site.test/", cookieURL{"https", "site.test", "/api"}, "cf_clearance", dtNow) != nil {
		t.Fatal("deleted cookie still held")
	}
	if c.firstPartyForURL("a@local", "https://site.test/", cookieURL{"https", "site.test", "/api"}, "keep", dtNow) == nil {
		t.Fatal("other name dropped")
	}
}

func TestOwnerContextHoldsSeveralCookies(t *testing.T) {
	var c popupOwnerContexts
	mk := func(n string) WebpopupCookie {
		return WebpopupCookie{Name: n, Value: "v" + n, Domain: ".site.test", Path: "/", Secure: true, Expires: dtNow.Unix() + 3600}
	}
	gen := c.begin("a@local")
	c.recordFirstParty("a@local", gen, "https://site.test/", dtNow, mk("one"), mk("two"))
	for _, n := range []string{"one", "two"} {
		if got := c.firstPartyFor("a@local", "https://site.test/", n, dtNow); got == nil || got.Value != "v"+n {
			t.Fatalf("%s = %+v", n, got)
		}
	}
	if c.firstPartyFor("a@local", "https://site.test/", "three", dtNow) != nil {
		t.Fatal("unknown name held")
	}
	c.dropFirstParty("site.test", "/", map[string]bool{"one": true})
	if c.firstPartyFor("a@local", "https://site.test/", "one", dtNow) != nil ||
		c.firstPartyFor("a@local", "https://site.test/", "two", dtNow) == nil {
		t.Fatal("a deletion by name must drop that name only")
	}
}

func TestDeletableFirstParty(t *testing.T) {
	list := []devtoolsCookie{
		dtCookie(nil),
		dtCookie(func(c *devtoolsCookie) { c.Name = "other" }),
		dtCookie(func(c *devtoolsCookie) { c.Name = "ancestor"; c.PartitionKey.HasCrossSiteAncestor = true }),
		dtCookie(func(c *devtoolsCookie) { c.Name = "foreign"; c.PartitionKey.TopLevelSite = "https://x.test" }),
		dtCookie(func(c *devtoolsCookie) { c.Name = "plain"; c.PartitionKey = nil }),
		dtCookie(func(c *devtoolsCookie) { c.Name = "legacy"; c.PartitionKey.Legacy = true }),
		dtCookie(func(c *devtoolsCookie) { c.Name = "elsewhere"; c.Domain = ".evil.test" }),
		dtCookie(func(c *devtoolsCookie) { c.Name = "admin"; c.Path = "/admin" }),
	}
	names := func(l []devtoolsCookie) map[string]bool {
		m := map[string]bool{}
		for _, c := range l {
			m[c.Name] = true
		}
		return m
	}
	got := names(deletableFirstParty(list, "forum.site.test", "/", nil))
	if len(got) != 2 || !got["cf_clearance"] || !got["other"] {
		t.Fatalf("all names: %v", got)
	}
	got = names(deletableFirstParty(list, "site.test", "/", map[string]bool{"cf_clearance": true}))
	if len(got) != 1 || !got["cf_clearance"] {
		t.Fatalf("by name: %v", got)
	}
}

func TestDevToolsDeleteRequests(t *testing.T) {
	c := dtCookie(nil)
	got := devtoolsDeleteRequest(4, c)
	want := `{"id":4,"method":"Network.deleteCookies","params":{"name":"cf_clearance","domain":".site.test","path":"/","partitionKey":{"topLevelSite":"https://site.test","hasCrossSiteAncestor":false}}}`
	if got != want {
		t.Fatalf("delete request = %s", got)
	}
	if got := devtoolsStorageRequest(2); got != `{"id":2,"method":"Storage.getCookies"}` {
		t.Fatalf("storage request = %s", got)
	}
}

// The path match uses the URL as the browser canonicalized it.
func TestFirstPartyHoldUsesTheCanonicalRequestURL(t *testing.T) {
	var c popupOwnerContexts
	cookie := WebpopupCookie{Name: "cf_clearance", Value: "v1", Domain: ".site.test", Path: "/admin",
		Secure: true, Expires: dtNow.Unix() + 3600}
	c.recordFirstParty("a@local", c.begin("a@local"), "https://site.test/admin/", dtNow, cookie)
	// /admin/../public is /public: out of the cookie's path.
	if c.firstPartyForURL("a@local", "https://site.test/admin/../public",
		cookieURL{"https", "site.test", "/public"}, "cf_clearance", dtNow) != nil {
		t.Fatal("handed out for a path that canonicalizes outside the cookie's")
	}
	// /public/../admin is /admin: inside.
	if c.firstPartyForURL("a@local", "https://site.test/public/../admin",
		cookieURL{"https", "site.test", "/admin"}, "cf_clearance", dtNow) == nil {
		t.Fatal("refused for a path that canonicalizes into the cookie's")
	}
}

// A blocker-only inventory (no eligible cookie, two cookies of one name that
// are not eligible) is still held, so the ambiguity is reported and the name
// is withheld rather than imported from the visitor's jar.
func TestHoldKeepsBlockerOnlyInventoryAsAmbiguity(t *testing.T) {
	var c popupOwnerContexts
	holdOf(&c, nil, []WebpopupCookie{
		{Name: "cf_clearance", Domain: ".site.test", Path: "/", Secure: true, Expires: dtNow.Unix() + 3600},
		{Name: "cf_clearance", Domain: ".site.test", Path: "/", Secure: true},
	})
	hit, amb := c.firstPartyLookup("a@local", "https://site.test/login",
		cookieURL{"https", "site.test", "/"}, "cf_clearance", dtNow)
	if hit != nil || !amb {
		t.Fatalf("hit=%+v ambiguous=%v, want nil, true", hit, amb)
	}
}
