// SPDX-License-Identifier: AGPL-3.0-or-later

package messenger

import (
	"context"
	"time"

	"go.mau.fi/mautrix-meta/pkg/messagix/socket"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/history"
	"github.com/erictran308/tuimeta/helper/internal/hlog"
)

// Paging Messenger's history.
const (
	// HistoryWait is how long a page of older messages is waited for.
	HistoryWait = 20 * time.Second
	// MaxHistoryFetches bounds the pages one history request asks for (an
	// "around" far in the past may need several).
	MaxHistoryFetches = 8
)

// History answers from the messages the backend has, asking Messenger for
// older pages while the answer would come up short. Encrypted chats have no
// history on the server: they have what arrived since this device was
// linked, and that's what they answer with.
func (m *Messenger) History(ctx context.Context, ref backend.ChatRef, q history.Query) (history.Page, error) {
	for fetches := 0; ; fetches++ {
		m.mu.Lock()
		c, err := m.chatOf(ref.ID)
		if err != nil {
			m.mu.Unlock()
			return history.Page{}, err
		}
		page := c.log.Page(q)
		need := m.needsOlder(c, q, page)
		var oldest *message
		if need {
			oldest = m.oldestFB(c)
		}
		key, thread := c.key, c.threadKey()
		m.mu.Unlock()
		if !need || fetches >= MaxHistoryFetches {
			return page, nil
		}
		if err := m.fetchOlder(ctx, key, thread, oldest); err != nil {
			if fetches == 0 && len(page.Messages) == 0 {
				return history.Page{}, err
			}
			hlog.Info("messenger: older messages didn't come", hlog.Kind(err))
			return page, nil
		}
	}
}

// needsOlder reports whether the answer to q would get better with an older
// page from Messenger; m.mu is held.
func (m *Messenger) needsOlder(c *chat, q history.Query, page history.Page) bool {
	if c.log.Complete() || !c.fbHistory || m.meta == nil {
		return false
	}
	if c.encrypted && !c.olderFB() && c.log.Len() > 0 {
		return false
	}
	switch {
	case q.After != nil:
		return false
	case q.Around != nil:
		oldest, ok := c.log.Oldest()
		return !ok || *q.Around < oldest.ID || len(page.Messages) < q.Limit
	}
	return len(page.Messages) < max(q.Limit, 1)
}

// oldestFB is the oldest Facebook message of c, the reference for the next
// page; m.mu is held.
func (m *Messenger) oldestFB(c *chat) *message {
	var oldest *message
	for _, msg := range c.msgs {
		if msg.wa != nil || len(msg.ids) == 0 || isEvent(msg) {
			continue
		}
		if oldest == nil || msg.ms < oldest.ms || (msg.ms == oldest.ms && msg.ids[0] < oldest.ids[0]) {
			oldest = msg
		}
	}
	return oldest
}

func isEvent(msg *message) bool { return len(msg.netID) > 9 && msg.netID[:9] == "wa-event:" }

// fetchOlder asks Messenger for the page of messages before oldest (or the
// newest, with none) and waits for it to be applied.
func (m *Messenger) fetchOlder(ctx context.Context, key, thread int64, oldest *message) error {
	meta, err := m.connected()
	if err != nil {
		return err
	}
	task := &socket.FetchMessagesTask{ThreadKey: thread, SyncGroup: 1, Cursor: meta.Cursor(1)}
	if oldest != nil {
		task.ReferenceTimestampMs = oldest.ms
		task.ReferenceMessageId = oldest.netID
	} else {
		task.ReferenceTimestampMs = m.now().UnixMilli()
	}
	woke := make(chan struct{})
	m.mu.Lock()
	m.waiters[key] = append(m.waiters[key], woke)
	before := 0
	if c := m.chats[key]; c != nil {
		before = c.log.Len()
	}
	m.mu.Unlock()
	if _, err := m.run(ctx, "history", task); err != nil {
		return err
	}
	// The page comes in the answer or soon after on the socket.
	timer := time.NewTimer(HistoryWait)
	defer timer.Stop()
	select {
	case <-woke:
	case <-timer.C:
		return errNotConnected
	case <-ctx.Done():
		return ctx.Err()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.chats[key]; c != nil && c.log.Len() == before && !c.log.Complete() {
		// Nothing older came: there's nothing older to show.
		c.fbHistory = false
		c.refreshComplete()
	}
	return nil
}
