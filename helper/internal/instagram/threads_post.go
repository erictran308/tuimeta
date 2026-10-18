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
	_, epoch, err := t.current(ctx)
	if err != nil {
		return proto.Post{}, err
	}
	f := &t.feed
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sync(epoch)
	html, err := t.postPage(ctx, epoch, page)
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
	p.ID = f.idFor(&m, p.URL)
	hlog.Info("threads: post loaded")
	return p, nil
}

// postPage is a post's page, as it came, for login epoch. The session's
// requests don't go from it (open), and a page that reads as nobody's
// doesn't end the session here, as the home page's would: the feed's next
// request finds out from the home page. t.net is taken.
func (t *Threads) postPage(ctx context.Context, epoch int, page string) (string, error) {
	s, err := t.currentOf(ctx, epoch)
	if err != nil {
		return "", err
	}
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

// maxJSONNesting is how deep a page's answers are read, which nest a few
// dozen deep: encoding/json's own bound.
const maxJSONNesting = 10000

// findThreadsPost is the post with code on a post's page: the media dict
// of that code (with its author) in the answers the page came with, the
// fullest where several have it (the post's own query's, not the replies'
// one naming it), the first of those where they're as full. The page is
// read once, however its answers are cut short or broken.
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
		read := scanJSON(html[i:], func(m map[string]any) {
			if c, _ := m["code"].(string); c != code {
				return
			}
			if _, ok := m["user"].(map[string]any); ok && len(m) > len(best) {
				best = m
			}
		})
		at = i + max(read, len(bboxMark))
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

// scanJSON reads the JSON value s starts with, a token at a time, and
// shows visit each object in it once it's closed: inner ones first. A
// value cut short or broken off still shows those that closed before it
// broke, so the answers inside a broken one are found all the same. It
// returns how far it read: to the value's end, or to the last token before
// it broke, past which nothing it had open can close (or where it nested
// past maxJSONNesting), so the next answer is looked for from there.
func scanJSON(s string, visit func(map[string]any)) int {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber() // so a number pk reads as it was written
	// open is an object (obj) or a list (arr) being read; key is the
	// object's key whose value comes next, if hasKey.
	type open struct {
		obj    map[string]any
		arr    []any
		key    string
		hasKey bool
	}
	var stack []*open
	for {
		tok, err := dec.Token()
		if err != nil {
			return int(dec.InputOffset())
		}
		var v any
		switch tok {
		case json.Delim('{'), json.Delim('['):
			if len(stack) == maxJSONNesting {
				return int(dec.InputOffset())
			}
			o := &open{arr: []any{}}
			if tok == json.Delim('{') {
				o.obj, o.arr = map[string]any{}, nil
			}
			stack = append(stack, o)
			continue
		case json.Delim('}'), json.Delim(']'):
			o := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if o.obj == nil {
				v = o.arr
				break
			}
			visit(o.obj)
			v = o.obj
		default:
			if len(stack) == 0 {
				return int(dec.InputOffset())
			}
			if top := stack[len(stack)-1]; top.obj != nil && !top.hasKey {
				top.key, top.hasKey = tok.(string)
				continue
			}
			v = tok
		}
		if len(stack) == 0 {
			return int(dec.InputOffset())
		}
		if top := stack[len(stack)-1]; top.obj != nil {
			top.obj[top.key], top.hasKey = v, false
		} else {
			top.arr = append(top.arr, v)
		}
	}
}
