// SPDX-License-Identifier: AGPL-3.0-or-later

package instagram

import (
	"context"
	"errors"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// tPostPage is a post's page as threads.com writes it, logged in (its form
// token dtsg-2), with the answers to the page's own queries inside, as the
// website preloads them: the replies' (which names the post with less of
// it) and the post's own.
func tPostPage(post string) string {
	downward := `{"__bbox":{"complete":true,"result":{"data":{"media":{"pk":"101","code":"C101","user":{"username":"maya.lens"},` +
		`"text_post_app_info":{"direct_replies":{"edges":[{"node":{"posts":{"edges":[{"node":` +
		tPost("701", "sam.climbs", 19, "a reply") + `}]}}}]}}}}}}}`
	target := `{"__bbox":{"complete":true,"result":{"data":{"data":{"edges":[{"node":{"thread_items":[{"post":` +
		post + `}]}}]}},"extensions":{"is_final":true}}}}`
	script := `<script type="application/json" data-sjs>{"__bbox": not json</script>` +
		`<script type="application/json" data-sjs>{"require":[["ScheduledServerJS","handle",null,[{"__bbox":{"require":[` +
		`["RelayPrefetchedStreamCache","next",[],["adp_BarcelonaPostPageDownwardQueryRelayPreloader_1",` + downward + `]],` +
		`["RelayPrefetchedStreamCache","next",[],["adp_BarcelonaPostPageTargetQueryRelayPreloader_2",` + target + `]]` +
		`]}}]]]}</script>`
	return strings.Replace(threadsHTML(true, "dtsg-2"), "</body>", script+"</body>", 1)
}

const mayaPost = "https://www.threads.com/@maya.lens/post/C101"

func TestAThreadsPostOpensFromItsPageAsALinkOpensIt(t *testing.T) {
	h := newThreadsHarness(t)
	h.login()
	h.site.pages[mayaPost] = tPostPage(tPost("101", "maya.lens", 19, "a post"))
	p, err := h.b.LoadPost(context.Background(), "C101", "maya.lens", backend.Box{})
	if err != nil {
		t.Fatalf("LoadPost: %v", err)
	}
	if p.ID == 0 || p.Caption != "a post" || p.URL != mayaPost || p.Author.Username != "maya.lens" ||
		p.Author.Name != "Maya.lens" || p.Context != "" || p.Continues || *p.Comments != 7 {
		t.Errorf("post %+v", p)
	}
	// Loaded as a link opened from another app: a navigation, from nowhere.
	r := h.site.lastRequest("/@maya.lens/post/C101")
	if r == nil || r.Method != "GET" {
		t.Fatalf("asked %v", h.site.asked())
	}
	for k, want := range map[string]string{"sec-fetch-site": "none", "sec-fetch-mode": "navigate", "sec-fetch-dest": "document", "referer": ""} {
		if got := r.Header.Get(k); got != want {
			t.Errorf("%s: %q", k, got)
		}
	}
	if !strings.Contains(r.Header.Get("cookie"), "sessionid=s3ss") {
		t.Error("without the session")
	}
	// Its replies come as the feed's posts' do, from its page, with the
	// session's tokens: the home page's, not the post page's.
	h.answers(tReplies("", false, tPost("701", "sam.climbs", 19, "a reply")))
	cs, _, err := h.b.LoadComments(context.Background(), p.ID, 0, false)
	if err != nil || len(cs) != 1 || cs[0].Text != "a reply" {
		t.Fatalf("replies %+v %v", cs, err)
	}
	form := h.site.lastForm()
	if v := vars(t, form); v["postID"] != "101" || form.Get("fb_dtsg") != "dtsg-1" {
		t.Errorf("replies asked %v with %s", v, form.Get("fb_dtsg"))
	}
	if ref := h.site.lastRequest("/api/graphql").Header.Get("referer"); ref != mayaPost {
		t.Errorf("referer %q", ref)
	}
	// The feed giving it later gives it the same id.
	h.answers(tFeed("", false, tThread("", tPost("101", "maya.lens", 19, "a post"))))
	if posts, _ := h.feed(false, ""); len(posts) != 1 || posts[0].ID != p.ID {
		t.Errorf("feed %+v, linked %d", posts, p.ID)
	}
}

func TestAThreadsPostsPageLeavesTheSessionOnTheHomePage(t *testing.T) {
	h := newThreadsHarness(t)
	h.login()
	ctx := context.Background()
	reply := tReplies("", false, tPost("701", "sam.climbs", 19, "a reply"))
	h.answers(tFeed("", false, tThread("", tPost("101", "maya.lens", 19, "a post"))), reply, reply)
	posts, _ := h.feed(false, "")
	if _, _, err := h.b.LoadComments(ctx, posts[0].ID, 0, false); err != nil {
		t.Fatal(err)
	}
	before := h.site.lastForm()

	// The post's page has other gates and another token, and sets a cookie.
	page := strings.ReplaceAll(tPostPage(tPost("101", "maya.lens", 19, "a post")), `"result":true,"hash"`, `"result":false,"hash"`)
	h.site.pages[mayaPost] = page
	h.site.pageHeader = []string{"Set-Cookie", "rur=rur9; Domain=.threads.com; Path=/; Secure"}
	p, err := h.b.LoadPost(ctx, "C101", "maya.lens", backend.Box{})
	if err != nil || p.ID != posts[0].ID {
		t.Fatalf("LoadPost: %+v %v", p, err)
	}
	if _, _, err := h.b.LoadComments(ctx, p.ID, 0, false); err != nil {
		t.Fatal(err)
	}
	after := h.site.lastForm()

	// The replies went as before it, one request later in the same page's
	// count, with the flags the home page's gates give...
	if after.Get("fb_dtsg") != "dtsg-1" || after.Get("__s") != before.Get("__s") || after.Get("__req") != "3" {
		t.Errorf("asked with %s, %s, request %s", after.Get("fb_dtsg"), after.Get("__s"), after.Get("__req"))
	}
	if !reflect.DeepEqual(vars(t, before), vars(t, after)) {
		t.Errorf("variables changed: %v, then %v", vars(t, before), vars(t, after))
	}
	// ...which aren't the post page's (else this would show nothing).
	q, err := h.b.cat.query(threadsRepliesQuery)
	if err != nil {
		t.Fatal(err)
	}
	given := map[string]any{"postID": "101", "sortOrder": "TOP"}
	home, _ := parseThreadsPage(threadsHTML(true, "dtsg-1"))
	post, _ := parseThreadsPage(page)
	if reflect.DeepEqual(h.b.cat.variables(q, given, &home), h.b.cat.variables(q, given, &post)) {
		t.Error("the post page's gates give the same flags")
	}
	// The cookie it set is the session's.
	if c := h.site.lastRequest("/api/graphql").Header.Get("cookie"); !strings.Contains(c, "rur=rur9") {
		t.Errorf("cookies %q", c)
	}
}

func TestReadingTheRepliesQueryAgainLeavesTheSessionOnTheHomePage(t *testing.T) {
	h := newThreadsHarness(t)
	h.login()
	h.answers(tFeed("", false, tThread("", tPost("101", "maya.lens", 19, "a post"))))
	posts, _ := h.feed(false, "")
	h.site.pages[mayaPost] = tPostPage(tPost("101", "maya.lens", 19, "a post"))
	h.site.graphql = func(url.Values) string {
		return `{"errors":[{"message":"A server error missing_required_variable_value occured.","code":1675012}]}`
	}
	if _, _, err := h.b.LoadComments(context.Background(), posts[0].ID, 0, false); err != errThreadsQueryChanged {
		t.Fatalf("got %v", err)
	}
	// The post's page was read for its scripts, as a navigation from home...
	if r := h.site.lastRequest(mayaPost); r.Header.Get("referer") != threadsBase+"/" || r.Header.Get("sec-fetch-site") != "same-origin" {
		t.Errorf("post page asked with %v", r.Header)
	}
	// ...and the session goes on from the home page.
	h.answers(tFeed("", false, tThread("", tPost("102", "maya.lens", 19, "another"))))
	h.feed(true, "")
	if form := h.site.lastForm(); form.Get("fb_dtsg") != "dtsg-1" || form.Get("__req") != "3" {
		t.Errorf("asked with %s, request %s", form.Get("fb_dtsg"), form.Get("__req"))
	}
}

func TestAThreadsShortLinkOpensByItsCodeAlone(t *testing.T) {
	h := newThreadsHarness(t)
	h.login()
	h.site.pages["https://www.threads.com/t/C101"] = tPostPage(tPost("101", "maya.lens", 19, "a post"))
	for _, username := range []string{"", "not a name"} {
		if p, err := h.b.LoadPost(context.Background(), "C101", username, backend.Box{}); err != nil || p.URL != mayaPost {
			t.Errorf("%q: %+v %v", username, p, err)
		}
	}
}

func TestAThreadsPostThatCantBeShownSaysWhy(t *testing.T) {
	h := newThreadsHarness(t)
	h.login()
	load := func(code string) error {
		_, err := h.b.LoadPost(context.Background(), code, "maya.lens", backend.Box{})
		return err
	}
	// No such page.
	if err := load("C404"); err != errThreadsPostGone {
		t.Errorf("404: %v", err)
	}
	// A page without it (a private account's, a code of none).
	h.site.pages["https://www.threads.com/@maya.lens/post/C102"] = threadsHTML(true, "dtsg-2")
	if err := load("C102"); err != errThreadsPostGone {
		t.Errorf("not on its page: %v", err)
	}
	// Taken down.
	h.site.pages["https://www.threads.com/@maya.lens/post/C101"] = tPostPage(tPost("101", "maya.lens", 19, "a post",
		`"text_post_app_info":`+tInfo(`"is_post_unavailable":true`)))
	if err := load("C101"); err != errThreadsPostGone {
		t.Errorf("unavailable: %v", err)
	}
	// Nothing to show.
	h.site.pages["https://www.threads.com/@maya.lens/post/C103"] = strings.ReplaceAll(
		tPostPage(tPost("103", "maya.lens", 19, "")), `"code":"C101"`, `"code":"C0"`)
	if err := load("C103"); err != errThreadsPostEmpty {
		t.Errorf("empty: %v", err)
	}
	// Not a code: nothing is asked.
	before := len(h.site.asked())
	if err := load("C1/../x"); err != errThreadsNotAPostCode {
		t.Errorf("not a code: %v", err)
	}
	if len(h.site.asked()) != before {
		t.Error("asked threads.com")
	}
	// A post's page that reads as nobody's doesn't end the session: the
	// home page is what says so.
	h.site.pages["https://www.threads.com/@maya.lens/post/C105"] = threadsHTML(false, "")
	var pe *proto.Error
	if err := load("C105"); !errors.As(err, &pe) || pe.Code != proto.NetworkError {
		t.Errorf("logged out page: %v", err)
	}
	h.b.mu.Lock()
	ready := h.b.ready
	h.b.mu.Unlock()
	if !ready || len(h.rec.events("account")) != 0 {
		t.Errorf("the session ended: %v", h.rec.events("account"))
	}
}

func TestTheThreadsPostOnAPageIsTheFullestWithItsCode(t *testing.T) {
	page := tPostPage(tPost("101", "maya.lens", 19, "a post"))
	m, ok := findThreadsPost(page, "C101")
	if !ok || m.Caption == nil || m.Caption.Text != "a post" || string(m.Pk) != "101" {
		t.Errorf("%+v %v", m, ok)
	}
	if m, ok := findThreadsPost(page, "C701"); !ok || m.Caption.Text != "a reply" {
		t.Errorf("the reply: %+v %v", m, ok)
	}
	for _, code := range []string{"C999", ""} {
		if _, ok := findThreadsPost(page, code); ok {
			t.Errorf("%q found", code)
		}
	}
	// A number pk reads as it was written.
	big := strings.Replace(tPostPage(tPost("101", "maya.lens", 19, "a post")), `"pk":"101","id"`, `"pk":3400000000000000101,"id"`, 1)
	if m, ok := findThreadsPost(big, "C101"); !ok || string(m.Pk) != "3400000000000000101" {
		t.Errorf("pk %q", m.Pk)
	}
}

func TestFindingAThreadsPostReadsItsPageOnceHoweverItsAnswersBreak(t *testing.T) {
	// Answers that never close, each inside the one before: read again
	// from each one's start, as decoding whole values does, a page of them
	// takes time in the square of its size.
	var open strings.Builder
	for range 3000 {
		open.WriteString(`{"__bbox":["` + strings.Repeat("a", 2000) + `",`)
	}
	// The post's own answer, inside them, closes, so it's found.
	post := `{"__bbox":{"result":` + tPost("101", "maya.lens", 19, "a post") + `}},`
	// And answers nested deeper than they're read are let go a stretch at
	// a time, each read once.
	deep := strings.Repeat(`{"__bbox":[`, 3*maxJSONNesting)
	for name, page := range map[string]string{
		"unclosed": `<script type="application/json">` + open.String() + post + `</script>`,
		"too deep": `<script type="application/json">` + post + deep + `</script>`,
	} {
		start := time.Now()
		m, ok := findThreadsPost(page, "C101")
		if took := time.Since(start); took > 2*time.Second {
			t.Errorf("%s: %v for %d KiB", name, took, len(page)>>10)
		}
		if !ok || m.Caption == nil || m.Caption.Text != "a post" {
			t.Errorf("%s: %+v %v", name, m, ok)
		}
	}
}
