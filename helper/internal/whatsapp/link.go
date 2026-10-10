// SPDX-License-Identifier: AGPL-3.0-or-later

package whatsapp

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	waTypes "go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// PairCodeLife is how long a pairing code works: WhatsApp closes the
// linking connection once its QR codes run out, about 160 seconds after it
// opened.
const PairCodeLife = 160 * time.Second

// linkTry is a login_link waiting for the phone. Once the phone has
// confirmed the link (paired), cancelling it no longer ends it: the device
// the phone added completes, rather than being thrown away here and left
// in the phone's list. Only quitting, or a forced end (logout), ends it then.
type linkTry struct {
	attempt uint64
	cancel  context.CancelFunc
	done    chan struct{}

	mu     sync.Mutex
	err    error
	ended  bool
	paired bool
}

// end stops the try with err (nil: the helper is quitting), unless the
// phone has confirmed it and force isn't set.
func (l *linkTry) end(err error, force bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ended || (l.paired && err != nil && !force) {
		return
	}
	l.ended, l.err = true, err
	l.cancel()
	close(l.done)
}

// pair marks the try confirmed by the phone, unless it has ended: whatsmeow
// asks just before it tells WhatsApp the link is accepted, so an ended try
// is refused there, and no device is added to the phone's list for it.
func (l *linkTry) pair() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ended {
		return false
	}
	l.paired = true
	return true
}

// over reports whether the try has ended, and why.
func (l *linkTry) over() (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ended, l.err
}

// CancelLink ends the waiting link of attempt (any, with 0), unless the
// phone has already confirmed it. A cancel meant for an older attempt never
// ends a newer one, and a link of attempt (or an older one) that comes
// after its cancel isn't started.
func (w *WhatsApp) CancelLink(attempt uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cancelled = max(w.cancelled, attempt)
	if w.link != nil && (attempt == 0 || w.link.attempt == attempt) {
		w.link.end(errCancelled, false)
	}
}

// Link links tuimeta as a new device of the account. It starts afresh:
// whatever was kept (an unlinked device, another account's chats, ids and
// downloads) goes first. The phone then scans a QR code, or takes a pairing
// code when a number is given; whatsmeow keeps the device, and once it's
// connected the account is ready.
func (w *WhatsApp) Link(ctx context.Context, phone string, attempt uint64) error {
	lctx, cancel := context.WithCancel(ctx)
	try := &linkTry{attempt: attempt, done: make(chan struct{}), cancel: cancel}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		cancel()
		return proto.Err(proto.Internal, "The helper is quitting.")
	}
	if attempt != 0 && attempt <= w.cancelled {
		// Given up before it started: nothing is wiped, nothing opened.
		w.mu.Unlock()
		cancel()
		return errCancelled
	}
	if w.ready || w.linked() {
		// A kept device reconnects by itself; linking again would throw it
		// away (and leave it in the phone's list).
		w.mu.Unlock()
		cancel()
		return proto.Err(proto.BadRequest, "WhatsApp is linked already and reconnects by itself; to link again, log out first.")
	}
	if w.link != nil {
		w.link.end(errCancelled, false)
	}
	w.link = try
	w.stop()
	w.life, w.stop = context.WithCancel(w.base)
	w.teardownLocked()
	old := w.st
	w.st = nil
	w.reset()
	gen, life := w.gen, w.life
	w.mu.Unlock()
	stopLife := context.AfterFunc(life, func() { try.end(nil, true) })
	defer stopLife()
	defer func() {
		w.mu.Lock()
		if w.link == try {
			w.link = nil
		}
		w.mu.Unlock()
		cancel()
	}()

	if old != nil {
		_ = old.Close()
	}
	w.keeping.Wait() // an unlinked device's store being opened is closed first
	if err := w.d.Session.Wipe(); err != nil {
		hlog.Error("whatsapp: can't wipe session", hlog.Kind(err))
	}
	// Nothing of an earlier account stays: its ids and its downloads go as
	// a logout's do.
	for _, id := range w.d.IDs.ChatsOf(net) {
		w.d.Messages.ForgetChat(id)
	}
	w.d.IDs.Forget(net)
	if w.d.Downloads != nil {
		if err := w.d.Downloads.Forget(net); err != nil {
			hlog.Error("whatsapp: can't delete downloads", hlog.Kind(err))
		}
	}
	if w.d.Events.State(net) != proto.LoggedOut {
		w.d.Events.Account(net, proto.LoggedOut, 0, "", "")
	}
	st, err := w.open(lctx)
	if err != nil {
		hlog.Error("whatsapp: can't open the store", hlog.Kind(err))
		return proto.Err(proto.Internal, "tuimeta can't make its WhatsApp store; its log has the details.")
	}
	dev := st.container.NewDevice()
	cli := whatsmeow.NewClient(dev, waLog.Noop)
	configure(cli)
	cli.PrePairCallback = func(waTypes.JID, string, string) bool { return try.pair() }
	codes, err := cli.GetQRChannel(lctx)
	if err != nil {
		_ = st.Close()
		return proto.Err(proto.Internal, "Linking couldn't start; try again.")
	}
	cli.AddEventHandlerWithSuccessStatus(func(evt any) bool { return w.onEvent(gen, evt) })
	w.mu.Lock()
	if w.gen != gen || w.closed {
		w.mu.Unlock()
		_ = st.Close()
		return errCancelled
	}
	w.st, w.cli = st, newConn(cli)
	w.watch(st)
	w.mu.Unlock()

	fail := func(err error) error {
		if dev.ID != nil {
			// The phone confirmed it before the link failed here: the
			// device it added is removed again, or the user is told to.
			uctx, ucancel := context.WithTimeout(context.Background(), 10*time.Second)
			uerr := cli.Logout(uctx)
			ucancel()
			if uerr != nil {
				hlog.Warn("whatsapp: can't unlink a failed link", hlog.Kind(uerr))
				w.d.Events.Error(net, "The link didn't finish; remove the new device in the phone's Linked devices.")
			}
		}
		w.mu.Lock()
		current := w.gen == gen
		if current {
			w.teardownLocked()
			w.st = nil
		}
		w.mu.Unlock()
		cli.Disconnect()
		_ = st.Close()
		if current {
			if werr := w.d.Session.Wipe(); werr != nil {
				hlog.Error("whatsapp: can't wipe session", hlog.Kind(werr))
			}
		}
		return err
	}
	if err := cli.Connect(); err != nil {
		hlog.Info("whatsapp: can't reach the linking server", hlog.Kind(err))
		return fail(proto.Err(proto.NetworkError, "Can't reach WhatsApp; check the connection and try again."))
	}
	pair := func(ctx context.Context, phone string) (string, error) {
		return cli.PairPhone(ctx, phone, true, whatsmeow.PairClientChrome, pairName())
	}
	if err := w.awaitScan(lctx, try, phone, codes, pair); err != nil {
		return fail(err)
	}

	// whatsmeow has kept the device; WhatsApp reconnects it as a linked one.
	if dev.ID == nil {
		return fail(proto.Err(proto.NetworkError, "Linking didn't work; try again."))
	}
	w.mu.Lock()
	if w.gen != gen {
		// Logged out as the phone confirmed it.
		w.mu.Unlock()
		return fail(errCancelled)
	}
	// Kept under the lock, so a logout can't wipe the folder in between and
	// find it linked again after.
	serr := w.d.Session.SaveJSON(sessionFile, savedSession{Version: 1, Device: dev.ID.String()})
	if serr == nil {
		w.setSelf(dev)
	}
	ready := w.ready
	w.mu.Unlock()
	if serr != nil {
		// Not kept, the device would be lost at the next start, still in
		// the phone's list: the link is undone instead.
		hlog.Error("whatsapp: can't save session", hlog.Kind(serr))
		return fail(proto.Err(proto.Internal, "tuimeta couldn't keep the link; check there's room on the disk, then try again."))
	}
	if !ready {
		w.d.Events.Account(net, proto.Connecting, 0, "", "")
	}
	hlog.Info("whatsapp: linked")
	return w.awaitReady(lctx, try, gen)
}

// linked reports whether a device that still works is kept (one the phone
// unlinked isn't); w.mu is held.
func (w *WhatsApp) linked() bool {
	var s savedSession
	ok, err := w.d.Session.LoadJSON(sessionFile, &s)
	return ok && err == nil && s.Device != ""
}

// awaitScan shows the codes as they come until the phone links: QR codes,
// or for a phone number the pairing code asked for once the linking
// connection is up. It returns nil once linked.
func (w *WhatsApp) awaitScan(ctx context.Context, try *linkTry, phone string, codes <-chan whatsmeow.QRChannelItem, pair func(context.Context, string) (string, error)) error {
	opened := w.now()
	asked := false
	stopped := func() error {
		_, err := try.over()
		if err == nil {
			return proto.Err(proto.Internal, "The helper is quitting.")
		}
		return err
	}
	for {
		select {
		case <-try.done:
			return stopped()
		case item, ok := <-codes:
			if !ok {
				return proto.Err(proto.Timeout, "Nothing was linked in time; ask for a new code.")
			}
			switch item.Event {
			case whatsmeow.QRChannelEventCode:
				var qr, pairing string
				var expires time.Time
				if phone == "" {
					qr, expires = item.Code, w.now().Add(item.Timeout)
				} else if !asked {
					// The first code says the connection is up; a phone
					// link asks for its pairing code then.
					asked = true
					code, err := pair(ctx, phone)
					if err != nil {
						if ended, _ := try.over(); ended {
							return stopped()
						}
						return pairError(err)
					}
					pairing, expires = code, opened.Add(PairCodeLife)
				} else {
					continue
				}
				// A code of a link given up meanwhile is never shown.
				if ended, _ := try.over(); ended {
					return stopped()
				}
				w.d.Events.LoginCode(net, try.attempt, qr, pairing, expires)
			case whatsmeow.QRChannelSuccess.Event:
				return nil
			case whatsmeow.QRChannelTimeout.Event:
				if phone != "" {
					return proto.Err(proto.Timeout, "The code wasn't typed on the phone in time; ask for a new one.")
				}
				return proto.Err(proto.Timeout, "No code was scanned in time; ask for a new one.")
			case whatsmeow.QRChannelClientOutdated.Event:
				return proto.Err(proto.Unsupported, "WhatsApp says this version of tuimeta is too old to link; update tuimeta.")
			case whatsmeow.QRChannelScannedWithoutMultidevice.Event:
				return proto.Err(proto.Unsupported, "The phone's WhatsApp can't link devices yet; update it, then try again.")
			case whatsmeow.QRChannelEventPasskeyRequest, whatsmeow.QRChannelEventPasskeyResponse:
				return proto.Err(proto.Unsupported, "WhatsApp wants this link confirmed with a passkey, which tuimeta can't do; try linking the other way.")
			default:
				hlog.Info("whatsapp: linking failed", hlog.Str("step", item.Event))
				return proto.Err(proto.NetworkError, "Linking didn't work; try again.")
			}
		}
	}
}

// awaitReady waits for the linked device's first connection.
func (w *WhatsApp) awaitReady(ctx context.Context, try *linkTry, gen int) error {
	deadline := time.NewTimer(ConnectWait)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		w.mu.Lock()
		ready, current := w.ready, w.gen == gen
		w.mu.Unlock()
		switch {
		case ready && current:
			return nil
		case !current:
			return errCancelled
		}
		select {
		case <-tick.C:
		case <-try.done:
			if _, err := try.over(); err != nil {
				return err
			}
			return proto.Err(proto.Internal, "The helper is quitting.")
		case <-ctx.Done():
			return errCancelled
		case <-deadline.C:
			// Linked, but not connected yet: whatsmeow keeps trying, and the
			// account turns ready when it gets through.
			return proto.Err(proto.NetworkError, "Linked, but WhatsApp hasn't connected yet; it keeps trying.")
		}
	}
}

// pairError is what to say when WhatsApp refuses a phone number.
func pairError(err error) error {
	hlog.Info("whatsapp: pairing code refused", hlog.Kind(err))
	switch {
	case errors.Is(err, whatsmeow.ErrPhoneNumberTooShort), errors.Is(err, whatsmeow.ErrPhoneNumberIsNotInternational):
		return proto.Err(proto.BadRequest, "That isn't a phone number in international form; write it with the country code.")
	case errors.Is(err, whatsmeow.ErrIQRateOverLimit):
		return proto.Err(proto.NetworkError, "WhatsApp says too many codes were asked for; wait a while and try again.")
	}
	return proto.Err(proto.NetworkError, "WhatsApp didn't give a code for that number; check it's the account's, then try again.")
}
