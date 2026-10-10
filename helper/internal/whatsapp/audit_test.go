// SPDX-License-Identifier: AGPL-3.0-or-later

package whatsapp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	waTypes "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	gproto "google.golang.org/protobuf/proto"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// downloads records which downloaded files the backend deletes.
type downloads struct {
	mu      sync.Mutex
	removed []string
}

func (d *downloads) Remove(ref ids.FileRef) {
	d.mu.Lock()
	d.removed = append(d.removed, ref.Key)
	d.mu.Unlock()
}

func (d *downloads) Forget(proto.Network) error { return nil }

func TestNothingASenderNamesCanBecomeAChatYouPostTo(t *testing.T) {
	h := newHarness(t)
	h.load()
	evt := &events.Message{Message: &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
		Text: gproto.String("see @0 and @120363000000000009"),
		ContextInfo: &waE2E.ContextInfo{
			MentionedJID: []string{waTypes.StatusBroadcastJID.String(), "120363000000000009@newsletter"},
			StanzaID:     gproto.String("Q"), Participant: gproto.String(waTypes.StatusBroadcastJID.String()),
		},
	}}}
	evt.Info.Chat, evt.Info.Sender, evt.Info.ID, evt.Info.Timestamp = benLID, benLID, "M1", at(-1)
	h.event(evt)
	for _, u := range h.rec.users() {
		if u.Name == "WhatsApp user" {
			t.Errorf("a status or channel became a person: %+v", u)
		}
	}
	h.w.mu.Lock()
	_, made := h.w.people[waTypes.StatusBroadcastJID.String()]
	h.w.mu.Unlock()
	if made {
		t.Error("the status broadcast is a person")
	}
	for _, target := range []waTypes.JID{waTypes.StatusBroadcastJID, waTypes.NewJID("1", waTypes.NewsletterServer), waTypes.NewJID("1", waTypes.BotServer)} {
		if _, err := h.w.OpenDM(context.Background(), backend.UserRef{NetID: target.String()}); err == nil {
			t.Errorf("a chat with %s opened", target.Server)
		}
	}
	// A chat that somehow has such a key is never sent to.
	h.w.mu.Lock()
	c := h.w.chatFor(waTypes.StatusBroadcastJID.String(), false)
	h.w.mu.Unlock()
	if err := h.w.Send(context.Background(), h.outgoing(c, "hello", nil)); err == nil || len(h.wa.sentMessages()) != 0 {
		t.Errorf("sent to the status broadcast: %v", err)
	}
}

func TestNothingGoesToAChatThatIsntAPersonsOrAGroups(t *testing.T) {
	h := newHarness(t)
	h.load()
	bot := waTypes.NewJID("867051314767696", waTypes.BotServer)
	h.event(text(bot, bot, "M1", -5, "how can I help?"))
	c := h.chat(bot.String())
	if c == nil {
		t.Fatal("no chat")
	}
	chats := h.rec.chats()
	if len(chats) == 0 || chats[len(chats)-1].CanSend {
		t.Errorf("the composer opens in a bot's chat: %+v", chats)
	}
	ctx := context.Background()
	ref := h.ref(c, "M1")
	if err := h.w.React(ctx, ref, "👍"); err == nil {
		t.Error("reacted")
	}
	if err := h.w.Mute(ctx, refOf(c), true); err == nil {
		t.Error("muted")
	}
	if err := h.w.SetTyping(ctx, refOf(c), true); err == nil {
		t.Error("typed")
	}
	if err := h.w.MarkRead(ctx, ref); err != nil {
		t.Errorf("can't be read here: %v", err)
	}
	h.wa.mu.Lock()
	defer h.wa.mu.Unlock()
	if len(h.wa.sent) != 0 || len(h.wa.reads) != 0 || len(h.wa.typing) != 0 || len(h.wa.mutes) != 0 {
		t.Errorf("went out: %d sent, %d reads, %d typing, %d mutes", len(h.wa.sent), len(h.wa.reads), len(h.wa.typing), len(h.wa.mutes))
	}
	if c.ReadUpTo != at(-5).UnixMilli() {
		t.Error("not read here")
	}
}

func TestASenderCantRewriteTheirOldMessageBySendingItsIdAgain(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(text(benLID, benLID, "B1", -10, "I'll pay you back"))
	h.event(text(benLID, benLID, "B1", -1, "you owe me"))
	c := h.chat(benLID.String())
	h.w.mu.Lock()
	defer h.w.mu.Unlock()
	if m := c.msgs["B1"]; m.Text != "I'll pay you back" || m.Edited {
		t.Fatalf("message %+v", m)
	}
}

func TestOnlyYourOwnDevicesSayYouReadAChat(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(text(benLID, benLID, "B1", -5, "unread"))
	forged := &events.Receipt{MessageIDs: []waTypes.MessageID{"B1"}, Type: waTypes.ReceiptTypeReadSelf, Timestamp: at(-1)}
	forged.Chat, forged.Sender = benLID, benLID
	h.event(forged)
	if c := h.chat(benLID.String()); c.Unread != 1 {
		t.Errorf("someone else's read-self cleared your unread (%d)", c.Unread)
	}
}

func timer(chat, sender waTypes.JID, secs uint32) *events.Message {
	evt := &events.Message{Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_EPHEMERAL_SETTING.Enum(), EphemeralExpiration: gproto.Uint32(secs),
	}}}
	evt.Info.Chat, evt.Info.Sender, evt.Info.ID, evt.Info.Timestamp = chat, sender, "T"+chat.User, at(-1)
	evt.Info.IsGroup = chat.Server == waTypes.GroupServer
	return evt
}

func TestAGroupsTimerIsWhatWhatsAppSaysNotWhatAMemberSends(t *testing.T) {
	h := newHarness(t)
	h.load()
	h.event(text(groupJID, benLID, "G1", -5, "hi"))
	h.event(timer(groupJID, benLID, 1))
	if c := h.chat(groupJID.String()); c.Ephemeral != 0 {
		t.Errorf("a member set the group's timer to %d", c.Ephemeral)
	}
	h.event(timer(benLID, benLID, 5))
	if c := h.chat(benLID.String()); c != nil && c.Ephemeral != 0 {
		t.Errorf("a timer WhatsApp doesn't have: %d", c.Ephemeral)
	}
	h.event(timer(benLID, benLID, 86400))
	if c := h.chat(benLID.String()); c.Ephemeral != 86400 {
		t.Errorf("a dm's 24-hour timer: %d", c.Ephemeral)
	}
}

func TestAChatsTimerIsTheNewestSettingAndOnlyOneOfWhatsApps(t *testing.T) {
	h := newHarness(t)
	h.load()
	set := func(id string, min int, secs uint32) {
		evt := timer(benLID, benLID, secs)
		evt.Info.ID, evt.Info.Timestamp = id, at(min)
		h.event(evt)
	}
	timerOf := func(key string) uint32 {
		h.w.mu.Lock()
		defer h.w.mu.Unlock()
		return h.w.chats[h.w.resolve(key)].Ephemeral
	}
	set("T1", -10, 86400)
	set("T2", -5, 7*86400)
	set("T1", -10, 86400)    // sent again
	set("T0", -20, 90*86400) // late
	if got := timerOf(benLID.String()); got != 7*86400 {
		t.Errorf("an older setting undid a newer one: %d", got)
	}
	// The phone's state at linking, coming after, is older.
	h.event(syncOf(waHistorySync.HistorySync_INITIAL_BOOTSTRAP, &waHistorySync.Conversation{
		ID: gproto.String(benLID.String()), EphemeralExpiration: gproto.Uint32(0), EphemeralSettingTimestamp: gproto.Int64(at(-60).Unix()),
	}))
	if got := timerOf(benLID.String()); got != 7*86400 {
		t.Errorf("the sync undid a later setting: %d", got)
	}
	// Where nothing later is known, the sync's is taken, if it's WhatsApp's.
	h.event(syncOf(waHistorySync.HistorySync_INITIAL_BOOTSTRAP,
		&waHistorySync.Conversation{ID: gproto.String(aliceLID.String()), EphemeralExpiration: gproto.Uint32(86400), EphemeralSettingTimestamp: gproto.Int64(at(-60).Unix())},
		&waHistorySync.Conversation{ID: gproto.String(carolLID.String()), EphemeralExpiration: gproto.Uint32(5)},
	))
	if a, c := timerOf(aliceLID.String()), timerOf(carolLID.String()); a != 86400 || c != 0 {
		t.Errorf("timers from the sync: %d, %d", a, c)
	}
	for _, param := range []string{"5", "soon"} {
		web := &waWeb.WebMessageInfo{Key: &waCommon.MessageKey{ID: gproto.String("S")}, MessageStubType: waWeb.WebMessageInfo_CHANGE_EPHEMERAL_SETTING.Enum(), MessageStubParameters: []string{param}}
		if m := plainReader.stub(web, "S", benLID.String(), 1000); m != nil {
			t.Errorf("%q: %+v", param, m.Service)
		}
	}
	// A group's timer turned off is off.
	h.event(text(groupJID, benLID, "G1", -5, "hi"))
	c := h.chat(groupJID.String())
	h.w.mu.Lock()
	defer h.w.mu.Unlock()
	c.Ephemeral = 86400
	h.w.groupDetails(c, &waTypes.GroupInfo{JID: groupJID})
	if c.Ephemeral != 0 {
		t.Errorf("a group's timer turned off stayed %d", c.Ephemeral)
	}
}

func TestAMessageArrivingAsTheHelperQuitsComesAgainNextRun(t *testing.T) {
	h := newHarness(t)
	h.w.mu.Lock()
	gen := h.w.gen
	h.w.closed = true
	h.w.mu.Unlock()
	if h.w.onEvent(gen, text(benLID, benLID, "B1", -1, "late")) {
		t.Error("acknowledged while quitting")
	}
	h.w.mu.Lock()
	h.w.closed = false
	h.w.gen++
	h.w.mu.Unlock()
	if !h.w.onEvent(gen, text(benLID, benLID, "B2", -1, "old connection")) {
		t.Error("a logged-out connection's message keeps coming back")
	}
}

func TestNamesPeopleGaveThemselvesAreMarkedAndStrangersShowTheirNumber(t *testing.T) {
	h := newHarness(t)
	h.load()
	mallory := waTypes.NewJID("447700900666", waTypes.DefaultUserServer)
	h.wa.contacts[mallory] = waTypes.ContactInfo{Found: true, PushName: "You"}
	h.event(text(mallory, mallory, "X1", -2, "it's me"))
	c := h.chat(mallory.String())
	h.w.mu.Lock()
	title := h.w.title(c)
	h.w.mu.Unlock()
	if title != "~You · +447700900666" {
		t.Errorf("title %q", title)
	}
	h.event(text(alicePN, alicePN, "A1", -1, "hi"))
	h.w.mu.Lock()
	alice := h.w.title(h.w.chats[h.w.resolve(alicePN.String())])
	h.w.mu.Unlock()
	if alice != "Alice Example" {
		t.Errorf("a contact's name %q", alice)
	}
}

func TestAnotherSendersFileCantTakeOverAFilesEntryOrThumbnail(t *testing.T) {
	h := newHarness(t)
	h.w.mu.Lock()
	defer h.w.mu.Unlock()
	alices := &media{Kind: proto.FileMedia, Name: "contract.pdf", DirectPath: "/a", MediaKey: key32("alice-key"),
		FileSHA256: key32("same-hash"), FileEncSHA256: []byte("e"), Thumb: []byte("ALICE-THUMB")}
	mallorys := &media{Kind: proto.FileMedia, Name: "evil.exe", DirectPath: "/m", MediaKey: key32("mallory-key"),
		FileSHA256: key32("same-hash"), FileEncSHA256: []byte("e"), Thumb: []byte("MALLORY-THUMB")}
	a, m := h.w.mediaOf(alices), h.w.mediaOf(mallorys)
	if a.FileID == m.FileID || a.Thumbnail.FileID == m.Thumbnail.FileID {
		t.Fatalf("shared ids: %+v %+v", a, m)
	}
	ref, _ := h.deps.Files.Get(a.FileID)
	if ref.Name != "contract.pdf" {
		t.Errorf("alice's file is now %q", ref.Name)
	}
}

func TestOnlyMentionsInTheTextMakePeopleAndThereAreAtMostSoMany(t *testing.T) {
	h := newHarness(t)
	h.load()
	var mentions []string
	for i := range 2000 {
		mentions = append(mentions, waTypes.NewJID("44770090"+strings.Repeat("1", 4)+string(rune('0'+i%10))+string(rune('0'+i/10%10))+string(rune('0'+i/100%10)), waTypes.DefaultUserServer).String())
	}
	evt := &events.Message{Message: &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
		Text: gproto.String("hi @447700900101"), ContextInfo: &waE2E.ContextInfo{MentionedJID: append([]string{alicePN.String()}, mentions...)},
	}}}
	evt.Info.Chat, evt.Info.Sender, evt.Info.ID, evt.Info.Timestamp = benLID, benLID, "M1", at(-1)
	h.event(evt)
	if n := len(h.rec.users()); n > 3 {
		t.Errorf("%d people told", n)
	}
	c := h.chat(benLID.String())
	h.w.mu.Lock()
	kept := len(c.msgs["M1"].Mentions)
	h.w.mu.Unlock()
	if kept > MaxMentions {
		t.Errorf("kept %d mentions", kept)
	}
}

func TestReactionsAreEmojiNotText(t *testing.T) {
	for emoji, ok := range map[string]bool{
		"👍": true, "❤️": true, "👨‍👩‍👧": true, "1️⃣": true, "‼️": true, "": true,
		"12": false, "✓✓ 12:00": false, "hello": false, strings.Repeat("😂", 20): false, "\x1b[31m": false,
	} {
		if reactionLike(emoji) != ok {
			t.Errorf("%q: want %v", emoji, ok)
		}
	}
}

func TestWhatASenderSendsIsKeptWithinBounds(t *testing.T) {
	ch := readMsg(&waE2E.Message{ImageMessage: &waE2E.ImageMessage{
		Caption: gproto.String(strings.Repeat("é", MaxText)), JPEGThumbnail: make([]byte, MaxThumb+1),
	}})
	if len(ch.msg.Text) > MaxText || !strings.HasSuffix(ch.msg.Text, "é") {
		t.Errorf("text of %d bytes", len(ch.msg.Text))
	}
	if ch.msg.Media.Thumb != nil {
		t.Error("an oversized thumbnail was kept")
	}
	evt := &events.Message{Message: &waE2E.Message{VideoMessage: &waE2E.VideoMessage{Seconds: gproto.Uint32(4294967295)}}}
	evt.Info.Chat, evt.Info.Sender, evt.Info.ID, evt.Info.Timestamp = benLID, benLID, "V", at(0)
	h := newHarness(t)
	h.event(evt)
	data, err := h.rec.messages()[0].Media.MarshalJSON()
	if err != nil || !strings.Contains(string(data), `"duration":2147483647`) {
		t.Errorf("duration sent as %s", data)
	}
}

func TestNamesTitlesAndTypesASenderSendsAreKeptWithinBounds(t *testing.T) {
	long := strings.Repeat("n", MaxText)
	doc := readMsg(&waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{FileName: gproto.String(long), Mimetype: gproto.String(long)}})
	if len(doc.msg.Media.Name) > MaxField || len(doc.msg.Media.Mime) > maxMime {
		t.Errorf("a name of %d bytes, a type of %d", len(doc.msg.Media.Name), len(doc.msg.Media.Mime))
	}
	card := readMsg(&waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
		Text: gproto.String("see example.com"), MatchedText: gproto.String("example.com"), Title: gproto.String(long), Description: gproto.String(long),
	}})
	if p := card.msg.Preview; p == nil || len(p.Title) > MaxField || len(p.Description) > MaxField {
		t.Errorf("card %d %d", len(p.Title), len(p.Description))
	}
	far := readMsg(&waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
		Text: gproto.String("see"), MatchedText: gproto.String("https://example.com/" + long), Title: gproto.String("t"),
	}})
	if far.msg.Preview != nil {
		t.Error("a card for a link of 64 KiB")
	}
	web := &waWeb.WebMessageInfo{Key: &waCommon.MessageKey{ID: gproto.String("S")}, MessageStubType: waWeb.WebMessageInfo_GROUP_CHANGE_SUBJECT.Enum(), MessageStubParameters: []string{long}}
	if m := plainReader.stub(web, "S", benLID.String(), 1000); m == nil || len(m.Service.Text) > MaxField {
		t.Error("a group name of 64 KiB in an event")
	}
	if q := quoteOf(&waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{FileName: gproto.String(long)}}); len([]rune(q)) > 100 {
		t.Errorf("a quote of %d characters", len([]rune(q)))
	}
}

func TestAPhotoSeenOnlyOnceIsQuotedWithoutItsCaption(t *testing.T) {
	for name, q := range map[string]*waE2E.Message{
		"flagged": {ImageMessage: &waE2E.ImageMessage{Caption: gproto.String("secret"), ViewOnce: gproto.Bool(true)}},
		"wrapped": {ViewOnceMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: gproto.String("secret")}}}},
		"video":   {ViewOnceMessageV2: &waE2E.FutureProofMessage{Message: &waE2E.Message{VideoMessage: &waE2E.VideoMessage{Caption: gproto.String("secret")}}}},
	} {
		if got := quoteOf(q); strings.Contains(got, "secret") || (got != "Photo" && got != "Video") {
			t.Errorf("%s: %q", name, got)
		}
	}
}

func TestAStrangersNumberShowsHoweverLongTheirName(t *testing.T) {
	h := newHarness(t)
	h.load()
	stranger := waTypes.NewJID("447700900109", waTypes.DefaultUserServer)
	h.wa.contacts[stranger] = waTypes.ContactInfo{Found: true, PushName: "Mum" + strings.Repeat("m", 400)}
	h.event(text(stranger, stranger, "X1", -2, "it's me"))
	c := h.chat(stranger.String())
	h.w.mu.Lock()
	title := h.w.title(c)
	h.w.mu.Unlock()
	if !strings.HasPrefix(title, "~Mum") || !strings.HasSuffix(title, " · +447700900109") || len([]rune(title)) > 300 {
		t.Errorf("title of %d characters ending %q", len([]rune(title)), title[len(title)-20:])
	}
}

func TestDeletingAMessageDeletesItsDownloadsAndScrubsTheLog(t *testing.T) {
	h := newHarness(t)
	dl := &downloads{}
	h.w.d.Downloads = dl
	h.load()
	evt := &events.Message{Message: &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
		FileName: gproto.String("a.pdf"), DirectPath: gproto.String("/d"), MediaKey: key32("k"),
		FileSHA256: key32("s"), FileEncSHA256: []byte("e"), JPEGThumbnail: []byte("T"),
	}}}
	evt.Info.Chat, evt.Info.Sender, evt.Info.ID, evt.Info.Timestamp = benLID, benLID, "D1", at(-2)
	h.event(evt)
	revoke := &events.Message{Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{ID: gproto.String("D1")},
	}}}
	revoke.Info.Chat, revoke.Info.Sender, revoke.Info.ID, revoke.Info.Timestamp = benLID, benLID, "R1", at(-1)
	h.event(revoke)
	if len(dl.removed) != 2 {
		t.Errorf("removed %q", dl.removed)
	}
	path, _ := h.deps.Session.Path(storeFile)
	if info, err := os.Stat(path + "-wal"); err == nil && info.Size() != 0 {
		t.Errorf("the write-ahead log holds %d bytes after a deletion", info.Size())
	}
}

func TestThePhoneIsAskedAboutANumberOnceItsTypedAndNotTooOften(t *testing.T) {
	h := newHarness(t)
	h.load()
	lookupSettle = 30 * time.Millisecond
	t.Cleanup(func() { lookupSettle = time.Millisecond })
	asked := func() int {
		h.wa.mu.Lock()
		defer h.wa.mu.Unlock()
		return h.wa.n
	}
	_ = asked
	var wg sync.WaitGroup
	for _, q := range []string{"+4477009", "+44770090", "+447700900"} {
		wg.Add(1)
		go func() { defer wg.Done(); h.w.Search(context.Background(), q) }()
		time.Sleep(5 * time.Millisecond)
	}
	wg.Wait()
	h.w.mu.Lock()
	lookups := len(h.w.lookups)
	h.w.mu.Unlock()
	if lookups != 1 {
		t.Errorf("%d numbers asked about while one was typed", lookups)
	}
	for i := range MaxLookups + 3 {
		h.w.Search(context.Background(), "+44770090"+strings.Repeat("9", 1+i%4)+string(rune('0'+i)))
	}
	h.w.mu.Lock()
	lookups = len(h.w.lookups)
	h.w.mu.Unlock()
	if lookups > MaxLookups {
		t.Errorf("%d lookups in the window", lookups)
	}
}

func TestALinkThePhoneConfirmedIsntThrownAwayAndAnEndedOneIsRefused(t *testing.T) {
	try := newTry()
	if !try.pair() {
		t.Fatal("a waiting link refused the phone")
	}
	try.end(errCancelled, false)
	if ended, _ := try.over(); ended {
		t.Error("a cancel ended a link the phone confirmed")
	}
	try.end(errCancelled, true)
	if ended, _ := try.over(); !ended {
		t.Error("a logout didn't end it")
	}
	late := newTry()
	late.end(errCancelled, false)
	if late.pair() {
		t.Error("the phone could still confirm a cancelled link")
	}
}

func TestACancelForAnOlderAttemptLeavesTheNewOneAlone(t *testing.T) {
	h := newHarness(t)
	try := newTry()
	try.attempt = 7
	h.w.mu.Lock()
	h.w.link = try
	h.w.mu.Unlock()
	h.w.CancelLink(6)
	if ended, _ := try.over(); ended {
		t.Fatal("an older attempt's cancel ended the new link")
	}
	h.w.CancelLink(7)
	if ended, _ := try.over(); !ended {
		t.Fatal("its own cancel didn't end it")
	}
}

func TestALinkWhoseCancelCameFirstNeverStarts(t *testing.T) {
	h := newHarness(t)
	h.event(text(benLID, benLID, "B1", -1, "kept"))
	h.w.mu.Lock()
	h.w.ready = false
	h.w.mu.Unlock()
	h.w.CancelLink(5)
	// Its context is over too, so a link that did start would stop at the
	// store, long before WhatsApp could be reached.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, attempt := range []uint64{5, 4} {
		err := h.w.Link(ctx, "", attempt)
		var pe *proto.Error
		if !errors.As(err, &pe) || pe.Code != proto.Cancelled {
			t.Errorf("attempt %d: %v", attempt, err)
		}
	}
	if h.kept(benLID.String(), "B1") == nil {
		t.Error("a link given up before it started wiped what was kept")
	}
}

func TestACodeIsNeverShownForALinkGivenUpWhileItWasAskedFor(t *testing.T) {
	h := newHarness(t)
	try := newTry()
	pair := func(context.Context, string) (string, error) {
		try.end(errCancelled, false) // given up while WhatsApp answered
		return "ABCD-EFGH", nil
	}
	err := h.w.awaitScan(context.Background(), try, "447700900100", codesOf(code("2@x", time.Minute)), pair)
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.Cancelled {
		t.Fatalf("got %v", err)
	}
	if len(h.rec.codes()) != 0 {
		t.Errorf("shown %+v", h.rec.codes())
	}
}

func TestLoggingOutSaysToRemoveTheDeviceOnlyWhenOneWasLinked(t *testing.T) {
	h := newHarness(t)
	if err := h.w.Logout(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, v := range h.rec.all() {
		if _, ok := v.(proto.ErrorEvent); ok {
			t.Error("a warning with nothing linked")
		}
	}
	h2 := newHarness(t)
	h2.deps.Session.SaveJSON(sessionFile, savedSession{Version: 1, Device: selfPN.String()})
	h2.w.mu.Lock()
	h2.w.ready = false // offline: it can't be unlinked
	h2.w.mu.Unlock()
	h2.w.Logout(context.Background())
	warned := false
	for _, v := range h2.rec.all() {
		if e, ok := v.(proto.ErrorEvent); ok && strings.Contains(e.Message, "Linked devices") {
			warned = true
		}
	}
	if !warned {
		t.Error("no word about the device left on the phone")
	}
}

func TestLeftoversAndKeysOfGoneMessagesGoWhenTheStoreOpens(t *testing.T) {
	h := newHarness(t)
	dir, _ := h.deps.Session.Dir()
	left := filepath.Join(dir, "download-123")
	if err := os.WriteFile(left, []byte("decrypted"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := h.w.st.db.ExecContext(ctx, `PRAGMA foreign_keys = OFF; INSERT INTO whatsmeow_message_secrets (our_jid, chat_jid, sender_jid, message_id, key) VALUES ('a', 'b', 'c', 'GONE', x'00')`); err != nil {
		t.Fatal(err)
	}
	h2 := h.restart()
	if _, err := os.Stat(left); !os.IsNotExist(err) {
		t.Error("a decrypted leftover stayed")
	}
	var n int
	h2.w.st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM whatsmeow_message_secrets WHERE message_id = 'GONE'`).Scan(&n)
	if n != 0 {
		t.Error("a gone message's key stayed")
	}
}

var _ = whatsmeow.QRChannelSuccess
