// SPDX-License-Identifier: AGPL-3.0-or-later

package instagram

import (
	"context"
	"errors"
	"net"

	"go.mau.fi/mautrix-meta/pkg/instameow"
	"go.mau.fi/mautrix-meta/pkg/messagix/httpclient"
	"go.mau.fi/mautrix-meta/pkg/messagix/types"

	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// What the user is told. Every sentence is the helper's own: the library's
// error texts can quote URLs with tokens, so they never reach tuimeta.
var (
	errBadCookies = proto.Err(proto.BadCookies,
		"Instagram didn't accept these cookies; copy them again from a tab where you're logged in to instagram.com.")
	errNoAccount = proto.Err(proto.BadCookies,
		"Instagram didn't say which account these cookies belong to; copy them again from a tab where you're logged in to instagram.com.")
	errCheckpoint = proto.Err(proto.Checkpoint,
		"Instagram wants you to confirm it's you: open instagram.com in a browser or the Instagram app, then log in here again.")
	errConsent = proto.Err(proto.Checkpoint,
		"Instagram wants you to accept its terms: open instagram.com in a browser or the Instagram app, then log in here again.")
	errSuspended = proto.Err(proto.Checkpoint,
		"Instagram has suspended this account: open instagram.com in a browser to see why.")
	errRateLimited = proto.Err(proto.NetworkError,
		"Instagram is limiting requests right now; wait a few minutes and try again.")
	errUnreachable = proto.Err(proto.NetworkError,
		"Couldn't reach Instagram; check your connection and try again.")
	errNotConnected = proto.Err(proto.NetworkError,
		"tuimeta-helper isn't connected to Instagram right now; try again in a moment.")
	errConnectTimeout = proto.Err(proto.NetworkError,
		"Instagram didn't open a connection in time; check your connection and try again.")
	errSessionEnded = proto.Err(proto.BadCookies,
		"Instagram ended this session; log in again with fresh cookies from instagram.com.")
	errKeepsClosing = proto.Err(proto.NetworkError,
		"Instagram keeps dropping the connection; restart tuimeta to try again.")
	errNoThread = proto.Err(proto.NotFound,
		"Instagram doesn't know that chat any more; reload the chat list.")
)

// authError is the sentence for an error that means the session can't go on
// (logged out, a checkpoint, a suspension), or nil for any other error.
func authError(err error) *proto.Error {
	switch {
	case errors.Is(err, httpclient.ErrChallengeRequired), errors.Is(err, httpclient.ErrCheckpointRequired):
		return errCheckpoint
	case errors.Is(err, httpclient.ErrConsentRequired):
		return errConsent
	case errors.Is(err, httpclient.ErrAccountSuspended):
		return errSuspended
	case errors.Is(err, httpclient.ErrTokenInvalidated):
		return errSessionEnded
	}
	return nil
}

// loginError is what a failed login (or resuming a saved one) tells the
// user. authenticated says whether Instagram recognized the cookies at all:
// with stale ones it answers with its login page, not an error.
func loginError(err error, authenticated bool) *proto.Error {
	var pe *proto.Error
	if errors.As(err, &pe) {
		return pe
	}
	if errors.Is(err, httpclient.ErrTokenInvalidated) {
		return errBadCookies
	}
	if a := authError(err); a != nil {
		return a
	}
	if errors.Is(err, httpclient.ErrRateLimited) {
		return errRateLimited
	}
	if unreachable(err) {
		return errUnreachable
	}
	if !authenticated {
		return errBadCookies
	}
	if lsErr := (&types.ErrorResponse{}); errors.As(err, &lsErr) && lsErr.ErrorCode == 1357053 {
		// The error the connector treats as bad credentials.
		return errBadCookies
	}
	return errUnreachable
}

// unreachable reports whether err is the network failing rather than
// Instagram refusing.
func unreachable(err error) bool {
	var netErr net.Error
	return errors.Is(err, httpclient.ErrRequestFailed) ||
		errors.Is(err, httpclient.ErrResponseReadFailed) ||
		errors.Is(err, httpclient.ErrMaxRetriesReached) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.As(err, &netErr)
}

// requestError turns a failed request into what tuimeta is told: what was
// being done (failed, one sentence) unless the session itself is in
// trouble, which says what to do about that instead.
func requestError(err error, failed string) error {
	if err == nil {
		return nil
	}
	var pe *proto.Error
	if errors.As(err, &pe) {
		return pe
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	if a := authError(err); a != nil {
		return a
	}
	if errors.Is(err, instameow.ErrThreadNotFound) {
		return errNoThread
	}
	if errors.Is(err, httpclient.ErrRateLimited) {
		return errRateLimited
	}
	return proto.Err(proto.NetworkError, failed)
}
