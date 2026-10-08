# tuimeta-helper

The part of [tuimeta](..) that speaks Messenger, Instagram and WhatsApp.
Meta's protocols are only implemented well in Go, in
[mautrix-meta](https://github.com/mautrix/meta) (`messagix` for Messenger,
`whatsmeow` for its end-to-end encrypted chats, `instameow` for Instagram)
and [whatsmeow](https://github.com/tulir/whatsmeow) (WhatsApp, which
mautrix-whatsapp drives), so tuimeta runs this program as a child process
and talks to it in newline-delimited JSON over stdin and stdout. The
contract is [PROTOCOL.md](PROTOCOL.md): a "tiny TDLib for Meta" that hides
each network's ids and protocols behind one small model.

```
tuimeta-helper --data-dir <absolute dir> [--fake]
```

`--fake` runs three made-up accounts with no network access at all, for
tuimeta's tests and for trying the app without an account. Without it, the
helper speaks the real networks: see Privacy → Instagram, Privacy →
Messenger and Privacy → WhatsApp for what each sends.

## Building

Go 1.27 or later. From this folder:

```sh
go build -o ../target/debug/tuimeta-helper .   # where tuimeta's dev build looks
go test ./...
go vet ./...
gofmt -l .                                     # prints nothing when formatted
```

It's pure Go (`CGO_ENABLED=0` works) and builds for Linux, macOS, Windows and
the BSDs. Besides mautrix-meta and whatsmeow, the encrypted chats' store
and WhatsApp's store use `modernc.org/sqlite`, a SQLite written in Go, so no C
compiler is needed.

## License

The helper is under the GNU Affero General Public License, version 3 or
later ([LICENSE](LICENSE)), because the mautrix-meta code it will be built on
is (whatsmeow itself is MPL-2.0). Every Go file starts with
`// SPDX-License-Identifier: AGPL-3.0-or-later`.
tuimeta itself is MIT: it's a separate program that starts the helper and
exchanges messages with it over a pipe, so the AGPL covers the helper, not
tuimeta. Whoever distributes a helper binary must offer its source (this
folder, with any changes).

## Privacy

What other people see about you goes out only when tuimeta asks, as in
tuigram:

- Read receipts only on `mark_read`, typing only on `typing`. No presence or
  "active now" at any time: whatever the libraries would send by themselves
  (marking threads read on sync, foreground pings, presence subscriptions that
  announce you) is switched off. Quitting (stdin closing, SIGHUP, SIGTERM,
  SIGINT) disconnects without marking anything read or setting a presence.
- Delivery acknowledgements the protocol needs in order to work are allowed
  and listed here. The fake sends none; each real backend adds its own here.
- Nothing is downloaded except on `download`, and no URL found in a message is
  ever fetched. Link previews come only from the network's own data.
- Files to send are read once, when `send_files` arrives: absolute paths of
  plain files only, never `\\server\share` paths (on Windows, looking at one
  hands the server your login hash).
- The browser it says it is: the libraries' own (Chrome 141 on Linux), or
  the Chrome a login named (`login_cookies`' `browser`), saved in the
  network's `session.json` and used for as long as that session lasts.
  WhatsApp, which has no cookies, links as WhatsApp Web in Chrome on this
  computer's system (see Privacy → WhatsApp).
  `internal/browser` wraps the libraries' HTTP and websocket clients and puts
  that Chrome's user agent and client hints in place of the libraries' on
  each request that carries them; Messenger's encrypted-chat handshake and
  both networks' downloads get them too. Requests that pretend to be a phone
  app, and the TLS handshake (always the libraries' Chrome), are left alone.
  A saved session naming a browser that doesn't parse isn't resumed.

What it keeps, all inside `--data-dir` (made 0700, refused if it's a symbolic
link or another user's; every file 0600, umask 077 on Unix):

| path | what |
|---|---|
| `helper.log` | connection states and error kinds; moved to `helper.log.1` at startup once past 4 MB |
| `ids.json` | the chat and person ids handed to tuimeta, by network-side id |
| `<network>/` | the login session (cookies; for Messenger, the encrypted chats' keys and messages; for WhatsApp, the linked device's keys and the chats and messages it has) |
| `files/<network>/` | downloads, named by a hash and a cleaned-up file name |
| `fake/<network>/` | downloads in `--fake` mode |

`logout` deletes `<network>/`, the network's downloads and its entries in
`ids.json` (its ids are never handed out again); for WhatsApp it also
unlinks the device first.

Never in the log, an error or a panic report: message text, names, cookies,
tokens or keys. The log takes fixed words (`hlog.Str`), numbers, and the
*kind* of an error (`hlog.Kind`: its code, or its Go types), never an error's
text, which for library errors can quote a URL with a token. A panic is
logged as its type and the function and line it came from, never its value.
Errors sent to tuimeta are always the helper's own sentences
(`*proto.Error`); any other error becomes `internal`. stdout carries protocol
lines only, and stderr is pointed at `/dev/null` once the helper is running
(on Windows, tuimeta starts it with stderr discarded), so even a crash in a
library's goroutine prints nothing over the terminal.

### Instagram

Instagram's direct messages go through `instameow` as the mautrix-instagram
bridge's connector drives it, with no bridge running. Login is by cookies
only: `sessionid`, `ds_user_id` and `csrftoken`, and of the rest only `mid`,
`ig_did`, `rur`, `shbid` and `shbts`; passwords, the app's login and captcha
pages are never used. The cookies (as Instagram rotates them) are saved in
`instagram/session.json` only once the socket has connected, and deleted when
Instagram ends the session. `logout` deletes them here; Instagram has no
logout this client uses, so end the session in the app's Login activity too
if you want it gone there.

What goes to Instagram without tuimeta asking, and why it's kept:

- At connect (and when Instagram asks for a fresh start, and every 20 hours)
  the inbox page and the inbox query, as the web client loads them. They only
  read.
- A websocket (DGW "lightspeed") subscribed to the message sync, with DGW's
  pings and frame acknowledgements. Those say an update reached this client,
  not that anyone read it.
- A second websocket (DGW "streamcontroller") subscribed to typing
  indicators, to receive other people's typing; it says nothing about you.
  The third, "mqttbypass", opens only when tuimeta sends `typing`.
- Fetching history (`IGDThreadDetailQuery`, `IGDMessageListOffMsysQuery`)
  and new threads only reads: Instagram marks a thread seen through
  `useIGDMarkThreadAsReadMutation` and its validation query, which only
  `mark_read` sends.
- Answering a message request accepts it first
  (`useIGDirectAcceptMessageRequestMutation`), as Instagram requires.

instameow itself sends no presence, "active now", foreground or app state,
and registers no push; it marks nothing read and sends no typing on its own
(its `MarkRead` and `SetTyping` are called only from `mark_read` and
`typing`, `RegisterPushNotifications` never). Instagram decides "Active now"
on its servers from any use of an account, which may include this
connection: to be sure nobody sees you active, turn off Show activity status
in the app (Settings → Messages and story replies).

Media are downloaded only on `download`, over https from Meta's CDNs only
(`fbcdn.net`, `cdninstagram.com`, `fbsbx.com`, every redirect checked), with
no cookies; an expired address is looked up again in its message once.
View-once and vanish-mode photos and videos have no file and no thumbnail.
Shared posts, reels, stories and links become link previews from Instagram's
own card, never fetched; the card's address is added to the text, since
tuimeta shows a card only for a link the text holds. Incoming formatting
(`*bold*` and the like) is read with `internal/metatext`; outgoing text goes
as typed. instameow logs message contents and URLs at debug level, so it gets
a logger that writes nothing (and mautrix-meta's global logger is silenced);
no proxy is taken from the environment.

### Messenger

Messenger goes through `messagix`, as facebook.com's web client (messenger.com
closed in April 2026), and its end-to-end encrypted chats through `whatsmeow`,
both driven as mautrix-meta's connector drives them, with no bridge running.
Login is by cookies only: `c_user`, `xs` and `datr`, plus whatever else was
pasted (`fr`, `sb`…) except `presence`, the web page's own record of its chat
sidebar. Passwords and Facebook's login pages are never used. The cookies are
saved in `messenger/session.json` only once the socket has synced, and again
on quit as Facebook last updated them.

On the first login the helper registers itself as a new encrypted-chat
device of the account, as Messenger's website does. That device's
whatsmeow store (identity and Signal session keys, sender keys, prekeys) is
`messenger/e2ee.db`, a SQLite file created 0600 (its `-wal` and `-shm` files
get the same mode), deleted rows overwritten. The encrypted messages
received or sent since the device was linked are kept there too, the newest
3000 per chat: older encrypted history is only in Messenger's PIN-locked
backup, which neither library can restore, so that's all those chats have. Someone with a copy of `e2ee.db` can read those
messages and, until the device is removed from the account, decrypt what's
sent to it and send as it; with `session.json` too, they have the web
session. `logout` is local: it disconnects, deletes the device from the
store and wipes `messenger/`, but never calls facebook.com's own log-out, so
the web session the cookies belong to stays valid (the browser tab that holds
them included) until the user ends it themselves. tuimeta's encrypted-chat
device is also never removed from the account server-side (Facebook offers
web clients no call for it); remove it in Messenger's settings if you want it
gone there.

What goes to Facebook without tuimeta asking, and why it's kept:

- At connect (and when the encrypted chats' server asks for a fresh token,
  at most every ten minutes) the facebook.com/messages page, then its
  socket (DGW "lightspeed") with the sync of the mailbox and contacts and the
  first page of threads, as the web client loads them, and a new token for
  the encrypted chats whenever theirs runs out. They only read.
- The socket's pings every 10 seconds and its frame acknowledgements: they
  say an update reached this client, not that anyone read it.
- Lookups that only read: the details of someone who wrote but isn't known
  yet (`GetContactsFullTask`), of a thread seen only by its key
  (`CreateThreadTask` without content), of an encrypted group
  (`GetGroupInfo`), older threads and messages as `load_chats` and `history`
  page back, and a message again when a file's address has expired.
- The encrypted chats' socket, connected as an active (not passive) device so
  messages are delivered to it, with its keepalive pings and prekey uploads
  when the server runs low.
- Encrypted delivery acknowledgements, which the protocol needs: an ack of
  each message and a delivery receipt of the "inactive" kind, which the
  official apps don't show; "sender" receipts to your own devices for what you
  sent from them; and a retry request to the sender of a message that
  couldn't be decrypted.

What it never sends: no presence or "active now" (whatsmeow's
`SendPresence`, `SubscribePresence` and `SetForceActiveDeliveryReceipts` aren't
reachable from the backend, so delivery receipts stay "inactive"), no
foreground or app state report (`ReportAppStateTask`), no push registration.
Read receipts (`ThreadMarkReadTask`, whatsmeow's `MarkRead`) go out only from
`mark_read` and typing (`UpdatePresenceTask`, `SendChatPresence`) only from
`typing`; every task passes a check that refuses those for any other request.
Messenger's web client marks a chat read with every message it sends; this
helper doesn't, so a chat you answer stays unread on Messenger until tuimeta
marks it read. Typing in encrypted chats may not reach the other side, since
Messenger relays it only from devices marked online and this one never is.
Facebook may still count a connected web session as active: to be sure
nobody sees you active, turn off Active Status in Messenger's settings.

Media are downloaded only on `download`, over https from Meta's hosts only
(`fbcdn.net`, `facebook.com`, `fbsbx.com`, `messenger.com`,
`cdninstagram.com`, `whatsapp.net`, every redirect checked), with no cookies;
an expired address is looked up again in its message once, and videos sent
to Messenger's video host come a megabyte at a time, as the web client
fetches them. Encrypted media are downloaded and decrypted by whatsmeow.
View-once photos and videos have no file and no thumbnail. Link previews
come from Messenger's own cards, never fetched; a message that is only a
link gets the link as its text, since tuimeta shows a card only for a link
the text holds. Incoming formatting (`*bold*` and the like) is read with
`internal/metatext`; outgoing text goes as typed. messagix, whatsmeow and
whatsmeow's store log message contents and URLs at debug level, so they get
loggers that write nothing (and zerolog's global logger is silenced); no
HTTP client takes a proxy from the environment.

### WhatsApp

WhatsApp goes through `whatsmeow`, as mautrix-whatsapp's connector drives
it, with no bridge running. There are no cookies: `login_link` links
tuimeta as a new device of the account, the way WhatsApp Web does, by a QR
code the phone scans or by a pairing code typed on the phone for the
account's number (Settings → Linked devices → Link a device). The phone
lists it as WhatsApp Web in Chrome on this computer's system ("Chrome (Mac
OS)"); only that list says Chrome: the connection itself is whatsmeow's
WhatsApp Web, with its version and no browser user agent, as for every
whatsmeow client. A link that wants a passkey is refused: tuimeta can't do
passkeys. Neither the codes nor the number are logged. A link cancelled
before the phone confirms it is refused at that confirmation (whatsmeow's
pre-pairing check), so it never adds a device; once the phone has confirmed
it, it completes.

Everything is kept in `whatsapp/wa.db`, a SQLite file created 0600 (its
`-wal` and `-shm` files get the same mode), deleted rows overwritten in the
database file (the write-ahead log is reused rather than scrubbed, so a
deleted row can linger in `wa.db-wal` until it's written over):
whatsmeow's device (identity and Signal session keys, sender keys, prekeys,
app state, the contact names and phone-number ↔ WhatsApp-id pairs the phone
shares) and, since WhatsApp keeps no history on its servers, the chats and
messages this device has: what the phone sent when it was linked, what came
since, and older messages asked of the phone, the newest 3000 per chat.
Messages are kept as read out of WhatsApp's protocol (text, who sent what
when, replies, reactions, and for attachments what's needed to download and
decrypt them), never the files themselves, which are downloaded only on
`download`. A view-once photo, video or voice message keeps nothing that
could fetch it. whatsmeow also holds each decrypted message in the store
for the moment between decrypting it and its being kept here (so one that
arrives as the helper quits isn't lost: it's acknowledged only once kept,
and comes again if keeping it failed), view-once ones included, then
deletes it. Disappearing messages are deleted here, from the store and
from tuimeta, when their time is up, as on the phone; what's sent in a chat
with a timer disappears too. A chat or message deleted on the phone (for you
or for everyone) is deleted here, with what was downloaded of it; in a
group, someone else's deletion of a message counts only from one of its
admins, which WhatsApp's servers can't check since it comes encrypted. A
message someone sent to a broadcast list you're on is in your chat with
them, as on the phone. After deletions, and when the helper stops, the
write-ahead log is emptied into the scrubbed database file; when the store
opens, the message keys whatsmeow kept for messages that aren't kept here,
and any temporary download a stop cut short, are deleted. The folder isn't
kept out of backups: Time Machine and the like copy `wa.db` (and Messenger's
`e2ee.db`) as they copy everything else. Someone with a copy of `wa.db` can read
those messages and, until the device is unlinked, act as it. `logout`
unlinks the device (as "Log out" in the phone's Linked devices does; the
phone and other linked devices stay logged in) and wipes `whatsapp/`; if
WhatsApp can't be reached, it says to remove the device on the phone. A
device the phone unlinked (or WhatsApp dropped after weeks unused) can't
reconnect: the account turns `error` until it's linked again, which starts
afresh. Linking while a device that still works is kept (say, reconnecting)
is refused: log out first.

What goes to WhatsApp without tuimeta asking, and why it's kept:

- The linking itself: the QR codes' websocket, the pairing-code request for
  a number, and once linked, whatsmeow's "unified session" note that WhatsApp
  Web also sends.
- After taking in each history blob the phone sent, its deletion from
  WhatsApp's media servers, as WhatsApp Web does.
- The connection (`web.whatsapp.com`'s websocket) as an active (not
  passive) device, so messages are delivered to it, with its keepalive pings
  and prekey uploads when the server runs low.
- Acknowledgements the protocol needs: an ack of each message, sent after
  it's stored, and a delivery receipt of the "inactive" kind, which tells the
  sender a device got it but not that anyone saw it; "sender" receipts to
  your own devices for what you sent from them; receipts for the history
  sync blobs the phone sends; a retry request to the sender (and a request
  to your phone) for a message that couldn't be decrypted.
- Reads that tell nobody anything: the history sync blobs and the app state
  (your contact names, which chats are muted, archived or pinned), groups'
  details when a message comes from a group not known yet or a group's
  announcement setting changes, your privacy settings (once, before the
  first read receipt), and on `download` the media servers and, for
  pictures, where a profile picture is. On `search`, for a query written as
  a phone number with `+` and its country code, whether that number is on
  WhatsApp: only once the number has stayed as typed for 1.5 seconds, once
  a run per number, and at most 10 numbers in 10 minutes, since WhatsApp
  counts lookups of numbers.
- What WhatsApp's apps send along to work, which tells nobody anything about
  you: the privacy tokens of the people you message (asked once a week each,
  and again when someone's security code changes), carried with what you
  send them; requests for the app-state keys to your own devices; acks of
  call notices.
- Asking the phone for older messages (a peer message to your own phone, as
  WhatsApp Web's "Get older messages" does) when a `history` page comes up
  short of what's kept here, opening a chat with few messages kept
  included; not again for two minutes after the phone didn't answer.
- With each message you send, WhatsApp's reporting token, as the official
  apps send.

What it never sends: no presence ("online", "last seen", "unavailable"):
whatsmeow's `SendPresence`, `SubscribePresence`,
`SetForceActiveDeliveryReceipts` and `SetPassive` aren't reachable from the
backend, so delivery receipts stay "inactive" and your last seen isn't moved
by tuimeta. Read receipts (`MarkRead`) go out only from `mark_read` and
typing (`SendChatPresence`) only from `typing`: the connection refuses them
for any other request. A read receipt follows WhatsApp's read-receipts
setting: with it off, the read is told to your own devices only; when the
setting can't be fetched, no receipt goes at all (whatsmeow alone would send
one everyone sees). The only app state it changes is a chat's mute, on
`mute`. Because it never says it's online, WhatsApp rarely tells it who's
typing: it sends typing to devices that are online. No push registration.

What other people send is held to bounds: a message's text to 64 KB,
pictures carried inside a message (thumbnails, link pictures) to 64 KB,
mentions to 256 (and only those whose "@" is in the text become people),
reactions to an emoji (no words, numbers or spaces). A message's id sent
again never replaces it (only the decryption of one that couldn't be
decrypted does); changes come as edits, which are marked. A group's
disappearing-messages timer is taken only from WhatsApp's own group notices,
a chat's only as one of WhatsApp's timers (24 hours, 7 days, 90 days).
Only phone numbers and WhatsApp ids become people: a status broadcast, a
channel, a bot or a group a sender names is never a chat you can open, and
tuimeta sends only to people and groups. A name someone gave WhatsApp (not
the one in your address book) is shown after a "~", as WhatsApp shows it,
and a chat with someone not in your address book is titled with their
number too, so "Mum" or "You" can't pass for the real one.

Media are downloaded only on `download`: attachments by whatsmeow from
WhatsApp's media servers (decrypted and checked against their hashes,
through a private temporary file in `whatsapp/` that's never let grow past
200 MB, whatever size the message claims, and deleted afterwards; photos and
stickers, which tuimeta draws from the file and so downloads as soon as
they're on screen, past 16 MB and 2 MB), the
thumbnails and link-preview pictures the sender's app put in the message
itself, and profile pictures over https from `whatsapp.net` hosts only
(every redirect checked), with no cookies. A file WhatsApp no longer has
says to open it on the phone (it isn't asked of the phone again). Link
previews come from the sender's app's own card, never fetched. Incoming
formatting (`*bold*`, `_italic_`, `~strike~`, `` `code` ``, ```` ```code``` ````,
`> quote`) is read with `internal/metatext`; outgoing text goes as typed.
Files to send go as photos (JPEG and PNG, with a 72-pixel JPEG thumbnail made
here), videos (MP4), audio, or documents (everything else, GIFs and WebP
pictures included). whatsmeow and its store get loggers that write nothing,
and no proxy is taken from the environment.

## Fake mode

Each network has six chats of sixty messages over the past week (dms and
groups of four and five, an encrypted Messenger dm and every WhatsApp chat
encrypted, a muted chat, an archived one, an Instagram message request,
unread counts), with replies (one to a message far older than the newest
page), reactions (some yours), edits, events ("Chloé unsent a message",
"Mum named the group Family 🏡"), an album of three photos, PDFs, a sticker,
a video with its still, voice messages (`audio/mp4`), a view-once photo, a
link preview, `[Poll]` and `[Location]` as unsupported content, formatting
(bold, italic, code, strike, pre, quote, link, mention, with emoji before
them for UTF-16 offsets), Arabic text, long messages that wrap, two messages
in the same millisecond, and people you have no chat with yet (for `search`
and `open_dm`). WhatsApp's people have phone numbers and no usernames, and
its `search` finds them by either. Pictures are PNGs drawn by
`internal/fake/draw.go`; the video and voice files are headers with nothing
playable after them.

Logging in takes any cookies with the required names (`c_user=1; xs=2;
datr=3`, or `sessionid`, `ds_user_id` and `csrftoken` for Instagram), parsed
as real ones are. WhatsApp links instead: `login_link` shows a QR code
starting `2@fake` (or, given a phone number, the pairing code `FAKE-C0DE`)
and takes it as scanned 3 seconds later; `cancel_login`, `logout` or a newer
`login_link` stop a waiting one. A message you send is accepted 300 ms later;
in a dm the other person reads it and starts typing 500 ms after that, and
answers "echo: …" 1.5 s later. Your text messages can be edited for 15
minutes, unsent any time. Message ids come from timestamps measured back from
the start of the current hour, so a run gives the same ids, names and texts
as any other run in that hour (chat and person ids, kept in `ids.json`, never
change).

## For backend authors

A network plugs in as a `backend.Backend` (`internal/backend/backend.go`),
added in `main.go` in place of `backend.Unavailable`. The server
(`internal/server`) reads requests, checks what it can, and calls the backend
from one goroutine per request, so every method must be safe for concurrent
use. Before calling, the server has resolved ids (a `ChatRef`, `MessageRef`
or `UserRef` carries the network's own id, `NetID`), refused requests to a
logged-out network, refused temporary ids, and validated params. It also
sends the pending messages of a send, answers with their temporary ids, and
reports a send that's neither confirmed nor failed after two minutes.

```go
type Backend interface {
	Network() proto.Network
	Start(ctx context.Context)  // account event at once, connect in the background
	Close()                     // quit quietly: no receipts, no presence
	LoginCookies(ctx context.Context, c cookies.Set, as browser.Identity) error
	Logout(ctx context.Context) error
	LoadChats(ctx context.Context, limit int) (hasMore bool, err error)
	History(ctx context.Context, chat ChatRef, q history.Query) (history.Page, error)
	GetMessage(ctx context.Context, msg MessageRef) (proto.Message, error)
	Send(ctx context.Context, out *Outgoing) error
	EditText(ctx context.Context, msg MessageRef, text string) error
	Delete(ctx context.Context, msg MessageRef) error
	React(ctx context.Context, msg MessageRef, emoji string) error // "" removes
	MarkRead(ctx context.Context, msg MessageRef) error
	SetTyping(ctx context.Context, chat ChatRef, typing bool) error
	Mute(ctx context.Context, chat ChatRef, muted bool) error
	Search(ctx context.Context, query string) ([]proto.SearchResult, error)
	OpenDM(ctx context.Context, user UserRef) (int64, error)
	Fetch(ctx context.Context, file ids.FileRef, w io.Writer) error
}
```

A network that logs in by linking a device (WhatsApp) also implements
`backend.Linker` (`Link(ctx, phone)`, which reports codes with
`Events.LoginCode` and returns once the account is ready, and
`CancelLink()`); the server sends it `login_link` and `cancel_login`, and
refuses `login_cookies` for it. `backend.PhoneDigits` reads a phone number in
international form.

A backend is given `backend.Deps`:

- **`Events`** writes events. `Account` reports the account's state (send it
  from `Start`, then on every change; `error` with a sentence saying what to
  do). `Chat`, `User`, `Message` (each album part), `MessageDeleted`, `Read`,
  `Typing`, `File`, `Error`. Lines go out in the order called, so call them
  under the backend's own lock wherever an older state could overtake a newer
  one. `Typing` says a start once and sends the stop itself 6 s after the last
  start if the network doesn't; a `Message` from someone typing ends their
  typing first. `LoadChats` sends its `chat` and `user` events itself, for the
  same reason.
- **`IDs`** (`ids.Store`): `Chat(network, netID)` and `User(network, netID)`
  give stable ids, kept in `ids.json` before any line carrying a new one is
  written. `RenameChat` and `RenameUser` move an id to a new network-side id
  (WhatsApp's chats by phone number becoming chats by WhatsApp id), so
  tuimeta keeps knowing them by the same number.
- **`Messages`** (`ids.Messages`): `Assign(chat, netID, ms, parts)` gives a
  network message `ms << 8 | slot` ids, consecutive for its parts, the same
  ids every time it's seen in the run; `Lookup` goes back from any part.
  `proto.SplitAlbum(base, media, ids)` makes the parts: `reply_to`,
  `forwarded` and `reactions` on the first, `text`, `entities`,
  `link_preview`, `editable_until` (and `unsupported`) on the last, the rest
  on all. `proto.UTF16Range` and `proto.EntityFor` turn byte ranges into
  entity offsets.
- **`Files`** (`ids.Files`): `Register(FileRef{Network, Key, Size, Mime, Name,
  Source})` gives anything downloadable a file id. `Key` must name the
  *content* and stay the same across runs (an attachment's id, not a signed
  URL): downloads are saved under it and reused next run. `Source` is the
  backend's own (a URL with its expiry, a WhatsApp media key) and is updated
  by registering the same key again. The download manager calls `Fetch` (one
  download per file at a time, four at once, `high` before `low`), counts the
  bytes, stops at 200 MB, reports progress, and saves the file 0600; call
  `download.SetSize(w, n)` once the size is known (a Content-Length).
- **`Outbox`**: `Send` gets an `*Outgoing` with the text or caption, the
  files (`Upload`s, already read), the reply and the temporary ids. Call
  `out.Sent(parts...)` once the network accepts it (temporary ids are paired
  with parts in order: `message_sent`; leftovers become `message_deleted`)
  or return an error. Messenger confirms a send either in the send call's
  answer or on the socket, whichever comes first: `out.SetKey(otid)` before
  sending, and `Outbox.Find(network, otid)` when a message arrives, so the
  first confirmation is the one that counts and the other is an ordinary
  `message` update.
- **`Session`** (`session.Store`): files under `<data-dir>/<network>/`,
  written atomically, 0600, synced. Save the cookies (`set.Values()`) only
  after login worked; `Path(name)` gives a library a file of its own (the
  encrypted chats' database); `Wipe()` on logout.

Other helpers:

- `cookies.Parse(network, input)` reads a `Cookie:` header, a JSON array of
  `{name, value, domain…}` (other sites' cookies dropped) or a JSON object,
  and checks the required names. The server calls it before `LoginCookies`.
  A `cookies.Set` prints as `[3 cookies]` whatever the verb, and marshals the
  same way; `Values()` and `Header()` hand the values out explicitly.
- `history.Log` keeps a chat's messages in id order and pages them as
  `history` asks (`before`/`after`/`around` are positions in time, never
  lookups; albums aren't cut). Keep its newest end current, add older pages
  as they're fetched, and `SetComplete(true)` at the chat's first message.
- `hlog.Go(name, f)` for every goroutine a backend starts: a panic is logged
  as its type and place and goes no further. Library loggers (zerolog in
  mautrix-meta) must not reach `helper.log` as they are: at debug and trace
  levels they log message contents and URLs.
- Errors: return `proto.Err(code, sentence)` for anything the user should
  read (`bad_cookies`, `checkpoint`, `network`, `unsupported`, …); everything
  else becomes `internal` and only its kind is logged.

Package layout:

| package | what |
|---|---|
| `main` | flags, the data folder, signals, stderr, wiring |
| `internal/proto` | the wire objects, events, error codes, album splitting, UTF-16 |
| `internal/server` | reading, dispatch, the single writer, request handlers, reading uploads |
| `internal/backend` | `Backend`, `Deps`, `Events` (with the typing timeout), `Outgoing`/`Outbox`, `Unavailable` |
| `internal/ids` | chat/person ids (kept), message ids and file ids (per run) |
| `internal/history` | a chat's messages and paging |
| `internal/cookies`, `internal/session`, `internal/download` | logins, sessions, downloads |
| `internal/browser` | the browser the helper says it is: the libraries' own, or the Chrome a login named, put in place of the libraries' on every request |
| `internal/hlog`, `internal/fsutil` | the log, private folders and atomic files |
| `internal/messenger`, `internal/instagram`, `internal/whatsapp` | the networks |
| `internal/fake` | the `--fake` networks |
| `internal/wiretest` | a protocol client for tests |
