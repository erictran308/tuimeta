// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// Instagram's made-up home feed (PROTOCOL.md, --fake mode).
const (
	FeedLen  = 30
	FeedPage = 10
	// ClipSeconds is how long a fake video's GIF plays, rounded up.
	ClipSeconds = 2
	// SoundApart is the post (from 0) whose video has its sound in a file
	// of its own.
	SoundApart = 7
)

var _ backend.Feeder = (*Fake)(nil)

// LoadFeed gives the next FeedPage posts; only Instagram has a feed.
func (f *Fake) LoadFeed(ctx context.Context, _ int, fresh bool) ([]proto.Post, bool, error) {
	if f.net != proto.Instagram {
		return nil, false, proto.Err(proto.Unsupported, f.net.Title()+" has no feed in tuimeta.")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	w, err := f.world()
	if err != nil {
		return nil, false, err
	}
	if fresh {
		w.feedAt = 0
	}
	end := min(w.feedAt+FeedPage, len(w.feed))
	posts := slices.Clone(w.feed[w.feedAt:end])
	w.feedAt = end
	return posts, end < len(w.feed), nil
}

// feedAuthor is who posted: one of the network's people, or a page only
// the feed knows.
type feedAuthor struct {
	name, username string
	verified       bool
	color          int // for a page's avatar; people have their own
}

var feedPages = []feedAuthor{
	{"Trail Notes", "trail.notes", true, 2},
	{"Kitchen Table", "kitchen.table", false, 5},
}

// suggestedPages are the accounts every fourth post comes from, as
// Instagram's "Suggested for you": nobody the fake account knows.
var suggestedPages = []feedAuthor{
	{"Coast Weekly", "coast.weekly", true, 6},
	{"Alder Studio", "alder.studio", false, 3},
	{"Night Market Eats", "nightmarket.eats", false, 0},
}

var feedCaptions = []string{
	"Golden hour at the lake 🌅",
	"Three days, one backpack, no regrets. Swipe for the summit →",
	"Morning laps 🧗‍♀️",
	"",
	"New route! Photo first, then the send.",
	"A long one, sorry.\n\nWe left before sunrise and the trail was still frozen. By noon it was mud.\nBy three it was snowing again.\n\nWould do it all again tomorrow.\n#hiking #mountains #notasingledryfoot",
	"Sunday bread 🍞",
	"Watch till the end 👀",
	"Sketchbook, page 41",
	"Before / after",
}

// buildFeed makes the feed's posts, newest first, a few hours apart over
// the last week, registering their files in a fixed order; f.mu is held
// (or the world is being built).
func (f *Fake) buildFeed(w *world) {
	if f.net != proto.Instagram {
		return
	}
	var authors []feedAuthor
	photos := map[string]*proto.Image{}
	for _, p := range w.people {
		if p.username == "deals.4.you" {
			continue // the stranger with a message request posts nothing here
		}
		authors = append(authors, feedAuthor{name: p.name, username: p.username})
		photos[p.username] = p.photo
	}
	for _, pg := range feedPages {
		authors = append(authors, pg)
		photos[pg.username] = f.picture("avatar:feed-"+pg.username, art{avatarArt, 160, 160, pg.color})
	}
	for _, a := range authors {
		w.feedAuthors = append(w.feedAuthors, proto.FeedAuthor{Name: a.name, Username: a.username, Verified: a.verified})
	}
	for _, pg := range suggestedPages {
		photos[pg.username] = f.picture("avatar:feed-"+pg.username, art{avatarArt, 160, 160, pg.color})
	}
	w.comments, w.replies, w.commentAt = map[int64][]proto.Comment{}, map[int64][]proto.Comment{}, map[[2]int64]int{}
	w.feed = make([]proto.Post, 0, FeedLen)
	for i := range FeedLen {
		a := authors[(i*3)%len(authors)]
		suggested := i%4 == 3 // posts 4, 8, …
		if suggested {
			a = suggestedPages[(i/4)%len(suggestedPages)]
		}
		key := fmt.Sprintf("feed:%02d", i)
		var media []proto.Media
		video := false
		switch i % 10 {
		case 0, 8:
			media = []proto.Media{f.feedPhoto(key+":0", tall(i))}
		case 1:
			media = []proto.Media{f.feedPhoto(key+":0", square(i)), f.feedPhoto(key+":1", wide(i+1)), f.feedPhoto(key+":2", tall(i+2))}
		case 2, 7:
			// One reel's sound comes apart, as Instagram's DASH videos' does.
			media, video = []proto.Media{f.feedClip(key+":0", i, i == SoundApart)}, true
		case 3, 6:
			media = []proto.Media{f.feedPhoto(key+":0", square(i))}
		case 4:
			media = []proto.Media{f.feedPhoto(key+":0", wide(i)), f.feedClip(key+":1", i, false)}
		case 5:
			media = []proto.Media{f.feedPhoto(key+":0", wide(i))}
		case 9:
			media = []proto.Media{f.feedPhoto(key+":0", tall(i)), f.feedPhoto(key+":1", tall(i+3))}
		}
		kind, code := "p", fmt.Sprintf("FAKEPOST%02d", i)
		if video {
			kind = "reel"
		}
		p := proto.Post{
			ID:        int64(i + 1),
			Author:    proto.FeedAuthor{Name: a.name, Username: a.username, Verified: a.verified, Photo: photos[a.username]},
			Date:      f.anchor.Add(-time.Duration(i)*5*time.Hour - 37*time.Minute).Unix(),
			Caption:   feedCaptions[i%len(feedCaptions)],
			Media:     media,
			URL:       "https://www.instagram.com/" + kind + "/" + code + "/",
			Suggested: suggested,
		}
		if i%10 != 6 { // the one with its likes hidden
			likes := int64((i*137 + 12) % 5000)
			p.Likes = &likes
		}
		comments := int64((i * 7) % 60)
		p.Comments = &comments
		w.feed = append(w.feed, p)
	}
}

func (f *Fake) feedPhoto(key string, ms mediaSpec) proto.Media {
	img := f.picture(key, ms.art)
	return proto.Media{Kind: proto.Photo, FileID: img.FileID, Thumbnail: img, Width: img.Width, Height: img.Height, Mime: "image/png"}
}

// feedClip is a video: a short animated GIF, which a player plays as one
// without anything fetched, with a PNG of its first frame as its cover;
// with sound, a quiet tone as long, in a WAV file of its own (audio).
func (f *Fake) feedClip(key string, seed int, sound bool) proto.Media {
	c := clip{w: 320, h: 400, seed: seed}
	name := strings.ReplaceAll(key, ":", "-")
	id := f.d.Files.Register(ids.FileRef{
		Network: f.net, Key: key, Mime: "image/gif", Name: name + ".gif",
		Source: &fakeFile{gen: c.gif},
	})
	cover := f.d.Files.Register(ids.FileRef{
		Network: f.net, Key: key + ":still", Mime: "image/png", Name: name + "-still.png",
		Source: &fakeFile{gen: c.still},
	})
	m := proto.Media{
		Kind: proto.Video, FileID: id, Thumbnail: &proto.Image{FileID: cover, Width: c.w, Height: c.h},
		Width: c.w, Height: c.h, Duration: ClipSeconds, Mime: "image/gif",
	}
	if sound {
		m.Audio = f.d.Files.Register(ids.FileRef{
			Network: f.net, Key: key + ":sound", Mime: "audio/wav", Name: name + "-sound.wav",
			Source: &fakeFile{gen: func() []byte { return tone(ClipSeconds) }},
		})
	}
	return m
}
