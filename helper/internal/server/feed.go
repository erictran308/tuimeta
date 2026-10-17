// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// Limits on load_feed's limit, which is only a hint: the network gives
// pages of its own size.
const (
	DefaultFeedLimit = 10
	MaxFeedLimit     = 50
)

func feedLimit(n int) int {
	if n <= 0 {
		return DefaultFeedLimit
	}
	return min(n, MaxFeedLimit)
}

// MaxBoxSide is the most a side of load_feed's box counts as, in pixels:
// past any screen's width.
const MaxBoxSide = 8192

// feedBox is load_feed's width and height as a box; none unless both are
// above 0.
func feedBox(width, height int) backend.Box {
	if width <= 0 || height <= 0 {
		return backend.Box{}
	}
	return backend.Box{Width: min(width, MaxBoxSide), Height: min(height, MaxBoxSide)}
}

// turns lets one network's feed loads through one at a time, in the order
// they were asked. Each request is served on its own goroutine, which may
// start after a later one's, so the place in line is taken while requests
// are still read one by one (handle), and kept by waiting for the call
// before it.
type turns struct {
	mu   sync.Mutex
	last chan struct{} // closed once the newest call in line is done
}

func newTurns() *turns {
	done := make(chan struct{})
	close(done)
	return &turns{last: done}
}

// take gives a call its place: it may go once before is closed, and must
// call done (once is enough; more are harmless) when it's through.
func (t *turns) take() (before <-chan struct{}, done func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	prev, mine := t.last, make(chan struct{})
	t.last = mine
	var once sync.Once
	return prev, func() { once.Do(func() { close(mine) }) }
}

// feedTurn is the place in its network's line of a load_feed request,
// taken as it's read; nil for one that doesn't name a network, which is
// refused without waiting.
func (s *Server) feedTurn(params json.RawMessage) *turn {
	p, err := decode[struct {
		Network string `json:"network"`
	}](params)
	if err != nil {
		return nil
	}
	t, ok := s.feeding[proto.Network(p.Network)]
	if !ok {
		return nil
	}
	before, done := t.take()
	return &turn{before: before, done: done}
}

// turn is a call's place in a line.
type turn struct {
	before <-chan struct{}
	done   func()
}

// wait holds the call until those before it are done. A call given up
// while waiting (the helper quitting) still lets the next one go only
// after the one before it.
func (t *turn) wait(ctx context.Context) error {
	select {
	case <-t.before:
		return nil
	case <-ctx.Done():
		go func() {
			<-t.before
			t.done()
		}()
		return ctx.Err()
	}
}

func (s *Server) loadFeed(c *call) (any, error) {
	if c.turn != nil {
		// Given up while waiting, wait lets the next one go in its turn.
		if err := c.turn.wait(c.ctx); err != nil {
			return nil, err
		}
		defer c.turn.done()
	}
	p, err := decode[struct {
		Network string `json:"network"`
		Limit   int    `json:"limit"`
		Fresh   bool   `json:"fresh"`
		Width   int    `json:"width"`
		Height  int    `json:"height"`
		Feed    string `json:"feed"`
	}](c.params)
	if err != nil {
		return nil, err
	}
	kind, ok := backend.ParseFeedKind(p.Feed)
	if !ok {
		return nil, proto.Err(proto.BadRequest, "feed is for_you or following.")
	}
	n, b, err := s.network(p.Network)
	if err != nil {
		return nil, err
	}
	f, ok := b.(backend.Feeder)
	if !ok {
		return nil, proto.Err(proto.Unsupported, n.Title()+" has no feed in tuimeta.")
	}
	if err := s.ready(n); err != nil {
		return nil, err
	}
	posts, more, err := f.LoadFeed(c.ctx, feedLimit(p.Limit), p.Fresh, feedBox(p.Width, p.Height), kind)
	if err != nil {
		return nil, err
	}
	raw, more, err := fitList(posts, more, PageBudget)
	if err != nil {
		return nil, err
	}
	s.notePosts(n, posts[:len(raw)])
	return map[string]any{"posts": raw, "has_more": more}, nil
}

// notePosts keeps which network gave the posts, so load_comments, which
// names only a post, goes to it; a post it didn't give is not_found.
func (s *Server) notePosts(n proto.Network, posts []proto.Post) {
	s.postsMu.Lock()
	defer s.postsMu.Unlock()
	for _, p := range posts {
		s.posts[p.ID] = n
	}
}

// postNetwork is the network that gave post id, if one did.
func (s *Server) postNetwork(id int64) (proto.Network, bool) {
	s.postsMu.Lock()
	defer s.postsMu.Unlock()
	n, ok := s.posts[id]
	return n, ok
}

// forgetPosts forgets the network's posts (logout): their comments can't
// be asked for any more.
func (s *Server) forgetPosts(n proto.Network) {
	s.postsMu.Lock()
	defer s.postsMu.Unlock()
	for id, net := range s.posts {
		if net == n {
			delete(s.posts, id)
		}
	}
}

// commentTurn is a load_comments request's place in its network's line,
// taken as it's read; nil for one naming no post this run gave, which is
// refused without waiting.
func (s *Server) commentTurn(params json.RawMessage) *turn {
	p, err := decode[struct {
		PostID int64 `json:"post_id"`
	}](params)
	if err != nil {
		return nil
	}
	n, ok := s.postNetwork(p.PostID)
	if !ok {
		return nil
	}
	before, done := s.commenting[n].take()
	return &turn{before: before, done: done}
}

var errNoPost = proto.Err(proto.NotFound, "That post isn't loaded; load the feed again.")

func (s *Server) loadComments(c *call) (any, error) {
	if c.turn != nil {
		if err := c.turn.wait(c.ctx); err != nil {
			return nil, err
		}
		defer c.turn.done()
	}
	p, err := decode[struct {
		PostID    int64 `json:"post_id"`
		RepliesOf int64 `json:"replies_of"`
		More      bool  `json:"more"`
	}](c.params)
	if err != nil {
		return nil, err
	}
	if p.PostID <= 0 {
		return nil, proto.Err(proto.BadRequest, "post_id is missing.")
	}
	if p.RepliesOf < 0 {
		return nil, proto.Err(proto.BadRequest, "replies_of isn't a comment id.")
	}
	n, ok := s.postNetwork(p.PostID)
	if !ok {
		return nil, errNoPost
	}
	b := s.backend(n)
	cm, ok := b.(backend.Commenter)
	if !ok {
		return nil, proto.Err(proto.Unsupported, n.Title()+"'s posts have no comments in tuimeta.")
	}
	if err := s.ready(n); err != nil {
		return nil, err
	}
	comments, more, err := cm.LoadComments(c.ctx, p.PostID, p.RepliesOf, p.More)
	if err != nil {
		return nil, err
	}
	raw, more, err := fitList(comments, more, PageBudget)
	if err != nil {
		return nil, err
	}
	return map[string]any{"comments": raw, "has_more": more}, nil
}

// fitList is a page's posts or comments as they go on the wire, cut short
// to fit budget bytes, with has_more then true. Each is small (a capped
// text and at most twenty media), so this is a guard, not something a page
// reaches: what's cut is past the network's cursor and doesn't come again
// until a fresh first page. At least one stays; MaxOutLine still holds.
func fitList[T any](items []T, more bool, budget int) ([]json.RawMessage, bool, error) {
	raw := make([]json.RawMessage, 0, len(items))
	total := 0
	for _, p := range items {
		b, err := json.Marshal(p)
		if err != nil {
			return nil, false, err
		}
		if len(raw) > 0 && total+len(b)+1 > budget {
			return raw, true, nil
		}
		raw = append(raw, b)
		total += len(b) + 1
	}
	return raw, more, nil
}
