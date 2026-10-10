// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"encoding/json"
	"io"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/browser"
	"github.com/erictran308/tuimeta/helper/internal/cookies"
	"github.com/erictran308/tuimeta/helper/internal/history"
	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
	"github.com/erictran308/tuimeta/helper/internal/wiretest"
)

// stub is a backend that's logged in from the start, answers history after
// a random pause (or with pages, when set), panics when asked to search for
// "panic", and records what it's asked to send and type. Its LoadChats waits
// for loads, when set.
type stub struct {
	backend.Unavailable
	deps   backend.Deps
	chatID int64
	closed atomic.Bool
	pages  func(history.Query) history.Page
	loads  chan struct{}
	inLoad chan struct{}

	mu    sync.Mutex
	sent  []*backend.Outgoing
	typed []bool
}

func (s *stub) Start(context.Context) {
	s.Events.Account(s.Net, proto.Ready, s.deps.IDs.User(s.Net, "me"), "Me", "")
}

func (s *stub) Close() { s.closed.Store(true) }

func (s *stub) History(ctx context.Context, _ backend.ChatRef, q history.Query) (history.Page, error) {
	if s.pages != nil {
		return s.pages(q), nil
	}
	time.Sleep(time.Duration(rand.IntN(20)) * time.Millisecond)
	return history.Page{Messages: []proto.Message{}}, nil
}

func (s *stub) LoadChats(ctx context.Context, _ int) (bool, error) {
	if s.loads != nil {
		s.inLoad <- struct{}{}
		select {
		case <-s.loads:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	return false, nil
}

func (s *stub) Send(_ context.Context, out *backend.Outgoing) error {
	s.mu.Lock()
	s.sent = append(s.sent, out)
	s.mu.Unlock()
	return nil
}

func (s *stub) SetTyping(_ context.Context, _ backend.ChatRef, typing bool) error {
	s.mu.Lock()
	s.typed = append(s.typed, typing)
	s.mu.Unlock()
	return nil
}

func (s *stub) sends() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}

func (s *stub) typings() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.typed)
}

func (s *stub) Search(_ context.Context, q string) ([]proto.SearchResult, error) {
	if q == "panic" {
		var m map[string]int
		m["secret message text"] = 1 // a nil-map write: the panic's value mustn't be logged
	}
	return []proto.SearchResult{{Title: "found", Kind: proto.DM}}, nil
}

// loginRecorder is a logged-out network that records the browser each
// login it's handed names.
type loginRecorder struct {
	backend.Unavailable
	mu     sync.Mutex
	logins []browser.Identity
}

func (l *loginRecorder) LoginCookies(_ context.Context, _ cookies.Set, as browser.Identity) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.logins = append(l.logins, as)
	return nil
}

// linkRecorder is a linking network that remembers what it was asked and
// shows one code per link.
type linkRecorder struct {
	backend.Unavailable
	mu       sync.Mutex
	phones   []string
	cancels  int
	accepted chan struct{}
}

func (l *linkRecorder) Link(ctx context.Context, phone string, attempt uint64) error {
	l.mu.Lock()
	l.phones = append(l.phones, phone)
	l.mu.Unlock()
	if phone == "" {
		l.Events.LoginCode(l.Net, attempt, "2@qr,code", "", time.Unix(1700000060, 0))
	} else {
		l.Events.LoginCode(l.Net, attempt, "", "ABCD-EFGH", time.Unix(1700000180, 0))
	}
	select {
	case <-l.accepted:
		return nil
	case <-ctx.Done():
		return proto.Err(proto.Cancelled, "Linking was cancelled.")
	}
}

func (l *linkRecorder) CancelLink(uint64) {
	l.mu.Lock()
	l.cancels++
	l.mu.Unlock()
}

func (l *linkRecorder) asked() ([]string, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.phones), l.cancels
}

func (l *loginRecorder) named() []browser.Identity {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.logins)
}

type harness struct {
	t      *testing.T
	srv    *Server
	c      *wiretest.Client
	stdin  *io.PipeWriter
	done   chan struct{}
	stub   *stub
	ig     *loginRecorder
	wa     *linkRecorder
	logBuf *syncBuffer
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func start(t *testing.T) *harness {
	t.Helper()
	return startWith(t, nil)
}

// startWith is start with setup run before the server does (to put other
// backends in the stand-ins' place, or give the stub pages).
func startWith(t *testing.T, setup func(*harness)) *harness {
	t.Helper()
	dir := t.TempDir()
	logBuf := &syncBuffer{}
	hlog.SetOutput(logBuf)
	store, err := ids.Open(filepath.Join(dir, "ids.json"))
	if err != nil {
		t.Fatal(err)
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	srv := New(Config{DataDir: dir, HelperVersion: "test", FilesDir: "files"}, store, outW)
	st := &stub{Unavailable: backend.Unavailable{Net: proto.Messenger, Events: srv.Events}, deps: srv.Deps(proto.Messenger)}
	st.chatID = store.Chat(proto.Messenger, "thread-1")
	srv.Add(st)
	ig := &loginRecorder{Unavailable: backend.Unavailable{Net: proto.Instagram, Events: srv.Events}}
	srv.Add(ig)
	wa := &linkRecorder{Unavailable: backend.Unavailable{Net: proto.WhatsApp, Events: srv.Events}, accepted: make(chan struct{}, 4)}
	srv.Add(wa)
	h := &harness{t: t, srv: srv, stdin: inW, done: make(chan struct{}), stub: st, ig: ig, wa: wa, logBuf: logBuf}
	if setup != nil {
		setup(h)
	}
	h.c = wiretest.New(t, inW, outR)
	go func() {
		srv.Run(context.Background(), inR)
		outW.Close()
		close(h.done)
	}()
	t.Cleanup(func() {
		inW.Close()
		<-h.done
	})
	return h
}

func TestTheFirstLineIsHello(t *testing.T) {
	h := start(t)
	first := h.c.Wait("a line", 0, func(wiretest.Line) bool { return true })
	if first.Seq != 0 || first.Event != "hello" {
		t.Fatalf("first line = %s", first.Raw)
	}
	// Version 4: send_files names what tuimeta checked of each file.
	if v := wiretest.Field[int](t, first, "version"); v != 4 || proto.Version != 4 {
		t.Errorf("version = %d", v)
	}
	nets := wiretest.Field[[]string](t, first, "networks")
	if strings.Join(nets, ",") != "messenger,instagram,whatsapp" {
		t.Errorf("networks = %v", nets)
	}
	// Then each network's account.
	for _, n := range nets {
		h.c.Event("account", 1, func(l wiretest.Line) bool { return wiretest.Field[string](t, l, "network") == n })
	}
}

func TestWhatsAppLinksWithACodeAndNeverTakesCookies(t *testing.T) {
	h := start(t)
	r := h.c.Call("login_cookies", map[string]any{"network": "whatsapp", "cookies": "c_user=1; xs=2; datr=3"})
	if r.Error == nil || r.Error.Code != proto.BadRequest {
		t.Fatalf("cookies for whatsapp: %s", r.Raw)
	}
	r = h.c.Call("login_link", map[string]any{"network": "instagram"})
	if r.Error == nil || r.Error.Code != proto.BadRequest {
		t.Fatalf("linking instagram: %s", r.Raw)
	}
	for _, bad := range []string{"0123 456 789", "+1 555", "+44 20 7946 0958 0000 1", "555-CALL-NOW", ""} {
		r = h.c.Call("login_link", map[string]any{"network": "whatsapp", "phone": bad})
		if r.Error == nil || r.Error.Code != proto.BadRequest {
			t.Fatalf("phone %q: %s", bad, r.Raw)
		}
	}
	if phones, _ := h.wa.asked(); len(phones) != 0 {
		t.Fatalf("the network was asked to link %v", phones)
	}

	h.wa.accepted <- struct{}{}
	id := h.c.Request("login_link", map[string]any{"network": "whatsapp", "phone": "+1 (555) 010-0100"})
	code := h.c.Event("login_code", 0, func(l wiretest.Line) bool { return wiretest.Field[string](t, l, "network") == "whatsapp" })
	if got := wiretest.Field[string](t, code, "pairing"); got != "ABCD-EFGH" {
		t.Errorf("pairing = %q", got)
	}
	if got := wiretest.Field[int64](t, code, "expires"); got != 1700000180 {
		t.Errorf("expires = %d", got)
	}
	if r := h.c.Response(id); r.Error != nil {
		t.Fatalf("got %s", r.Raw)
	}

	h.wa.accepted <- struct{}{}
	id = h.c.Request("login_link", map[string]any{"network": "whatsapp"})
	code = h.c.Event("login_code", 0, func(l wiretest.Line) bool { return strings.Contains(l.Raw, `"qr":"2@qr,code"`) })
	if strings.Contains(code.Raw, "pairing") {
		t.Errorf("a QR code came with a pairing code: %s", code.Raw)
	}
	h.c.Response(id)
	if r := h.c.Call("cancel_login", map[string]any{"network": "whatsapp"}); r.Error != nil {
		t.Fatalf("got %s", r.Raw)
	}
	phones, cancels := h.wa.asked()
	if !slices.Equal(phones, []string{"15550100100", ""}) || cancels != 1 {
		t.Errorf("asked to link %q, cancelled %d times", phones, cancels)
	}
	if strings.Contains(h.logBuf.String(), "5550100") {
		t.Error("the phone number reached the log")
	}
}

func TestMalformedLinesAreAnsweredAndReadingGoesOn(t *testing.T) {
	h := start(t)
	from := h.c.Mark()
	h.c.Send(`this is not json`)
	l := h.c.Wait("bad_request for garbage", from, func(l wiretest.Line) bool { return l.IsResponse })
	if l.ID != nil || l.Error == nil || l.Error.Code != proto.BadRequest {
		t.Fatalf("garbage got %s", l.Raw)
	}
	h.c.Send(`{"method":"search","params":{}}`)
	h.c.Send(`{"id":"seven","method":"search"}`)
	for range 2 {
		from = l.Seq + 1
		l = h.c.Wait("bad_request without an id", from, func(l wiretest.Line) bool { return l.IsResponse })
		if l.ID != nil || l.Error == nil || l.Error.Code != proto.BadRequest {
			t.Fatalf("got %s", l.Raw)
		}
	}
	h.c.Send(`{"id":5,"params":{}}`)
	if l := h.c.Response(5); l.Error == nil || l.Error.Code != proto.BadRequest {
		t.Fatalf("no method: %s", l.Raw)
	}
	h.c.Send(`{"id":6,"method":"history","params":{"chat_id":"twelve"}}`)
	if l := h.c.Response(6); l.Error == nil || l.Error.Code != proto.BadRequest {
		t.Fatalf("bad params: %s", l.Raw)
	}
	h.c.Send(`{"id":7,"method":"history","params":{"chat_id":1,"before":1,"after":2}}`)
	if l := h.c.Response(7); l.Error == nil || l.Error.Code != proto.BadRequest {
		t.Fatalf("before and after: %s", l.Raw)
	}
	// Still serving.
	if l := h.c.Call("search", map[string]any{"network": "messenger", "query": "x"}); l.Error != nil {
		t.Fatalf("search after bad lines: %s", l.Raw)
	}
}

func TestALineOverTheLimitIsSkipped(t *testing.T) {
	h := start(t)
	from := h.c.Mark()
	h.c.Send(`{"id":1,"method":"search","params":{"query":"` + strings.Repeat("a", MaxLine) + `"}}`)
	l := h.c.Wait("bad_request for the long line", from, func(l wiretest.Line) bool { return l.IsResponse })
	if l.ID != nil || l.Error == nil || l.Error.Code != proto.BadRequest {
		t.Fatalf("long line got %s", l.Raw)
	}
	if l := h.c.Call("search", map[string]any{"network": "messenger", "query": "x"}); l.Error != nil {
		t.Fatalf("search after the long line: %s", l.Raw)
	}
}

func TestAnUnknownMethodSaysSo(t *testing.T) {
	h := start(t)
	l := h.c.Call("fly_to_the_moon", map[string]any{"chat_id": 1})
	if l.Error == nil || l.Error.Code != proto.UnknownMethod {
		t.Fatalf("got %s", l.Raw)
	}
}

func TestConcurrentRequestsEachGetOneResponse(t *testing.T) {
	h := start(t)
	const n = 300
	sent := map[uint64]bool{}
	for i := range n {
		var id uint64
		switch i % 3 {
		case 0:
			id = h.c.Request("history", map[string]any{"chat_id": h.stub.chatID, "limit": 5})
		case 1:
			id = h.c.Request("search", map[string]any{"network": "messenger", "query": "q"})
		default:
			id = h.c.Request("no_such_method", nil)
		}
		sent[id] = true
	}
	for id := range sent {
		h.c.Response(id)
	}
	time.Sleep(100 * time.Millisecond) // room for a duplicate to show up
	got := map[uint64]int{}
	for _, l := range h.c.Lines() {
		if l.IsResponse && l.ID != nil {
			got[*l.ID]++
		}
	}
	for id := range sent {
		if got[id] != 1 {
			t.Errorf("request %d got %d responses", id, got[id])
		}
	}
	if len(got) != n {
		t.Errorf("%d responses for %d requests", len(got), n)
	}
}

func TestAPanicInAHandlerAnswersInternalWithoutItsValue(t *testing.T) {
	h := start(t)
	l := h.c.Call("search", map[string]any{"network": "messenger", "query": "panic"})
	if l.Error == nil || l.Error.Code != proto.Internal {
		t.Fatalf("got %s", l.Raw)
	}
	log := h.logBuf.String()
	if !strings.Contains(log, "panic") || !strings.Contains(log, "stub).Search") {
		t.Errorf("the log doesn't say where it panicked:\n%s", log)
	}
	if strings.Contains(log, "secret message text") || strings.Contains(l.Raw, "secret") {
		t.Errorf("the panic's value leaked:\n%s", log)
	}
	if l := h.c.Call("search", map[string]any{"network": "messenger", "query": "fine"}); l.Error != nil {
		t.Fatalf("after the panic: %s", l.Raw)
	}
}

func TestRequestsToALoggedOutNetworkAreRefused(t *testing.T) {
	h := start(t)
	l := h.c.Call("load_chats", map[string]any{"network": "instagram", "limit": 10})
	if l.Error == nil || l.Error.Code != proto.NotLoggedIn {
		t.Fatalf("got %s", l.Raw)
	}
	l = h.c.Call("history", map[string]any{"chat_id": 999, "limit": 10})
	if l.Error == nil || l.Error.Code != proto.NotFound {
		t.Fatalf("unknown chat: %s", l.Raw)
	}
	l = h.c.Call("search", map[string]any{"network": "myspace", "query": "x"})
	if l.Error == nil || l.Error.Code != proto.BadRequest {
		t.Fatalf("unknown network: %s", l.Raw)
	}
}

func TestALoginNamingAnythingButChromeIsRefusedBeforeTheNetworkHearsOfIt(t *testing.T) {
	h := start(t)
	const igCookies = "sessionid=1; ds_user_id=2; csrftoken=3"
	r := h.c.Call("login_cookies", map[string]any{"network": "instagram", "cookies": igCookies, "browser": "Safari 18.6"})
	if r.Error == nil || r.Error.Code != proto.BadRequest {
		t.Fatalf("got %s", r.Raw)
	}
	if len(h.ig.named()) != 0 {
		t.Fatal("the network was asked to log in")
	}
	if r := h.c.Call("login_cookies", map[string]any{"network": "instagram", "cookies": igCookies, "browser": "Chrome 150.0.7712.45"}); r.Error != nil {
		t.Fatalf("got %s", r.Raw)
	}
	if r := h.c.Call("login_cookies", map[string]any{"network": "instagram", "cookies": igCookies}); r.Error != nil {
		t.Fatalf("got %s", r.Raw)
	}
	got := h.ig.named()
	if len(got) != 2 || got[0].Name() != "Chrome 150.0.7712.45" || !got[1].IsDefault() {
		t.Errorf("logins named %v", got)
	}
}

func TestStdinEndingClosesBackendsAndReturnsQuickly(t *testing.T) {
	h := start(t)
	h.c.Call("search", map[string]any{"network": "messenger", "query": "x"})
	began := time.Now()
	h.stdin.Close()
	select {
	case <-h.done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run didn't return within 2 s of stdin closing")
	}
	if !h.stub.closed.Load() {
		t.Error("the backend wasn't closed")
	}
	if d := time.Since(began); d > 2*time.Second {
		t.Errorf("took %v", d)
	}
	if !h.c.Ended(time.Second) {
		t.Error("stdout wasn't closed")
	}
}

func TestErrorsFromLibrariesNeverReachTheWire(t *testing.T) {
	s := &Server{}
	leaky := &json.SyntaxError{} // any non-proto error
	if got := s.wireError("x", leaky); got.Code != proto.Internal || got != proto.ErrInternal {
		t.Errorf("got %+v", got)
	}
	mine := proto.Err(proto.NotFound, "No.")
	if got := s.wireError("x", mine); got != mine {
		t.Errorf("got %+v", got)
	}
}

// readyNet is a network logged in from the start whose chats load at once.
type readyNet struct {
	backend.Unavailable
	deps backend.Deps
}

func (r *readyNet) Start(context.Context) {
	r.Events.Account(r.Net, proto.Ready, r.deps.IDs.User(r.Net, "me"), "Me", "")
}

func (r *readyNet) LoadChats(context.Context, int) (bool, error) { return false, nil }

func TestOneNetworksStuckChatListDoesntHoldBackAnothers(t *testing.T) {
	h := startWith(t, func(h *harness) {
		h.stub.loads = make(chan struct{})
		h.stub.inLoad = make(chan struct{}, 1)
		h.srv.Add(&readyNet{Unavailable: backend.Unavailable{Net: proto.Instagram, Events: h.srv.Events}, deps: h.srv.Deps(proto.Instagram)})
	})
	h.c.Event("account", 0, func(l wiretest.Line) bool {
		return wiretest.Field[string](t, l, "network") == "instagram" && wiretest.Field[string](t, l, "state") == "ready"
	})
	stuck := h.c.Request("load_chats", map[string]any{"network": "messenger", "limit": 10})
	<-h.stub.inLoad // Messenger's load is under way, and stays stuck
	from := h.c.Mark()
	ig := h.c.Request("load_chats", map[string]any{"network": "instagram", "limit": 10})
	l, ok := h.c.WaitFor(2*time.Second, from, func(l wiretest.Line) bool { return l.IsResponse && l.ID != nil && *l.ID == ig })
	if !ok {
		t.Fatal("Instagram's chats waited for Messenger's")
	}
	if l.Error != nil {
		t.Fatalf("got %s", l.Raw)
	}
	// Messenger's own next load waits its turn.
	second := h.c.Request("load_chats", map[string]any{"network": "messenger", "limit": 10})
	if _, ok := h.c.WaitFor(100*time.Millisecond, from, func(l wiretest.Line) bool { return l.IsResponse && l.ID != nil && *l.ID == second }); ok {
		t.Error("two of Messenger's loads ran at once")
	}
	h.stub.loads <- struct{}{}
	<-h.stub.inLoad
	h.stub.loads <- struct{}{}
	for _, id := range []uint64{stuck, second} {
		if r := h.c.Response(id); r.Error != nil {
			t.Fatalf("got %s", r.Raw)
		}
	}
}

func TestATypingStartThatArrivesAfterItsStopIsDropped(t *testing.T) {
	h := start(t)
	h.c.Event("account", 0, func(l wiretest.Line) bool {
		return wiretest.Field[string](t, l, "network") == "messenger" && wiretest.Field[string](t, l, "state") == "ready"
	})
	typing := func(seq uint64, on bool) *call {
		params, _ := json.Marshal(map[string]any{"chat_id": h.stub.chatID, "typing": on})
		return &call{ctx: context.Background(), method: "typing", params: params, seq: seq}
	}
	// Numbered as they came: one letter typed and deleted (a start, then a
	// stop), then another.
	var seq [4]uint64
	for i := range seq {
		seq[i] = h.srv.seq.Add(1)
	}
	// Each is served on its own goroutine, and the first stop got there
	// before its start.
	for _, c := range []*call{typing(seq[1], false), typing(seq[0], true), typing(seq[2], true), typing(seq[3], false)} {
		if _, err := h.srv.typing(c); err != nil {
			t.Fatal(err)
		}
	}
	if got := h.stub.typings(); !slices.Equal(got, []bool{false, true, false}) {
		t.Errorf("the network was told %v", got)
	}
	// Over the wire, in order.
	if l := h.c.Call("typing", map[string]any{"chat_id": h.stub.chatID, "typing": true}); l.Error != nil {
		t.Fatalf("got %s", l.Raw)
	}
	if got := h.stub.typings(); len(got) != 4 || !got[3] {
		t.Errorf("the network was told %v", got)
	}
}
