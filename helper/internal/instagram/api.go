// SPDX-License-Identifier: AGPL-3.0-or-later

package instagram

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-meta/pkg/instameow"
	"go.mau.fi/mautrix-meta/pkg/instameow/slidetypes"
	mcookies "go.mau.fi/mautrix-meta/pkg/messagix/cookies"
	"go.mau.fi/mautrix-meta/pkg/messagix/httpclient"
	"go.mau.fi/mautrix-meta/pkg/messagix/types"
	"go.mau.fi/util/exhttp"
)

// api is the part of instameow's Client the backend uses. The real one is
// the library; tests put a recording stand-in here, so nothing in a test can
// reach Instagram.
type api interface {
	LoadIndex(ctx context.Context) (*types.PolarisViewer, *slidetypes.Mailbox, error)
	GetOwnFBID() int64
	IsAuthenticated() bool
	GetCookies() *mcookies.Cookies
	Connect(ctx context.Context)
	Disconnect()

	GetMailbox(ctx context.Context) (*slidetypes.MailboxResponse, error)
	PaginateMailbox(ctx context.Context, req *slidetypes.PaginateMailboxRequest) (*slidetypes.PaginateMailboxResponse, error)
	GetThread(ctx context.Context, req *slidetypes.GetThreadInfoRequest) (*slidetypes.ThreadInfoResponse, error)
	PaginateMessages(ctx context.Context, req *slidetypes.PaginateMessagesRequest) (*slidetypes.PaginateMessagesResponse, error)
	FetchThreadID(ctx context.Context, threadFBID int64) (*instameow.ThreadIGIDs, error)
	GetUserForNewDM(ctx context.Context, fbid int64) (*slidetypes.UserInfoResponse, error)
	SearchUsers(ctx context.Context, query string) (*slidetypes.SearchResponse, error)

	SendMessage(ctx context.Context, req *slidetypes.SendTextRequest) (*slidetypes.SendTextResponse, error)
	SendMedia(ctx context.Context, req *slidetypes.SendMediaRequest) (*slidetypes.SendMediaResponse, error)
	EditMessage(ctx context.Context, req *slidetypes.EditMessageRequest) (*slidetypes.EditMessageResponse, error)
	UnsendMessage(ctx context.Context, req *slidetypes.UnsendMessageRequest) (*slidetypes.UnsendMessageResponse, error)
	SendReaction(ctx context.Context, req *slidetypes.CreateReactionRequest) (*slidetypes.SendReactionResponse, error)
	MarkRead(ctx context.Context, req *slidetypes.MarkReadRequest) (*slidetypes.MarkReadResponse, error)
	MarkReadValidation(ctx context.Context, req *slidetypes.MarkReadRequest) (*slidetypes.MarkReadValidationResponse, error)
	MuteThread(ctx context.Context, req *slidetypes.MuteThreadRequest) (*slidetypes.MuteThreadResponse, error)
	AcceptMessageRequest(ctx context.Context, req *slidetypes.AcceptMessageRequestRequest) (*slidetypes.AcceptMessageRequestResponse, error)
	SetTyping(ctx context.Context, threadID string, typing bool) error

	// Upload sends a file to Instagram's upload endpoint and returns the
	// attachment id a media message then names.
	Upload(ctx context.Context, threadKey int64, name, mime string, data []byte, voice bool) (int64, error)
}

// realAPI is instameow itself.
type realAPI struct{ *instameow.Client }

// dialReal makes the library's client. Typing stays enabled: instameow then
// keeps a socket that receives other people's typing, and sends yours only
// when SetTyping is called (which only SetTyping here does).
func dialReal(c *mcookies.Cookies, handler instameow.EventHandler) api {
	return realAPI{instameow.NewClient(instameow.ClientParams{
		Cookies:      c,
		Log:          quietLogger(),
		Settings:     httpSettings(),
		EventHandler: handler,
	})}
}

// httpSettings are the library's HTTP settings: sensible timeouts, and no
// proxy. Left alone, the HTTP library would take one from HTTPS_PROXY and
// the like, and the helper reads no environment variables (PROTOCOL.md): a
// variable left over in a shell could otherwise route the session's
// cookies through someone else's machine.
func httpSettings() exhttp.ClientSettings {
	s := exhttp.SensibleClientSettings
	s.HTTPProxy = noProxy
	return s
}

func noProxy(*http.Request) (*url.URL, error) { return nil, nil }

func (r realAPI) Upload(ctx context.Context, threadKey int64, name, mime string, data []byte, voice bool) (int64, error) {
	resp, err := r.GetHTTP().SendMercuryUploadRequest(ctx, threadKey, &httpclient.MercuryUploadMedia{
		Filename:    name,
		MimeType:    mime,
		MediaData:   data,
		IsVoiceClip: voice,
	})
	if err != nil {
		return 0, err
	}
	if resp.Payload.RealMetadata == nil {
		return 0, fmt.Errorf("instagram: upload answered without metadata")
	}
	return resp.Payload.RealMetadata.GetFbId(), nil
}

// quietLogger is the only logger instameow gets. At debug and trace levels
// the library logs message contents, URLs with tokens and cookie names, so
// it writes nothing at all: what the helper needs in its log (connection
// states, error kinds) the backend writes itself through hlog. Its level
// isn't Disabled, so that withQuietLog really stores it in a context
// (zerolog skips storing disabled loggers, and a context without one falls
// back to whatever zerolog.DefaultContextLogger holds).
func quietLogger() zerolog.Logger {
	return zerolog.New(io.Discard).Level(zerolog.PanicLevel)
}

// withQuietLog is ctx carrying the quiet logger, for every context handed to
// instameow: parts of the library log through zerolog.Ctx(ctx).
func withQuietLog(ctx context.Context) context.Context {
	l := quietLogger()
	return l.WithContext(ctx)
}
