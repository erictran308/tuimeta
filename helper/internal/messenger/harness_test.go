// SPDX-License-Identifier: AGPL-3.0-or-later

package messenger

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	armadillo "go.mau.fi/whatsmeow/proto"
	"go.mau.fi/whatsmeow/proto/waMediaTransport"
	"go.mau.fi/whatsmeow/proto/waMsgApplication"
	waTypes "go.mau.fi/whatsmeow/types"

	"go.mau.fi/mautrix-meta/pkg/messagix/httpclient"
	"go.mau.fi/mautrix-meta/pkg/messagix/socket"
	"go.mau.fi/mautrix-meta/pkg/messagix/table"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
	"github.com/erictran308/tuimeta/helper/internal/session"
)

// The account tests log in as, and people and threads they talk to. Every
// id and name here is made up.
const (
	selfID  int64 = 100001
	aliceID int64 = 100002
	benID   int64 = 100003
	groupID int64 = 900001
)

// base is a fixed "now" for tests: 2026-10-08 12:00 UTC.
var base = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func ms(min int) int64 { return base.Add(time.Duration(min) * time.Minute).UnixMilli() }

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

func (r *recorder) of(kind string) []any {
	var out []any
	for _, v := range r.all() {
		switch v.(type) {
		case proto.MessageSentEvent:
			if kind == "message_sent" {
				out = append(out, v)
			}
		case proto.MessageFailedEvent:
			if kind == "message_failed" {
				out = append(out, v)
			}
		case proto.MessageDeletedEvent:
			if kind == "message_deleted" {
				out = append(out, v)
			}
		case proto.ReadEvent:
			if kind == "read" {
				out = append(out, v)
			}
		case proto.TypingEvent:
			if kind == "typing" {
				out = append(out, v)
			}
		case proto.AccountEvent:
			if kind == "account" {
				out = append(out, v)
			}
		case proto.ChatRemovedEvent:
			if kind == "chat_removed" {
				out = append(out, v)
			}
		case proto.ErrorEvent:
			if kind == "error" {
				out = append(out, v)
			}
		}
	}
	return out
}

// fakeMeta is a Messenger connection that records every task and answers
// with tables the test sets up. It never touches the network.
type fakeMeta struct {
	mu        sync.Mutex
	tasks     []socket.Task
	stateless []socket.Task
	uploads   []*httpclient.MercuryUploadMedia
	answer    func(tasks []socket.Task) (*table.LSTable, error)
	pages     []*table.LSTable // FetchMoreThreads answers, in order
	posted    int
	loggedOut bool
	cookies   map[string]string
	closed    bool
}

func (f *fakeMeta) ExecuteTasks(_ context.Context, tasks ...socket.Task) (*table.LSTable, error) {
	f.mu.Lock()
	f.tasks = append(f.tasks, tasks...)
	answer := f.answer
	f.mu.Unlock()
	if answer != nil {
		return answer(tasks)
	}
	return &table.LSTable{}, nil
}

func (f *fakeMeta) ExecuteStatelessTask(_ context.Context, task socket.Task) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stateless = append(f.stateless, task)
	return nil
}

func (f *fakeMeta) FetchMoreThreads(context.Context, int64) (*socket.KeyStoreData, *table.LSTable, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pages) == 0 {
		return nil, nil, nil
	}
	page := f.pages[0]
	f.pages = f.pages[1:]
	ks := &socket.KeyStoreData{HasMoreBefore: len(f.pages) > 0, MinThreadKey: int64(len(f.pages) + 1)}
	return ks, page, nil
}

func (f *fakeMeta) Upload(_ context.Context, _ int64, media *httpclient.MercuryUploadMedia) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads = append(f.uploads, media)
	return int64(5000 + len(f.uploads)), nil
}

func (f *fakeMeta) Cursor(int64) string                                   { return "cursor" }
func (f *fakeMeta) WaitUntilCanSend(context.Context, time.Duration) error { return nil }
func (f *fakeMeta) PostHandle(*table.LSTable)                             { f.mu.Lock(); f.posted++; f.mu.Unlock() }
func (f *fakeMeta) Logout(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loggedOut = true
	return nil
}
func (f *fakeMeta) Cookies() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cookies
}
func (f *fakeMeta) Disconnect() { f.mu.Lock(); f.closed = true; f.mu.Unlock() }

func (f *fakeMeta) sent() []socket.Task {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]socket.Task(nil), f.tasks...)
}

// fakeE2EE is an encrypted chats' connection that records calls.
type fakeE2EE struct {
	mu        sync.Mutex
	sent      []sentFB
	reads     []readCall
	presences []waTypes.ChatPresence
	files     map[string][]byte
	group     *waTypes.GroupInfo
	failSend  error
}

type sentFB struct {
	to   waTypes.JID
	msg  armadillo.RealMessageApplicationSub
	meta *waMsgApplication.MessageApplication_Metadata
	id   string
}

type readCall struct {
	ids    []waTypes.MessageID
	chat   waTypes.JID
	sender waTypes.JID
}

var fakeOwnJID = waTypes.JID{User: strconv.FormatInt(selfID, 10), Device: 7, Server: waTypes.MessengerServer}

func (f *fakeE2EE) SendFBMessage(_ context.Context, to waTypes.JID, msg armadillo.RealMessageApplicationSub, meta *waMsgApplication.MessageApplication_Metadata, extra whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSend != nil {
		return whatsmeow.SendResponse{}, f.failSend
	}
	f.sent = append(f.sent, sentFB{to, msg, meta, extra.ID})
	return whatsmeow.SendResponse{ID: extra.ID, Timestamp: base.Add(time.Minute)}, nil
}

func (f *fakeE2EE) MarkRead(_ context.Context, ids []waTypes.MessageID, _ time.Time, chat, sender waTypes.JID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, readCall{ids, chat, sender})
	return nil
}

func (f *fakeE2EE) SendChatPresence(_ context.Context, _ waTypes.JID, state waTypes.ChatPresence, _ waTypes.ChatPresenceMedia) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.presences = append(f.presences, state)
	return nil
}

func (f *fakeE2EE) DownloadFB(_ context.Context, t *waMediaTransport.WAMediaTransport_Integral, _ whatsmeow.MediaType) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.files[t.GetDirectPath()]
	if !ok {
		return nil, errors.New("no such file")
	}
	return data, nil
}

func (f *fakeE2EE) Upload(_ context.Context, data []byte, _ whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	return whatsmeow.UploadResponse{DirectPath: "/up/1", FileSHA256: []byte{1, 2}, FileEncSHA256: []byte{3}, MediaKey: []byte{4}, FileLength: uint64(len(data))}, nil
}

func (f *fakeE2EE) GetGroupInfo(context.Context, waTypes.JID) (*waTypes.GroupInfo, error) {
	if f.group == nil {
		return nil, errors.New("no group")
	}
	return f.group, nil
}

func (f *fakeE2EE) OwnJID() waTypes.JID { return fakeOwnJID }
func (f *fakeE2EE) Disconnect()         {}

// harness is a logged-in backend with fake connections.
type harness struct {
	t    *testing.T
	m    *Messenger
	rec  *recorder
	meta *fakeMeta
	e2ee *fakeE2EE
	deps backend.Deps
	dir  string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	store, err := ids.Open(filepath.Join(dir, "ids.json"))
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	events := backend.NewEvents(rec.add)
	msgs := ids.NewMessages()
	deps := backend.Deps{
		Events: events, IDs: store, Messages: msgs, Files: ids.NewFiles(),
		Outbox: backend.NewOutbox(events, msgs), Session: session.New(dir, proto.Messenger),
	}
	m := newMessenger(deps)
	m.now = func() time.Time { return base }
	m.dial = func(context.Context, context.Context, map[string]string) error {
		t.Error("the test tried to connect to Facebook")
		return errNetwork
	}
	h := &harness{t: t, m: m, rec: rec, meta: &fakeMeta{cookies: map[string]string{"c_user": "100001", "xs": "x", "datr": "d"}}, e2ee: &fakeE2EE{files: map[string][]byte{}}, deps: deps, dir: dir}
	m.mu.Lock()
	m.meta = h.meta
	m.e2ee = h.e2ee
	m.self = selfID
	m.selfName = "Robin Hale"
	m.ready = true
	m.sess = &savedSession{Version: 1, UserID: selfID, Cookies: map[string]string{"c_user": "100001"}}
	p := m.person(selfID)
	p.name, p.known = "Robin Hale", true
	m.mu.Unlock()
	h.people()
	return h
}

// people makes the test's contacts known, as the initial sync would.
func (h *harness) people() {
	h.apply(&table.LSTable{LSDeleteThenInsertContact: []*table.LSDeleteThenInsertContact{
		{Id: aliceID, Name: "Alice Example", SecondaryName: "alice.example", ProfilePictureUrl: "https://scontent.xx.fbcdn.net/v/t1/alice.jpg?oh=sig"},
		{Id: benID, Name: "Ben Carter"},
	}})
	h.rec.reset()
}

// apply applies a table as if it came on the socket.
func (h *harness) apply(tbl *table.LSTable) {
	h.m.mu.Lock()
	h.m.applyTable(tbl, fromSocket)
	h.m.mu.Unlock()
}

// chat is the backend's chat with key.
func (h *harness) chat(key int64) *chat {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	return h.m.lookupChat(key)
}

func (h *harness) userID(fbid int64) int64 {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	return h.m.person(fbid).id
}

// load sends every chat, as tuimeta's load_chats would.
func (h *harness) load() {
	h.t.Helper()
	for range 20 {
		more, err := h.m.LoadChats(context.Background(), 50)
		if err != nil {
			h.t.Fatal(err)
		}
		if !more {
			return
		}
	}
	h.t.Fatal("load_chats never ended")
}

// ref is a message request's reference to the part with id in chat.
func (h *harness) ref(c *chat, id int64) backend.MessageRef {
	h.t.Helper()
	p, ok := h.deps.Messages.Lookup(c.id, id)
	if !ok {
		h.t.Fatalf("message %d unknown", id)
	}
	return backend.MessageRef{Chat: backend.ChatRef{ID: c.id, Network: proto.Messenger, NetID: c.netID()}, ID: id, NetID: p.NetID, Index: p.Index, Count: p.Count, IDs: p.IDs}
}

// dmThread is a one-to-one thread with alice, read up to readMin.
func dmThread(key int64, lastMin, readMin int, unread int64) *table.LSDeleteThenInsertThread {
	return &table.LSDeleteThenInsertThread{
		ThreadKey: key, ThreadType: table.ONE_TO_ONE, FolderName: "inbox",
		LastActivityTimestampMs: ms(lastMin), LastReadWatermarkTimestampMs: ms(readMin), UnreadMessageCount: unread,
	}
}

func groupThread(key int64, name string, lastMin int) *table.LSDeleteThenInsertThread {
	return &table.LSDeleteThenInsertThread{
		ThreadKey: key, ThreadType: table.GROUP_THREAD, FolderName: "inbox", ThreadName: name,
		LastActivityTimestampMs: ms(lastMin), LastReadWatermarkTimestampMs: ms(lastMin),
	}
}

// textMsg is a message as Messenger's tables carry it.
func textMsg(thread int64, id string, sender int64, min int, text string) *table.LSInsertMessage {
	return &table.LSInsertMessage{ThreadKey: thread, MessageId: id, SenderId: sender, TimestampMs: ms(min), Text: text}
}
