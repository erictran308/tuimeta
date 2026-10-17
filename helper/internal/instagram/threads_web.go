// SPDX-License-Identifier: AGPL-3.0-or-later

package instagram

// Threads' website, as the browser the session came from uses it: its
// pages, loaded as a navigation, its GraphQL queries, sent as the page's
// own code sends them, and its JavaScript, fetched without cookies as a
// page's scripts are. Every request goes through Chrome's handshake (the
// libraries' uTLS Chrome) and carries the session's browser's headers. One
// request to threads.com at a time, at least threadsGap apart: nothing
// pages faster than a person scrolls.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/imroc/req/v3"
	"go.mau.fi/mautrix-meta/pkg/messagix/methods"
	"go.mau.fi/mautrix-meta/pkg/messagix/useragent"

	"github.com/erictran308/tuimeta/helper/internal/browser"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

const (
	threadsBase = "https://www.threads.com"
	// threadsGap is the least time between two requests to threads.com.
	threadsGap = 2 * time.Second
	// pageLife is how long a page's tokens and gates are used before the
	// page is loaded again.
	pageLife = time.Hour
	// Sizes past which an answer isn't read: a page is about half a
	// megabyte, a page of the feed a few hundred kilobytes, a bundle a
	// megabyte or two.
	maxThreadsPage   = 8 << 20
	maxThreadsAnswer = 16 << 20
	maxBundleSize    = 8 << 20
	// maxDiscovery is the most JavaScript one rediscovery reads, and
	// rediscoverEvery the least time between two.
	maxDiscovery    = 60 << 20
	rediscoverEvery = 10 * time.Minute
	// maxSessionCookies bounds the cookies kept: threads.com sets a few.
	maxSessionCookies = 64
	// maxRedirects is how many of threads.com's own redirects a page
	// follows.
	maxRedirects = 3
)

var (
	errThreadsBadCookies = proto.Err(proto.BadCookies,
		"Threads didn't accept these cookies; copy them again from a tab where you're logged in to threads.com.")
	errThreadsCheckpoint = proto.Err(proto.Checkpoint,
		"Threads wants you to confirm it's you: open threads.com in a browser, then log in here again.")
	errThreadsSuspended = proto.Err(proto.Checkpoint,
		"Threads says this account is suspended or disabled: open threads.com in a browser to see why.")
	errThreadsSessionEnded = proto.Err(proto.BadCookies,
		"Threads ended this session; log in again with fresh cookies from threads.com.")
	errThreadsUnreachable = proto.Err(proto.NetworkError,
		"Couldn't reach Threads; check your connection and try again.")
	errThreadsRateLimited = proto.Err(proto.NetworkError,
		"Threads is limiting requests right now; wait a few minutes and try again.")
	errThreadsNotConnected = proto.Err(proto.NetworkError,
		"tuimeta-helper isn't connected to Threads right now; try again in a moment.")
	errThreadsPage = proto.Err(proto.NetworkError,
		"Threads' page couldn't be read; try again later.")
	errThreadsQueryChanged = proto.Err(proto.NetworkError,
		"Threads changed how its website asks for this, and tuimeta-helper couldn't follow; try again later.")
)

// Why a GraphQL answer said no, for what to do about it.
var (
	// errStaleQuery: the query's definition changed (a variable it now
	// needs, a doc id it doesn't know): read it again from the website.
	errStaleQuery = errors.New("threads: query out of date")
	// errStaleTokens: the page's tokens expired: load the page again.
	errStaleTokens = errors.New("threads: page tokens out of date")
	// errLoggedOutPage: threads.com answered as to nobody logged in.
	errLoggedOutPage = errors.New("threads: logged out")
)

// threadsCookie is one cookie of the session.
type threadsCookie struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// threadsSession is a logged-in session: its cookies, in the order the
// browser sent them, the browser it says it is, and what its last page
// gave. Its fields change only under Threads.net.
type threadsSession struct {
	cookies []threadsCookie
	as      browser.Identity
	page    *threadsPage
	pageAt  time.Time
	// requests counts the GraphQL requests sent (__req); sid is the page's
	// session id (__s), as the website makes one per page load.
	requests int
	sid      string
	// changed says the cookies changed since they were last saved.
	changed bool
}

func (s *threadsSession) cookie(name string) string {
	for _, c := range s.cookies {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

func (s *threadsSession) header() string {
	parts := make([]string, len(s.cookies))
	for i, c := range s.cookies {
		parts[i] = c.Name + "=" + c.Value
	}
	return strings.Join(parts, "; ")
}

// take keeps what threads.com set, as the browser's cookie jar would: a
// new value, a new cookie (after the others), or one deleted.
func (s *threadsSession) take(resp *http.Response, now time.Time) {
	for _, c := range resp.Cookies() {
		domain := strings.TrimPrefix(strings.ToLower(c.Domain), ".")
		if domain != "" && domain != "threads.com" && domain != "www.threads.com" {
			continue
		}
		if c.Name == "" || !cookieSafe(c.Name) || !cookieSafe(c.Value) {
			continue
		}
		gone := c.MaxAge < 0 || (!c.Expires.IsZero() && c.Expires.Before(now))
		at := -1
		for i, have := range s.cookies {
			if have.Name == c.Name {
				at = i
			}
		}
		switch {
		case gone && at >= 0:
			s.cookies = append(s.cookies[:at], s.cookies[at+1:]...)
			s.changed = true
		case gone:
		case at >= 0 && s.cookies[at].Value != c.Value:
			s.cookies[at].Value = c.Value
			s.changed = true
		case at < 0 && len(s.cookies) < maxSessionCookies:
			s.cookies = append(s.cookies, threadsCookie{c.Name, c.Value})
			s.changed = true
		}
	}
}

// cookieSafe reports whether s can go in a Cookie header as it is.
func cookieSafe(s string) bool {
	if len(s) > 4096 {
		return false
	}
	for _, r := range s {
		if r <= 0x20 || r >= 0x7f || r == ';' || r == ',' || r == '"' || r == '\\' {
			return false
		}
	}
	return true
}

// newThreadsWeb is the client threads.com's requests go through: Chrome's
// handshake, no proxy from the environment, no cookie jar (the session's
// cookies are sent by hand), and no redirect followed by itself, so one to
// the login page or a checkpoint is seen.
func newThreadsWeb() *http.Client {
	cli := req.WithTransportOverride(httpSettings(), req.C().ImpersonateChrome()).Compile()
	cli.Jar = nil
	cli.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return cli
}

// hints sets the identity's user agent and client hints, as Chrome sends
// them to a site that asked for them all (threads.com does, in Accept-CH).
func hints(h http.Header, as browser.Identity, page bool) {
	platform, version := strconv.Quote(as.Platform), strconv.Quote(as.PlatformVersion)
	h.Set("user-agent", as.UserAgent)
	h.Set("sec-ch-ua", as.Brands)
	h.Set("sec-ch-ua-mobile", useragent.SecCHMobile)
	h.Set("sec-ch-ua-platform", platform)
	h.Set("sec-ch-ua-full-version-list", as.FullVersionList)
	h.Set("sec-ch-ua-platform-version", version)
	h.Set("sec-ch-ua-model", useragent.SecCHModel)
	h.Set("dpr", "2")
	if page {
		h.Set("viewport-width", "1512")
		h.Set("sec-ch-prefers-color-scheme", "dark")
	}
	h.Set("accept-language", "en-US,en;q=0.9")
}

// wait holds a request to threads.com back until threadsGap has passed
// since the last; t.net is held.
func (t *Threads) wait(ctx context.Context) error {
	if !t.last.IsZero() {
		if left := time.Until(t.last.Add(t.gap)); left > 0 {
			timer := time.NewTimer(left)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	t.last = time.Now()
	return nil
}

// load loads a page of threads.com as a navigation does (from is the page
// it was opened from; none for one typed in), following threads.com's own
// redirects, and reads it: a fresh page for the session's requests. t.net
// is held.
func (t *Threads) load(ctx context.Context, s *threadsSession, page, from string) (*threadsPage, error) {
	p, _, err := t.open(ctx, s, page, from)
	if errors.Is(err, errPageGone) {
		err = errThreadsPage
	}
	return p, err
}

// errPageGone is threads.com saying it has no such page: a post deleted,
// or a code of none.
var errPageGone = errors.New("threads: no such page")

// open is load's, with the page as it came. t.net is held.
func (t *Threads) open(ctx context.Context, s *threadsSession, page, from string) (*threadsPage, string, error) {
	for range maxRedirects + 1 {
		if err := t.wait(ctx); err != nil {
			return nil, "", err
		}
		r, err := http.NewRequestWithContext(ctx, http.MethodGet, page, nil)
		if err != nil {
			return nil, "", errThreadsPage
		}
		h := r.Header
		hints(h, s.as, true)
		h.Set("accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7")
		h.Set("upgrade-insecure-requests", "1")
		h.Set("sec-fetch-dest", "document")
		h.Set("sec-fetch-mode", "navigate")
		h.Set("sec-fetch-user", "?1")
		if from == "" {
			h.Set("sec-fetch-site", "none")
		} else {
			h.Set("sec-fetch-site", "same-origin")
			h.Set("referer", from)
		}
		h.Set("cookie", s.header())
		resp, err := t.web.Do(r)
		if err != nil {
			return nil, "", netError(ctx, err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxThreadsPage+1))
		resp.Body.Close()
		s.take(resp, time.Now())
		if err != nil {
			return nil, "", netError(ctx, err)
		}
		switch {
		case resp.StatusCode >= 300 && resp.StatusCode < 400:
			next, err := redirect(resp)
			if err != nil {
				return nil, "", err
			}
			from, page = page, next
			continue
		case resp.StatusCode == http.StatusTooManyRequests:
			return nil, "", errThreadsRateLimited
		case resp.StatusCode == http.StatusNotFound:
			return nil, "", errPageGone
		case resp.StatusCode != http.StatusOK || len(body) > maxThreadsPage:
			return nil, "", errThreadsPage
		}
		html := string(body)
		p, ok := parseThreadsPage(html)
		if !ok {
			return nil, "", errLoggedOutPage
		}
		s.page, s.pageAt = &p, time.Now()
		s.requests = 0
		s.sid = methods.GenerateWebsessionID(true)
		return &p, html, nil
	}
	return nil, "", errThreadsPage
}

// redirect is where threads.com sent a page: another of its own pages, or
// what the session's trouble is (the login page, a checkpoint).
func redirect(resp *http.Response) (string, error) {
	loc, err := resp.Location()
	if err != nil {
		return "", errThreadsPage
	}
	return sentTo(loc)
}

func sentTo(loc *url.URL) (string, error) {
	p := strings.ToLower(loc.Path)
	switch {
	case strings.Contains(p, "challenge") || strings.Contains(p, "checkpoint"):
		return "", errThreadsCheckpoint
	case strings.Contains(p, "suspended") || strings.Contains(p, "disabled"):
		return "", errThreadsSuspended
	case strings.Contains(p, "login"):
		return "", errLoggedOutPage
	}
	host := strings.ToLower(loc.Hostname())
	if loc.Scheme != "https" || (host != "www.threads.com" && host != "threads.com") {
		return "", errThreadsPage
	}
	return loc.String(), nil
}

// netError is what a request that didn't get an answer tells the user.
func netError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	return errThreadsUnreachable
}

// form is what a GraphQL request of q carries, as the page's code sends
// it, besides what its tokens are; t.net is held.
func threadsForm(s *threadsSession, q threadsQuery, vars map[string]any) (url.Values, error) {
	p := s.page
	encoded, err := json.Marshal(vars)
	if err != nil {
		return nil, err
	}
	s.requests++
	form := url.Values{}
	set := func(k, v string) {
		if v != "" {
			form.Set(k, v)
		}
	}
	set("av", p.actor)
	set("__user", "0")
	set("__a", "1")
	set("__req", strconv.FormatInt(int64(s.requests), 36))
	set("__hs", p.haste)
	set("dpr", p.dpr)
	set("__ccg", p.ccg)
	set("__rev", p.rev)
	set("__s", s.sid)
	set("__hsi", p.hsi)
	set("__comet_req", p.cometReq)
	set("fb_dtsg", p.dtsg)
	set("jazoest", jazoest(p.dtsg))
	set("lsd", p.lsd)
	set("__spin_r", p.spinR)
	set("__spin_b", p.spinB)
	set("__spin_t", p.spinT)
	set("__crn", p.crn)
	set("fb_api_caller_class", "RelayModern")
	set("fb_api_req_friendly_name", q.Name)
	set("variables", string(encoded))
	set("server_timestamps", "true")
	set("doc_id", q.Doc)
	return form, nil
}

// post sends one GraphQL query from the page from (its referer) and
// returns the answer's data, or why there's none. It's the one place a
// GraphQL request is made, and it sends only a query the catalog says is
// one; t.net is held.
func (t *Threads) post(ctx context.Context, s *threadsSession, name string, given map[string]any, from string) (json.RawMessage, error) {
	q, err := t.cat.query(name)
	if err != nil {
		return nil, err
	}
	if !s.page.usable() {
		return nil, errStaleTokens
	}
	form, err := threadsForm(s, q, t.cat.variables(q, given, s.page))
	if err != nil {
		return nil, err
	}
	if err := t.wait(ctx); err != nil {
		return nil, err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, threadsBase+"/api/graphql", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	h := r.Header
	hints(h, s.as, false)
	h.Set("accept", "*/*")
	h.Set("content-type", "application/x-www-form-urlencoded")
	h.Set("origin", threadsBase)
	h.Set("referer", from)
	h.Set("sec-fetch-dest", "empty")
	h.Set("sec-fetch-mode", "cors")
	h.Set("sec-fetch-site", "same-origin")
	h.Set("x-asbd-id", useragent.ASBDID)
	h.Set("x-csrftoken", s.cookie("csrftoken"))
	h.Set("x-fb-friendly-name", q.Name)
	h.Set("x-fb-lsd", s.page.lsd)
	h.Set("x-ig-app-id", s.page.appID)
	h.Set("cookie", s.header())
	resp, err := t.web.Do(r)
	if err != nil {
		return nil, netError(ctx, err)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxThreadsAnswer+1))
	resp.Body.Close()
	s.take(resp, time.Now())
	if err != nil {
		return nil, netError(ctx, err)
	}
	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		if _, err := redirect(resp); err != nil {
			return nil, err
		}
		return nil, errThreadsPage
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, errThreadsRateLimited
	case resp.StatusCode != http.StatusOK || len(body) > maxThreadsAnswer:
		return nil, errThreadsPage
	}
	return graphqlData(body)
}

// graphqlData is an answer's data, or what its errors mean. An answer can
// be several JSON objects (a query's deferred parts come after it): the
// first holds the data.
func graphqlData(body []byte) (json.RawMessage, error) {
	body = bytes.TrimPrefix(bytes.TrimSpace(body), []byte("for (;;);"))
	var a struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
			Code    int64  `json:"code"`
		} `json:"errors"`
		// The older answer of an endpoint that won't serve the session.
		Error   int64  `json:"error"`
		Summary string `json:"errorSummary"`
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&a); err != nil {
		return nil, errThreadsPage
	}
	switch a.Error {
	case 0:
	case 1357001:
		return nil, errLoggedOutPage
	case 1357004, 1357053:
		return nil, errStaleTokens
	default:
		return nil, errThreadsPage
	}
	if present(a.Data) {
		return a.Data, nil
	}
	for _, e := range a.Errors {
		msg := strings.ToLower(e.Message)
		switch {
		case e.Code == 1675012 || strings.Contains(msg, "missing_required_variable") ||
			strings.Contains(msg, "persisted") || strings.Contains(msg, "doc_id") || strings.Contains(msg, "document"):
			return nil, errStaleQuery
		case strings.Contains(msg, "login") || strings.Contains(msg, "logged out"):
			return nil, errLoggedOutPage
		case strings.Contains(msg, "rate limit") || strings.Contains(msg, "too many"):
			return nil, errThreadsRateLimited
		}
	}
	return nil, errThreadsPage
}

// bundle fetches one of the website's JavaScript files, as a page's script
// with crossorigin is fetched: without cookies, and only from Instagram's
// static host.
func (t *Threads) bundle(ctx context.Context, as browser.Identity, raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "static.cdninstagram.com" || !strings.HasPrefix(u.Path, "/rsrc.php/") {
		return "", errNotCDN
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return "", errNotCDN
	}
	h := r.Header
	hints(h, as, false)
	h.Del("dpr")
	h.Set("accept", "*/*")
	h.Set("origin", threadsBase)
	h.Set("referer", threadsBase+"/")
	h.Set("sec-fetch-dest", "script")
	h.Set("sec-fetch-mode", "cors")
	h.Set("sec-fetch-site", "cross-site")
	resp, err := t.web.Do(r)
	if err != nil {
		return "", netError(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errFetch
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBundleSize+1))
	if err != nil {
		return "", netError(ctx, err)
	}
	if len(body) > maxBundleSize {
		return "", errFetch
	}
	return string(body), nil
}
