// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"encoding/binary"
	"image/gif"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erictran308/tuimeta/helper/internal/fake"
	"github.com/erictran308/tuimeta/helper/internal/proto"
	"github.com/erictran308/tuimeta/helper/internal/wiretest"
)

type feedPage struct {
	Posts   []proto.Post `json:"posts"`
	HasMore bool         `json:"has_more"`
}

func (h *helper) feed(params map[string]any) feedPage {
	h.t.Helper()
	return wiretest.Decode[feedPage](h.t, h.ok("load_feed", params))
}

func TestFakeFeedPagesUntilItsEndAndAFreshLoadStartsOver(t *testing.T) {
	h := startHelper(t, t.TempDir())
	if l := h.c.Call("load_feed", map[string]any{"network": "instagram", "limit": 10}); l.Error == nil || l.Error.Code != proto.NotLoggedIn {
		t.Fatalf("logged out: %s", l.Raw)
	}
	h.login(proto.Instagram)
	h.login(proto.Messenger)
	if l := h.c.Call("load_feed", map[string]any{"network": "messenger", "limit": 10}); l.Error == nil || l.Error.Code != proto.Unsupported {
		t.Errorf("messenger: %s", l.Raw)
	}

	var all []proto.Post
	for i := range 3 {
		p := h.feed(map[string]any{"network": "instagram", "limit": 10})
		if len(p.Posts) != fake.FeedPage || p.HasMore != (i < 2) {
			t.Fatalf("page %d: %d posts, has_more %v", i, len(p.Posts), p.HasMore)
		}
		all = append(all, p.Posts...)
	}
	if p := h.feed(map[string]any{"network": "instagram", "limit": 10}); len(p.Posts) != 0 || p.HasMore {
		t.Errorf("past the end: %+v", p)
	}
	seen := map[int64]bool{}
	var carousel3, mixed, videos, uncaptioned, hidden, multiline int
	for i, p := range all {
		if p.ID <= 0 || seen[p.ID] {
			t.Errorf("post %d has id %d", i, p.ID)
		}
		seen[p.ID] = true
		if i > 0 && p.Date >= all[i-1].Date {
			t.Errorf("post %d isn't older than the one before", i)
		}
		if p.Author.Username == "" || p.Author.Photo == nil || len(p.Media) == 0 || !strings.HasPrefix(p.URL, "https://www.instagram.com/") {
			t.Errorf("post %d: %+v", i, p)
		}
		kinds := ""
		for _, m := range p.Media {
			kinds += string(m.Kind[0])
			if m.FileID <= 0 || m.Thumbnail == nil || m.Thumbnail.FileID <= 0 || m.Width <= 0 || m.Height <= 0 {
				t.Errorf("post %d media %+v", i, m)
			}
			if m.Kind == proto.Video && (m.Mime != "image/gif" || m.Duration <= 0 || m.Thumbnail.FileID == m.FileID) {
				t.Errorf("post %d video %+v", i, m)
			}
		}
		switch kinds {
		case "ppp":
			carousel3++
		case "pv":
			mixed++
		case "v":
			videos++
			if !strings.Contains(p.URL, "/reel/") {
				t.Errorf("a reel at %s", p.URL)
			}
		}
		if p.Caption == "" {
			uncaptioned++
		}
		if strings.Count(p.Caption, "\n") >= 3 {
			multiline++
		}
		if p.Likes == nil {
			hidden++
		}
	}
	if carousel3 == 0 || mixed == 0 || videos == 0 || uncaptioned == 0 || hidden == 0 || multiline == 0 {
		t.Errorf("carousels %d, mixed %d, videos %d, no caption %d, likes hidden %d, many lines %d",
			carousel3, mixed, videos, uncaptioned, hidden, multiline)
	}

	// Every fourth is Instagram's suggestion, from someone not known.
	contacts := map[string]bool{}
	for _, p := range all {
		if !p.Suggested {
			contacts[p.Author.Username] = true
		}
	}
	for i, p := range all {
		if p.Suggested != (p.ID%4 == 0) || (p.Suggested && contacts[p.Author.Username]) {
			t.Errorf("post %d (id %d): suggested %v, by %s", i, p.ID, p.Suggested, p.Author.Username)
		}
	}

	// From the top again, the same posts with the same ids.
	top := h.feed(map[string]any{"network": "instagram", "limit": 10, "fresh": true})
	if len(top.Posts) != fake.FeedPage || !top.HasMore || top.Posts[0].ID != all[0].ID || top.Posts[9].ID != all[9].ID {
		t.Errorf("fresh: %d posts, first %d", len(top.Posts), top.Posts[0].ID)
	}
}

func TestFakeFeedVideosAreAnimatedGIFsWithAPNGCover(t *testing.T) {
	dir := t.TempDir()
	h := startHelper(t, dir)
	h.login(proto.Instagram)
	var video *proto.Media
	for _, p := range h.feed(map[string]any{"network": "instagram", "limit": 10}).Posts {
		for i := range p.Media {
			if p.Media[i].Kind == proto.Video && video == nil {
				video = &p.Media[i]
			}
		}
	}
	if video == nil {
		t.Fatal("no video on the first page")
	}
	download := func(id int32) string {
		t.Helper()
		from := h.c.Mark()
		h.ok("download", map[string]any{"file_id": id, "priority": "high"})
		done := h.c.Event("file", from, func(l wiretest.Line) bool {
			f := wiretest.Field[proto.File](t, l, "file")
			return f.ID == id && (f.Done || f.Error != "")
		})
		f := wiretest.Field[proto.File](t, done, "file")
		if f.Path == nil || !strings.HasPrefix(*f.Path, filepath.Join(dir, "fake")) {
			t.Fatalf("download: %s", done.Raw)
		}
		if info, err := os.Stat(*f.Path); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("file on disk: %v %v", info, err)
		}
		return *f.Path
	}
	clip := download(video.FileID)
	if filepath.Ext(clip) != ".gif" {
		t.Errorf("saved as %s", clip)
	}
	data, _ := os.ReadFile(clip)
	anim, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil || len(anim.Image) < 10 || anim.Image[0].Bounds().Dx() != video.Width || anim.Image[0].Bounds().Dy() != video.Height {
		t.Fatalf("not the clip promised: %v", err)
	}
	length := 0
	for _, d := range anim.Delay {
		length += d
	}
	if (length+99)/100 != video.Duration {
		t.Errorf("plays %d hundredths of a second, said %d s", length, video.Duration)
	}
	if video.Audio != 0 {
		t.Errorf("the first video's sound is apart: %d", video.Audio)
	}
	cover := download(video.Thumbnail.FileID)
	data, _ = os.ReadFile(cover)
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil || filepath.Ext(cover) != ".png" || img.Bounds().Dx() != video.Thumbnail.Width || img.Bounds().Dy() != video.Thumbnail.Height {
		t.Fatalf("not the cover promised (%s): %v", cover, err)
	}

	// One video has its sound apart, as a WAV file: a quiet tone as long.
	var apart []proto.Media
	for page := range 3 {
		for _, p := range h.feed(map[string]any{"network": "instagram", "limit": 10, "fresh": page == 0}).Posts {
			for _, m := range p.Media {
				if m.Audio != 0 {
					apart = append(apart, m)
				}
			}
		}
	}
	if len(apart) != 1 || apart[0].Kind != proto.Video || apart[0].Audio == apart[0].FileID {
		t.Fatalf("videos with their sound apart: %+v", apart)
	}
	sound := download(apart[0].Audio)
	if filepath.Ext(sound) != ".wav" {
		t.Errorf("saved as %s", sound)
	}
	wav, _ := os.ReadFile(sound)
	if len(wav) < 44 || string(wav[0:4]) != "RIFF" || string(wav[8:16]) != "WAVEfmt " || string(wav[36:40]) != "data" {
		t.Fatalf("not a WAV file: %q", wav[:min(len(wav), 44)])
	}
	le16 := func(i int) int { return int(int16(binary.LittleEndian.Uint16(wav[i:]))) }
	rate := int(binary.LittleEndian.Uint32(wav[24:]))
	size := int(binary.LittleEndian.Uint32(wav[40:]))
	if le16(20) != 1 || le16(22) != 1 || le16(34) != 16 || size != len(wav)-44 || size/2/rate != apart[0].Duration {
		t.Errorf("format %d, channels %d, bits %d, %d bytes at %d Hz", le16(20), le16(22), le16(34), size, rate)
	}
	peak := 0
	for i := 44; i+1 < len(wav); i += 2 {
		peak = max(peak, abs(le16(i)))
	}
	// About -24 dBFS: quiet, but there.
	if peak < 1500 || peak > 2500 {
		t.Errorf("peak %d", peak)
	}
}

type commentsPage struct {
	Comments []proto.Comment `json:"comments"`
	HasMore  bool            `json:"has_more"`
}

func (h *helper) comments(params map[string]any) commentsPage {
	h.t.Helper()
	return wiretest.Decode[commentsPage](h.t, h.ok("load_comments", params))
}

func TestFakeCommentsComeAPageAtATimeWithTheirReplies(t *testing.T) {
	h := startHelper(t, t.TempDir())
	h.login(proto.Instagram)
	var posts []proto.Post
	for range 3 {
		posts = append(posts, h.feed(map[string]any{"network": "instagram", "limit": 10}).Posts...)
	}
	if l := h.c.Call("load_comments", map[string]any{"post_id": 999}); l.Error == nil || l.Error.Code != proto.NotFound {
		t.Errorf("unknown post: %s", l.Raw)
	}
	verified, manyLines, mention, withReplies := false, false, false, 0
	for _, p := range posts {
		want := min(int(*p.Comments), fake.MaxComments)
		var all []proto.Comment
		page := h.comments(map[string]any{"post_id": p.ID})
		for {
			if len(page.Comments) > fake.CommentPage {
				t.Fatalf("a page of %d", len(page.Comments))
			}
			all = append(all, page.Comments...)
			if !page.HasMore {
				break
			}
			page = h.comments(map[string]any{"post_id": p.ID, "more": true})
		}
		if len(all) != want {
			t.Fatalf("post %d: %d comments, says %d", p.ID, len(all), want)
		}
		for _, c := range all {
			if c.ID <= 0 || c.Author.Username == "" || c.Author.Photo != nil || c.Text == "" || c.ReplyTo != 0 || c.Date < p.Date {
				t.Errorf("post %d comment %+v", p.ID, c)
			}
			verified = verified || c.Author.Verified
			manyLines = manyLines || strings.Count(c.Text, "\n") >= 2
			mention = mention || (strings.Contains(c.Text, "@") && strings.Contains(c.Text, "#"))
			if c.Replies == 0 {
				continue
			}
			withReplies++
			var replies []proto.Comment
			r := h.comments(map[string]any{"post_id": p.ID, "replies_of": c.ID})
			for {
				if len(r.Comments) > fake.ReplyPage {
					t.Fatalf("a page of %d replies", len(r.Comments))
				}
				replies = append(replies, r.Comments...)
				if !r.HasMore {
					break
				}
				r = h.comments(map[string]any{"post_id": p.ID, "replies_of": c.ID, "more": true})
			}
			if int64(len(replies)) != c.Replies || c.Replies > fake.MaxReplies {
				t.Errorf("comment %d: %d replies, says %d", c.ID, len(replies), c.Replies)
			}
			for i, rp := range replies {
				if rp.ReplyTo != c.ID || rp.Replies != 0 || (i > 0 && rp.Date < replies[i-1].Date) {
					t.Errorf("reply %+v", rp)
				}
			}
		}
		// The first page again, the same comments.
		if len(all) > 0 {
			if again := h.comments(map[string]any{"post_id": p.ID}); again.Comments[0].ID != all[0].ID || again.Comments[0].Text != all[0].Text {
				t.Errorf("post %d's first page changed", p.ID)
			}
		}
	}
	if !verified || !manyLines || !mention || withReplies == 0 {
		t.Errorf("verified %v, many lines %v, mention and tag %v, %d with replies", verified, manyLines, mention, withReplies)
	}
	// A comment of another post, or none at all.
	if l := h.c.Call("load_comments", map[string]any{"post_id": posts[2].ID, "replies_of": 5}); l.Error == nil || l.Error.Code != proto.NotFound {
		t.Errorf("unknown comment: %s", l.Raw)
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
