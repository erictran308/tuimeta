// SPDX-License-Identifier: AGPL-3.0-or-later

package instagram

// The home feed (load_feed): photos and videos from the people and pages
// the account follows, read with the request instagram.com's home page
// makes for it, through the logged-in web session. Only pages are asked
// for: nothing says which posts were seen, watched or for how long (the
// web client's seen_posts and feed_view_info are never sent), and nothing
// is liked or followed. Files download only on download, like any other.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	mcookies "go.mau.fi/mautrix-meta/pkg/messagix/cookies"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

const (
	// feedPath is the web client's feed request, on www.instagram.com.
	feedPath = "/api/v1/feed/timeline/"
	// feedGap is the least time between two feed requests. A key held down
	// in tuimeta asks for page after page, and a client paging faster than
	// anyone scrolls is what Instagram's checks look for.
	feedGap = 2 * time.Second
	// feedTries is how many pages one load asks for while every post of
	// the ones before was left out (only ads, say), so a load doesn't come
	// back empty while there's more.
	feedTries = 3
	// maxCarousel is the most media of one post kept: Instagram allows 20.
	maxCarousel = 20
	// maxCaption is Instagram's own limit on a caption, in characters.
	maxCaption = 2200
	// maxAuthor caps a name or username, far past what Instagram allows
	// (30 for a username), so a page can't send a long one.
	maxAuthor = 100
	// The sizes PROTOCOL.md picks: pictures about as wide as a window shows
	// them, videos smaller than their largest, to keep downloads small.
	photoWidth = 1080
	videoWidth = 720
)

var (
	errFeed       = "Couldn't load the feed from Instagram; try again."
	errFeedAnswer = proto.Err(proto.NetworkError,
		"Instagram's answer about the feed couldn't be read; try again later.")
)

// feedState is where the feed got to. It has its own lock, held for a
// whole load (so loads are one at a time), apart from b.mu: a load waits
// on Instagram, and b.mu is held while events are written.
type feedState struct {
	mu sync.Mutex
	// epoch is the login the cursor belongs to: another login starts over.
	epoch   int
	cursor  string // next_max_id of the last page; "" before the first
	started bool   // a page has come since the login or the last fresh load
	more    bool
	last    time.Time // when the last request went out
	gap     time.Duration
	// posts are the ids given, by Instagram's media id, so a post seen
	// again keeps its id; next is the last id given, never reused in the
	// run, even after another login.
	posts map[string]int64
	next  int64

	// What loading a post's comments needs (comments.go), by post id, and
	// the comments given: their ids by Instagram's comment pk, and back,
	// assigned like posts' ids.
	media       map[int64]postRef
	comments    map[string]int64
	commentOf   map[int64]commentRef
	nextComment int64
	// Where each list of comments got to: a post's, by post id, and a
	// comment's replies, by comment id.
	threads map[int64]*listState
	replies map[int64]*listState
}

// sync starts over when the login changed (epoch): the cursors, and the
// posts and comments ids stand for, were another session's, perhaps
// another account's. The last ids given stay, so none is given twice.
// f.mu is held.
func (f *feedState) sync(epoch int) {
	if f.posts != nil && f.epoch == epoch {
		return
	}
	f.epoch, f.cursor, f.started, f.more = epoch, "", false, false
	f.posts = map[string]int64{}
	f.media = map[int64]postRef{}
	f.comments = map[string]int64{}
	f.commentOf = map[int64]commentRef{}
	f.threads = map[int64]*listState{}
	f.replies = map[int64]*listState{}
}

var _ backend.Feeder = (*Instagram)(nil)

// LoadFeed is the next page of the home feed. limit is only a hint:
// Instagram's pages are their own size, and a page isn't cut, since the
// cursor has moved past it.
func (b *Instagram) LoadFeed(ctx context.Context, _ int, fresh bool, box backend.Box, _ backend.FeedKind) ([]proto.Post, bool, error) {
	conn, err := b.waitReady(ctx)
	if err != nil {
		return nil, false, err
	}
	b.mu.Lock()
	epoch := b.epoch
	b.mu.Unlock()
	f := &b.feed
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sync(epoch)
	if fresh {
		f.cursor, f.started = "", false
	}
	if f.started && !f.more {
		return []proto.Post{}, false, nil
	}
	posts := []proto.Post{}
	given := map[int64]bool{}
	for range feedTries {
		if err := f.wait(ctx); err != nil {
			return nil, false, err
		}
		body, err := conn.cli.FeedTimeline(withQuietLog(ctx), feedForm(conn.cookies.Get(mcookies.IGCookieDeviceID), f.cursor))
		f.last = time.Now()
		if err == nil {
			err = refused(body, errFeed)
		}
		if err != nil {
			hlog.Warn("instagram: feed failed", hlog.Kind(err))
			return nil, false, feedError(err, body, errFeed)
		}
		page, err := parseFeed(body)
		if err != nil {
			hlog.Warn("instagram: feed unreadable", hlog.Kind(err))
			return nil, false, errFeedAnswer
		}
		f.started = true
		f.cursor = page.next
		f.more = page.more && page.next != ""
		st := feedStats{rows: page.rows, ads: page.ads, unreadable: page.unreadable}
		for i := range page.media {
			p, ok := b.feedPost(f, &page.media[i], box, &st)
			if ok && !given[p.ID] {
				given[p.ID] = true
				posts = append(posts, p)
			}
		}
		st.log()
		if len(posts) > 0 || !f.more {
			break
		}
	}
	return posts, f.more, nil
}

// wait holds a request back until feedGap has passed since the last one;
// f.mu is held.
func (f *feedState) wait(ctx context.Context) error {
	if f.last.IsZero() {
		return nil
	}
	gap := f.gap
	if gap == 0 {
		gap = feedGap
	}
	left := time.Until(f.last.Add(gap))
	if left <= 0 {
		return nil
	}
	t := time.NewTimer(left)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// feedForm is the feed request's body, as the web client sends it, less
// what reports views: deviceID is the session's ig_did cookie (left out
// when it has none), cursor the last page's next_max_id (none for the
// first page).
func feedForm(deviceID, cursor string) url.Values {
	form := url.Values{
		"is_async_ads_double_request":      {"0"},
		"is_async_ads_in_headload_enabled": {"0"},
		"is_async_ads_rti":                 {"0"},
		"rti_delivery_backend":             {"0"},
	}
	if deviceID != "" {
		form.Set("device_id", deviceID)
	}
	if cursor != "" {
		form.Set("max_id", cursor)
	}
	return form
}

// feedAnswer is how Instagram says no: {"status":"fail","message":…}.
type feedAnswer struct {
	Status    string `json:"status"`
	Message   string `json:"message"`
	ErrorType string `json:"error_type"`
}

// refused is the error for an answer that came back but says no, or nil:
// the session's trouble, else failed (what wasn't done).
func refused(body []byte, failed string) error {
	var a feedAnswer
	if json.Unmarshal(body, &a) != nil || a.Status != "fail" {
		return nil
	}
	if e := refusal(a); e != nil {
		return e
	}
	return proto.Err(proto.NetworkError, failed)
}

// refusal is what to tell the user about why Instagram said no, if it's
// about the session rather than this request.
func refusal(a feedAnswer) *proto.Error {
	for _, why := range []string{a.Message, a.ErrorType} {
		switch why {
		case "checkpoint_required", "challenge_required":
			return errCheckpoint
		case "login_required":
			return errSessionEnded
		case "consent_required":
			return errConsent
		case "feedback_required", "rate_limit_error":
			return errRateLimited
		}
	}
	if strings.Contains(strings.ToLower(a.Message), "wait a few minutes") {
		return errRateLimited
	}
	return nil
}

// feedError is what a failed feed or comments request tells the user: the
// session's trouble if the answer (body, which instameow returns with an
// error status) or the error says so, else failed.
func feedError(err error, body []byte, failed string) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	var pe *proto.Error
	if errors.As(err, &pe) {
		return pe
	}
	var a feedAnswer
	if json.Unmarshal(body, &a) == nil {
		if e := refusal(a); e != nil {
			return e
		}
	}
	return requestError(err, failed)
}

// --- reading a page ---------------------------------------------------------

// Instagram writes some numbers as strings and some ids as numbers, and
// what it writes changes: these read either, and anything odd as zero, so
// one strange field never costs a page.

type flexInt int64

func (n *flexInt) UnmarshalJSON(b []byte) error {
	*n = 0
	s := strings.Trim(string(bytes.TrimSpace(b)), `"`)
	if v, err := strconv.ParseInt(s, 10, 64); err == nil {
		*n = flexInt(v)
		return nil
	}
	if v, err := strconv.ParseFloat(s, 64); err == nil && !math.IsNaN(v) && !math.IsInf(v, 0) {
		*n = flexInt(min(max(v, math.MinInt64/2), math.MaxInt64/2))
	}
	return nil
}

type flexFloat float64

func (n *flexFloat) UnmarshalJSON(b []byte) error {
	*n = 0
	s := strings.Trim(string(bytes.TrimSpace(b)), `"`)
	if v, err := strconv.ParseFloat(s, 64); err == nil && !math.IsNaN(v) && !math.IsInf(v, 0) {
		*n = flexFloat(v)
	}
	return nil
}

// flexString is a string, or a number written as one.
type flexString string

func (s *flexString) UnmarshalJSON(b []byte) error {
	*s = ""
	b = bytes.TrimSpace(b)
	var str string
	if json.Unmarshal(b, &str) == nil {
		*s = flexString(str)
		return nil
	}
	var num json.Number
	if json.Unmarshal(b, &num) == nil {
		*s = flexString(num.String())
	}
	return nil
}

// flexBool is true, 1 or "true".
type flexBool bool

func (v *flexBool) UnmarshalJSON(b []byte) error {
	switch strings.Trim(string(bytes.TrimSpace(b)), `"`) {
	case "true", "1", "True":
		*v = true
	default:
		*v = false
	}
	return nil
}

// present reports whether a field holds anything: not missing, null, false
// or empty.
func present(raw json.RawMessage) bool {
	switch string(bytes.TrimSpace(raw)) {
	case "", "null", "false", "0", `""`, "[]", "{}":
		return false
	}
	return true
}

type feedCandidate struct {
	Width  flexInt `json:"width"`
	Height flexInt `json:"height"`
	URL    string  `json:"url"`
}

type feedUser struct {
	Username      string   `json:"username"`
	FullName      string   `json:"full_name"`
	Verified      flexBool `json:"is_verified"`
	ProfilePicURL string   `json:"profile_pic_url"`
}

// feedMedia is the part of Instagram's media dict a post needs. A carousel
// holds more of them; their own carousels, if any, are never read.
type feedMedia struct {
	ID          flexString `json:"id"`
	Pk          flexString `json:"pk"`
	Code        string     `json:"code"`
	MediaType   flexInt    `json:"media_type"`
	ProductType string     `json:"product_type"`
	TakenAt     flexInt    `json:"taken_at"`
	Caption     *struct {
		Text string `json:"text"`
	} `json:"caption"`
	User         feedUser        `json:"user"`
	LikeCount    *flexInt        `json:"like_count"`
	CommentCount *flexInt        `json:"comment_count"`
	CountsHidden flexBool        `json:"like_and_view_counts_disabled"`
	Images       feedImages      `json:"image_versions2"`
	Videos       []feedCandidate `json:"video_versions"`
	Duration     flexFloat       `json:"video_duration"`
	Width        flexInt         `json:"original_width"`
	Height       flexInt         `json:"original_height"`
	Carousel     []feedMedia     `json:"carousel_media"`
	// DashManifest is the video as DASH (dash.go), when it has no
	// video_versions; anything but a string reads as none.
	DashManifest flexString `json:"video_dash_manifest"`

	// What marks an ad, any of which leaves the item out. Not sponsor_tags,
	// which marks a paid partnership: a post of someone followed.
	AdID       flexString      `json:"ad_id"`
	AdAction   json.RawMessage `json:"ad_action"`
	AdMetadata json.RawMessage `json:"ad_metadata"`
	AdHeader   json.RawMessage `json:"ad_header_style"`
	Injected   json.RawMessage `json:"injected"`
	IsAd       flexBool        `json:"is_ad"`

	// What marks a post Instagram picked from an account not followed, in
	// a media_or_ad item; a post found in another row is one anyway.
	IsSuggested flexBool        `json:"is_suggested"`
	ExploreInfo json.RawMessage `json:"explore_item_info"`
	suggested   bool
}

type feedImages struct {
	Candidates []feedCandidate `json:"candidates"`
}

// ad reports whether the item is an ad: a field only ads have, or one
// that names it one.
func (m *feedMedia) ad() bool {
	pt := strings.ToLower(m.ProductType)
	return m.AdID != "" || bool(m.IsAd) || present(m.AdAction) || present(m.AdMetadata) ||
		present(m.AdHeader) || present(m.Injected) ||
		pt == "ad" || strings.HasPrefix(pt, "ad_") || strings.HasSuffix(pt, "_ad")
}

// feedPage is one answer, read.
type feedPage struct {
	media           []feedMedia
	more            bool
	next            string
	ads, unreadable int
	rows            map[string]*rowCount // items that aren't media_or_ad, by rowKey
}

// parseFeed reads an answer's media, in order: each media_or_ad item, and
// the posts other rows hold (rows.go), which are suggested ones. Rows with
// none (people to follow, stories, "you're all caught up") are passed
// over, and so are ads and items that don't read.
func parseFeed(body []byte) (feedPage, error) {
	var r struct {
		Items []json.RawMessage `json:"feed_items"`
		More  flexBool          `json:"more_available"`
		Next  flexString        `json:"next_max_id"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return feedPage{}, err
	}
	page := feedPage{more: bool(r.More), next: string(r.Next), rows: map[string]*rowCount{}}
	take := func(raw json.RawMessage, suggested bool) {
		var m feedMedia
		switch {
		case json.Unmarshal(raw, &m) != nil:
			page.unreadable++
		case m.ad():
			page.ads++
		default:
			m.suggested = suggested || bool(m.IsSuggested) || present(m.ExploreInfo)
			page.media = append(page.media, m)
		}
	}
	for _, raw := range r.Items {
		var item map[string]json.RawMessage
		if json.Unmarshal(raw, &item) != nil {
			page.unreadable++
			continue
		}
		if media, ok := item["media_or_ad"]; ok && present(media) {
			take(media, false)
			continue
		}
		key := rowKey(item)
		count := page.rows[key]
		if count == nil {
			count = &rowCount{}
			page.rows[key] = count
		}
		count.rows++
		if slices.Contains(postless, key) {
			continue
		}
		var found []json.RawMessage
		for _, k := range sortedKeys(item) {
			if len(found) < rowPosts {
				found = append(found, rowMedia(item[k])...)
			}
		}
		if len(found) > 0 {
			count.withPosts++
		}
		for _, m := range found[:min(len(found), rowPosts)] {
			take(m, true)
		}
	}
	return page, nil
}

// feedStats is what one page held, for helper.log: counts only, under
// fixed names (hlog's rules), so what was wrong with a feed can be told
// without anything of it in the log.
type feedStats struct {
	// threads is Threads' feed, whose lines say so.
	threads         bool
	rows            map[string]*rowCount
	ads, unreadable int
	// Videos by where their file came from: video_versions, DASH with
	// sound, DASH without, or nowhere (shown with its cover only).
	progressive, dash, dashSilent, unplayable int
	// Posts left out: no media to show, no shortcode to open them at.
	noMedia, noCode int
}

func (st *feedStats) log() {
	net := "instagram: "
	if st.threads {
		net = "threads: "
	}
	if st.ads+st.unreadable > 0 {
		hlog.Info(net+"feed items left out", hlog.Int("ads", int64(st.ads)), hlog.Int("unreadable", int64(st.unreadable)))
	}
	if n := st.progressive + st.dash + st.dashSilent + st.unplayable; n > 0 {
		hlog.Info(net+"feed videos", hlog.Int("progressive", int64(st.progressive)), hlog.Int("dash", int64(st.dash)),
			hlog.Int("dash_silent", int64(st.dashSilent)), hlog.Int("unplayable", int64(st.unplayable)))
	}
	if st.noMedia+st.noCode > 0 {
		hlog.Info(net+"feed posts dropped", hlog.Int("no_media", int64(st.noMedia)), hlog.Int("no_code", int64(st.noCode)))
	}
	if rows := rowAttrs(st.rows); len(rows) > 0 {
		hlog.Info(net+"feed rows", rows...)
	}
}

// maxRowNames is how many kinds of row one log line names; the rest are
// counted as "other".
const maxRowNames = 8

// rowAttrs are the rows of a page for the log, as name=rows/with posts:
// by Instagram's field name when the key reads as one (schemaName), else,
// and past maxRowNames, as "other", which comes last.
func rowAttrs(rows map[string]*rowCount) []hlog.Attr {
	named := map[string]rowCount{}
	var other rowCount
	var names []string
	for k := range rows {
		if k != "other" && schemaName(k) {
			names = append(names, k)
		}
	}
	slices.Sort(names)
	if len(names) > maxRowNames {
		names = names[:maxRowNames]
	}
	for k, c := range rows {
		if slices.Contains(names, k) {
			named[k] = *c
		} else {
			other.rows += c.rows
			other.withPosts += c.withPosts
		}
	}
	var out []hlog.Attr
	say := func(k string, c rowCount) {
		out = append(out, hlog.Str(k, strconv.Itoa(c.rows)+"/"+strconv.Itoa(c.withPosts)))
	}
	for _, k := range names {
		say(k, named[k])
	}
	if other.rows > 0 {
		say("other", other)
	}
	return out
}

// --- making posts -----------------------------------------------------------

// feedPost is the protocol's post of a media dict, registering its files;
// false when there's nothing to show (no media on Instagram's CDN, no
// address to open it at). f.mu is held.
func (b *Instagram) feedPost(f *feedState, m *feedMedia, box backend.Box, st *feedStats) (proto.Post, bool) {
	key := string(m.ID)
	if key == "" {
		key = string(m.Pk)
	}
	link := postURL(m.Code, m.ProductType)
	if key == "" || link == "" {
		st.noCode++
		return proto.Post{}, false
	}
	items := []*feedMedia{m}
	if m.MediaType == 8 {
		items = items[:0]
		for i := range m.Carousel {
			if len(items) == maxCarousel {
				break
			}
			items = append(items, &m.Carousel[i])
		}
	}
	var media []proto.Media
	for _, it := range items {
		if md, ok := b.feedMediaOf(it, box, st); ok {
			media = append(media, md)
		}
	}
	if len(media) == 0 {
		st.noMedia++
		return proto.Post{}, false
	}
	id, ok := f.posts[key]
	if !ok {
		f.next++
		id = f.next
		f.posts[key] = id
	}
	f.media[id] = postRef{pk: mediaPK(m), url: link}
	p := proto.Post{
		ID: id,
		Author: proto.FeedAuthor{
			Name:     runes(strings.TrimSpace(m.User.FullName), maxAuthor),
			Username: runes(strings.TrimSpace(m.User.Username), maxAuthor),
			Verified: bool(m.User.Verified),
			// Under the key chats' avatars have, so a person in both is
			// one file.
			Photo: b.feedFile(m.User.ProfilePicURL, "avatar", proto.Photo, avatarSize, avatarSize),
		},
		Date:      max(int64(m.TakenAt), 0),
		Media:     media,
		URL:       link,
		Suggested: m.suggested,
	}
	if m.Caption != nil {
		p.Caption = runes(m.Caption.Text, maxCaption)
	}
	if m.LikeCount != nil && !m.CountsHidden {
		n := max(int64(*m.LikeCount), 0)
		p.Likes = &n
	}
	if m.CommentCount != nil {
		n := max(int64(*m.CommentCount), 0)
		p.Comments = &n
	}
	return p, true
}

// feedMediaOf is one photo or video of a post. A photo needs its picture
// on Instagram's CDN: the one about photoWidth wide is its file, for the
// viewer, and its thumbnail is what the feed draws, no bigger than box
// needs (drawnIn). A video's file is its progressive version (picture and
// sound together) about videoWidth wide, else its DASH picture (with its
// sound apart, as audio, when there is one); with neither it still comes,
// file 0, so its cover, picked as a photo's thumbnail is, shows, unless it
// hasn't one either.
func (b *Instagram) feedMediaOf(m *feedMedia, box backend.Box, st *feedStats) (proto.Media, bool) {
	return b.feedFiles().mediaOf(m, box, st)
}

// feedFiles registers the feed's files under a network: Threads' posts are
// Instagram's media dicts too, and their files are Threads'.
type feedFiles struct {
	files   *ids.Files
	network proto.Network
}

func (b *Instagram) feedFiles() feedFiles { return feedFiles{b.d.Files, proto.Instagram} }

// mediaOf is feedMediaOf's, for either network.
func (fs feedFiles) mediaOf(m *feedMedia, box backend.Box, st *feedStats) (proto.Media, bool) {
	video := m.MediaType == 2 || (m.MediaType != 1 && (len(m.Videos) > 0 || m.DashManifest != ""))
	var full, cover *proto.Image
	stillURL := ""
	w, h := int(m.Width), int(m.Height)
	if still, ok := atLeast(m.Images.Candidates, photoWidth); ok {
		drawn := drawnIn(m.Images.Candidates, box, still)
		cover = fs.file(drawn.URL, stillKey(drawn), proto.Photo, int(drawn.Width), int(drawn.Height))
		full = cover
		// A video's cover is only drawn: no larger one for the viewer.
		if !video && drawn.URL != still.URL {
			full = fs.file(still.URL, stillKey(still), proto.Photo, int(still.Width), int(still.Height))
		}
		stillURL = still.URL
		if w <= 0 || h <= 0 {
			w, h = int(still.Width), int(still.Height)
		}
	}
	if !video {
		if full == nil || cover == nil {
			return proto.Media{}, false
		}
		return proto.Media{
			Kind: proto.Photo, FileID: full.FileID, Thumbnail: cover,
			Width: w, Height: h, Mime: mimeOf(stillURL, proto.Photo),
		}, true
	}
	md := proto.Media{Kind: proto.Video, Thumbnail: cover, Width: w, Height: h, Mime: "video/mp4"}
	seconds := float64(m.Duration)
	if v, ok := nearest(m.Videos, videoWidth); ok {
		if f := fs.file(v.URL, "feed-video", proto.Video, 0, 0); f != nil {
			md.FileID = f.FileID
			st.progressive++
		}
	}
	if md.FileID == 0 {
		if pic, sound, length, ok := dashPick(string(m.DashManifest)); ok {
			if f := fs.file(pic.url, "feed-video", proto.Video, 0, 0); f != nil {
				md.FileID = f.FileID
				if a := fs.file(sound.url, "feed-audio", proto.Audio, 0, 0); a != nil {
					md.Audio = a.FileID
				}
				if md.Width <= 0 || md.Height <= 0 {
					md.Width, md.Height = pic.width, pic.height
				}
				if seconds <= 0 {
					seconds = length
				}
				if md.Audio != 0 {
					st.dash++
				} else {
					st.dashSilent++
				}
			}
		}
	}
	if md.FileID == 0 {
		st.unplayable++
		if cover == nil {
			return proto.Media{}, false
		}
	}
	md.Duration = int(math.Ceil(min(max(seconds, 0), math.MaxInt32)))
	return md, true
}

// stillKey is the which of a picture's file at one size. Instagram serves
// a picture's sizes at one path, with the size in its query (stp=), so
// each size needs a key of its own, or the drawn one and the viewer's
// would be one file.
func stillKey(c feedCandidate) string {
	return "feed-still:" + strconv.FormatInt(int64(c.Width), 10) + "x" + strconv.FormatInt(int64(c.Height), 10)
}

// feedFile registers a file of the feed, keyed by its address's path (not
// its expiring signature) and which, so the same picture keeps its id; nil
// unless it's on Instagram's CDN. There's no message to look a fresh
// address up in once this one expires: a fresh load has new ones.
func (b *Instagram) feedFile(raw, which string, kind proto.MediaKind, w, h int) *proto.Image {
	return b.feedFiles().file(raw, which, kind, w, h)
}

// file is feedFile's, for either network.
func (fs feedFiles) file(raw, which string, kind proto.MediaKind, w, h int) *proto.Image {
	if !onCDNURL(raw) {
		return nil
	}
	mime := mimeOf(raw, kind)
	switch kind {
	case proto.Video:
		mime = "video/mp4"
	case proto.Audio:
		// A DASH stream's sound is an MP4 of its own, saved as .m4a.
		mime = "audio/mp4"
	}
	id := fs.files.Register(ids.FileRef{
		Network: fs.network, Key: fileKey("", raw, which), Mime: mime,
		Source: &mediaSource{url: raw},
	})
	if id == 0 {
		return nil
	}
	return &proto.Image{FileID: id, Width: w, Height: h}
}

// onCDNURL reports whether raw is an address files are downloaded from.
func onCDNURL(raw string) bool {
	u, err := url.Parse(raw)
	return raw != "" && err == nil && onCDN(u)
}

// atLeast is the narrowest candidate on the CDN at least want px wide,
// else the widest there is.
func atLeast(cs []feedCandidate, want int) (feedCandidate, bool) {
	var best feedCandidate
	found := false
	for _, c := range cs {
		if !onCDNURL(c.URL) {
			continue
		}
		w, bw := int(c.Width), int(best.Width)
		switch {
		case !found:
			best, found = c, true
		case bw < want && w > bw:
			best = c // still short: wider is better
		case bw >= want && w >= want && w < bw:
			best = c // wide enough: narrower is smaller
		}
	}
	return best, found
}

// drawnIn is the narrowest candidate on the CDN, of most's shape, that
// fills box without being enlarged (at least as wide or as tall), so the
// feed downloads and decodes no more than it draws; most when there's no
// box, or no narrower one fills it. Another shape would be a crop of the
// picture.
func drawnIn(cs []feedCandidate, box backend.Box, most feedCandidate) feedCandidate {
	best := most
	if box.Width <= 0 || box.Height <= 0 || most.Width <= 0 || most.Height <= 0 {
		return best
	}
	for _, c := range cs {
		if !onCDNURL(c.URL) || c.Width <= 0 || c.Height <= 0 || c.Width >= best.Width || !sameShape(c, most) {
			continue
		}
		if int(c.Width) >= box.Width || int(c.Height) >= box.Height {
			best = c
		}
	}
	return best
}

// sameShape reports whether a's sides are b's in proportion, within the
// rounding of a smaller version's (2%).
func sameShape(a, b feedCandidate) bool {
	across, down := float64(a.Width)*float64(b.Height), float64(a.Height)*float64(b.Width)
	return math.Abs(across-down) <= 0.02*across
}

// nearest is the candidate on the CDN nearest want px wide, the narrower
// of two as near.
func nearest(cs []feedCandidate, want int) (feedCandidate, bool) {
	var best feedCandidate
	found := false
	dist := func(c feedCandidate) int {
		d := int(c.Width) - want
		if d < 0 {
			d = -d
		}
		return d
	}
	for _, c := range cs {
		if !onCDNURL(c.URL) {
			continue
		}
		if !found || dist(c) < dist(best) || (dist(c) == dist(best) && c.Width < best.Width) {
			best, found = c, true
		}
	}
	return best, found
}

// postURL is where the post is on instagram.com: /reel/ for a reel, /p/
// for the rest; "" if code isn't one of Instagram's shortcodes.
func postURL(code, productType string) string {
	if !backend.PostCode(code) {
		return ""
	}
	kind := "p"
	if productType == "clips" {
		kind = "reel"
	}
	return "https://www.instagram.com/" + kind + "/" + code + "/"
}

// runes is s cut to at most n characters, on a character's boundary.
func runes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}
