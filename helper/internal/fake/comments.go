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
// comment's replies ReplyPage at a time; only Instagram has them.
func (f *Fake) LoadComments(ctx context.Context, postID, repliesOf int64, more bool) ([]proto.Comment, bool, error) {
	if f.net != proto.Instagram {
		return nil, false, proto.Err(proto.Unsupported, f.net.Title()+"'s posts have no comments in tuimeta.")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	w, err := f.world()
	if err != nil {
		return nil, false, err
	}
	i := int(postID) - 1
	if i < 0 || i >= len(w.feed) || w.feed[i].ID != postID {
		return nil, false, errNoPost
	}
	list, per := w.commentsOf(i), CommentPage
	if repliesOf != 0 {
		at := slices.IndexFunc(list, func(c proto.Comment) bool { return c.ID == repliesOf })
		if at < 0 {
			return nil, false, errNoComment
		}
		list, per = w.repliesOf(i, at), ReplyPage
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

// commentsOf is post i's comments, as many as it says it has (at most
// MaxComments), from the feed's people in turn, a few minutes apart.
func (w *world) commentsOf(i int) []proto.Comment {
	p := w.feed[i]
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

// repliesOf is the replies to post i's comment at index j, oldest first.
func (w *world) repliesOf(i, j int) []proto.Comment {
	parent := w.comments[w.feed[i].ID][j]
	if rs, ok := w.replies[parent.ID]; ok {
		return rs
	}
	rs := make([]proto.Comment, 0, parent.Replies)
	for k := range int(parent.Replies) {
		likes := int64((j + k*5) % 40)
		rs = append(rs, proto.Comment{
			ID:      1_000_000 + w.feed[i].ID*10_000 + int64(j+1)*100 + int64(k) + 1,
			Author:  w.feedAuthors[(i+j+k+1)%len(w.feedAuthors)],
			Date:    parent.Date + int64(k+1)*int64(3*time.Minute/time.Second),
			Text:    replyTexts[(j+k)%len(replyTexts)],
			Likes:   &likes,
			ReplyTo: parent.ID,
		})
	}
	w.replies[parent.ID] = rs
	return rs
}
