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
// a random pause, and panics when asked to search for "panic".
type stub struct {
	backend.Unavailable
	deps   backend.Deps
	chatID int64
	closed atomic.Bool
}

func (s *stub) Start(context.Context) {
	s.Events.Account(s.Net, proto.Ready, s.deps.IDs.User(s.Net, "me"), "Me", "")
}

func (s *stub) Close() { s.closed.Store(true) }

func (s *stub) History(ctx context.Context, _ backend.ChatRef, q history.Query) (history.Page, error) {
	time.Sleep(time.Duration(rand.IntN(20)) * time.Millisecond)
	return history.Page{Messages: []proto.Message{}}, nil
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
	h := &harness{t: t, srv: srv, stdin: inW, done: make(chan struct{}), stub: st, ig: ig, logBuf: logBuf}
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
	if v := wiretest.Field[int](t, first, "version"); v != proto.Version {
		t.Errorf("version = %d", v)
	}
	nets := wiretest.Field[[]string](t, first, "networks")
	if strings.Join(nets, ",") != "messenger,instagram" {
		t.Errorf("networks = %v", nets)
	}
	// Then each network's account.
	h.c.Event("account", 1, func(l wiretest.Line) bool { return wiretest.Field[string](t, l, "network") == "messenger" })
	h.c.Event("account", 1, func(l wiretest.Line) bool { return wiretest.Field[string](t, l, "network") == "instagram" })
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
