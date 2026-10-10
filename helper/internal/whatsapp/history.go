// SPDX-License-Identifier: AGPL-3.0-or-later

package whatsapp

import (
	"context"
	"slices"
	"time"

	"go.mau.fi/whatsmeow"
	waTypes "go.mau.fi/whatsmeow/types"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/history"
	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// HistoryWait is how long a page of older messages is waited for (tests
// shorten it).
var HistoryWait = 20 * time.Second

// Asking the phone for older messages.
const (
	// HistoryPage is how many older messages one request asks for.
	HistoryPage = 50
	// MaxHistoryFetches bounds the pages one history request asks for.
	MaxHistoryFetches = 4
	// PhoneQuietFor is how long a chat's older messages aren't asked for
	// again after the phone didn't answer (it's likely offline).
	PhoneQuietFor = 2 * time.Minute
)

// History answers from the messages kept here, asking the phone for older
// ones while the answer would come up short. WhatsApp has no history on its
// servers: what the phone doesn't send, there isn't.
func (w *WhatsApp) History(ctx context.Context, ref backend.ChatRef, q history.Query) (history.Page, error) {
	for fetches := 0; ; fetches++ {
		w.mu.Lock()
		c, err := w.chatOf(ref.ID)
		if err != nil {
			w.mu.Unlock()
			return history.Page{}, err
		}
		w.ensureLoaded(c)
		page := c.log.Page(q)
		need := needsOlder(c, q, page)
		var oldest *message
		if since, ok := w.quiet[c.key]; ok && w.now().Sub(since) < PhoneQuietFor {
			need = false
		}
		if need {
			oldest = w.oldest(c)
			need = oldest != nil
		}
		key, before := c.key, c.log.Len()
		w.mu.Unlock()
		if !need || fetches >= MaxHistoryFetches {
			return page, nil
		}
		if err := w.fetchOlder(ctx, key, oldest, before); err != nil {
			hlog.Info("whatsapp: older messages didn't come", hlog.Kind(err))
			return page, nil
		}
	}
}

// needsOlder reports whether the answer to q would get better with older
// messages from the phone.
func needsOlder(c *chat, q history.Query, page history.Page) bool {
	if c.log.Complete() || c.log.Len() == 0 {
		return false
	}
	switch {
	case q.After != nil:
		return false
	case q.Around != nil:
		oldest, ok := c.log.Oldest()
		return ok && *q.Around < oldest.ID
	}
	return len(page.Messages) < max(q.Limit, 1)
}

// oldest is c's oldest real message (not an event made here), the anchor
// for asking the phone for older ones; w.mu is held.
func (w *WhatsApp) oldest(c *chat) *message {
	var oldest *message
	for _, m := range c.msgs {
		if len(m.ID) > 6 && m.ID[:6] == "event:" {
			continue
		}
		if oldest == nil || m.MS < oldest.MS {
			oldest = m
		}
	}
	return oldest
}

// fetchOlder asks the phone for the messages before oldest and waits for
// them (or for the phone to say there are none).
func (w *WhatsApp) fetchOlder(ctx context.Context, key string, oldest *message, before int) error {
	cli, err := w.connected()
	if err != nil {
		return err
	}
	w.mu.Lock()
	chat, _ := waTypes.ParseJID(key)
	sender, _ := waTypes.ParseJID(w.resolve(oldest.Sender))
	info := &waTypes.MessageInfo{
		MessageSource: waTypes.MessageSource{Chat: chat, Sender: sender, IsFromMe: w.isSelf(oldest.Sender), IsGroup: chat.Server == waTypes.GroupServer},
		ID:            oldest.ID,
		Timestamp:     time.UnixMilli(oldest.MS),
	}
	own := cli.OwnID()
	woke := make(chan struct{})
	w.waiters[key] = append(w.waiters[key], woke)
	w.mu.Unlock()
	defer func() {
		// A wait that ended without the phone's answer leaves nothing behind.
		w.mu.Lock()
		w.waiters[key] = slices.DeleteFunc(w.waiters[key], func(ch chan struct{}) bool { return ch == woke })
		if len(w.waiters[key]) == 0 {
			delete(w.waiters, key)
		}
		w.mu.Unlock()
	}()
	if own.IsEmpty() {
		return errNotConnected
	}
	req := cli.BuildHistorySyncRequest(info, HistoryPage)
	if _, err := cli.SendMessage(ctx, own, req, whatsmeow.SendRequestExtra{Peer: true}); err != nil {
		return requestError(err)
	}
	timer := time.NewTimer(HistoryWait)
	defer timer.Stop()
	select {
	case <-woke:
	case <-timer.C:
		w.mu.Lock()
		w.quiet[key] = w.now()
		w.mu.Unlock()
		return proto.Err(proto.NetworkError, "The phone didn't send older messages; is it online?")
	case <-ctx.Done():
		return ctx.Err()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if c := w.chats[key]; c != nil && c.log.Len() == before && !c.log.Complete() {
		// Nothing older came: there's nothing older to show.
		c.Complete = true
		c.log.SetComplete(true)
		w.saveChat(c)
	}
	return nil
}

// sweepLoop deletes disappearing messages once their time is up, here as
// on the phone.
func (w *WhatsApp) sweepLoop(life context.Context, st *waStore) {
	tick := time.NewTicker(SweepEvery)
	defer tick.Stop()
	for {
		if !w.sweep(st) {
			return
		}
		select {
		case <-tick.C:
		case <-life.Done():
			return
		}
	}
}

// sweep deletes the messages whose time is up; false once st isn't the
// open store any more.
func (w *WhatsApp) sweep(st *waStore) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.st != st || st == nil || w.closed {
		return false
	}
	defer w.scrubbed()
	ctx, cancel := dbCtx()
	gone, err := w.st.expired(ctx, w.now().UnixMilli())
	cancel()
	if err != nil {
		hlog.Error("whatsapp: can't look for disappearing messages", hlog.Kind(err))
		return true
	}
	for _, e := range gone {
		c := w.chats[e.chat]
		if c == nil {
			// Kept for a chat that isn't here (a device the phone unlinked
			// has none): it goes with what was downloaded of it.
			if e.msg != nil {
				w.forgetFiles(e.msg)
			}
			ctx, cancel := dbCtx()
			_ = w.st.deleteMessage(ctx, e.chat, e.id, nil)
			cancel()
			w.scrub = true
			continue
		}
		// Read in or not (one whose time is up is left out when they're
		// read in), it goes from tuimeta too, with its files.
		w.ensureLoaded(c)
		w.deleted(c, e.id)
	}
	if len(gone) > 0 {
		hlog.Info("whatsapp: disappearing messages deleted", hlog.Int("count", int64(len(gone))))
	}
	return true
}
