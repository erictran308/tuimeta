// SPDX-License-Identifier: AGPL-3.0-or-later

package backend

import (
	"context"
	"strings"

	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// Linker is a network that logs in by linking tuimeta as a new device of
// the account (WhatsApp), instead of with cookies. The server sends
// login_link and cancel_login to it.
type Linker interface {
	// Link links a new device and returns once it's connected (account
	// ready). With phone empty it reports QR codes to scan as they come
	// (Events.LoginCode, with attempt); with a phone number (digits only,
	// PhoneDigits), the pairing code to type on the phone. A newer Link,
	// CancelLink or Logout ends a waiting one with cancelled, until the
	// phone has confirmed it (then only Logout does). Errors: timeout,
	// cancelled, network, unsupported.
	Link(ctx context.Context, phone string, attempt uint64) error

	// CancelLink ends the waiting Link of attempt (any, with 0), so no code
	// shown for it works any more. Nothing waiting, nothing happens.
	CancelLink(attempt uint64)
}

// Feeder is a network with a home feed (Instagram, Threads): the server
// sends load_feed to it, one load at a time.
type Feeder interface {
	// LoadFeed is the next posts of the feed, after those it gave since the
	// last fresh load (or login), and whether more remain. limit is a hint:
	// the network gives pages of its own size. fresh starts again from the
	// newest posts. box is what each picture is drawn in, so its thumbnail
	// is no bigger than that needs. kind is which of the network's feeds
	// (Threads'; "" is the default, and Instagram has only one): another
	// than the last load's starts from the top, as fresh does. It never
	// tells the network what was looked at.
	LoadFeed(ctx context.Context, limit int, fresh bool, box Box, kind FeedKind) (posts []proto.Post, more bool, err error)
}

// FeedKind is one of a network's feeds: Threads has two.
type FeedKind string

const (
	// ForYou is Threads' default feed: its picks, from everyone.
	ForYou FeedKind = "for_you"
	// Following is only the accounts followed, newest first.
	Following FeedKind = "following"
)

// ParseFeedKind reads load_feed's feed: absent is ForYou.
func ParseFeedKind(s string) (FeedKind, bool) {
	switch FeedKind(s) {
	case "", ForYou:
		return ForYou, true
	case Following:
		return Following, true
	}
	return "", false
}

// Box is the largest box, in pixels, tuimeta draws a post's picture in
// (load_feed's width and height); zero when it didn't say.
type Box struct {
	Width, Height int
}

// Commenter is a network whose feed's posts have comments (Instagram): the
// server sends load_comments to it, for posts its LoadFeed gave.
type Commenter interface {
	// LoadComments is a page of the comments on post postID, or with
	// repliesOf (a comment id it gave) of that comment's replies, oldest
	// first, and whether more remain. Without more, the first page; with
	// it, the page after the last one given. It never tells the network
	// what was read. Errors: not_found for a post or comment it didn't give
	// in this session.
	LoadComments(ctx context.Context, postID, repliesOf int64, more bool) (comments []proto.Comment, hasMore bool, err error)
}

// PhoneDigits reads a phone number in international form: digits, with an
// optional leading "+", and spaces, dashes, dots or brackets between them.
// It returns the digits, 7 to 15 of them (E.164's most), not starting with
// 0, which would be a number in national form.
func PhoneDigits(s string) (string, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "+")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '.' || r == '(' || r == ')':
		default:
			return "", false
		}
	}
	d := b.String()
	if len(d) < 7 || len(d) > 15 || d[0] == '0' {
		return "", false
	}
	return d, true
}
