// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"strings"
	"testing"

	"github.com/erictran308/tuimeta/helper/internal/fake"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

func TestFakeThreadsHasTwoFeedsOfEverythingAThreadsPostCanBeAndNoChats(t *testing.T) {
	h := startHelper(t, t.TempDir())
	h.login(proto.Threads)
	chats := h.loadChats(proto.Threads, 20)
	if len(chats) != 0 {
		t.Errorf("Threads has %d chats", len(chats))
	}
	if l := h.c.Call("search", map[string]any{"network": "threads", "query": "maya"}); l.Error == nil || l.Error.Code != proto.Unsupported {
		t.Errorf("search: %s", l.Raw)
	}
	load := func(kind string) []proto.Post {
		var all []proto.Post
		for i := 0; ; i++ {
			p := h.feed(map[string]any{"network": "threads", "limit": 10, "feed": kind})
			if len(p.Posts) > fake.FeedPage {
				t.Fatalf("a page of %d", len(p.Posts))
			}
			all = append(all, p.Posts...)
			if !p.HasMore {
				return all
			}
			if i > 10 {
				t.Fatal("the feed never ends")
			}
		}
	}
	forYou := load("for_you")
	following := load("following")
	if len(forYou) != fake.ThreadsFeedLen || len(following) != fake.ThreadsFeedLen || forYou[0].ID == following[0].ID {
		t.Fatalf("%d and %d posts", len(forYou), len(following))
	}
	var text, longest, many, photo, carousel4, video, quote, card, repost, continues int
	for _, p := range forYou {
		if !strings.HasPrefix(p.URL, "https://www.threads.com/@"+p.Author.Username+"/post/") || p.Likes == nil || p.Reposts == nil || p.Shares == nil {
			t.Errorf("post %+v", p)
		}
		switch {
		case len(p.Media) == 0 && p.Quoted == nil && p.LinkPreview == nil:
			text++
		case len(p.Media) == 4:
			carousel4++
		case len(p.Media) == 1 && p.Media[0].Kind == proto.Video:
			video++
		case len(p.Media) == 1:
			photo++
		}
		if len([]rune(p.Caption)) == fake.ThreadsMaxText {
			longest++
		}
		if strings.Count(p.Caption, "\n") >= 2 {
			many++
		}
		if p.Quoted != nil {
			quote++
			if p.Quoted.ID != 0 || p.Quoted.Quoted != nil {
				t.Errorf("quoted %+v", p.Quoted)
			}
		}
		if p.LinkPreview != nil && p.LinkPreview.Image != nil {
			card++
		}
		if strings.HasSuffix(p.Context, " reposted") {
			repost++
		}
		if p.Continues {
			continues++
		}
	}
	for what, n := range map[string]int{"text": text, "text at the limit": longest, "text of many lines": many, "photo": photo,
		"carousel of four": carousel4, "video": video, "quote": quote, "link card": card, "repost": repost} {
		if n == 0 {
			t.Errorf("no %s", what)
		}
	}
	if continues != 2 {
		t.Errorf("%d posts continue a thread", continues)
	}

	// Replies, some with an attachment, some with replies of their own.
	var post proto.Post
	for _, p := range forYou {
		if p.Comments != nil && *p.Comments >= 20 {
			post = p
			break
		}
	}
	replies := h.comments(map[string]any{"post_id": post.ID})
	replies.Comments = append(replies.Comments, h.comments(map[string]any{"post_id": post.ID, "more": true}).Comments...)
	attached := 0
	var parent proto.Comment
	for _, c := range replies.Comments {
		if c.Attachment != "" {
			attached++
		}
		if c.Replies > 0 && parent.ID == 0 {
			parent = c
		}
	}
	if attached == 0 || parent.ID == 0 {
		t.Fatalf("%d with attachments, a parent %+v", attached, parent)
	}
	under := h.comments(map[string]any{"post_id": post.ID, "replies_of": parent.ID}).Comments
	var nested proto.Comment
	for _, c := range under {
		if c.ReplyTo != parent.ID {
			t.Errorf("reply %+v", c)
		}
		if c.Replies > 0 {
			nested = c
		}
	}
	if nested.ID == 0 {
		t.Fatal("no reply with replies of its own")
	}
	deeper := h.comments(map[string]any{"post_id": post.ID, "replies_of": nested.ID}).Comments
	if len(deeper) != int(nested.Replies) || deeper[0].ReplyTo != nested.ID {
		t.Errorf("replies to a reply: %+v", deeper)
	}
}
