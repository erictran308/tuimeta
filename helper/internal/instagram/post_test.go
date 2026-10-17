// SPDX-License-Identifier: AGPL-3.0-or-later

package instagram

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.mau.fi/mautrix-meta/pkg/messagix/httpclient"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// The post answers below are hand-written in the shape the web client's
// post request is answered with; nothing here talks to Instagram.

// codeOf is the code a post with media pk has in its link.
func codeOf(pk uint64) string {
	var b []byte
	for ; pk > 0; pk /= 64 {
		b = append([]byte{shortcodeAlphabet[pk%64]}, b...)
	}
	return string(b)
}

func mediaInfoJSON(items ...string) []byte {
	return fmt.Appendf(nil, `{"items":[%s],"num_results":%d,"more_available":false,"auto_load_more_enabled":false,"status":"ok"}`,
		strings.Join(items, ","), len(items))
}

func TestAPostsCodeIsItsMediaIDInBase64(t *testing.T) {
	for code, pk := range map[string]string{
		"B": "1", "_": "63", "-": "62", "BA": "64", "BAA": "4096",
		codeOf(3400000000000000001): "3400000000000000001",
		// The most a pk can be, in eleven.
		"H__________": "9223372036854775807",
		// A private account's longer code starts with the post's.
		codeOf(3400000000000000001) + "xYz-_AbC123dEf": "3400000000000000001",
	} {
		if got, ok := shortcodePK(code); !ok || got != pk {
			t.Errorf("%s: %s, %v; want %s", code, got, ok, pk)
		}
	}
	for _, code := range []string{"", "A", "AAAA", "IAAAAAAAAAA", "a/b", "a.b", "ü", "%41", strings.Repeat("B", 65)} {
		if got, ok := shortcodePK(code); ok {
			t.Errorf("%q stands for %s", code, got)
		}
	}
}

// postHarness is a logged-in harness whose post requests mediaInfo answers.
func postHarness(t *testing.T, mediaInfo func(pk string) ([]byte, error)) *harness {
	t.Helper()
	h := newHarness(t)
	h.b.feed.gap = time.Millisecond
	h.api.mediaInfo = mediaInfo
	h.login(mailbox(t, "", false))
	return h
}

// mediaCalls are the MediaInfo requests made, in order.
func mediaCalls(h *harness) []mediaCall {
	h.api.mu.Lock()
	defer h.api.mu.Unlock()
	var out []mediaCall
	for _, c := range h.api.calls {
		if c.name == "MediaInfo" {
			out = append(out, c.req.(mediaCall))
		}
	}
	return out
}

func TestALinkedPostIsAskedForAsItsPageAsksAndKeepsTheFeedsID(t *testing.T) {
	const pk = "3400000000000000003"
	code := codeOf(3400000000000000003)
	post := photoPost(pk, code, mayaFeed, `{"text":"Golden hour"}`, "")
	h := postHarness(t, func(asked string) ([]byte, error) {
		if asked != pk {
			t.Errorf("asked for %s", asked)
		}
		return mediaInfoJSON(post), nil
	})
	p, err := h.b.LoadPost(context.Background(), code, "maya.lens", backend.Box{Width: 320, Height: 400})
	if err != nil {
		t.Fatalf("LoadPost: %v", err)
	}
	if p.ID == 0 || p.URL != "https://www.instagram.com/p/"+code+"/" || p.Caption != "Golden hour" ||
		p.Author.Username != "maya.lens" || len(p.Media) != 1 || p.Suggested {
		t.Errorf("post %+v", p)
	}
	// The picture drawn is the one its box needs, as the feed's.
	if got := fileURL(t, h, p.Media[0].Thumbnail.FileID); !strings.Contains(got, "_640") {
		t.Errorf("thumbnail %s", got)
	}
	if calls := mediaCalls(h); len(calls) != 1 || calls[0].referer != "https://www.instagram.com/p/"+code+"/" {
		t.Errorf("calls %+v", calls)
	}
	// The feed giving it later gives it the same id.
	h.api.feed = func(string) ([]byte, error) { return feedPageJSON(false, "", post), nil }
	posts, _ := load(t, h, false)
	if len(posts) != 1 || posts[0].ID != p.ID {
		t.Errorf("feed %+v, linked %d", posts, p.ID)
	}
	// Its comments load as the feed's do.
	h.api.comments = func(path string, _ url.Values) ([]byte, error) {
		if path != "/api/v1/media/"+pk+"/comments/" {
			t.Errorf("comments at %s", path)
		}
		return topPage(false, "", ""), nil
	}
	if _, _, err := h.b.LoadComments(context.Background(), p.ID, 0, false); err != nil {
		t.Errorf("LoadComments: %v", err)
	}
}

func TestALinkedPostThatCantBeShownSaysWhy(t *testing.T) {
	const pk = "3400000000000000004"
	code := codeOf(3400000000000000004)
	fail := func(msg string) []byte { return []byte(`{"message":"` + msg + `","status":"fail"}`) }
	bad := fmt.Errorf("%w 400", httpclient.ErrUnexpectedError)
	cases := []struct {
		name string
		body []byte
		err  error
		want *proto.Error
	}{
		{"gone", fail("Media not found or unavailable"), bad, errPostGone},
		{"gone with an ok status", fail("Media not found or unavailable"), nil, errPostGone},
		{"another post", mediaInfoJSON(photoPost(pk, "Bother", mayaFeed, `null`, "")), nil, errPostGone},
		{"no post", mediaInfoJSON(), nil, errPostGone},
		{"nothing to show", mediaInfoJSON(fmt.Sprintf(`{"pk":"%s","id":"%s_1","code":"%s","media_type":1,"user":%s}`, pk, pk, code, mayaFeed)), nil, errPostEmpty},
		{"checkpoint", fail("checkpoint_required"), bad, errCheckpoint},
		{"logged out", fail("login_required"), nil, errSessionEnded},
		{"no network", nil, httpclient.ErrRequestFailed, proto.Err(proto.NetworkError, errPost)},
		{"a page that isn't JSON", []byte("<!DOCTYPE html><html>"), nil, errPostAnswer},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := postHarness(t, func(string) ([]byte, error) { return c.body, c.err })
			_, err := h.b.LoadPost(context.Background(), code, "", backend.Box{})
			var pe *proto.Error
			if !errors.As(err, &pe) || pe.Code != c.want.Code || pe.Message != c.want.Message {
				t.Errorf("got %v, want %v", err, c.want)
			}
		})
	}
	// A code that stands for no post isn't asked for.
	h := postHarness(t, nil)
	if _, err := h.b.LoadPost(context.Background(), "AAAA", "", backend.Box{}); err != errNotAPostCode {
		t.Errorf("got %v", err)
	}
	if len(mediaCalls(h)) != 0 {
		t.Error("asked Instagram")
	}
	// Nor anything while logged out.
	h = newHarness(t)
	if _, err := h.b.LoadPost(context.Background(), code, "", backend.Box{}); err != errNotConnected {
		t.Errorf("got %v", err)
	}
	if len(mediaCalls(h)) != 0 {
		t.Error("asked Instagram while logged out")
	}
}
