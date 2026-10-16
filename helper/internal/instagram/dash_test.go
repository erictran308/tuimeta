// SPDX-License-Identifier: AGPL-3.0-or-later

package instagram

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/erictran308/tuimeta/helper/internal/download"
	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// A manifest in the shape Instagram's web feed sends in video_dash_manifest
// (trimmed): three pictures, one of them off the CDN, and two sounds, one
// off the CDN. The audio set says what it is only by its representations'
// mimeType.
func manifest(id string, sound bool) string {
	base := cdn + "/o1/v/t2/f2/m86/AQ" + id
	video := func(w, h, bw int, at string) string {
		return fmt.Sprintf(`<Representation id="%s%dv" bandwidth="%d" codecs="avc1.64001F" mimeType="video/mp4" sar="1:1" FBEncodingTag="dash_h264-basic-gen2_%dp" width="%d" height="%d" FBQualityClass="hd"><BaseURL>%s</BaseURL><SegmentBase indexRange="821-940" timescale="15360"><Initialization range="0-820"/></SegmentBase></Representation>`,
			id, w, bw, w, w, h, at)
	}
	sets := `<AdaptationSet id="0" contentType="video" frameRate="15360/512" subsegmentAlignment="true" par="9:16">` +
		video(1080, 1920, 1855316, base+"_1080.mp4?efg=eyJ2&amp;_nc_ht=scontent&amp;oh=00_sig&amp;oe=68F0") +
		video(720, 1280, 960000, "https://video.example.com/AQ"+id+"_720.mp4") +
		video(640, 1136, 700000, " "+base+"_640.mp4?oh=00_sig&amp;oe=68F0\n") +
		`</AdaptationSet>`
	if sound {
		sets += `<AdaptationSet id="1" subsegmentAlignment="true">` +
			`<Representation id="` + id + `a64" bandwidth="64000" codecs="mp4a.40.5" mimeType="audio/mp4" audioSamplingRate="44100"><AudioChannelConfiguration schemeIdUri="urn:mpeg:dash:23003:3:audio_channel_configuration:2011" value="2"/><BaseURL>` + base + `_audio64.mp4?oh=00_sig&amp;oe=68F0</BaseURL></Representation>` +
			`<Representation id="` + id + `a128" bandwidth="128000" codecs="mp4a.40.2" mimeType="audio/mp4"><BaseURL>https://evil.example/AQ` + id + `_audio128.mp4</BaseURL></Representation>` +
			`</AdaptationSet>`
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" minBufferTime="PT1.500S" type="static" mediaPresentationDuration="PT0H0M9.433S" maxSegmentDuration="PT0H0M2.000S" profiles="urn:mpeg:dash:profile:isoff-on-demand:2011,http://dashif.org/guidelines/dash264"><Period id="0" duration="PT0H0M9.433S">` +
		sets + `</Period></MPD>`
}

// dashReel is a reel with what's given of its video (raw JSON fields).
func dashReel(id, code, video string) string {
	return fmt.Sprintf(`{"taken_at":1759870000,"pk":"%s","id":"%s_%s","code":"%s","media_type":2,"product_type":"clips",
		"user":%s,"caption":{"text":"reel"},"original_width":1080,"original_height":1920,"image_versions2":%s%s}`,
		id, id, samIGID, code, samFeed, images(cand(1080, 1920, "c"+id)), video)
}

func manifestField(m string) string {
	b, _ := json.Marshal(m)
	return `,"video_dash_manifest":` + string(b)
}

func TestAReelSentAsDashPlaysItsPictureWithItsSoundApart(t *testing.T) {
	h, _ := feedHarness(t, map[string][]byte{"": feedPageJSON(false, "",
		dashReel("3400000000000000030", "DAbcDash30", manifestField(manifest("30", true))+`,"video_versions":[]`),
		dashReel("3400000000000000031", "DAbcMute31", manifestField(manifest("31", false))))})
	posts, _ := load(t, h, false)
	if len(posts) != 2 {
		t.Fatalf("got %+v", posts)
	}
	v := posts[0].Media[0]
	// The picture nearest 720 px wide on the CDN (the 720 one isn't), and
	// the best sound there (the better one isn't).
	if v.Kind != proto.Video || !strings.Contains(fileURL(t, h, v.FileID), "AQ30_640.mp4") {
		t.Errorf("picture %+v", v)
	}
	if v.Audio == 0 || !strings.Contains(fileURL(t, h, v.Audio), "AQ30_audio64.mp4") {
		t.Fatalf("sound %d", v.Audio)
	}
	pic, _ := h.deps.Files.Get(v.FileID)
	sound, _ := h.deps.Files.Get(v.Audio)
	if pic.Mime != "video/mp4" || sound.Mime != "audio/mp4" {
		t.Errorf("mimes %q %q", pic.Mime, sound.Mime)
	}
	// Saved as an MP4 and an M4A.
	if !strings.HasSuffix(download.SafeName(sound.Name, sound.Mime), ".m4a") || !strings.HasSuffix(download.SafeName(pic.Name, pic.Mime), ".mp4") {
		t.Errorf("names %s %s", download.SafeName(pic.Name, pic.Mime), download.SafeName(sound.Name, sound.Mime))
	}
	// Its length from the manifest, the post saying none; the cover as ever.
	if v.Duration != 10 || v.Width != 1080 || v.Height != 1920 || v.Mime != "video/mp4" || v.Thumbnail == nil {
		t.Errorf("video %+v", v)
	}
	if !strings.Contains(fileURL(t, h, v.Thumbnail.FileID), "c3400000000000000030.jpg") {
		t.Errorf("cover %s", fileURL(t, h, v.Thumbnail.FileID))
	}
	// Without a sound track, the picture alone.
	if m := posts[1].Media[0]; m.FileID == 0 || m.Audio != 0 {
		t.Errorf("silent %+v", m)
	}
	// On the wire, audio only where there's one.
	raw, _ := json.Marshal(posts)
	if strings.Count(string(raw), `"audio":`) != 1 {
		t.Errorf("wire: %s", raw)
	}
}

func TestAWholeVideoFileWinsOverDash(t *testing.T) {
	both := dashReel("3400000000000000032", "DAbcBoth32", manifestField(manifest("32", true))+
		`,"video_versions":[`+cand(720, 1280, "v32_720")+`]`)
	h, _ := feedHarness(t, map[string][]byte{"": feedPageJSON(false, "", both)})
	posts, _ := load(t, h, false)
	v := posts[0].Media[0]
	if !strings.Contains(fileURL(t, h, v.FileID), "v32_720.mp4") || v.Audio != 0 {
		t.Errorf("got %+v from %s", v, fileURL(t, h, v.FileID))
	}
}

func TestAVideoWithNothingToPlayStillShowsItsCover(t *testing.T) {
	huge := manifest("34", true) + "<!--" + strings.Repeat("x", maxManifest) + "-->"
	h, _ := feedHarness(t, map[string][]byte{"": feedPageJSON(false, "",
		dashReel("3400000000000000033", "DAbcNone33", ""),
		dashReel("3400000000000000034", "DAbcHuge34", manifestField(huge)),
		dashReel("3400000000000000035", "DAbcBad035", manifestField(`<MPD><Period><AdaptationSet contentType="video"><Representation width="720"><BaseURL>`+cdn+`/x.mp4</BaseURL>`)),
		dashReel("3400000000000000036", "DAbcOff036", manifestField(`<MPD><Period><AdaptationSet contentType="video"><Representation width="720"><BaseURL>https://video.example.com/x.mp4</BaseURL></Representation></AdaptationSet></Period></MPD>`)),
		// No cover and nothing to play: nothing to show at all.
		`{"pk":"3400000000000000037","code":"DAbcBare37","media_type":2,"user":`+samFeed+`}`,
		// No cover, but a picture to play: it comes, without a thumbnail.
		`{"pk":"3400000000000000038","code":"DAbcNoCv38","media_type":2,"user":`+samFeed+manifestField(manifest("38", false))+`}`,
		photoPost("3400000000000000039", "DAbcAfter9", mayaFeed, `null`, ""))})
	posts, _ := load(t, h, false)
	if len(posts) != 6 {
		t.Fatalf("got %d posts: %+v", len(posts), posts)
	}
	for _, p := range posts[:4] {
		m := p.Media[0]
		if m.Kind != proto.Video || m.FileID != 0 || m.Audio != 0 || m.Thumbnail == nil || m.Thumbnail.FileID == 0 {
			t.Errorf("%s: %+v", p.URL, m)
		}
	}
	if m := posts[4].Media[0]; m.FileID == 0 || m.Thumbnail != nil || m.Width != 640 || m.Height != 1136 {
		t.Errorf("no cover: %+v", m)
	}
	if posts[5].URL != "https://www.instagram.com/p/DAbcAfter9/" {
		t.Errorf("the page went on to %s", posts[5].URL)
	}
}

func TestAPaidPartnershipIsNoAd(t *testing.T) {
	h, _ := feedHarness(t, map[string][]byte{"": feedPageJSON(false, "",
		photoPost("3400000000000000040", "DAbcPaid40", mayaFeed, `{"text":"with @brand"}`,
			`"is_paid_partnership":true,"sponsor_tags":[{"sponsor":{"username":"brand"},"permission":true}]`))})
	if posts, _ := load(t, h, false); len(posts) != 1 {
		t.Errorf("got %+v", posts)
	}
}

// logBuffer keeps what's logged during a test.
type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestTheLogCountsWhatAPageHeldAndNothingOfIt(t *testing.T) {
	log := &logBuffer{}
	hlog.SetOutput(log)
	t.Cleanup(func() { hlog.SetOutput(io.Discard) })
	h, _ := feedHarness(t, map[string][]byte{"": feedPageJSON(false, "",
		reelPost("3400000000000000041", "DAbcProg41"),
		dashReel("3400000000000000042", "DAbcDash42", manifestField(manifest("42", true))),
		dashReel("3400000000000000043", "DAbcMute43", manifestField(manifest("43", false))),
		dashReel("3400000000000000044", "DAbcNone44", ""),
		photoPost("3400000000000000045", "", mayaFeed, `null`, ""),
		photoPost("3400000000000000046", "AdSecret46", mayaFeed, `null`, `"ad_id":"1"`),
		`{"suggested_users":{"suggestions":[{"user":`+mayaFeed+`}]}}`,
		`{"stories_netego":{"id":"s"}}`,
		rawItem+`{"Weird.Secret Row":{"maya.lens":1}}`,
		rawItem+`{"netego_carousel":{"id":"n"}}`,
		rawItem+`["not","an","object"]`,
		`{"pk":"3400000000000000047","code":"DAbcBroken","image_versions2":"nope"}`)})
	load(t, h, false)
	got := log.String()
	for _, line := range []string{
		"INFO instagram: feed videos progressive=1 dash=1 dash_silent=1 unplayable=1\n",
		"INFO instagram: feed items left out ads=1 unreadable=2\n",
		"INFO instagram: feed posts dropped no_media=0 no_code=1\n",
		"INFO instagram: feed rows netego_carousel=1/0 stories_netego=1/0 suggested_users=1/0 other=1/0\n",
	} {
		if !strings.Contains(got, line) {
			t.Errorf("no line %q in:\n%s", line, got)
		}
	}
	for _, secret := range []string{"weird", "Weird", "Secret", "maya", "Sam", "cdninstagram", "DAbc", "AdSecret", "3400000000", "reel"} {
		if strings.Contains(got, secret) {
			t.Errorf("the log says %q:\n%s", secret, got)
		}
	}
}

func TestManifestsAreReadWithinBounds(t *testing.T) {
	pic, sound, seconds, ok := dashPick(manifest("50", true))
	if !ok || !strings.Contains(pic.url, "AQ50_640.mp4") || pic.width != 640 || pic.height != 1136 || !strings.Contains(sound.url, "AQ50_audio64.mp4") || seconds != 9.433 {
		t.Errorf("got %+v %+v %v %v", pic, sound, seconds, ok)
	}
	// &amp; in a BaseURL is an &.
	if strings.Contains(pic.url, "&amp;") || strings.TrimSpace(pic.url) != pic.url {
		t.Errorf("url %q", pic.url)
	}
	for name, m := range map[string]string{
		"empty":      "",
		"too big":    manifest("51", true) + strings.Repeat(" ", maxManifest),
		"not XML":    "{\"mpd\":1}",
		"unclosed":   `<MPD><Period><AdaptationSet contentType="video"><Representation width="720"><BaseURL>` + cdn + `/x.mp4`,
		"sound only": `<MPD><Period><AdaptationSet contentType="audio"><Representation bandwidth="1"><BaseURL>` + cdn + `/a.mp4</BaseURL></Representation></AdaptationSet></Period></MPD>`,
		"an entity":  `<!DOCTYPE MPD [<!ENTITY x SYSTEM "file:///etc/passwd">]><MPD><Period><AdaptationSet contentType="video"><Representation width="720"><BaseURL>&x;</BaseURL></Representation></AdaptationSet></Period></MPD>`,
	} {
		if _, _, _, ok := dashPick(m); ok {
			t.Errorf("%s: read", name)
		}
	}
	// Ties go to the narrower picture; a representation's own type wins.
	tie := `<MPD><Period><AdaptationSet mimeType="audio/mp4"><Representation mimeType="video/mp4" width="820"><BaseURL>` + cdn + `/820.mp4</BaseURL></Representation><Representation mimeType="video/mp4" width="620"><BaseURL>` + cdn + `/620.mp4</BaseURL></Representation></AdaptationSet></Period></MPD>`
	if pic, sound, _, ok := dashPick(tie); !ok || !strings.HasSuffix(pic.url, "/620.mp4") || sound.url != "" {
		t.Errorf("tie: %+v %+v", pic, sound)
	}
	for in, want := range map[string]float64{"PT0H0M14.700S": 14.7, "PT1M": 60, "PT2H": 7200, "P1D": 0, "PTxS": 0, "PT-3S": 0, "": 0} {
		if got := isoSeconds(in); got != want {
			t.Errorf("isoSeconds(%q) = %v", in, got)
		}
	}
}
