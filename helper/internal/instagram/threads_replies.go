// SPDX-License-Identifier: AGPL-3.0-or-later

package instagram

// Replies to Threads' posts (load_comments), only read: the post's page's
// own queries, its first replies (top ones first) and the ones after, and
// a reply's replies the same way, by the reply's own id, as the reply's
// page has them. Nothing is liked, written or marked seen. They share the
// feed's lock, and every request threads.com's gap.

import (
	"context"
	"encoding/json"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// repliesPage is how many replies after the first a page asks for.
const repliesPage = 10

var (
	errThreadsNoPost = proto.Err(proto.NotFound,
		"Threads didn't say how to find this post's replies; open it on threads.com.")
	errThreadsRepliesAnswer = proto.Err(proto.NetworkError,
		"Threads' answer about the replies couldn't be read; try again later.")
)

var _ backend.Commenter = (*Threads)(nil)

// LoadComments is a page of post postID's replies, or of reply repliesOf's
// replies; more goes on from the last page given, else it's the first page
// again.
func (t *Threads) LoadComments(ctx context.Context, postID, repliesOf int64, more bool) ([]proto.Comment, bool, error) {
	_, epoch, err := t.current(ctx)
	if err != nil {
		return nil, false, err
	}
	f := &t.feed
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sync(epoch)
	post, ok := f.refs[postID]
	if !ok {
		return nil, false, errNoPost
	}
	of := post.pk
	if repliesOf != 0 {
		c, ok := f.commentOf[repliesOf]
		if !ok || c.post != postID {
			return nil, false, errNoComment
		}
		of = c.pk
	}
	if !digits(of) {
		return nil, false, errThreadsNoPost
	}
	key := [2]int64{postID, repliesOf}
	list := f.lists[key]
	if list == nil || !more {
		list = &listState{}
		f.lists[key] = list
	}
	if list.started && !list.more {
		return []proto.Comment{}, false, nil
	}
	name, vars := threadsRepliesQuery, map[string]any{"postID": of, "sortOrder": "TOP"}
	if list.started {
		name = threadsMoreQuery
		vars = map[string]any{"after": list.cursor, "first": repliesPage, "postID": of, "sortOrder": "TOP"}
	}
	data, err := t.ask(ctx, name, vars, post.url)
	if err != nil {
		hlog.Warn("threads: replies failed", hlog.Kind(err))
		return nil, false, err
	}
	replies, cursor, next, err := parseReplies(data)
	if err != nil {
		hlog.Warn("threads: replies unreadable", hlog.Kind(err))
		return nil, false, errThreadsRepliesAnswer
	}
	list.started, list.cursor = true, cursor
	list.more = next && cursor != ""
	out := []proto.Comment{}
	for i := range replies {
		if c, ok := f.replyComment(postID, repliesOf, &replies[i]); ok {
			out = append(out, c)
		}
	}
	return out, list.more, nil
}

// parseReplies reads an answer's replies: each reply edge's first post
// (the reply; the rest are its author's posts under it), and where the
// list goes on from.
func parseReplies(data []byte) (replies []threadsMedia, cursor string, more bool, err error) {
	var r struct {
		Media *struct {
			Info struct {
				Replies *struct {
					Edges []struct {
						Node struct {
							Posts struct {
								Edges []struct {
									Node json.RawMessage `json:"node"`
								} `json:"edges"`
							} `json:"posts"`
						} `json:"node"`
					} `json:"edges"`
					PageInfo struct {
						End     *string  `json:"end_cursor"`
						HasNext flexBool `json:"has_next_page"`
					} `json:"page_info"`
				} `json:"direct_replies"`
			} `json:"text_post_app_info"`
		} `json:"media"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, "", false, err
	}
	if r.Media == nil || r.Media.Info.Replies == nil {
		return nil, "", false, errThreadsRepliesAnswer
	}
	d := r.Media.Info.Replies
	for _, e := range d.Edges {
		if len(e.Node.Posts.Edges) == 0 {
			continue
		}
		var m threadsMedia
		if json.Unmarshal(e.Node.Posts.Edges[0].Node, &m) == nil {
			replies = append(replies, m)
		}
	}
	if d.PageInfo.End != nil {
		cursor = *d.PageInfo.End
	}
	return replies, cursor, bool(d.PageInfo.HasNext), nil
}

// replyComment is the protocol's comment of a reply on post postID (to reply
// repliesOf, or to the post), with its id for the run; false for one
// without an id or an author; f.mu is held.
func (f *threadsFeed) replyComment(postID, repliesOf int64, m *threadsMedia) (proto.Comment, bool) {
	pk := mediaPK(&m.feedMedia)
	if pk == "" || (m.User.Username == "" && m.User.FullName == "") || bool(m.Info.Unavailable) {
		return proto.Comment{}, false
	}
	id, ok := f.comments[pk]
	if !ok {
		f.nextComment++
		id = f.nextComment
		f.comments[pk] = id
	}
	f.commentOf[id] = commentRef{post: postID, pk: pk}
	c := proto.Comment{
		ID: id,
		Author: proto.FeedAuthor{
			Name:     runes(m.User.FullName, maxAuthor),
			Username: runes(m.User.Username, maxAuthor),
			Verified: bool(m.User.Verified),
		},
		Date:    max(int64(m.TakenAt), 0),
		ReplyTo: repliesOf,
	}
	if m.Caption != nil {
		c.Text = runes(m.Caption.Text, maxCaption)
	}
	if m.LikeCount != nil && !m.CountsHidden {
		n := max(int64(*m.LikeCount), 0)
		c.Likes = &n
	}
	if m.Info.Replies != nil {
		c.Replies = max(int64(*m.Info.Replies), 0)
	}
	switch m.MediaType {
	case 1:
		c.Attachment = proto.AttachPhoto
	case 2:
		c.Attachment = proto.AttachVideo
	case 8:
		c.Attachment = proto.AttachCarousel
	}
	return c, true
}
