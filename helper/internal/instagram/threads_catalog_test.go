// SPDX-License-Identifier: AGPL-3.0-or-later

package instagram

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"testing"
)

func TestAThreadsPageIsReadForItsTokensItsViewerItsGatesAndItsCode(t *testing.T) {
	p, ok := parseThreadsPage(threadsHTML(true, "tok"))
	if !ok {
		t.Fatal("a logged-in page read as logged out")
	}
	for what, got := range map[string]string{"dtsg": p.dtsg, "lsd": p.lsd, "app": p.appID, "rev": p.rev, "spinR": p.spinR,
		"spinB": p.spinB, "spinT": p.spinT, "hsi": p.hsi, "comet": p.cometReq, "ccg": p.ccg, "crn": p.crn, "dpr": p.dpr,
		"actor": p.actor, "viewer": p.viewerID, "username": p.username} {
		if got == "" {
			t.Errorf("no %s", what)
		}
	}
	if p.dtsg != "tok" || p.actor != threadsActor || p.viewerID != threadsViewer || p.username != "robin.hale" || p.appID != "238260118697367" {
		t.Errorf("read %+v", p)
	}
	if !p.gkx["7479"] || p.gkx["6542"] || string(p.qex["5477"]) != "true" || string(p.qex["518"]) != "null" || string(p.config["92"]) != "true" {
		t.Errorf("gates %v %v %v", p.gkx, p.qex, p.config)
	}
	// In the order the page names them, each once, written plainly.
	if !slices.Equal(p.bundles, []string{bundleOne, bundleTwo, bundleThree}) {
		t.Errorf("bundles %v", p.bundles)
	}
	if _, ok := parseThreadsPage(threadsHTML(false, "tok")); ok {
		t.Error("a page of nobody read as logged in")
	}
	if _, ok := parseThreadsPage("<html>a login form</html>"); ok {
		t.Error("a page with nothing read as logged in")
	}
	if jazoest("ab") != "2195" {
		t.Errorf("jazoest %s", jazoest("ab"))
	}
}

func TestTheBuiltInCatalogHasEveryFlagsCode(t *testing.T) {
	c := builtinCatalog()
	if !c.usable() {
		t.Fatal("the built-in catalog isn't usable")
	}
	page, _ := parseThreadsPage(threadsHTML(true, "x"))
	for _, q := range c.Queries {
		if q.Kind != "query" {
			t.Errorf("%s is a %s", q.Name, q.Kind)
		}
		for _, f := range q.Flags {
			if _, err := c.evalProvider(f, &page, 0); err != nil {
				t.Errorf("%s's flag %s: %v", q.Name, f, err)
			}
		}
	}
}

func TestFlagsAreWorkedOutAsTheWebsitesOwnCodeWorksThemOut(t *testing.T) {
	page := &threadsPage{
		gkx:    map[string]bool{"1": true, "2": false},
		qex:    map[string]json.RawMessage{"10": json.RawMessage("true"), "11": json.RawMessage("null"), "12": json.RawMessage("false"), "13": json.RawMessage(`"x"`)},
		config: map[string]json.RawMessage{"20": json.RawMessage("true")},
	}
	c := &threadsCatalog{Providers: map[string]string{"On": `return r("gkx")("1")`}}
	for code, want := range map[string]bool{
		`return!0`:                                                  true,
		`return!1`:                                                  false,
		`return r("gkx")("1")`:                                      true,
		`return r("gkx")("2")`:                                      false,
		`return r("gkx")("404")`:                                    false, // a gate the page didn't ship
		`return r("gkx")("1")===!0`:                                 true,
		`return r("gkx")("2")===!1`:                                 true,
		`return r("qex")._("10")===!0`:                              true,
		`return r("qex")._("11")===!0`:                              false,
		`return r("qex")._("11")!==!1`:                              true, // null isn't false
		`return r("qex")._("12")!==!1`:                              false,
		`return r("qex")._("13")`:                                   true,
		`return r("qex")._("404")===!0`:                             false,
		`return r("MetaConfig")._("20")`:                            true,
		`return r("MetaConfig")._("21")`:                            false,
		`return r("gkx")("1")===!0&&r("gkx")("2")===!0`:             false,
		`return r("gkx")("2")===!0||r("qex")._("10")===!0`:          true,
		`return r("On.relayprovider").get()&&r("qex")._("11")!==!1`: true,
		`var e;return(e=r("qex")._("11"))!=null?e:!1`:               false,
		`var e;return(e=r("qex")._("10"))!=null?e:!1`:               true,
		`return (r("gkx")("2")||r("gkx")("1"))===!0`:                true,
		`return r("Missing.relayprovider").get()`:                   false,
		`return r("Date").now()>0`:                                  false, // a shape providers don't have
		`fetch("https://evil.example");return!0`:                    false,
		`return r("gkx")("1");doSomething()`:                        false,
	} {
		c.Providers["Under"] = code
		if got := c.flag("Under", page); got != want {
			t.Errorf("%s: got %v", code, got)
		}
	}
	// A provider that reads itself stops.
	c.Providers["Loop"] = `return r("Loop.relayprovider").get()`
	if c.flag("Loop", page) {
		t.Error("a loop was true")
	}
}

func TestTheVariablesAreTheQuerysOwnAndNothingElse(t *testing.T) {
	c := &threadsCatalog{Providers: map[string]string{"A": `return!0`}}
	q := threadsQuery{Name: "XQuery", Doc: "1", Kind: "query", Vars: []string{"after", "first"}, Flags: []string{"A", "B"}}
	v := c.variables(q, map[string]any{"first": 3, "sneaky": "x"}, &threadsPage{})
	want := map[string]any{"after": nil, "first": 3, flagPrefix + "A" + flagSuffix: true, flagPrefix + "B" + flagSuffix: false}
	if fmt.Sprint(v) != fmt.Sprint(want) {
		t.Errorf("got %v", v)
	}
}

func TestOnlyAQueryTheCatalogCallsOneIsEverSent(t *testing.T) {
	c := builtinCatalog()
	c.Queries["AMutation"] = threadsQuery{Name: "AMutation", Doc: "1", Kind: "mutation"}
	c.Queries["HiddenMutationQuery"] = threadsQuery{Name: "HiddenMutationQuery", Doc: "1", Kind: "query"}
	c.Queries["BadDocQuery"] = threadsQuery{Name: "BadDocQuery", Doc: "1; DROP", Kind: "query"}
	c.Queries["Sub"] = threadsQuery{Name: "Sub", Doc: "1", Kind: "subscription"}
	for _, name := range []string{"AMutation", "HiddenMutationQuery", "BadDocQuery", "Sub", "Nothing"} {
		if _, err := c.query(name); !errors.Is(err, errNotAQuery) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := c.query(threadsFeedQuery); err != nil {
		t.Error(err)
	}
}

func TestDefinitionsAreFoundWhereverTheyAreInTheBundles(t *testing.T) {
	d := newDiscovery(threadsFeedQuery)
	ops := `__d("BarcelonaFeedPaginationDirectQuery_threadsRelayOperation",[],(function(t,n,r,o,a,i){a.exports="777"}),null);`
	params := `__d("BarcelonaFeedPaginationDirectQuery$Parameters",["BarcelonaFeedPaginationDirectQuery_threadsRelayOperation"],(function(t,n,r,o,a,i,l){"use strict";var e={kind:"PreloadableConcreteRequest",params:{id:n("BarcelonaFeedPaginationDirectQuery_threadsRelayOperation"),metadata:{},name:"BarcelonaFeedPaginationDirectQuery",operationKind:"query",text:null}};}),null);`
	full := `__d("BarcelonaFeedPaginationDirectQuery.threads.graphql",[],(function(t,n,r,o,a,i,l){"use strict";var e=[{defaultValue:null,kind:"LocalArgument",name:"after"},{defaultValue:null,kind:"LocalArgument",name:"__relay_internal__pv__Arelayprovider"}];return{params:{id:n("BarcelonaFeedPaginationDirectQuery_threadsRelayOperation"),metadata:{},name:"BarcelonaFeedPaginationDirectQuery",operationKind:"query",text:null}}}),null);`
	provider := `__d("A.relayprovider",["B.relayprovider"],(function(t,n,r,o,a,i,l){"use strict";var e={get:function(){return r("B.relayprovider").get()}};l.default=e}),98);`
	dep := `__d("B.relayprovider",["gkx"],(function(t,n,r,o,a,i,l){"use strict";var e={get:function(){return r("gkx")("1")}};l.default=e}),98);`
	if d.scan(`__d("Other",[],(function(){}),1);`) {
		t.Error("a bundle of nothing held something")
	}
	for i, js := range []string{params, ops, full, provider} {
		if !d.scan(js) {
			t.Errorf("bundle %d held nothing", i)
		}
		if d.done() {
			t.Errorf("done after bundle %d", i)
		}
	}
	d.scan(dep)
	if !d.done() {
		t.Fatal("not done with everything found")
	}
	q := d.queries()[threadsFeedQuery]
	if q.Doc != "777" || q.Kind != "query" || !slices.Equal(q.Vars, []string{"after"}) || !slices.Equal(q.Flags, []string{"A"}) {
		t.Errorf("found %+v", q)
	}
	merged := d.merge(builtinCatalog())
	if merged.Queries[threadsFeedQuery].Doc != "777" || merged.Queries[threadsRepliesQuery].Doc != "28686245017651778" ||
		merged.Providers["A"] == "" || merged.Providers["BarcelonaIsLoggedIn"] == "" {
		t.Errorf("merged %+v", merged)
	}
	// The inline form, and a mutation named like a query is found as one.
	d = newDiscovery("XQuery")
	d.scan(`return{params:{id:"55",metadata:{},name:"XQuery",operationKind:"mutation",text:null}}`)
	if d.docs["XQuery"] != "55" || d.kinds["XQuery"] != "mutation" {
		t.Errorf("inline %v %v", d.docs, d.kinds)
	}
}

func TestBundlesThatHeldNothingAreRememberedUpToALimit(t *testing.T) {
	c := builtinCatalog()
	for i := range maxEmpty + 3 {
		c.markEmpty(fmt.Sprintf("https://static.cdninstagram.com/rsrc.php/%d.js", i))
	}
	if len(c.Empty) != maxEmpty || c.isEmpty("https://static.cdninstagram.com/rsrc.php/0.js") ||
		!c.isEmpty(fmt.Sprintf("https://static.cdninstagram.com/rsrc.php/%d.js", maxEmpty+2)) {
		t.Errorf("%d remembered", len(c.Empty))
	}
}

func TestFeedStateIsWhatTheWebsiteSendsWritten(t *testing.T) {
	units := []feedUnit{
		{thread: true, items: []unitItem{{"a", viewStateThread}}},
		{thread: true, items: []unitItem{{"b", 0}, {"c", viewStateThreadReply}}},
		{items: []unitItem{{"people", 223}}},
		{thread: true, items: []unitItem{{"d", 7}}},
		{thread: true},
	}
	st := buildFeedState(units, 1700, 1)
	got, _ := json.Marshal(st)
	want := `{"last_n_items":[{"thread_items":[{"post":{"id":"b","view_state_item_type":0}},{"post":{"id":"c","view_state_item_type":194}}]}],` +
		`"last_request_time_ms":1700,"num_organic_items":2,"num_organic_thread_items":3,"num_total_items":4,"num_total_thread_items":5,` +
		`"position_sensitive_items":[{"index":2,"item":{"id":"people","view_state_item_type":223}},{"index":3,"item":{"id":"d","view_state_item_type":7}}]}`
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	empty, _ := json.Marshal(buildFeedState(nil, 5, 0))
	if string(empty) != `{"last_n_items":[],"last_request_time_ms":5,"num_organic_items":0,"num_organic_thread_items":0,"num_total_items":0,"num_total_thread_items":0,"position_sensitive_items":[]}` {
		t.Errorf("empty %s", empty)
	}
}

func TestWhatAGraphQLAnswerSaysIsReadForWhatToDo(t *testing.T) {
	for body, want := range map[string]error{
		`{"data":{"x":1}}`: nil,
		`for (;;);{"data":{"x":1},"errors":[{"message":"a part failed","severity":"WARNING"}]}`:          nil,
		`{"data":null,"errors":[{"message":"A server error missing_required_variable_value occured."}]}`: errStaleQuery,
		`{"errors":[{"message":"x","code":1675012}]}`:                                                    errStaleQuery,
		`{"errors":[{"message":"PersistedQueryNotFound"}]}`:                                              errStaleQuery,
		`for (;;);{"error":1357001,"errorSummary":"Not Logged In"}`:                                      errLoggedOutPage,
		`for (;;);{"error":1357004,"errorSummary":"Sorry, something went wrong"}`:                        errStaleTokens,
		`{"errors":[{"message":"Rate limit exceeded"}]}`:                                                 errThreadsRateLimited,
		`{"errors":[{"message":"something else"}]}`:                                                      errThreadsPage,
		`<html>not json</html>`: errThreadsPage,
	} {
		_, err := graphqlData([]byte(body))
		if !errors.Is(err, want) && !(want == nil && err == nil) {
			t.Errorf("%s: got %v, want %v", body, err, want)
		}
	}
}

func TestARedirectSaysWhatTheSessionsTroubleIs(t *testing.T) {
	for loc, want := range map[string]error{
		"https://www.threads.com/login?next=%2F":      errLoggedOutPage,
		"https://www.threads.com/accounts/login/":     errLoggedOutPage,
		"https://www.threads.com/challenge/":          errThreadsCheckpoint,
		"https://www.threads.com/checkpoint/1/":       errThreadsCheckpoint,
		"https://www.threads.com/accounts/suspended/": errThreadsSuspended,
		"http://www.threads.com/":                     errThreadsPage,
		"https://threads.com.evil.example/":           errThreadsPage,
		"https://www.threads.com/?hl=en":              nil,
	} {
		u, _ := url.Parse(loc)
		next, err := sentTo(u)
		if err != want || (want == nil && next != loc) {
			t.Errorf("%s: %q %v", loc, next, err)
		}
	}
}

func TestALinkCardGoesWhereThreadsRedirectSendsIt(t *testing.T) {
	for raw, want := range map[string]string{
		"https://l.threads.com/?u=https%3A%2F%2Fexample.com%2Fx&e=1":                                    "https://example.com/x",
		"https://l.threads.com/?u=https%3A%2F%2Fl.instagram.com%2F%3Fu%3Dhttps%253A%252F%252Fa.example": "https://a.example",
		"https://example.org/plain":                      "https://example.org/plain",
		"https://l.threads.com/?u=javascript%3Aalert(1)": "",
		"ftp://example.org/":                             "",
		"https://l.threads.com/?u=https%3A%2F%2F" + string(make([]byte, 0)) + "x.example%2F" + longPath(): "",
	} {
		if got := linkTarget(raw); got != want {
			t.Errorf("%s: %q", raw, got)
		}
	}
}

func longPath() string {
	b := make([]byte, maxLinkURL)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}
