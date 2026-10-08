// SPDX-License-Identifier: AGPL-3.0-or-later

// Package history keeps a chat's messages in order and pages through them as
// the history request does: before, after or around a position in time.
package history

import (
	"slices"
	"sort"
	"sync"

	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// Query is a history request. Before, After and Around are positions in
// time (any id, a message's or not); at most one is set. None means the
// newest messages.
type Query struct {
	Before, After, Around *int64
	Limit                 int
}

// Page is a history answer, oldest first. HasMore says more messages lie
// past the page's far end: older ones, or newer ones for After.
type Page struct {
	Messages []proto.Message
	HasMore  bool
}

// Log is one chat's known messages, ordered by id. A backend keeps the
// newest end current (new messages, edits, deletions) and fills in older
// ones as it fetches them; Complete says when it reaches the chat's start.
type Log struct {
	mu       sync.Mutex
	msgs     []proto.Message
	complete bool
}

func New() *Log { return &Log{} }

func (l *Log) index(id int64) (int, bool) {
	i := sort.Search(len(l.msgs), func(i int) bool { return l.msgs[i].ID >= id })
	return i, i < len(l.msgs) && l.msgs[i].ID == id
}

// Put adds messages, or replaces ones with the same id.
func (l *Log) Put(ms ...proto.Message) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, m := range ms {
		i, found := l.index(m.ID)
		if found {
			l.msgs[i] = m
		} else {
			l.msgs = slices.Insert(l.msgs, i, m)
		}
	}
}

// Remove deletes messages by id, returning the ids it had.
func (l *Log) Remove(ids ...int64) []int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	var removed []int64
	for _, id := range ids {
		if i, found := l.index(id); found {
			l.msgs = slices.Delete(l.msgs, i, i+1)
			removed = append(removed, id)
		}
	}
	return removed
}

// Get is the message with id.
func (l *Log) Get(id int64) (proto.Message, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if i, found := l.index(id); found {
		return l.msgs[i], true
	}
	return proto.Message{}, false
}

// Newest is the last message, if any.
func (l *Log) Newest() (proto.Message, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.msgs) == 0 {
		return proto.Message{}, false
	}
	return l.msgs[len(l.msgs)-1], true
}

// Oldest is the first message, if any.
func (l *Log) Oldest() (proto.Message, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.msgs) == 0 {
		return proto.Message{}, false
	}
	return l.msgs[0], true
}

// Len is how many messages there are.
func (l *Log) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.msgs)
}

// All is every message, oldest first.
func (l *Log) All() []proto.Message {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.msgs)
}

// SetComplete records whether the log reaches the chat's first message.
func (l *Log) SetComplete(c bool) {
	l.mu.Lock()
	l.complete = c
	l.mu.Unlock()
}

// Complete reports whether the log reaches the chat's first message.
func (l *Log) Complete() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.complete
}

// Page answers q from what the log holds. An album is never cut: a page
// that would end inside one takes the rest of it too, so it can hold a few
// more than Limit.
func (l *Log) Page(q Query) Page {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(l.msgs)
	limit := max(q.Limit, 1)
	first := func(id int64) int { // the first message at or after id
		i, _ := l.index(id)
		return i
	}
	var lo, hi int
	older := true // whether HasMore looks at the older end
	switch {
	case q.Before != nil:
		hi = first(*q.Before)
		lo = max(0, hi-limit)
		lo = l.down(lo)
	case q.After != nil:
		lo = n
		if *q.After < 1<<62 {
			lo = first(*q.After + 1)
		}
		hi = min(n, lo+limit)
		hi = l.up(hi)
		older = false
	case q.Around != nil:
		pos := first(*q.Around)
		lo = max(0, pos-limit/2)
		hi = min(n, lo+limit)
		if hi-lo < limit {
			lo = max(0, hi-limit)
		}
		lo, hi = l.down(lo), l.up(hi)
	default:
		hi = n
		lo = max(0, n-limit)
		lo = l.down(lo)
	}
	p := Page{Messages: slices.Clone(l.msgs[lo:hi])}
	if older {
		p.HasMore = lo > 0 || !l.complete
	} else {
		p.HasMore = hi < n
	}
	if p.Messages == nil {
		p.Messages = []proto.Message{}
	}
	return p
}

// down moves a page's first index back to its album's first part.
func (l *Log) down(lo int) int {
	for lo > 0 && lo < len(l.msgs) && l.msgs[lo].Album != 0 && l.msgs[lo-1].Album == l.msgs[lo].Album {
		lo--
	}
	return lo
}

// up moves a page's end past its last album's last part.
func (l *Log) up(hi int) int {
	for hi > 0 && hi < len(l.msgs) && l.msgs[hi-1].Album != 0 && l.msgs[hi].Album == l.msgs[hi-1].Album {
		hi++
	}
	return hi
}
