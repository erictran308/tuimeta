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

	"github.com/erictran308/tuimeta/helper/internal/browser"
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

	// FeedTimeline asks for a page of the home feed with form (feedForm)
	// and returns the answer's body, also with an error, when Instagram
	// said why.
	FeedTimeline(ctx context.Context, form url.Values) ([]byte, error)

	// Comments GETs path (a post's comments, or a comment's replies) on
	// www.instagram.com with query, as from the post's page (referer), and
	// returns the answer's body, also with an error when Instagram said why.
	Comments(ctx context.Context, path string, query url.Values, referer string) ([]byte, error)
}

// realAPI is instameow itself.
type realAPI struct{ *instameow.Client }

// dialReal makes the library's client, saying it's the given browser.
// Typing stays enabled: instameow then keeps a socket that receives other
// people's typing, and sends yours only when SetTyping is called (which only
// SetTyping here does).
func dialReal(c *mcookies.Cookies, as browser.Identity, handler instameow.EventHandler) api {
	cli := instameow.NewClient(instameow.ClientParams{
		Cookies:      c,
		Log:          quietLogger(),
		Settings:     httpSettings(),
		EventHandler: handler,
	})
	as.Use(cli.GetHTTP())
	return realAPI{cli}
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

// FeedTimeline is the web client's own request for the home feed, through
// the session's client, so it carries the session's cookies and the
// browser it says it is (browser.Identity.Use wraps the client's
// transport), with the headers instameow's own web API requests have.
func (r realAPI) FeedTimeline(ctx context.Context, form url.Values) ([]byte, error) {
	cli := r.GetHTTP()
	base := r.GetEndpoint("base_url")
	h := cli.BuildHeaders(true, false)
	feedHeaders(h, base)
	_, body, err := cli.MakeRequest(ctx, base+feedPath, http.MethodPost, h, []byte(form.Encode()), types.FORM)
	return body, err
}

// Comments is the web client's request for a post's comments, or a
// comment's replies, made as FeedTimeline is, from the post's page: a GET,
// which a page's fetch sends without an Origin.
func (r realAPI) Comments(ctx context.Context, path string, query url.Values, referer string) ([]byte, error) {
	cli := r.GetHTTP()
	base := r.GetEndpoint("base_url")
	h := cli.BuildHeaders(true, false)
	commentHeaders(h, base, referer)
	_, body, err := cli.MakeRequest(ctx, commentsURL(base, path, query), http.MethodGet, h, nil, types.NONE)
	return body, err
}

// commentHeaders are feedHeaders for a GET from the post's page (referer;
// the site's own page without one).
func commentHeaders(h http.Header, base, referer string) {
	feedHeaders(h, base)
	h.Del("origin")
	if referer != "" {
		h.Set("referer", referer)
	}
}

// commentsURL is base and path, with query when it has anything.
func commentsURL(base, path string, query url.Values) string {
	if len(query) == 0 {
		return base + path
	}
	return base + path + "?" + query.Encode()
}

// feedHeaders are what the web client's feed request adds to the headers
// every web request has: the ones instameow's FetchMedia sets, as a fetch
// from the instagram.com page. (FetchMedia also sends X-Web-Session-ID,
// which instameow keeps to itself: the only way to it, NewHTTPQuery, also
// counts a GraphQL request, unguarded, while others may be counting.)
func feedHeaders(h http.Header, base string) {
	h.Set("x-requested-with", "XMLHttpRequest")
	h.Set("origin", base)
	h.Set("referer", base+"/")
	h.Set("sec-fetch-dest", "empty")
	h.Set("sec-fetch-mode", "cors")
	h.Set("sec-fetch-site", "same-origin")
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
