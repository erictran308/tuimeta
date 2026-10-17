// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"fmt"
	"strings"
	"time"

	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// Threads' made-up feeds (PROTOCOL.md, --fake mode): no chats, a "for you"
// feed and a "following" one, of text, photos, videos, quotes, links,
// reposts and a thread of three posts.
const (
	// ThreadsFeedLen is each feed's length.
	ThreadsFeedLen = 30
	// ThreadsMaxText is Threads' own limit on a post's text.
	ThreadsMaxText = 500
)

// threads is the account and the people its feeds are by: nobody to chat
// with.
func threads() networkDef {
	return networkDef{
		self: personDef{"500001", "Robin Hale", "robin.hale", 1, 0},
		people: []personDef{
			{"500002", "Maya Lopez", "maya.lens", 3, 0},
			{"500003", "Sam Rivera", "sam.climbs", 2, 0},
			{"500004", "Noor Aziz", "noor.draws", 5, 0},
			{"500005", "Oliver King", "oliver.k", 6, 0},
		},
	}
}

// threadsPages are accounts "for you" shows besides the people followed.
var threadsPages = []feedAuthor{
	{"Product Notes", "product.notes", true, 2},
	{"Terminal Daily", "terminal.daily", false, 4},
	{"Coast Weekly", "coast.weekly", true, 6},
}

var threadsTexts = []string{
	"Hot take: the best feature is the one you remove.",
	"Morning walk, coffee, and a long list of things I won't do today ☕",
	"Anyone else think dark mode should be the default everywhere?",
	"Shipped it. Now for the part where I find out what I broke.",
	"Reading list for the weekend 👇",
	"Three things I learned building a terminal app:\n\n1. Unicode is hard\n2. Unicode is very hard\n3. Test with emoji ❤️",
	"",
	"New post on the blog about tiny tools that do one thing well.",
	"Sunset from the roof tonight 🌇",
	"Which one would you pick?",
}

// buildThreads makes both feeds, newest first, an hour or so apart,
// registering their files in a fixed order; f.mu is held (or the world is
// being built).
func (f *Fake) buildThreads(w *world) {
	var people, all []feedAuthor
	photos := map[string]*proto.Image{}
	for _, p := range w.people {
		people = append(people, feedAuthor{name: p.name, username: p.username})
		photos[p.username] = p.photo
	}
	all = append(all, people...)
	for _, pg := range threadsPages {
		all = append(all, pg)
		photos[pg.username] = f.picture("avatar:threads-"+pg.username, art{avatarArt, 160, 160, pg.color})
	}
	for _, a := range all {
		w.feedAuthors = append(w.feedAuthors, proto.FeedAuthor{Name: a.name, Username: a.username, Verified: a.verified})
	}
	w.comments, w.replies, w.commentAt = map[int64][]proto.Comment{}, map[int64][]proto.Comment{}, map[[2]int64]int{}
	author := func(a feedAuthor) proto.FeedAuthor {
		return proto.FeedAuthor{Name: a.name, Username: a.username, Verified: a.verified, Photo: photos[a.username]}
	}
	w.feed = f.threadsFeed("fy", 0, all, author)
	w.following = f.threadsFeed("fl", ThreadsFeedLen, people, author)
}

// threadsFeed is one feed: its posts' ids follow first, its authors take
// turns.
func (f *Fake) threadsFeed(tag string, first int, authors []feedAuthor, author func(feedAuthor) proto.FeedAuthor) []proto.Post {
	posts := make([]proto.Post, 0, ThreadsFeedLen)
	for i := range ThreadsFeedLen {
		a := authors[(i*2+first/ThreadsFeedLen)%len(authors)]
		key := fmt.Sprintf("threads:%s:%02d", tag, i)
		p := proto.Post{
			ID:      int64(first + i + 1),
			Author:  author(a),
			Date:    f.anchor.Add(-time.Duration(i)*70*time.Minute - 9*time.Minute).Unix(),
			Caption: threadsTexts[i%len(threadsTexts)],
			URL:     fmt.Sprintf("https://www.threads.com/@%s/post/FAKETH%s%02d", a.username, strings.ToUpper(tag), i),
		}
		switch i % 10 {
		case 1:
			p.Media = []proto.Media{f.feedPhoto(key+":0", wide(i))}
		case 2:
			p.Media = []proto.Media{f.feedPhoto(key+":0", tall(i)), f.feedPhoto(key+":1", tall(i+1)),
				f.feedPhoto(key+":2", square(i+2)), f.feedPhoto(key+":3", tall(i+3))}
		case 3:
			p.Media = []proto.Media{f.feedClip(key+":0", i, false)}
		case 4:
			// A quote of the post before it.
			q := posts[len(posts)-1]
			q.ID, q.Quoted, q.Context, q.Continues = 0, nil, "", false
			p.Quoted = &q
			p.Caption = "Couldn't have said it better."
		case 5:
			p.Caption = threadsTexts[5]
		case 6:
			p.Caption = "Worth a read: tiny tools, big ideas"
			p.LinkPreview = &proto.LinkPreview{
				URL:   "https://example.com/posts/tiny-tools",
				Title: "Tiny tools that do one thing well",
				Image: f.picture(key+":card", art{photoArt, 600, 315, i}),
			}
		case 7:
			p.Context = authors[(i+1)%len(authors)].username + " reposted"
		case 8:
			p.Caption = longThreadsText(i)
		}
		if i%10 == 0 && i > 0 {
			p.Context = "First thread"
		}
		likes, replies := int64((i*91+7)%2400), int64((i*7)%45)
		reposts, shares := int64((i*13)%60), int64((i*29)%300)
		p.Likes, p.Comments, p.Reposts, p.Shares = &likes, &replies, &reposts, &shares
		posts = append(posts, p)
		// A thread of three: this post and two more of its author's after it.
		if i == 11 {
			for k := 1; k <= 2 && len(posts) < ThreadsFeedLen; k++ {
				next := p
				next.ID = int64(first + len(posts) + 1)
				next.Caption = fmt.Sprintf("(%d/3) And another thing about that.", k+1)
				next.Media, next.Context, next.Quoted, next.LinkPreview = nil, "", nil, nil
				next.Continues = true
				next.URL = fmt.Sprintf("https://www.threads.com/@%s/post/FAKETH%s%02dT%d", a.username, strings.ToUpper(tag), i, k)
				posts = append(posts, next)
			}
		}
		if len(posts) == ThreadsFeedLen {
			break
		}
	}
	// The posts the thread pushed past the end aren't made; ids stay in
	// order.
	for j := range posts {
		posts[j].ID = int64(first + j + 1)
	}
	return posts[:min(len(posts), ThreadsFeedLen)]
}

// longThreadsText is a post at Threads' limit, ThreadsMaxText characters.
func longThreadsText(seed int) string {
	var b strings.Builder
	words := []string{"terminal", "threads", "feed", "quiet", "reading", "slowly", "today", "again"}
	for i := 0; b.Len() < ThreadsMaxText; i++ {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(words[(seed+i)%len(words)])
	}
	return b.String()[:ThreadsMaxText]
}
