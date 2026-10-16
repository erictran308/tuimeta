// SPDX-License-Identifier: AGPL-3.0-or-later

package instagram

// Comments on the feed's posts (load_comments), only read: a page of a
// post's comments, or of a comment's replies, with the requests the post's
// page on instagram.com makes for them. Nothing is liked, written or
// reported as seen. They share the feed's lock and its gap between
// requests (feedGap), so going back and forth between the feed and
// comments can't page faster than either alone.

import (
	"context"
	"encoding/json"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

const errComments = "Couldn't load the comments from Instagram; try again."

var (
	errNoPost    = proto.Err(proto.NotFound, "That post isn't loaded; load the feed again.")
	errNoComment = proto.Err(proto.NotFound,
		"That comment isn't loaded; load the post's comments again.")
	errNoMediaID = proto.Err(proto.NotFound,
		"Instagram didn't say how to find this post's comments; open it on instagram.com.")
	errCommentsAnswer = proto.Err(proto.NetworkError,
		"Instagram's answer about the comments couldn't be read; try again later.")
)

// postRef is what a post's comments are found by: its media pk (digits,
// for the address) and its page, the requests' referer.
type postRef struct {
	pk, url string
}

// commentRef is what a comment id stands for: the post it's on, and its
// pk (digits).
type commentRef struct {
	post int64
	pk   string
}

// listState is where a list of comments got to: the cursor the last page
// gave and the query parameter it goes back as.
type listState struct {
	param, cursor string
	started, more bool
}

var _ backend.Commenter = (*Instagram)(nil)

// LoadComments is a page of post postID's comments, or of comment
// repliesOf's replies, oldest first; more goes on from the last page given,
// else it's the first page again.
func (b *Instagram) LoadComments(ctx context.Context, postID, repliesOf int64, more bool) ([]proto.Comment, bool, error) {
	conn, err := b.waitReady(ctx)
	if err != nil {
		return nil, false, err
	}
	b.mu.Lock()
	epoch := b.epoch
	b.mu.Unlock()
	f := &b.feed
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sync(epoch)
	post, ok := f.media[postID]
	if !ok {
		return nil, false, errNoPost
	}
	if !digits(post.pk) {
		return nil, false, errNoMediaID
	}
	query := url.Values{}
	path, parentPK := "/api/v1/media/"+post.pk+"/comments/", ""
	lists := f.threads
	key := postID
	if repliesOf != 0 {
		ref, ok := f.commentOf[repliesOf]
		if !ok || ref.post != postID {
			return nil, false, errNoComment
		}
		parentPK = ref.pk
		path += parentPK + "/child_comments/"
		lists, key = f.replies, repliesOf
	} else {
		query.Set("can_support_threading", "true")
		query.Set("permalink_enabled", "false")
	}
	list := lists[key]
	if list == nil || !more {
		list = &listState{}
		lists[key] = list
	}
	if list.started && !list.more {
		return []proto.Comment{}, false, nil
	}
	if list.started {
		query.Set(list.param, list.cursor)
	}
	if err := f.wait(ctx); err != nil {
		return nil, false, err
	}
	body, err := conn.cli.Comments(withQuietLog(ctx), path, query, post.url)
	f.last = time.Now()
	if err == nil {
		err = refused(body, errComments)
	}
	if err != nil {
		hlog.Warn("instagram: comments failed", hlog.Kind(err))
		return nil, false, feedError(err, body, errComments)
	}
	page, err := parseComments(body, repliesOf != 0)
	if err != nil {
		hlog.Warn("instagram: comments unreadable", hlog.Kind(err))
		return nil, false, errCommentsAnswer
	}
	list.started, list.param, list.cursor, list.more = true, page.param, page.cursor, page.more
	out := []proto.Comment{}
	given := map[int64]bool{}
	skipped := page.unreadable
	for _, c := range page.comments {
		if parentPK != "" && c.Parent != "" && string(c.Parent) != parentPK {
			skipped++ // a reply, but to another comment
			continue
		}
		id := f.commentID(postID, string(c.Pk))
		if given[id] {
			continue
		}
		given[id] = true
		out = append(out, c.comment(id, repliesOf))
	}
	if repliesOf != 0 {
		slices.SortStableFunc(out, func(a, b proto.Comment) int { return int(min(max(a.Date-b.Date, -1), 1)) })
	}
	what := "instagram: comments loaded"
	if repliesOf != 0 {
		what = "instagram: replies loaded"
	}
	hlog.Info(what, hlog.Int("count", int64(len(out))), hlog.Int("left_out", int64(skipped)))
	return out, list.more, nil
}

// commentID is the id of the comment with pk on post, given now if it
// hasn't one; f.mu is held.
func (f *feedState) commentID(post int64, pk string) int64 {
	if id, ok := f.comments[pk]; ok {
		return id
	}
	f.nextComment++
	id := f.nextComment
	f.comments[pk] = id
	f.commentOf[id] = commentRef{post: post, pk: pk}
	return id
}

// feedComment is the part of Instagram's comment a Comment needs.
type feedComment struct {
	Pk           flexString `json:"pk"`
	Text         flexString `json:"text"`
	Type         *flexInt   `json:"type"`
	CreatedAt    flexInt    `json:"created_at"`
	CreatedAtUTC flexInt    `json:"created_at_utc"`
	User         *feedUser  `json:"user"`
	Likes        *flexInt   `json:"comment_like_count"`
	Children     flexInt    `json:"child_comment_count"`
	Parent       flexString `json:"parent_comment_id"`
}

// comment is the protocol's comment; a reply (to repliesOf) has no
// replies of its own.
func (c *feedComment) comment(id, repliesOf int64) proto.Comment {
	out := proto.Comment{ID: id, Text: runes(string(c.Text), maxCaption), ReplyTo: repliesOf}
	if c.User != nil {
		out.Author = proto.FeedAuthor{
			Name:     runes(strings.TrimSpace(c.User.FullName), maxAuthor),
			Username: runes(strings.TrimSpace(c.User.Username), maxAuthor),
			Verified: bool(c.User.Verified),
		}
	}
	out.Date = max(int64(c.CreatedAt), 0)
	if out.Date == 0 {
		out.Date = max(int64(c.CreatedAtUTC), 0)
	}
	if c.Likes != nil {
		n := max(int64(*c.Likes), 0)
		out.Likes = &n
	}
	if repliesOf == 0 {
		out.Replies = max(int64(c.Children), 0)
	}
	return out
}

// commentPage is one answer, read.
type commentPage struct {
	comments      []feedComment
	param, cursor string // the cursor for the next page, and how it's asked for
	more          bool
	unreadable    int
}

// parseComments reads a page of a post's comments, or of a comment's
// replies. Comments that don't read, with a pk that isn't a number, or
// that are the post's caption (type 1) are left out; preview replies
// inside a comment are never read (replies come only with replies_of).
func parseComments(body []byte, replies bool) (commentPage, error) {
	var r struct {
		Comments    []json.RawMessage `json:"comments"`
		Children    []json.RawMessage `json:"child_comments"`
		NextMin     flexString        `json:"next_min_id"`
		NextMax     flexString        `json:"next_max_id"`
		HasMore     flexBool          `json:"has_more_comments"`
		HasHeadload flexBool          `json:"has_more_headload_comments"`
		NextChild   flexString        `json:"next_max_child_cursor"`
		HasTail     flexBool          `json:"has_more_tail_child_comments"`
		HasHead     flexBool          `json:"has_more_head_child_comments"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return commentPage{}, err
	}
	var page commentPage
	items := r.Comments
	if replies {
		items = r.Children
		if r.NextChild != "" {
			page.param, page.cursor = "max_id", string(r.NextChild)
		}
		page.more = bool(r.HasTail || r.HasHead) && page.cursor != ""
	} else {
		// The newer answers page with next_min_id; older ones with
		// next_max_id.
		switch {
		case r.NextMin != "":
			page.param, page.cursor = "min_id", string(r.NextMin)
		case r.NextMax != "":
			page.param, page.cursor = "max_id", string(r.NextMax)
		}
		page.more = bool(r.HasMore || r.HasHeadload) && page.cursor != ""
	}
	for _, raw := range items {
		var c feedComment
		if json.Unmarshal(raw, &c) != nil || !digits(string(c.Pk)) || (c.Type != nil && *c.Type == 1) {
			page.unreadable++
			continue
		}
		page.comments = append(page.comments, c)
	}
	return page, nil
}

// mediaPK is a media's numeric pk, which its comments' address names:
// its pk, else its id up to the "_" ("<pk>_<owner>"); "" if neither is
// a number.
func mediaPK(m *feedMedia) string {
	if digits(string(m.Pk)) {
		return string(m.Pk)
	}
	if pk, _, _ := strings.Cut(string(m.ID), "_"); digits(pk) {
		return pk
	}
	return ""
}

// digits reports whether s is a number of 1 to 30 digits, the only thing
// that goes into a comments address.
func digits(s string) bool {
	if s == "" || len(s) > 30 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
