// SPDX-License-Identifier: AGPL-3.0-or-later

package instagram

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/erictran308/tuimeta/helper/internal/hlog"
)

// Suggested posts come in rows other than media_or_ad, in shapes that
// aren't known for sure: these are guesses at the likely ones.

// sugg is media dict n, a photo post with its own id and shortcode.
func sugg(n int, extra string) string {
	return photoPost(fmt.Sprintf("34000000000000001%02d", n), fmt.Sprintf("DAbcSugg%02d", n), samFeed, `null`, extra)
}

func row(key, value string) string {
	return rawItem + `{"` + key + `":` + value + `}`
}

func TestPostsInOtherRowsComeAsSuggested(t *testing.T) {
	log := &logBuffer{}
	hlog.SetOutput(log)
	t.Cleanup(func() { hlog.SetOutput(io.Discard) })
	h, _ := feedHarness(t, map[string][]byte{"": feedPageJSON(false, "",
		sugg(1, ""), // a media_or_ad item
		row("explore_story", `{"media":`+sugg(2, "")+`,"explore_item_info":{"num_columns":1,"aspect_ratio":1}}`),
		row("explore_story", `{"media_or_ad":`+sugg(3, "")+`}`),
		// A unit of several, under a list.
		row("explore_story", `{"title":"Suggested reels","items":[{"media":`+sugg(4, "")+`},{"media":`+sugg(5, "")+`}]}`),
		// Two levels down.
		row("explore_story", `{"layout":{"content":`+sugg(6, "")+`}}`),
		// A suggested ad stays out.
		row("explore_story", `{"media":`+sugg(7, `"ad_id":"120200000000"`)+`}`),
		// Nothing in it.
		row("explore_story", `{"id":"x","title":"Suggested for you"}`),
		// A row of a name not known, holding a post.
		row("clips_netego", `{"clips":[{"media":`+sugg(8, "")+`}]}`),
		// The media_or_ad post again: one post, as it first came.
		row("explore_story", `{"media":`+sugg(1, "")+`}`),
		// Four levels down: too deep to look.
		row("explore_story", `{"a":{"b":{"c":{"media":`+sugg(9, "")+`}}}}`),
		// Marked suggested in a media_or_ad item itself.
		sugg(10, `"is_suggested":true`),
		// A row of people to follow isn't searched.
		row("suggested_users", `{"media":`+sugg(11, "")+`}`),
		// A carousel is one post, its parts not more.
		row("explore_story", `{"media":`+carouselPost("3400000000000000112", "DAbcSugg12")+`}`))})
	posts, _ := load(t, h, false)
	var got []string
	for _, p := range posts {
		code := strings.TrimSuffix(strings.TrimPrefix(p.URL, "https://www.instagram.com/p/"), "/")
		got = append(got, fmt.Sprintf("%s:%v", code, p.Suggested))
	}
	want := "DAbcSugg01:false DAbcSugg02:true DAbcSugg03:true DAbcSugg04:true DAbcSugg05:true DAbcSugg06:true DAbcSugg08:true DAbcSugg10:true DAbcSugg12:true"
	if strings.Join(got, " ") != want {
		t.Errorf("got  %s\nwant %s", strings.Join(got, " "), want)
	}
	if last := posts[len(posts)-1]; len(last.Media) != 2 {
		t.Errorf("the carousel: %+v", last.Media)
	}
	raw, _ := json.Marshal(posts[0])
	if !strings.Contains(string(raw), `"suggested":false`) {
		t.Errorf("wire: %s", raw)
	}
	for _, line := range []string{
		"INFO instagram: feed rows clips_netego=1/1 explore_story=9/7 suggested_users=1/0\n",
		"INFO instagram: feed items left out ads=1 unreadable=0\n",
	} {
		if !strings.Contains(log.String(), line) {
			t.Errorf("no line %q in:\n%s", line, log.String())
		}
	}
}

func TestARowGivesAtMostTwentyPosts(t *testing.T) {
	var many []string
	for n := range 25 {
		many = append(many, sugg(20+n, ""))
	}
	h, _ := feedHarness(t, map[string][]byte{"": feedPageJSON(false, "",
		row("explore_story", `{"medias":[`+strings.Join(many, ",")+`]}`))})
	if posts, _ := load(t, h, false); len(posts) != rowPosts {
		t.Errorf("got %d posts", len(posts))
	}
}

func TestOnlyPlainFieldNamesOfRowsAreLogged(t *testing.T) {
	for k, want := range map[string]bool{
		"explore_story": true, "clips_netego2": true, "a": true,
		"": false, "Explore": false, "maya.lens": false, "has space": false, "_x": false, "9lives": false,
		strings.Repeat("a", 40): true, strings.Repeat("a", 41): false, "café": false,
	} {
		if schemaName(k) != want {
			t.Errorf("schemaName(%q) = %v", k, !want)
		}
	}
	// Past eight kinds, the rest are "other", with the ones that aren't
	// plain names.
	rows := map[string]*rowCount{"Not.Plain": {rows: 2, withPosts: 1}}
	for i := range 10 {
		rows[fmt.Sprintf("row_%02d", i)] = &rowCount{rows: 1, withPosts: i % 2}
	}
	var line []string
	for _, a := range rowAttrs(rows) {
		line = append(line, fmt.Sprint(a))
	}
	got := strings.Join(line, " ")
	if !strings.Contains(got, "row_07") || strings.Contains(got, "row_08") || strings.Contains(got, "Not.Plain") || len(line) != 9 {
		t.Errorf("got %s", got)
	}
	// row_08 (1/0), row_09 (1/1) and Not.Plain (2/1) as other: 4/2.
	if !strings.HasSuffix(got, "other 4/2}") {
		t.Errorf("other: %s", got)
	}
}
