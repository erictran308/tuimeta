// SPDX-License-Identifier: AGPL-3.0-or-later

// Package instagram is the Instagram backend: mautrix-meta's instameow,
// driven the way the mautrix-instagram bridge's connector drives it, with
// none of the bridge running. Instagram's ids, messages and events become
// the protocol's (PROTOCOL.md).
package instagram

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
	"go.mau.fi/mautrix-meta/pkg/instameow"
	"go.mau.fi/mautrix-meta/pkg/instameow/slidetypes"
	mcookies "go.mau.fi/mautrix-meta/pkg/messagix/cookies"
	"go.mau.fi/mautrix-meta/pkg/messagix/dgw"
	"go.mau.fi/mautrix-meta/pkg/messagix/httpclient"
	"go.mau.fi/mautrix-meta/pkg/messagix/types"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/browser"
	"github.com/erictran308/tuimeta/helper/internal/cookies"
	"github.com/erictran308/tuimeta/helper/internal/history"
	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

const (
	sessionFile = "session.json"
	// connectTimeout is how long a login waits for Instagram's socket.
	connectTimeout = 45 * time.Second
	// refreshEvery is how often the whole connection is made again (the
	// connector's default force refresh), so a long run doesn't go stale.
	refreshEvery = 20 * time.Hour
	// retryFirst and retryMax space out attempts to resume a session while
	// Instagram can't be reached.
	retryFirst = 5 * time.Second
	retryMax   = 5 * time.Minute
	// closeWait is how long Close waits for the sockets to shut.
	closeWait = 800 * time.Millisecond
)

// cookieNames are the instagram.com cookies instameow is given: the ones
// the connector's cookie login takes. Anything else pasted is left out.
var cookieNames = slices.Concat(mcookies.IGRequiredCookies, mcookies.IGOptionalCookies)

// savedSession is what's kept in <data-dir>/instagram/ between runs.
type savedSession struct {
	Version int               `json:"version"`
	Cookies map[string]string `json:"cookies"`
	// Browser is the browser the login named, "" for the libraries' own.
	Browser string `json:"browser,omitempty"`
}

// Instagram is the Instagram account.
type Instagram struct {
	d backend.Deps
	// dial makes the library's client (tests put a stand-in here).
	dial func(*mcookies.Cookies, browser.Identity, instameow.EventHandler) api
	// media fetches files from Instagram's CDN (tests swap its transport).
	media *http.Client

	// mu guards everything below, and is held while events are written, so
	// an older state of a chat never follows a newer one.
	mu    sync.Mutex
	base  context.Context
	epoch int         // counts logins, logouts and quitting: a stale attempt stops
	conn  *connection // nil while logged out, broken or reconnecting
	// as is the browser this session says it is, from the login that
	// started it; every connection and download says the same.
	as    browser.Identity
	state proto.AccountState

	selfFBID int64
	selfName string
	chats    map[int64]*chat // by thread key
	byIGID   map[string]*chat
	byLongID map[string]*chat
	people   map[int64]*person // by fbid
	byIGUser map[string]int64  // an Instagram user id's fbid
	told     map[int64]proto.User
	sent     map[int64]bool // chats tuimeta has been sent

	inboxCursor string
	inboxMore   bool

	sending   map[string]*sendState // messages being sent, by offline threading id
	reactLogs map[string]reactionRef
}

// connection is one client and its sockets.
type connection struct {
	cli     api
	cookies *mcookies.Cookies
	as      browser.Identity
	ctx     context.Context
	cancel  context.CancelFunc

	connected chan struct{} // closed at the first Connected
	connOnce  sync.Once
	failed    chan error    // why the first connect can't work
	ready     chan struct{} // closed once established

	// established is set once the session is saved and the account ready
	// (under b.mu); before that, socket news doesn't change the account
	// state.
	established bool
}

var _ backend.Backend = (*Instagram)(nil)

var quietGlobalLog sync.Once

// New is the Instagram account.
func New(deps backend.Deps) backend.Backend {
	// Parts of mautrix-meta log through zerolog's global logger, which
	// writes to stderr; nothing of theirs may reach the terminal or a log.
	quietGlobalLog.Do(func() { zlog.Logger = zerolog.Nop() })
	return newInstagram(deps, dialReal)
}

func newInstagram(deps backend.Deps, dial func(*mcookies.Cookies, browser.Identity, instameow.EventHandler) api) *Instagram {
	b := &Instagram{d: deps, dial: dial, media: newMediaClient(), base: context.Background(), state: proto.LoggedOut, as: browser.Default()}
	b.reset()
	return b
}

// reset forgets the account's chats and people; b.mu is held (or b is new).
func (b *Instagram) reset() {
	b.selfFBID, b.selfName = 0, ""
	b.chats = map[int64]*chat{}
	b.byIGID = map[string]*chat{}
	b.byLongID = map[string]*chat{}
	b.people = map[int64]*person{}
	b.byIGUser = map[string]int64{}
	b.told = map[int64]proto.User{}
	b.sent = map[int64]bool{}
	b.inboxCursor, b.inboxMore = "", false
	b.sending = map[string]*sendState{}
	b.reactLogs = map[string]reactionRef{}
}

func (b *Instagram) Network() proto.Network { return proto.Instagram }

// account reports the account's state; b.mu is held, so states go out in
// the order they happen.
func (b *Instagram) account(state proto.AccountState, msg string) {
	b.state = state
	if state == proto.Ready {
		b.d.Events.Account(proto.Instagram, state, b.userID(b.selfFBID), b.selfName, "")
		return
	}
	b.d.Events.Account(proto.Instagram, state, 0, "", msg)
}

// Start resumes a saved session in the background.
func (b *Instagram) Start(ctx context.Context) {
	c, as, ok := b.loadSession()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.base = ctx
	if !ok {
		b.account(proto.LoggedOut, "")
		return
	}
	b.as = as
	b.account(proto.Connecting, "")
	epoch := b.epoch
	hlog.Go("instagram resume", func() { b.keepTrying(c, epoch) })
}

// loadSession reads the saved cookies and browser; false if there are none
// to use.
func (b *Instagram) loadSession() (*mcookies.Cookies, browser.Identity, bool) {
	var s savedSession
	ok, err := b.d.Session.LoadJSON(sessionFile, &s)
	if err != nil {
		hlog.Warn("instagram: saved session unreadable", hlog.Kind(err))
		return nil, browser.Identity{}, false
	}
	if !ok || s.Version != 1 {
		return nil, browser.Identity{}, false
	}
	// A browser that doesn't parse wasn't written by this helper, and
	// resuming as some other browser is what a session must never do.
	as, err := browser.Parse(s.Browser)
	if err != nil {
		hlog.Warn("instagram: saved session names an unknown browser")
		return nil, browser.Identity{}, false
	}
	c := libCookies(s.Cookies)
	if len(c.GetMissingCookieNames()) > 0 {
		return nil, browser.Identity{}, false
	}
	return c, as, true
}

// libCookies is the cookies instameow is given.
func libCookies(values map[string]string) *mcookies.Cookies {
	c := &mcookies.Cookies{Platform: types.Instagram}
	m := map[mcookies.MetaCookieName]string{}
	for _, name := range cookieNames {
		if v := values[string(name)]; v != "" {
			m[name] = v
		}
	}
	c.UpdateValues(m)
	return c
}

// saveSession keeps the connection's cookies, which Instagram rotates, and
// the browser it says it is.
func (b *Instagram) saveSession(conn *connection) error {
	values := map[string]string{}
	for k, v := range conn.cookies.GetAll() {
		if v != "" && slices.Contains(cookieNames, k) {
			values[string(k)] = v
		}
	}
	return b.d.Session.SaveJSON(sessionFile, savedSession{Version: 1, Cookies: values, Browser: conn.as.Name()})
}

// loadError is the inbox failing to load, with whether Instagram knew the
// cookies at all (with stale ones it answers with its login page).
type loadError struct {
	err           error
	authenticated bool
}

func (e *loadError) Error() string { return "instagram: loading the inbox failed" }
func (e *loadError) Unwrap() error { return e.err }

// describe is what the user is told about a connection that didn't work.
func describe(err error) *proto.Error {
	if le := (*loadError)(nil); errors.As(err, &le) {
		return loginError(le.err, le.authenticated)
	}
	return loginError(err, true)
}

// keepTrying resumes a session, and keeps trying while Instagram can't be
// reached; it gives up on anything else (the session's over, a checkpoint).
func (b *Instagram) keepTrying(c *mcookies.Cookies, epoch int) {
	delay := retryFirst
	for {
		b.mu.Lock()
		base := b.base
		b.mu.Unlock()
		conn, err := b.open(base, c, epoch)
		if err == nil {
			b.establish(conn, epoch)
			return
		}
		if base.Err() != nil || errors.Is(err, context.Canceled) {
			return
		}
		hlog.Warn("instagram: connecting failed", hlog.Kind(err))
		pe := describe(err)
		b.mu.Lock()
		if b.epoch != epoch {
			b.mu.Unlock()
			return
		}
		if pe.Code != proto.NetworkError || pe == errKeepsClosing {
			if errors.Is(err, httpclient.ErrTokenInvalidated) {
				// Instagram has ended the session: the cookies are no use.
				if rerr := b.d.Session.Remove(sessionFile); rerr != nil {
					hlog.Warn("instagram: can't delete the session", hlog.Kind(rerr))
				}
			}
			if pe.Code == proto.BadCookies {
				pe = errSessionEnded
			}
			b.account(proto.Errored, pe.Message)
			b.mu.Unlock()
			return
		}
		b.account(proto.Errored, "Can't reach Instagram right now; tuimeta-helper keeps trying.")
		b.mu.Unlock()
		select {
		case <-base.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, retryMax)
	}
}

func (b *Instagram) stale(epoch int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.epoch != epoch
}

// open makes a client, loads the inbox and connects the socket, returning
// once Instagram has subscribed it. The connection becomes the current one
// as soon as the inbox is in, so the socket's first updates find it.
func (b *Instagram) open(ctx context.Context, c *mcookies.Cookies, epoch int) (*connection, error) {
	b.mu.Lock()
	base, as := b.base, b.as
	b.mu.Unlock()
	conn := &connection{cookies: c, as: as, connected: make(chan struct{}), failed: make(chan error, 1), ready: make(chan struct{})}
	conn.ctx, conn.cancel = context.WithCancel(withQuietLog(base))
	conn.cli = b.dial(c, as, func(_ context.Context, evt slidetypes.ClientEvent) error {
		return b.handle(conn, evt)
	})
	viewer, mailbox, err := conn.cli.LoadIndex(withQuietLog(ctx))
	if err != nil {
		conn.cancel()
		return nil, &loadError{err: err, authenticated: conn.cli.IsAuthenticated()}
	}
	fbid := conn.cli.GetOwnFBID()
	if fbid == 0 && mailbox != nil {
		for _, edge := range mailbox.ThreadsByFolder.Edges {
			if t := edge.Node.AsIGDirectThread; t != nil && t.Viewer != nil && t.Viewer.InteropMessagingUserFBID != 0 {
				fbid = t.Viewer.InteropMessagingUserFBID
				break
			}
		}
	}
	if fbid == 0 || viewer == nil {
		conn.cancel()
		return nil, errNoAccount
	}

	b.mu.Lock()
	if b.epoch != epoch {
		b.mu.Unlock()
		conn.cancel()
		return nil, context.Canceled
	}
	if b.selfFBID != 0 && b.selfFBID != fbid {
		b.reset() // another account: nothing of the old one carries over
	}
	b.selfFBID = fbid
	b.selfName = viewer.GetName()
	if b.selfName == "" {
		b.selfName = viewer.GetUsername()
	}
	b.person(&slidetypes.User{InteropMessagingUserFBID: fbid, ID: viewer.ID, Username: viewer.GetUsername(),
		FullName: viewer.GetName(), ProfilePicURL: viewer.GetAvatarURL()})
	if mailbox != nil {
		b.addInbox(mailbox.ThreadsByFolder)
		for i := range mailbox.PinnedThreadsV2 {
			b.upsertThread(mailbox.PinnedThreadsV2[i].AsIGDirectThread, false)
		}
	}
	b.conn = conn
	b.mu.Unlock()

	hlog.Go("instagram socket", func() { conn.cli.Connect(conn.ctx) })
	timer := time.NewTimer(connectTimeout)
	defer timer.Stop()
	select {
	case <-conn.connected:
		return conn, nil
	case err = <-conn.failed:
	case <-timer.C:
		err = errConnectTimeout
	case <-ctx.Done():
		err = ctx.Err()
	}
	b.drop(conn)
	return nil, err
}

// establish saves the session of a connection that works and says the
// account is ready.
func (b *Instagram) establish(conn *connection, epoch int) {
	if err := b.saveSession(conn); err != nil {
		hlog.Error("instagram: can't save the session", hlog.Kind(err))
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != conn || b.epoch != epoch {
		return
	}
	conn.established = true
	close(conn.ready)
	b.account(proto.Ready, "")
	b.tellUser(b.people[b.selfFBID])
	// After a reconnect, chats tuimeta has are sent again as they are now.
	for _, c := range b.chats {
		if b.sent[c.id] {
			b.tellChat(c)
		}
	}
	hlog.Info("instagram: ready")
	hlog.Go("instagram refresh", func() { b.refreshLater(conn) })
}

// refreshLater makes the connection again after refreshEvery, as the
// connector does, so a long run doesn't hold a stale one.
func (b *Instagram) refreshLater(conn *connection) {
	select {
	case <-conn.ctx.Done():
	case <-time.After(refreshEvery):
		b.reconnect(conn)
	}
}

// reconnect replaces a connection with a new one: Instagram asked for a
// fresh start, or it's time to refresh. Meanwhile requests wait or fail.
func (b *Instagram) reconnect(conn *connection) {
	b.mu.Lock()
	if b.conn != conn {
		b.mu.Unlock()
		return
	}
	epoch := b.epoch
	b.conn = nil
	b.forgetHistory()
	b.account(proto.Connecting, "")
	b.mu.Unlock()
	b.disconnect(conn, 0)
	hlog.Info("instagram: reconnecting")
	b.keepTrying(conn.cookies, epoch)
}

// forgetHistory drops what was loaded of each chat's history, which may
// have a hole now (it's fetched again when asked for); b.mu is held. Message
// ids stay the same.
func (b *Instagram) forgetHistory() {
	for _, c := range b.chats {
		c.log = history.New()
		c.histGen++
		c.fetched, c.cursor = false, ""
		for _, n := range c.msgs {
			n.inLog = false
		}
	}
}

// drop ends a connection that didn't work out.
func (b *Instagram) drop(conn *connection) {
	b.mu.Lock()
	if b.conn == conn {
		b.conn = nil
	}
	b.mu.Unlock()
	b.disconnect(conn, 0)
}

// disconnect closes a connection's sockets without telling Instagram
// anything, waiting at most wait (0: not at all).
func (b *Instagram) disconnect(conn *connection, wait time.Duration) {
	if conn == nil {
		return
	}
	conn.cancel()
	done := make(chan struct{})
	hlog.Go("instagram disconnect", func() {
		defer close(done)
		conn.cli.Disconnect()
	})
	if wait > 0 {
		select {
		case <-done:
		case <-time.After(wait):
		}
	}
}

// LoginCookies logs in with pasted cookies as the browser named, and saves
// them (and it) once Instagram's socket has connected with them.
func (b *Instagram) LoginCookies(ctx context.Context, set cookies.Set, as browser.Identity) error {
	c := libCookies(set.Values())
	if len(c.GetMissingCookieNames()) > 0 {
		return errBadCookies
	}
	b.mu.Lock()
	b.epoch++
	epoch := b.epoch
	old := b.conn
	b.conn = nil
	b.reset()
	b.as = as
	b.account(proto.Connecting, "")
	b.mu.Unlock()
	b.disconnect(old, 0)

	conn, err := b.open(ctx, c, epoch)
	if err != nil {
		hlog.Info("instagram: login failed", hlog.Kind(err))
		b.mu.Lock()
		if b.epoch == epoch {
			b.account(proto.LoggedOut, "")
		}
		b.mu.Unlock()
		if errors.Is(err, context.Canceled) {
			return err
		}
		return describe(err)
	}
	b.establish(conn, epoch)
	if b.stale(epoch) {
		return errNotConnected
	}
	return nil
}

// Logout disconnects and deletes the session. The web client's logout isn't
// something the connector does either, so the session stays valid on
// Instagram's side until it expires or is ended there (Settings → Login
// activity); README says so.
func (b *Instagram) Logout(ctx context.Context) error {
	b.mu.Lock()
	b.epoch++
	old := b.conn
	b.conn = nil
	b.reset()
	b.as = browser.Default()
	b.mu.Unlock()
	b.disconnect(old, closeWait)
	if err := b.d.Session.Wipe(); err != nil {
		hlog.Warn("instagram: can't delete the session", hlog.Kind(err))
	}
	b.mu.Lock()
	b.account(proto.LoggedOut, "")
	b.mu.Unlock()
	return nil
}

// Close disconnects quietly: no receipts, no presence, nothing said to
// Instagram but closing the sockets. The cookies are kept as Instagram last
// rotated them.
func (b *Instagram) Close() {
	b.mu.Lock()
	b.epoch++
	conn := b.conn
	b.conn = nil
	established := conn != nil && conn.established
	b.mu.Unlock()
	if conn == nil {
		return
	}
	if established {
		if err := b.saveSession(conn); err != nil {
			hlog.Warn("instagram: can't save the session", hlog.Kind(err))
		}
	}
	b.disconnect(conn, closeWait)
}

// current is the connection requests go through.
func (b *Instagram) current() (*connection, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn == nil || !b.conn.established {
		return nil, errNotConnected
	}
	return b.conn, nil
}

// waitReady waits for the connection to be established: a request can come
// while a saved session is still resuming.
func (b *Instagram) waitReady(ctx context.Context) (*connection, error) {
	deadline := time.Now().Add(connectTimeout)
	for {
		if conn, err := b.current(); err == nil {
			return conn, nil
		}
		b.mu.Lock()
		state := b.state
		b.mu.Unlock()
		if state != proto.Connecting || time.Now().After(deadline) {
			return nil, errNotConnected
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// errStop tells instameow to stop reconnecting a socket.
var errStop = errors.New("instagram: stop reconnecting")

// handle is instameow's event handler. It runs on the socket's goroutine;
// a panic in it is logged as its kind only and goes no further.
func (b *Instagram) handle(conn *connection, evt slidetypes.ClientEvent) (err error) {
	defer func() {
		if v := recover(); v != nil {
			hlog.Recovered("instagram event", v)
			err = nil
		}
	}()
	b.mu.Lock()
	current := b.conn == conn
	b.mu.Unlock()
	if !current {
		if _, ok := evt.(*slidetypes.Disconnected); ok {
			return errStop
		}
		return nil
	}
	switch e := evt.(type) {
	case *slidetypes.Connected:
		b.onConnected(conn)
	case *slidetypes.Disconnected:
		return b.onDisconnected(conn, e)
	case *slidetypes.AuthError:
		b.onAuthError(conn, e.Error)
	case *slidetypes.ResnapshotRequired:
		hlog.Info("instagram: resnapshot required")
		hlog.Go("instagram reconnect", func() { b.reconnect(conn) })
	case *slidetypes.Delta:
		if b.waitEstablished(conn) {
			b.onDelta(conn, e)
		}
	case *slidetypes.TypingNotification:
		if b.waitEstablished(conn) {
			b.onTyping(e)
		}
	case *slidetypes.SeqIDUpdate, *slidetypes.ReconnectionStateUpdate:
		// The connector keeps these to resume faster; every run here loads
		// the inbox afresh instead.
	}
	return nil
}

// waitEstablished holds the socket's updates until the account is ready, as
// the connector holds them until the inbox is processed, so tuimeta hears of
// chats only once it knows the account. False if that won't happen.
func (b *Instagram) waitEstablished(conn *connection) bool {
	select {
	case <-conn.ready:
		return true
	case <-conn.ctx.Done():
		return false
	case <-time.After(connectTimeout):
		return false
	}
}

func (b *Instagram) onConnected(conn *connection) {
	conn.connOnce.Do(func() { close(conn.connected) })
	b.mu.Lock()
	established := conn.established
	if established && b.state != proto.Ready {
		b.account(proto.Ready, "")
	}
	b.mu.Unlock()
	hlog.Info("instagram: connected")
	if established {
		if err := b.saveSession(conn); err != nil {
			hlog.Warn("instagram: can't save the session", hlog.Kind(err))
		}
	}
}

func (b *Instagram) onDisconnected(conn *connection, e *slidetypes.Disconnected) error {
	hlog.Info("instagram: disconnected", hlog.Kind(e.Error), hlog.Int("failures", int64(e.FailureCount)))
	switch {
	case websocket.CloseStatus(e.Error) == dgw.CloseStatusUnauthorized:
		b.fail(conn, errSessionEnded)
		return errStop
	case e.FailureCount > 5 && errors.Is(e.Error, instameow.ErrMainStreamClosed):
		b.fail(conn, errKeepsClosing)
		return errStop
	}
	b.mu.Lock()
	if conn.established && e.FailureCount > 0 && b.state == proto.Ready {
		// A quick drop is reconnected at once; only repeated failures are
		// worth showing.
		b.account(proto.Connecting, "")
	}
	b.mu.Unlock()
	return nil
}

func (b *Instagram) onAuthError(conn *connection, err error) {
	pe := authError(err)
	if pe == nil {
		hlog.Warn("instagram: unrecognized auth error", hlog.Kind(err))
		return
	}
	hlog.Warn("instagram: session error", hlog.Kind(err))
	if errors.Is(err, httpclient.ErrTokenInvalidated) {
		if rerr := b.d.Session.Remove(sessionFile); rerr != nil {
			hlog.Warn("instagram: can't delete the session", hlog.Kind(rerr))
		}
	}
	b.fail(conn, pe)
}

// fail ends a connection Instagram won't have any more, and says what to do.
func (b *Instagram) fail(conn *connection, pe *proto.Error) {
	select {
	case conn.failed <- pe:
	default:
	}
	b.mu.Lock()
	if b.conn != conn || !conn.established {
		b.mu.Unlock()
		return
	}
	b.conn = nil
	b.account(proto.Errored, pe.Message)
	b.mu.Unlock()
	b.disconnect(conn, 0)
}
