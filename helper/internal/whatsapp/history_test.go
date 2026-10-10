// SPDX-License-Identifier: AGPL-3.0-or-later

package whatsapp

import (
	"context"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	waTypes "go.mau.fi/whatsmeow/types"
	gproto "google.golang.org/protobuf/proto"

	"github.com/erictran308/tuimeta/helper/internal/history"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// reactionBy is a reaction as the phone's history lists it under a message:
// by participant (fromMe for yours).
func reactionBy(participant string, fromMe bool, emoji string) *waWeb.Reaction {
	key := &waCommon.MessageKey{FromMe: gproto.Bool(fromMe), RemoteJID: gproto.String(groupJID.String())}
	if participant != "" {
		key.Participant = gproto.String(participant)
	}
	return &waWeb.Reaction{Key: key, Text: gproto.String(emoji)}
}

// withReactions puts reactions on a history message.
func withReactions(hm *waHistorySync.HistorySyncMsg, rs ...*waWeb.Reaction) *waHistorySync.HistorySyncMsg {
	hm.Message.Reactions = rs
	return hm
}

// webOf is a history message carrying m.
func webOf(id string, fromMe bool, participant string, min int, m *waE2E.Message) *waHistorySync.HistorySyncMsg {
	hm := webMsg(id, fromMe, participant, min, "")
	hm.Message.Message = m
	return hm
}

// kept is the message with id in the chat with key, as kept; nil if none.
func (h *harness) kept(key, id string) *message {
	h.w.mu.Lock()
	defer h.w.mu.Unlock()
	c := h.w.chats[h.w.resolve(key)]
	if c == nil {
		return nil
	}
	h.w.ensureLoaded(c)
	return c.msgs[id]
}

func TestHistoryReactionsAreEmojiFromPeopleAsLiveOnesAre(t *testing.T) {
	h := newHarness(t)
	h.load()
	other := waTypes.NewJID("120363000000000009", waTypes.GroupServer)
	g1 := withReactions(webMsg("G1", false, benLID.String(), -10, "see you at noon"),
		reactionBy(benLID.String(), false, "✓✓ Seen 12:04"),
		reactionBy(aliceLID.String(), false, "👍"),
		reactionBy(carolLID.String(), false, strings.Repeat("A", 40000)),
		reactionBy(other.String(), false, "😂"),
	)
	h.event(syncOf(waHistorySync.HistorySync_INITIAL_BOOTSTRAP, &waHistorySync.Conversation{
		ID: gproto.String(groupJID.String()), Messages: []*waHistorySync.HistorySyncMsg{g1},
	}))
	// A timer change in a dm takes no reaction either.
	timerMsg := withReactions(webOf("T1", false, "", -9, &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_EPHEMERAL_SETTING.Enum(), EphemeralExpiration: gproto.Uint32(86400),
	}}), reactionBy("", true, "👍"))
	h.event(syncOf(waHistorySync.HistorySync_INITIAL_BOOTSTRAP, &waHistorySync.Conversation{
		ID: gproto.String(benLID.String()), Messages: []*waHistorySync.HistorySyncMsg{timerMsg},
	}))
	m := h.kept(groupJID.String(), "G1")
	if m == nil {
		t.Fatal("the message wasn't kept")
	}
	if len(m.Reactions) != 1 || m.Reactions[0].Emoji != "👍" {
		var got []string
		for _, r := range m.Reactions {
			got = append(got, proto.Snippet(r.Emoji, 20))
		}
		t.Fatalf("reactions kept %q", got)
	}
	if ev := h.kept(benLID.String(), "T1"); ev == nil || ev.Service == nil || len(ev.Reactions) != 0 {
		t.Errorf("the timer change %+v", ev)
	}
	h.w.mu.Lock()
	_, made := h.w.people[other.String()]
	h.w.mu.Unlock()
	if made {
		t.Error("a group's id reacting became a person")
	}
}

func TestHistoryEventsNameOnlyPeople(t *testing.T) {
	h := newHarness(t)
	h.load()
	other := waTypes.NewJID("120363000000000009", waTypes.GroupServer)
	add := &waHistorySync.HistorySyncMsg{Message: &waWeb.WebMessageInfo{
		Key:                   &waCommon.MessageKey{ID: gproto.String("S1"), Participant: gproto.String(waTypes.StatusBroadcastJID.String())},
		MessageTimestamp:      gproto.Uint64(uint64(at(-5).Unix())),
		MessageStubType:       waWeb.WebMessageInfo_GROUP_PARTICIPANT_ADD.Enum(),
		MessageStubParameters: []string{other.String(), aliceLID.String()},
	}}
	h.event(syncOf(waHistorySync.HistorySync_INITIAL_BOOTSTRAP, &waHistorySync.Conversation{
		ID: gproto.String(groupJID.String()), Messages: []*waHistorySync.HistorySyncMsg{add},
	}))
	m := h.kept(groupJID.String(), "S1")
	if m == nil || m.Service.Actor != "" || len(m.Service.Targets) != 1 || m.Service.Targets[0] != aliceLID.String() {
		t.Fatalf("event %+v", m)
	}
	h.w.mu.Lock()
	defer h.w.mu.Unlock()
	for _, key := range []string{other.String(), waTypes.StatusBroadcastJID.String()} {
		if _, made := h.w.people[key]; made {
			t.Errorf("%s became a person", key)
		}
	}
}

func TestAnEncryptedReactionInTheHistoryIsntAnUnsupportedMessage(t *testing.T) {
	if ch := readMsg(&waE2E.Message{EncReactionMessage: &waE2E.EncReactionMessage{}}); ch.kind != ignored {
		t.Errorf("read as %+v", ch)
	}
}

// historyOf is the chat with key as tuimeta's history request gets it.
func (h *harness) historyOf(key string) []string {
	h.t.Helper()
	c := h.chat(key)
	if c == nil {
		return nil
	}
	page, err := h.w.History(context.Background(), refOf(c), history.Query{Limit: 50})
	if err != nil {
		h.t.Fatal(err)
	}
	var out []string
	for _, m := range page.Messages {
		out = append(out, m.Text+m.Service)
	}
	return out
}
