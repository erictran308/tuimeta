// SPDX-License-Identifier: AGPL-3.0-or-later

package ids

import (
	"slices"
	"sync"
	"time"
)

// MaxSlot is the last of the 256 ids a millisecond has.
const MaxSlot = 255

// maxParts bounds an album, so one message can't take a whole millisecond.
const maxParts = 64

// maxMs keeps ms << 8 a positive int64.
const maxMs = 1<<55 - 1

// MessageID is the id of slot in millisecond ms.
func MessageID(ms int64, slot int) int64 { return ms<<8 | int64(slot) }

// Millis is the millisecond a message id was made from.
func Millis(id int64) int64 { return id >> 8 }

// Part is what a message id stands for.
type Part struct {
	// NetID is the network's id of the whole message; empty for a message
	// still being sent.
	NetID string
	// Index is this part's place in the message, of Count parts.
	Index, Count int
	// IDs are the ids of all the message's parts, in order.
	IDs []int64
	// Temp is set on the temporary id of a message being sent.
	Temp bool
}

// Messages hands out message ids for the run: ms << 8 | slot, from the
// network's timestamp, so they're chronological; the next free slot when two
// messages share a millisecond, and consecutive slots for a message's parts.
// The mapping lives for the run only.
type Messages struct {
	mu    sync.Mutex
	chats map[int64]*chatMessages
	// Now is the clock temporary ids come from (tests set it).
	Now func() time.Time
}

type chatMessages struct {
	byNet  map[string][]int64
	byID   map[int64]Part
	newest int64 // the newest millisecond given out
}

// NewMessages makes an empty mapping.
func NewMessages() *Messages {
	return &Messages{chats: map[int64]*chatMessages{}, Now: time.Now}
}

func (m *Messages) chat(chat int64) *chatMessages {
	c := m.chats[chat]
	if c == nil {
		c = &chatMessages{byNet: map[string][]int64{}, byID: map[int64]Part{}}
		m.chats[chat] = c
	}
	return c
}

// free finds n consecutive unused ids from millisecond ms on.
func (c *chatMessages) free(ms int64, n int) []int64 {
	ms = min(max(ms, 1), maxMs)
	for ; ; ms++ {
		for slot := 0; slot+n-1 <= MaxSlot; slot++ {
			ok := true
			for k := range n {
				if _, used := c.byID[MessageID(ms, slot+k)]; used {
					ok = false
					slot += k // the next run can't start before the used one
					break
				}
			}
			if ok {
				ids := make([]int64, n)
				for k := range n {
					ids[k] = MessageID(ms, slot+k)
				}
				c.newest = max(c.newest, ms)
				return ids
			}
		}
	}
}

// Assign gives the network's message netID, sent at millisecond ms, ids for
// its parts (at least one), or returns the ones it already has.
func (m *Messages) Assign(chat int64, netID string, ms int64, parts int) []int64 {
	parts = min(max(parts, 1), maxParts)
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.chat(chat)
	if got, ok := c.byNet[netID]; ok {
		return slices.Clone(got)
	}
	ids := c.free(ms, parts)
	c.byNet[netID] = ids
	for i, id := range ids {
		c.byID[id] = Part{NetID: netID, Index: i, Count: parts, IDs: ids}
	}
	return slices.Clone(ids)
}

// Lookup is what message id in chat stands for.
func (m *Messages) Lookup(chat, id int64) (Part, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.chats[chat]
	if c == nil {
		return Part{}, false
	}
	p, ok := c.byID[id]
	if !ok || p.Count == 0 {
		return Part{}, false
	}
	p.IDs = slices.Clone(p.IDs)
	return p, true
}

// Known is the ids the network's message netID has, if it has any.
func (m *Messages) Known(chat int64, netID string) ([]int64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.chats[chat]
	if c == nil {
		return nil, false
	}
	ids, ok := c.byNet[netID]
	return slices.Clone(ids), ok
}

// Temp gives n consecutive temporary ids for messages being sent, after
// every id the chat has so far, so they sort last even when the clock here
// is behind the network's.
func (m *Messages) Temp(chat int64, n int) []int64 {
	n = min(max(n, 1), maxParts)
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.chat(chat)
	ids := c.free(max(m.Now().UnixMilli(), c.newest), n)
	for i, id := range ids {
		c.byID[id] = Part{Index: i, Count: n, IDs: ids, Temp: true}
	}
	return slices.Clone(ids)
}

// Retire marks a temporary id as done with once its message was sent or
// failed: it names nothing any more, and is never given out again.
func (m *Messages) Retire(chat, id int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.chats[chat]; c != nil {
		if p, ok := c.byID[id]; ok && p.Temp {
			c.byID[id] = Part{}
		}
	}
}

// ForgetChat drops a chat's mapping (on logout).
func (m *Messages) ForgetChat(chat int64) {
	m.mu.Lock()
	delete(m.chats, chat)
	m.mu.Unlock()
}
