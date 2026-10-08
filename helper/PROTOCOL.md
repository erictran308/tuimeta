# tuimeta helper protocol, version 3

`tuimeta-helper` is a separate program (Go, AGPL-3.0-or-later) that speaks
Messenger, Instagram and WhatsApp for tuimeta (Rust, MIT). tuimeta starts it as a child
process and talks to it over stdin/stdout. The helper is "a tiny TDLib for
Meta": it hides each network's ids and protocols behind one small model, so
tuimeta's UI code works the way it did with TDLib.

```
tuimeta-helper --data-dir <dir> [--fake]
```

- `--data-dir` is a folder only the helper uses (tuimeta passes
  `<tuimeta data dir>/helper`). The helper creates it 0700 if missing and keeps
  every file it writes inside it, 0600.
- `--fake` runs made-up accounts with no network access at all (see below).
- No other arguments, no environment variables are read except `HOME`/`TMPDIR`
  as the Go runtime needs. Nothing is read from the working directory.

## Framing

One JSON object per line, UTF-8, in both directions. Strings never contain a
raw newline (JSON escapes them).

```
→ {"id": 7, "method": "send_text", "params": {"chat_id": 12, "text": "hi"}}
← {"id": 7, "result": {"message_id": 1759912345678000}}
← {"id": 8, "error": {"code": "not_found", "message": "No such chat."}}
← {"event": "message", "message": {...}}
```

- `id` is a u64 that tuimeta picks, unique for the helper's run.
- Every request gets exactly one response, `result` (an object, `{}` when
  there's nothing to say) or `error`. Requests may be handled concurrently, so
  responses can arrive in any order, and events a request causes can arrive
  before its response.
- An unknown method answers `{"code": "unknown_method"}`. Unknown fields in
  params are ignored. Fields marked `?` may be absent or `null`.
- Error `code`s: `bad_request`, `unknown_method`, `not_found`,
  `not_logged_in`, `bad_cookies`, `checkpoint`, `network`, `unsupported`,
  `timeout`, `cancelled`, `internal`. `message` is one sentence a person can act on; tuimeta may show
  it in its status bar.
- The helper's first line is always:
  `{"event":"hello","version":3,"helper":"<semver>","networks":["messenger","instagram","whatsapp"]}`.
  tuimeta refuses to go on if `version` isn't one it knows. (Version 2 added
  `login_cookies`' `browser`: a helper that ignored it would log in as
  another browser than the user asked for. Version 3 added WhatsApp, which
  logs in by linking a device: `login_link`, `cancel_login` and the
  `login_code` event.)
- When stdin closes (tuimeta quit or crashed), the helper disconnects every
  account, without marking anything read or setting any presence, and exits
  within two seconds.
- Nothing but protocol lines goes to stdout. The helper logs to
  `<data-dir>/helper.log` (connection states and error kinds only: never
  message text, names, cookies, tokens or keys). stderr stays quiet.

## Ids

All ids are numbers the helper assigns, so tuimeta never sees a network's own
ids.

- `network`: `"messenger"`, `"instagram"` or `"whatsapp"`. One account per
  network.
- `chat_id` (i64 > 0): unique across all networks, stable across runs
  (the helper persists the mapping).
- `user_id` (i64 > 0): people, including yourself; unique across networks,
  stable across runs. On WhatsApp a person known by a phone number and
  later by their WhatsApp id (or the other way round) keeps one `user_id`,
  and so does the chat with them.
- `message_id` (i64 > 0): unique within its chat and **chronological**: a
  message sent later has a larger id. The helper derives it from the server
  timestamp in milliseconds, `ms << 8 | slot`, using the next free slot when
  two messages share a millisecond, and remembers the mapping for the run.
  - A message with several attachments (photos sent together) is reported as
    that many messages with consecutive ids, the same `album` (the first
    part's id), the media one per part, and the text on the last part. A
    request naming any part acts on the whole message on the network.
    `reply_to`, `forwarded` and `reactions` go on the first part only (the
    others have none), `text`, `entities`, `link_preview` and
    `editable_until` on the last; `date`, `edited`, `deletable` and `state`
    on every part.
  - A message you're sending gets a temporary id until the network confirms
    it; `message_sent` then gives the final one.
- `file_id` (i32 > 0): anything downloadable (a photo, a video, a thumbnail, a
  profile picture, a file). Stable for the run.

## Objects

### Chat

```json
{
  "id": 12, "network": "messenger",
  "kind": "dm",                 // "dm" | "group"
  "title": "Alice Example",
  "user_id": 34,                // ? the other person, in a dm
  "photo": {"file_id": 5, "width": 160, "height": 160},   // ?
  "order": 1759912345678,       // ordering key: the last activity in ms
  "unread": 2,                  // unread messages (0 if read)
  "muted": false,
  "archived": false,
  "encrypted": true,            // end-to-end encrypted: every WhatsApp chat, Messenger's encrypted ones
  "request": false,             // a message request you haven't accepted
  "can_send": true,
  "read_inbox": 1759912000000000,   // you've read up to this message id
  "read_outbox": 1759911000000000,  // the other side read yours up to this id
  "last_message": {...}         // ? Message: the newest one, for the preview
}
```

### User

```json
{"id": 34, "network": "messenger", "name": "Alice Example",
 "username": "alice.example",   // ?
 "photo": {"file_id": 6, "width": 160, "height": 160},   // ?
 "is_self": false,
 "active_at": 1759912345}       // ? unix seconds when last seen active, if the network says
```

### Message

```json
{
  "id": 1759912345678000, "chat_id": 12, "sender_id": 34,
  "outgoing": false,
  "date": 1759912345,           // unix seconds
  "text": "see you at 5",
  "entities": [ {"offset": 0, "length": 3, "type": "bold"} ],
  "album": 0,
  "media": null,                // ? Media
  "reply_to": {"message_id": 1759900000000000, "sender_id": 12, "text": "when?"},  // ?
  "forwarded": false,
  "reactions": [ {"emoji": "❤️", "count": 2, "mine": true} ],
  "edited": false,
  "editable_until": 1759913245, // ? unix seconds; set only on your own text messages
  "deletable": true,            // you can unsend it
  "state": "sent",              // "sent" | "pending" | "failed"
  "service": null,              // ? a sentence when this is an event ("Alice named the group Trip")
  "link_preview": null,         // ? LinkPreview
  "unsupported": null           // ? a label for content not shown, e.g. "[Poll]"
}
```

- `entities` offsets and lengths are UTF-16 code units of `text`, as in TDLib.
  `type`: `bold`, `italic`, `strike`, `code`, `pre`, `quote`, `link` (with
  `url`), `mention` (with `user_id`).
- `reply_to.message_id` is absent when the answered message isn't known;
  `text` is a one-line snippet of it when the network gives one.
- `service`, when set, means the message is drawn as an event in the middle
  of the chat, not as a bubble; `text` is then empty.

### Media

```json
{
  "kind": "photo",              // photo | video | gif | sticker | audio | voice | file
  "file_id": 21,                // the full file
  "thumbnail": {"file_id": 22, "width": 320, "height": 240},   // ? a still to draw
  "width": 1280, "height": 960, // ? 0 or absent when unknown
  "duration": 0,                // ? seconds
  "name": "IMG_0001.jpg",       // ?
  "mime": "image/jpeg",         // ?
  "size": 123456,               // ? bytes
  "view_once": false            // shown only once on the phone; never offered as a file
}
```

For a photo, `thumbnail` may be the same file as `file_id`. A view-once photo
or video has no `file_id` (0) and no `thumbnail`.

### LinkPreview

```json
{"url": "https://example.com/post", "title": "A post", "description": "…",
 "image": {"file_id": 30, "width": 600, "height": 315}}   // image ?
```

Only from the network's own data: the helper never fetches a URL found in a
message.

### File

```json
{"id": 21, "size": 123456, "downloaded": 4096, "done": false,
 "path": null,          // absolute path once done
 "error": null}         // ? set if the download failed for good
```

## Events

| event | fields | when |
|---|---|---|
| `hello` | see Framing | first line |
| `account` | `network`, `state`, `user_id?`, `name?`, `error?` | at startup for each network, then on every change. `state`: `logged_out`, `connecting`, `ready`, `error`. `error` says what to do, e.g. "Facebook wants you to confirm it's you: open facebook.com in a browser, then log in here again." |
| `chat` | `chat` | a chat is new or changed (full object, replaces the old one) |
| `chat_removed` | `chat_id` | a chat left the list (deleted, left) |
| `user` | `user` | a person is new or changed |
| `message` | `message` | a new message, or a known one changed (full object, replaces it: edits, reactions, state) |
| `message_sent` | `chat_id`, `old_id`, `message` | your message was accepted; `old_id` was its temporary id |
| `message_failed` | `chat_id`, `old_id`, `error` | your message could not be sent |
| `message_deleted` | `chat_id`, `message_ids` | unsent or deleted |
| `read` | `chat_id`, `inbox?`, `outbox?`, `unread?` | you read up to `inbox` elsewhere; the other side read yours up to `outbox` |
| `typing` | `chat_id`, `user_id`, `typing` | someone started or stopped typing. The helper sends `typing: false` itself 6 s after the last start if the network doesn't. |
| `file` | `file` | download progress, and once done |
| `error` | `network?`, `message` | something the user should see that no request caused |
| `login_code` | `network`, `attempt?`, `qr?`, `pairing?`, `expires` | while `login_link` waits: the code to show, which replaces the one before. `attempt` is the `login_link`'s, so a code of a link given up is known for one. `qr` is the text to draw as a QR code for the phone to scan; `pairing` is the code to type on the phone (8 letters and digits, written `ABCD-EFGH`). `expires` is when it stops working, in unix seconds. |

## Requests

| method | params | result |
|---|---|---|
| `login_cookies` | `network`, `cookies`, `browser?` | `{}` once logged in and connected. `cookies` is what the user pasted: a `Cookie:` header value (`c_user=…; xs=…`) or the JSON browser extensions export (an array of `{name, value}` or an object). Required: Messenger `c_user`, `xs`, `datr`; Instagram `sessionid`, `ds_user_id`, `csrftoken`. `browser` is the browser the cookies came from, Chrome and its full version (`"Chrome 150.0.7712.45"`; "Google Chrome" and chrome://version's "(Official Build)" notes are accepted too): the helper then says it's that Chrome on this computer's system (user agent and client hints, see below). Absent, it says what the libraries say (Chrome 141 on Linux). Saved with the session, so the session never changes browser; logging in again is the only way to change it. Errors: `bad_request` (`browser` isn't Chrome with a full version; WhatsApp, which logs in with `login_link`), `bad_cookies`, `checkpoint`, `network`. |
| `login_link` | `network`, `phone?`, `attempt?` | `{}` once linked and connected (account `ready`): WhatsApp, which logs in by linking tuimeta as a new device of the account (on the phone: Settings → Linked devices → Link a device). Without `phone`, `login_code` events with `qr` follow: the first at once and good for a minute, then a fresh one about every 20 seconds, for about 2½ minutes in all. With `phone` (the account's number in international form: digits, with an optional leading `+`, spaces and dashes), one `login_code` with `pairing` follows, to type on the phone under "Link with phone number instead". `attempt` is a number tuimeta picks for this login, given back in its `login_code` events and named by `cancel_login`. A newer `login_link`, `cancel_login` or `logout` for the network ends a waiting one, until the phone has confirmed the link: from then on it completes (only `logout` ends it), rather than leaving a device in the phone's list that nothing here can use. Errors: `bad_request` (not a linking network, `phone` isn't a number, or a device that still works is linked: log out first), `timeout` (nothing was scanned or typed in time), `cancelled`, `network`, `unsupported` (WhatsApp asked for something tuimeta can't do, like a passkey). |
| `cancel_login` | `network`, `attempt?` | `{}`: ends the waiting `login_link` of that attempt (any, without one), so no code shown for it works any more; it answers `cancelled`. A link of another attempt, or one the phone has already confirmed, goes on. |
| `logout` | `network` | `{}`. Disconnects and deletes everything stored for that network (for Messenger, its encrypted-chat device store too). For Messenger and Instagram it's local: it never ends the session on Meta's side, and the web session the cookies belong to stays valid until the user ends it themselves (in the browser, or in the site's list of logged-in devices). For WhatsApp, where tuimeta is a linked device of its own, it also unlinks that device, as "Log out" in the phone's list of linked devices does; the phone and any other linked devices stay logged in. |
| `load_chats` | `network?`, `limit` | `{"has_more": bool}`; sends `chat` (and `user`) events for up to `limit` more chats, newest activity first, beyond those already sent. tuimeta calls it again until `has_more` is false. |
| `history` | `chat_id`, `before?`, `after?`, `around?`, `limit` | `{"messages": [Message], "has_more": bool}`, oldest first. With no `before`/`after`/`around`: the newest. They are positions in time, not lookups: any id works, a message's or not (`ms << 8` order), so `before` gives messages with smaller ids, `after` larger ones, and `around` about `limit/2` on each side of it (including one with that id). WhatsApp keeps no history on its servers: a chat has what this device received since it was linked and what the phone sent it then, and older messages are asked of the phone, which must be online (a page may take up to 20 s; without an answer, what's here is the answer). |
| `get_message` | `chat_id`, `message_id` | `{"message": Message}` (e.g. one a reply answers that isn't loaded) |
| `send_text` | `chat_id`, `text`, `reply_to?` | `{"message_id": i64}` (temporary). The text goes as typed: all three networks read `*bold*`-style formatting themselves. |
| `send_files` | `chat_id`, `paths`, `caption?`, `reply_to?` | `{"message_ids": [i64]}` (temporary). Paths are absolute; the helper reads them once, now. |
| `edit_text` | `chat_id`, `message_id`, `text` | `{}` |
| `delete` | `chat_id`, `message_id` | `{}`: unsends your message for everyone. |
| `react` | `chat_id`, `message_id`, `emoji?` | `{}`: sets your one reaction; `null` removes it. |
| `mark_read` | `chat_id`, `message_id` | `{}`: marks the chat read up to that message. |
| `typing` | `chat_id`, `typing` | `{}` |
| `download` | `file_id`, `priority` (`high`/`low`) | `{}`, then `file` events. |
| `search` | `network`, `query` | `{"results": [{"chat_id?", "user_id?", "title", "username?", "kind"}]}`: people and groups to start or open a chat with. On WhatsApp: the contacts and groups this device knows, and, for a query that is a phone number written with `+` and its country code, that number if it's on WhatsApp. |
| `open_dm` | `network`, `user_id` | `{"chat_id": i64}`: the chat with that person, made if needed. |
| `mute` | `chat_id`, `muted` | `{}` |

`send_text` and `send_files` first send a `message` event for each new
message, with `"state": "pending"`, its temporary id, `outgoing: true`, your
own `sender_id`, `date` now, the text or caption, and for files a `media`
with its kind, name, size and mime (and `file_id` 0 unless the helper can
serve the local copy). Then they answer with those temporary ids, and
`message_sent` or `message_failed` follows with `old_id`.

`download` of a file already on disk still sends a `file` event with
`done: true` and its `path` at once.

### The browser the helper says it is

With `browser`, every request that would carry the libraries' own browser
(the website's requests, its websockets, the encrypted chats' connection and
file downloads) carries the named Chrome instead: its user agent for this
computer's system (`Macintosh; Intel Mac OS X 10_15_7`, `Windows NT 10.0;
Win64; x64` or `X11; Linux x86_64`, as Chrome writes them), and the client
hints a request already had: `sec-ch-ua` and `sec-ch-ua-full-version-list`
(the brands and order Chrome derives from its version), `sec-ch-ua-platform`
(`macOS`, `Windows`, `Linux`) and `sec-ch-ua-platform-version` (macOS's
version, or Linux's kernel release, as three numbers; empty on Windows).
Requests that pretend to be a phone app are left alone. Nothing else changes:
the connections still shake hands the way the libraries' Chrome does, which
is why only Chrome can be named.

## What the helper must never do on its own

These mirror tuimeta's rules: what other people see about you goes out only
when tuimeta asks.

- Read receipts only on `mark_read`; typing only on `typing`. No presence or
  "active now" status at any time: whatever the libraries would send by
  themselves (marking threads read on sync, foreground/active pings, presence
  subscriptions that announce you) is switched off. A delivery acknowledgement
  the protocol needs in order to work is allowed, and documented in the
  helper's README.
- No downloads except on `download`. The one exception is what WhatsApp's
  own syncing needs: the history blobs the phone sends when a device is
  linked and the encrypted app-state patches, which whatsmeow fetches from
  WhatsApp's servers to work (the helper's README lists them).
- No URL from a message is ever fetched.
- No message text, names, cookies, tokens or keys in logs, errors or panics.

## `--fake` mode

For tuimeta's tests and for trying the app without an account. No network
access at all.

- Every network starts `logged_out`. `login_cookies` succeeds for any
  string that contains the required cookie names (e.g. `c_user=1; xs=2;
  datr=3`), and fails with `bad_cookies` otherwise, or with `bad_request` if
  `browser` is there and isn't Chrome with a full version. WhatsApp's
  `login_link` sends a `login_code` at once (`qr` starting `2@fake`, or with
  `phone`, `pairing` `FAKE-C0DE`) and links 3 s later; a `phone` with fewer
  than 7 or more than 15 digits is a `bad_request`. Logged-in state lasts for
  the run.
- Each network has 6 chats (dms and groups, one Messenger chat `encrypted`
  and every WhatsApp chat, one Instagram chat with a link preview) and 60
  messages of history per chat over the last week, including replies,
  reactions, an edited message, a service message, an album of 3 photos and
  a file.
- Photos and avatars are small PNGs the helper draws and writes to
  `<data-dir>/fake/` on `download`, with a few `file` progress events.
- `send_text` / `send_files`: `message_sent` after 300 ms. In a dm, the other
  person starts `typing` 500 ms later and answers "echo: <text>" 1.5 s later.
- `mark_read`, `typing`, `react`, `edit_text`, `delete`, `mute` change the
  fake state and send the events the real network would.
- The run is deterministic apart from timing: the same ids, names and texts
  every time.
