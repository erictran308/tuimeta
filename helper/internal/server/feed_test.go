// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/proto"
	"github.com/erictran308/tuimeta/helper/internal/wiretest"
)

// feeder is a ready network with a feed. Each load says which it was by
// its limit, and waits for loads when set; its chat loads wait for chats.
type feeder struct {
	readyNet
	loads  chan struct{}
	chats  chan struct{}
	inLoad chan struct{}

	mu      sync.Mutex
	asked   []int
	fresh   []bool
	boxes   []backend.Box
	running int
	most    int
}

func (f *feeder) LoadFeed(ctx context.Context, limit int, fresh bool, box backend.Box) ([]proto.Post, bool, error) {
	f.mu.Lock()
	f.asked = append(f.asked, limit)
	f.fresh = append(f.fresh, fresh)
	f.boxes = append(f.boxes, box)
	f.running++
	f.most = max(f.most, f.running)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.running--
		f.mu.Unlock()
	}()
	if f.loads != nil {
		select {
		case <-f.loads:
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
	}
	likes := int64(-5)
	return []proto.Post{{ID: int64(limit), Author: proto.FeedAuthor{Username: "maya.lens"}, Caption: "hi",
		Media: []proto.Media{{Kind: proto.Photo, FileID: 3}}, Likes: &likes, URL: "https://www.instagram.com/p/X/"}}, true, nil
}

func (f *feeder) LoadChats(ctx context.Context, _ int) (bool, error) {
	if f.chats != nil {
		f.inLoad <- struct{}{}
		select {
		case <-f.chats:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	return false, nil
}

func (f *feeder) seen() ([]int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.asked...), f.most
}

func withFeeder(t *testing.T, setup func(*feeder)) (*harness, *feeder) {
	t.Helper()
	var fd *feeder
	h := startWith(t, func(h *harness) {
		fd = &feeder{readyNet: readyNet{Unavailable: backend.Unavailable{Net: proto.Instagram, Events: h.srv.Events}, deps: h.srv.Deps(proto.Instagram)}}
		if setup != nil {
			setup(fd)
		}
		h.srv.Add(fd)
	})
	h.c.Event("account", 0, func(l wiretest.Line) bool {
		return wiretest.Field[string](t, l, "network") == "instagram" && wiretest.Field[string](t, l, "state") == "ready"
	})
	return h, fd
}

func TestOnlyANetworkWithAFeedLoadsOne(t *testing.T) {
	h, fd := withFeeder(t, nil)
	l := h.c.Call("load_feed", map[string]any{"network": "instagram", "limit": 7, "fresh": true})
	if l.Error != nil {
		t.Fatalf("got %s", l.Raw)
	}
	page := wiretest.Decode[struct {
		Posts   []map[string]any `json:"posts"`
		HasMore bool             `json:"has_more"`
	}](t, l.Result)
	if len(page.Posts) != 1 || !page.HasMore || page.Posts[0]["id"] != 7.0 || page.Posts[0]["url"] != "https://www.instagram.com/p/X/" {
		t.Errorf("got %s", l.Raw)
	}
	// A count below zero is written as none.
	if page.Posts[0]["likes"] != 0.0 {
		t.Errorf("likes = %v", page.Posts[0]["likes"])
	}
	if asked, _ := fd.seen(); len(asked) != 1 || asked[0] != 7 || !fd.fresh[0] {
		t.Errorf("the network was asked %v %v", asked, fd.fresh)
	}
	// The limit is a hint, kept within bounds.
	h.c.Call("load_feed", map[string]any{"network": "instagram"})
	h.c.Call("load_feed", map[string]any{"network": "instagram", "limit": 5000})
	if asked, _ := fd.seen(); asked[1] != DefaultFeedLimit || asked[2] != MaxFeedLimit {
		t.Errorf("limits %v", asked)
	}
	// The box a picture is drawn in, kept within bounds; none without both
	// sides.
	h.c.Call("load_feed", map[string]any{"network": "instagram", "width": 600, "height": 99999})
	h.c.Call("load_feed", map[string]any{"network": "instagram", "width": 600})
	h.c.Call("load_feed", map[string]any{"network": "instagram", "width": -1, "height": 400})
	fd.mu.Lock()
	boxes := fd.boxes
	fd.mu.Unlock()
	want := []backend.Box{{}, {}, {}, {Width: 600, Height: MaxBoxSide}, {}, {}}
	if !slices.Equal(boxes, want) {
		t.Errorf("boxes %v", boxes)
	}

	// Messenger and WhatsApp have none.
	for _, n := range []string{"messenger", "whatsapp"} {
		if l := h.c.Call("load_feed", map[string]any{"network": n, "limit": 10}); l.Error == nil || l.Error.Code != proto.Unsupported {
			t.Errorf("%s: %s", n, l.Raw)
		}
	}
	for _, params := range []map[string]any{{"limit": 10}, {"network": "myspace"}} {
		if l := h.c.Call("load_feed", params); l.Error == nil || l.Error.Code != proto.BadRequest {
			t.Errorf("%v: %s", params, l.Raw)
		}
	}
	if l := h.c.Call("load_feed", "not an object"); l.Error == nil || l.Error.Code != proto.BadRequest {
		t.Errorf("bad params: %s", l.Raw)
	}
}

func TestALoggedOutNetworksFeedIsRefused(t *testing.T) {
	h := startWith(t, func(h *harness) {
		h.srv.Add(&loggedOutFeeder{Unavailable: backend.Unavailable{Net: proto.Instagram, Events: h.srv.Events}})
	})
	if l := h.c.Call("load_feed", map[string]any{"network": "instagram", "limit": 10}); l.Error == nil || l.Error.Code != proto.NotLoggedIn {
		t.Errorf("got %s", l.Raw)
	}
}

type loggedOutFeeder struct{ backend.Unavailable }

func (*loggedOutFeeder) LoadFeed(context.Context, int, bool, backend.Box) ([]proto.Post, bool, error) {
	panic("a logged-out network was asked for its feed")
}

func TestFeedLoadsRunOneAtATimeInTheOrderAsked(t *testing.T) {
	h, fd := withFeeder(t, func(f *feeder) { f.loads = make(chan struct{}) })
	var ids []uint64
	for i := 1; i <= 6; i++ {
		ids = append(ids, h.c.Request("load_feed", map[string]any{"network": "instagram", "limit": i}))
	}
	for range ids {
		fd.loads <- struct{}{}
	}
	for _, id := range ids {
		if r := h.c.Response(id); r.Error != nil {
			t.Fatalf("got %s", r.Raw)
		}
	}
	asked, most := fd.seen()
	for i, limit := range asked {
		if limit != i+1 {
			t.Fatalf("loads ran in the order %v", asked)
		}
	}
	if most != 1 {
		t.Errorf("%d loads ran at once", most)
	}
}

func TestAFeedLoadNeverWaitsOnTheChatList(t *testing.T) {
	h, fd := withFeeder(t, func(f *feeder) {
		f.chats = make(chan struct{})
		f.inLoad = make(chan struct{}, 1)
	})
	stuck := h.c.Request("load_chats", map[string]any{"network": "instagram", "limit": 10})
	<-fd.inLoad
	from := h.c.Mark()
	feed := h.c.Request("load_feed", map[string]any{"network": "instagram", "limit": 10})
	if _, ok := h.c.WaitFor(2*time.Second, from, func(l wiretest.Line) bool { return l.IsResponse && l.ID != nil && *l.ID == feed }); !ok {
		t.Fatal("the feed waited for the chat list")
	}
	fd.chats <- struct{}{}
	if r := h.c.Response(stuck); r.Error != nil {
		t.Fatalf("got %s", r.Raw)
	}
}

func TestAFeedLoadGivenUpWhileWaitingKeepsTheLine(t *testing.T) {
	tr := newTurns()
	first, second, third := &turn{}, &turn{}, &turn{}
	first.before, first.done = tr.take()
	second.before, second.done = tr.take()
	third.before, third.done = tr.take()
	if err := first.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := second.wait(ctx); err == nil {
		t.Fatal("a cancelled wait went ahead")
	}
	select {
	case <-third.before:
		t.Fatal("the third went while the first still ran")
	case <-time.After(30 * time.Millisecond):
	}
	first.done()
	select {
	case <-third.before:
	case <-time.After(time.Second):
		t.Fatal("the third never went")
	}
}

// commenter is a feeder whose posts have comments: each load answers one
// comment saying what it was asked, waiting for loads when set.
type commenter struct {
	feeder
	cloads chan struct{}

	cmu    sync.Mutex
	casked [][3]int64 // post, replies_of, more (0 or 1)
	crun   int
	cmost  int
}

func (c *commenter) LoadComments(ctx context.Context, post, of int64, more bool) ([]proto.Comment, bool, error) {
	c.cmu.Lock()
	m := int64(0)
	if more {
		m = 1
	}
	c.casked = append(c.casked, [3]int64{post, of, m})
	c.crun++
	c.cmost = max(c.cmost, c.crun)
	c.cmu.Unlock()
	defer func() {
		c.cmu.Lock()
		c.crun--
		c.cmu.Unlock()
	}()
	if c.cloads != nil {
		select {
		case <-c.cloads:
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
	}
	likes := int64(-1)
	return []proto.Comment{{ID: 100 + of, Author: proto.FeedAuthor{Username: "sam", Photo: &proto.Image{FileID: 9}}, Text: "hi", Likes: &likes, Replies: 2, ReplyTo: of}}, true, nil
}

func (c *commenter) asked() ([][3]int64, int) {
	c.cmu.Lock()
	defer c.cmu.Unlock()
	return append([][3]int64(nil), c.casked...), c.cmost
}

func withCommenter(t *testing.T, setup func(*commenter)) (*harness, *commenter) {
	t.Helper()
	var cm *commenter
	h := startWith(t, func(h *harness) {
		cm = &commenter{feeder: feeder{readyNet: readyNet{Unavailable: backend.Unavailable{Net: proto.Instagram, Events: h.srv.Events}, deps: h.srv.Deps(proto.Instagram)}}}
		if setup != nil {
			setup(cm)
		}
		h.srv.Add(cm)
	})
	h.c.Event("account", 0, func(l wiretest.Line) bool {
		return wiretest.Field[string](t, l, "network") == "instagram" && wiretest.Field[string](t, l, "state") == "ready"
	})
	return h, cm
}

func TestCommentsGoToTheNetworkThatGaveThePost(t *testing.T) {
	h, cm := withCommenter(t, nil)
	// Not given by load_feed yet: not found, and nobody is asked.
	if l := h.c.Call("load_comments", map[string]any{"post_id": 7}); l.Error == nil || l.Error.Code != proto.NotFound {
		t.Fatalf("unknown post: %s", l.Raw)
	}
	h.c.Call("load_feed", map[string]any{"network": "instagram", "limit": 7}) // gives post 7
	l := h.c.Call("load_comments", map[string]any{"post_id": 7, "replies_of": 3, "more": true})
	if l.Error != nil {
		t.Fatalf("got %s", l.Raw)
	}
	page := wiretest.Decode[struct {
		Comments []map[string]any `json:"comments"`
		HasMore  bool             `json:"has_more"`
	}](t, l.Result)
	if len(page.Comments) != 1 || !page.HasMore {
		t.Fatalf("got %s", l.Raw)
	}
	c := page.Comments[0]
	author := c["author"].(map[string]any)
	if c["id"] != 103.0 || c["reply_to"] != 3.0 || c["likes"] != 0.0 || c["replies"] != 2.0 || author["photo"] != nil {
		t.Errorf("comment %v", c)
	}
	if asked, _ := cm.asked(); len(asked) != 1 || asked[0] != [3]int64{7, 3, 1} {
		t.Errorf("asked %v", asked)
	}
	for _, params := range []any{map[string]any{}, map[string]any{"post_id": -1}, map[string]any{"post_id": 7, "replies_of": -2}, "x"} {
		if l := h.c.Call("load_comments", params); l.Error == nil || l.Error.Code != proto.BadRequest {
			t.Errorf("%v: %s", params, l.Raw)
		}
	}
	// Logged out, its posts are forgotten.
	h.c.Call("logout", map[string]any{"network": "instagram"})
	if l := h.c.Call("load_comments", map[string]any{"post_id": 7}); l.Error == nil || l.Error.Code != proto.NotFound {
		t.Errorf("after logout: %s", l.Raw)
	}
	if asked, _ := cm.asked(); len(asked) != 1 {
		t.Errorf("asked %v", asked)
	}
}

func TestPostsWithoutCommentsSaySo(t *testing.T) {
	h, _ := withFeeder(t, nil)
	h.c.Call("load_feed", map[string]any{"network": "instagram", "limit": 4})
	if l := h.c.Call("load_comments", map[string]any{"post_id": 4}); l.Error == nil || l.Error.Code != proto.Unsupported {
		t.Errorf("got %s", l.Raw)
	}
}

func TestCommentLoadsRunOneAtATimeInTheOrderAsked(t *testing.T) {
	h, cm := withCommenter(t, func(c *commenter) { c.cloads = make(chan struct{}) })
	h.c.Call("load_feed", map[string]any{"network": "instagram", "limit": 7})
	var ids []uint64
	for i := 1; i <= 6; i++ {
		ids = append(ids, h.c.Request("load_comments", map[string]any{"post_id": 7, "replies_of": i}))
	}
	for range ids {
		cm.cloads <- struct{}{}
	}
	for _, id := range ids {
		if r := h.c.Response(id); r.Error != nil {
			t.Fatalf("got %s", r.Raw)
		}
	}
	asked, most := cm.asked()
	for i, a := range asked {
		if a[1] != int64(i+1) {
			t.Fatalf("loads ran in the order %v", asked)
		}
	}
	if most != 1 {
		t.Errorf("%d loads ran at once", most)
	}
}

func TestAFeedPageTooBigForALineIsCutAndSaysThereIsMore(t *testing.T) {
	post := func(id int64) proto.Post {
		return proto.Post{ID: id, Caption: strings.Repeat("x", 1000), URL: "https://www.instagram.com/p/X/"}
	}
	posts := []proto.Post{post(1), post(2), post(3)}
	raw, more, err := fitList(posts, false, 2500)
	if err != nil || len(raw) != 2 || !more {
		t.Errorf("got %d posts, more %v, %v", len(raw), more, err)
	}
	raw, more, _ = fitList(posts, false, 10)
	if len(raw) != 1 || !more {
		t.Errorf("one post always stays: got %d, more %v", len(raw), more)
	}
	raw, more, _ = fitList(posts, false, PageBudget)
	if len(raw) != 3 || more {
		t.Errorf("got %d, more %v", len(raw), more)
	}
	if raw, _, _ := fitList[proto.Post](nil, false, PageBudget); raw == nil {
		t.Error("no posts must be [], not null")
	}
}
