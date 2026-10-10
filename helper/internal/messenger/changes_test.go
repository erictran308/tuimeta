// SPDX-License-Identifier: AGPL-3.0-or-later

package messenger

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waConsumerApplication"
	waTypes "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	gproto "google.golang.org/protobuf/proto"

	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// waEditOf is an edit of the message with id, at (ms) by the editor's clock.
func waEditOf(chat, editor waTypes.JID, id string, min int, target string, fromMe bool, participant, text string, at int64) *events.FBMessage {
	return waMsg(chat, editor, id, min, &waConsumerApplication.ConsumerApplication_Content{Content: &waConsumerApplication.ConsumerApplication_Content_EditMessage{
		EditMessage: &waConsumerApplication.ConsumerApplication_EditMessage{
			Key: key(chat, target, fromMe, participant), Message: &waCommon.MessageText{Text: gproto.String(text)}, TimestampMS: gproto.Int64(at),
		},
	}}, nil)
}

// kinds counts the kept rows by kind.
func (h *harness) kinds() map[string]int {
	out := map[string]int{}
	for _, r := range h.rows() {
		out[r.Kind]++
	}
	return out
}

func TestEditsAndReactionsThatChangeNothingDontPushMessagesOut(t *testing.T) {
	h := newHarness(t)
	gen := h.openKept()
	h.load()
	alice := jid(aliceID).String()
	h.m.receiveWA(waMsg(waGroup, jid(aliceID), "M1", -30, waText("first"), nil))
	h.m.receiveWA(waMsg(waGroup, jid(aliceID), "M2", -29, waText("second"), nil))
	h.m.receiveWA(waMsg(waGroup, jid(benID), "M3", -28, waText("ben's"), nil))
	h.rec.reset()
	for i := range MaxStoredPerChat {
		id := "N" + strconv.Itoa(i)
		switch i % 3 {
		case 0: // an edit of a message that doesn't exist
			h.m.receiveWA(waEditOf(waGroup, jid(benID), id, -20, "nothing-"+id, true, "", "x", int64(i+1)))
		case 1: // an edit of someone else's message
			h.m.receiveWA(waEditOf(waGroup, jid(benID), id, -20, "M1", false, alice, "hijacked", int64(i+1)))
		case 2: // the reaction Ben already has, again
			h.m.receiveWA(waMsg(waGroup, jid(benID), id, -20, waReaction(waGroup, "M1", false, alice, "👍"), nil))
		}
	}
	if got := h.kinds(); got[rowMessage] != 3 || got[rowReaction] != 1 || len(got) != 2 {
		t.Errorf("kept %v", got)
	}
	h.restart(gen)
	all := h.chat(777).log.All()
	if len(all) != 3 || all[0].Text != "first" || len(all[0].Reactions) != 1 {
		t.Errorf("read back %+v", all)
	}
}

func TestAReactionTogglingAwayKeepsOneRowAndEveryMessage(t *testing.T) {
	h := newHarness(t)
	gen := h.openKept()
	h.load()
	alice := jid(aliceID).String()
	h.m.receiveWA(waMsg(waGroup, jid(aliceID), "M1", -30, waText("first"), nil))
	h.m.receiveWA(waMsg(waGroup, jid(aliceID), "M2", -29, waText("second"), nil))
	for i := range MaxStoredPerChat + 1 {
		emoji := []string{"👍", "❤"}[i%2]
		h.m.receiveWA(waMsg(waGroup, jid(benID), "R"+strconv.Itoa(i), -20, waReaction(waGroup, "M1", false, alice, emoji), nil))
	}
	if got := h.kinds(); got[rowMessage] != 2 || got[rowReaction] != 1 {
		t.Errorf("kept %v", got)
	}
	h.restart(gen)
	all := h.chat(777).log.All()
	if len(all) != 2 || len(all[0].Reactions) != 1 || all[0].Reactions[0].Emoji != "👍" {
		t.Errorf("read back %+v", all)
	}
}

func TestAnUnsentMessageLeavesNothingKept(t *testing.T) {
	h := newHarness(t)
	h.openKept()
	h.load()
	alice := jid(aliceID).String()
	h.m.receiveWA(waMsg(waGroup, jid(aliceID), "M1", -5, waText("oops, wrong chat"), nil))
	h.m.receiveWA(waMsg(waGroup, jid(aliceID), "M2", -4, waText("stays"), nil))
	h.m.receiveWA(waEditOf(waGroup, jid(aliceID), "E1", -3, "M1", true, "", "oops, still wrong", 1))
	h.m.receiveWA(waMsg(waGroup, jid(benID), "R1", -2, waReaction(waGroup, "M1", false, alice, "😮"), nil))
	if got := h.kinds(); got[rowMessage] != 2 || got[rowEdit] != 1 || got[rowReaction] != 1 {
		t.Fatalf("kept %v", got)
	}
	revoke := waMsg(waGroup, jid(aliceID), "X1", -1, nil, nil)
	revoke.Message = &waConsumerApplication.ConsumerApplication{Payload: &waConsumerApplication.ConsumerApplication_Payload{
		Payload: &waConsumerApplication.ConsumerApplication_Payload_ApplicationData{ApplicationData: &waConsumerApplication.ConsumerApplication_ApplicationData{
			ApplicationContent: &waConsumerApplication.ConsumerApplication_ApplicationData_Revoke{Revoke: &waConsumerApplication.ConsumerApplication_RevokeMessage{Key: key(waGroup, "M1", true, "")}},
		}},
	}}
	h.m.receiveWA(revoke)
	rows := h.rows()
	if len(rows) != 1 || rows[0].ID != "M2" {
		t.Errorf("after the unsend, kept %+v", rows)
	}
}

func TestAnUnsentMessagesTextIsntLeftInTheStoresFiles(t *testing.T) {
	h := newHarness(t)
	h.openKept()
	h.load()
	h.m.receiveWA(waMsg(jid(aliceID), jid(aliceID), "M1", -5, waText("the-unsent-secret"), nil))
	revoke := waMsg(jid(aliceID), jid(aliceID), "X1", -1, nil, nil)
	revoke.Message = &waConsumerApplication.ConsumerApplication{Payload: &waConsumerApplication.ConsumerApplication_Payload{
		Payload: &waConsumerApplication.ConsumerApplication_Payload_ApplicationData{ApplicationData: &waConsumerApplication.ConsumerApplication_ApplicationData{
			ApplicationContent: &waConsumerApplication.ConsumerApplication_ApplicationData_Revoke{Revoke: &waConsumerApplication.ConsumerApplication_RevokeMessage{Key: key(jid(aliceID), "M1", true, "")}},
		}},
	}}
	h.m.receiveWA(revoke)
	path, _ := h.deps.Session.Path(storeFile)
	for _, suffix := range []string{"", "-wal"} {
		data, err := os.ReadFile(path + suffix)
		if err != nil {
			continue
		}
		if bytes.Contains(data, []byte("the-unsent-secret")) {
			t.Errorf("e2ee.db%s still has the unsent text", suffix)
		}
	}
}

func TestWhatYouEditReactAndUnsendHereIsKeptTheSameWay(t *testing.T) {
	h := newHarness(t)
	h.openKept()
	h.load()
	h.m.receiveWA(waMsg(jid(aliceID), jid(aliceID), "A1", -5, waText("hi"), nil))
	c := h.chat(aliceID)
	theirs := h.rec.messages()[0]
	if err := h.m.Send(context.Background(), h.outgoing(c, "helo", nil, nil)); err != nil {
		t.Fatal(err)
	}
	sent := h.rec.of("message_sent")[0].(proto.MessageSentEvent).Message
	ctx := context.Background()
	if err := h.m.EditText(ctx, h.ref(c, sent.ID), "hello"); err != nil {
		t.Fatal(err)
	}
	if err := h.m.React(ctx, h.ref(c, theirs.ID), "👍"); err != nil {
		t.Fatal(err)
	}
	if got := h.kinds(); got[rowMessage] != 2 || got[rowEdit] != 1 || got[rowReaction] != 1 {
		t.Fatalf("kept %v", got)
	}
	if err := h.m.Delete(ctx, h.ref(c, sent.ID)); err != nil {
		t.Fatal(err)
	}
	if got := h.kinds(); got[rowMessage] != 1 || got[rowEdit] != 0 || got[rowReaction] != 1 {
		t.Errorf("after unsending, kept %v", got)
	}
}

func TestOnlyAnEmojiIsAnEncryptedReaction(t *testing.T) {
	h := newHarness(t)
	h.openKept()
	h.load()
	h.m.receiveWA(waMsg(jid(aliceID), jid(aliceID), "A1", -5, waText("did you see?"), nil))
	h.rec.reset()
	for i, text := range []string{"Seen by everyone ✓✓ 20:01", "2", "...", "ok", "👍 👍"} {
		h.m.receiveWA(waMsg(jid(aliceID), jid(aliceID), "R"+strconv.Itoa(i), -4, waReaction(jid(aliceID), "A1", true, "", text), nil))
	}
	if msgs := h.rec.messages(); len(msgs) != 0 {
		t.Errorf("shown as reactions: %+v", msgs)
	}
	if got := h.kinds(); got[rowReaction] != 0 {
		t.Errorf("kept %v", got)
	}
	for i, emoji := range []string{"👍", "❤", "‼", "1️⃣", "👍🏽"} {
		h.m.receiveWA(waMsg(jid(aliceID), jid(aliceID), "E"+strconv.Itoa(i), -3, waReaction(jid(aliceID), "A1", true, "", emoji), nil))
	}
	got := h.rec.messages()
	if len(got) != 5 || got[4].Reactions[0].Emoji != "👍🏽" {
		t.Errorf("emoji reactions %+v", got)
	}
}

func TestPruningTakesTheEditsAndReactionsOfTheMessagesItDrops(t *testing.T) {
	ctx := context.Background()
	s, err := openStore(ctx, filepath.Join(t.TempDir(), storeFile))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	put := func(m storedMessage) {
		t.Helper()
		if err := s.put(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	ids := func() []string {
		t.Helper()
		all, err := s.all(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, m := range all {
			if m.Kind != rowMessage {
				out = append(out, m.Chat+" "+m.ID)
			}
		}
		slices.Sort(out)
		return out
	}
	newest := "m" + strconv.Itoa(MaxStoredPerChat+1)
	for _, chat := range []string{"1@msgr", "2@msgr"} {
		for i := range MaxStoredPerChat + 2 {
			put(storedMessage{Chat: chat, Sender: "9@msgr", ID: "m" + strconv.Itoa(i), TS: time.UnixMilli(int64(i + 1)), App: []byte{1}})
		}
		// The oldest message (past the cap) and the newest each have a
		// reaction; the newest an edit, and the chat a setting.
		at := time.UnixMilli(1 << 40)
		put(storedMessage{Chat: chat, Sender: "8@msgr", ID: "r0", TS: at, App: []byte{2}, Kind: rowReaction, TargetSender: "9@msgr", TargetID: "m0"})
		put(storedMessage{Chat: chat, Sender: "8@msgr", ID: "r1", TS: at, App: []byte{2}, Kind: rowReaction, TargetSender: "9@msgr", TargetID: newest})
		put(storedMessage{Chat: chat, Sender: "9@msgr", ID: "e1", TS: at, App: []byte{2}, Kind: rowEdit, TargetSender: "9@msgr", TargetID: newest})
		put(storedMessage{Chat: chat, Sender: "9@msgr", ID: "s1", TS: at, App: []byte{2}, Kind: rowSetting})
	}
	if err := s.pruneChat(ctx, "1@msgr"); err != nil {
		t.Fatal(err)
	}
	if got, want := ids(), []string{"1@msgr e1", "1@msgr r1", "1@msgr s1", "2@msgr e1", "2@msgr r0", "2@msgr r1", "2@msgr s1"}; !slices.Equal(got, want) {
		t.Errorf("after pruning one chat: %v", got)
	}
	if err := s.prune(ctx); err != nil {
		t.Fatal(err)
	}
	if got, want := ids(), []string{"1@msgr e1", "1@msgr r1", "1@msgr s1", "2@msgr e1", "2@msgr r1", "2@msgr s1"}; !slices.Equal(got, want) {
		t.Errorf("after pruning all: %v", got)
	}
	// A disappearing message takes its edit and reaction with it.
	if err := s.setExpires(ctx, "1@msgr", "9@msgr", newest, 5); err != nil {
		t.Fatal(err)
	}
	if err := s.deleteExpired(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if got, want := ids(), []string{"1@msgr s1", "2@msgr e1", "2@msgr r1", "2@msgr s1"}; !slices.Equal(got, want) {
		t.Errorf("after it disappeared: %v", got)
	}
}

func TestAnOlderStoresEditsAndReactionsAreSortedOutOnce(t *testing.T) {
	h := newHarness(t)
	path, err := h.deps.Session.Path(storeFile)
	if err != nil {
		t.Fatal(err)
	}
	// A store as the previous build made it: every row a message.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE tuimeta_e2ee_message (
		chat TEXT NOT NULL, sender TEXT NOT NULL, id TEXT NOT NULL, ts INTEGER NOT NULL, from_me INTEGER NOT NULL, app BLOB NOT NULL,
		PRIMARY KEY (chat, sender, id))`); err != nil {
		t.Fatal(err)
	}
	alice := jid(aliceID).String()
	for _, evt := range []*events.FBMessage{
		waMsg(waGroup, jid(aliceID), "M1", -10, waText("helo"), nil),
		waEditOf(waGroup, jid(aliceID), "E1", -9, "M1", true, "", "hello", 1),
		waEditOf(waGroup, jid(benID), "E2", -8, "M1", false, alice, "hijacked", 2),
		waMsg(waGroup, jid(benID), "R1", -7, waReaction(waGroup, "M1", false, alice, "👍"), nil),
		waMsg(waGroup, jid(benID), "R2", -6, waReaction(waGroup, "M1", false, alice, "Seen ✓✓"), nil),
	} {
		app, _ := gproto.Marshal(evt.FBApplication)
		if _, err := db.Exec(`INSERT INTO tuimeta_e2ee_message VALUES (?, ?, ?, ?, 0, ?)`,
			evt.Info.Chat.String(), evt.Info.Sender.String(), evt.Info.ID, evt.Info.Timestamp.UnixMilli(), app); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	h.openKept()
	all := h.chat(777).log.All()
	if len(all) != 1 || all[0].Text != "hello" || !all[0].Edited || len(all[0].Reactions) != 1 {
		t.Fatalf("read back %+v", all)
	}
	var got []string
	for _, r := range h.rows() {
		got = append(got, r.ID+":"+r.Kind+":"+r.TargetID)
	}
	if want := []string{"M1::", "E1:edit:M1", "R1:reaction:M1"}; !slices.Equal(got, want) {
		t.Errorf("rows %v, want %v", got, want)
	}
}
