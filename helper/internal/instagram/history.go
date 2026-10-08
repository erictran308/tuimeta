// SPDX-License-Identifier: AGPL-3.0-or-later

package instagram

import (
	"context"
	"errors"
	"math"
	"strconv"

	"go.mau.fi/mautrix-meta/pkg/instameow/slidetypes"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/history"
	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

const (
	// pageSize is how many messages a history page asks Instagram for, as
	// the web client and the connector do.
	pageSize = 20
	// maxHistoryPages bounds what one history request fetches.
	maxHistoryPages = 25
)

const errHistory = "Couldn't load messages from Instagram; try again."

// chatOf is the backend's chat for a chat the server resolved; b.mu is held.
func (b *Instagram) chatOf(ref backend.ChatRef) (*chat, error) {
	key, err := strconv.ParseInt(ref.NetID, 10, 64)
	if err != nil {
		return nil, proto.ErrNoChat
	}
	c := b.chats[key]
	if c == nil {
		return nil, proto.ErrNoChat
	}
	return c, nil
}

// History answers from what's loaded, fetching older pages from Instagram
// until the request can be answered. Fetching history only reads: nothing
// here marks anything seen (only MarkRead does).
func (b *Instagram) History(ctx context.Context, ref backend.ChatRef, q history.Query) (history.Page, error) {
	conn, err := b.current()
	if err != nil {
		return history.Page{}, err
	}
	b.mu.Lock()
	c, err := b.chatOf(ref)
	b.mu.Unlock()
	if err != nil {
		return history.Page{}, err
	}
	c.fetchMu.Lock()
	defer c.fetchMu.Unlock()
	for range maxHistoryPages {
		b.mu.Lock()
		need, newest := c.needsOlder(q)
		gen, igid, cursor := c.histGen, c.igid, c.cursor
		oldest := ""
		if m, ok := c.log.Oldest(); ok {
			if p, ok := b.d.Messages.Lookup(c.id, m.ID); ok {
				oldest = p.NetID
			}
		}
		if need && igid == "" {
			// A dm that isn't on Instagram yet (opened, not written in).
			c.fetched = true
			c.log.SetComplete(true)
			need = false
		}
		b.mu.Unlock()
		if !need {
			break
		}
		if newest {
			resp, err := conn.cli.GetThread(withQuietLog(ctx), slidetypes.MakeGetThreadInfoRequest(igid))
			if err != nil {
				return history.Page{}, requestError(err, errHistory)
			}
			b.mu.Lock()
			if c.histGen == gen && b.conn == conn {
				if t := resp.ThreadInfo.AsIGDirectThread; t != nil {
					b.threadPage(t)
				}
				if !c.fetched {
					// Instagram answered without the thread's messages.
					c.fetched = true
					c.log.SetComplete(true)
				}
				b.changed(c)
			}
			b.mu.Unlock()
			continue
		}
		req := &slidetypes.PaginateMessagesRequest{ThreadID: igid, FirstN: pageSize, InitialMessagePageCount: pageSize}
		switch {
		case cursor != "":
			req.AfterCursor = &cursor
		case oldest != "":
			req.OlderThanMessageID = &oldest
		default:
			b.mu.Lock()
			c.log.SetComplete(true)
			b.mu.Unlock()
			continue
		}
		resp, err := conn.cli.PaginateMessages(withQuietLog(ctx), req)
		if err != nil {
			// What's loaded is still an answer (has_more stays true, so
			// tuimeta asks again later); nothing at all is an error.
			if b.hasHistory(c) && !errors.Is(err, context.Canceled) {
				hlog.Warn("instagram: an older page failed", hlog.Kind(err))
				break
			}
			return history.Page{}, requestError(err, errHistory)
		}
		b.mu.Lock()
		if c.histGen == gen && b.conn == conn {
			b.olderPage(c, resp.ThreadInfo.AsIGDirectThread)
		}
		b.mu.Unlock()
	}
	b.mu.Lock()
	page := c.log.Page(q)
	b.mu.Unlock()
	return page, nil
}

// hasHistory says whether any of the chat's history is loaded.
func (b *Instagram) hasHistory(c *chat) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return c.log.Len() > 0
}

// needsOlder says whether q needs messages older than the log holds, and
// whether the newest page itself is still to be fetched; b.mu is held.
func (c *chat) needsOlder(q history.Query) (need, newest bool) {
	if !c.fetched {
		return true, true
	}
	if c.log.Complete() {
		return false, false
	}
	limit := max(q.Limit, 1)
	before, want := int64(math.MaxInt64), limit
	switch {
	case q.Before != nil:
		before = *q.Before
	case q.Around != nil:
		before, want = *q.Around, limit/2
	case q.After != nil:
		// Newer messages are all loaded; older ones are needed only if the
		// position lies before everything loaded.
		oldest, ok := c.log.Oldest()
		return !ok || oldest.ID > *q.After, false
	}
	count := 0
	for _, m := range c.log.All() {
		if m.ID < before {
			count++
		}
	}
	return count < want, false
}

// olderPage keeps a page of older messages; b.mu is held.
func (b *Instagram) olderPage(c *chat, t *slidetypes.MessagesOnlyThread) {
	if t == nil || t.Messages == nil {
		c.log.SetComplete(true)
		return
	}
	added := 0
	for _, edge := range t.Messages.Edges {
		n := b.convert(c, edge.Node)
		if n == nil || n.skip {
			continue
		}
		if old := c.msgs[n.netID]; old == nil || !old.inLog {
			added++
		}
		n = b.keep(c, n)
		b.putLog(c, n)
	}
	info := t.Messages.PageInfo
	c.cursor = info.EndCursor
	// A page that brings nothing new would only be asked for again.
	if !info.HasNextPage || len(t.Messages.Edges) == 0 || added == 0 || info.EndCursor == "" {
		c.log.SetComplete(true)
	}
}

// GetMessage is a message from the history, or one known from a reply that
// quoted it.
func (b *Instagram) GetMessage(ctx context.Context, ref backend.MessageRef) (proto.Message, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, err := b.chatOf(ref.Chat)
	if err != nil {
		return proto.Message{}, err
	}
	if m, ok := c.log.Get(ref.ID); ok {
		return m, nil
	}
	n := c.msgs[ref.NetID]
	if n == nil {
		return proto.Message{}, proto.ErrNoMessage
	}
	parts := b.parts(c, n)
	for _, p := range parts {
		if p.ID == ref.ID {
			return p, nil
		}
	}
	return proto.Message{}, proto.ErrNoMessage
}
