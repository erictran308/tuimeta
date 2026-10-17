// SPDX-License-Identifier: AGPL-3.0-or-later

package instagram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/browser"
	"github.com/erictran308/tuimeta/helper/internal/cookies"
	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
	"github.com/erictran308/tuimeta/helper/internal/session"
)

// What the made-up threads.com is made of: nothing in it is anyone's.
const (
	threadsCookieText = "sessionid=s3ss; ds_user_id=9000001; csrftoken=csrf1; mid=mid1; rur=rur1"
	threadsViewer     = "9000001"
	threadsActor      = "17840000000000001"
	bundleOne         = "https://static.cdninstagram.com/rsrc.php/v4/yA/r/one.js"
	bundleTwo         = "https://static.cdninstagram.com/rsrc.php/v4/yB/l/en_US-j/two.js"
	bundleThree       = "https://static.cdninstagram.com/rsrc.php/v4/yC/l/en_US-j/three.js"
)

// threadsHTML is a page of threads.com as the website writes it, logged in
// (or, with no viewer, not), with dtsg as its form token.
func threadsHTML(viewer bool, dtsg string) string {
	actor, nonFB, who := threadsActor, threadsActor, `{"fbid":"`+threadsActor+`","id":"`+threadsViewer+`","interop_messaging_user_fbid":"`+threadsActor+`","username":"robin.hale","profile_picture_url":"https:\/\/scontent.cdninstagram.com\/v\/a.jpg"}`
	if !viewer {
		actor, nonFB, who = "0", "0", "null"
	}
	return `<!DOCTYPE html><html><head><script src="` + bundleOne + `" crossorigin="anonymous"></script>
<link rel="preload" href="` + bundleOne + `" as="script"></head><body><script>
["SiteData",[],{"server_revision":1001,"client_revision":1001,"push_phase":"C3","pkg_cohort":"HYP:barcelona_web_pkg","haste_session":"20380.HYP:barcelona_web_pkg.2.1...0","pr":2,"hsi":"7000000000000000001","comet_env":29,"spin":4,"__spin_r":1001,"__spin_b":"trunk","__spin_t":1792200000},317],
["CurrentUserInitialData",[],{"ACCOUNT_ID":"0","USER_ID":"0","NAME":"","APP_ID":"238260118697367","NON_FACEBOOK_USER_ID":"` + nonFB + `","IS_THREADS_USER":1},270],
["DTSGInitialData",[],{"token":"` + dtsg + `"},258],["LSD",[],{"token":"lsd-token"},323],
["WebConnectionClassServerGuess",[],{"connectionClass":"EXCELLENT"},4705],
["RelayAPIConfigDefaults",[],{"accessToken":"","actorID":"` + actor + `","customHeaders":{"X-IG-App-ID":"238260118697367"}},5094],
{"route":{"canonicalRouteName":"comet.threads.BarcelonaHomeRouteV2"},"viewer":` + who + `}
{"gkxData":{"7479":{"result":true,"hash":"a"},"6542":{"result":false,"hash":"b"},"899":{"result":true,"hash":"c"},"14002":{"result":true,"hash":"d"},"27036":{"result":true,"hash":"e"}}}
{"qexData":{"5477":{"r":true,"l":null},"518":{"r":null,"l":null},"3519":{"r":null}}}
{"92":{"value":true}}
"https:\/\/static.cdninstagram.com\/rsrc.php\/v4\/yB\/l\/en_US-j\/two.js" "https:\/\/static.cdninstagram.com\/rsrc.php\/v4\/yC\/l\/en_US-j\/three.js" "` + bundleOne + `"
</script></body></html>`
}

// threadsSite is a made-up threads.com: its home page and posts' pages,
// its GraphQL endpoint and its JavaScript, recording what was asked.
type threadsSite struct {
	t  testing.TB
	mu sync.Mutex
	// home answers the home page: a status, a body and headers.
	home func(r *http.Request) *http.Response
	// pages are other pages, by address.
	pages map[string]string
	// graphql answers a query, by its form.
	graphql func(form url.Values) string
	bundles map[string]string

	reqs  []*http.Request
	forms []url.Values
}

func newThreadsSite(t testing.TB) *threadsSite {
	s := &threadsSite{t: t, pages: map[string]string{}, bundles: map[string]string{}}
	s.home = func(r *http.Request) *http.Response { return respond(200, threadsHTML(true, "dtsg-1"))(r) }
	s.graphql = func(url.Values) string { return `{"data":null,"errors":[{"message":"not stubbed","code":1}]}` }
	return s
}

func (s *threadsSite) RoundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.reqs = append(s.reqs, r)
	s.mu.Unlock()
	switch {
	case r.URL.Host == "www.threads.com" && r.URL.Path == "/" && r.Method == http.MethodGet:
		return s.home(r), nil
	case r.URL.Host == "www.threads.com" && r.URL.Path == "/api/graphql" && r.Method == http.MethodPost:
		body, _ := io.ReadAll(r.Body)
		form, err := url.ParseQuery(string(body))
		if err != nil {
			s.t.Errorf("a form that doesn't read: %v", err)
		}
		s.mu.Lock()
		s.forms = append(s.forms, form)
		answer := s.graphql
		s.mu.Unlock()
		return respond(200, answer(form), "Content-Type", "text/javascript; charset=utf-8")(r), nil
	case r.URL.Host == "www.threads.com":
		s.mu.Lock()
		page, ok := s.pages[r.URL.String()]
		s.mu.Unlock()
		if ok {
			return respond(200, page)(r), nil
		}
		return respond(404, "")(r), nil
	case r.URL.Host == "static.cdninstagram.com":
		s.mu.Lock()
		js, ok := s.bundles[r.URL.String()]
		s.mu.Unlock()
		if ok {
			return respond(200, js)(r), nil
		}
		return respond(404, "")(r), nil
	}
	s.t.Errorf("a request went to %s", r.URL.Host)
	return nil, fmt.Errorf("no such site")
}

// asked is what was requested, as "METHOD url".
func (s *threadsSite) asked() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, r := range s.reqs {
		out = append(out, r.Method+" "+r.URL.String())
	}
	return out
}

func (s *threadsSite) lastForm() url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.forms) == 0 {
		s.t.Fatal("no GraphQL request was made")
	}
	return s.forms[len(s.forms)-1]
}

func (s *threadsSite) lastRequest(path string) *http.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.reqs) - 1; i >= 0; i-- {
		if s.reqs[i].URL.Path == path || s.reqs[i].URL.String() == path {
			return s.reqs[i]
		}
	}
	s.t.Fatalf("nothing asked for %s", path)
	return nil
}

// vars is a form's variables.
func vars(t testing.TB, form url.Values) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(form.Get("variables")), &v); err != nil {
		t.Fatalf("variables: %v", err)
	}
	return v
}

type threadsHarness struct {
	t    *testing.T
	b    *Threads
	site *threadsSite
	rec  *recorder
	dir  string
	deps backend.Deps
}

func newThreadsHarness(t *testing.T) *threadsHarness {
	t.Helper()
	return newThreadsHarnessIn(t, t.TempDir())
}

func newThreadsHarnessIn(t *testing.T, dir string) *threadsHarness {
	t.Helper()
	store, err := ids.Open(filepath.Join(dir, "ids.json"))
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	events := backend.NewEvents(rec.add)
	messages := ids.NewMessages()
	deps := backend.Deps{
		Events: events, IDs: store, Messages: messages, Files: ids.NewFiles(),
		Outbox: backend.NewOutbox(events, messages), Session: session.New(dir, proto.Threads),
	}
	site := newThreadsSite(t)
	web := &http.Client{Transport: site, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	h := &threadsHarness{t: t, site: site, rec: rec, dir: dir, deps: deps}
	h.b = newThreads(deps, web, &http.Client{Transport: noNetwork{t}})
	h.b.gap = 0
	t.Cleanup(h.b.Close)
	return h
}

// login logs in with the made-up cookies as Chrome 150, and forgets the
// events so far.
func (h *threadsHarness) login() {
	h.t.Helper()
	if err := h.loginWith(threadsCookieText); err != nil {
		h.t.Fatalf("login: %v", err)
	}
	h.rec.reset()
}

func (h *threadsHarness) loginWith(text string) error {
	set, err := cookies.Parse(proto.Threads, text)
	if err != nil {
		h.t.Fatal(err)
	}
	as, err := browser.Parse("Chrome 150.0.7712.45")
	if err != nil {
		h.t.Fatal(err)
	}
	return h.b.LoginCookies(context.Background(), set, as)
}

// --- answers ----------------------------------------------------------------

// tUser is a Threads user dict.
func tUser(username, name string) string {
	return fmt.Sprintf(`{"friendship_status":{"following":true},"id":"%s","pk":"%s","profile_pic_url":"%s/v/t51.2885-19/%s_n.jpg?oh=sig","username":"%s","full_name":"%s","is_verified":false}`,
		strings.Repeat("1", 8), strings.Repeat("1", 8), cdn, username, username, name)
}

// tImages is an image_versions2 of a picture w×h, and one half its size.
func tImages(name string, w, h int) string {
	return fmt.Sprintf(`{"candidates":[{"width":%d,"height":%d,"url":"%s/v/%s_full.jpg?stp=a&oh=s"},{"width":%d,"height":%d,"url":"%s/v/%s_full.jpg?stp=b&oh=s"}]}`,
		w, h, cdn, name, w/2, h/2, cdn, name)
}

// tPost is a Threads post: pk, author, media type (19 text, 1 photo, 2
// video, 8 carousel), its text, and anything else as raw fields.
func tPost(pk, username string, mediaType int, text string, more ...string) string {
	fields := []string{
		fmt.Sprintf(`"pk":"%s"`, pk),
		fmt.Sprintf(`"id":"%s_9000"`, pk),
		fmt.Sprintf(`"code":"C%s"`, pk),
		`"user":` + tUser(username, strings.ToUpper(username[:1])+username[1:]),
		fmt.Sprintf(`"media_type":%d`, mediaType),
		fmt.Sprintf(`"caption":{"text":%q,"pk":"1"}`, text),
		`"taken_at":1792200000`,
		`"like_count":42`,
		`"view_state_item_type":132`,
		`"image_versions2":{"candidates":[]}`,
	}
	hasInfo := false
	for _, m := range more {
		if strings.HasPrefix(m, `"text_post_app_info"`) {
			hasInfo = true
		}
	}
	if !hasInfo {
		fields = append(fields, `"text_post_app_info":`+tInfo(``))
	}
	return "{" + strings.Join(append(fields, more...), ",") + "}"
}

// tInfo is a text_post_app_info with the counts, and extra fields.
func tInfo(extra string) string {
	info := `{"direct_reply_count":7,"repost_count":2,"quote_count":1,"reshare_count":5,"is_post_unavailable":false,"share_info":{"reposted_post":null,"quoted_post":null}`
	if extra != "" {
		info += "," + extra
	}
	return info + "}"
}

// tThread is a feed edge of a thread of posts, with the line over it.
func tThread(context string, posts ...string) string {
	header := "null"
	if context != "" {
		header = fmt.Sprintf(`{"context_type":"first_post","text":%q}`, context)
	}
	items := make([]string, len(posts))
	for i, p := range posts {
		items[i] = `{"post":` + p + `}`
	}
	return `{"node":{"text_post_app_thread":{"thread_header_context":` + header + `,"thread_items":[` + strings.Join(items, ",") +
		`],"thread_type":"thread","id":"T1"},"suggested_users":null,"__typename":"XDTFeedItem"},"cursor":null}`
}

// tPeople is a feed edge of people to follow.
const tPeople = `{"node":{"text_post_app_thread":null,"suggested_users":{"id":"S1","title":"Suggested for you","suggestion_cards":[{"user":{}}],"view_state_item_type":223},"__typename":"XDTFeedItem"},"cursor":null}`

// tFeed is an answer with a page of the feed.
func tFeed(cursor string, more bool, edges ...string) string {
	return fmt.Sprintf(`{"data":{"feedData":{"edges":[%s],"pagination_source":"text_post_feed_threads","session_id":null,"page_info":{"end_cursor":%q,"has_next_page":%v,"has_previous_page":false,"start_cursor":null}}},"extensions":{"is_final":true}}`,
		strings.Join(edges, ","), cursor, more)
}

// tReplies is an answer with a page of a post's replies.
func tReplies(cursor string, more bool, posts ...string) string {
	edges := make([]string, len(posts))
	for i, p := range posts {
		edges[i] = `{"node":{"fetch_token":"f","posts":{"edges":[{"node":` + p + `}],"page_info":{}},"post_ids":{"edges":[]},"__typename":"XDTReply"},"cursor":null}`
	}
	return fmt.Sprintf(`{"data":{"media":{"text_post_app_info":{"direct_replies":{"edges":[%s],"page_info":{"end_cursor":%q,"has_next_page":%v}},"id":"x"},"id":"y"}},"extensions":{"is_final":true}}`,
		strings.Join(edges, ","), cursor, more)
}
