// SPDX-License-Identifier: AGPL-3.0-or-later

// Package messenger is the Messenger backend: mautrix-meta's messagix for
// the account and its chats, and whatsmeow for the end-to-end encrypted
// ones. It does what mautrix-meta's bridge connector does with the two
// libraries, without any of the bridge: tables from Messenger's socket and
// messages from the encrypted chats' socket become the protocol's chats,
// people and messages.
package messenger

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"go.mau.fi/whatsmeow"

	"go.mau.fi/mautrix-meta/pkg/messagix"
	"go.mau.fi/mautrix-meta/pkg/messagix/dgw"
	"go.mau.fi/mautrix-meta/pkg/messagix/table"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/browser"
	"github.com/erictran308/tuimeta/helper/internal/cookies"
	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

const net = proto.Messenger

// Timings.
const (
	// ConnectTimeout bounds how long a login waits for the socket to sync.
	ConnectTimeout = 90 * time.Second
	// MinFullReconnect spaces out reloading the whole session after the
	// encrypted chats' server asks for it.
	MinFullReconnect = 10 * time.Minute
	// closeWait is how long Close waits for the libraries to let go.
	closeWait = 900 * time.Millisecond
)

// retryWait is the first pause before a resumed session tries again
// (tests shorten it); it doubles each time, up to five minutes.
var retryWait = 2 * time.Second

// sessionFile keeps the login between runs.
const sessionFile = "session.json"

// savedSession is what's kept: the cookies (as messagix last updated them),
// the browser the login named, and which encrypted-chat device this login
// registered.
type savedSession struct {
	Version  int               `json:"version"`
	UserID   int64             `json:"user_id"`
	Cookies  map[string]string `json:"cookies"`
	Browser  string            `json:"browser,omitempty"`
	WADevice string            `json:"wa_device,omitempty"`
}

// login is what a connection is made with: the cookies, and the browser to
// say it is.
type login struct {
	cookies map[string]string
	as      browser.Identity
}

// Messenger is the Messenger account.
type Messenger struct {
	d   backend.Deps
	now func() time.Time
	// dial connects with a login (connectWith); tests replace it, so they
	// never reach Facebook.
	dial func(ctx, life context.Context, l login) error

	mu      sync.Mutex
	base    context.Context
	life    context.Context // ends at logout or quit
	stop    context.CancelFunc
	gen     int // the current connection; events from older ones are dropped
	upGen   int // the connection that finished connecting
	meta    metaAPI
	msgx    *messagix.Client
	as      browser.Identity // the browser msgx says it is
	e2ee    e2eeAPI
	wa      *whatsmeow.Client
	store   *e2eeStore
	sess    *savedSession
	closed  bool
	ready   bool // the Messenger socket is synced
	e2eeOK  bool // the encrypted chats' socket is connected
	lastFul time.Time

	self       int64
	selfName   string
	selfAvatar string

	chats   map[int64]*chat // by key
	byID    map[int64]*chat // by helper id
	people  map[int64]*person
	msgChat map[string]int64 // Facebook message id → chat key (edits carry no chat)
	hybrid  map[int64]int64  // Facebook thread key → encrypted chat key
	sent    map[int64]bool   // chats load_chats has sent since login
	asked   map[int64]bool   // threads whose details were asked for
	more    bool             // Messenger may have older threads
	minKey  int64            // the oldest thread key paging reached
	listed  bool             // load_chats said there are no more
	dirty   map[int64]bool   // chats changed while applying a table
	// replaying is set while kept encrypted messages are read back: they
	// rebuild the chats without being reported as new.
	replaying bool
	waiters   map[int64][]chan struct{}
	edits     map[string]chan string

	// Asking Messenger who people are (askContact): those waiting their
	// turn, when the latest lookups went out (kept across logins: the
	// budget is the helper's), and the life the worker asking serves.
	contactQueue   []int64
	contactTimes   []time.Time
	contactLife    context.Context
	contactRecheck time.Duration // ContactRecheck
}

var _ backend.Backend = (*Messenger)(nil)

// New is the Messenger account.
func New(deps backend.Deps) backend.Backend { return newMessenger(deps) }

func newMessenger(deps backend.Deps) *Messenger {
	silenceLibraries()
	m := &Messenger{d: deps, now: time.Now, base: context.Background(), contactRecheck: ContactRecheck}
	m.dial = m.connectWith
	m.life, m.stop = context.WithCancel(m.base)
	m.reset()
	return m
}

// reset forgets the account's state (login, logout); m.mu is held or m is new.
func (m *Messenger) reset() {
	m.chats = map[int64]*chat{}
	m.byID = map[int64]*chat{}
	m.people = map[int64]*person{}
	m.msgChat = map[string]int64{}
	m.hybrid = map[int64]int64{}
	m.sent = map[int64]bool{}
	m.asked = map[int64]bool{}
	m.waiters = map[int64][]chan struct{}{}
	m.edits = map[string]chan string{}
	m.contactQueue = nil
	m.dirty = nil
	m.more = true
	m.minKey = 0
	m.listed = false
	m.ready = false
	m.e2eeOK = false
	m.self, m.selfName, m.selfAvatar = 0, "", ""
	m.as = browser.Default()
}

func (m *Messenger) Network() proto.Network { return net }

func (m *Messenger) Start(ctx context.Context) {
	m.mu.Lock()
	m.base = ctx
	m.stop()
	m.life, m.stop = context.WithCancel(ctx)
	var s savedSession
	ok, err := m.d.Session.LoadJSON(sessionFile, &s)
	if err != nil {
		hlog.Warn("messenger: session unreadable", hlog.Kind(err))
	}
	// The session says the browser it was logged in as. One that doesn't
	// parse wasn't written by this helper, and resuming as some other
	// browser is what a session must never do.
	as, berr := browser.Parse(s.Browser)
	if berr != nil {
		hlog.Warn("messenger: saved session names an unknown browser")
	}
	if !ok || err != nil || berr != nil || len(s.Cookies) == 0 {
		m.mu.Unlock()
		m.d.Events.Account(net, proto.LoggedOut, 0, "", "")
		return
	}
	m.sess = &s
	life := m.life
	m.mu.Unlock()
	m.d.Events.Account(net, proto.Connecting, 0, "", "")
	hlog.Go("messenger resume", func() { m.resume(life, login{s.Cookies, as}) })
}

// resume connects with the saved session, trying again with growing pauses
// while the network is the problem.
func (m *Messenger) resume(life context.Context, l login) {
	wait := retryWait
	for {
		err := m.connect(life, l)
		if err == nil || life.Err() != nil || errors.Is(err, errReplaced) {
			return
		}
		var pe *proto.Error
		if errors.As(err, &pe) && pe.Code != proto.NetworkError {
			hlog.Warn("messenger: session refused", hlog.Kind(err))
			m.d.Events.Account(net, proto.Errored, 0, "", sessionDead(pe))
			return
		}
		hlog.Info("messenger: can't connect yet", hlog.Kind(err))
		select {
		case <-time.After(wait):
		case <-life.Done():
			return
		}
		wait = min(wait*2, 5*time.Minute)
	}
}

// sessionDead is what to do when a saved session stops working.
func sessionDead(pe *proto.Error) string {
	if pe.Code == proto.BadCookies {
		return "Facebook logged this session out: log in to facebook.com in a browser, then log in here again with fresh cookies."
	}
	return pe.Message
}

func (m *Messenger) LoginCookies(ctx context.Context, c cookies.Set, as browser.Identity) error {
	m.mu.Lock()
	// A session being resumed (or retried) gives way to this login, and the
	// encrypted chats' store is opened afresh for it.
	m.stop()
	m.life, m.stop = context.WithCancel(m.base)
	st := m.store
	m.store = nil
	m.teardownLocked()
	m.reset()
	if st != nil {
		hlog.Go("messenger store close", func() { _ = st.Close() })
	}
	life := m.life
	m.mu.Unlock()
	m.d.Events.Account(net, proto.Connecting, 0, "", "")
	ctx, cancel := context.WithTimeout(ctx, ConnectTimeout)
	defer cancel()
	stopLife := context.AfterFunc(life, cancel)
	defer stopLife()
	err := m.dial(ctx, life, login{c.Values(), as})
	if err != nil {
		// Only while this is still the login: one given up for a newer
		// login, or a logout, mustn't disconnect or log out what replaced it.
		m.mu.Lock()
		current := m.life == life
		if current {
			m.teardownLocked()
			m.reset()
		}
		m.mu.Unlock()
		if current {
			m.d.Events.Account(net, proto.LoggedOut, 0, "", "")
		}
		return err
	}
	return nil
}

// errReplaced is a login that gave way before it was done.
var errReplaced = proto.Err(proto.Cancelled, "This login gave way to a newer one, or to logging out.")

// connect is a resumed session's connection.
func (m *Messenger) connect(life context.Context, l login) error {
	ctx, cancel := context.WithTimeout(life, ConnectTimeout)
	defer cancel()
	return m.dial(ctx, life, l)
}

// connectWith loads the Messenger page with the cookies, connects its socket
// and waits for the first sync. The session is saved only then. ctx bounds
// the wait; life is the connection's own lifetime.
func (m *Messenger) connectWith(ctx, life context.Context, l login) error {
	values := l.cookies
	cli := newMessagix(values, l.as)
	user, initial, err := cli.LoadMessagesPage(ctx)
	if err != nil {
		hlog.Info("messenger: page load failed", hlog.Kind(err))
		return loginError(err)
	}
	fbid := user.GetFBID()
	cuser, _ := strconv.ParseInt(values["c_user"], 10, 64)
	if fbid == 0 {
		return errLoggedOutCookies
	}
	if fbid != cuser {
		return errMixedCookies
	}
	ready := make(chan error, 1)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return proto.Err(proto.Internal, "The helper is quitting.")
	}
	m.teardownLocked()
	gen := m.gen
	m.msgx = cli
	m.as = l.as
	m.meta = &metaConn{cli: cli}
	m.self = fbid
	m.selfName = user.GetName()
	m.selfAvatar = user.GetAvatarURL()
	p := m.person(fbid)
	p.name, p.username, p.avatar, p.known = m.selfName, user.GetUsername(), m.selfAvatar, true
	m.mu.Unlock()

	var once sync.Once
	signal := func(err error) { once.Do(func() { ready <- err }) }
	cli.SetEventHandler(func(_ context.Context, evt any) {
		defer func() {
			if v := recover(); v != nil {
				hlog.Recovered("messenger event", v)
			}
		}()
		m.onMetaEvent(gen, evt, initial, signal)
	})
	if err := cli.Connect(life); err != nil {
		return proto.Err(proto.NetworkError, "Can't connect to Messenger; check the connection and try again.")
	}
	select {
	case err = <-ready:
	case <-ctx.Done():
		err = proto.Err(proto.NetworkError, "Messenger didn't answer in time; check the connection and try again.")
	}
	if err != nil {
		m.mu.Lock()
		if m.gen == gen {
			m.teardownLocked()
		}
		m.mu.Unlock()
		return err
	}
	if !m.keepSession(gen, fbid, cookieValues(cli), l.as) {
		return errReplaced
	}
	m.becameReady(gen)
	hlog.Go("messenger e2ee", func() { m.connectE2EE(gen) })
	return nil
}

// keepSession records and saves the login of the connection gen, which has
// synced, and reports whether it's still the connection. It's saved under
// the lock, and only then, so a logout can't wipe the folder in between and
// find the session back at the next start.
func (m *Messenger) keepSession(gen int, fbid int64, values map[string]string, as browser.Identity) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gen != gen || m.closed {
		return false
	}
	if m.sess == nil || m.sess.UserID != fbid {
		// A new account: the old one's encrypted-chat device isn't reused.
		m.sess = &savedSession{Version: 1}
	}
	m.sess.UserID = fbid
	m.sess.Cookies = values
	m.sess.Browser = as.Name()
	m.upGen = gen
	if err := m.d.Session.SaveJSON(sessionFile, *m.sess); err != nil {
		hlog.Error("messenger: can't save session", hlog.Kind(err))
	}
	return true
}

func cookieValues(cli *messagix.Client) map[string]string {
	return (&metaConn{cli: cli}).Cookies()
}

// onMetaEvent handles what messagix reports about its socket.
func (m *Messenger) onMetaEvent(gen int, evt any, initial *table.LSTable, signal func(error)) {
	switch e := evt.(type) {
	case *table.LSTable:
		m.applySocketTable(gen, e)
	case *messagix.ConnectedEvent:
		hlog.Info("messenger: connected")
		m.applyInitialPage(gen, initial)
		m.openStore(gen)
		signal(nil)
	case *messagix.ReconnectedEvent:
		hlog.Info("messenger: reconnected")
		m.becameReady(gen)
		signal(nil)
	case *messagix.TransientDisconnectEvent:
		hlog.Info("messenger: disconnected; reconnecting")
		m.mu.Lock()
		current := m.gen == gen && m.ready && !m.closed
		if current {
			m.ready = false
		}
		m.mu.Unlock()
		if current {
			m.d.Events.Account(net, proto.Connecting, 0, "", "")
		}
	case *messagix.PermanentErrorEvent:
		msg := "Messenger stopped answering this session; log in again."
		if websocket.CloseStatus(e.Err) == dgw.CloseStatusUnauthorized {
			msg = "Facebook logged this session out: log in to facebook.com in a browser, then log in here again with fresh cookies."
		}
		hlog.Warn("messenger: socket gave up", hlog.Kind(e.Err))
		signal(proto.Err(proto.BadCookies, msg))
		m.mu.Lock()
		// A connection still logging in reports through its login instead.
		current := m.gen == gen && m.upGen == gen && !m.closed
		if current {
			m.ready = false
		}
		m.mu.Unlock()
		if current {
			m.d.Events.Account(net, proto.Errored, 0, "", msg)
		}
	}
}

// applySocketTable applies a table pushed on the socket of connection gen.
// A panic while applying is recovered by the socket's event handler, so
// m.mu is let go of by defer, and messagix is told the table was handled
// even then: its sync cursors move past a table that can't be applied,
// rather than have it sent again at every reconnect.
func (m *Messenger) applySocketTable(gen int, tbl *table.LSTable) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gen != gen || m.closed {
		return
	}
	if meta := m.meta; meta != nil {
		defer meta.PostHandle(tbl)
	}
	m.applyTable(tbl, fromSocket)
}

// applyInitialPage applies the data the Messenger page came with. A row in
// it that can't be applied gives up the rest of the page, but the account
// still connects: the same page would fail again at every try.
func (m *Messenger) applyInitialPage(gen int, initial *table.LSTable) {
	defer func() {
		if v := recover(); v != nil {
			hlog.Recovered("messenger initial page", v)
		}
	}()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gen == gen && initial != nil {
		m.applyTable(initial, fromInitial)
	}
}

// becameReady reports the account ready.
func (m *Messenger) becameReady(gen int) {
	m.mu.Lock()
	if m.gen != gen || m.closed || m.ready {
		m.mu.Unlock()
		return
	}
	m.ready = true
	self := m.person(m.self)
	name := m.selfName
	m.mu.Unlock()
	m.d.Events.Account(net, proto.Ready, self.id, name, "")
	m.mu.Lock()
	m.tellUser(self, true)
	m.mu.Unlock()
}

// fullReconnect loads the page again and reconnects everything, as the
// connector does when the encrypted chats' server wants a fresh token.
func (m *Messenger) fullReconnect(gen int) {
	m.mu.Lock()
	if m.gen != gen || m.closed || m.sess == nil || m.now().Sub(m.lastFul) < MinFullReconnect {
		m.mu.Unlock()
		return
	}
	m.lastFul = m.now()
	l := login{m.msgxCookies(), m.as}
	life := m.life
	m.mu.Unlock()
	hlog.Info("messenger: reconnecting from scratch")
	m.d.Events.Account(net, proto.Connecting, 0, "", "")
	hlog.Go("messenger reconnect", func() { m.resume(life, l) })
}

// msgxCookies is the current cookies; m.mu is held.
func (m *Messenger) msgxCookies() map[string]string {
	if m.meta != nil {
		return m.meta.Cookies()
	}
	if m.sess != nil {
		return m.sess.Cookies
	}
	return nil
}

// teardownLocked drops the current connection without telling anyone
// anything; m.mu is held. The libraries are let go of in the background.
func (m *Messenger) teardownLocked() {
	meta, wa := m.meta, m.wa
	m.meta, m.msgx, m.e2ee, m.wa = nil, nil, nil, nil
	m.ready, m.e2eeOK = false, false
	m.gen++
	if meta != nil || wa != nil {
		hlog.Go("messenger disconnect", func() {
			if wa != nil {
				wa.Disconnect()
			}
			if meta != nil {
				meta.Disconnect()
			}
		})
	}
}

func (m *Messenger) Close() {
	m.mu.Lock()
	m.closed = true
	m.stop()
	meta, wa, store := m.meta, m.wa, m.store
	m.meta, m.msgx, m.e2ee, m.wa, m.store = nil, nil, nil, nil, nil
	if meta != nil && m.sess != nil {
		// Keep the cookies as messagix last updated them, so the next run
		// starts with what Facebook expects. Under the lock, so a logout
		// can't wipe the folder in between and find them back.
		m.sess.Cookies = meta.Cookies()
		if err := m.d.Session.SaveJSON(sessionFile, *m.sess); err != nil {
			hlog.Warn("messenger: can't save session", hlog.Kind(err))
		}
	}
	m.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		if wa != nil {
			wg.Add(1)
			go func() { defer wg.Done(); wa.Disconnect() }()
		}
		if meta != nil {
			wg.Add(1)
			go func() { defer wg.Done(); meta.Disconnect() }()
		}
		wg.Wait()
		if store != nil {
			_ = store.Close()
		}
	}()
	select {
	case <-done:
	case <-time.After(closeWait):
		hlog.Warn("messenger: libraries slow to close")
	}
}

func (m *Messenger) Logout(ctx context.Context) error {
	m.mu.Lock()
	meta, wa, store := m.meta, m.wa, m.store
	m.stop()
	m.life, m.stop = context.WithCancel(m.base)
	m.meta, m.msgx, m.e2ee, m.wa, m.store = nil, nil, nil, nil, nil
	m.gen++
	m.sess = nil
	m.reset()
	m.mu.Unlock()

	if meta != nil {
		// A local logout only: disconnect, but never call facebook.com's own
		// "Log out", so the browser the cookies came from stays logged in.
		// The web session lives on until the user ends it themselves, in the
		// browser or in Facebook's list of logged-in devices.
		meta.Disconnect()
	}
	if wa != nil {
		wa.Disconnect()
		dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := wa.Store.Delete(dctx); err != nil {
			hlog.Warn("messenger: can't delete the encrypted-chat device", hlog.Kind(err))
		}
		cancel()
	}
	if store != nil {
		_ = store.Close()
	}
	if err := m.d.Session.Wipe(); err != nil {
		hlog.Error("messenger: can't wipe session", hlog.Kind(err))
	}
	m.d.Events.Account(net, proto.LoggedOut, 0, "", "")
	return nil
}

// connected is the Messenger connection for a request, or an error saying
// why there's none.
func (m *Messenger) connected() (metaAPI, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.meta == nil {
		return nil, errNotConnected
	}
	return m.meta, nil
}

// encrypted is the encrypted chats' connection for a request.
func (m *Messenger) encrypted() (e2eeAPI, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.e2ee == nil {
		return nil, errNoE2EE
	}
	return m.e2ee, nil
}
