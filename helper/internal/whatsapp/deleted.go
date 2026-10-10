// SPDX-License-Identifier: AGPL-3.0-or-later

package whatsapp

import (
	"github.com/erictran308/tuimeta/helper/internal/hlog"
)

// A message deleted here (for everyone, on your phone, or its time up)
// leaves a tombstone, so that no later copy of it brings it back: a
// decryption that comes late, the phone sending it again, or the phone's
// history, built when this device was linked and sent in blobs minutes
// apart. A deletion of a message this device doesn't have yet leaves one
// too, for when it comes.

// deletions is what was deleted in one chat, by message id.
type deletions struct {
	byID map[string][]tombstone
	n    int
}

// deletionsIn is what was deleted in the chat with key, read from the store
// the first time; w.mu is held. It's nil when the store can't be read, and
// what's being handled then isn't acknowledged, so it comes again.
func (w *WhatsApp) deletionsIn(key string) *deletions {
	if d := w.deletions[key]; d != nil {
		return d
	}
	d := &deletions{byID: map[string][]tombstone{}}
	if w.st != nil {
		ctx, cancel := dbCtx()
		list, err := w.st.deletedIn(ctx, key)
		cancel()
		if err != nil {
			w.storeFailed = true
			hlog.Error("whatsapp: can't read deleted messages", hlog.Kind(err))
			return nil
		}
		for _, t := range list {
			d.add(t)
		}
	}
	w.deletions[key] = d
	return d
}

// add records t, a sender's tombstone made sure if it now is.
func (d *deletions) add(t tombstone) {
	for i, old := range d.byID[t.id] {
		if old.sender == t.sender {
			d.byID[t.id][i].sure = old.sure || t.sure
			return
		}
	}
	d.byID[t.id] = append(d.byID[t.id], t)
	d.n++
}

// wasDeleted reports whether a message with id from sender, in the chat
// with key, was deleted here before; w.mu is held.
func (w *WhatsApp) wasDeleted(key, id, sender string) bool {
	d := w.deletionsIn(key)
	if d == nil {
		return true // can't tell now: it comes again
	}
	for _, t := range d.byID[id] {
		if t.bars(id, sender, w.same) {
			return true
		}
	}
	return false
}

// noteDeleted keeps t, for the chat with key; w.mu is held. stored says
// the store has it already.
func (w *WhatsApp) noteDeleted(key string, t tombstone, stored bool) {
	t.sender = w.resolve(t.sender)
	if !stored && w.st != nil {
		ctx, cancel := dbCtx()
		if err := w.st.putDeleted(ctx, key, t); err != nil {
			w.storeFailed = true
			hlog.Error("whatsapp: can't keep a deletion", hlog.Kind(err))
		}
		cancel()
	}
	if d := w.deletionsIn(key); d != nil {
		d.add(t)
		if d.n > MaxDeletedPerChat {
			// The store keeps the ones that matter most: read them back.
			delete(w.deletions, key)
		}
	}
}

// heldEdit is an edit of a message this device doesn't have yet, waiting
// for it: from the phone's history, or decrypted at last.
type heldEdit struct {
	sender string
	edit   *message
}

// MaxHeldEdits bounds the messages whose edits wait for them.
const MaxHeldEdits = 1000

func editKey(chat, id string) string { return chat + "\x00" + id }

// holdEdit keeps an edit of a message not here (yet), its sender's newest
// only; w.mu is held.
func (w *WhatsApp) holdEdit(chat, id, sender string, edit *message) {
	k := editKey(chat, id)
	held, ok := w.edits[k]
	if !ok {
		w.editOrder = append(w.editOrder, k)
	}
	for i, h := range held {
		if w.same(h.sender, sender) {
			if edit.MS > h.edit.MS {
				held[i].edit = edit
			}
			return
		}
	}
	w.edits[k] = append(held, heldEdit{sender: sender, edit: edit})
	for len(w.editOrder) > MaxHeldEdits {
		delete(w.edits, w.editOrder[0])
		w.editOrder = w.editOrder[1:]
	}
}

// settle applies to msg, as it's kept, the edits that came before it, by
// the rules live ones follow; w.mu is held.
func (w *WhatsApp) settle(c *chat, msg *message) {
	if msg.Placeholder {
		return // they wait for its decryption
	}
	k := editKey(c.key, msg.ID)
	held := w.edits[k]
	if len(held) == 0 {
		return
	}
	delete(w.edits, k)
	for _, h := range held {
		w.edit(msg, h.sender, h.edit)
	}
}

// editSlack is how much later than WhatsApp's window an edit may say it was
// made, in seconds: for clocks that disagree a little.
const editSlack = 10 * 60

// edit applies edit, by sender, to msg if it may be: only its sender edits
// a message, never an event, within WhatsApp's window, and an older edit
// never undoes a newer one. Only the text, its mentions and its link change,
// and the message says it was edited. w.mu is held.
func (w *WhatsApp) edit(msg *message, sender string, edit *message) bool {
	switch {
	case msg.Service != nil, msg.Placeholder, !w.same(msg.Sender, sender):
		return false
	case edit.MS <= msg.EditMS, edit.MS > msg.MS+(EditWindow+editSlack)*1000:
		return false
	}
	old := msg.Preview
	msg.Text, msg.Mentions, msg.Preview = edit.Text, edit.Mentions, edit.Preview
	msg.Edited, msg.EditMS = true, edit.MS
	if old != nil {
		// The card it had goes, picture and all (unless it's the same).
		w.forgetFiles(&message{Preview: old})
	}
	return true
}
