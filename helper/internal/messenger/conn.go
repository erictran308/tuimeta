// SPDX-License-Identifier: AGPL-3.0-or-later

package messenger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
	"go.mau.fi/util/exhttp"
	"go.mau.fi/whatsmeow"
	armadillo "go.mau.fi/whatsmeow/proto"
	"go.mau.fi/whatsmeow/proto/waMediaTransport"
	"go.mau.fi/whatsmeow/proto/waMsgApplication"
	waTypes "go.mau.fi/whatsmeow/types"

	"go.mau.fi/mautrix-meta/pkg/messagix"
	"go.mau.fi/mautrix-meta/pkg/messagix/cookies"
	"go.mau.fi/mautrix-meta/pkg/messagix/httpclient"
	"go.mau.fi/mautrix-meta/pkg/messagix/socket"
	"go.mau.fi/mautrix-meta/pkg/messagix/table"
	"go.mau.fi/mautrix-meta/pkg/messagix/types"
)

// The libraries log through zerolog, and at debug and trace levels they log
// message contents, URLs with tokens and whole server responses. Every
// logger they're handed is a disabled one, and zerolog's global logger (which
// messagix's table decoder writes to, and which would go to stderr) is
// silenced too. What the helper logs about Messenger it logs itself, through
// hlog, as fixed words and error kinds.
var quiet = zerolog.Nop()

func silenceLibraries() { zlog.Logger = zerolog.Nop() }

// noProxy keeps the libraries' HTTP clients off the environment's proxy
// settings: the helper reads no environment variables, and a proxy someone
// slipped into the environment would see every request.
func noProxy(*http.Request) (*url.URL, error) { return nil, nil }

// clientSettings are messagix's HTTP settings: the usual timeouts, no proxy.
func clientSettings() exhttp.ClientSettings {
	s := exhttp.SensibleClientSettings
	s.HTTPProxy = noProxy
	return s
}

// metaAPI is what the backend uses of the Messenger connection. Tasks go
// through runTasks, which refuses the kinds that would tell other people
// something unless the caller says it's sending exactly that on purpose.
type metaAPI interface {
	ExecuteTasks(ctx context.Context, tasks ...socket.Task) (*table.LSTable, error)
	ExecuteStatelessTask(ctx context.Context, task socket.Task) error
	FetchMoreThreads(ctx context.Context, syncGroup int64) (*socket.KeyStoreData, *table.LSTable, error)
	Upload(ctx context.Context, threadKey int64, media *httpclient.MercuryUploadMedia) (int64, error)
	Cursor(db int64) string
	WaitUntilCanSend(ctx context.Context, timeout time.Duration) error
	PostHandle(tbl *table.LSTable)
	Logout(ctx context.Context) error
	Cookies() map[string]string
	Disconnect()
}

// metaConn is the live connection: a messagix client, of which only the
// calls above are reachable.
type metaConn struct{ cli *messagix.Client }

func (c *metaConn) ExecuteTasks(ctx context.Context, tasks ...socket.Task) (*table.LSTable, error) {
	return c.cli.ExecuteTasks(ctx, tasks...)
}

func (c *metaConn) ExecuteStatelessTask(ctx context.Context, task socket.Task) error {
	return c.cli.ExecuteStatelessTask(ctx, task)
}

func (c *metaConn) FetchMoreThreads(ctx context.Context, syncGroup int64) (*socket.KeyStoreData, *table.LSTable, error) {
	return c.cli.FetchMoreThreads(ctx, syncGroup)
}

func (c *metaConn) Upload(ctx context.Context, threadKey int64, media *httpclient.MercuryUploadMedia) (int64, error) {
	resp, err := c.cli.GetHTTP().SendMercuryUploadRequest(ctx, threadKey, media)
	if err != nil {
		return 0, err
	}
	if resp.Payload.RealMetadata == nil {
		return 0, errors.New("upload response without metadata")
	}
	id := resp.Payload.RealMetadata.GetFbId()
	if id == 0 {
		return 0, errors.New("upload response without an id")
	}
	return id, nil
}

func (c *metaConn) Cursor(db int64) string { return c.cli.GetCursor(db) }

func (c *metaConn) WaitUntilCanSend(ctx context.Context, timeout time.Duration) error {
	return c.cli.WaitUntilCanSendMessages(ctx, timeout)
}

func (c *metaConn) PostHandle(tbl *table.LSTable) { c.cli.PostHandlePublishResponse(tbl) }

func (c *metaConn) Cookies() map[string]string {
	all := c.cli.GetCookies().GetAll()
	out := make(map[string]string, len(all))
	for k, v := range all {
		out[string(k)] = v
	}
	return out
}

func (c *metaConn) Disconnect() {
	c.cli.SetEventHandler(nil)
	c.cli.Disconnect()
}

// Logout ends the facebook.com web session the cookies belong to, as the
// website's own "Log out" does, so they stop working everywhere.
func (c *metaConn) Logout(ctx context.Context) error {
	token := logoutToken(c.cli)
	if token == "" {
		return errors.New("no logout token")
	}
	h := c.cli.GetHTTP()
	q := h.NewHTTPQuery()
	form := url.Values{"fb_dtsg": {q.FbDtsg}, "jazoest": {q.Jazoest}, "ref": {"mb"}, "h": {token}}
	headers := h.BuildHeaders(true, false)
	headers.Set("origin", c.cli.GetEndpoint("base_url"))
	headers.Set("referer", c.cli.GetEndpoint("base_url")+"/")
	headers.Set("sec-fetch-site", "same-origin")
	resp, _, err := h.MakeRequestOnceNoRedirect(ctx, c.cli.GetEndpoint("base_url")+"/logout.php", http.MethodPost, headers, []byte(form.Encode()), types.FORM)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("logout status %d", resp.StatusCode)
	}
	return nil
}

// newMessagix makes a messagix client for the facebook.com site (messenger.com
// closed in April 2026) with the given cookies, logging nowhere.
func newMessagix(values map[string]string) *messagix.Client {
	c := &cookies.Cookies{Platform: types.Facebook}
	vals := make(map[cookies.MetaCookieName]string, len(values))
	for k, v := range values {
		if dropCookie(k) {
			continue
		}
		vals[cookies.MetaCookieName(k)] = v
	}
	c.UpdateValues(vals)
	return messagix.NewClient(c, quiet, &messagix.Config{ClientSettings: clientSettings()})
}

// dropCookie names cookies that aren't handed to messagix: "presence" is the
// web page's own record of the chat sidebar's state, written by the page's
// scripts; tuimeta has no sidebar and says nothing about being around.
func dropCookie(name string) bool { return name == "presence" }

// e2eeAPI is what the backend uses of the encrypted chats' connection. There
// is deliberately no way to set a presence through it: whatsmeow's
// SendPresence, SubscribePresence and SetForceActiveDeliveryReceipts aren't
// reachable. SendChatPresence is typing, sent only from SetTyping.
type e2eeAPI interface {
	SendFBMessage(ctx context.Context, to waTypes.JID, msg armadillo.RealMessageApplicationSub, meta *waMsgApplication.MessageApplication_Metadata, extra whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error)
	MarkRead(ctx context.Context, ids []waTypes.MessageID, at time.Time, chat, sender waTypes.JID) error
	SendChatPresence(ctx context.Context, chat waTypes.JID, state waTypes.ChatPresence, media waTypes.ChatPresenceMedia) error
	DownloadFB(ctx context.Context, transport *waMediaTransport.WAMediaTransport_Integral, mediaType whatsmeow.MediaType) ([]byte, error)
	Upload(ctx context.Context, data []byte, mediaType whatsmeow.MediaType) (whatsmeow.UploadResponse, error)
	GetGroupInfo(ctx context.Context, jid waTypes.JID) (*waTypes.GroupInfo, error)
	OwnJID() waTypes.JID
	Disconnect()
}

// e2eeConn is the live encrypted chats' connection.
type e2eeConn struct{ cli *whatsmeow.Client }

func (c *e2eeConn) SendFBMessage(ctx context.Context, to waTypes.JID, msg armadillo.RealMessageApplicationSub, meta *waMsgApplication.MessageApplication_Metadata, extra whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	return c.cli.SendFBMessage(ctx, to, msg, meta, extra)
}

func (c *e2eeConn) MarkRead(ctx context.Context, ids []waTypes.MessageID, at time.Time, chat, sender waTypes.JID) error {
	return c.cli.MarkRead(ctx, ids, at, chat, sender)
}

func (c *e2eeConn) SendChatPresence(ctx context.Context, chat waTypes.JID, state waTypes.ChatPresence, media waTypes.ChatPresenceMedia) error {
	return c.cli.SendChatPresence(ctx, chat, state, media)
}

func (c *e2eeConn) DownloadFB(ctx context.Context, transport *waMediaTransport.WAMediaTransport_Integral, mediaType whatsmeow.MediaType) ([]byte, error) {
	return c.cli.DownloadFB(ctx, transport, mediaType)
}

func (c *e2eeConn) Upload(ctx context.Context, data []byte, mediaType whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	return c.cli.Upload(ctx, data, mediaType)
}

func (c *e2eeConn) GetGroupInfo(ctx context.Context, jid waTypes.JID) (*waTypes.GroupInfo, error) {
	return c.cli.GetGroupInfo(ctx, jid)
}

func (c *e2eeConn) OwnJID() waTypes.JID {
	if c.cli.Store.ID == nil {
		return waTypes.EmptyJID
	}
	return *c.cli.Store.ID
}

func (c *e2eeConn) Disconnect() { c.cli.Disconnect() }

// configureE2EE switches off what whatsmeow would otherwise do or keep by
// itself that isn't needed:
//
//   - no proxy from the environment;
//   - sent messages are kept for retries in memory only, never in the
//     database (UseRetryMessageStore);
//   - no asking a phone to re-send messages (Messenger has no phone);
//   - delivery receipts stay of the "inactive" kind, which the official apps
//     don't show, since nothing ever marks this device online.
//
// Acks go out after the backend has stored a message (SynchronousAck), so a
// message that arrives as the helper quits is delivered again rather than
// lost; until then its decrypted form waits in the store's buffer, which
// whatsmeow empties once the message is handled.
func configureE2EE(cli *whatsmeow.Client) {
	cli.SetProxy(nil)
	cli.UseRetryMessageStore = false
	cli.AutomaticMessageRerequestFromPhone = false
	cli.SetForceActiveDeliveryReceipts(false)
	cli.SendReportingTokens = false
	cli.EnableAutoReconnect = true
	cli.InitialAutoReconnect = true
	cli.SynchronousAck = true
	cli.EnableDecryptedEventBuffer = true
}

// logoutToken digs the web page's logout token out of messagix's saved
// state, the only place it's exposed.
func logoutToken(cli *messagix.Client) string {
	state, err := cli.DumpState()
	if err != nil || state == nil {
		return ""
	}
	var dumped struct {
		Configs struct {
			BrowserConfigTable struct {
				MessengerWebInitData struct {
					LogoutToken string `json:"logoutToken"`
				}
			}
		}
	}
	if json.Unmarshal(state, &dumped) != nil {
		return ""
	}
	return dumped.Configs.BrowserConfigTable.MessengerWebInitData.LogoutToken
}
