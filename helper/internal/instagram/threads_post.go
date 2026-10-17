// SPDX-License-Identifier: AGPL-3.0-or-later

package instagram

// One Threads post by its link (load_post), only read: the post's page on
// threads.com, loaded as a browser opening the link from another app loads
// it, and the post as that page has it, in the answers to the page's own
// queries that come inside it. It shares the feed's lock, and the post gets
// the id the feed gives it, so its replies load as the feed's.

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

var (
	errThreadsNotAPostCode = proto.Err(proto.BadRequest, "That isn't a Threads post's code.")
	errThreadsPostGone     = proto.Err(proto.NotFound,
		"Threads has no post of this link that this account can see; o opens it on threads.com.")
	errThreadsPostEmpty = proto.Err(proto.NotFound,
		"This post has nothing tuimeta can show; o opens it on threads.com.")
)

var _ backend.PostLoader = (*Threads)(nil)

// LoadPost is the post a link's code names, on its author's page when the
// link said who (username), else on the address Threads' short links have.
func (t *Threads) LoadPost(ctx context.Context, code, username string, box backend.Box) (proto.Post, error) {
	if !backend.PostCode(code) {
		return proto.Post{}, errThreadsNotAPostCode
	}
	page := threadsBase + "/t/" + code
	if u := threadsURL(username, code); u != "" {
		page = u
	}
	s, epoch, err := t.current(ctx)
	if err != nil {
		return proto.Post{}, err
	}
	f := &t.feed
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sync(epoch)
	html, err := t.postPage(ctx, s, epoch, page)
	if err != nil {
		hlog.Warn("threads: post failed", hlog.Kind(err))
		return proto.Post{}, err
	}
	m, ok := findThreadsPost(html, code)
	if !ok || bool(m.Info.Unavailable) {
		return proto.Post{}, errThreadsPostGone
	}
	st := feedStats{threads: true}
	p, ok := postOf(feedFiles{t.d.Files, proto.Threads}, &m, box, &st, false)
	st.log()
	if !ok {
		return proto.Post{}, errThreadsPostEmpty
	}
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
	f.refs[id] = postRef{pk: mediaPK(&m.feedMedia), url: p.URL}
	p.ID = id
	hlog.Info("threads: post loaded")
	return p, nil
}

// postPage is a post's page, as it came: the session's next requests go
// from it, as a browser's would. A page that reads as nobody's doesn't end
// the session here, as the home page's would: the feed's next request
// finds out from the home page. t.net is taken.
func (t *Threads) postPage(ctx context.Context, s *threadsSession, epoch int, page string) (string, error) {
	t.net.Lock()
	defer t.net.Unlock()
	defer func() {
		if s.changed {
			t.save(s, epoch)
		}
	}()
	_, html, err := t.open(ctx, s, page, "")
	switch {
	case err == nil:
		return html, nil
	case errors.Is(err, errPageGone):
		return "", errThreadsPostGone
	case errors.Is(err, errLoggedOutPage):
		return "", errThreadsPage
	}
	return "", t.trouble(s, epoch, err)
}

// bboxMark starts an answer to one of a page's own queries, as the page
// has it: {"__bbox":{"result":{"data":…}}}, or a list of them.
const bboxMark = `{"__bbox":`

// maxJSONDepth bounds walkJSON: a page's answers nest a few dozen deep.
const maxJSONDepth = 64

// findThreadsPost is the post with code on a post's page: the media dict
// of that code (with its author) in the answers the page came with, the
// fullest where several have it (the post's own query's, not the replies'
// one naming it).
func findThreadsPost(html, code string) (threadsMedia, bool) {
	if code == "" {
		return threadsMedia{}, false
	}
	var best map[string]any
	for at := 0; at < len(html); {
		i := strings.Index(html[at:], bboxMark)
		if i < 0 {
			break
		}
		i += at
		dec := json.NewDecoder(strings.NewReader(html[i:]))
		dec.UseNumber() // so a number pk reads as it was written
		var v any
		if dec.Decode(&v) != nil {
			at = i + len(bboxMark)
			continue
		}
		at = i + int(dec.InputOffset())
		walkJSON(v, 0, func(m map[string]any) {
			if c, _ := m["code"].(string); c != code {
				return
			}
			if _, ok := m["user"].(map[string]any); ok && len(m) > len(best) {
				best = m
			}
		})
	}
	if best == nil {
		return threadsMedia{}, false
	}
	raw, err := json.Marshal(best)
	if err != nil {
		return threadsMedia{}, false
	}
	var m threadsMedia
	if json.Unmarshal(raw, &m) != nil {
		return threadsMedia{}, false
	}
	return m, true
}

// walkJSON shows visit every object in v, in the order of their keys, to
// maxJSONDepth deep.
func walkJSON(v any, depth int, visit func(map[string]any)) {
	if depth > maxJSONDepth {
		return
	}
	switch v := v.(type) {
	case map[string]any:
		visit(v)
		for _, k := range slices.Sorted(maps.Keys(v)) {
			walkJSON(v[k], depth+1, visit)
		}
	case []any:
		for _, w := range v {
			walkJSON(w, depth+1, visit)
		}
	}
}
