// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"context"
	"slices"
	"time"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// Instagram's made-up comments (PROTOCOL.md, --fake mode).
const (
	MaxComments = 40
	CommentPage = 10
	MaxReplies  = 12
	ReplyPage   = 5
)

var _ backend.Commenter = (*Fake)(nil)

var (
	errNoPost    = proto.Err(proto.NotFound, "That post isn't loaded; load the feed again.")
	errNoComment = proto.Err(proto.NotFound, "That comment isn't loaded; load the post's comments again.")
)

var commentTexts = []string{
	"so good 😍",
	"Where is this?",
	"@maya.lens we have to go here #weekendplans",
	"Saving this for later",
	"🔥🔥🔥",
	"The light in this one…",
	"Okay this made my day.\nThe second one especially.\n\nTeach me your ways",
	"How long did that take?",
	"Same spot as last year?",
	"Wow",
}

var replyTexts = []string{"Right?!", "Agreed", "@sam.climbs you need to see this", "Haha yes", "😂", "Next time!", "Thank you!"}

// LoadComments gives a post's comments CommentPage at a time, or a
// comment's replies ReplyPage at a time; only Instagram and Threads have
// them. On Threads a reply can have replies too.
func (f *Fake) LoadComments(ctx context.Context, postID, repliesOf int64, more bool) ([]proto.Comment, bool, error) {
	if f.net != proto.Instagram && f.net != proto.Threads {
		return nil, false, proto.Err(proto.Unsupported, f.net.Title()+"'s posts have no comments in tuimeta.")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	w, err := f.world()
	if err != nil {
		return nil, false, err
	}
	i, ok := w.postIndex(postID)
	if !ok {
		return nil, false, errNoPost
	}
	list, per := w.commentsOf(i, f.net == proto.Threads), CommentPage
	if repliesOf != 0 {
		parent, ok := w.findComment(i, repliesOf)
		if !ok {
			return nil, false, errNoComment
		}
		list, per = w.repliesTo(i, parent, f.net == proto.Threads), ReplyPage
	}
	key := [2]int64{postID, repliesOf}
	from := 0
	if more {
		from = w.commentAt[key]
	}
	end := min(from+per, len(list))
	w.commentAt[key] = end
	return slices.Clone(list[from:end]), end < len(list), nil
}

// posts are both feeds' posts, "for you" first; a post's index in it is
// its id less one.
func (w *world) posts() []proto.Post { return slices.Concat(w.feed, w.following) }

// postIndex is post id's index in posts.
func (w *world) postIndex(id int64) (int, bool) {
	i := int(id) - 1
	all := w.posts()
	return i, i >= 0 && i < len(all) && all[i].ID == id
}

// findComment is one of post i's comments, or a reply to one that has
// been given.
func (w *world) findComment(i int, id int64) (proto.Comment, bool) {
	post := w.posts()[i]
	for _, c := range w.comments[post.ID] {
		if c.ID == id {
			return c, true
		}
	}
	for _, rs := range w.replies {
		for _, r := range rs {
			if r.ID == id && w.onPost(r, post.ID) {
				return r, true
			}
		}
	}
	return proto.Comment{}, false
}

// onPost reports whether reply c is under post id's comments.
func (w *world) onPost(c proto.Comment, id int64) bool {
	for range 8 {
		for _, top := range w.comments[id] {
			if top.ID == c.ReplyTo {
				return true
			}
		}
		parent, ok := w.replyByID(c.ReplyTo)
		if !ok {
			return false
		}
		c = parent
	}
	return false
}

func (w *world) replyByID(id int64) (proto.Comment, bool) {
	for _, rs := range w.replies {
		for _, r := range rs {
			if r.ID == id {
				return r, true
			}
		}
	}
	return proto.Comment{}, false
}

// threadsReplyTexts are what Threads' made-up replies say.
var threadsReplyTexts = []string{
	"This is the take I needed today",
	"Respectfully, no 😂",
	"@sam.climbs thoughts?",
	"Saving this thread",
	"Same here. Took me years to figure it out.\nStill learning.",
	"Source?",
	"🔥",
	"Okay but the second post in this thread is the real one",
}

// commentsOf is post i's comments, as many as it says it has (at most
// MaxComments), from the feed's people in turn, a few minutes apart. On
// Threads (threads), they're replies: some with a photo or a video, some
// with replies of their own.
func (w *world) commentsOf(i int, threads bool) []proto.Comment {
	p := w.posts()[i]
	if cs, ok := w.comments[p.ID]; ok {
		return cs
	}
	n := 0
	if p.Comments != nil {
		n = int(min(*p.Comments, MaxComments))
	}
	cs := make([]proto.Comment, 0, n)
	for j := range n {
		c := proto.Comment{
			ID:     p.ID*1000 + int64(j) + 1,
			Author: w.feedAuthors[(i+j*2)%len(w.feedAuthors)],
			Date:   p.Date + int64(j+1)*int64(11*time.Minute/time.Second),
			Text:   commentTexts[(i+j)%len(commentTexts)],
		}
		if threads {
			c.Text = threadsReplyTexts[(i+j)%len(threadsReplyTexts)]
			switch j % 7 {
			case 2:
				c.Attachment = proto.AttachPhoto
			case 5:
				c.Attachment = proto.AttachVideo
			case 6:
				c.Attachment, c.Text = proto.AttachCarousel, ""
			}
		}
		if j%6 != 5 { // some say no likes
			likes := int64((i*7 + j*13) % 300)
			c.Likes = &likes
		}
		if j%4 == 1 {
			c.Replies = int64((i*3 + j) % (MaxReplies + 1))
		}
		cs = append(cs, c)
	}
	w.comments[p.ID] = cs
	return cs
}

// repliesTo is the replies to parent, a comment on post i or (on Threads)
// a reply under one, oldest first. On Threads, every third reply to a
// comment has two replies of its own.
func (w *world) repliesTo(i int, parent proto.Comment, threads bool) []proto.Comment {
	if rs, ok := w.replies[parent.ID]; ok {
		return rs
	}
	// j is the comment's place among its post's (as Instagram's made-up
	// replies have always been numbered by), or for a reply its id's end.
	post := w.posts()[i].ID
	j := int(parent.ID%1000) - 1
	if parent.ReplyTo != 0 {
		j = int(parent.ID % 100)
	}
	rs := make([]proto.Comment, 0, parent.Replies)
	for k := range int(parent.Replies) {
		likes := int64((j + k*5) % 40)
		r := proto.Comment{
			ID:      1_000_000 + post*10_000 + int64(j+1)*100 + int64(k) + 1,
			Author:  w.feedAuthors[(i+j+k+1)%len(w.feedAuthors)],
			Date:    parent.Date + int64(k+1)*int64(3*time.Minute/time.Second),
			Text:    replyTexts[(j+k)%len(replyTexts)],
			Likes:   &likes,
			ReplyTo: parent.ID,
		}
		if parent.ReplyTo != 0 {
			// Replies to a reply (Threads'): from 100 000 000 up, so they
			// never meet a comment's.
			r.ID = 100_000_000 + parent.ID*10 + int64(k) + 1
		}
		if threads && parent.ReplyTo == 0 && k%3 == 0 {
			r.Replies = 2
		}
		rs = append(rs, r)
	}
	w.replies[parent.ID] = rs
	return rs
}
