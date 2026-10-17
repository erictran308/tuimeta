// SPDX-License-Identifier: AGPL-3.0-or-later

package instagram

// Threads, which runs on Instagram's servers and whose posts are
// Instagram's media dicts: a network with a feed and its posts' replies and
// no chats, read through threads.com's website with the session the pasted
// cookies are (threads.com's own, which an instagram.com session isn't).
// Only read: the website's pages and GraphQL queries, never a mutation, and
// none of its logging (threads_web.go). Files come from Instagram's CDN, as
// the feed's do.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/browser"
	"github.com/erictran308/tuimeta/helper/internal/cookies"
	"github.com/erictran308/tuimeta/helper/internal/history"
	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

var errThreadsNoChats = proto.Err(proto.Unsupported, "Threads has no chats in tuimeta: only its feed.")

// threadsSaved is what's kept in <data-dir>/threads/ between runs.
type threadsSaved struct {
	Version int             `json:"version"`
	Cookies []threadsCookie `json:"cookies"`
	// Browser is the browser the login named, "" for the libraries' own.
	Browser string `json:"browser,omitempty"`
}

// Threads is the Threads account.
type Threads struct {
	d backend.Deps
	// web is threads.com's client, and media the CDN's (tests swap their
	// transports).
	web   *http.Client
	media *http.Client

	// mu guards the account: its state, its session, the login it
	// belongs to.
	mu    sync.Mutex
	base  context.Context
	epoch int // counts logins, logouts and quitting: a stale attempt stops
	state proto.AccountState
	sess  *threadsSession // nil while logged out; ready once its page is in
	ready bool
	self  string // the account's Instagram user id
	name  string

	// net lets one request go to threads.com at a time, threadsGap apart
	// (last, gap), and guards the session's fields, the catalog of queries
	// and when it was last read again (found).
	net   sync.Mutex
	last  time.Time
	gap   time.Duration
	cat   *threadsCatalog
	found time.Time

	// feed is where the feed and the replies got to (threads_feed.go).
	feed threadsFeed
}

var (
	_ backend.Backend   = (*Threads)(nil)
	_ backend.Feeder    = (*Threads)(nil)
	_ backend.Commenter = (*Threads)(nil)
)

// NewThreads is the Threads account.
func NewThreads(deps backend.Deps) backend.Backend {
	return newThreads(deps, newThreadsWeb(), newMediaClient())
}

func newThreads(deps backend.Deps, web, media *http.Client) *Threads {
	t := &Threads{d: deps, web: web, media: media, base: context.Background(), state: proto.LoggedOut,
		gap: threadsGap, cat: builtinCatalog()}
	var saved threadsCatalog
	if ok, err := deps.Session.LoadJSON(catalogFile, &saved); err != nil {
		hlog.Warn("threads: saved catalog unreadable", hlog.Kind(err))
	} else if ok && saved.usable() {
		t.cat = &saved
	}
	return t
}

func (t *Threads) Network() proto.Network { return proto.Threads }

// account reports the account's state; t.mu is held, so states go out in
// the order they happen.
func (t *Threads) account(state proto.AccountState, msg string) {
	t.state = state
	if state == proto.Ready {
		id := t.d.IDs.User(proto.Threads, t.self)
		t.d.Events.Account(proto.Threads, state, id, t.name, "")
		t.d.Events.User(proto.User{ID: id, Network: proto.Threads, Name: t.name, Username: t.name, IsSelf: true})
		return
	}
	t.d.Events.Account(proto.Threads, state, 0, "", msg)
}

// Start resumes a saved session in the background.
func (t *Threads) Start(ctx context.Context) {
	s, ok := t.loadSession()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.base = ctx
	if !ok {
		t.account(proto.LoggedOut, "")
		return
	}
	t.sess = s
	t.account(proto.Connecting, "")
	epoch := t.epoch
	hlog.Go("threads resume", func() { t.keepTrying(s, epoch) })
}

// loadSession reads the saved cookies and browser; false if there are none
// to use.
func (t *Threads) loadSession() (*threadsSession, bool) {
	var saved threadsSaved
	ok, err := t.d.Session.LoadJSON(sessionFile, &saved)
	if err != nil {
		hlog.Warn("threads: saved session unreadable", hlog.Kind(err))
		return nil, false
	}
	if !ok || saved.Version != 1 {
		return nil, false
	}
	// A browser that doesn't parse wasn't written by this helper, and
	// resuming as some other browser is what a session must never do.
	as, err := browser.Parse(saved.Browser)
	if err != nil {
		hlog.Warn("threads: saved session names an unknown browser")
		return nil, false
	}
	s := &threadsSession{as: as}
	for _, c := range saved.Cookies {
		if c.Name != "" && cookieSafe(c.Name) && cookieSafe(c.Value) && len(s.cookies) < maxSessionCookies {
			s.cookies = append(s.cookies, c)
		}
	}
	for _, name := range cookies.Required[proto.Threads] {
		if s.cookie(name) == "" {
			return nil, false
		}
	}
	return s, true
}

// save keeps the session's cookies, which threads.com changes, and the
// browser it says it is, unless a login or logout since (epoch) made it
// stale: a logout wipes the folder, and saving after it would make it
// again. t.net is held (the cookies are read), and t.mu is taken.
func (t *Threads) save(s *threadsSession, epoch int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.epoch != epoch || t.sess != s {
		return
	}
	saved := threadsSaved{Version: 1, Cookies: append([]threadsCookie(nil), s.cookies...), Browser: s.as.Name()}
	if err := t.d.Session.SaveJSON(sessionFile, saved); err != nil {
		hlog.Error("threads: can't save the session", hlog.Kind(err))
		return
	}
	s.changed = false
}

// connect loads threads.com's home page with the session: it's logged in
// if the page has who. t.net is taken.
func (t *Threads) connect(ctx context.Context, s *threadsSession) error {
	t.net.Lock()
	defer t.net.Unlock()
	_, err := t.load(ctx, s, threadsBase+"/", "")
	return err
}

// establish makes a session whose page loaded the account's, and says it's
// ready; false if a login or logout since (epoch) made it stale.
func (t *Threads) establish(s *threadsSession, epoch int) bool {
	t.net.Lock()
	self, name := s.page.viewerID, s.page.username
	t.net.Unlock()
	t.mu.Lock()
	if t.epoch != epoch {
		t.mu.Unlock()
		return false
	}
	if t.self != "" && t.self != self {
		t.feed.reset() // another account: nothing of the old one carries over
	}
	t.sess, t.ready, t.self, t.name = s, true, self, name
	t.account(proto.Ready, "")
	t.mu.Unlock()
	t.net.Lock()
	t.save(s, epoch)
	t.net.Unlock()
	hlog.Info("threads: ready")
	return true
}

// keepTrying resumes a session, and keeps trying while Threads can't be
// reached; it gives up on anything else (the session's over, a
// checkpoint).
func (t *Threads) keepTrying(s *threadsSession, epoch int) {
	delay := retryFirst
	for {
		t.mu.Lock()
		base := t.base
		t.mu.Unlock()
		err := t.connect(base, s)
		if err == nil {
			t.establish(s, epoch)
			return
		}
		if base.Err() != nil || errors.Is(err, context.Canceled) {
			return
		}
		hlog.Warn("threads: connecting failed", hlog.Kind(err))
		pe := sessionError(err)
		t.mu.Lock()
		if t.epoch != epoch {
			t.mu.Unlock()
			return
		}
		if pe.Code != proto.NetworkError {
			if errors.Is(err, errLoggedOutPage) {
				// Threads has ended the session: the cookies are no use.
				if rerr := t.d.Session.Remove(sessionFile); rerr != nil {
					hlog.Warn("threads: can't delete the session", hlog.Kind(rerr))
				}
			}
			t.sess = nil
			t.account(proto.Errored, pe.Message)
			t.mu.Unlock()
			return
		}
		t.account(proto.Errored, "Can't reach Threads right now; tuimeta-helper keeps trying.")
		t.mu.Unlock()
		select {
		case <-base.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, retryMax)
	}
}

// sessionError is what the user is told about a session that didn't
// load: what to do when it's the session's trouble.
func sessionError(err error) *proto.Error {
	var pe *proto.Error
	switch {
	case errors.Is(err, errLoggedOutPage):
		return errThreadsSessionEnded
	case errors.As(err, &pe):
		return pe
	}
	return errThreadsUnreachable
}

// LoginCookies logs in with pasted cookies as the browser named: they're
// the session if threads.com's home page, loaded with them, is the
// account's. They're saved (and the browser) only then.
func (t *Threads) LoginCookies(ctx context.Context, set cookies.Set, as browser.Identity) error {
	s := &threadsSession{as: as}
	for _, c := range set.List() {
		if cookieSafe(c.Name) && cookieSafe(c.Value) && len(s.cookies) < maxSessionCookies {
			s.cookies = append(s.cookies, threadsCookie{c.Name, c.Value})
		}
	}
	t.mu.Lock()
	t.epoch++
	epoch := t.epoch
	t.sess, t.ready = nil, false
	t.account(proto.Connecting, "")
	t.mu.Unlock()

	err := t.connect(ctx, s)
	if err == nil && t.establish(s, epoch) {
		return nil
	}
	t.mu.Lock()
	if t.epoch == epoch {
		t.account(proto.LoggedOut, "")
	}
	t.mu.Unlock()
	switch {
	case err == nil:
		return proto.Err(proto.Cancelled, "A newer login or a logout came first.")
	case errors.Is(err, context.Canceled):
		return err
	case errors.Is(err, errLoggedOutPage):
		return errThreadsBadCookies
	}
	hlog.Info("threads: login failed", hlog.Kind(err))
	return sessionError(err)
}

// Logout forgets the session and everything kept for it. Like Instagram's
// it's local: the web session stays valid on Threads' side until it's
// ended there (README says so).
func (t *Threads) Logout(context.Context) error {
	t.mu.Lock()
	t.epoch++
	t.sess, t.ready, t.self, t.name = nil, false, "", ""
	t.mu.Unlock()
	t.feed.reset()
	if err := t.d.Session.Wipe(); err != nil {
		hlog.Warn("threads: can't delete the session", hlog.Kind(err))
	}
	t.net.Lock()
	t.cat, t.found = builtinCatalog(), time.Time{}
	t.net.Unlock()
	t.mu.Lock()
	t.account(proto.LoggedOut, "")
	t.mu.Unlock()
	return nil
}

// Close keeps the cookies as threads.com last changed them; nothing is
// said to Threads.
func (t *Threads) Close() {
	t.mu.Lock()
	s, epoch, ready := t.sess, t.epoch, t.ready
	t.mu.Unlock()
	if s != nil && ready && t.net.TryLock() {
		if s.changed {
			t.save(s, epoch)
		}
		t.net.Unlock()
	}
	t.mu.Lock()
	t.epoch++
	t.mu.Unlock()
}

// current is the session requests go through, once it's ready: a request
// can come while a saved one is still resuming.
func (t *Threads) current(ctx context.Context) (*threadsSession, int, error) {
	deadline := time.Now().Add(connectTimeout)
	for {
		t.mu.Lock()
		s, ready, epoch, state := t.sess, t.ready, t.epoch, t.state
		t.mu.Unlock()
		if s != nil && ready {
			return s, epoch, nil
		}
		if state != proto.Connecting || time.Now().After(deadline) {
			return nil, 0, errThreadsNotConnected
		}
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// ask sends query name with given variables, from the page from: with the
// page loaded again first when it's old, again and once more when Threads
// says its tokens expired, and with the query's definition read again from
// the website, once more, when Threads says it changed. Threads ending the
// session ends the account's readiness and says why.
func (t *Threads) ask(ctx context.Context, name string, given map[string]any, from string) ([]byte, error) {
	s, epoch, err := t.current(ctx)
	if err != nil {
		return nil, err
	}
	t.net.Lock()
	defer t.net.Unlock()
	defer func() {
		if s.changed {
			t.save(s, epoch)
		}
	}()
	if time.Since(s.pageAt) > pageLife {
		if _, err := t.load(ctx, s, threadsBase+"/", ""); err != nil {
			return nil, t.trouble(s, epoch, err)
		}
	}
	data, err := t.post(ctx, s, name, given, from)
	if errors.Is(err, errStaleTokens) {
		if _, lerr := t.load(ctx, s, threadsBase+"/", ""); lerr != nil {
			return nil, t.trouble(s, epoch, lerr)
		}
		data, err = t.post(ctx, s, name, given, from)
	}
	if errors.Is(err, errStaleQuery) {
		hlog.Info("threads: query out of date", hlog.Str("query", name))
		if derr := t.rediscover(ctx, s, name, from); derr != nil {
			hlog.Warn("threads: can't read the query again", hlog.Str("query", name), hlog.Kind(derr))
			return nil, t.trouble(s, epoch, derr)
		}
		data, err = t.post(ctx, s, name, given, from)
		if errors.Is(err, errStaleQuery) {
			return nil, errThreadsQueryChanged
		}
	}
	if err != nil {
		return nil, t.trouble(s, epoch, err)
	}
	return data, nil
}

// trouble is what a failed request tells the user, and the session's end
// when threads.com says it's over (or wants a checkpoint): the account
// says so, and nothing more is asked with it.
func (t *Threads) trouble(s *threadsSession, epoch int, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, errNotAQuery) || errors.Is(err, errStaleQuery) {
		return errThreadsQueryChanged
	}
	pe := sessionError(err)
	if pe.Code == proto.BadCookies || pe.Code == proto.Checkpoint {
		t.mu.Lock()
		if t.epoch == epoch && t.sess == s {
			t.ready = false
			t.account(proto.Errored, pe.Message)
		}
		t.mu.Unlock()
		if errors.Is(err, errLoggedOutPage) {
			if rerr := t.d.Session.Remove(sessionFile); rerr != nil {
				hlog.Warn("threads: can't delete the session", hlog.Kind(rerr))
			}
		}
	}
	return pe
}

// rediscover reads query name's definition again from the website's
// JavaScript: from the page from's bundles (the page loaded again, as a
// navigation, for its list), past those known to hold nothing, until it
// and the providers its flags read are found, at most maxDiscovery bytes,
// and not again within rediscoverEvery. t.net is held.
func (t *Threads) rediscover(ctx context.Context, s *threadsSession, name, from string) error {
	if !t.found.IsZero() && time.Since(t.found) < rediscoverEvery {
		return errStaleQuery
	}
	t.found = time.Now()
	// The home page as one typed in; a post's as one opened from it.
	opener := threadsBase + "/"
	if from == opener {
		opener = ""
	}
	page, err := t.load(ctx, s, from, opener)
	if err != nil {
		return err
	}
	// The query and the others like it (replies come with more replies).
	want := []string{name}
	if name == threadsRepliesQuery || name == threadsMoreQuery {
		want = []string{threadsRepliesQuery, threadsMoreQuery}
	}
	d := newDiscovery(want...)
	cat := newDiscovery().merge(t.cat)
	read, bundles := 0, 0
	for _, u := range page.bundles {
		if cat.isEmpty(u) {
			continue
		}
		js, err := t.bundle(ctx, s.as, u)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		read += len(js)
		bundles++
		if !d.scan(js) {
			cat.markEmpty(u)
		}
		if d.done() || read >= maxDiscovery {
			break
		}
	}
	hlog.Info("threads: read the website's code", hlog.Int("bundles", int64(bundles)), hlog.Int("kib", int64(read>>10)))
	if len(d.queries()) == 0 {
		// Nothing found, but what was read needn't be again.
		t.cat.Empty = cat.Empty
		t.keepCatalog()
		return errThreadsQueryChanged
	}
	next := d.merge(cat)
	if !next.usable() {
		return errThreadsQueryChanged
	}
	if _, err := next.query(name); err != nil {
		return err
	}
	t.cat = next
	t.keepCatalog()
	return nil
}

// keepCatalog saves the catalog; t.net is held.
func (t *Threads) keepCatalog() {
	if err := t.d.Session.SaveJSON(catalogFile, t.cat); err != nil {
		hlog.Warn("threads: can't save the catalog", hlog.Kind(err))
	}
}

// Fetch downloads a file of the feed from Instagram's CDN. An address that
// has expired isn't looked up again: a fresh load of the feed has new ones.
func (t *Threads) Fetch(ctx context.Context, ref ids.FileRef, w io.Writer) error {
	src, ok := ref.Source.(*mediaSource)
	if !ok || src == nil {
		return proto.ErrNoFile
	}
	t.mu.Lock()
	as := browser.Default()
	if t.sess != nil {
		as = t.sess.as
	}
	t.mu.Unlock()
	err := cdnClient{client: t.media, as: as, who: "threads"}.download(ctx, src.url, ref.Mime, w)
	if errors.Is(err, errExpired) {
		return proto.Err(proto.NetworkError, "Threads' link to this file has expired; load the feed again and try again.")
	}
	return err
}

// Threads has no chats: the server never routes a chat of it here, and
// what isn't about a chat says so.

func (t *Threads) LoadChats(context.Context, int) (bool, error) { return false, nil }
func (t *Threads) History(context.Context, backend.ChatRef, history.Query) (history.Page, error) {
	return history.Page{}, errThreadsNoChats
}
func (t *Threads) GetMessage(context.Context, backend.MessageRef) (proto.Message, error) {
	return proto.Message{}, errThreadsNoChats
}
func (t *Threads) Send(context.Context, *backend.Outgoing) error { return errThreadsNoChats }
func (t *Threads) EditText(context.Context, backend.MessageRef, string) error {
	return errThreadsNoChats
}
func (t *Threads) Delete(context.Context, backend.MessageRef) error        { return errThreadsNoChats }
func (t *Threads) React(context.Context, backend.MessageRef, string) error { return errThreadsNoChats }
func (t *Threads) MarkRead(context.Context, backend.MessageRef) error      { return errThreadsNoChats }
func (t *Threads) SetTyping(context.Context, backend.ChatRef, bool) error  { return errThreadsNoChats }
func (t *Threads) Mute(context.Context, backend.ChatRef, bool) error       { return errThreadsNoChats }
func (t *Threads) OpenDM(context.Context, backend.UserRef) (int64, error) {
	return 0, errThreadsNoChats
}
func (t *Threads) Search(context.Context, string) ([]proto.SearchResult, error) {
	return nil, errThreadsNoChats
}
