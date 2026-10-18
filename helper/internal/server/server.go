// SPDX-License-Identifier: AGPL-3.0-or-later

// Package server speaks the protocol on stdin/stdout: it reads requests,
// handles each on its own goroutine, routes them to the network's backend,
// and writes responses and events through one writer.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/download"
	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
	"github.com/erictran308/tuimeta/helper/internal/session"
)

// Config is how the helper was started.
type Config struct {
	DataDir string
	// HelperVersion goes in the hello line.
	HelperVersion string
	// FilesDir is the folder under DataDir downloads go to ("files", or
	// "fake" in fake mode).
	FilesDir string
}

// Server is one run of the helper.
type Server struct {
	cfg Config
	out *writer

	IDs       *ids.Store
	Messages  *ids.Messages
	Files     *ids.Files
	Events    *backend.Events
	Outbox    *backend.Outbox
	Downloads *download.Manager

	mu       sync.Mutex
	backends map[proto.Network]backend.Backend
	ctx      context.Context

	// loading has one lock per network: its load_chats calls run one at a
	// time, and a network that's stuck holds back only its own.
	loading map[proto.Network]*sync.Mutex
	// feeding lines up each network's load_feed and load_post calls, apart
	// from its chat loads: a feed load never waits on the chat list, nor it
	// on the feed.
	feeding map[proto.Network]*turns
	// commenting lines up each network's load_comments calls the same way.
	commenting map[proto.Network]*turns
	// posting is each network's newest load_post call, which the next one
	// calls off.
	postingMu sync.Mutex
	posting   map[proto.Network]*running
	// calls are the calls cancel can call off, by id, while they run.
	callsMu sync.Mutex
	calls   map[uint64]*running
	// posts are the networks that gave each post load_feed answered with.
	postsMu sync.Mutex
	posts   map[int64]proto.Network

	// seq numbers requests in the order they came.
	seq atomic.Uint64

	typingMu sync.Mutex
	typingIn map[int64]*typingOrder // by chat
}

// New sets up a run writing to stdout. The hello line is written first,
// before anything else can be.
func New(cfg Config, store *ids.Store, stdout io.Writer) *Server {
	s := &Server{
		cfg:        cfg,
		IDs:        store,
		Messages:   ids.NewMessages(),
		Files:      ids.NewFiles(),
		backends:   map[proto.Network]backend.Backend{},
		ctx:        context.Background(),
		loading:    map[proto.Network]*sync.Mutex{},
		feeding:    map[proto.Network]*turns{},
		commenting: map[proto.Network]*turns{},
		posting:    map[proto.Network]*running{},
		calls:      map[uint64]*running{},
		posts:      map[int64]proto.Network{},
		typingIn:   map[int64]*typingOrder{},
	}
	for _, n := range proto.Networks {
		s.loading[n] = &sync.Mutex{}
		s.feeding[n] = newTurns()
		s.commenting[n] = newTurns()
	}
	s.out = newWriter(stdout, proto.NewHello(cfg.HelperVersion), func() {
		if err := store.Flush(); err != nil {
			hlog.Error("can't save ids", hlog.Kind(err))
		}
	})
	s.Events = backend.NewEvents(s.out.send)
	s.Outbox = backend.NewOutbox(s.Events, s.Messages)
	s.Downloads = download.New(filepath.Join(cfg.DataDir, cfg.FilesDir), s.Files, s.fetcher, s.Events.File)
	return s
}

// Deps is what a backend for network n is given.
func (s *Server) Deps(n proto.Network) backend.Deps {
	return backend.Deps{
		Events:    s.Events,
		IDs:       s.IDs,
		Messages:  s.Messages,
		Files:     s.Files,
		Outbox:    s.Outbox,
		Session:   session.New(s.cfg.DataDir, n),
		Downloads: s.Downloads,
	}
}

// Add plugs in a network's backend.
func (s *Server) Add(b backend.Backend) {
	s.mu.Lock()
	s.backends[b.Network()] = b
	s.mu.Unlock()
}

func (s *Server) backend(n proto.Network) backend.Backend {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.backends[n]
}

func (s *Server) fetcher(n proto.Network) download.Fetch {
	b := s.backend(n)
	if b == nil || s.Events.State(n) == proto.LoggedOut {
		return nil
	}
	return b.Fetch
}

// Run starts the backends and serves requests from stdin until it ends or
// ctx is cancelled (a signal), then disconnects every account quietly and
// returns, within two seconds.
func (s *Server) Run(ctx context.Context, stdin io.Reader) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()
	for _, n := range proto.Networks {
		if b := s.backend(n); b != nil {
			b.Start(ctx)
		}
	}
	hlog.Info("serving")

	ended := make(chan struct{})
	go func() {
		defer close(ended)
		err := readLines(stdin, MaxLine, s.handle, func() {
			s.reply(nil, nil, proto.Err(proto.BadRequest, "That request line is longer than 16 MiB."))
		})
		if err != nil {
			hlog.Warn("stdin failed", hlog.Kind(err))
		}
	}()
	select {
	case <-ended:
		hlog.Info("stdin closed; quitting")
	case <-ctx.Done():
		hlog.Info("signalled; quitting")
	}
	s.shutdown(cancel)
}

// shutdown ends requests, disconnects every backend without telling the
// networks anything, and writes what's queued.
func (s *Server) shutdown(cancel context.CancelFunc) {
	cancel()
	var wg sync.WaitGroup
	for _, n := range proto.Networks {
		if b := s.backend(n); b != nil {
			wg.Add(1)
			hlog.Go("close "+string(n), func() {
				defer wg.Done()
				b.Close()
			})
		}
	}
	closed := make(chan struct{})
	go func() { wg.Wait(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(1200 * time.Millisecond):
		hlog.Warn("a backend took too long to close")
	}
	s.Events.Close()
	s.Downloads.Close(150 * time.Millisecond)
	s.out.close(300 * time.Millisecond)
	if err := s.IDs.Flush(); err != nil {
		hlog.Error("can't save ids", hlog.Kind(err))
	}
}

type request struct {
	ID     *uint64         `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type response struct {
	ID     *uint64      `json:"id"`
	Result any          `json:"result,omitempty"`
	Error  *proto.Error `json:"error,omitempty"`
}

// call is one request being handled. A handler sets then to do more once
// its answer has been written (a send waits for the network after
// answering with temporary ids).
type call struct {
	ctx    context.Context
	method string
	params json.RawMessage
	then   func()
	// seq is the request's place in the order requests came: each is
	// served on its own goroutine, so they can reach a network in another.
	seq uint64
	// turn is a load_feed's, load_post's or load_comments' place in its
	// network's line, taken as the request was read.
	turn *turn
	// end, if set, is run once the handler is through.
	end func()
}

func (s *Server) handle(line []byte) {
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		var idOnly struct {
			ID *uint64 `json:"id"`
		}
		if json.Unmarshal(line, &idOnly) != nil {
			idOnly.ID = nil // a half-read id is no id
		}
		s.reply(idOnly.ID, nil, proto.Err(proto.BadRequest, "That line isn't a JSON request."))
		return
	}
	if req.ID == nil {
		s.reply(nil, nil, proto.Err(proto.BadRequest, "The request has no id."))
		return
	}
	if req.Method == "" {
		s.reply(req.ID, nil, proto.Err(proto.BadRequest, "The request has no method."))
		return
	}
	h, ok := handlers[req.Method]
	if !ok {
		s.reply(req.ID, nil, proto.Err(proto.UnknownMethod, "This helper doesn't know that request; update tuimeta-helper."))
		return
	}
	s.mu.Lock()
	ctx := s.ctx
	s.mu.Unlock()
	c := &call{ctx: ctx, method: req.Method, params: req.Params, seq: s.seq.Add(1)}
	switch req.Method {
	case "load_feed":
		c.turn = s.feedTurn(req.Params)
	case "load_post":
		c.turn = s.feedTurn(req.Params)
		c.ctx, c.end = s.newestPost(ctx, req.Params)
	case "load_comments":
		c.turn = s.commentTurn(req.Params)
	case "cancel":
		// As it's read: the call it names was read before it, and one read
		// after it can't be the one it means.
		s.callOff(req.Params)
	}
	if cancellable[req.Method] {
		s.track(*req.ID, c)
	}
	go s.serve(req.ID, h, c)
}

// cancellable are the requests cancel can call off: the loads, which only
// read, so one stopped halfway leaves nothing half done.
var cancellable = map[string]bool{"load_feed": true, "load_post": true, "load_comments": true}

// running is a call that can be called off.
type running struct {
	cancel context.CancelCauseFunc
}

// errCalledOff is what a call that cancel called off is answered.
var errCalledOff = proto.Err(proto.Cancelled, "tuimeta stopped waiting for it.")

// track lets cancel call c off by its id until it's through.
func (s *Server) track(id uint64, c *call) {
	ctx, cancel := context.WithCancelCause(c.ctx)
	c.ctx = ctx
	mine := &running{cancel: cancel}
	s.callsMu.Lock()
	s.calls[id] = mine
	s.callsMu.Unlock()
	end := c.end
	c.end = func() {
		s.callsMu.Lock()
		if s.calls[id] == mine {
			delete(s.calls, id)
		}
		s.callsMu.Unlock()
		cancel(nil)
		if end != nil {
			end()
		}
	}
}

// callOff calls off the call a cancel names, as the cancel is read. One
// that's through, or that can't be called off, is left alone.
func (s *Server) callOff(params json.RawMessage) {
	p, err := decode[struct {
		ID *uint64 `json:"id"`
	}](params)
	if err != nil || p.ID == nil {
		return
	}
	s.callsMu.Lock()
	if r := s.calls[*p.ID]; r != nil {
		r.cancel(errCalledOff)
	}
	s.callsMu.Unlock()
}

// cancelCall is cancel, whose work callOff did as it was read: here only
// that it named a call.
func (s *Server) cancelCall(c *call) (any, error) {
	p, err := decode[struct {
		ID *uint64 `json:"id"`
	}](c.params)
	if err != nil {
		return nil, err
	}
	if p.ID == nil {
		return nil, proto.Err(proto.BadRequest, "id is missing: say which request to call off.")
	}
	return nil, nil
}

// calledOff is why the call was called off, by cancel or a newer call of
// its kind, if it was.
func calledOff(ctx context.Context) *proto.Error {
	var pe *proto.Error
	if errors.As(context.Cause(ctx), &pe) && pe.Code == proto.Cancelled {
		return pe
	}
	return nil
}

func (s *Server) serve(id *uint64, h handler, c *call) {
	answered := false
	defer func() {
		if v := recover(); v != nil {
			hlog.Recovered(c.method, v)
			if !answered {
				s.reply(id, nil, proto.ErrInternal)
			}
		}
	}()
	if c.end != nil {
		defer c.end()
	}
	result, err := h(s, c)
	// One called off is answered so, whatever came of it: nobody waits for
	// what it found any more.
	if pe := calledOff(c.ctx); pe != nil {
		result, err = nil, pe
	}
	if err != nil {
		s.reply(id, nil, s.wireError(c.method, err))
	} else {
		if result == nil {
			result = struct{}{}
		}
		s.reply(id, result, nil)
	}
	answered = true
	if err == nil && c.then != nil {
		c.then()
	}
}

func (s *Server) reply(id *uint64, result any, err *proto.Error) {
	s.out.send(response{ID: id, Result: result, Error: err})
}

// wireError is what tuimeta is told: a *proto.Error as it is, anything else
// as internal, since a library's error text may hold what must not leak.
func (s *Server) wireError(method string, err error) *proto.Error {
	var pe *proto.Error
	if errors.As(err, &pe) {
		if pe.Code == proto.Internal {
			hlog.Error("request failed", hlog.Str("method", method), hlog.Kind(err))
		}
		return pe
	}
	if errors.Is(err, context.Canceled) {
		return proto.Err(proto.Internal, "The helper is quitting.")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return proto.Err(proto.NetworkError, "That took too long; try again.")
	}
	hlog.Error("request failed", hlog.Str("method", method), hlog.Kind(err))
	return proto.ErrInternal
}
