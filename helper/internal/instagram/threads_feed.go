// SPDX-License-Identifier: AGPL-3.0-or-later

package instagram

// Threads' feed (load_feed): a page at a time, with the query threads.com's
// home page pages its feed with, sent as the page sends it, less what
// reports views: the list of posts seen (feed_view_info) always goes out
// empty, as from a page just opened. What it does tell is what any page
// does to get the next one: which threads it was given (feed_state, ids
// only), so Threads doesn't give them again.

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

const (
	// threadsPage is how many posts a page asks for, as the website's
	// "load more" does.
	threadsPageSize = 10
	// threadsLastN is how many of the latest threads feed_state names,
	// unless the page's experiment (qex 5205) says otherwise.
	threadsLastN = 10
	// maxUnits is the most feed items remembered for feed_state: a long
	// read keeps only the latest.
	maxUnits = 500
	// maxContext caps the line over a post.
	maxContext = 120
	// maxLinkTitle caps a link card's title, and maxLinkURL its address.
	maxLinkTitle = 300
	maxLinkURL   = 2048
)

// The view state item types of a thread, as the website counts them in
// feed_state: one of these last in a thread makes it "organic".
const (
	viewStateThread      = 132
	viewStateThreadReply = 194
)

var errThreadsFeedAnswer = proto.Err(proto.NetworkError,
	"Threads' answer about the feed couldn't be read; try again later.")

// threadsFeed is where the feed and its posts' replies got to. Its lock is
// held for a whole load, so loads are one at a time.
type threadsFeed struct {
	mu sync.Mutex
	// epoch is the login the cursors belong to: another starts over.
	epoch   int
	kind    backend.FeedKind
	cursor  string
	started bool
	more    bool
	// session and source are what the last page said to page on with;
	// units are the feed items given, and lastAt when the last page was
	// asked for (ms), for feed_state.
	session json.RawMessage
	source  string
	units   []feedUnit
	lastAt  int64
	// posts are the ids given, by Threads' post pk, so a post seen again
	// keeps its id; refs what a post's replies are found by. Comments
	// likewise, and lists where each list of replies got to, by post and
	// reply (0 for the post's own).
	posts       map[string]int64
	next        int64
	refs        map[int64]postRef
	comments    map[string]int64
	commentOf   map[int64]commentRef
	nextComment int64
	lists       map[[2]int64]*listState
}

// feedUnit is a feed item as feed_state counts it: a thread and its
// posts, or another row (people to follow).
type feedUnit struct {
	thread bool
	items  []unitItem
}

type unitItem struct {
	ID   string `json:"id"`
	Type int64  `json:"view_state_item_type"`
}

// reset forgets the feed and the replies, but not the last ids given: none
// is given twice in a run.
func (f *threadsFeed) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clear()
}

// clear is reset's; f.mu is held.
func (f *threadsFeed) clear() {
	f.kind, f.cursor, f.started, f.more = backend.ForYou, "", false, false
	f.session, f.source, f.units, f.lastAt = nil, "", nil, 0
	f.posts = map[string]int64{}
	f.refs = map[int64]postRef{}
	f.comments = map[string]int64{}
	f.commentOf = map[int64]commentRef{}
	f.lists = map[[2]int64]*listState{}
}

// sync starts over when the login changed; f.mu is held.
func (f *threadsFeed) sync(epoch int) {
	if f.posts != nil && f.epoch == epoch {
		return
	}
	f.clear()
	f.epoch = epoch
}

// restart goes back to the top of feed kind; f.mu is held.
func (f *threadsFeed) restart(kind backend.FeedKind) {
	f.kind, f.cursor, f.started, f.more = kind, "", false, false
	f.session, f.source, f.units = nil, "", nil
}

// paginationSource is what the website calls each feed when paging it.
func paginationSource(kind backend.FeedKind) string {
	if kind == backend.Following {
		return "text_post_feed_following"
	}
	return "text_post_feed_threads"
}

// textAppState is the website's TextAppFeedState, as it sends it: what the
// feed has given, by id, and when it was last asked.
type textAppState struct {
	LastNItems             []lastThread   `json:"last_n_items"`
	LastRequestTimeMs      int64          `json:"last_request_time_ms"`
	NumOrganicItems        int            `json:"num_organic_items"`
	NumOrganicThreadItems  int            `json:"num_organic_thread_items"`
	NumTotalItems          int            `json:"num_total_items"`
	NumTotalThreadItems    int            `json:"num_total_thread_items"`
	PositionSensitiveItems []positionItem `json:"position_sensitive_items"`
}

type lastThread struct {
	ThreadItems []threadPost `json:"thread_items"`
}

type threadPost struct {
	Post unitItem `json:"post"`
}

type positionItem struct {
	Index int      `json:"index"`
	Item  unitItem `json:"item"`
}

// buildFeedState is the website's buildTextAppFeedState: the threads given
// whose last post is a thread or a reply are "organic", the last n of
// them named; every other item is named with its place.
func buildFeedState(units []feedUnit, at int64, n int) textAppState {
	st := textAppState{LastNItems: []lastThread{}, LastRequestTimeMs: at, PositionSensitiveItems: []positionItem{}}
	var organic [][]unitItem
	i := 0
	for _, u := range units {
		if len(u.items) == 0 {
			continue
		}
		last := u.items[len(u.items)-1]
		if u.thread {
			st.NumTotalThreadItems += len(u.items)
		} else {
			st.NumTotalThreadItems++
		}
		if u.thread && (last.Type == viewStateThread || last.Type == viewStateThreadReply) {
			organic = append(organic, u.items)
			st.NumOrganicThreadItems += len(u.items)
		} else {
			st.PositionSensitiveItems = append(st.PositionSensitiveItems, positionItem{Index: i, Item: last})
		}
		st.NumTotalItems++
		i++
	}
	st.NumOrganicItems = len(organic)
	if n > 0 {
		for _, items := range organic[max(len(organic)-n, 0):] {
			t := lastThread{ThreadItems: []threadPost{}}
			for _, it := range items {
				t.ThreadItems = append(t.ThreadItems, threadPost{Post: it})
			}
			st.LastNItems = append(st.LastNItems, t)
		}
	}
	return st
}

// variables are the next page's, as the website's feed asks for it: the
// first, then each after it. now is when it's asked (ms).
func (f *threadsFeed) variables(now int64) map[string]any {
	var state textAppState
	data := map[string]any{"feed_view_info": "[]"}
	if !f.started {
		// A first load names nothing given yet, as of now.
		state = buildFeedState(nil, now, 0)
		data["pagination_source"] = paginationSource(f.kind)
	} else {
		state = buildFeedState(f.units, f.lastAt, threadsLastN)
		source := f.source
		if source == "" {
			source = paginationSource(f.kind)
		}
		data["pagination_source"] = source
		data["reason"] = "pagination"
		data["session_id"] = json.RawMessage("null")
		if len(f.session) > 0 {
			data["session_id"] = f.session
		}
	}
	encoded, _ := json.Marshal(state)
	data["feed_state"] = string(encoded)
	vars := map[string]any{"first": threadsPageSize, "variant": string(f.kind), "data": data}
	if f.started {
		vars["after"] = f.cursor
	}
	return vars
}

// LoadFeed is the next page of the feed kind (for you, unless it says).
// limit is only a hint: Threads' pages are its own size.
func (t *Threads) LoadFeed(ctx context.Context, _ int, fresh bool, box backend.Box, kind backend.FeedKind) ([]proto.Post, bool, error) {
	if kind == "" {
		kind = backend.ForYou
	}
	_, epoch, err := t.current(ctx)
	if err != nil {
		return nil, false, err
	}
	f := &t.feed
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sync(epoch)
	if fresh || kind != f.kind {
		f.restart(kind)
	}
	if f.started && !f.more {
		return []proto.Post{}, false, nil
	}
	posts := []proto.Post{}
	given := map[int64]bool{}
	for range feedTries {
		now := time.Now().UnixMilli()
		data, err := t.ask(ctx, epoch, threadsFeedQuery, f.variables(now), threadsBase+"/")
		if err != nil {
			warnFailed(ctx, "threads: feed failed", err)
			return nil, false, err
		}
		page, err := parseThreadsFeed(data)
		if err != nil {
			hlog.Warn("threads: feed unreadable", hlog.Kind(err))
			return nil, false, errThreadsFeedAnswer
		}
		f.started, f.lastAt = true, now
		f.cursor = page.cursor
		f.more = page.more && page.cursor != ""
		f.session, f.source = page.session, page.source
		f.units = append(f.units, page.units...)
		if over := len(f.units) - maxUnits; over > 0 {
			f.units = f.units[over:]
		}
		st := feedStats{threads: true, ads: page.ads, unreadable: page.unreadable}
		for _, p := range t.feedPosts(f, page.threads, box, &st) {
			if !given[p.ID] {
				given[p.ID] = true
				posts = append(posts, p)
			}
		}
		if page.people > 0 {
			hlog.Info("threads: feed rows left out", hlog.Int("people", int64(page.people)))
		}
		st.log()
		if len(posts) > 0 || !f.more {
			break
		}
	}
	return posts, f.more, nil
}

// --- reading a page ---------------------------------------------------------

// threadsMedia is a Threads post: Instagram's media dict, and what Threads
// adds to it.
type threadsMedia struct {
	feedMedia
	ViewState flexInt     `json:"view_state_item_type"`
	Info      threadsInfo `json:"text_post_app_info"`
}

type threadsInfo struct {
	Replies     *flexInt `json:"direct_reply_count"`
	Reposts     *flexInt `json:"repost_count"`
	Quotes      *flexInt `json:"quote_count"`
	Reshares    *flexInt `json:"reshare_count"`
	Unavailable flexBool `json:"is_post_unavailable"`
	Share       struct {
		Reposted *threadsMedia `json:"reposted_post"`
		Quoted   *threadsMedia `json:"quoted_post"`
	} `json:"share_info"`
	Link *struct {
		URL   string `json:"url"`
		Title string `json:"title"`
		Image string `json:"image_url"`
	} `json:"link_preview_attachment"`
}

// feedThread is one thread of a page: its posts, in order, and the line
// over it.
type feedThread struct {
	context string
	posts   []threadsMedia
}

// threadsFeedPage is one answer, read.
type threadsFeedPage struct {
	threads []feedThread
	units   []feedUnit
	cursor  string
	more    bool
	session json.RawMessage
	source  string
	// people counts the rows of people to follow left out; ads and
	// unreadable, the posts.
	people, ads, unreadable int
}

// parseThreadsFeed reads an answer's data: each thread's posts in order.
// A post that doesn't read is left out alone; rows of people to follow are
// counted for feed_state and left out.
func parseThreadsFeed(data []byte) (threadsFeedPage, error) {
	var r struct {
		Feed *struct {
			Edges []struct {
				Node struct {
					Thread *struct {
						Header *struct {
							Text string `json:"text"`
						} `json:"thread_header_context"`
						Items []struct {
							Post json.RawMessage `json:"post"`
						} `json:"thread_items"`
					} `json:"text_post_app_thread"`
					People *struct {
						ID    flexString      `json:"id"`
						Type  flexInt         `json:"view_state_item_type"`
						Cards json.RawMessage `json:"suggestion_cards"`
					} `json:"suggested_users"`
				} `json:"node"`
			} `json:"edges"`
			Source   string          `json:"pagination_source"`
			Session  json.RawMessage `json:"session_id"`
			PageInfo struct {
				End     *string  `json:"end_cursor"`
				HasNext flexBool `json:"has_next_page"`
			} `json:"page_info"`
		} `json:"feedData"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return threadsFeedPage{}, err
	}
	if r.Feed == nil {
		return threadsFeedPage{}, errThreadsFeedAnswer
	}
	page := threadsFeedPage{more: bool(r.Feed.PageInfo.HasNext), source: r.Feed.Source}
	if r.Feed.PageInfo.End != nil {
		page.cursor = *r.Feed.PageInfo.End
	}
	if s := r.Feed.Session; present(s) {
		page.session = s
	}
	for _, e := range r.Feed.Edges {
		switch n := e.Node; {
		case n.Thread != nil:
			th := feedThread{}
			if n.Thread.Header != nil {
				th.context = oneLine(n.Thread.Header.Text, maxContext)
			}
			unit := feedUnit{thread: true}
			for _, it := range n.Thread.Items {
				var m threadsMedia
				if !present(it.Post) {
					continue
				}
				if json.Unmarshal(it.Post, &m) != nil {
					page.unreadable++
					continue
				}
				unit.items = append(unit.items, unitItem{ID: string(m.ID), Type: int64(m.ViewState)})
				if m.ad() || (m.Info.Share.Reposted != nil && m.Info.Share.Reposted.ad()) {
					page.ads++
					continue
				}
				th.posts = append(th.posts, m)
			}
			page.units = append(page.units, unit)
			if len(th.posts) > 0 {
				page.threads = append(page.threads, th)
			}
		case n.People != nil:
			page.people++
			if present(n.People.Cards) {
				page.units = append(page.units, feedUnit{items: []unitItem{{ID: string(n.People.ID), Type: int64(n.People.Type)}}})
			}
		}
	}
	return page, nil
}

// oneLine is s with its runs of spaces and line breaks made one space,
// cut to n characters: the line over a post is the network's sentence, but
// it names people, who could write anything in a name.
func oneLine(s string, n int) string {
	s = strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ")
	return runes(s, n)
}

// --- making posts -----------------------------------------------------------

// feedPosts are a page's threads' posts, in order: a thread's later posts
// go on from the one before (continues); f.mu is held.
func (t *Threads) feedPosts(f *threadsFeed, threads []feedThread, box backend.Box, st *feedStats) []proto.Post {
	fs := feedFiles{t.d.Files, proto.Threads}
	var out []proto.Post
	for _, th := range threads {
		first := true
		for i := range th.posts {
			m := &th.posts[i]
			context := ""
			if first {
				context = th.context
			}
			if r := m.Info.Share.Reposted; r != nil {
				// A repost is the post reposted, and who reposted it.
				context = "Reposted"
				if u := strings.TrimSpace(m.User.Username); backend.Username(u) {
					context = u + " reposted"
				}
				m = r
			}
			p, ok := postOf(fs, m, box, st, false)
			if !ok {
				continue
			}
			p.ID, p.Context, p.Continues = f.idFor(m, p.URL), context, !first
			first = false
			out = append(out, p)
		}
	}
	return out
}

// idFor is the id of post m, which opens at url: the one it was given
// before (by its pk, else its id), or the next. Its replies are found by
// it from now on. f.mu is held.
func (f *threadsFeed) idFor(m *threadsMedia, url string) int64 {
	key := string(m.Pk)
	if key == "" {
		key = string(m.ID)
	}
	id, seen := f.posts[key]
	if !seen {
		f.next++
		id = f.next
		f.posts[key] = id
	}
	f.refs[id] = postRef{pk: mediaPK(&m.feedMedia), url: url}
	return id
}

// postOf is the protocol's post of a Threads post, registering its files;
// false when there's nothing to show or (but for a quoted one) no address
// to open it at. A quoted post's id is 0, and it quotes nothing itself.
func postOf(fs feedFiles, m *threadsMedia, box backend.Box, st *feedStats, quoted bool) (proto.Post, bool) {
	link := threadsURL(m.User.Username, m.Code)
	if link == "" && !quoted {
		st.noCode++
		return proto.Post{}, false
	}
	var items []*feedMedia
	switch m.MediaType {
	case 1, 2:
		items = []*feedMedia{&m.feedMedia}
	case 8:
		for i := range m.Carousel {
			if len(items) == maxCarousel {
				break
			}
			items = append(items, &m.Carousel[i])
		}
	}
	var media []proto.Media
	for _, it := range items {
		if md, ok := fs.mediaOf(it, box, st); ok {
			media = append(media, md)
		}
	}
	p := proto.Post{
		Author: proto.FeedAuthor{
			Name:     runes(strings.TrimSpace(m.User.FullName), maxAuthor),
			Username: runes(strings.TrimSpace(m.User.Username), maxAuthor),
			Verified: bool(m.User.Verified),
			Photo:    fs.file(m.User.ProfilePicURL, "avatar", proto.Photo, avatarSize, avatarSize),
		},
		Date:  max(int64(m.TakenAt), 0),
		Media: media,
		URL:   link,
	}
	if m.Caption != nil {
		p.Caption = runes(m.Caption.Text, maxCaption)
	}
	if m.LikeCount != nil && !m.CountsHidden {
		n := max(int64(*m.LikeCount), 0)
		p.Likes = &n
	}
	if c := m.Info.Replies; c != nil {
		n := max(int64(*c), 0)
		p.Comments = &n
	}
	if m.Info.Reposts != nil || m.Info.Quotes != nil {
		var n int64
		for _, c := range []*flexInt{m.Info.Reposts, m.Info.Quotes} {
			if c != nil {
				n += max(int64(*c), 0)
			}
		}
		p.Reposts = &n
	}
	if c := m.Info.Reshares; c != nil {
		n := max(int64(*c), 0)
		p.Shares = &n
	}
	if l := m.Info.Link; l != nil {
		if u := linkTarget(l.URL); u != "" {
			p.LinkPreview = &proto.LinkPreview{URL: u, Title: oneLine(l.Title, maxLinkTitle),
				Image: fs.file(l.Image, "link-image", proto.Photo, 0, 0)}
		}
	}
	if q := m.Info.Share.Quoted; q != nil && !quoted {
		if qp, ok := postOf(fs, q, box, st, true); ok {
			p.Quoted = &qp
		}
	}
	if len(media) == 0 && strings.TrimSpace(p.Caption) == "" && p.Quoted == nil && p.LinkPreview == nil {
		st.noMedia++
		return proto.Post{}, false
	}
	return p, true
}

// threadsURL is where a post is on threads.com, "" unless its author's
// username and its code are what Threads' are made of.
func threadsURL(username, code string) string {
	username = strings.TrimSpace(username)
	if !backend.Username(username) || !backend.PostCode(code) {
		return ""
	}
	return threadsBase + "/@" + username + "/post/" + code
}

// linkTarget is a link card's address with Threads' redirect taken off: a
// web address, not too long, or "".
func linkTarget(raw string) string {
	if len(raw) > maxLinkURL*4 {
		return ""
	}
	u := webURL(unwrapRedirect(raw))
	if len(u) > maxLinkURL {
		return ""
	}
	return u
}
