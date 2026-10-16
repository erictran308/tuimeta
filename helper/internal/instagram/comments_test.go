// SPDX-License-Identifier: AGPL-3.0-or-later

package instagram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"go.mau.fi/mautrix-meta/pkg/messagix/httpclient"

	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// The comment answers below are hand-written in the shape the web client's
// comments requests are answered with; nothing here talks to Instagram.

const mediaPKOne = "3400000000000000001"

func commentJSON(pk any, user, text string, created int64, likes, children int, extra string) string {
	if extra != "" {
		extra = "," + extra
	}
	pkJSON := fmt.Sprintf("%v", pk)
	if s, ok := pk.(string); ok {
		pkJSON = fmt.Sprintf("%q", s)
	}
	return fmt.Sprintf(`{"pk":%s,"user_id":"34000000003","text":%q,"type":0,"created_at":%d,"created_at_utc":%d,
		"content_type":"comment","status":"Active","bit_flags":0,"did_report_as_spam":false,"share_enabled":false,
		"user":%s,"is_covered":false,"is_ranked_comment":true,"media_id":"%s","has_liked_comment":false,
		"comment_like_count":%d,"child_comment_count":%d%s}`,
		pkJSON, text, created, created, user, mediaPKOne, likes, children, extra)
}

func topPage(more bool, cursorField, cursor string, comments ...string) []byte {
	c := ""
	if cursorField != "" {
		c = fmt.Sprintf(`,%q:%q`, cursorField, cursor)
	}
	return fmt.Appendf(nil, `{"comments":[%s],"comment_count":40,"caption":{"pk":"1","text":"Golden hour"},
		"caption_is_edited":false,"has_more_comments":%v,"has_more_headload_comments":false,"media_header_display":"none",
		"can_view_more_preview_comments":false%s,"status":"ok"}`, strings.Join(comments, ","), more, c)
}

func childPage(more bool, cursor string, comments ...string) []byte {
	c := `"next_max_child_cursor":null`
	if cursor != "" {
		c = fmt.Sprintf(`"next_max_child_cursor":%q`, cursor)
	}
	return fmt.Appendf(nil, `{"child_comments":[%s],"child_comment_count":3,"has_more_tail_child_comments":%v,%s,
		"has_more_head_child_comments":false,"next_min_child_cursor":null,"num_tail_child_comments":1,
		"parent_comment":{"pk":"17900000000000001"},"status":"ok"}`, strings.Join(comments, ","), more, c)
}

// commentsHarness is a logged-in harness with the feed loaded (two posts:
// mediaPKOne's and another), answering Comments by path and the query's
// cursor (min_id or max_id, "" for a first page).
func commentsHarness(t *testing.T, pages map[string][]byte) (*harness, []proto.Post, *[]time.Time) {
	t.Helper()
	h, at := feedHarness(t, map[string][]byte{"": feedPageJSON(false, "",
		photoPost(mediaPKOne, "DAbcPhoto1", mayaFeed, `null`, ""),
		reelPost("3400000000000000002", "DAbcReel02"))})
	posts, _ := load(t, h, false)
	h.api.comments = func(path string, q url.Values) ([]byte, error) {
		*at = append(*at, time.Now())
		key := path + "?" + q.Get("min_id") + q.Get("max_id")
		if page, ok := pages[key]; ok {
			return page, nil
		}
		t.Errorf("asked for %s", key)
		return nil, errors.New("no such page")
	}
	return h, posts, at
}

// commentCalls are the Comments requests made, in order.
func commentCalls(h *harness) []commentCall {
	h.api.mu.Lock()
	defer h.api.mu.Unlock()
	var out []commentCall
	for _, c := range h.api.calls {
		if c.name == "Comments" {
			out = append(out, c.req.(commentCall))
		}
	}
	return out
}

func comments(t *testing.T, h *harness, post, of int64, more bool) ([]proto.Comment, bool) {
	t.Helper()
	cs, m, err := h.b.LoadComments(context.Background(), post, of, more)
	if err != nil {
		t.Fatalf("LoadComments: %v", err)
	}
	return cs, m
}

const topPath = "/api/v1/media/" + mediaPKOne + "/comments/"

func TestAPostsCommentsComeAPageAtATime(t *testing.T) {
	minCursor := `{"cached_comments_cursor": "17900000000000002", "bifilter_token": "KDsBB"}`
	h, posts, _ := commentsHarness(t, map[string][]byte{
		topPath + "?": topPage(true, "next_min_id", minCursor,
			commentJSON("17900000000000001", samFeed, "so good 😍", 1759900100, 4, 2,
				`"preview_child_comments":[`+commentJSON("17900000000000099", mayaFeed, "preview", 1759900200, 0, 0, "")+`]`),
			commentJSON("17900000000000000", mayaFeed, "Golden hour", 1759900000, 0, 0, `"type":1`),
			commentJSON("not-a-number", mayaFeed, "odd", 1759900000, 0, 0, ""),
			commentJSON(17900000000000002, mayaFeed, "Where is this?", 1759900300, 0, 0, "")),
		topPath + "?" + minCursor: topPage(true, "next_max_id", "QVFDmax",
			commentJSON("17900000000000003", samFeed, "third", 1759900400, 1, 0, "")),
		topPath + "?QVFDmax": topPage(false, "", "",
			commentJSON("17900000000000004", samFeed, "last", 1759900500, 1, 0, "")),
	})
	post := posts[0].ID
	first, more := comments(t, h, post, 0, false)
	if !more || len(first) != 2 {
		t.Fatalf("first page: %+v, more %v", first, more)
	}
	c := first[0]
	if c.ID <= 0 || c.Author.Name != "Sam Rivera" || c.Author.Username != "sam.climbs" || !c.Author.Verified || c.Author.Photo != nil ||
		c.Date != 1759900100 || c.Text != "so good 😍" || *c.Likes != 4 || c.Replies != 2 || c.ReplyTo != 0 {
		t.Errorf("comment %+v", c)
	}
	// A pk written as a number is the same as one written as a string.
	if first[1].Text != "Where is this?" || first[1].ID == c.ID {
		t.Errorf("second %+v", first[1])
	}
	calls := commentCalls(h)
	if len(calls) != 1 || calls[0].path != topPath || calls[0].referer != "https://www.instagram.com/p/DAbcPhoto1/" {
		t.Fatalf("asked %+v", calls)
	}
	if q := calls[0].query; len(q) != 2 || q.Get("can_support_threading") != "true" || q.Get("permalink_enabled") != "false" {
		t.Errorf("query %v", q)
	}

	// Then the next pages, by the cursor each gave: min_id, then max_id.
	second, more := comments(t, h, post, 0, true)
	if len(second) != 1 || !more || second[0].Text != "third" {
		t.Fatalf("second page %+v %v", second, more)
	}
	if q := commentCalls(h)[1].query; q.Get("min_id") != minCursor || q.Has("max_id") || len(q) != 3 {
		t.Errorf("second query %v", q)
	}
	last, more := comments(t, h, post, 0, true)
	if len(last) != 1 || more {
		t.Fatalf("last page %+v %v", last, more)
	}
	if q := commentCalls(h)[2].query; q.Get("max_id") != "QVFDmax" || q.Has("min_id") || len(q) != 3 {
		t.Errorf("third query %v", q)
	}
	// Past the end, nothing is asked for.
	if none, more := comments(t, h, post, 0, true); len(none) != 0 || more || none == nil || len(commentCalls(h)) != 3 {
		t.Errorf("past the end: %v %v", none, more)
	}
	// The first page again, from the top, with the same ids.
	again, _ := comments(t, h, post, 0, false)
	if len(commentCalls(h)) != 4 || commentCalls(h)[3].query.Has("min_id") || again[0].ID != c.ID {
		t.Errorf("again: %+v", commentCalls(h)[3])
	}
	noCalls(t, h.api, "MarkRead", "SendReaction", "SendMessage", "SetTyping")
}

func TestRepliesComeOldestFirstForTheCommentAsked(t *testing.T) {
	childPath := topPath + "17900000000000001/child_comments/"
	reply := func(pk string, created int64, parent string) string {
		return commentJSON(pk, mayaFeed, "reply "+pk, created, 1, 0, `"parent_comment_id":"`+parent+`"`)
	}
	h, posts, _ := commentsHarness(t, map[string][]byte{
		topPath + "?": topPage(false, "", "",
			commentJSON("17900000000000001", samFeed, "has replies", 1759900100, 4, 3, ""),
			commentJSON("17900000000000005", samFeed, "none", 1759900150, 0, 0, "")),
		childPath + "?": childPage(true, "QVFDchild",
			reply("17900000000000012", 1759900300, "17900000000000001"),
			reply("17900000000000011", 1759900200, "17900000000000001"),
			reply("17900000000000099", 1759900250, "17900000000000005")),
		childPath + "?QVFDchild": childPage(false, "", reply("17900000000000013", 1759900400, "17900000000000001")),
	})
	post := posts[0].ID
	top, _ := comments(t, h, post, 0, false)
	parent := top[0].ID
	replies, more := comments(t, h, post, parent, false)
	if !more || len(replies) != 2 {
		t.Fatalf("replies %+v %v", replies, more)
	}
	if replies[0].Text != "reply 17900000000000011" || replies[1].Text != "reply 17900000000000012" {
		t.Errorf("not oldest first: %q %q", replies[0].Text, replies[1].Text)
	}
	for _, r := range replies {
		if r.ReplyTo != parent || r.Replies != 0 || r.ID == parent {
			t.Errorf("reply %+v", r)
		}
	}
	calls := commentCalls(h)
	if c := calls[len(calls)-1]; c.path != childPath || len(c.query) != 0 || c.referer != "https://www.instagram.com/p/DAbcPhoto1/" {
		t.Errorf("asked %+v", c)
	}
	rest, more := comments(t, h, post, parent, true)
	if len(rest) != 1 || more {
		t.Errorf("rest %+v %v", rest, more)
	}
	calls = commentCalls(h)
	if q := calls[len(calls)-1].query; q.Get("max_id") != "QVFDchild" || len(q) != 1 {
		t.Errorf("query %v", q)
	}
	// A comment's replies are asked for only through its own post.
	if _, _, err := h.b.LoadComments(context.Background(), posts[1].ID, parent, false); err != errNoComment {
		t.Errorf("through another post: %v", err)
	}
}

func TestCommentsOfWhatThisSessionDidntGiveAreNotFound(t *testing.T) {
	h, posts, _ := commentsHarness(t, map[string][]byte{
		topPath + "?": topPage(false, "", "", commentJSON("17900000000000001", samFeed, "x", 1, 0, 0, "")),
	})
	ctx := context.Background()
	if _, _, err := h.b.LoadComments(ctx, 999, 0, false); err != errNoPost {
		t.Errorf("unknown post: %v", err)
	}
	if _, _, err := h.b.LoadComments(ctx, posts[0].ID, 999, false); err != errNoComment {
		t.Errorf("unknown comment: %v", err)
	}
	top, _ := comments(t, h, posts[0].ID, 0, false)
	// After another login, the old session's posts and comments are gone.
	h.login(mailbox(t, "", false))
	if _, _, err := h.b.LoadComments(ctx, posts[0].ID, 0, false); err != errNoPost {
		t.Errorf("old post: %v", err)
	}
	if _, _, err := h.b.LoadComments(ctx, posts[0].ID, top[0].ID, false); err != errNoPost {
		t.Errorf("old comment: %v", err)
	}
	if n := len(commentCalls(h)); n != 1 {
		t.Errorf("%d requests", n)
	}
}

func TestOddCommentFieldsReadAsNone(t *testing.T) {
	long := strings.Repeat("é", maxCaption+10)
	h, posts, _ := commentsHarness(t, map[string][]byte{
		topPath + "?": topPage(false, "", "",
			commentJSON("17900000000000006", `{"username":null,"full_name":null,"is_verified":null}`, long, 1759900100, 0, 0, `"comment_like_count":"7","child_comment_count":"x"`),
			commentJSON("17900000000000007", `null`, "nobody", 0, 0, 0, `"created_at":null,"created_at_utc":"1759900200","comment_like_count":null`),
			`{"pk":"17900000000000008","text":{"no":1}}`,
			`"not an object"`),
	})
	cs, _ := comments(t, h, posts[0].ID, 0, false)
	if len(cs) != 3 {
		t.Fatalf("got %+v", cs)
	}
	if utf8.RuneCountInString(cs[0].Text) != maxCaption || cs[0].Author != (proto.FeedAuthor{}) || *cs[0].Likes != 7 || cs[0].Replies != 0 {
		t.Errorf("first %+v", cs[0])
	}
	if cs[1].Date != 1759900200 || cs[1].Likes != nil || cs[1].Author != (proto.FeedAuthor{}) {
		t.Errorf("second %+v", cs[1])
	}
	if cs[2].Text != "" {
		t.Errorf("a text that isn't one: %q", cs[2].Text)
	}
}

func TestAPostWithoutANumericIDHasNoCommentsToAsk(t *testing.T) {
	h, _ := feedHarness(t, map[string][]byte{"": feedPageJSON(false, "",
		`{"pk":"abc","id":"xyz_1","code":"DAbcNoID01","media_type":1,"user":`+mayaFeed+`,"image_versions2":`+images(cand(1080, 1080, "n1"))+`}`,
		`{"pk":"abc","id":"3400000000000000009_34000000002","code":"DAbcIdOK02","media_type":1,"user":`+mayaFeed+`,"image_versions2":`+images(cand(1080, 1080, "n2"))+`}`)})
	posts, _ := load(t, h, false)
	if _, _, err := h.b.LoadComments(context.Background(), posts[0].ID, 0, false); err != errNoMediaID {
		t.Errorf("got %v", err)
	}
	comments(t, h, posts[1].ID, 0, false)
	if calls := commentCalls(h); len(calls) != 1 || calls[0].path != "/api/v1/media/3400000000000000009/comments/" {
		t.Errorf("asked %+v", calls)
	}
}

func TestCommentsAndTheFeedShareOneGap(t *testing.T) {
	h, posts, at := commentsHarness(t, map[string][]byte{
		topPath + "?": topPage(false, "", "", commentJSON("17900000000000001", samFeed, "x", 1, 0, 0, "")),
	})
	h.b.feed.gap = 120 * time.Millisecond
	comments(t, h, posts[0].ID, 0, false)
	load(t, h, true)
	comments(t, h, posts[0].ID, 0, false)
	if len(*at) != 4 {
		t.Fatalf("%d requests", len(*at))
	}
	for i := 1; i < len(*at); i++ {
		if d := (*at)[i].Sub((*at)[i-1]); d < 110*time.Millisecond {
			t.Errorf("request %d came %v after the one before", i, d)
		}
	}
}

func TestACommentsRefusalSaysWhatToDo(t *testing.T) {
	for _, c := range []struct {
		body []byte
		err  error
		want *proto.Error
	}{
		{[]byte(`{"message":"checkpoint_required","status":"fail"}`), fmt.Errorf("%w 400", httpclient.ErrUnexpectedError), errCheckpoint},
		{[]byte(`{"message":"login_required","status":"fail"}`), nil, errSessionEnded},
		{[]byte(`{"message":"feedback_required","status":"fail"}`), nil, errRateLimited},
		{nil, httpclient.ErrRateLimited, errRateLimited},
		{[]byte(`{"message":"Media not found or unavailable","status":"fail"}`), fmt.Errorf("%w 400", httpclient.ErrUnexpectedError), proto.Err(proto.NetworkError, errComments)},
		{[]byte(`<html>`), nil, errCommentsAnswer},
	} {
		h, posts, _ := commentsHarness(t, nil)
		h.api.comments = func(string, url.Values) ([]byte, error) { return c.body, c.err }
		_, _, err := h.b.LoadComments(context.Background(), posts[0].ID, 0, false)
		var pe *proto.Error
		if !errors.As(err, &pe) || pe.Code != c.want.Code || pe.Message != c.want.Message {
			t.Errorf("%s / %v: got %v, want %v", c.body, c.err, err, c.want)
		}
	}
}

func TestTheCommentsLogSaysHowManyAndNothingOfThem(t *testing.T) {
	log := &logBuffer{}
	hlog.SetOutput(log)
	t.Cleanup(func() { hlog.SetOutput(io.Discard) })
	childPath := topPath + "17900000000000001/child_comments/"
	h, posts, _ := commentsHarness(t, map[string][]byte{
		topPath + "?": topPage(false, "", "",
			commentJSON("17900000000000001", samFeed, "secret words", 1759900100, 4, 1, ""),
			commentJSON("x", samFeed, "odd", 1, 0, 0, "")),
		childPath + "?": childPage(false, "", commentJSON("17900000000000011", mayaFeed, "more secrets", 1759900200, 0, 0, "")),
	})
	top, _ := comments(t, h, posts[0].ID, 0, false)
	comments(t, h, posts[0].ID, top[0].ID, false)
	got := log.String()
	for _, line := range []string{"INFO instagram: comments loaded count=1 left_out=1\n", "INFO instagram: replies loaded count=1 left_out=0\n"} {
		if !strings.Contains(got, line) {
			t.Errorf("no line %q in:\n%s", line, got)
		}
	}
	for _, secret := range []string{"secret", "sam", "Sam", "maya", "1790000", "3400000", "DAbc"} {
		if strings.Contains(got, secret) {
			t.Errorf("the log says %q:\n%s", secret, got)
		}
	}
}

func TestCommentRequestsAreGETsFromThePostsPage(t *testing.T) {
	h := http.Header{}
	commentHeaders(h, "https://www.instagram.com", "https://www.instagram.com/reel/DAbc/")
	if h.Get("Origin") != "" || h.Get("Referer") != "https://www.instagram.com/reel/DAbc/" || h.Get("X-Requested-With") != "XMLHttpRequest" || h.Get("Sec-Fetch-Mode") != "cors" {
		t.Errorf("headers %v", h)
	}
	h = http.Header{}
	commentHeaders(h, "https://www.instagram.com", "")
	if h.Get("Referer") != "https://www.instagram.com/" {
		t.Errorf("no page: %v", h)
	}
	q := url.Values{"can_support_threading": {"true"}, "permalink_enabled": {"false"}}
	if got := commentsURL("https://www.instagram.com", topPath, q); got != "https://www.instagram.com"+topPath+"?can_support_threading=true&permalink_enabled=false" {
		t.Errorf("url %s", got)
	}
	if got := commentsURL("https://www.instagram.com", "/x/", nil); got != "https://www.instagram.com/x/" {
		t.Errorf("url %s", got)
	}
	for s, want := range map[string]bool{"17900000000000001": true, "": false, "12a": false, "../1": false, strings.Repeat("1", 31): false} {
		if digits(s) != want {
			t.Errorf("digits(%q)", s)
		}
	}
}
