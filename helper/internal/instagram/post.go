// SPDX-License-Identifier: AGPL-3.0-or-later

package instagram

// One post by its link (load_post), only read: the request the post's page
// on instagram.com makes for it, by the media id its link's code stands
// for. It shares the feed's lock and its gap between requests, and the
// post gets the id the feed gives it, so its comments load as the feed's.

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/proto"
)

const errPost = "Couldn't load this post from Instagram; try again."

var (
	errNotAPostCode = proto.Err(proto.BadRequest, "That isn't an Instagram post's code.")
	errPostGone     = proto.Err(proto.NotFound,
		"Instagram has no post of this link that this account can see; o opens it on instagram.com.")
	errPostEmpty = proto.Err(proto.NotFound,
		"This post has nothing tuimeta can show; o opens it on instagram.com.")
	errPostAnswer = proto.Err(proto.NetworkError,
		"Instagram's answer about this post couldn't be read; try again later.")
)

// shortcodeAlphabet is the digits of a post's code, lowest first: its
// media id written in base 64.
const shortcodeAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

// shortcodeLen is how long a public post's code is. A private account's
// link has more after it; its first shortcodeLen are the post's.
const shortcodeLen = 11

// shortcodePK is the media pk a post's code stands for, in digits; false
// for a code that stands for none (or for one past what a pk can be).
func shortcodePK(code string) (string, bool) {
	if !backend.PostCode(code) {
		return "", false
	}
	var n uint64
	for _, r := range code[:min(len(code), shortcodeLen)] {
		d := uint64(strings.IndexRune(shortcodeAlphabet, r))
		if n > (math.MaxInt64-d)/64 {
			return "", false
		}
		n = n*64 + d
	}
	if n == 0 {
		return "", false
	}
	return strconv.FormatUint(n, 10), true
}

// sameCode reports whether the post Instagram gave (code given) is the one
// a link's code asked for.
func sameCode(given, asked string) bool {
	return given != "" && (given == asked || given == asked[:min(len(asked), shortcodeLen)])
}

// mediaInfoPath is where a post is asked for, by its pk (digits).
func mediaInfoPath(pk string) string { return "/api/v1/media/" + pk + "/info/" }

var _ backend.PostLoader = (*Instagram)(nil)

// LoadPost is the post a link's code names, as its page on instagram.com
// shows it. A link's username isn't needed.
func (b *Instagram) LoadPost(ctx context.Context, code, _ string, box backend.Box) (proto.Post, error) {
	pk, ok := shortcodePK(code)
	if !ok {
		return proto.Post{}, errNotAPostCode
	}
	conn, err := b.waitReady(ctx)
	if err != nil {
		return proto.Post{}, err
	}
	b.mu.Lock()
	epoch := b.epoch
	b.mu.Unlock()
	f := &b.feed
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sync(epoch)
	if err := f.wait(ctx); err != nil {
		return proto.Post{}, err
	}
	body, err := conn.cli.MediaInfo(withQuietLog(ctx), pk, postURL(code, ""))
	f.last = time.Now()
	if err == nil {
		err = refused(body, errPost)
	}
	if err != nil {
		err = postError(err, body)
		if err != errPostGone {
			hlog.Warn("instagram: post failed", hlog.Kind(err))
		}
		return proto.Post{}, err
	}
	m, err := parseMediaInfo(body)
	if err != nil {
		hlog.Warn("instagram: post unreadable", hlog.Kind(err))
		return proto.Post{}, errPostAnswer
	}
	if m == nil || !sameCode(m.Code, code) {
		return proto.Post{}, errPostGone
	}
	st := feedStats{}
	p, ok := b.feedPost(f, m, box, &st)
	st.log()
	if !ok {
		return proto.Post{}, errPostEmpty
	}
	hlog.Info("instagram: post loaded")
	return p, nil
}

// postError is why a post didn't come: the session's trouble or the
// service's first, and only then Instagram saying it has no such post.
func postError(err error, body []byte) error {
	err = feedError(err, body, errPost)
	var pe *proto.Error
	if errors.As(err, &pe) && pe.Code == proto.NetworkError && pe.Message == errPost && gone(body) {
		return errPostGone
	}
	return err
}

// gone reports whether Instagram's answer says it has no such post, or
// none this account may see: not that it can't answer now ("Service
// Unavailable").
func gone(body []byte) bool {
	var a feedAnswer
	if json.Unmarshal(body, &a) != nil || a.Status != "fail" {
		return false
	}
	msg := strings.ToLower(a.Message)
	for _, now := range []string{"service", "server", "temporar", "try again", "later"} {
		if strings.Contains(msg, now) {
			return false
		}
	}
	return strings.Contains(msg, "not found") || strings.Contains(msg, "unavailable") ||
		strings.Contains(msg, "private") || strings.Contains(msg, "does not exist") || strings.Contains(msg, "invalid media")
}

// parseMediaInfo reads the post an answer is about: {"items":[media]}; nil
// when it has none.
func parseMediaInfo(body []byte) (*feedMedia, error) {
	var r struct {
		Items []feedMedia `json:"items"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	if len(r.Items) == 0 {
		return nil, nil
	}
	return &r.Items[0], nil
}
