// SPDX-License-Identifier: AGPL-3.0-or-later

package instagram

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"go.mau.fi/mautrix-meta/pkg/messagix/httpclient"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/browser"
	"github.com/erictran308/tuimeta/helper/internal/cookies"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// The feed answers below are hand-written in the shape the web client's
// feed request is answered with; nothing here talks to Instagram.

func feedUserJSON(igid, username, name string, verified bool) string {
	return fmt.Sprintf(`{"pk":"%s","pk_id":"%s","username":"%s","full_name":%q,"is_verified":%v,"is_private":false,
		"profile_pic_url":"%s/v/t51.2885-19/%s_n.jpg?stp=dst-jpg_s150x150&_nc_ht=x&oh=sig2","friendship_status":{"following":true}}`,
		igid, igid, username, name, verified, cdn, igid)
}

var (
	mayaFeed = feedUserJSON(mayaIGID, "maya.lens", "Maya Lopez", false)
	samFeed  = feedUserJSON(samIGID, "sam.climbs", "Sam Rivera", true)
)

// cand is an image or video version at width × height, its file named by
// name on the CDN.
func cand(w, h int, name string) string {
	ext := ".jpg"
	if strings.HasPrefix(name, "v") {
		ext = ".mp4"
	}
	return fmt.Sprintf(`{"width":%d,"height":%d,"url":"%s/v/t51.2885-15/%s%s?stp=dst-jpg_e35&efg=eyJ2&_nc_ht=x&oh=sig%d&oe=68F00000"}`,
		w, h, cdn, name, ext, w)
}

func images(cs ...string) string {
	return `{"candidates":[` + strings.Join(cs, ",") + `],"additional_candidates":{"first_frame":` + cs[0] + `}}`
}

func photoPost(id, code, user, caption string, extra string) string {
	if extra != "" {
		extra = "," + extra
	}
	return fmt.Sprintf(`{"taken_at":1759900000,"pk":%s,"id":"%s_%s","code":"%s","media_type":1,"product_type":"feed",
		"user":%s,"caption":%s,"like_count":120,"comment_count":"8","original_width":1440,"original_height":1800,
		"image_versions2":%s,"has_liked":false,"can_viewer_reshare":true%s}`,
		id, id, mayaIGID, code, user, caption, images(cand(1440, 1800, "p"+id+"_1440"), cand(1080, 1350, "p"+id+"_1080"), cand(640, 800, "p"+id+"_640")), extra)
}

func reelPost(id, code string) string {
	return fmt.Sprintf(`{"taken_at":"1759890000","pk":"%s","id":"%s_%s","code":"%s","media_type":2,"product_type":"clips",
		"user":%s,"caption":null,"like_count":"55","comment_count":3,"video_duration":14.7,"has_audio":true,
		"original_width":1080,"original_height":1920,"image_versions2":%s,
		"video_versions":[%s,%s,%s],"play_count":9001}`,
		id, id, samIGID, code, samFeed, images(cand(1080, 1920, "c"+id+"_1080"), cand(540, 960, "c"+id+"_540")),
		cand(1080, 1920, "v"+id+"_1080"), cand(720, 1280, "v"+id+"_720"), cand(480, 854, "v"+id+"_480"))
}

// carouselPost has a photo, a photo off the CDN (dropped) and a video.
func carouselPost(id, code string) string {
	off := `{"width":1080,"height":1080,"url":"https://evil.example/v/t51.2885-15/x.jpg"}`
	return fmt.Sprintf(`{"taken_at":1759880000,"pk":"%s","id":"%s_%s","code":"%s","media_type":8,"product_type":"carousel_container",
		"user":%s,"caption":{"text":"Swipe →"},"like_count":7,"comment_count":0,"carousel_media_count":3,
		"carousel_media":[
			{"id":"%s_1","media_type":1,"original_width":1080,"original_height":1080,"image_versions2":%s},
			{"id":"%s_2","media_type":1,"original_width":1080,"original_height":1080,"image_versions2":{"candidates":[%s]}},
			{"id":"%s_3","media_type":2,"video_duration":3,"original_width":720,"original_height":720,"image_versions2":%s,"video_versions":[%s]}
		]}`,
		id, id, mayaIGID, code, mayaFeed,
		id, images(cand(1080, 1080, "k"+id+"_1")),
		id, off,
		id, images(cand(720, 720, "k"+id+"_3")), cand(720, 720, "v"+id+"_3"))
}

// rawItem before an item puts it in feed_items as it is, not as media.
const rawItem = "\x00raw"

func feedPageJSON(more bool, next string, items ...string) []byte {
	for i, it := range items {
		if raw, ok := strings.CutPrefix(it, rawItem); ok {
			items[i] = raw
		} else if !strings.HasPrefix(it, `{"suggested_users"`) && !strings.HasPrefix(it, `{"end_of_feed`) && !strings.HasPrefix(it, `{"stories_netego`) {
			items[i] = `{"media_or_ad":` + it + `}`
		}
	}
	return fmt.Appendf(nil, `{"num_results":%d,"more_available":%v,"auto_load_more_enabled":true,"next_max_id":%q,
		"feed_items":[%s],"is_direct_v2_enabled":true,"status":"ok"}`, len(items), more, next, strings.Join(items, ","))
}

// feedHarness is a logged-in harness whose feed answers pages by max_id,
// recording when each request came.
func feedHarness(t *testing.T, pages map[string][]byte) (*harness, *[]time.Time) {
	t.Helper()
	h := newHarness(t)
	h.b.feed.gap = time.Millisecond
	var mu sync.Mutex
	var at []time.Time
	h.api.feed = func(maxID string) ([]byte, error) {
		mu.Lock()
		at = append(at, time.Now())
		mu.Unlock()
		page, ok := pages[maxID]
		if !ok {
			t.Errorf("asked for a page after %q", maxID)
			return nil, errors.New("no such page")
		}
		return page, nil
	}
	h.login(mailbox(t, "", false))
	return h, &at
}

func load(t *testing.T, h *harness, fresh bool) ([]proto.Post, bool) {
	t.Helper()
	posts, more, err := h.b.LoadFeed(context.Background(), 10, fresh, backend.Box{})
	if err != nil {
		t.Fatalf("LoadFeed: %v", err)
	}
	return posts, more
}

// fileURL is where a file id would be downloaded from.
func fileURL(t *testing.T, h *harness, id int32) string {
	t.Helper()
	ref, ok := h.deps.Files.Get(id)
	if !ok {
		t.Fatalf("file %d isn't registered", id)
	}
	return ref.Source.(*mediaSource).url
}

func TestTheFeedTurnsPhotosReelsAndCarouselsIntoPosts(t *testing.T) {
	h, _ := feedHarness(t, map[string][]byte{
		"": feedPageJSON(true, "QVFDcursor1",
			photoPost("3400000000000000001", "DAbcPhoto1", mayaFeed, `{"pk":"1","text":"Golden hour 🌅"}`, ""),
			reelPost("3400000000000000002", "DAbcReel02"),
			carouselPost("3400000000000000003", "DAbcCarou3")),
	})
	posts, more := load(t, h, false)
	if !more || len(posts) != 3 {
		t.Fatalf("got %d posts, more %v", len(posts), more)
	}
	photo, reel, carousel := posts[0], posts[1], posts[2]

	if photo.ID <= 0 || photo.ID == reel.ID || reel.ID == carousel.ID {
		t.Errorf("ids %d %d %d", photo.ID, reel.ID, carousel.ID)
	}
	if photo.Author.Name != "Maya Lopez" || photo.Author.Username != "maya.lens" || photo.Author.Verified || photo.Author.Photo == nil {
		t.Errorf("author %+v", photo.Author)
	}
	// The author's picture is the file a chat with them shows.
	avatarKey := fileKey("", fileURL(t, h, photo.Author.Photo.FileID), "avatar")
	if ref, _ := h.deps.Files.Get(photo.Author.Photo.FileID); ref.Key != avatarKey {
		t.Errorf("avatar key %q", ref.Key)
	}
	if photo.Date != 1759900000 || photo.Caption != "Golden hour 🌅" || *photo.Likes != 120 || *photo.Comments != 8 {
		t.Errorf("photo post %+v", photo)
	}
	if photo.URL != "https://www.instagram.com/p/DAbcPhoto1/" {
		t.Errorf("url %s", photo.URL)
	}
	if len(photo.Media) != 1 {
		t.Fatalf("photo media %+v", photo.Media)
	}
	pm := photo.Media[0]
	if pm.Kind != proto.Photo || pm.Thumbnail == nil || pm.Thumbnail.FileID != pm.FileID || pm.Width != 1440 || pm.Height != 1800 || pm.Mime != "image/jpeg" {
		t.Errorf("photo %+v", pm)
	}
	// About 1080 px wide: the smallest at least that wide.
	if u := fileURL(t, h, pm.FileID); !strings.Contains(u, "_1080.jpg") || pm.Thumbnail.Width != 1080 {
		t.Errorf("picked %s", u)
	}

	if reel.URL != "https://www.instagram.com/reel/DAbcReel02/" || reel.Caption != "" || !reel.Author.Verified || reel.Date != 1759890000 {
		t.Errorf("reel %+v", reel)
	}
	if *reel.Likes != 55 || *reel.Comments != 3 {
		t.Errorf("reel counts %d %d", *reel.Likes, *reel.Comments)
	}
	vm := reel.Media[0]
	if vm.Kind != proto.Video || vm.Mime != "video/mp4" || vm.Duration != 15 || vm.Width != 1080 || vm.Height != 1920 {
		t.Errorf("video %+v", vm)
	}
	// The video about 720 px wide, not the largest; the cover about 1080.
	if u := fileURL(t, h, vm.FileID); !strings.Contains(u, "v3400000000000000002_720.mp4") {
		t.Errorf("video picked %s", u)
	}
	if ref, _ := h.deps.Files.Get(vm.FileID); ref.Mime != "video/mp4" {
		t.Errorf("video mime %q", ref.Mime)
	}
	if vm.Thumbnail == nil || !strings.Contains(fileURL(t, h, vm.Thumbnail.FileID), "c3400000000000000002_1080.jpg") {
		t.Errorf("cover %+v", vm.Thumbnail)
	}

	// The photo off Instagram's CDN is left out of the carousel.
	if len(carousel.Media) != 2 || carousel.Media[0].Kind != proto.Photo || carousel.Media[1].Kind != proto.Video || carousel.Media[1].Duration != 3 {
		t.Errorf("carousel %+v", carousel.Media)
	}
	if carousel.Caption != "Swipe →" || carousel.URL != "https://www.instagram.com/p/DAbcCarou3/" {
		t.Errorf("carousel %+v", carousel)
	}
	// Nothing was downloaded, and nothing but the page was asked for.
	if got := h.api.names(); !slices.Equal(got[len(got)-1:], []string{"FeedTimeline"}) || h.api.count("FeedTimeline") != 1 {
		t.Errorf("requests %v", got)
	}
}

func TestAdsAndRowsThatArentPostsAreLeftOut(t *testing.T) {
	ad := func(code, field string) string {
		return photoPost("3400000000000000099", code, mayaFeed, `{"text":"Buy now"}`, field)
	}
	h, _ := feedHarness(t, map[string][]byte{
		"": feedPageJSON(false, "",
			`{"suggested_users":{"type":2,"suggestions":[{"user":`+mayaFeed+`}]}}`,
			ad("AdOne1", `"ad_id":"120200000000","ad_action":"Shop now"`),
			ad("AdTwo2", `"injected":{"label":"Sponsored","show_icon":true}`),
			ad("AdThree3", `"is_ad":true`),
			ad("AdFour4", `"ad_metadata":[{"value":"1","type":1}]`),
			ad("AdFive5", `"product_type":"ad"`),
			photoPost("3400000000000000004", "DAbcKept04", mayaFeed, `null`, `"ad_metadata":[],"injected":null`),
			`{"stories_netego":{"id":"s"}}`,
			`{"end_of_feed_demarcator":{"id":"end","title":"You're all caught up"}}`),
	})
	posts, more := load(t, h, false)
	if more || len(posts) != 1 || posts[0].URL != "https://www.instagram.com/p/DAbcKept04/" {
		t.Fatalf("got %+v, more %v", posts, more)
	}
}

func TestAnOddFieldCostsOnlyItsOwnPost(t *testing.T) {
	h, _ := feedHarness(t, map[string][]byte{
		"": feedPageJSON(false, "",
			// Ids as numbers, counts as strings, a time that isn't one.
			`{"taken_at":{"odd":1},"pk":3400000000000000005,"id":3400000000000000005,"code":"DAbcNumb05","media_type":"1",
				"user":`+mayaFeed+`,"caption":{"text":"ok"},"like_count":"12","comment_count":"x",
				"image_versions2":`+images(cand(1080, 1080, "n5"))+`}`,
			// Pictures that aren't a list: this one goes.
			`{"pk":"3400000000000000006","code":"DAbcBadd06","media_type":1,"image_versions2":"nope"}`,
			// No shortcode, or one that isn't: nowhere to open it.
			photoPost("3400000000000000007", "", mayaFeed, `null`, ""),
			photoPost("3400000000000000008", "../../etc", mayaFeed, `null`, ""),
			photoPost("3400000000000000009", "DAbcLast09", mayaFeed, `null`, "")),
	})
	posts, _ := load(t, h, false)
	if len(posts) != 2 {
		t.Fatalf("got %d posts: %+v", len(posts), posts)
	}
	p := posts[0]
	if p.Date != 0 || *p.Likes != 12 || *p.Comments != 0 || p.Caption != "ok" || len(p.Media) != 1 {
		t.Errorf("post %+v", p)
	}
	if posts[1].URL != "https://www.instagram.com/p/DAbcLast09/" {
		t.Errorf("last %s", posts[1].URL)
	}
}

func TestOnlyFilesOnInstagramsCDNAreOffered(t *testing.T) {
	off := `{"pk":"3400000000000000010","id":"3400000000000000010","code":"DAbcOffs10","media_type":1,
		"user":{"username":"x","profile_pic_url":"https://evil.example/a.jpg"},
		"image_versions2":{"candidates":[{"width":1080,"height":1080,"url":"http://scontent.cdninstagram.com/v/a.jpg"},
			{"width":1080,"height":1080,"url":"https://cdninstagram.com.evil.example/a.jpg"}]}}`
	videoOff := `{"pk":"3400000000000000011","code":"DAbcVoff11","media_type":2,"user":` + samFeed + `,
		"image_versions2":` + images(cand(1080, 1920, "c11")) + `,
		"video_versions":[{"width":720,"height":1280,"url":"https://video.example.com/v.mp4"}]}`
	mixed := `{"pk":"3400000000000000012","code":"DAbcMixd12","media_type":1,
		"user":{"username":"lena.v","full_name":"Lena","profile_pic_url":"https://evil.example/a.jpg"},
		"image_versions2":{"candidates":[{"width":1440,"height":1440,"url":"https://evil.example/big.jpg"},` + cand(640, 640, "m12") + `]}}`
	h, _ := feedHarness(t, map[string][]byte{"": feedPageJSON(false, "", off, videoOff, mixed)})
	posts, _ := load(t, h, false)
	if len(posts) != 2 {
		t.Fatalf("got %+v", posts)
	}
	// A video whose file is elsewhere still shows its cover, with nothing
	// to play.
	if v := posts[0].Media[0]; v.Kind != proto.Video || v.FileID != 0 || v.Thumbnail == nil || !strings.Contains(fileURL(t, h, v.Thumbnail.FileID), "c11.jpg") {
		t.Errorf("video %+v", v)
	}
	// The one on the CDN, though smaller; and no picture of its author.
	p := posts[1]
	if p.Author.Photo != nil || p.Author.Name != "Lena" {
		t.Errorf("author %+v", p.Author)
	}
	if u := fileURL(t, h, p.Media[0].FileID); !strings.Contains(u, "m12.jpg") {
		t.Errorf("picked %s", u)
	}
}

func TestHiddenLikesSendNoCount(t *testing.T) {
	h, _ := feedHarness(t, map[string][]byte{
		"": feedPageJSON(false, "",
			photoPost("3400000000000000013", "DAbcHide13", mayaFeed, `null`, `"like_and_view_counts_disabled":true`),
			`{"pk":"3400000000000000014","code":"DAbcNone14","media_type":1,"user":`+mayaFeed+`,"image_versions2":`+images(cand(1080, 1080, "h14"))+`}`),
	})
	posts, _ := load(t, h, false)
	if posts[0].Likes != nil || posts[0].Comments == nil {
		t.Errorf("hidden: likes %v comments %v", posts[0].Likes, posts[0].Comments)
	}
	if posts[1].Likes != nil || posts[1].Comments != nil {
		t.Errorf("unsaid: likes %v comments %v", posts[1].Likes, posts[1].Comments)
	}
}

func TestCaptionsAndNamesAreCapped(t *testing.T) {
	long := strings.Repeat("é", maxCaption+50)
	name := strings.Repeat("名", 500)
	post := fmt.Sprintf(`{"pk":"3400000000000000015","code":"DAbcLong15","media_type":1,
		"user":{"username":%q,"full_name":%q},"caption":{"text":%q},"image_versions2":%s}`, name, name, long, images(cand(1080, 1080, "l15")))
	h, _ := feedHarness(t, map[string][]byte{"": feedPageJSON(false, "", post)})
	posts, _ := load(t, h, false)
	p := posts[0]
	if utf8.RuneCountInString(p.Caption) != maxCaption || !utf8.ValidString(p.Caption) {
		t.Errorf("caption of %d characters", utf8.RuneCountInString(p.Caption))
	}
	if utf8.RuneCountInString(p.Author.Name) != maxAuthor || utf8.RuneCountInString(p.Author.Username) != maxAuthor {
		t.Errorf("name of %d characters", utf8.RuneCountInString(p.Author.Name))
	}
}

func TestTheFeedRequestAsksForPagesAndNothingAboutViews(t *testing.T) {
	first := feedPageJSON(true, "QVFDcursor1",
		photoPost("3400000000000000016", "DAbcPage16", mayaFeed, `null`, ""),
		photoPost("3400000000000000017", "DAbcPage17", mayaFeed, `null`, ""))
	second := feedPageJSON(false, "",
		photoPost("3400000000000000017", "DAbcPage17", mayaFeed, `null`, ""),
		photoPost("3400000000000000018", "DAbcPage18", mayaFeed, `null`, ""))
	h := newHarness(t)
	h.b.feed.gap = time.Millisecond
	h.api.feed = func(maxID string) ([]byte, error) {
		if maxID == "QVFDcursor1" {
			return second, nil
		}
		return first, nil
	}
	// A session with a device id, as a browser's has.
	set, err := cookies.Parse(proto.Instagram, cookieText+"; ig_did=6B3F-DEVICE-UUID")
	if err != nil {
		t.Fatal(err)
	}
	h.api.inbox = mailbox(t, "", false)
	if err := h.b.LoginCookies(context.Background(), set, browser.Default()); err != nil {
		t.Fatal(err)
	}

	forms := func() []url.Values {
		var out []url.Values
		h.api.mu.Lock()
		defer h.api.mu.Unlock()
		for _, c := range h.api.calls {
			if c.name == "FeedTimeline" {
				out = append(out, c.req.(url.Values))
			}
		}
		return out
	}
	allowed := []string{"device_id", "is_async_ads_double_request", "is_async_ads_in_headload_enabled", "is_async_ads_rti", "rti_delivery_backend", "max_id"}
	check := func(form url.Values, maxID string) {
		t.Helper()
		for k := range form {
			if !slices.Contains(allowed, k) {
				t.Errorf("the request says %s", k)
			}
		}
		for _, k := range []string{"is_async_ads_double_request", "is_async_ads_in_headload_enabled", "is_async_ads_rti", "rti_delivery_backend"} {
			if form.Get(k) != "0" {
				t.Errorf("%s = %q", k, form.Get(k))
			}
		}
		if form.Get("device_id") != "6B3F-DEVICE-UUID" || form.Get("max_id") != maxID || form.Has("max_id") != (maxID != "") {
			t.Errorf("form %v", form)
		}
	}

	posts, more := load(t, h, false)
	if len(posts) != 2 || !more {
		t.Fatalf("first page: %d, more %v", len(posts), more)
	}
	check(forms()[0], "")
	again, more := load(t, h, false)
	if len(again) != 2 || more {
		t.Fatalf("second page: %d, more %v", len(again), more)
	}
	check(forms()[1], "QVFDcursor1")
	// The post on both pages keeps its id.
	if again[0].ID != posts[1].ID || again[1].ID == posts[0].ID || again[1].ID == posts[1].ID {
		t.Errorf("ids %d %d then %d %d", posts[0].ID, posts[1].ID, again[0].ID, again[1].ID)
	}
	// At the end: nothing more is asked for.
	none, more := load(t, h, false)
	if len(none) != 0 || more || none == nil || len(forms()) != 2 {
		t.Errorf("after the end: %v, more %v, %d requests", none, more, len(forms()))
	}
	// A fresh load starts at the top again, with the same ids.
	top, more := load(t, h, true)
	if len(top) != 2 || !more || top[0].ID != posts[0].ID || len(forms()) != 3 {
		t.Fatalf("fresh: %+v, more %v", top, more)
	}
	check(forms()[2], "")
	// Nothing else went to Instagram: no marking, liking or view logging.
	noCalls(t, h.api, "MarkRead", "MarkReadValidation", "SendReaction", "SendMessage", "SetTyping")
}

func TestAnotherLoginStartsTheFeedOverWithNewIDs(t *testing.T) {
	page := feedPageJSON(true, "QVFDcursor1", photoPost("3400000000000000019", "DAbcAgain9", mayaFeed, `null`, ""))
	h, at := feedHarness(t, map[string][]byte{"": page, "QVFDcursor1": page})
	before, _ := load(t, h, false)
	h.login(mailbox(t, "", false))
	after, _ := load(t, h, false)
	if len(*at) != 2 || after[0].ID <= before[0].ID {
		t.Errorf("after another login: %d requests, id %d then %d", len(*at), before[0].ID, after[0].ID)
	}
}

func TestFeedRequestsAreSpacedOut(t *testing.T) {
	page := feedPageJSON(true, "next", photoPost("3400000000000000020", "DAbcGap020", mayaFeed, `null`, ""))
	h, at := feedHarness(t, map[string][]byte{"": page, "next": page})
	h.b.feed.gap = 120 * time.Millisecond
	load(t, h, false)
	load(t, h, false)
	load(t, h, true)
	for i := 1; i < len(*at); i++ {
		if d := (*at)[i].Sub((*at)[i-1]); d < 110*time.Millisecond {
			t.Errorf("request %d came %v after the one before", i, d)
		}
	}
	// Given up while it waits, a load asks for nothing.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, err := h.b.LoadFeed(ctx, 10, false, backend.Box{}); !errors.Is(err, context.DeadlineExceeded) || len(*at) != 3 {
		t.Errorf("got %v after %d requests", err, len(*at))
	}
	if feedGap < 2*time.Second {
		t.Errorf("feedGap is %v", feedGap)
	}
}

func TestAPageOfOnlyAdsAsksForTheNext(t *testing.T) {
	adOnly := func(next string) []byte {
		return feedPageJSON(true, next, photoPost("3400000000000000098", "AdOnly98", mayaFeed, `null`, `"ad_id":"1"`))
	}
	h, at := feedHarness(t, map[string][]byte{
		"":   adOnly("a1"),
		"a1": feedPageJSON(true, "a2", photoPost("3400000000000000021", "DAbcReal21", mayaFeed, `null`, "")),
		"a2": adOnly("a3"),
		"a3": adOnly("a4"),
		"a4": adOnly("a5"),
	})
	posts, more := load(t, h, false)
	if len(posts) != 1 || !more || len(*at) != 2 {
		t.Fatalf("got %d posts after %d requests", len(posts), len(*at))
	}
	// Never more than feedTries requests for one load.
	posts, more = load(t, h, false)
	if len(posts) != 0 || !more || len(*at) != 2+feedTries {
		t.Errorf("got %d posts after %d requests", len(posts), len(*at))
	}
}

func TestAFeedRefusalSaysWhatToDo(t *testing.T) {
	fail := func(msg string) []byte {
		return []byte(`{"message":"` + msg + `","status":"fail"}`)
	}
	bad := fmt.Errorf("%w 400", httpclient.ErrUnexpectedError)
	cases := []struct {
		name string
		body []byte
		err  error
		want *proto.Error
	}{
		{"checkpoint", fail("checkpoint_required"), bad, errCheckpoint},
		{"challenge with ok status code", fail("challenge_required"), nil, errCheckpoint},
		{"logged out", fail("login_required"), nil, errSessionEnded},
		{"too fast", fail("Please wait a few minutes before you try again."), bad, errRateLimited},
		{"429", nil, httpclient.ErrRateLimited, errRateLimited},
		{"redirected to log in", nil, httpclient.ErrTokenInvalidatedRedirect, errSessionEnded},
		{"something else", fail("oops"), nil, proto.Err(proto.NetworkError, errFeed)},
		{"no network", nil, httpclient.ErrRequestFailed, proto.Err(proto.NetworkError, errFeed)},
		{"a page that isn't JSON", []byte("<!DOCTYPE html><html>"), nil, errFeedAnswer},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.b.feed.gap = time.Millisecond
			h.api.feed = func(string) ([]byte, error) { return c.body, c.err }
			h.login(mailbox(t, "", false))
			_, _, err := h.b.LoadFeed(context.Background(), 10, false, backend.Box{})
			var pe *proto.Error
			if !errors.As(err, &pe) || pe.Code != c.want.Code || pe.Message != c.want.Message {
				t.Errorf("got %v, want %v", err, c.want)
			}
			// The library's own words never get through.
			if strings.Contains(err.Error(), "400") || strings.Contains(err.Error(), "oops") {
				t.Errorf("leaked: %v", err)
			}
		})
	}
	// A failed page is asked for again next time, from the same place.
	h, at := feedHarness(t, map[string][]byte{"": feedPageJSON(true, "c1", photoPost("3400000000000000022", "DAbcFail22", mayaFeed, `null`, ""))})
	load(t, h, false)
	failed := true
	h.api.feed = func(maxID string) ([]byte, error) {
		*at = append(*at, time.Now())
		if maxID != "c1" {
			t.Errorf("asked after %q", maxID)
		}
		if failed {
			failed = false
			return nil, httpclient.ErrRequestFailed
		}
		return feedPageJSON(false, ""), nil
	}
	if _, _, err := h.b.LoadFeed(context.Background(), 10, false, backend.Box{}); err == nil {
		t.Fatal("no error")
	}
	if _, more := load(t, h, false); more {
		t.Error("more after the last page")
	}
}

func TestTheFeedNeedsTheAccountConnected(t *testing.T) {
	h := newHarness(t)
	if _, _, err := h.b.LoadFeed(context.Background(), 10, false, backend.Box{}); err != errNotConnected {
		t.Errorf("got %v", err)
	}
	if h.api.count("FeedTimeline") != 0 {
		t.Error("asked Instagram while logged out")
	}
}

func TestFeedHeadersAreTheWebPagesOwn(t *testing.T) {
	h := http.Header{}
	feedHeaders(h, "https://www.instagram.com")
	want := map[string]string{
		"X-Requested-With": "XMLHttpRequest", "Origin": "https://www.instagram.com", "Referer": "https://www.instagram.com/",
		"Sec-Fetch-Dest": "empty", "Sec-Fetch-Mode": "cors", "Sec-Fetch-Site": "same-origin",
	}
	for k, v := range want {
		if h.Get(k) != v {
			t.Errorf("%s = %q", k, h.Get(k))
		}
	}
	if len(h) != len(want) {
		t.Errorf("headers %v", h)
	}
}

func TestCandidatesArePickedBySize(t *testing.T) {
	c := func(w int) feedCandidate {
		return feedCandidate{Width: flexInt(w), Height: flexInt(w), URL: fmt.Sprintf("%s/v/%d.jpg", cdn, w)}
	}
	off := feedCandidate{Width: 1080, URL: "https://evil.example/1080.jpg"}
	for _, tc := range []struct {
		cs        []feedCandidate
		least     int
		nearestTo int
	}{
		{[]feedCandidate{c(1440), c(1080), c(640)}, 1080, 1080},
		{[]feedCandidate{c(640), c(1440), c(1200)}, 1200, 1200},
		{[]feedCandidate{c(320), c(640), off}, 640, 640},
		{[]feedCandidate{c(480), c(960)}, 960, 960},
	} {
		if got, _ := atLeast(tc.cs, photoWidth); int(got.Width) != tc.least {
			t.Errorf("atLeast %v = %d", tc.cs, got.Width)
		}
		if got, _ := nearest(tc.cs, photoWidth); int(got.Width) != tc.nearestTo {
			t.Errorf("nearest %v = %d", tc.cs, got.Width)
		}
	}
	if got, _ := nearest([]feedCandidate{c(1080), c(720), c(480)}, videoWidth); got.Width != 720 {
		t.Errorf("video %d", got.Width)
	}
	// Two as near: the smaller.
	if got, _ := nearest([]feedCandidate{c(820), c(620)}, videoWidth); got.Width != 620 {
		t.Errorf("tie %d", got.Width)
	}
	if _, ok := atLeast([]feedCandidate{off}, photoWidth); ok {
		t.Error("picked a picture off the CDN")
	}
}

func TestTheFeedDrawsAPictureNoBiggerThanItsBoxAndTheViewerTheLargerOne(t *testing.T) {
	h, _ := feedHarness(t, map[string][]byte{
		"": feedPageJSON(true, "QVFDcursor1",
			photoPost("3400000000000000001", "DAbcPhoto1", mayaFeed, `{"pk":"1","text":"hi"}`, ""),
			reelPost("3400000000000000002", "DAbcReel02")),
	})
	posts, _, err := h.b.LoadFeed(context.Background(), 10, false, backend.Box{Width: 600, Height: 600})
	if err != nil || len(posts) != 2 {
		t.Fatalf("got %+v, %v", posts, err)
	}
	// 640×800 is the smallest that fills 600×600; the viewer's is still
	// about 1080 wide.
	pm := posts[0].Media[0]
	if u := fileURL(t, h, pm.Thumbnail.FileID); !strings.Contains(u, "_640.jpg") || pm.Thumbnail.Width != 640 || pm.Thumbnail.Height != 800 {
		t.Errorf("drawn %s %+v", u, pm.Thumbnail)
	}
	if u := fileURL(t, h, pm.FileID); !strings.Contains(u, "_1080.jpg") || pm.Width != 1440 {
		t.Errorf("viewer's %s %+v", u, pm)
	}
	// A video's cover the same way: 540×960 is tall enough.
	vm := posts[1].Media[0]
	if u := fileURL(t, h, vm.Thumbnail.FileID); !strings.Contains(u, "c3400000000000000002_540.jpg") {
		t.Errorf("cover %s", u)
	}
}

func TestEachSizeOfAPictureAtOnePathIsAFileOfItsOwn(t *testing.T) {
	at := func(w, h int) string {
		return fmt.Sprintf(`{"width":%d,"height":%d,"url":"%s/v/t51.2885-15/5521_n.jpg?stp=dst-jpg_e35_p%dx%d&_nc_ht=x&oh=sig%d"}`, w, h, cdn, w, w, w)
	}
	post := `{"pk":"3400000000000000013","code":"DAbcSame13","media_type":1,"user":` + mayaFeed + `,
		"image_versions2":{"candidates":[` + at(1080, 1350) + `,` + at(640, 800) + `]}}`
	h, _ := feedHarness(t, map[string][]byte{"": feedPageJSON(false, "", post)})
	posts, _, err := h.b.LoadFeed(context.Background(), 10, false, backend.Box{Width: 600, Height: 600})
	if err != nil || len(posts) != 1 {
		t.Fatalf("got %+v, %v", posts, err)
	}
	pm := posts[0].Media[0]
	if pm.Thumbnail.FileID == pm.FileID {
		t.Fatalf("one file for both sizes: %+v", pm)
	}
	if u := fileURL(t, h, pm.Thumbnail.FileID); !strings.Contains(u, "p640x640") {
		t.Errorf("drawn %s", u)
	}
	if u := fileURL(t, h, pm.FileID); !strings.Contains(u, "p1080x1080") {
		t.Errorf("viewer's %s", u)
	}
	// Without a box, the one size is one file.
	again, _, _ := h.b.LoadFeed(context.Background(), 10, true, backend.Box{})
	if m := again[0].Media[0]; m.Thumbnail.FileID != m.FileID || m.FileID != pm.FileID {
		t.Errorf("again %+v, first %+v", m, pm)
	}
}

func TestTheVersionDrawnIsTheSmallestThatFillsItsBox(t *testing.T) {
	c := func(w, h int) feedCandidate {
		return feedCandidate{Width: flexInt(w), Height: flexInt(h), URL: fmt.Sprintf("%s/v/%dx%d.jpg", cdn, w, h)}
	}
	most := c(1080, 1350)
	cs := []feedCandidate{c(1440, 1800), most, c(750, 938), c(640, 800), c(320, 400), c(150, 188)}
	for _, tc := range []struct {
		box   backend.Box
		width int
	}{
		{backend.Box{}, 1080},
		{backend.Box{Width: 600, Height: 2000}, 640}, // as wide as the box
		{backend.Box{Width: 2000, Height: 390}, 320}, // as tall
		{backend.Box{Width: 100, Height: 100}, 150},
		{backend.Box{Width: 2400, Height: 3000}, 1080}, // never past the viewer's
	} {
		if got := drawnIn(cs, tc.box, most); int(got.Width) != tc.width {
			t.Errorf("in %v: %d", tc.box, got.Width)
		}
	}
	// A square version is a crop, and one off the CDN isn't downloaded.
	off := feedCandidate{Width: 640, Height: 800, URL: "https://evil.example/640.jpg"}
	if got := drawnIn([]feedCandidate{most, c(640, 640), off}, backend.Box{Width: 600, Height: 600}, most); got != most {
		t.Errorf("picked %+v", got)
	}
}

func TestPostAddressesAreOnlyInstagramsOwn(t *testing.T) {
	for code, want := range map[string]string{
		"DAbc_-9":               "https://www.instagram.com/p/DAbc_-9/",
		"":                      "",
		"a/b":                   "",
		"a?b":                   "",
		"аbc":                   "", // a Cyrillic a
		strings.Repeat("a", 65): "",
	} {
		if got := postURL(code, "feed"); got != want {
			t.Errorf("%q: %q", code, got)
		}
	}
	if got := postURL("DAbc", "clips"); got != "https://www.instagram.com/reel/DAbc/" {
		t.Errorf("reel %q", got)
	}
}
