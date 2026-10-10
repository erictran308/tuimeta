// SPDX-License-Identifier: AGPL-3.0-or-later

package whatsapp

import (
	"context"
	"errors"
	"io"
	"os"
	"runtime"
	"sync/atomic"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/store"
	waTypes "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	gproto "google.golang.org/protobuf/proto"

	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// waAPI is what the backend uses of the WhatsApp connection. There is
// deliberately no way to set a presence through it: whatsmeow's
// SendPresence, SubscribePresence, SetForceActiveDeliveryReceipts and
// SetPassive aren't reachable. Read receipts and typing take the request
// they're for, and go out only for mark_read and typing; the only app state
// it sends is a chat's mute.
type waAPI interface {
	SendMessage(ctx context.Context, to waTypes.JID, msg *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error)
	MarkRead(ctx context.Context, purpose string, ids []waTypes.MessageID, at time.Time, chat, sender waTypes.JID) error
	SendChatPresence(ctx context.Context, purpose string, chat waTypes.JID, state waTypes.ChatPresence) error
	Mute(ctx context.Context, chat waTypes.JID, muted bool) error
	// Download writes the attachment's decrypted bytes to out, through a
	// private file in dir that's never let grow past limit.
	Download(ctx context.Context, m *media, dir string, limit int64, out io.Writer) error
	Upload(ctx context.Context, data []byte, mediaType whatsmeow.MediaType) (whatsmeow.UploadResponse, error)
	GetGroupInfo(ctx context.Context, jid waTypes.JID) (*waTypes.GroupInfo, error)
	ProfilePicture(ctx context.Context, jid waTypes.JID) (*waTypes.ProfilePictureInfo, error)
	IsOnWhatsApp(ctx context.Context, phones []string) ([]waTypes.IsOnWhatsAppResponse, error)
	DecryptReaction(ctx context.Context, evt *events.Message) (*waE2E.ReactionMessage, error)
	ParseWebMessage(chat waTypes.JID, msg *waWeb.WebMessageInfo) (*events.Message, error)
	GenerateMessageID() waTypes.MessageID
	BuildEdit(chat waTypes.JID, id waTypes.MessageID, content *waE2E.Message) *waE2E.Message
	BuildRevoke(chat, sender waTypes.JID, id waTypes.MessageID) *waE2E.Message
	BuildReaction(chat, sender waTypes.JID, id waTypes.MessageID, emoji string) *waE2E.Message
	BuildHistorySyncRequest(oldest *waTypes.MessageInfo, count int) *waE2E.Message

	// The account's own ids, and its address book as the phone shares it.
	OwnID() waTypes.JID
	OwnLID() waTypes.JID
	PushName() string
	Contact(ctx context.Context, jid waTypes.JID) (waTypes.ContactInfo, error)
	Contacts(ctx context.Context) (map[waTypes.JID]waTypes.ContactInfo, error)
	LIDForPN(ctx context.Context, pn waTypes.JID) (waTypes.JID, error)
	PNForLID(ctx context.Context, lid waTypes.JID) (waTypes.JID, error)

	// Logout unlinks this device from the account.
	Logout(ctx context.Context) error
	Disconnect()
}

// errUnasked stops a receipt or a typing notice no request asked for.
var errUnasked = proto.Err(proto.Internal, "Something was about to tell people about you unasked; it was stopped.")

// waConn is the live connection: a whatsmeow client, of which only the calls
// above are reachable.
type waConn struct {
	cli *whatsmeow.Client
	// settled is set once the account's privacy settings were asked for on
	// the current connection. whatsmeow keeps them, but only WhatsApp's
	// notices of a change update its copy, and a device that was offline
	// when the setting changed on the phone misses them.
	settled atomic.Bool
}

// newConn wraps cli, and watches it connect.
func newConn(cli *whatsmeow.Client) *waConn {
	c := &waConn{cli: cli}
	cli.AddEventHandler(func(evt any) {
		if _, ok := evt.(*events.Connected); ok {
			c.reconnected()
		}
	})
	return c
}

// reconnected makes the next read receipt ask for the setting afresh.
func (c *waConn) reconnected() { c.settled.Store(false) }

func (c *waConn) SendMessage(ctx context.Context, to waTypes.JID, msg *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	return c.cli.SendMessage(ctx, to, msg, extra...)
}

func (c *waConn) MarkRead(ctx context.Context, purpose string, ids []waTypes.MessageID, at time.Time, chat, sender waTypes.JID) error {
	if purpose != "mark_read" {
		return errUnasked
	}
	// With read receipts off in WhatsApp's settings, a read is told only to
	// your own devices. whatsmeow checks that setting itself, but sends a
	// receipt everyone sees when it can't get it: it's got first here (from
	// WhatsApp, once each connection), and without it nothing goes.
	fetch := !c.settled.Load()
	settings, err := c.cli.TryFetchPrivacySettings(ctx, fetch)
	if err != nil {
		return err
	}
	if fetch {
		c.settled.Store(true)
	}
	return c.cli.MarkRead(ctx, ids, at, chat, sender, receiptKind(settings))
}

// receiptKind is how a read is told: to everyone only when the account's
// setting says so, else (read receipts off, or a setting not known) only to
// your own devices.
func receiptKind(s *waTypes.PrivacySettings) waTypes.ReceiptType {
	if s != nil && s.ReadReceipts == waTypes.PrivacySettingAll {
		return waTypes.ReceiptTypeRead
	}
	return waTypes.ReceiptTypeReadSelf
}

func (c *waConn) SendChatPresence(ctx context.Context, purpose string, chat waTypes.JID, state waTypes.ChatPresence) error {
	if purpose != "typing" {
		return errUnasked
	}
	return c.cli.SendChatPresence(ctx, chat, state, waTypes.ChatPresenceMediaText)
}

func (c *waConn) Mute(ctx context.Context, chat waTypes.JID, muted bool) error {
	return c.cli.SendAppState(ctx, appstate.BuildMute(chat, muted, 0))
}

func (c *waConn) Download(ctx context.Context, m *media, dir string, limit int64, out io.Writer) error {
	f, err := os.CreateTemp(dir, "download-*")
	if err != nil {
		return err
	}
	defer func() {
		f.Close()
		os.Remove(f.Name())
	}()
	// The size a message says can be anything: what the server sends is
	// what's capped, on disk, not in memory. 64 bytes are the encryption's
	// padding and checksum.
	capped := &cappedFile{File: f, limit: limit + 64}
	err = c.cli.DownloadMediaWithPathToFile(ctx, m.DirectPath, m.FileEncSHA256, m.FileSHA256, m.MediaKey, whatsmeow.MediaType(m.MediaType), mmsType(m), false, capped)
	if err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	_, err = io.Copy(out, f)
	return err
}

// errTooBig stops a download that grew past the limit.
var errTooBig = errors.New("download over the size limit")

// cappedFile is a file that refuses to grow past limit bytes.
type cappedFile struct {
	*os.File
	limit int64
}

func (f *cappedFile) Write(p []byte) (int, error) {
	pos, err := f.File.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	if pos+int64(len(p)) > f.limit {
		return 0, errTooBig
	}
	return f.File.Write(p)
}

func (f *cappedFile) WriteAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > f.limit {
		return 0, errTooBig
	}
	return f.File.WriteAt(p, off)
}

// ReadFrom goes through Write, so io.Copy into the file is capped too
// (*os.File's own ReadFrom would bypass it).
func (f *cappedFile) ReadFrom(r io.Reader) (int64, error) {
	return io.Copy(struct{ io.Writer }{f}, r)
}

func (f *cappedFile) Truncate(size int64) error {
	if size > f.limit {
		return errTooBig
	}
	return f.File.Truncate(size)
}

func (c *waConn) Upload(ctx context.Context, data []byte, mediaType whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	return c.cli.Upload(ctx, data, mediaType)
}

func (c *waConn) GetGroupInfo(ctx context.Context, jid waTypes.JID) (*waTypes.GroupInfo, error) {
	return c.cli.GetGroupInfo(ctx, jid)
}

func (c *waConn) ProfilePicture(ctx context.Context, jid waTypes.JID) (*waTypes.ProfilePictureInfo, error) {
	return c.cli.GetProfilePictureInfo(ctx, jid, &whatsmeow.GetProfilePictureParams{Preview: true, IsCommunity: false})
}

func (c *waConn) IsOnWhatsApp(ctx context.Context, phones []string) ([]waTypes.IsOnWhatsAppResponse, error) {
	return c.cli.IsOnWhatsApp(ctx, phones)
}

func (c *waConn) DecryptReaction(ctx context.Context, evt *events.Message) (*waE2E.ReactionMessage, error) {
	return c.cli.DecryptReaction(ctx, evt)
}

func (c *waConn) ParseWebMessage(chat waTypes.JID, msg *waWeb.WebMessageInfo) (*events.Message, error) {
	return c.cli.ParseWebMessage(chat, msg)
}

func (c *waConn) GenerateMessageID() waTypes.MessageID { return c.cli.GenerateMessageID() }

func (c *waConn) BuildEdit(chat waTypes.JID, id waTypes.MessageID, content *waE2E.Message) *waE2E.Message {
	return c.cli.BuildEdit(chat, id, content)
}

func (c *waConn) BuildRevoke(chat, sender waTypes.JID, id waTypes.MessageID) *waE2E.Message {
	return c.cli.BuildRevoke(chat, sender, id)
}

func (c *waConn) BuildReaction(chat, sender waTypes.JID, id waTypes.MessageID, emoji string) *waE2E.Message {
	return c.cli.BuildReaction(chat, sender, id, emoji)
}

func (c *waConn) BuildHistorySyncRequest(oldest *waTypes.MessageInfo, count int) *waE2E.Message {
	return c.cli.BuildHistorySyncRequest(oldest, count)
}

func (c *waConn) OwnID() waTypes.JID {
	if c.cli.Store.ID == nil {
		return waTypes.EmptyJID
	}
	return c.cli.Store.ID.ToNonAD()
}

func (c *waConn) OwnLID() waTypes.JID { return c.cli.Store.GetLID().ToNonAD() }

func (c *waConn) PushName() string { return c.cli.Store.PushName }

func (c *waConn) Contact(ctx context.Context, jid waTypes.JID) (waTypes.ContactInfo, error) {
	return c.cli.Store.Contacts.GetContact(ctx, jid)
}

func (c *waConn) Contacts(ctx context.Context) (map[waTypes.JID]waTypes.ContactInfo, error) {
	return c.cli.Store.Contacts.GetAllContacts(ctx)
}

func (c *waConn) LIDForPN(ctx context.Context, pn waTypes.JID) (waTypes.JID, error) {
	return c.cli.Store.LIDs.GetLIDForPN(ctx, pn)
}

func (c *waConn) PNForLID(ctx context.Context, lid waTypes.JID) (waTypes.JID, error) {
	return c.cli.Store.LIDs.GetPNForLID(ctx, lid)
}

func (c *waConn) Logout(ctx context.Context) error { return c.cli.Logout(ctx) }

func (c *waConn) Disconnect() { c.cli.Disconnect() }

// mmsType is the kind of media WhatsApp's media servers are asked for.
func mmsType(m *media) string {
	switch whatsmeow.MediaType(m.MediaType) {
	case whatsmeow.MediaImage:
		if m.Kind == proto.Sticker {
			return "sticker"
		}
		return "image"
	case whatsmeow.MediaVideo:
		return "video"
	case whatsmeow.MediaAudio:
		return "audio"
	case whatsmeow.MediaDocument:
		return "document"
	}
	return ""
}

// configure switches off what whatsmeow would otherwise do or keep by itself
// that isn't needed:
//
//   - no proxy from the environment;
//   - sent messages are kept for retries in memory only (UseRetryMessageStore);
//   - delivery receipts stay of the "inactive" kind, since nothing ever marks
//     this device online.
//
// Messages that can't be decrypted are asked of the phone again, as WhatsApp
// Web does. Acks go out after the backend has stored a message
// (SynchronousAck), so a message that arrives as the helper quits is
// delivered again rather than lost.
func configure(cli *whatsmeow.Client) {
	cli.SetProxy(nil)
	cli.UseRetryMessageStore = false
	cli.AutomaticMessageRerequestFromPhone = true
	cli.SetForceActiveDeliveryReceipts(false)
	cli.SendReportingTokens = true
	cli.EnableAutoReconnect = true
	cli.InitialAutoReconnect = true
	cli.SynchronousAck = true
	cli.EnableDecryptedEventBuffer = true
	cli.EmitAppStateEventsOnFullSync = true
}

// describeDevice is how this device is named in the phone's list of linked
// devices: as WhatsApp Web in Chrome on this computer's system, the way the
// browser it stands in for would be. whatsmeow keeps this in package
// globals, which Messenger's encrypted chats don't read.
func describeDevice() {
	store.DeviceProps.Os = gproto.String(systemName())
	store.DeviceProps.PlatformType = waCompanionReg.DeviceProps_CHROME.Enum()
	// No full history: the phone sends its recent chats, and older
	// messages are asked of it as they're scrolled to.
	store.DeviceProps.RequireFullSync = gproto.Bool(false)
}

// systemName is this computer's system as WhatsApp Web names it.
func systemName() string {
	switch runtime.GOOS {
	case "darwin":
		return "Mac OS"
	case "windows":
		return "Windows"
	}
	return "Linux"
}

// pairName is the name a phone-number link shows on the phone before it's
// confirmed: "Browser (OS)", which WhatsApp checks.
func pairName() string { return "Chrome (" + systemName() + ")" }
