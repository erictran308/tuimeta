// SPDX-License-Identifier: AGPL-3.0-or-later

package messenger

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waConsumerApplication"
	"go.mau.fi/whatsmeow/proto/waMediaTransport"
	waTypes "go.mau.fi/whatsmeow/types"
	gproto "google.golang.org/protobuf/proto"

	"go.mau.fi/mautrix-meta/pkg/messagix"
	"go.mau.fi/mautrix-meta/pkg/messagix/socket"
	"go.mau.fi/mautrix-meta/pkg/messagix/table"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/browser"
	"github.com/erictran308/tuimeta/helper/internal/cookies"
	"github.com/erictran308/tuimeta/helper/internal/download"
	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// explode makes h's backend panic, as a bug while applying would, at every
// event boom picks; the others are recorded as usual.
func explode(h *harness, boom func(any) bool) {
	ev := backend.NewEvents(func(v any) {
		if boom(v) {
			panic("boom")
		}
		h.rec.add(v)
	})
	h.m.mu.Lock()
	h.m.d.Events = ev
	h.m.d.Outbox = backend.NewOutbox(ev, h.deps.Messages)
	h.m.mu.Unlock()
	h.deps.Events, h.deps.Outbox = ev, h.m.d.Outbox
}

// startsTyping picks someone starting to type.
func startsTyping(v any) bool {
	e, ok := v.(proto.TypingEvent)
	return ok && e.Typing
}

// aliceTypes is a table saying Alice is typing in her chat.
func aliceTypes() *table.LSTable {
	return &table.LSTable{LSUpdateTypingIndicator: []*table.LSUpdateTypingIndicator{{ThreadKey: aliceID, SenderId: aliceID, IsTyping: true}}}
}

// survive runs f as the helper's dispatchers and event handlers do: a panic
// is recovered and the run goes on.
func survive(f func()) {
	defer func() { _ = recover() }()
	f()
}

// stillUsable fails the test if what left the backend locked, or a batch
// of chat changes open that would hold back every later chat event.
func stillUsable(t *testing.T, h *harness, what string) {
	t.Helper()
	if !h.m.mu.TryLock() {
		t.Fatalf("%s left Messenger locked", what)
	}
	defer h.m.mu.Unlock()
	if h.m.dirty != nil {
		t.Errorf("%s left a batch of chat changes open", what)
	}
}

func TestAPanicWhileApplyingWhatArrivesNeverLeavesMessengerLocked(t *testing.T) {
	withAlice := func(t *testing.T) *harness {
		h := newHarness(t)
		h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{dmThread(aliceID, -10, -10, 0)}})
		return h
	}
	t.Run("a table on the socket", func(t *testing.T) {
		h := withAlice(t)
		h.load()
		explode(h, startsTyping)
		survive(func() { h.m.onMetaEvent(0, aliceTypes(), nil, func(error) {}) })
		stillUsable(t, h, "the socket's table")
		// messagix moves past a table that can't be applied, rather than
		// have it sent again at every reconnect.
		h.meta.mu.Lock()
		posted := h.meta.posted
		h.meta.mu.Unlock()
		if posted != 1 {
			t.Errorf("handled %d times", posted)
		}
	})
	t.Run("the page's own first data", func(t *testing.T) {
		h := withAlice(t)
		h.load()
		explode(h, startsTyping)
		var signalled []error
		h.m.onMetaEvent(0, &messagix.ConnectedEvent{}, aliceTypes(), func(err error) { signalled = append(signalled, err) })
		stillUsable(t, h, "the initial page")
		// The account still connects: the same page would fail again at
		// every try.
		if len(signalled) != 1 || signalled[0] != nil {
			t.Errorf("signalled %v", signalled)
		}
		h.m.mu.Lock()
		st := h.m.store
		h.m.mu.Unlock()
		if st == nil {
			t.Error("the encrypted chats' store wasn't opened")
		} else {
			st.Close()
		}
	})
	t.Run("a request's answer", func(t *testing.T) {
		h := withAlice(t)
		h.load()
		explode(h, startsTyping)
		h.meta.answer = func([]socket.Task) (*table.LSTable, error) { return aliceTypes(), nil }
		survive(func() { _, _ = h.m.run(context.Background(), "history", &socket.FetchMessagesTask{ThreadKey: aliceID}) })
		stillUsable(t, h, "a request's answer")
	})
	t.Run("a file's fresh address", func(t *testing.T) {
		h := withAlice(t)
		h.load()
		explode(h, startsTyping)
		h.meta.answer = func([]socket.Task) (*table.LSTable, error) { return aliceTypes(), nil }
		ref := ids.FileRef{ID: 9, Network: proto.Messenger, Source: &fbSource{URL: "https://scontent.xx.fbcdn.net/p.jpg", Thread: aliceID, MessageID: "mid.$p"}}
		survive(func() { h.m.refreshURL(context.Background(), ref) })
		stillUsable(t, h, "refreshing a file's address")
	})
	t.Run("a page of older threads", func(t *testing.T) {
		h := withAlice(t)
		h.meta.pages = []*table.LSTable{aliceTypes()}
		explode(h, startsTyping)
		survive(func() { _, _ = h.m.LoadChats(context.Background(), 50) })
		stillUsable(t, h, "a page of threads")
	})
	t.Run("a reaction sent in an encrypted chat", func(t *testing.T) {
		h := newHarness(t)
		h.load()
		h.wa(waMsg(jid(aliceID), jid(aliceID), "M1", -3, waText("hi"), nil))
		c := h.chat(aliceID)
		msg := h.rec.messages()[0]
		explode(h, func(v any) bool {
			e, ok := v.(proto.MessageEvent)
			return ok && len(e.Message.Reactions) > 0
		})
		survive(func() { _ = h.m.React(context.Background(), h.ref(c, msg.ID), "👍") })
		stillUsable(t, h, "an encrypted reaction")
	})
	t.Run("a message sent that Messenger confirms only by its id", func(t *testing.T) {
		h := withAlice(t)
		h.load()
		c := h.chat(aliceID)
		explode(h, func(v any) bool { _, ok := v.(proto.MessageSentEvent); return ok })
		h.meta.answer = func(tasks []socket.Task) (*table.LSTable, error) {
			otid := strconv.FormatInt(sendTask(t, tasks).Otid, 10)
			return &table.LSTable{LSReplaceOptimsiticMessage: []*table.LSReplaceOptimsiticMessage{{OfflineThreadingId: otid, MessageId: "mid.$s"}}}, nil
		}
		survive(func() { _ = h.m.Send(context.Background(), h.outgoing(c, "hi", nil, nil)) })
		stillUsable(t, h, "confirming a send")
	})
}

func TestAMessageRowThatCantBeAppliedIsDroppedAndTheRestOfItsTableApplies(t *testing.T) {
	saysBoom := func(v any) bool {
		e, ok := v.(proto.MessageEvent)
		return ok && e.Message.Text == "boom"
	}
	texts := func(h *harness) []string {
		var out []string
		for _, m := range h.rec.messages() {
			out = append(out, m.Text)
		}
		return out
	}
	t.Run("new messages", func(t *testing.T) {
		h := newHarness(t)
		h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{dmThread(aliceID, -10, -10, 0)}})
		h.load()
		explode(h, saysBoom)
		var escaped any
		func() {
			defer func() { escaped = recover() }()
			h.apply(&table.LSTable{
				LSInsertMessage:         []*table.LSInsertMessage{textMsg(aliceID, "mid.$b", aliceID, -2, "boom"), textMsg(aliceID, "mid.$f", aliceID, -1, "fine")},
				LSUpdateTypingIndicator: []*table.LSUpdateTypingIndicator{{ThreadKey: aliceID, SenderId: aliceID, IsTyping: true}},
			})
		}()
		if escaped != nil {
			t.Fatal("one row cost the whole table")
		}
		if got := texts(h); !slices.Equal(got, []string{"fine"}) {
			t.Errorf("messages %q", got)
		}
		if len(h.rec.of("typing")) != 1 {
			t.Error("the rest of the table wasn't applied")
		}
		stillUsable(t, h, "a row that can't be applied")
	})
	t.Run("a page of history", func(t *testing.T) {
		h := newHarness(t)
		h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{dmThread(aliceID, -10, -10, 0)}})
		h.load()
		h.apply(&table.LSTable{LSInsertMessage: []*table.LSInsertMessage{textMsg(aliceID, "mid.$k", aliceID, -10, "known")}})
		h.rec.reset()
		explode(h, saysBoom)
		var escaped any
		func() {
			defer func() { escaped = recover() }()
			// A gap filled after a reconnect: the newer ones are new.
			h.apply(&table.LSTable{
				LSInsertNewMessageRange: []*table.LSInsertNewMessageRange{{ThreadKey: aliceID, HasMoreBefore: true}},
				LSUpsertMessage: []*table.LSUpsertMessage{
					{ThreadKey: aliceID, MessageId: "mid.$hb", SenderId: aliceID, TimestampMs: ms(-3), Text: "boom"},
					{ThreadKey: aliceID, MessageId: "mid.$hf", SenderId: aliceID, TimestampMs: ms(-2), Text: "fine"},
				},
			})
		}()
		if escaped != nil {
			t.Fatal("one row cost the whole page")
		}
		if got := texts(h); !slices.Equal(got, []string{"fine"}) {
			t.Errorf("messages %q", got)
		}
		stillUsable(t, h, "a history row that can't be applied")
	})
}

func TestAQuoteReadsOnlyTheStartOfTheTextItQuotes(t *testing.T) {
	// The marker that would close the bold comes past what's read.
	long := "*" + strings.Repeat("a", 2*maxQuoted) + "*"
	h := newHarness(t)
	h.apply(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{dmThread(aliceID, -10, -10, 0)}})
	h.load()
	reply := textMsg(aliceID, "mid.$r", aliceID, -4, "at 5")
	reply.ReplySourceId, reply.ReplyToUserId, reply.ReplyMessageText = "mid.$q", selfID, long
	h.apply(&table.LSTable{LSInsertMessage: []*table.LSInsertMessage{reply}})
	got := h.rec.messages()[0]
	if got.ReplyTo == nil || !strings.HasPrefix(got.ReplyTo.Text, "*aaa") {
		t.Errorf("reply_to %+v", got.ReplyTo)
	}
	// The text is cut on a character's boundary.
	if cut := cutText("a"+strings.Repeat("é", maxQuoted), maxQuoted); len(cut) != maxQuoted-1 || !utf8.ValidString(cut) {
		t.Errorf("cut to %d bytes, valid %v", len(cut), utf8.ValidString(cut))
	}
	if cutText("short", maxQuoted) != "short" || cutText("", maxQuoted) != "" {
		t.Error("a short text was cut")
	}
}

func TestALinkThroughFacebooksRedirectIsUnwrappedWithinBounds(t *testing.T) {
	shim := func(inner string) string { return "https://l.facebook.com/l.php?u=" + url.QueryEscape(inner) }
	longest := "https://a.example/" + strings.Repeat("x", MaxLink-len("https://a.example/"))
	cases := map[string]string{
		shim("https://a.example/"):                               "https://a.example/",
		shim(shim("https://a.example/")):                         "https://a.example/",
		shim(shim(shim("https://a.example/"))):                   "", // no real link is wrapped like that
		"https://L.FACEBOOK.COM/l.php?u=https%3A%2F%2Fb.example": "https://b.example",
		"https://facebook.com/l.php?u=https%3A%2F%2Fb.example":   "https://b.example",
		shim("javascript:alert(1)"):                              "",
		// Only facebook.com's own names are unwrapped.
		"https://lfacebook.com/l.php?u=https%3A%2F%2Fb.example":        "https://lfacebook.com/l.php?u=https%3A%2F%2Fb.example",
		"https://facebook.com.example/l.php?u=https%3A%2F%2Fb.example": "https://facebook.com.example/l.php?u=https%3A%2F%2Fb.example",
		// An address longer than MaxLink isn't looked at, wrapped or not.
		longest:       longest,
		longest + "x": "",
		shim(longest): "",
	}
	for in, want := range cases {
		if got := webLink(in); got != want {
			t.Errorf("webLink(%.60q…) = %.60q…, want %.60q…", in, got, want)
		}
	}
	// A sender's chain of redirects costs as little as a plain link, however
	// long: unwrapped as far as it went, a megabyte took half a minute.
	for _, size := range []int{MaxLink - 100, 1 << 20} {
		chain := strings.Repeat("//facebook.com/l.php?u=", size/23) + "https://a.example/"
		start := time.Now()
		if got := webLink(chain); got != "" {
			t.Errorf("a %d-byte chain gave %.60q", len(chain), got)
		}
		if d := time.Since(start); d > 20*time.Millisecond {
			t.Errorf("a %d-byte chain took %v", len(chain), d)
		}
	}
}

// waFile is an encrypted attachment of kind kept at path on the fake media
// server, stating size and mime.
func waFile(id int32, path string, kind proto.MediaKind, mime string, size int64) ids.FileRef {
	return ids.FileRef{
		ID: id, Network: proto.Messenger, Key: "wa:" + path, Mime: mime, Size: size,
		Source: &waSource{Integral: &waMediaTransport.WAMediaTransport_Integral{DirectPath: gproto.String(path)}, MediaType: whatsmeow.MediaImage, Kind: kind},
	}
}

func TestEncryptedAttachmentsAreDownloadedOnlyUpToTheirKindsLimit(t *testing.T) {
	h := newHarness(t)
	big := bytes.Repeat([]byte{0xff}, MaxPhoto+(1<<20))
	h.e2ee.files["/v/big"] = big
	// A chunk of the copy past the limit may come before it's refused.
	const slack = 64 + 32<<10
	cases := []struct {
		kind  proto.MediaKind
		mime  string
		limit int64
	}{
		{proto.Photo, "image/jpeg", MaxPhoto},
		{proto.Photo, "video/mp4", MaxPhoto},
		{proto.GIF, "image/gif", MaxPhoto},
		{proto.Sticker, "image/webp", MaxSticker},
	}
	for i, tc := range cases {
		h.e2ee.served = 0
		var buf writeBuf
		// The message says it's 5 bytes: what's capped is what comes.
		err := h.m.Fetch(context.Background(), waFile(int32(i+1), "/v/big", tc.kind, tc.mime, 5), &buf)
		if !errors.Is(err, errTooBigHere) || len(buf) != 0 {
			t.Errorf("%v %s: %v, %d bytes", tc.kind, tc.mime, err, len(buf))
		}
		if h.e2ee.served > tc.limit+slack {
			t.Errorf("%v %s: %d bytes taken in for a limit of %d", tc.kind, tc.mime, h.e2ee.served, tc.limit)
		}
	}
	// A file asked for only by the user (a document, a video) may be as big
	// as any download.
	var buf writeBuf
	if err := h.m.Fetch(context.Background(), waFile(10, "/v/big", proto.FileMedia, "application/pdf", 5), &buf); err != nil || !bytes.Equal(buf, big) {
		t.Errorf("document: %v, %d bytes", err, len(buf))
	}
	// One that says it's over its limit isn't asked for at all.
	h.e2ee.served = 0
	if err := h.m.Fetch(context.Background(), waFile(11, "/v/big", proto.Sticker, "image/webp", MaxSticker+1), &buf); !errors.Is(err, errTooBigHere) || h.e2ee.served != 0 {
		t.Errorf("stated too big: %v, %d bytes", err, h.e2ee.served)
	}
	// Nothing is left behind in the session folder.
	dir, _ := h.deps.Session.Dir()
	if left, _ := filepath.Glob(filepath.Join(dir, "download-*")); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}

func TestAnEncryptedPhotoOnScreenIsBoundedFromTheMessageItCameIn(t *testing.T) {
	h := newHarness(t)
	h.load()
	img := &waConsumerApplication.ConsumerApplication_ImageMessage{}
	if err := img.Set(&waMediaTransport.ImageTransport{
		Integral: &waMediaTransport.ImageTransport_Integral{Transport: &waMediaTransport.WAMediaTransport{
			Integral:  &waMediaTransport.WAMediaTransport_Integral{FileSHA256: []byte{0xbe, 0xef}, FileEncSHA256: []byte{1}, MediaKey: []byte{2}, DirectPath: gproto.String("/v/huge")},
			Ancillary: &waMediaTransport.WAMediaTransport_Ancillary{FileLength: gproto.Uint64(5), Mimetype: gproto.String("image/jpeg")},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	h.wa(waMsg(jid(aliceID), jid(aliceID), "P1", -1, &waConsumerApplication.ConsumerApplication_Content{Content: &waConsumerApplication.ConsumerApplication_Content_ImageMessage{ImageMessage: img}}, nil))
	md := h.rec.messages()[0].Media
	if md == nil || md.Thumbnail == nil {
		t.Fatalf("media %+v", md)
	}
	h.e2ee.files["/v/huge"] = bytes.Repeat([]byte{1}, 3*MaxPhoto)
	ref, _ := h.deps.Files.Get(md.Thumbnail.FileID)
	var buf writeBuf
	if err := h.m.Fetch(context.Background(), ref, &buf); !errors.Is(err, errTooBigHere) || h.e2ee.served > MaxPhoto+64+32<<10 {
		t.Errorf("preview: %v, %d bytes taken in", err, h.e2ee.served)
	}
}

func TestADownloadsFileNeverGrowsPastItsLimit(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "download-*")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	capped := &cappedFile{File: f, limit: 10}
	if _, err := capped.ReadFrom(bytes.NewReader(make([]byte, 11))); !errors.Is(err, errTooBig) {
		t.Errorf("copy: %v", err)
	}
	if _, err := capped.WriteAt([]byte("x"), 10); !errors.Is(err, errTooBig) {
		t.Errorf("write at: %v", err)
	}
	if err := capped.Truncate(11); !errors.Is(err, errTooBig) {
		t.Errorf("truncate: %v", err)
	}
	if info, _ := f.Stat(); info.Size() > 10 {
		t.Errorf("size %d", info.Size())
	}
	if waLimit(proto.FileMedia, "image/png") != download.MaxSize || waLimit(proto.GIF, "video/mp4") != download.MaxSize || waLimit(proto.Video, "video/mp4") != download.MaxSize {
		t.Error("what's downloaded only on request is limited like any download")
	}
}

// contactLookups is how many contact lookups h's connection was sent.
func contactLookups(h *harness) map[int64]int {
	asked := map[int64]int{}
	for _, task := range h.meta.sent() {
		if g, ok := task.(*socket.GetContactsFullTask); ok {
			asked[g.ContactID]++
		}
	}
	return asked
}

func TestContactLookupsGoOutAFewAtATimeAndWithinABudget(t *testing.T) {
	h := newHarness(t)
	h.m.mu.Lock()
	h.m.contactRecheck = 5 * time.Millisecond
	h.m.mu.Unlock()
	defer func() {
		h.m.mu.Lock()
		h.m.stop()
		h.m.mu.Unlock()
	}()
	var inFlight, most atomic.Int32
	h.meta.answer = func([]socket.Task) (*table.LSTable, error) {
		n := inFlight.Add(1)
		for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
		}
		time.Sleep(time.Millisecond)
		inFlight.Add(-1)
		return &table.LSTable{}, nil
	}
	// One message naming many people nobody here knows yet.
	const named = MaxContactLookups + 20
	h.m.mu.Lock()
	for i := range named {
		h.m.tellUser(h.m.person(int64(200000+i)), false)
	}
	h.m.mu.Unlock()
	waitFor(t, "the budget's lookups", func() bool { return len(contactLookups(h)) == MaxContactLookups })
	time.Sleep(50 * time.Millisecond)
	if n := len(contactLookups(h)); n != MaxContactLookups {
		t.Errorf("%d lookups within the window", n)
	}
	if most.Load() > ContactsAtOnce {
		t.Errorf("%d lookups at once", most.Load())
	}
	// Past the window, the rest are asked about, each once.
	h.m.mu.Lock()
	h.m.now = func() time.Time { return base.Add(ContactWindow) }
	h.m.mu.Unlock()
	waitFor(t, "the rest", func() bool { return len(contactLookups(h)) == named })
	for fbid, n := range contactLookups(h) {
		if n != 1 {
			t.Errorf("%d asked about %d times", fbid, n)
		}
	}
	// The line is bounded too: whoever doesn't fit is asked about when
	// seen again.
	h.m.mu.Lock()
	for i := range MaxContactQueue + 10 {
		h.m.tellUser(h.m.person(int64(300000+i)), false)
	}
	waiting := len(h.m.contactQueue)
	late := h.m.people[int64(300000+MaxContactQueue+9)]
	h.m.mu.Unlock()
	if waiting > MaxContactQueue || late.asked {
		t.Errorf("%d waiting; the last one asked: %v", waiting, late.asked)
	}
}

func TestALoginGivenUpForANewerOneDoesntLogTheNewerOneOut(t *testing.T) {
	h := newHarness(t)
	firstIn := make(chan struct{})
	release := make(chan struct{})
	var dials atomic.Int32
	h.m.dial = func(ctx, _ context.Context, _ login) error {
		if dials.Add(1) == 1 {
			close(firstIn)
			<-ctx.Done() // the newer login gives this one up
			return errNetwork
		}
		<-release
		return nil
	}
	set, err := cookies.Parse(proto.Messenger, "c_user=1; xs=2; datr=3")
	if err != nil {
		t.Fatal(err)
	}
	first := make(chan error, 1)
	go func() { first <- h.m.LoginCookies(context.Background(), set, browser.Default()) }()
	<-firstIn
	second := make(chan error, 1)
	go func() { second <- h.m.LoginCookies(context.Background(), set, browser.Default()) }()
	if err := <-first; err == nil {
		t.Error("the first login says it worked")
	}
	close(release)
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	for _, a := range h.rec.of("account") {
		if a.(proto.AccountEvent).State == proto.LoggedOut {
			t.Error("the given-up login logged the newer one out")
		}
	}
}

func TestASessionIsntSavedForAConnectionLoggedOutOfAsItSynced(t *testing.T) {
	h := newHarness(t)
	h.m.mu.Lock()
	gen := h.m.gen
	h.m.mu.Unlock()
	if err := h.m.Logout(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.m.keepSession(gen, selfID, map[string]string{"c_user": "100001", "xs": "x"}, browser.Default()) {
		t.Error("a connection logged out of was kept")
	}
	if ok, _ := h.deps.Session.LoadJSON(sessionFile, &savedSession{}); ok {
		t.Error("its cookies were saved after the logout")
	}
	h.m.mu.Lock()
	gen = h.m.gen
	h.m.mu.Unlock()
	if !h.m.keepSession(gen, selfID, map[string]string{"c_user": "100001", "xs": "x"}, browser.Default()) {
		t.Fatal("the current connection wasn't kept")
	}
	var s savedSession
	if ok, _ := h.deps.Session.LoadJSON(sessionFile, &s); !ok || s.Cookies["xs"] != "x" || s.UserID != selfID {
		t.Errorf("session %+v", s)
	}
}

func TestAnEncryptedDeviceRegisteredAsYouLogOutIsntSaved(t *testing.T) {
	h := newHarness(t)
	h.m.mu.Lock()
	gen := h.m.gen
	h.m.mu.Unlock()
	if !h.m.keepDevice(gen, "100001.0:7@msgr", map[string]string{"c_user": "100001", "xs": "y"}) {
		t.Fatal("the current connection's device wasn't kept")
	}
	var s savedSession
	if ok, _ := h.deps.Session.LoadJSON(sessionFile, &s); !ok || s.WADevice != "100001.0:7@msgr" || s.Cookies["xs"] != "y" {
		t.Errorf("session %+v", s)
	}
	if err := h.m.Logout(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.m.keepDevice(gen, "100001.0:8@msgr", map[string]string{"c_user": "100001", "xs": "z"}) {
		t.Error("a connection logged out of kept its device")
	}
	if ok, _ := h.deps.Session.LoadJSON(sessionFile, &savedSession{}); ok {
		t.Error("the session was saved again after the logout")
	}
}

func TestALinkInTheEncryptedStoresPlaceIsRefusedNotFollowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("links aren't refused on Windows")
	}
	dir := t.TempDir()
	elsewhere := filepath.Join(t.TempDir(), "elsewhere.db")
	if err := os.WriteFile(elsewhere, []byte("theirs"), 0o644); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(dir, storeFile)
	if err := os.Symlink(elsewhere, planted); err != nil {
		t.Fatal(err)
	}
	if st, err := openStore(context.Background(), planted); err == nil {
		st.Close()
		t.Error("the store was opened through a link")
	}
	if data, _ := os.ReadFile(elsewhere); string(data) != "theirs" {
		t.Errorf("written through: %q", data)
	}
	if info, _ := os.Stat(elsewhere); info.Mode().Perm() != 0o644 {
		t.Errorf("the link's target was chmodded: %v", info.Mode())
	}
	// Nor one pointing at nothing yet: nothing is made where it points.
	dangling := filepath.Join(t.TempDir(), storeFile)
	nowhere := filepath.Join(dir, "nothing-yet")
	if err := os.Symlink(nowhere, dangling); err != nil {
		t.Fatal(err)
	}
	if st, err := openStore(context.Background(), dangling); err == nil {
		st.Close()
		t.Error("the store was opened through a dangling link")
	}
	if _, err := os.Lstat(nowhere); !os.IsNotExist(err) {
		t.Error("a file was made where the dangling link pointed")
	}
	// The store's own file opens as before, private.
	own := filepath.Join(t.TempDir(), storeFile)
	st, err := openStore(context.Background(), own)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if info, _ := os.Stat(own); info.Mode().Perm() != 0o600 {
		t.Errorf("the store is %v", info.Mode())
	}
}

func TestEncryptedTypingGoesOutOnlyForATypingRequest(t *testing.T) {
	var conn e2eeAPI = &e2eeConn{}
	for _, purpose := range []string{"", "mark_read", "send", "history", "open_dm"} {
		if err := conn.SendChatPresence(context.Background(), purpose, jid(aliceID), waTypes.ChatPresenceComposing, waTypes.ChatPresenceMediaText); err != errUnasked {
			t.Errorf("%q: %v", purpose, err)
		}
	}
}

func TestEncryptedReadReceiptsGoOutOnlyForAMarkReadRequest(t *testing.T) {
	var conn e2eeAPI = &e2eeConn{}
	ids := []waTypes.MessageID{"A1"}
	for _, purpose := range []string{"", "typing", "send", "history", "open_dm", "react"} {
		if err := conn.MarkRead(context.Background(), purpose, ids, base, jid(aliceID), waTypes.EmptyJID); err != errUnasked {
			t.Errorf("read for %q: %v", purpose, err)
		}
		if err := conn.MarkReadSelf(context.Background(), purpose, ids, base, jid(aliceID), waTypes.EmptyJID); err != errUnasked {
			t.Errorf("read-self for %q: %v", purpose, err)
		}
	}
	// MarkRead passes its own purpose, whichever kind goes out.
	for _, off := range []bool{false, true} {
		f := &fakeE2EE{}
		if err := markReadWA(context.Background(), f, "mark_read", off, ids, base, jid(aliceID), waTypes.EmptyJID); err != nil || len(f.reads)+len(f.selfReads) != 1 || (len(f.selfReads) == 1) != off {
			t.Errorf("receipts off %v: %v, %+v %+v", off, err, f.reads, f.selfReads)
		}
		if err := markReadWA(context.Background(), f, "history", off, ids, base, jid(aliceID), waTypes.EmptyJID); err != errUnasked {
			t.Errorf("receipts off %v, for history: %v", off, err)
		}
	}
}
