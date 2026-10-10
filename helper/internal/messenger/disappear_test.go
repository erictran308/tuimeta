// SPDX-License-Identifier: AGPL-3.0-or-later

package messenger

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waMsgApplication"
	"go.mau.fi/whatsmeow/types/events"
	gproto "google.golang.org/protobuf/proto"

	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// fakeDownloads records which downloads were deleted.
type fakeDownloads struct {
	mu      sync.Mutex
	removed []string
}

func (f *fakeDownloads) Remove(ref ids.FileRef) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, ref.Key)
}

func (f *fakeDownloads) Forget(proto.Network) error { return nil }

func (f *fakeDownloads) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.removed)
}

// downloads gives the backend a record of deleted downloads.
func (h *harness) downloads() *fakeDownloads {
	f := &fakeDownloads{}
	h.m.mu.Lock()
	h.m.d.Downloads = f
	h.m.mu.Unlock()
	return f
}

// at sets the backend's clock to base plus d.
func (h *harness) at(d time.Duration) {
	h.m.mu.Lock()
	h.m.now = func() time.Time { return base.Add(d) }
	h.m.mu.Unlock()
}

// timer is a message's disappearing-messages setting.
func timer(secs uint32, kind waMsgApplication.MessageApplication_EphemeralSetting_EphemeralityType, at int64) *waMsgApplication.MessageApplication_Metadata {
	s := &waMsgApplication.MessageApplication_EphemeralSetting{EphemeralExpiration: gproto.Uint32(secs), EphemeralityType: kind.Enum()}
	if at > 0 {
		s.EphemeralSettingTimestamp = gproto.Int64(at)
	}
	return &waMsgApplication.MessageApplication_Metadata{Ephemeral: &waMsgApplication.MessageApplication_Metadata_ChatEphemeralSetting{ChatEphemeralSetting: s}}
}

// timerSet is the message that turns a chat's disappearing messages on (or
// off, with 0).
func timerSet(secs uint32, id string, min int) *events.FBMessage {
	evt := waMsg(jid(aliceID), jid(aliceID), id, min, waText("x"), nil)
	evt.Message = nil
	evt.FBApplication = &waMsgApplication.MessageApplication{Metadata: timer(secs, waMsgApplication.MessageApplication_EphemeralSetting_UNKNOWN, 0)}
	return evt
}

const (
	sendBased = waMsgApplication.MessageApplication_EphemeralSetting_SEND_BASED_WITH_TIMER
	seenBased = waMsgApplication.MessageApplication_EphemeralSetting_SEEN_BASED_WITH_TIMER
	unset     = waMsgApplication.MessageApplication_EphemeralSetting_UNKNOWN
)

func TestDisappearingMessagesGoWhenTheirSenderSaid(t *testing.T) {
	h := newHarness(t)
	h.openKept()
	h.load()
	dl := h.downloads()
	photo := waMsg(jid(aliceID), jid(aliceID), "D1", -2, waPhoto(t, []byte("sha"), []byte("key"), "/v/d1", nil), timer(3600, sendBased, 0))
	h.m.receiveWA(photo)
	h.m.receiveWA(waMsg(jid(aliceID), jid(aliceID), "D2", -1, waText("for a minute once read"), timer(60, seenBased, 0)))
	h.m.receiveWA(waMsg(jid(aliceID), jid(aliceID), "K1", 0, waText("stays"), nil))
	msgs := h.rec.messages()
	if len(msgs) != 3 {
		t.Fatalf("messages %+v", msgs)
	}
	ref, _ := h.deps.Files.Get(msgs[0].Media.FileID)
	h.rec.reset()

	// An hour on, the photo has gone, with its download; the one counted
	// from when it's seen hasn't been seen.
	h.at(61 * time.Minute)
	h.m.sweep(h.kept())
	del := h.rec.of("message_deleted")
	if len(del) != 1 || !slices.Equal(del[0].(proto.MessageDeletedEvent).MessageIDs, []int64{msgs[0].ID}) {
		t.Fatalf("deleted %+v", del)
	}
	if !slices.Contains(dl.keys(), ref.Key) {
		t.Errorf("the photo's download wasn't deleted: %v", dl.keys())
	}
	// Read now, it goes a minute later.
	c := h.chat(aliceID)
	if err := h.m.MarkRead(context.Background(), h.ref(c, msgs[2].ID)); err != nil {
		t.Fatal(err)
	}
	h.rec.reset()
	h.at(61*time.Minute + 59*time.Second)
	h.m.sweep(h.kept())
	if len(h.rec.of("message_deleted")) != 0 {
		t.Error("it went before its minute was up")
	}
	h.at(62*time.Minute + 1*time.Second)
	h.m.sweep(h.kept())
	if del := h.rec.of("message_deleted"); len(del) != 1 || del[0].(proto.MessageDeletedEvent).MessageIDs[0] != msgs[1].ID {
		t.Errorf("deleted %+v", del)
	}
	if rows := h.rows(); len(rows) != 1 || rows[0].ID != "K1" {
		t.Errorf("kept %+v", rows)
	}
}

func TestDisappearedMessagesDontComeBackAtTheNextStart(t *testing.T) {
	h := newHarness(t)
	gen := h.openKept()
	h.load()
	h.m.receiveWA(waMsg(jid(aliceID), jid(aliceID), "D1", -3, waPhoto(t, []byte("sha"), []byte("key"), "/v/d1", nil), timer(60*60, sendBased, 0)))
	h.m.receiveWA(waMsg(jid(aliceID), jid(aliceID), "D2", -2, waText("unset is from sending"), timer(30*60, unset, 0)))
	h.m.receiveWA(waMsg(jid(aliceID), jid(aliceID), "D3", -1, waText("read here"), timer(60, seenBased, 0)))
	h.m.receiveWA(waMsg(jid(aliceID), jid(aliceID), "K1", 0, waText("stays"), nil))
	c := h.chat(aliceID)
	if err := h.m.MarkRead(context.Background(), h.ref(c, h.rec.messages()[3].ID)); err != nil {
		t.Fatal(err)
	}
	ref, _ := h.deps.Files.Get(h.rec.messages()[0].Media.FileID)
	// A day later.
	h.at(24 * time.Hour)
	dl := h.downloads()
	h.restart(gen)
	all := h.chat(aliceID).log.All()
	if len(all) != 1 || all[0].Text != "stays" {
		t.Errorf("read back %+v", all)
	}
	if len(h.rec.messages()) != 0 || len(h.rec.of("message_deleted")) != 0 {
		t.Error("reading back reported something")
	}
	if rows := h.rows(); len(rows) != 1 || rows[0].ID != "K1" {
		t.Errorf("kept %+v", rows)
	}
	if !slices.Contains(dl.keys(), ref.Key) {
		t.Errorf("the photo's download wasn't deleted: %v", dl.keys())
	}
}

func TestAMessageThatHasAlreadyDisappearedIsntShown(t *testing.T) {
	h := newHarness(t)
	h.openKept()
	h.load()
	h.m.receiveWA(waMsg(jid(aliceID), jid(aliceID), "D1", -10, waText("late"), timer(60, sendBased, 0)))
	if len(h.rec.messages()) != 0 || len(h.rows()) != 0 {
		t.Error("a message past its time was shown or kept")
	}
}

// sentTimer sends text in Alice's chat and is the disappearing-messages
// setting it went with.
func (h *harness) sentTimer(text string) *waMsgApplication.MessageApplication_EphemeralSetting {
	h.t.Helper()
	before := len(h.e2ee.sent)
	if err := h.m.Send(context.Background(), h.outgoing(h.chat(aliceID), text, nil, nil)); err != nil {
		h.t.Fatal(err)
	}
	if len(h.e2ee.sent) != before+1 {
		h.t.Fatalf("sent %+v", h.e2ee.sent)
	}
	return h.e2ee.sent[before].meta.GetChatEphemeralSetting()
}

func TestWhatYouSendCarriesTheChatsTimer(t *testing.T) {
	h := newHarness(t)
	gen := h.openKept()
	h.load()
	h.m.receiveWA(waMsg(jid(aliceID), jid(aliceID), "A1", -10, waText("hi"), nil))
	h.m.receiveWA(timerSet(86400, "S1", -5))
	if s := h.sentTimer("one"); s.GetEphemeralExpiration() != 86400 || s.GetEphemeralityType() != unset {
		t.Errorf("sent with %+v", s)
	}
	// What you sent goes when the timer says, as the others' does.
	var expires int64
	for _, r := range h.rows() {
		if r.FromMe && r.Kind == rowMessage {
			expires = r.Expires
		}
	}
	if want := base.Add(time.Minute).Add(24 * time.Hour).UnixMilli(); expires != want {
		t.Errorf("yours disappears at %d, want %d", expires, want)
	}
	// After a restart the chat still has its timer.
	h.restart(gen)
	if s := h.sentTimer("two"); s.GetEphemeralExpiration() != 86400 {
		t.Errorf("after a restart, sent with %+v", s)
	}
	// A newer setting on someone's message changes it; turned off, what you
	// send carries none.
	h.m.receiveWA(waMsg(jid(aliceID), jid(aliceID), "A2", -1, waText("shorter"), timer(3600, unset, base.Unix())))
	if s := h.sentTimer("three"); s.GetEphemeralExpiration() != 3600 {
		t.Errorf("sent with %+v", s)
	}
	h.m.receiveWA(timerSet(0, "S2", 0))
	if s := h.sentTimer("four"); s != nil {
		t.Errorf("sent with %+v after it was turned off", s)
	}
}

func TestAnOlderTimerSettingArrivingLateChangesNothing(t *testing.T) {
	h := newHarness(t)
	gen := h.openKept()
	h.load()
	h.m.receiveWA(waMsg(jid(aliceID), jid(aliceID), "A1", -10, waText("hi"), nil))
	h.m.receiveWA(timerSet(3600, "S2", -5))
	h.m.receiveWA(timerSet(86400, "S1", -8)) // set before, delivered after
	h.m.mu.Lock()
	timer := h.m.lookupChat(aliceID).timer
	h.m.mu.Unlock()
	if timer != 3600 {
		t.Errorf("the chat's timer became %d", timer)
	}
	h.restart(gen)
	if s := h.sentTimer("after"); s.GetEphemeralExpiration() != 3600 {
		t.Errorf("after a restart, sent with %+v", s)
	}
}
