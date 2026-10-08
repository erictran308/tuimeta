// SPDX-License-Identifier: AGPL-3.0-or-later

package instagram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/mautrix-meta/pkg/instameow"
	"go.mau.fi/mautrix-meta/pkg/instameow/slidetypes"
	mcookies "go.mau.fi/mautrix-meta/pkg/messagix/cookies"
	"go.mau.fi/mautrix-meta/pkg/messagix/types"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/browser"
	"github.com/erictran308/tuimeta/helper/internal/cookies"
	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
	"github.com/erictran308/tuimeta/helper/internal/session"
)

// Everything here is made up: no test talks to Instagram, and the "recorded"
// answers below are hand-written in the shape instameow parses.

const (
	selfFBID  = 1000
	mayaFBID  = 2002
	samFBID   = 2003
	noorFBID  = 2004
	dealsFBID = 2007

	mayaIGID = "34000000002"
	samIGID  = "34000000003"
	noorIGID = "34000000004"

	dmIGID     = "340282366841710301244276017723107508377"
	dmLong     = "340282366841710300949128132112345678"
	groupKey   = "8123456789012345"
	groupLong  = "340282366841710300949128132198765432"
	reqIGID    = "340282366841710301244276017723100000007"
	reqLong    = "340282366841710300949128132100000007"
	tsBase     = 1759912345678 // ms
	cdn        = "https://scontent-lhr8-1.cdninstagram.com"
	otherCDN   = "https://scontent-ams2-1.cdninstagram.com"
	cookieText = "sessionid=s3ss10n; ds_user_id=1000; csrftoken=csrf; mid=mid1; rur=rur1; other=dropped"
)

func user(fbid int64, igid, username, name string) string {
	return fmt.Sprintf(`{"interop_messaging_user_fbid":"%d","id":"%s","username":"%s","full_name":"%s","profile_pic_url":"%s/v/t51.2885-19/%s_n.jpg?stp=dst&oh=sig1"}`,
		fbid, igid, username, name, cdn, igid)
}

var (
	selfJSON = user(selfFBID, "34000000001", "robin.hale", "Robin Hale")
	mayaJSON = user(mayaFBID, mayaIGID, "maya.lens", "Maya Lopez")
	samJSON  = user(samFBID, samIGID, "sam.climbs", "Sam Rivera")
	noorJSON = user(noorFBID, noorIGID, "noor.draws", "Noor Aziz")
)

// textMsg is a text message's JSON.
func textMsg(id string, from int64, ms int64, text string) string {
	return fmt.Sprintf(`{"id":"%s","message_id":"%s","sender_fbid":"%d","timestamp_ms":"%d","offline_threading_id":"7%d",
		"content":{"__typename":"SlideMessageText","text_body":%q},"text_body":%q,"reactions":[]}`,
		id, id, from, ms, ms, text, text)
}

// thread is a thread's JSON; messages newest first, as Instagram sends them.
func thread(igid, key, long string, group bool, title string, users []string, lastMS int64, messages []string, cursor string, more bool, extra string) string {
	edges := make([]string, len(messages))
	for i, m := range messages {
		edges[i] = `{"node":` + m + `}`
	}
	if extra != "" {
		extra = "," + extra
	}
	return fmt.Sprintf(`{"id":"%s","thread_fbid":"%s","thread_key":"%s","thread_id":"%s","is_group":%v,"thread_title":%q,
		"viewer":%s,"users":[%s],"last_activity_timestamp_ms":"%d","system_folder":"INBOX","input_mode":0,"is_muted":false,
		"slide_messages":{"edges":[%s],"page_info":{"end_cursor":"%s","has_next_page":%v}}%s}`,
		igid, igid, key, long, group, title, selfJSON, strings.Join(users, ","), lastMS, strings.Join(edges, ","), cursor, more, extra)
}

func dmThread(messages ...string) string {
	return thread(dmIGID, fmt.Sprint(mayaFBID), dmLong, false, "", []string{mayaJSON}, tsBase, messages, "dm-older", true, "")
}

func groupThread(messages ...string) string {
	return thread(groupKey, groupKey, groupLong, true, "Climbing crew 🧗", []string{mayaJSON, samJSON, noorJSON}, tsBase-60_000, messages, "g-older", true, "")
}

func decode[T any](t testing.TB, js string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(js), &v); err != nil {
		t.Fatalf("fixture doesn't parse: %v\n%s", err, js)
	}
	return v
}

func threadInfo(t testing.TB, js string) *slidetypes.ThreadInfo {
	return decode[*slidetypes.ThreadInfo](t, js)
}

func message(t testing.TB, js string) *slidetypes.Message {
	return decode[*slidetypes.Message](t, js)
}

func delta(t testing.TB, typename, threadIGID, body string) *slidetypes.Delta {
	t.Helper()
	if body != "" {
		body = "," + body
	}
	js := fmt.Sprintf(`{"__typename":%q,"uq_seq_id":"101","thread_fbid":%q%s}`, typename, threadIGID, body)
	return decode[*slidetypes.Delta](t, js)
}

// mailbox is an inbox page holding the given threads.
func mailbox(t testing.TB, cursor string, more bool, threads ...string) *slidetypes.Mailbox {
	edges := make([]string, len(threads))
	for i, th := range threads {
		edges[i] = `{"node":{"id":"x","as_ig_direct_thread":` + th + `}}`
	}
	return decode[*slidetypes.Mailbox](t, fmt.Sprintf(`{"iris_inactive_subscription_uq_seq_id":"100",
		"threads_by_folder":{"edges":[%s],"page_info":{"end_cursor":"%s","has_next_page":%v}}}`, strings.Join(edges, ","), cursor, more))
}

// call is one request the stand-in got.
type call struct {
	name string
	req  any
}

// fakeAPI stands in for instameow: it answers from fixtures and records
// every request, so tests can check what would have gone to Instagram.
type fakeAPI struct {
	t       testing.TB
	mu      sync.Mutex
	calls   []call
	handler instameow.EventHandler
	cookies *mcookies.Cookies
	as      browser.Identity // the browser the client was made to say it is

	viewer        *types.PolarisViewer
	inbox         *slidetypes.Mailbox
	loadErr       error
	authenticated bool
	fbid          int64
	onConnect     []slidetypes.ClientEvent

	threads      map[string]*slidetypes.ThreadInfo // GetThread, by thread id
	pages        map[string]*slidetypes.MessagesOnlyThread
	inboxPages   map[string]*slidetypes.MailboxPage
	sendText     func(*slidetypes.SendTextRequest) (*slidetypes.SendTextResponse, error)
	sendMedia    func(*slidetypes.SendMediaRequest) (*slidetypes.SendMediaResponse, error)
	threadIDs    map[int64]*instameow.ThreadIGIDs
	nextUpload   int64
	reactionResp *slidetypes.SendReactionResponse
	search       *slidetypes.SearchResponse
}

func newFakeAPI(t testing.TB) *fakeAPI {
	return &fakeAPI{
		t:             t,
		viewer:        &types.PolarisViewer{ID: "34000000001", Data: types.ViewerData{FullName: "Robin Hale", Username: "robin.hale", Fbid: "17841400000000001"}},
		authenticated: true,
		fbid:          selfFBID,
		onConnect:     []slidetypes.ClientEvent{&slidetypes.Connected{SubscribedSeqID: 100, LatestSeqID: 100}},
		threads:       map[string]*slidetypes.ThreadInfo{},
		pages:         map[string]*slidetypes.MessagesOnlyThread{},
		inboxPages:    map[string]*slidetypes.MailboxPage{},
		threadIDs:     map[int64]*instameow.ThreadIGIDs{},
		nextUpload:    5550001,
	}
}

func (f *fakeAPI) record(name string, req any) {
	f.mu.Lock()
	f.calls = append(f.calls, call{name, req})
	f.mu.Unlock()
}

// names lists the requests made, in order.
func (f *fakeAPI) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.name
	}
	return out
}

func (f *fakeAPI) count(name string) int {
	n := 0
	for _, c := range f.names() {
		if c == name {
			n++
		}
	}
	return n
}

// last is the newest request of that name.
func (f *fakeAPI) last(name string) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].name == name {
			return f.calls[i].req
		}
	}
	return nil
}

func (f *fakeAPI) emit(evt slidetypes.ClientEvent) error {
	return f.handler(context.Background(), evt)
}

func (f *fakeAPI) LoadIndex(ctx context.Context) (*types.PolarisViewer, *slidetypes.Mailbox, error) {
	f.record("LoadIndex", nil)
	if f.loadErr != nil {
		return nil, nil, f.loadErr
	}
	inbox := f.inbox
	if inbox == nil {
		inbox = mailbox(f.t, "", false)
	}
	return f.viewer, inbox, nil
}
func (f *fakeAPI) GetOwnFBID() int64             { return f.fbid }
func (f *fakeAPI) IsAuthenticated() bool         { return f.authenticated }
func (f *fakeAPI) GetCookies() *mcookies.Cookies { return f.cookies }
func (f *fakeAPI) Disconnect()                   { f.record("Disconnect", nil) }
func (f *fakeAPI) Connect(ctx context.Context) {
	f.record("Connect", nil)
	for _, e := range f.onConnect {
		_ = f.handler(ctx, e)
	}
}
func (f *fakeAPI) GetMailbox(ctx context.Context) (*slidetypes.MailboxResponse, error) {
	f.record("GetMailbox", nil)
	return &slidetypes.MailboxResponse{Mailbox: f.inbox}, nil
}
func (f *fakeAPI) PaginateMailbox(ctx context.Context, req *slidetypes.PaginateMailboxRequest) (*slidetypes.PaginateMailboxResponse, error) {
	f.record("PaginateMailbox", req)
	page, ok := f.inboxPages[req.Cursor]
	if !ok {
		return nil, errors.New("no such inbox page")
	}
	return &slidetypes.PaginateMailboxResponse{Mailbox: page}, nil
}
func (f *fakeAPI) GetThread(ctx context.Context, req *slidetypes.GetThreadInfoRequest) (*slidetypes.ThreadInfoResponse, error) {
	f.record("GetThread", req)
	t, ok := f.threads[req.ThreadFBID]
	if !ok {
		return nil, instameow.ErrThreadNotFound
	}
	return &slidetypes.ThreadInfoResponse{ThreadInfo: slidetypes.WrappedThreadInfo{AsIGDirectThread: t, ID: t.ID}}, nil
}
func (f *fakeAPI) PaginateMessages(ctx context.Context, req *slidetypes.PaginateMessagesRequest) (*slidetypes.PaginateMessagesResponse, error) {
	f.record("PaginateMessages", req)
	key := ""
	switch {
	case req.AfterCursor != nil:
		key = *req.AfterCursor
	case req.OlderThanMessageID != nil:
		key = "older:" + *req.OlderThanMessageID
	}
	page, ok := f.pages[key]
	if !ok {
		return nil, errors.New("no such page")
	}
	return &slidetypes.PaginateMessagesResponse{ThreadInfo: slidetypes.AsThread[slidetypes.MessagesOnlyThread]{AsIGDirectThread: page}}, nil
}
func (f *fakeAPI) FetchThreadID(ctx context.Context, fbid int64) (*instameow.ThreadIGIDs, error) {
	f.record("FetchThreadID", fbid)
	if ids, ok := f.threadIDs[fbid]; ok {
		return ids, nil
	}
	return nil, instameow.ErrThreadNotFound
}
func (f *fakeAPI) GetUserForNewDM(ctx context.Context, fbid int64) (*slidetypes.UserInfoResponse, error) {
	f.record("GetUserForNewDM", fbid)
	return &slidetypes.UserInfoResponse{Data: decode[*slidetypes.User](f.t, user(fbid, "34000000099", "new.person", "New Person"))}, nil
}
func (f *fakeAPI) SearchUsers(ctx context.Context, q string) (*slidetypes.SearchResponse, error) {
	f.record("SearchUsers", q)
	return f.search, nil
}
func (f *fakeAPI) SendMessage(ctx context.Context, req *slidetypes.SendTextRequest) (*slidetypes.SendTextResponse, error) {
	f.record("SendMessage", req)
	if f.sendText != nil {
		return f.sendText(req)
	}
	return &slidetypes.SendTextResponse{Message: sentMessage("mid.sent."+req.OfflineThreadingID, tsBase+600_000)}, nil
}
func (f *fakeAPI) SendMedia(ctx context.Context, req *slidetypes.SendMediaRequest) (*slidetypes.SendMediaResponse, error) {
	f.record("SendMedia", req)
	if f.sendMedia != nil {
		return f.sendMedia(req)
	}
	return &slidetypes.SendMediaResponse{Message: sentMessage("mid.media."+req.AttachmentFBID, tsBase+500_000)}, nil
}
func (f *fakeAPI) EditMessage(ctx context.Context, req *slidetypes.EditMessageRequest) (*slidetypes.EditMessageResponse, error) {
	f.record("EditMessage", req)
	return &slidetypes.EditMessageResponse{}, nil
}
func (f *fakeAPI) UnsendMessage(ctx context.Context, req *slidetypes.UnsendMessageRequest) (*slidetypes.UnsendMessageResponse, error) {
	f.record("UnsendMessage", req)
	return &slidetypes.UnsendMessageResponse{DirectUnsendMessage: true}, nil
}
func (f *fakeAPI) SendReaction(ctx context.Context, req *slidetypes.CreateReactionRequest) (*slidetypes.SendReactionResponse, error) {
	f.record("SendReaction", req)
	if f.reactionResp != nil {
		return f.reactionResp, nil
	}
	return &slidetypes.SendReactionResponse{}, nil
}
func (f *fakeAPI) MarkRead(ctx context.Context, req *slidetypes.MarkReadRequest) (*slidetypes.MarkReadResponse, error) {
	f.record("MarkRead", req)
	return &slidetypes.MarkReadResponse{}, nil
}
func (f *fakeAPI) MarkReadValidation(ctx context.Context, req *slidetypes.MarkReadRequest) (*slidetypes.MarkReadValidationResponse, error) {
	f.record("MarkReadValidation", req)
	return &slidetypes.MarkReadValidationResponse{Validated: true}, nil
}
func (f *fakeAPI) MuteThread(ctx context.Context, req *slidetypes.MuteThreadRequest) (*slidetypes.MuteThreadResponse, error) {
	f.record("MuteThread", req)
	return &slidetypes.MuteThreadResponse{}, nil
}
func (f *fakeAPI) AcceptMessageRequest(ctx context.Context, req *slidetypes.AcceptMessageRequestRequest) (*slidetypes.AcceptMessageRequestResponse, error) {
	f.record("AcceptMessageRequest", req)
	return &slidetypes.AcceptMessageRequestResponse{}, nil
}
func (f *fakeAPI) SetTyping(ctx context.Context, threadID string, typing bool) error {
	f.record("SetTyping", []any{threadID, typing})
	return nil
}
func (f *fakeAPI) Upload(ctx context.Context, threadKey int64, name, mime string, data []byte, voice bool) (int64, error) {
	f.record("Upload", name)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextUpload++
	return f.nextUpload, nil
}

func sentMessage(id string, ms int64) slidetypes.SentMessage {
	var sm slidetypes.SentMessage
	sm.ID, sm.MessageID = id, id
	sm.TimestampMS.Time = time.UnixMilli(ms)
	return sm
}

// recorder keeps the lines the backend writes, as JSON objects.
type recorder struct {
	mu    sync.Mutex
	lines []map[string]any
	raw   []string
}

func (r *recorder) add(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	r.mu.Lock()
	r.lines = append(r.lines, m)
	r.raw = append(r.raw, string(data))
	r.mu.Unlock()
}

// events are the lines with that event name.
func (r *recorder) events(name string) []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []map[string]any
	for _, l := range r.lines {
		if l["event"] == name {
			out = append(out, l)
		}
	}
	return out
}

func (r *recorder) all() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.raw, "\n")
}

func (r *recorder) reset() {
	r.mu.Lock()
	r.lines, r.raw = nil, nil
	r.mu.Unlock()
}

// harness is a backend wired to the stand-in, with real ids, files and
// sessions in a temporary folder.
type harness struct {
	t    *testing.T
	b    *Instagram
	api  *fakeAPI
	rec  *recorder
	dir  string
	deps backend.Deps
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	return newHarnessIn(t, dir)
}

func newHarnessIn(t *testing.T, dir string) *harness {
	t.Helper()
	store, err := ids.Open(filepath.Join(dir, "ids.json"))
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	events := backend.NewEvents(rec.add)
	messages := ids.NewMessages()
	deps := backend.Deps{
		Events: events, IDs: store, Messages: messages, Files: ids.NewFiles(),
		Outbox: backend.NewOutbox(events, messages), Session: session.New(dir, proto.Instagram),
	}
	h := &harness{t: t, api: newFakeAPI(t), rec: rec, dir: dir, deps: deps}
	h.b = newInstagram(deps, func(c *mcookies.Cookies, as browser.Identity, handler instameow.EventHandler) api {
		h.api.handler = handler
		h.api.cookies = c
		h.api.as = as
		return h.api
	})
	h.b.media = &http.Client{Transport: noNetwork{t}}
	t.Cleanup(h.b.Close)
	return h
}

// noNetwork fails a test that tries to download anything.
type noNetwork struct{ t testing.TB }

func (n noNetwork) RoundTrip(req *http.Request) (*http.Response, error) {
	n.t.Errorf("a test tried to fetch %s", req.URL.Host)
	return nil, errors.New("no network in tests")
}

// login logs in with the inbox given, and forgets the events so far.
func (h *harness) login(inbox *slidetypes.Mailbox) {
	h.t.Helper()
	h.api.inbox = inbox
	set, err := cookies.Parse(proto.Instagram, cookieText)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.b.LoginCookies(context.Background(), set, browser.Default()); err != nil {
		h.t.Fatalf("login: %v", err)
	}
	h.rec.reset()
}

// chatID is the helper's id of a thread key.
func (h *harness) chatID(key any) int64 {
	return h.deps.IDs.Chat(proto.Instagram, fmt.Sprint(key))
}

func (h *harness) userID(fbid int64) int64 {
	return h.deps.IDs.User(proto.Instagram, fmt.Sprint(fbid))
}

func (h *harness) chatRef(key any) backend.ChatRef {
	return backend.ChatRef{ID: h.chatID(key), Network: proto.Instagram, NetID: fmt.Sprint(key)}
}

// msgRef is a message as the server would resolve it.
func (h *harness) msgRef(key any, netID string) backend.MessageRef {
	h.t.Helper()
	chat := h.chatRef(key)
	got, ok := h.deps.Messages.Known(chat.ID, netID)
	if !ok {
		h.t.Fatalf("message %s has no ids", netID)
	}
	p, _ := h.deps.Messages.Lookup(chat.ID, got[0])
	return backend.MessageRef{Chat: chat, ID: got[0], NetID: netID, Index: 0, Count: p.Count, IDs: p.IDs}
}

// convertOne converts a message in a chat made from the thread JSON.
func (h *harness) convertOne(threadJSON, msgJSON string) (*chat, []proto.Message) {
	h.t.Helper()
	h.b.mu.Lock()
	defer h.b.mu.Unlock()
	if h.b.selfFBID == 0 {
		h.b.selfFBID = selfFBID
	}
	c := h.b.upsertThread(threadInfo(h.t, threadJSON), false)
	n := h.b.convert(c, message(h.t, msgJSON))
	if n == nil {
		h.t.Fatal("message didn't convert")
	}
	n = h.b.keep(c, n)
	return c, h.b.parts(c, n)
}

func noCalls(t *testing.T, f *fakeAPI, forbidden ...string) {
	t.Helper()
	for _, name := range f.names() {
		if slices.Contains(forbidden, name) {
			t.Errorf("%s was sent to Instagram (requests: %v)", name, f.names())
		}
	}
}
