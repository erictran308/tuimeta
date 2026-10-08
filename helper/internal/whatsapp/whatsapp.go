// SPDX-License-Identifier: AGPL-3.0-or-later

// Package whatsapp is the WhatsApp backend: whatsmeow, linked to the account
// as a new device the way WhatsApp Web is, driven as mautrix-whatsapp's
// connector drives it, with no bridge running. WhatsApp keeps no history on
// its servers, so the chats and messages this device has are kept in its
// database, and older ones are asked of the phone.
package whatsapp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	waTypes "go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/browser"
	"github.com/erictran308/tuimeta/helper/internal/cookies"
	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

const net = proto.WhatsApp

// Timings.
const (
	// ConnectWait bounds how long a link waits for the connection after the
	// phone confirmed it.
	ConnectWait = 45 * time.Second
	// SweepEvery is how often disappearing messages are looked for.
	SweepEvery = time.Minute
	// closeWait is how long Close waits for whatsmeow to let go.
	closeWait = 900 * time.Millisecond
)

// sessionFile says which device in the store is this login's.
const sessionFile = "session.json"

type savedSession struct {
	Version int    `json:"version"`
	Device  string `json:"device"`
}

// WhatsApp is the WhatsApp account.
type WhatsApp struct {
	d   backend.Deps
	now func() time.Time
	// dial connects a device (connectDevice); tests replace it, so they
	// never reach WhatsApp.
	dial func(gen int, dev *store.Device) error

	mu     sync.Mutex
	base   context.Context
	life   context.Context // ends at logout or quit
	stop   context.CancelFunc
	gen    int // the current connection; events from older ones are dropped
	st     *waStore
	cli    waAPI
	link   *linkTry
	ready  bool
	closed bool
	// storeFailed is set when keeping something failed while an event was
	// handled, so the message isn't acknowledged and comes again.
	storeFailed bool
	// scrub is set when something was deleted from the store, so its
	// write-ahead log is emptied (deleted text lingers there otherwise).
	scrub    bool
	self     string          // your key (your WhatsApp id once known)
	selfName string          // your name as WhatsApp has it
	selves   map[string]bool // every key that is you: your number's and your WhatsApp id's

	chats    map[string]*chat // by key
	byID     map[int64]*chat
	people   map[string]*person
	alias    map[string]string      // a number's JID → that person's WhatsApp id
	numbers  map[string]string      // a WhatsApp id → that person's number's JID
	sent     map[int64]bool         // chats load_chats has sent since login
	listed   bool                   // load_chats said there are no more
	asked    map[string]bool        // groups whose details were asked for
	quiet    map[string]time.Time   // chats whose older messages the phone didn't send, and when
	looked   map[string]waTypes.JID // numbers asked about this run, and who they are (empty: nobody)
	lookups  []time.Time            // when numbers were asked about lately
	searches int                    // searches so far: a newer one stops an older one's lookup
	waiters  map[string][]chan struct{}
}

var (
	_ backend.Backend = (*WhatsApp)(nil)
	_ backend.Linker  = (*WhatsApp)(nil)
)

// New is the WhatsApp account.
func New(deps backend.Deps) backend.Backend { return newWhatsApp(deps) }

func newWhatsApp(deps backend.Deps) *WhatsApp {
	describeDevice()
	w := &WhatsApp{d: deps, now: time.Now, base: context.Background()}
	w.dial = w.connectDevice
	w.life, w.stop = context.WithCancel(w.base)
	w.reset()
	return w
}

// reset forgets the account's state; w.mu is held or w is new.
func (w *WhatsApp) reset() {
	w.chats = map[string]*chat{}
	w.byID = map[int64]*chat{}
	w.people = map[string]*person{}
	w.alias = map[string]string{}
	w.numbers = map[string]string{}
	w.sent = map[int64]bool{}
	w.asked = map[string]bool{}
	w.quiet = map[string]time.Time{}
	w.looked = map[string]waTypes.JID{}
	w.waiters = map[string][]chan struct{}{}
	w.selves = map[string]bool{}
	w.listed = false
	w.ready = false
	w.self, w.selfName = "", ""
}

func (w *WhatsApp) Network() proto.Network { return net }

func (w *WhatsApp) Start(ctx context.Context) {
	w.mu.Lock()
	w.base = ctx
	w.stop()
	w.life, w.stop = context.WithCancel(ctx)
	var s savedSession
	ok, err := w.d.Session.LoadJSON(sessionFile, &s)
	if err != nil {
		hlog.Warn("whatsapp: session unreadable", hlog.Kind(err))
	}
	if ok && err == nil && s.Device == "" && s.Version > 0 {
		// The phone unlinked this device: what's kept stays until the user
		// logs out or links again, less what has disappeared since.
		w.mu.Unlock()
		w.d.Events.Account(net, proto.Errored, 0, "", unlinked)
		hlog.Go("whatsapp sweep kept", w.sweepKept)
		return
	}
	if !ok || err != nil || s.Device == "" {
		w.mu.Unlock()
		w.d.Events.Account(net, proto.LoggedOut, 0, "", "")
		return
	}
	gen, life := w.gen, w.life
	w.mu.Unlock()
	w.d.Events.Account(net, proto.Connecting, 0, "", "")
	hlog.Go("whatsapp resume", func() { w.resume(life, gen, s) })
}

// resume opens the kept device and connects it; whatsmeow keeps trying
// while the network is the problem.
func (w *WhatsApp) resume(life context.Context, gen int, s savedSession) {
	ctx, cancel := context.WithTimeout(life, 30*time.Second)
	defer cancel()
	st, err := w.open(ctx)
	if err != nil {
		hlog.Error("whatsapp: can't open the store", hlog.Kind(err))
		w.d.Events.Account(net, proto.Errored, 0, "", "tuimeta can't read what it kept for WhatsApp; log out and link it again.")
		return
	}
	jid, _ := waTypes.ParseJID(s.Device)
	dev, err := st.container.GetDevice(ctx, jid)
	if err != nil || dev == nil {
		st.Close()
		hlog.Warn("whatsapp: kept device missing", hlog.Kind(err))
		w.d.Events.Account(net, proto.Errored, 0, "", unlinked)
		return
	}
	w.mu.Lock()
	if w.gen != gen || w.closed {
		w.mu.Unlock()
		st.Close()
		return
	}
	w.st = st
	w.setSelf(dev)
	w.loadKept(ctx)
	w.watch(st)
	w.mu.Unlock()
	if err := w.dial(gen, dev); err != nil {
		hlog.Info("whatsapp: can't connect yet", hlog.Kind(err))
	}
}

// unlinked is what to do once the phone no longer has this device.
const unlinked = "WhatsApp unlinked tuimeta (from the phone's Linked devices, or after weeks unused): link it again with :login."

// open opens the store in the session folder.
func (w *WhatsApp) open(ctx context.Context) (*waStore, error) {
	path, err := w.d.Session.Path(storeFile)
	if err != nil {
		return nil, err
	}
	st, err := openStore(ctx, path)
	if err != nil {
		return nil, err
	}
	if err := st.container.LIDMap.FillCache(ctx); err != nil {
		hlog.Warn("whatsapp: can't read the id map", hlog.Kind(err))
	}
	if err := st.prune(ctx); err != nil {
		hlog.Warn("whatsapp: can't prune kept messages", hlog.Kind(err))
	}
	// Disappearing messages whose time ran out while tuimeta wasn't running,
	// and the keys whatsmeow kept for messages that aren't kept.
	if err := st.deleteExpired(ctx, w.now().UnixMilli()); err != nil {
		hlog.Warn("whatsapp: can't delete disappeared messages", hlog.Kind(err))
	}
	if err := st.pruneSecrets(ctx); err != nil {
		hlog.Warn("whatsapp: can't prune message keys", hlog.Kind(err))
	}
	st.checkpoint(ctx)
	// Decrypted files a download left when the helper stopped mid-way.
	if dir, err := w.d.Session.Dir(); err == nil {
		leftovers, _ := filepath.Glob(filepath.Join(dir, "download-*"))
		for _, f := range leftovers {
			_ = os.Remove(f)
		}
	}
	return st, nil
}

// watch runs the disappearing-message sweep for as long as st is the open
// store, connected or not; w.mu is held.
func (w *WhatsApp) watch(st *waStore) {
	life := w.life
	hlog.Go("whatsapp sweep", func() { w.sweepLoop(life, st) })
}

// scrubbed empties the store's write-ahead log after deletions, so what was
// deleted isn't left readable there; w.mu is held.
func (w *WhatsApp) scrubbed() {
	if !w.scrub || w.st == nil {
		return
	}
	w.scrub = false
	ctx, cancel := dbCtx()
	defer cancel()
	w.st.checkpoint(ctx)
}

// sweepKept deletes the disappeared messages of a store no device uses.
func (w *WhatsApp) sweepKept() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if st, err := w.open(ctx); err == nil {
		_ = st.Close()
	}
}

// setSelf records which keys are you; w.mu is held.
func (w *WhatsApp) setSelf(dev *store.Device) {
	w.selves = map[string]bool{}
	if dev.ID != nil {
		pn := dev.ID.ToNonAD().String()
		w.selves[pn] = true
		w.self = pn
	}
	if lid := dev.GetLID(); !lid.IsEmpty() {
		key := lid.ToNonAD().String()
		w.selves[key] = true
		if w.self != "" && w.self != key {
			w.alias[w.self] = key
			w.numbers[key] = w.self
		}
		w.self = key
	}
	w.selfName = dev.PushName
}

// connectDevice makes the whatsmeow client for dev and connects it.
func (w *WhatsApp) connectDevice(gen int, dev *store.Device) error {
	cli := whatsmeow.NewClient(dev, waLog.Noop)
	configure(cli)
	cli.AddEventHandlerWithSuccessStatus(func(evt any) bool { return w.onEvent(gen, evt) })
	w.mu.Lock()
	if w.gen != gen || w.closed {
		w.mu.Unlock()
		return nil
	}
	w.cli = &waConn{cli: cli}
	w.mu.Unlock()
	err := cli.Connect()
	w.mu.Lock()
	stale := w.gen != gen || w.closed
	w.mu.Unlock()
	if stale {
		// Logged out (or quitting) while it connected.
		cli.Disconnect()
	}
	return err
}

// becameReady reports the account ready.
func (w *WhatsApp) becameReady(gen int) {
	w.mu.Lock()
	if w.gen != gen || w.closed || w.ready {
		w.mu.Unlock()
		return
	}
	w.ready = true
	if w.cli != nil {
		// A link's first connection can come before Link has recorded who
		// you are.
		if own := w.cli.OwnID(); !own.IsEmpty() && w.self == "" {
			w.self = own.String()
			w.selves[w.self] = true
		}
		if own := w.cli.OwnLID(); !own.IsEmpty() {
			key := own.String()
			w.selves[key] = true
			if numberOf(w.self) != "" {
				w.alias[w.self], w.numbers[key] = key, w.self
			}
			w.self = key
		}
		if name := w.cli.PushName(); name != "" {
			w.selfName = name
		}
	}
	self := w.person(w.self)
	name := w.selfLabel()
	w.mu.Unlock()
	w.d.Events.Account(net, proto.Ready, self.id, name, "")
	w.mu.Lock()
	w.tellUser(self, true)
	w.mu.Unlock()
}

// selfLabel is how you're named in the account event; w.mu is held.
func (w *WhatsApp) selfLabel() string {
	if w.selfName != "" {
		return oneLine(w.selfName)
	}
	return phoneLabel(w.self)
}

// teardownLocked drops the connection without telling anyone anything;
// w.mu is held. whatsmeow lets go in the background.
func (w *WhatsApp) teardownLocked() {
	cli := w.cli
	w.cli = nil
	w.ready = false
	w.gen++
	if cli != nil {
		hlog.Go("whatsapp disconnect", cli.Disconnect)
	}
}

func (w *WhatsApp) Close() {
	w.mu.Lock()
	w.closed = true
	w.stop()
	if w.link != nil {
		w.link.end(nil, true)
	}
	w.scrub = true
	w.scrubbed()
	cli, st := w.cli, w.st
	w.cli, w.st = nil, nil
	w.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if cli != nil {
			cli.Disconnect()
		}
		if st != nil {
			_ = st.Close()
		}
	}()
	select {
	case <-done:
	case <-time.After(closeWait):
		hlog.Warn("whatsapp: whatsmeow slow to close")
	}
}

// Logout unlinks this device from the account (as "Log out" in the phone's
// Linked devices does) and deletes everything kept for it. The phone and the
// account's other devices stay logged in.
func (w *WhatsApp) Logout(ctx context.Context) error {
	w.mu.Lock()
	if w.link != nil {
		w.link.end(errCancelled, true)
	}
	cli, st, ready := w.cli, w.st, w.ready
	linked := w.linked()
	w.stop()
	w.life, w.stop = context.WithCancel(w.base)
	w.cli, w.st = nil, nil
	w.gen++
	w.reset()
	w.mu.Unlock()

	unlinked := false
	if cli != nil {
		if ready {
			lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := cli.Logout(lctx)
			cancel()
			unlinked = err == nil
			if err != nil {
				hlog.Warn("whatsapp: unlinking failed", hlog.Kind(err))
			}
		}
		cli.Disconnect()
	}
	if linked && !unlinked {
		// The device is still in the phone's list: say so, as it can't be
		// removed from here any more.
		w.d.Events.Error(net, "tuimeta couldn't reach WhatsApp to unlink itself; remove it in the phone's Linked devices.")
	}
	if st != nil {
		_ = st.Close()
	}
	if err := w.d.Session.Wipe(); err != nil {
		hlog.Error("whatsapp: can't wipe session", hlog.Kind(err))
	}
	w.d.Events.Account(net, proto.LoggedOut, 0, "", "")
	return nil
}

// LoginCookies isn't how WhatsApp logs in (the server never calls it).
func (w *WhatsApp) LoginCookies(context.Context, cookies.Set, browser.Identity) error {
	return proto.Err(proto.BadRequest, "WhatsApp logs in by linking tuimeta to your phone, not with cookies.")
}

// connected is the connection for a request, or an error saying why there's
// none.
func (w *WhatsApp) connected() (waAPI, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cli == nil || !w.ready {
		return nil, errNotConnected
	}
	return w.cli, nil
}

var (
	errNotConnected = proto.Err(proto.NetworkError, "WhatsApp isn't connected right now; try again in a moment.")
	errCancelled    = proto.Err(proto.Cancelled, "Linking was cancelled.")
)

// requestError turns a library error into one tuimeta can show.
func requestError(err error) error {
	if err == nil {
		return nil
	}
	var pe *proto.Error
	if errors.As(err, &pe) {
		return pe
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return proto.Err(proto.NetworkError, "WhatsApp didn't answer in time; try again.")
	case errors.Is(err, whatsmeow.ErrNotConnected), errors.Is(err, whatsmeow.ErrNotLoggedIn):
		return errNotConnected
	case errors.Is(err, whatsmeow.ErrIQRateOverLimit):
		return proto.Err(proto.NetworkError, "WhatsApp says that was too many requests; wait a little and try again.")
	}
	return proto.Err(proto.NetworkError, "WhatsApp refused that; try again.")
}
