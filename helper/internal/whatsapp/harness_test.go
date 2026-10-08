// SPDX-License-Identifier: AGPL-3.0-or-later

package whatsapp

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	wastore "go.mau.fi/whatsmeow/store"
	waTypes "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	gproto "google.golang.org/protobuf/proto"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
	"github.com/erictran308/tuimeta/helper/internal/session"
)

// The account tests are linked to, and the people and groups it talks to.
// Every number, id and name here is made up (UK drama numbers, 07700 900xxx).
var (
	selfPN   = waTypes.NewJID("447700900100", waTypes.DefaultUserServer)
	selfLID  = waTypes.NewJID("100000000000001", waTypes.HiddenUserServer)
	alicePN  = waTypes.NewJID("447700900101", waTypes.DefaultUserServer)
	aliceLID = waTypes.NewJID("100000000000002", waTypes.HiddenUserServer)
	benPN    = waTypes.NewJID("447700900102", waTypes.DefaultUserServer)
	benLID   = waTypes.NewJID("100000000000003", waTypes.HiddenUserServer)
	groupJID = waTypes.NewJID("120363000000000001", waTypes.GroupServer)
)

// base is a fixed "now" for tests: 2026-10-08 12:00 UTC.
var base = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func at(min int) time.Time { return base.Add(time.Duration(min) * time.Minute) }

// recorder keeps every event the backend writes.
type recorder struct {
	mu  sync.Mutex
	out []any
}

func (r *recorder) add(v any) {
	r.mu.Lock()
	r.out = append(r.out, v)
	r.mu.Unlock()
}

func (r *recorder) all() []any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]any(nil), r.out...)
}

func (r *recorder) reset() {
	r.mu.Lock()
	r.out = nil
	r.mu.Unlock()
}

func (r *recorder) messages() []proto.Message {
	var out []proto.Message
	for _, v := range r.all() {
		if e, ok := v.(proto.MessageEvent); ok {
			out = append(out, e.Message)
		}
	}
	return out
}

func (r *recorder) chats() []proto.Chat {
	var out []proto.Chat
	for _, v := range r.all() {
		if e, ok := v.(proto.ChatEvent); ok {
			out = append(out, e.Chat)
		}
	}
	return out
}

func (r *recorder) users() []proto.User {
	var out []proto.User
	for _, v := range r.all() {
		if e, ok := v.(proto.UserEvent); ok {
			out = append(out, e.User)
		}
	}
	return out
}

func (r *recorder) reads() []proto.ReadEvent {
	var out []proto.ReadEvent
	for _, v := range r.all() {
		if e, ok := v.(proto.ReadEvent); ok {
			out = append(out, e)
		}
	}
	return out
}

func (r *recorder) deletions() []proto.MessageDeletedEvent {
	var out []proto.MessageDeletedEvent
	for _, v := range r.all() {
		if e, ok := v.(proto.MessageDeletedEvent); ok {
			out = append(out, e)
		}
	}
	return out
}

func (r *recorder) codes() []proto.LoginCodeEvent {
	var out []proto.LoginCodeEvent
	for _, v := range r.all() {
		if e, ok := v.(proto.LoginCodeEvent); ok {
			out = append(out, e)
		}
	}
	return out
}

func (r *recorder) accounts() []proto.AccountEvent {
	var out []proto.AccountEvent
	for _, v := range r.all() {
		if e, ok := v.(proto.AccountEvent); ok {
			out = append(out, e)
		}
	}
	return out
}

// sentMsg is a message the backend sent.
type sentMsg struct {
	to    waTypes.JID
	msg   *waE2E.Message
	extra whatsmeow.SendRequestExtra
}

type readCall struct {
	ids          []waTypes.MessageID
	chat, sender waTypes.JID
}

// fakeWA is a WhatsApp connection that records what's asked of it. Its
// builders and its reading of history are a real whatsmeow client's, which
// never connects: it only knows who it is.
type fakeWA struct {
	real *whatsmeow.Client

	mu        sync.Mutex
	contacts  map[waTypes.JID]waTypes.ContactInfo
	lids      map[waTypes.JID]waTypes.JID // number → WhatsApp id
	groups    map[waTypes.JID]*waTypes.GroupInfo
	onWA      map[string]waTypes.JID
	files     map[string][]byte // by direct path
	sent      []sentMsg
	reads     []readCall
	typing    []waTypes.ChatPresence
	purposes  []string
	mutes     []bool
	uploads   [][]byte
	loggedOut bool
	sendErr   error
	n         int
}

func (f *fakeWA) SendMessage(_ context.Context, to waTypes.JID, msg *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return whatsmeow.SendResponse{}, f.sendErr
	}
	s := sentMsg{to: to, msg: msg}
	if len(extra) > 0 {
		s.extra = extra[0]
	}
	f.sent = append(f.sent, s)
	return whatsmeow.SendResponse{Timestamp: base.Add(time.Duration(len(f.sent)) * time.Second)}, nil
}

func (f *fakeWA) MarkRead(_ context.Context, purpose string, list []waTypes.MessageID, _ time.Time, chat, sender waTypes.JID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.purposes = append(f.purposes, purpose)
	if purpose != "mark_read" {
		return errUnasked
	}
	f.reads = append(f.reads, readCall{ids: append([]waTypes.MessageID(nil), list...), chat: chat, sender: sender})
	return nil
}

func (f *fakeWA) SendChatPresence(_ context.Context, purpose string, _ waTypes.JID, state waTypes.ChatPresence) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.purposes = append(f.purposes, purpose)
	if purpose != "typing" {
		return errUnasked
	}
	f.typing = append(f.typing, state)
	return nil
}

func (f *fakeWA) Mute(_ context.Context, _ waTypes.JID, muted bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mutes = append(f.mutes, muted)
	return nil
}

func (f *fakeWA) Download(_ context.Context, m *media, _ string, limit int64, out io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.files[m.DirectPath]
	if !ok {
		return whatsmeow.ErrMediaDownloadFailedWith404
	}
	if int64(len(data)) > limit {
		return errTooBig
	}
	_, err := out.Write(data)
	return err
}

func (f *fakeWA) Upload(_ context.Context, data []byte, _ whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads = append(f.uploads, data)
	n := strconv.Itoa(len(f.uploads))
	return whatsmeow.UploadResponse{
		URL: "https://mmg.whatsapp.net/up/" + n, DirectPath: "/up/" + n,
		MediaKey: []byte("key" + n), FileSHA256: []byte("sha" + n), FileEncSHA256: []byte("enc" + n), FileLength: uint64(len(data)),
	}, nil
}

func (f *fakeWA) GetGroupInfo(_ context.Context, jid waTypes.JID) (*waTypes.GroupInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if g := f.groups[jid]; g != nil {
		return g, nil
	}
	return nil, errors.New("no such group")
}

func (f *fakeWA) ProfilePicture(context.Context, waTypes.JID) (*waTypes.ProfilePictureInfo, error) {
	return nil, whatsmeow.ErrProfilePictureNotSet
}

func (f *fakeWA) IsOnWhatsApp(_ context.Context, phones []string) ([]waTypes.IsOnWhatsAppResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []waTypes.IsOnWhatsAppResponse
	for _, p := range phones {
		j, ok := f.onWA[p]
		out = append(out, waTypes.IsOnWhatsAppResponse{Query: p, JID: j, IsIn: ok})
	}
	return out, nil
}

func (f *fakeWA) DecryptReaction(context.Context, *events.Message) (*waE2E.ReactionMessage, error) {
	return nil, errors.New("no secret")
}

func (f *fakeWA) ParseWebMessage(chat waTypes.JID, msg *waWeb.WebMessageInfo) (*events.Message, error) {
	return f.real.ParseWebMessage(chat, msg)
}

func (f *fakeWA) GenerateMessageID() waTypes.MessageID {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	return "3EB0SENT" + strconv.Itoa(f.n)
}

func (f *fakeWA) BuildEdit(chat waTypes.JID, id waTypes.MessageID, content *waE2E.Message) *waE2E.Message {
	return f.real.BuildEdit(chat, id, content)
}

func (f *fakeWA) BuildRevoke(chat, sender waTypes.JID, id waTypes.MessageID) *waE2E.Message {
	return f.real.BuildRevoke(chat, sender, id)
}

func (f *fakeWA) BuildReaction(chat, sender waTypes.JID, id waTypes.MessageID, emoji string) *waE2E.Message {
	return f.real.BuildReaction(chat, sender, id, emoji)
}

func (f *fakeWA) BuildHistorySyncRequest(oldest *waTypes.MessageInfo, count int) *waE2E.Message {
	return f.real.BuildHistorySyncRequest(oldest, count)
}

func (f *fakeWA) OwnID() waTypes.JID  { return selfPN }
func (f *fakeWA) OwnLID() waTypes.JID { return selfLID }
func (f *fakeWA) PushName() string    { return "Robin Hale" }

func (f *fakeWA) Contact(_ context.Context, jid waTypes.JID) (waTypes.ContactInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.contacts[jid], nil
}

func (f *fakeWA) Contacts(context.Context) (map[waTypes.JID]waTypes.ContactInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[waTypes.JID]waTypes.ContactInfo{}
	for k, v := range f.contacts {
		out[k] = v
	}
	return out, nil
}

func (f *fakeWA) LIDForPN(_ context.Context, pn waTypes.JID) (waTypes.JID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lids[pn], nil
}

func (f *fakeWA) PNForLID(_ context.Context, lid waTypes.JID) (waTypes.JID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for pn, l := range f.lids {
		if l == lid {
			return pn, nil
		}
	}
	return waTypes.EmptyJID, nil
}

func (f *fakeWA) Logout(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loggedOut = true
	return nil
}

func (f *fakeWA) Disconnect() {}

func (f *fakeWA) sentMessages() []sentMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentMsg(nil), f.sent...)
}

type harness struct {
	t    *testing.T
	w    *WhatsApp
	rec  *recorder
	wa   *fakeWA
	deps backend.Deps
	dir  string
}

// newHarness is a linked, connected account with the test's contacts, its
// store in a temporary folder.
func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessIn(t, t.TempDir())
}

func newHarnessIn(t *testing.T, dir string) *harness {
	t.Helper()
	HistoryWait = 50 * time.Millisecond
	lookupSettle = time.Millisecond
	rec := &recorder{}
	events := backend.NewEvents(rec.add)
	idStore, err := ids.Open(filepath.Join(dir, "ids.json"))
	if err != nil {
		t.Fatal(err)
	}
	msgs := ids.NewMessages()
	deps := backend.Deps{
		Events: events, IDs: idStore, Messages: msgs, Files: ids.NewFiles(),
		Outbox: backend.NewOutbox(events, msgs), Session: session.New(dir, proto.WhatsApp),
	}
	w := newWhatsApp(deps)
	w.now = func() time.Time { return base }
	w.dial = func(int, *wastore.Device) error {
		t.Error("the test tried to connect to WhatsApp")
		return errNotConnected
	}
	st, err := w.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	// Registered after the store's close, so it runs before it.
	t.Cleanup(func() { w.Close() })
	dev := st.container.NewDevice()
	dev.ID = &waTypes.JID{User: selfPN.User, Server: selfPN.Server, Device: 7}
	dev.LID = waTypes.JID{User: selfLID.User, Server: selfLID.Server, Device: 7}
	dev.PushName = "Robin Hale"
	wa := &fakeWA{
		real: whatsmeow.NewClient(dev, waLog.Noop),
		contacts: map[waTypes.JID]waTypes.ContactInfo{
			alicePN: {Found: true, FullName: "Alice Example", FirstName: "Alice"},
			benLID:  {Found: true, PushName: "Ben"},
		},
		lids:   map[waTypes.JID]waTypes.JID{},
		groups: map[waTypes.JID]*waTypes.GroupInfo{},
		onWA:   map[string]waTypes.JID{},
		files:  map[string][]byte{},
	}
	h := &harness{t: t, w: w, rec: rec, wa: wa, deps: deps, dir: dir}
	w.mu.Lock()
	w.st, w.cli = st, wa
	w.setSelf(dev)
	w.loadKept(context.Background())
	w.mu.Unlock()
	w.becameReady(w.gen)
	rec.reset()
	return h
}

// restart is the next run of the helper on the same data folder: the ids
// are kept (the server writes them before every line), the rest is read
// back from the store.
func (h *harness) restart() *harness {
	h.t.Helper()
	if err := h.deps.IDs.Flush(); err != nil {
		h.t.Fatal(err)
	}
	return newHarnessIn(h.t, h.dir)
}

// event hands the backend a whatsmeow event of the current connection.
func (h *harness) event(evt any) {
	h.t.Helper()
	h.w.mu.Lock()
	gen := h.w.gen
	h.w.mu.Unlock()
	if !h.w.onEvent(gen, evt) {
		h.t.Fatal("an event wasn't handled")
	}
}

// text is a text message from sender in chat.
func text(chat, sender waTypes.JID, id string, min int, body string) *events.Message {
	evt := &events.Message{Message: &waE2E.Message{Conversation: gproto.String(body)}}
	evt.Info.Chat, evt.Info.Sender, evt.Info.ID, evt.Info.Timestamp = chat, sender, id, at(min)
	evt.Info.IsGroup = chat.Server == waTypes.GroupServer
	evt.Info.IsFromMe = sender.User == selfPN.User || sender.User == selfLID.User
	return evt
}

// withAlt gives a message's sender their other id, as WhatsApp sends it.
func withAlt(evt *events.Message, alt waTypes.JID) *events.Message {
	evt.Info.SenderAlt = alt
	return evt
}

// chat is the backend's chat with key.
func (h *harness) chat(key string) *chat {
	h.w.mu.Lock()
	defer h.w.mu.Unlock()
	return h.w.chats[h.w.resolve(key)]
}

// load sends every chat, as tuimeta's load_chats would.
func (h *harness) load() {
	h.t.Helper()
	for range 20 {
		more, err := h.w.LoadChats(context.Background(), 50)
		if err != nil {
			h.t.Fatal(err)
		}
		if !more {
			return
		}
	}
	h.t.Fatal("load_chats never ended")
}

// ref is a request's reference to message id (WhatsApp's) in c.
func (h *harness) ref(c *chat, id string) backend.MessageRef {
	h.t.Helper()
	h.w.mu.Lock()
	m := c.msgs[id]
	h.w.mu.Unlock()
	if m == nil || len(m.ids) == 0 {
		h.t.Fatalf("message %s unknown", id)
	}
	return backend.MessageRef{Chat: backend.ChatRef{ID: c.id, Network: proto.WhatsApp, NetID: c.key}, ID: m.ids[0], NetID: id, Count: 1, IDs: m.ids}
}

func (h *harness) userID(key string) int64 {
	h.w.mu.Lock()
	defer h.w.mu.Unlock()
	return h.w.person(key).id
}
