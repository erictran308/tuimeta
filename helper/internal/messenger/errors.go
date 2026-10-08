// SPDX-License-Identifier: AGPL-3.0-or-later

package messenger

import (
	"context"
	"errors"
	stdnet "net"

	"go.mau.fi/whatsmeow"

	"go.mau.fi/mautrix-meta/pkg/messagix"
	"go.mau.fi/mautrix-meta/pkg/messagix/httpclient"
	mtypes "go.mau.fi/mautrix-meta/pkg/messagix/types"

	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// The sentences tuimeta shows. None quotes a library's error: those can hold
// URLs with tokens.
var (
	errCheckpoint       = proto.Err(proto.Checkpoint, "Facebook wants you to confirm it's you: open facebook.com in a browser, then log in here again.")
	errConsent          = proto.Err(proto.Checkpoint, "Facebook wants you to accept something first: open facebook.com in a browser, then log in here again.")
	errSuspended        = proto.Err(proto.Checkpoint, "Facebook says this account is suspended: open facebook.com in a browser to see what it needs.")
	errBadCookies       = proto.Err(proto.BadCookies, "Facebook didn't accept these cookies: log in to facebook.com in a browser and copy them again.")
	errLoggedOutCookies = proto.Err(proto.BadCookies, "These cookies are from a logged-out facebook.com tab: log in there, then copy them again.")
	errMixedCookies     = proto.Err(proto.BadCookies, "These cookies belong to two different accounts: copy them again, all from one logged-in tab.")
	errRateLimited      = proto.Err(proto.NetworkError, "Facebook is limiting requests from this network; wait a few minutes and try again.")
	errNetwork          = proto.Err(proto.NetworkError, "Can't reach Facebook; check the connection and try again.")
	errPage             = proto.Err(proto.NetworkError, "Facebook's Messenger page didn't load as expected; try again in a minute.")
	errNotConnected     = proto.Err(proto.NetworkError, "Messenger isn't connected right now; try again in a moment.")
	errNoE2EE           = proto.Err(proto.NetworkError, "Encrypted chats aren't connected right now; try again in a moment.")
	errRefused          = proto.Err(proto.NetworkError, "Messenger didn't accept that; try again.")
)

// loginError turns what loading the Messenger page with the cookies failed
// with into a sentence and a code a person can act on.
func loginError(err error) error {
	switch {
	case errors.Is(err, httpclient.ErrCheckpointRequired), errors.Is(err, httpclient.ErrChallengeRequired):
		return errCheckpoint
	case errors.Is(err, httpclient.ErrConsentRequired):
		return errConsent
	case errors.Is(err, httpclient.ErrAccountSuspended):
		return errSuspended
	case errors.Is(err, httpclient.ErrUserIDIsZero):
		return errLoggedOutCookies
	case errors.Is(err, httpclient.ErrTokenInvalidated):
		return errBadCookies
	case errors.Is(err, httpclient.ErrRateLimited):
		return errRateLimited
	}
	if lsErr := (&mtypes.ErrorResponse{}); errors.As(err, &lsErr) && lsErr.ErrorCode == 1357053 {
		// Facebook's "please log in again".
		return errBadCookies
	}
	if isNetwork(err) {
		return errNetwork
	}
	return errPage
}

// isNetwork reports errors that come from not reaching Facebook at all.
func isNetwork(err error) bool {
	var ne stdnet.Error
	return errors.As(err, &ne) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, httpclient.ErrRequestFailed) ||
		errors.Is(err, httpclient.ErrResponseReadFailed) ||
		errors.Is(err, httpclient.ErrMaxRetriesReached) ||
		errors.Is(err, httpclient.ErrUnexpectedError)
}

// requestError turns a failed request to Messenger into a sentence.
func requestError(err error) error {
	var pe *proto.Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &pe):
		return pe
	case errors.Is(err, context.Canceled):
		return err
	case errors.Is(err, httpclient.ErrTokenInvalidated):
		return proto.Err(proto.BadCookies, "Facebook logged this session out: log in again with fresh cookies.")
	case errors.Is(err, httpclient.ErrCheckpointRequired), errors.Is(err, httpclient.ErrChallengeRequired):
		return errCheckpoint
	case errors.Is(err, httpclient.ErrRateLimited):
		return errRateLimited
	case errors.Is(err, messagix.ErrClientIsNil), errors.Is(err, whatsmeow.ErrNotConnected), errors.Is(err, whatsmeow.ErrNotLoggedIn):
		return errNotConnected
	case errors.Is(err, whatsmeow.ErrMessageTimedOut):
		return proto.Err(proto.NetworkError, "Messenger didn't confirm in time; check the connection and try again.")
	case isNetwork(err):
		return errNetwork
	}
	return errRefused
}
