# tuimeta-helper

The part of [tuimeta](..) that speaks Messenger and Instagram. Meta's
protocols are only implemented well in Go, in
[mautrix-meta](https://github.com/mautrix/meta) (`messagix` for Messenger,
`whatsmeow` for its end-to-end encrypted chats, `instameow` for Instagram),
so tuimeta runs this program as a child process and talks to it in
newline-delimited JSON over stdin and stdout. The contract is
[PROTOCOL.md](PROTOCOL.md): a "tiny TDLib for Meta" that hides each network's
ids and protocols behind one small model.

```
tuimeta-helper --data-dir <absolute dir> [--fake]
```

`--fake` runs two made-up accounts with no network access at all, for
tuimeta's tests and for trying the app without an account. Without it, the
helper speaks the real networks: see Privacy → Instagram and Privacy →
Messenger for what each sends.

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
uses `modernc.org/sqlite`, a SQLite written in Go, so no C compiler is needed.

## License

The helper is under the GNU Affero General Public License, version 3 or
later ([LICENSE](LICENSE)), because the mautrix-meta code it will be built on
is. Every Go file starts with `// SPDX-License-Identifier: AGPL-3.0-or-later`.
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
| `<network>/` | the login session (cookies; for Messenger, the encrypted chats' keys and messages) |
| `files/<network>/` | downloads, named by a hash and a cleaned-up file name |
| `fake/<network>/` | downloads in `--fake` mode |

`logout` deletes `<network>/`, the network's downloads and its entries in
`ids.json` (its ids are never handed out again).

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

## Fake mode

Each network has six chats of sixty messages over the past week (dms and
groups of five, an encrypted Messenger dm, a muted chat, an archived one, an
Instagram message request, unread counts), with replies (one to a message far
older than the newest page), reactions (some yours), edits, events ("Chloé
unsent a message"), an album of three photos, a PDF, a sticker, a video with
its still, voice messages (`audio/mp4`), a view-once photo, a link preview,
`[Poll]` as unsupported content, formatting (bold, italic, code, strike, pre,
quote, link, mention, with emoji before them for UTF-16 offsets), long
messages that wrap, two messages in the same millisecond, and people you have
no chat with yet (for `search` and `open_dm`). Pictures are PNGs drawn by
`internal/fake/draw.go`; the video and voice files are headers with nothing
playable after them.

Logging in takes any cookies with the required names (`c_user=1; xs=2;
datr=3`, or `sessionid`, `ds_user_id` and `csrftoken` for Instagram), parsed
as real ones are. A message you send is accepted 300 ms later; in a dm the
other person reads it and starts typing 500 ms after that, and answers
"echo: …" 1.5 s later. Your text messages can be edited for 15 minutes,
unsent any time. Message ids come from timestamps measured back from the start
of the current hour, so a run gives the same ids, names and texts as any other
run in that hour (chat and person ids, kept in `ids.json`, never change).

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
  written.
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
| `internal/fake` | the `--fake` networks |
| `internal/wiretest` | a protocol client for tests |
