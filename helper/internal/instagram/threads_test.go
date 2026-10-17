// SPDX-License-Identifier: AGPL-3.0-or-later

package instagram

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

func TestThreadsLogsInWithItsOwnCookiesAsTheBrowserNamed(t *testing.T) {
	h := newThreadsHarness(t)
	if err := h.loginWith(threadsCookieText); err != nil {
		t.Fatal(err)
	}
	states := h.rec.events("account")
	if len(states) != 2 || states[0]["state"] != "connecting" || states[1]["state"] != "ready" ||
		states[1]["name"] != "robin.hale" || states[1]["network"] != "threads" || states[1]["user_id"] == nil {
		t.Fatalf("account events: %v", states)
	}
	if users := h.rec.events("user"); len(users) != 1 || users[0]["user"].(map[string]any)["is_self"] != true {
		t.Errorf("user events: %v", users)
	}
	// The home page, as a navigation typed in, with the cookies as pasted
	// and the named Chrome's headers.
	r := h.site.lastRequest("/")
	if r.Header.Get("cookie") != threadsCookieText {
		t.Errorf("cookie header %q", r.Header.Get("cookie"))
	}
	for k, v := range map[string]string{"sec-fetch-dest": "document", "sec-fetch-mode": "navigate", "sec-fetch-site": "none",
		"sec-ch-ua-mobile": "?0", "upgrade-insecure-requests": "1"} {
		if r.Header.Get(k) != v {
			t.Errorf("%s = %q", k, r.Header.Get(k))
		}
	}
	if ua := r.Header.Get("user-agent"); !strings.Contains(ua, "Chrome/150.0.0.0") {
		t.Errorf("user agent %q", ua)
	}
	if !strings.Contains(r.Header.Get("sec-ch-ua-full-version-list"), "150.0.7712.45") {
		t.Errorf("full version list %q", r.Header.Get("sec-ch-ua-full-version-list"))
	}
	// Saved, private, with the browser.
	path := filepath.Join(h.dir, "threads", sessionFile)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("session file: %v %v", info, err)
	}
	var saved threadsSaved
	data, _ := os.ReadFile(path)
	if json.Unmarshal(data, &saved) != nil || saved.Browser != "Chrome 150.0.7712.45" || len(saved.Cookies) != 5 || saved.Cookies[0].Name != "sessionid" {
		t.Errorf("saved %s", data)
	}
}

func TestThreadsRefusesCookiesOfNobodyAndSaysWhatToDoAboutACheckpoint(t *testing.T) {
	for _, c := range []struct {
		home func(*http.Request) *http.Response
		want proto.Code
	}{
		{respond(200, threadsHTML(false, "x")), proto.BadCookies},
		{respond(302, "", "Location", "https://www.threads.com/login?next=%2F"), proto.BadCookies},
		{respond(302, "", "Location", "https://www.threads.com/accounts/login/"), proto.BadCookies},
		{respond(302, "", "Location", "https://www.threads.com/challenge/?next=/"), proto.Checkpoint},
		{respond(302, "", "Location", "https://evil.example/"), proto.NetworkError},
		{respond(500, ""), proto.NetworkError},
	} {
		h := newThreadsHarness(t)
		h.site.home = c.home
		err := h.loginWith(threadsCookieText)
		var pe *proto.Error
		if !errors.As(err, &pe) || pe.Code != c.want {
			t.Errorf("got %v, want %s", err, c.want)
		}
		if _, err := os.Stat(filepath.Join(h.dir, "threads", sessionFile)); err == nil {
			t.Error("cookies that didn't log in were saved")
		}
		if s := h.rec.events("account"); s[len(s)-1]["state"] != "logged_out" {
			t.Errorf("state %v", s)
		}
	}
}

func TestThreadsResumesASavedSessionAndKeepsTheCookiesThreadsSets(t *testing.T) {
	dir := t.TempDir()
	h := newThreadsHarnessIn(t, dir)
	h.login()
	h.b.Close()

	h2 := newThreadsHarnessIn(t, dir)
	h2.site.home = func(r *http.Request) *http.Response {
		resp := respond(200, threadsHTML(true, "dtsg-2"))(r)
		resp.Header.Add("Set-Cookie", "rur=rur2; Domain=.threads.com; Path=/; Secure")
		resp.Header.Add("Set-Cookie", "mid=; Domain=.threads.com; Max-Age=0")
		resp.Header.Add("Set-Cookie", "ig_did=new1; Domain=.threads.com; Path=/")
		resp.Header.Add("Set-Cookie", "evil=1; Domain=.example.com")
		return resp
	}
	h2.b.Start(context.Background())
	deadline := time.Now().Add(5 * time.Second)
	for {
		if s := h2.rec.events("account"); len(s) > 0 && s[len(s)-1]["state"] == "ready" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never ready: %v", h2.rec.events("account"))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := h2.site.lastRequest("/").Header.Get("cookie"); got != threadsCookieText {
		t.Errorf("resumed with %q", got)
	}
	var saved threadsSaved
	data, _ := os.ReadFile(filepath.Join(dir, "threads", sessionFile))
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range saved.Cookies {
		names = append(names, c.Name+"="+c.Value)
	}
	if strings.Join(names, "; ") != "sessionid=s3ss; ds_user_id=9000001; csrftoken=csrf1; rur=rur2; ig_did=new1" {
		t.Errorf("cookies kept: %v", names)
	}
}

// feedPage stubs the GraphQL answers in turn.
func (h *threadsHarness) answers(bodies ...string) {
	i := 0
	h.site.graphql = func(url.Values) string {
		if i >= len(bodies) {
			h.t.Errorf("one GraphQL request too many")
			return `{"data":null}`
		}
		i++
		return bodies[i-1]
	}
}

func (h *threadsHarness) feed(fresh bool, kind backend.FeedKind) ([]proto.Post, bool) {
	h.t.Helper()
	posts, more, err := h.b.LoadFeed(context.Background(), 10, fresh, backend.Box{}, kind)
	if err != nil {
		h.t.Fatalf("feed: %v", err)
	}
	return posts, more
}

func TestThreadsFeedAsksAsTheWebsiteDoesAndSaysNothingWasSeen(t *testing.T) {
	h := newThreadsHarness(t)
	h.login()
	h.answers(
		"for (;;);"+tFeed("cursor-1", true, tThread("", tPost("101", "maya.lens", 19, "hello")), tPeople),
		tFeed("cursor-2", false, tThread("", tPost("102", "sam.climbs", 19, "again"))),
	)
	posts, more := h.feed(false, "")
	if len(posts) != 1 || !more {
		t.Fatalf("first page: %v %v", posts, more)
	}
	form := h.site.lastForm()
	for k, want := range map[string]string{
		"doc_id": "28783769534619828", "fb_api_req_friendly_name": threadsFeedQuery, "fb_api_caller_class": "RelayModern",
		"fb_dtsg": "dtsg-1", "lsd": "lsd-token", "jazoest": jazoest("dtsg-1"), "av": threadsActor, "__user": "0", "__a": "1",
		"__req": "1", "__comet_req": "29", "__rev": "1001", "__hsi": "7000000000000000001", "__spin_r": "1001",
		"__spin_b": "trunk", "__spin_t": "1792200000", "__crn": "comet.threads.BarcelonaHomeRouteV2", "__ccg": "EXCELLENT",
		"dpr": "2", "server_timestamps": "true",
	} {
		if got := form.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if len(strings.Split(form.Get("__s"), ":")) != 3 || form.Get("__hs") == "" {
		t.Errorf("__s %q, __hs %q", form.Get("__s"), form.Get("__hs"))
	}
	r := h.site.lastRequest("/api/graphql")
	for k, want := range map[string]string{"x-fb-friendly-name": threadsFeedQuery, "x-fb-lsd": "lsd-token", "x-ig-app-id": "238260118697367",
		"x-csrftoken": "csrf1", "referer": "https://www.threads.com/", "origin": "https://www.threads.com", "sec-fetch-mode": "cors",
		"sec-fetch-site": "same-origin", "content-type": "application/x-www-form-urlencoded", "cookie": threadsCookieText} {
		if got := r.Header.Get(k); got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}
	v := vars(t, form)
	if v["variant"] != "for_you" || v["first"] != 10.0 || v["after"] != nil || v["sort_by"] != nil {
		t.Errorf("variables %v", v)
	}
	if _, ok := v["after"]; !ok {
		t.Error("a declared variable not given isn't sent as null")
	}
	data := v["data"].(map[string]any)
	if data["feed_view_info"] != "[]" || data["pagination_source"] != "text_post_feed_threads" || data["reason"] != nil {
		t.Errorf("data %v", data)
	}
	var state textAppState
	if err := json.Unmarshal([]byte(data["feed_state"].(string)), &state); err != nil || state.NumTotalItems != 0 || state.LastRequestTimeMs == 0 {
		t.Errorf("first feed_state %v %v", data["feed_state"], err)
	}
	// Every flag the query declares, worked out from the page's gates.
	flags := 0
	for k, val := range v {
		if strings.HasPrefix(k, flagPrefix) {
			flags++
			if _, ok := val.(bool); !ok {
				t.Errorf("%s = %v", k, val)
			}
		}
	}
	if flags != len(threadsFlags) || v[flagPrefix+"BarcelonaIsLoggedIn"+flagSuffix] != true ||
		v[flagPrefix+"BarcelonaHasCommunities"+flagSuffix] != false || v[flagPrefix+"BarcelonaHasBestOfThreads"+flagSuffix] != true ||
		v[flagPrefix+"BarcelonaShouldFetchPostAuthorFullName"+flagSuffix] != true ||
		v[flagPrefix+"BarcelonaHasPrivateRepliesDeprecation"+flagSuffix] != true ||
		v[flagPrefix+"BarcelonaHasSportTeamAllegianceCard"+flagSuffix] != false ||
		v[flagPrefix+"BarcelonaShouldShowFediverseM075Features"+flagSuffix] != true {
		t.Errorf("%d flags: %v", flags, v)
	}

	// The next page goes on from the cursor, naming what was given.
	posts, more = h.feed(false, "")
	if len(posts) != 1 || more || posts[0].ID != 2 {
		t.Fatalf("second page: %v %v", posts, more)
	}
	form = h.site.lastForm()
	v = vars(t, form)
	data = v["data"].(map[string]any)
	if v["after"] != "cursor-1" || data["reason"] != "pagination" || data["session_id"] != nil || data["feed_view_info"] != "[]" {
		t.Errorf("second page variables %v", v)
	}
	if _, ok := data["session_id"]; !ok {
		t.Error("no session_id")
	}
	if err := json.Unmarshal([]byte(data["feed_state"].(string)), &state); err != nil {
		t.Fatal(err)
	}
	if state.NumTotalItems != 2 || state.NumOrganicItems != 1 || len(state.LastNItems) != 1 ||
		state.LastNItems[0].ThreadItems[0].Post.ID != "101_9000" || len(state.PositionSensitiveItems) != 1 ||
		state.PositionSensitiveItems[0].Index != 1 {
		t.Errorf("feed_state %+v", state)
	}
	if form.Get("__req") != "2" {
		t.Errorf("__req %q", form.Get("__req"))
	}
	// The end: nothing more is asked.
	if posts, more := h.feed(false, ""); len(posts) != 0 || more {
		t.Errorf("past the end: %v %v", posts, more)
	}
	// What the website logs isn't asked for, ever.
	for _, a := range h.site.asked() {
		if strings.Contains(a, "/ajax/") || strings.Contains(a, "logging") || strings.Contains(a, "falco") {
			t.Errorf("asked %s", a)
		}
	}
}

func TestThreadsPostsAreInstagramsMediaWithWhatThreadsAdds(t *testing.T) {
	h := newThreadsHarness(t)
	h.login()
	video := `"video_versions":[{"width":720,"height":1280,"url":"` + cdn + `/v/clip_720.mp4?oh=s"}],"video_duration":12.5,"original_width":720,"original_height":1280,"image_versions2":` + tImages("cover", 1080, 1920)
	carousel := `"carousel_media":[{"media_type":1,"image_versions2":` + tImages("c1", 1080, 1350) + `},{"media_type":2,` + video + `}]`
	quoted := tPost("301", "noor.draws", 1, "the original", `"image_versions2":`+tImages("q", 1080, 1080))
	link := `"link_preview_attachment":{"url":"https://l.threads.com/?u=https%3A%2F%2Fexample.com%2Fa%3Fb%3D1&e=AT0","title":"An\narticle","display_url":"example.com","image_url":"https://external.fsgn5-1.fna.fbcdn.net/emg1/v/t13/x.jpg"}`
	reposter := tPost("401", "oliver.k", 19, "", `"text_post_app_info":`+tInfo(`"share_info":{"reposted_post":`+tPost("402", "maya.lens", 19, "reposted words")+`}`))
	ad := tPost("501", "deals", 1, "buy", `"ad_id":"9"`, `"image_versions2":`+tImages("ad", 1080, 1080))
	h.answers(tFeed("", false,
		tThread("robin.hale's first thread\n  ", tPost("201", "maya.lens", 19, "just words")),
		tThread("", tPost("202", "sam.climbs", 1, "a photo", `"image_versions2":`+tImages("p", 1080, 1350))),
		tThread("", tPost("203", "sam.climbs", 8, "", carousel)),
		tThread("", tPost("204", "noor.draws", 2, "a clip", video)),
		tThread("", tPost("205", "maya.lens", 19, "quoting", `"text_post_app_info":`+tInfo(`"share_info":{"quoted_post":`+quoted+`}`))),
		tThread("", tPost("206", "maya.lens", 19, "read this", `"text_post_app_info":`+tInfo(link))),
		tThread("", reposter),
		tThread("", tPost("207", "oliver.k", 19, "one"), tPost("208", "oliver.k", 19, "two")),
		tThread("", ad),
		tThread("", tPost("209", "аlice", 19, "a look-alike username")),
		tThread("", tPost("210", "maya.lens", 19, "")),
		tPeople,
	))
	posts, _ := h.feed(false, "")
	if len(posts) != 9 {
		t.Fatalf("%d posts", len(posts))
	}
	text, photo, car, clip, quote, card, repost, one, two := posts[0], posts[1], posts[2], posts[3], posts[4], posts[5], posts[6], posts[7], posts[8]
	if len(text.Media) != 0 || text.Caption != "just words" || text.Context != "robin.hale's first thread" ||
		text.URL != "https://www.threads.com/@maya.lens/post/C201" || text.Author.Photo == nil {
		t.Errorf("text post %+v", text)
	}
	if *text.Likes != 42 || *text.Comments != 7 || *text.Reposts != 3 || *text.Shares != 5 {
		t.Errorf("counts %v %v %v %v", *text.Likes, *text.Comments, *text.Reposts, *text.Shares)
	}
	if len(photo.Media) != 1 || photo.Media[0].Kind != proto.Photo || photo.Media[0].Width != 1080 {
		t.Errorf("photo %+v", photo.Media)
	}
	if len(car.Media) != 2 || car.Media[1].Kind != proto.Video || car.Media[1].FileID == 0 {
		t.Errorf("carousel %+v", car.Media)
	}
	if len(clip.Media) != 1 || clip.Media[0].Duration != 13 || clip.Media[0].Mime != "video/mp4" {
		t.Errorf("video %+v", clip.Media)
	}
	if q := quote.Quoted; q == nil || q.ID != 0 || q.Caption != "the original" || len(q.Media) != 1 ||
		q.URL != "https://www.threads.com/@noor.draws/post/C301" {
		t.Errorf("quote %+v", quote.Quoted)
	}
	if lp := card.LinkPreview; lp == nil || lp.URL != "https://example.com/a?b=1" || lp.Title != "An article" || lp.Image == nil {
		t.Errorf("link card %+v", card.LinkPreview)
	}
	if repost.Caption != "reposted words" || repost.Context != "oliver.k reposted" || repost.Author.Username != "maya.lens" {
		t.Errorf("repost %+v", repost)
	}
	if one.Continues || !two.Continues || two.Caption != "two" {
		t.Errorf("a thread: %v %v", one.Continues, two.Continues)
	}
	// Every file is Threads', on the CDN.
	for _, p := range posts {
		for _, m := range p.Media {
			ref, ok := h.deps.Files.Get(m.FileID)
			if !ok || ref.Network != proto.Threads {
				t.Errorf("file %d: %+v", m.FileID, ref)
			}
		}
	}
	// Wire form: a quoted post carries nothing of its own.
	raw, _ := json.Marshal(quote)
	if !strings.Contains(string(raw), `"quoted":{"id":0`) || strings.Count(string(raw), `"quoted"`) != 1 {
		t.Errorf("wire %s", raw)
	}
	// The same post again keeps its id; a fresh load starts again.
	h.answers(tFeed("", false, tThread("", tPost("202", "sam.climbs", 1, "a photo", `"image_versions2":`+tImages("p", 1080, 1350)))))
	again, _ := h.feed(true, "")
	if len(again) != 1 || again[0].ID != photo.ID {
		t.Errorf("again %v, was %d", again, photo.ID)
	}
}

func TestThreadsFollowingFeedIsAFeedOfItsOwn(t *testing.T) {
	h := newThreadsHarness(t)
	h.login()
	h.answers(
		tFeed("a", true, tThread("", tPost("101", "maya.lens", 19, "x"))),
		tFeed("b", true, tThread("", tPost("102", "maya.lens", 19, "y"))),
	)
	h.feed(false, backend.ForYou)
	h.feed(false, backend.Following)
	v := vars(t, h.site.lastForm())
	if v["variant"] != "following" || v["after"] != nil || v["data"].(map[string]any)["pagination_source"] != "text_post_feed_following" {
		t.Errorf("switching feeds: %v", v)
	}
}

func TestThreadsSendsOnlyQueriesNeverAMutation(t *testing.T) {
	h := newThreadsHarness(t)
	h.login()
	before := len(h.site.asked())
	h.b.net.Lock()
	h.b.cat.Queries["useBarcelonaLikeMutationLikeMutation"] = threadsQuery{Name: "useBarcelonaLikeMutationLikeMutation", Doc: "1", Kind: "mutation"}
	h.b.cat.Queries["BarcelonaSneakyQuery"] = threadsQuery{Name: "BarcelonaSneakyQuery", Doc: "2", Kind: "mutation"}
	h.b.cat.Queries["BarcelonaLikeMutationQuery"] = threadsQuery{Name: "BarcelonaLikeMutationQuery", Doc: "3", Kind: "query"}
	h.b.net.Unlock()
	for _, name := range []string{"useBarcelonaLikeMutationLikeMutation", "BarcelonaSneakyQuery", "BarcelonaLikeMutationQuery", "BarcelonaUnknownQuery"} {
		if _, err := h.b.ask(context.Background(), name, nil, threadsBase+"/"); err == nil {
			t.Errorf("%s was sent", name)
		}
	}
	if after := h.site.asked(); len(after) != before {
		t.Errorf("requests made: %v", after[before:])
	}
}

// newBundle is JavaScript defining the feed query anew: doc id 999, a
// variable more, and a new flag with its provider.
const newBundle = `__d("BarcelonaFeedPaginationDirectQuery_threadsRelayOperation",[],(function(t,n,r,o,a,i){a.exports="999"}),null);
__d("BarcelonaFeedPaginationDirectQuery.threads.graphql",["BarcelonaFeedPaginationDirectQuery_threadsRelayOperation"],(function(t,n,r,o,a,i,l){"use strict";var e=[{defaultValue:null,kind:"LocalArgument",name:"after"},{defaultValue:null,kind:"LocalArgument",name:"data"},{defaultValue:null,kind:"LocalArgument",name:"first"},{defaultValue:null,kind:"LocalArgument",name:"variant"},{defaultValue:null,kind:"LocalArgument",name:"mood"},{defaultValue:null,kind:"LocalArgument",name:"__relay_internal__pv__BarcelonaIsLoggedInrelayprovider"},{defaultValue:null,kind:"LocalArgument",name:"__relay_internal__pv__BarcelonaHasNewThingrelayprovider"}];return{params:{id:n("BarcelonaFeedPaginationDirectQuery_threadsRelayOperation"),metadata:{is_fully_www_executable:!0},name:"BarcelonaFeedPaginationDirectQuery",operationKind:"query",text:null}}}),null);
__d("BarcelonaHasNewThing.relayprovider",["gkx"],(function(t,n,r,o,a,i,l){"use strict";var e={get:function(){return r("gkx")("6542")===!1}};l.default=e}),98);`

func TestThreadsReadsAChangedQueryAgainFromTheWebsiteWithoutCookies(t *testing.T) {
	h := newThreadsHarness(t)
	h.login()
	h.site.bundles[bundleTwo] = `__d("Nothing",[],(function(){}),1);`
	h.site.bundles[bundleThree] = newBundle
	h.site.bundles[bundleOne] = `__d("AlsoNothing",[],(function(){}),1);`
	calls := 0
	h.site.graphql = func(form url.Values) string {
		calls++
		if form.Get("doc_id") != "999" {
			return `{"errors":[{"message":"A server error missing_required_variable_value occured.","code":1675012}],"extensions":{"is_final":true}}`
		}
		return tFeed("", false, tThread("", tPost("101", "maya.lens", 19, "x")))
	}
	posts, _ := h.feed(false, "")
	if len(posts) != 1 || calls != 2 {
		t.Fatalf("%d posts after %d calls", len(posts), calls)
	}
	v := vars(t, h.site.lastForm())
	if _, ok := v["mood"]; !ok || v[flagPrefix+"BarcelonaHasNewThing"+flagSuffix] != true || len(v) != 7 {
		t.Errorf("the new definition wasn't followed: %v", v)
	}
	// The bundles went without cookies, from the page's list in order,
	// stopping once all was found.
	var bundles []string
	for _, r := range h.site.reqs {
		if r.URL.Host == "static.cdninstagram.com" {
			bundles = append(bundles, r.URL.String())
			if r.Header.Get("cookie") != "" || r.Header.Get("sec-fetch-dest") != "script" {
				t.Errorf("bundle headers %v", r.Header)
			}
		}
	}
	if !slices.Equal(bundles, []string{bundleOne, bundleTwo, bundleThree}) {
		t.Errorf("bundles read %v", bundles)
	}
	// Kept, with the bundles that held nothing.
	var saved threadsCatalog
	if ok, err := h.deps.Session.LoadJSON(catalogFile, &saved); !ok || err != nil || saved.Queries[threadsFeedQuery].Doc != "999" || len(saved.Empty) != 2 {
		t.Errorf("catalog kept: %v %v %+v", ok, err, saved)
	}
	// A new backend starts from it.
	h2 := newThreadsHarnessIn(t, h.dir)
	if h2.b.cat.Queries[threadsFeedQuery].Doc != "999" {
		t.Error("the kept catalog wasn't read")
	}

	// Changed again within minutes: not read again, and the user is told.
	h.site.graphql = func(url.Values) string {
		return `{"errors":[{"message":"PersistedQueryNotFound","code":1675030}]}`
	}
	asked := len(h.site.asked())
	_, _, err := h.b.LoadFeed(context.Background(), 10, true, backend.Box{}, "")
	if !errors.Is(err, errThreadsQueryChanged) {
		t.Errorf("got %v", err)
	}
	if more := h.site.asked()[asked:]; len(more) != 1 {
		t.Errorf("asked %v", more)
	}
}

func TestThreadsLoadsThePageAgainWhenItsTokensExpire(t *testing.T) {
	h := newThreadsHarness(t)
	h.login()
	h.site.home = func(r *http.Request) *http.Response { return respond(200, threadsHTML(true, "dtsg-fresh"))(r) }
	h.site.graphql = func(form url.Values) string {
		if form.Get("fb_dtsg") != "dtsg-fresh" {
			return `for (;;);{"__ar":1,"error":1357004,"errorSummary":"Sorry, something went wrong"}`
		}
		return tFeed("", false, tThread("", tPost("101", "maya.lens", 19, "x")))
	}
	if posts, _ := h.feed(false, ""); len(posts) != 1 {
		t.Fatalf("%v", posts)
	}
	if f := h.site.lastForm(); f.Get("jazoest") != jazoest("dtsg-fresh") || f.Get("__req") != "1" {
		t.Errorf("retried with %v", f)
	}
}

func TestAnEndedThreadsSessionSaysSoAndItsCookiesGo(t *testing.T) {
	h := newThreadsHarness(t)
	h.login()
	h.site.graphql = func(url.Values) string {
		return `for (;;);{"__ar":1,"error":1357001,"errorSummary":"Not Logged In"}`
	}
	_, _, err := h.b.LoadFeed(context.Background(), 10, false, backend.Box{}, "")
	if err != errThreadsSessionEnded {
		t.Fatalf("got %v", err)
	}
	if s := h.rec.events("account"); len(s) != 1 || s[0]["state"] != "error" || s[0]["error"] != errThreadsSessionEnded.Message {
		t.Errorf("account %v", s)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "threads", sessionFile)); !os.IsNotExist(err) {
		t.Error("the ended session's cookies are still there")
	}
	if _, _, err := h.b.LoadFeed(context.Background(), 10, false, backend.Box{}, ""); err != errThreadsNotConnected {
		t.Errorf("after: %v", err)
	}
}

func TestThreadsRepliesAreThePostsPagesQueries(t *testing.T) {
	h := newThreadsHarness(t)
	h.login()
	h.answers(tFeed("", false, tThread("", tPost("101", "maya.lens", 19, "a post"))))
	posts, _ := h.feed(false, "")
	post := posts[0].ID

	reply := func(pk, text string, mediaType int) string {
		return tPost(pk, "sam.climbs", mediaType, text, `"text_post_app_info":`+tInfo(``))
	}
	h.answers(
		tReplies("r-1", true, reply("701", "first", 19), reply("702", "", 1), reply("703", "clip", 2), reply("704", "many", 8)),
		tReplies("", false, reply("705", "later", 19)),
		tReplies("", false, reply("801", "nested", 19)),
	)
	load := func(of int64, more bool) ([]proto.Comment, bool) {
		t.Helper()
		cs, m, err := h.b.LoadComments(context.Background(), post, of, more)
		if err != nil {
			t.Fatal(err)
		}
		return cs, m
	}
	cs, more := load(0, false)
	if len(cs) != 4 || !more || cs[0].Text != "first" || cs[0].Replies != 7 || *cs[0].Likes != 42 || cs[0].ReplyTo != 0 {
		t.Fatalf("first page %+v %v", cs, more)
	}
	if cs[0].Attachment != "" || cs[1].Attachment != proto.AttachPhoto || cs[2].Attachment != proto.AttachVideo || cs[3].Attachment != proto.AttachCarousel {
		t.Errorf("attachments %v %v %v %v", cs[0].Attachment, cs[1].Attachment, cs[2].Attachment, cs[3].Attachment)
	}
	form := h.site.lastForm()
	v := vars(t, form)
	if form.Get("doc_id") != "28686245017651778" || v["postID"] != "101" || v["sortOrder"] != "TOP" || v["after"] != nil {
		t.Errorf("first page asked %v %v", form.Get("doc_id"), v)
	}
	if ref := h.site.lastRequest("/api/graphql").Header.Get("referer"); ref != "https://www.threads.com/@maya.lens/post/C101" {
		t.Errorf("referer %q", ref)
	}
	later, more := load(0, true)
	form = h.site.lastForm()
	v = vars(t, form)
	if len(later) != 1 || more || form.Get("doc_id") != "30205808475684005" || v["after"] != "r-1" || v["first"] != 10.0 || v["postID"] != "101" {
		t.Errorf("later page %v %v %v", later, form.Get("doc_id"), v)
	}
	// A reply's replies are the reply's own page's.
	nested, _ := load(cs[0].ID, false)
	v = vars(t, h.site.lastForm())
	if len(nested) != 1 || nested[0].ReplyTo != cs[0].ID || v["postID"] != "701" {
		t.Errorf("nested %+v %v", nested, v)
	}
	if _, _, err := h.b.LoadComments(context.Background(), post+50, 0, false); err != errNoPost {
		t.Errorf("unknown post: %v", err)
	}
	if _, _, err := h.b.LoadComments(context.Background(), post, 99999, false); err != errNoComment {
		t.Errorf("unknown comment: %v", err)
	}
}

func TestThreadsHasNoChatsAndItsFilesComeOnlyFromTheCDN(t *testing.T) {
	h := newThreadsHarness(t)
	h.login()
	if more, err := h.b.LoadChats(context.Background(), 20); more || err != nil {
		t.Errorf("load_chats: %v %v", more, err)
	}
	if _, err := h.b.Search(context.Background(), "maya"); err != errThreadsNoChats {
		t.Errorf("search: %v", err)
	}
	if _, err := h.b.OpenDM(context.Background(), backend.UserRef{ID: 1}); err != errThreadsNoChats {
		t.Errorf("open_dm: %v", err)
	}
	if len(h.rec.events("chat")) != 0 {
		t.Error("a chat event")
	}
	if _, err := h.b.bundle(context.Background(), h.b.sess.as, "https://evil.example/rsrc.php/x.js"); err != errNotCDN {
		t.Errorf("a bundle from elsewhere: %v", err)
	}
}

func TestThreadsLogoutForgetsTheSessionAndWhatWasLearned(t *testing.T) {
	h := newThreadsHarness(t)
	h.login()
	h.b.net.Lock()
	h.b.cat.Queries[threadsFeedQuery] = threadsQuery{Name: threadsFeedQuery, Doc: "5", Kind: "query"}
	h.b.keepCatalog()
	h.b.net.Unlock()
	if err := h.b.Logout(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "threads")); !os.IsNotExist(err) {
		t.Error("the folder is still there")
	}
	if h.b.cat.Queries[threadsFeedQuery].Doc != "28783769534619828" {
		t.Error("the learned catalog outlived the logout")
	}
	if s := h.rec.events("account"); s[len(s)-1]["state"] != "logged_out" {
		t.Errorf("state %v", s)
	}
	if _, _, err := h.b.LoadFeed(context.Background(), 10, false, backend.Box{}, ""); err != errThreadsNotConnected {
		t.Errorf("feed after logout: %v", err)
	}
}

func TestThreadsWaitsBetweenRequests(t *testing.T) {
	h := newThreadsHarness(t)
	h.login()
	h.b.gap = 60 * time.Millisecond
	h.answers(tFeed("c", true, tThread("", tPost("1", "maya.lens", 19, "x"))), tFeed("", false, tThread("", tPost("2", "maya.lens", 19, "y"))))
	start := time.Now()
	h.feed(false, "")
	h.feed(false, "")
	if took := time.Since(start); took < 60*time.Millisecond {
		t.Errorf("two requests in %v", took)
	}
}
