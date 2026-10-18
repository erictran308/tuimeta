# tuimeta helper protocol, version 8

`tuimeta-helper` is a separate program (Go, AGPL-3.0-or-later) that speaks
Messenger, Instagram, WhatsApp and Threads for tuimeta (Rust, MIT). tuimeta starts it as a child
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
  `{"event":"hello","version":8,"helper":"<semver>","networks":["messenger","instagram","whatsapp","threads"]}`.
  tuimeta refuses to go on if `version` isn't one it knows. (Version 2 added
  `login_cookies`' `browser`: a helper that ignored it would log in as
  another browser than the user asked for. Version 3 added WhatsApp, which
  logs in by linking a device: `login_link`, `cancel_login` and the
  `login_code` event. Version 4 changed `send_files` to name each file with
  what tuimeta checked when it was listed: a helper that read only paths
  would upload a file swapped since. Version 5 added Instagram's home feed,
  `load_feed` and its posts: tuimeta lists the feed only with a helper that
  has it. Version 6 added Threads, a network with a feed and replies and no
  chats, and what its posts have that Instagram's don't: `reposts`,
  `shares`, `context`, `continues`, `quoted`, `link_preview`, posts without
  media, and a comment's `attachment`. Version 7 added `load_post`, a post
  by its link, which tuimeta shows in place of opening the link in a
  browser. Version 8 added `cancel`, which calls off a load tuimeta stopped
  waiting for, so a post or comments left before they came aren't asked
  of the network if they're still waiting their turn.)
- No line the helper writes is longer than 8 MiB. An answer that would be
  is sent as an `internal` error for its `id` instead, and an event that would
  be is dropped; `history` stops a page short to fit (see `history`).
  tuimeta reads lines of up to 16 MiB, and skips a longer one rather than
  stopping: only the request it answers fails. An answer line starts with
  its `id` (`{"id":7,…`), which is how tuimeta tells which request a line it
  skips answers.
- When stdin closes (tuimeta quit or crashed), the helper disconnects every
  account, without marking anything read or setting any presence, and exits
  within two seconds.
- Nothing but protocol lines goes to stdout. The helper logs to
  `<data-dir>/helper.log` (connection states and error kinds only: never
  message text, names, cookies, tokens or keys). stderr stays quiet.

## Ids

All ids are numbers the helper assigns, so tuimeta never sees a network's own
ids.

- `network`: `"messenger"`, `"instagram"`, `"whatsapp"` or `"threads"`. One
  account per network. Threads has no chats, only its feed (`load_feed`)
  and its posts' replies (`load_comments`).
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
  Ids are chronological, but `message` events aren't in id order: a message
  can arrive after a newer one (one that crossed yours, one the network held
  back, a retried decryption), and goes in at its place.
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
- `post_id` (i64 > 0): a post of a network's home feed (`load_feed`), or
  one asked for by its link (`load_post`). Stable for the run: the same
  post given again, by a later page, a `fresh` load or `load_post`, has the
  same id. Not chronological: the feed is in the network's order.
- `comment_id` (i64 > 0): a comment on a post of the feed, or a reply to
  one (`load_comments`). Stable for the run, like `post_id`.
- `file_id` (i32 > 0): anything downloadable (a photo, a video, a thumbnail, a
  profile picture, a file). Stable for the run, but a file whose message
  was deleted can be forgotten, and `logout` forgets all of the network's
  files: `download` then answers `not_found`, and the same file seen again
  gets a new id.

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
  "view_once": false,           // shown only once on the phone; never offered as a file
  "audio": 23                   // ? a video's sound, when it's a file of its own
}
```

For a photo, `thumbnail` may be the same file as `file_id`. A view-once photo
or video has no `file_id` (0) and no `thumbnail`. A video with `audio` has its
picture alone in `file_id` and its sound in `audio` (Instagram's web feed
sends them apart, as DASH streams): a player plays the two together, and
one without `audio` has its sound in `file_id`, or none.

### LinkPreview

```json
{"url": "https://example.com/post", "title": "A post", "description": "…",
 "image": {"file_id": 30, "width": 600, "height": 315}}   // image ?
```

Only from the network's own data: the helper never fetches a URL found in a
message.

### Post

A post of a home feed (`load_feed`): photos, videos and, on Threads, text
from the people and pages the account follows, in the order the network
puts them.

```json
{
  "id": 5,                      // post_id
  "author": {"name": "Alice Example", "username": "alice.example",
             "verified": false,
             "photo": {"file_id": 40, "width": 150, "height": 150}},   // photo ?
  "date": 1759912345,           // unix seconds
  "caption": "sunset over the lake",   // "" when there's none
  "media": [Media],             // in order, up to 20: several are a carousel
  "likes": 120,                 // ? absent when the author hides the count
  "comments": 8,                // ? on Threads, its replies
  "reposts": 3,                 // ? Threads: reposts and quotes of it
  "shares": 18,                 // ? Threads: times it was sent on
  "url": "https://www.instagram.com/p/DAbc123/",  // the post on the network's website
  "suggested": false,           // Instagram's pick from an account you don't follow
  "context": "sam reposted",    // ? Threads: the line over the post
  "continues": false,           // Threads: it goes on from the post before it
  "quoted": {Post},             // ? Threads: the post it quotes
  "link_preview": {LinkPreview} // ? Threads: the card of a link in its text
}
```

- `author` is the post's owner as the feed describes them, not a `user_id`:
  people seen only in the feed aren't added to anyone's chats or users.
  `name` may be empty (then `username` is what to show).
- `caption` is plain text (no `entities`): at most 2200 characters, as
  Instagram allows, longer ones cut. On Threads it's the post's text, with
  links and mentions as the plain words the post shows.
- An Instagram post has 1 to 20 `media`; a Threads post 0 (text alone) to
  20.
- Each `media` is a `photo` or a `video` (a reel too). For a photo,
  `file_id` is the picture about 1080 px wide (the smallest at least that
  wide, else the largest), for the photo viewer, and `thumbnail` the one
  the feed draws: the smallest version that fills the box `load_feed`
  named without being enlarged (at least `width` wide or `height` tall),
  else the largest, but never one wider than `file_id`'s; without a box,
  the same file as `file_id`. For a video, `file_id` is the video (`mime`
  `video/mp4`), the version about 720 px wide rather than the largest, to
  keep downloads small, `duration` its length, and `thumbnail` its cover
  picture, picked as a photo's `thumbnail` is. The video comes
  from the media's `video_versions`, whole files with their sound; without
  any, from its DASH manifest (`video_dash_manifest`): the picture
  representation about 720 px wide as `file_id` and the sound as `audio`,
  each a whole file (a representation's `BaseURL`). A video with neither
  still comes, with `file_id` 0, so its post and cover show. `width` and
  `height` are the media's own. Nothing is downloaded until asked for.
- `url` is, on Instagram, always `https://www.instagram.com/p/<code>/` or
  `https://www.instagram.com/reel/<code>/`; on Threads always
  `https://www.threads.com/@<username>/post/<code>`.
- `suggested` posts are the ones Instagram mixes into the feed from accounts
  the user doesn't follow ("Suggested for you": the timeline's
  `explore_story` rows, and any other row holding a post), in the feed's
  order with the rest. Threads doesn't mark its picks: its "For you" feed
  is all picks.
- Threads only:
  - `context` is the line Threads puts over a post, one line of plain text
    ("sam reposted", "First thread"). A repost comes as the post reposted,
    with `context` naming who reposted it.
  - `continues` is set on a post that goes on from the one before it in the
    feed: a thread of several posts comes as consecutive posts, the first
    without `continues`.
  - `quoted` is the post this one quotes, as a Post with `id` 0 (it isn't
    one of the feed's: it has no comments to load) and no `quoted`,
    `context` or `continues` of its own.
  - `link_preview` is the card Threads made for a link in the post: its
    `url` the link's own address (Threads' `l.threads.com` redirect taken
    off), `title`, and `image` when the card has one on Meta's CDN.
    `description` is empty.

### Comment

A comment on a post of the feed, or a reply to one (`load_comments`). On
Threads, a reply to the post, or a reply to a reply.

```json
{
  "id": 9,                      // comment_id
  "author": {"name": "Sam Rivera", "username": "sam.climbs", "verified": false},
  "date": 1759912345,           // unix seconds
  "text": "so good 😍",
  "likes": 4,                   // ? absent when the network doesn't say
  "replies": 2,                 // replies it has; on Instagram 0 for a reply
  "reply_to": 7,                // ? the comment a reply answers
  "attachment": "photo"         // ? Threads: what it holds besides its text
}
```

- `author` is as a post's, without `photo`.
- `text` is plain text (no `entities`), at most 2200 characters, longer
  ones cut. A comment can mention people (`@name`) and tags (`#tag`) as
  plain words.
- `attachment` is `photo`, `video` or `carousel` (several), for a Threads
  reply that has them: only named, not downloadable. On Threads a reply to
  a reply can have replies of its own; `replies` counts the ones it has.

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
| `login_cookies` | `network`, `cookies`, `browser?` | `{}` once logged in and connected. `cookies` is what the user pasted: a `Cookie:` header value (`c_user=…; xs=…`) or the JSON browser extensions export (an array of `{name, value}` or an object). Required: Messenger `c_user`, `xs`, `datr`; Instagram `sessionid`, `ds_user_id`, `csrftoken` (instagram.com's); Threads `sessionid`, `ds_user_id`, `csrftoken` (threads.com's, which aren't instagram.com's). `browser` is the browser the cookies came from, Chrome and its full version (`"Chrome 150.0.7712.45"`; "Google Chrome" and chrome://version's "(Official Build)" notes are accepted too): the helper then says it's that Chrome on this computer's system (user agent and client hints, see below). Absent, it says what the libraries say (Chrome 141 on Linux). Saved with the session, so the session never changes browser; logging in again is the only way to change it. Errors: `bad_request` (`browser` isn't Chrome with a full version; WhatsApp, which logs in with `login_link`), `bad_cookies`, `checkpoint`, `network`, `cancelled` (a newer `login_cookies` or a `logout` for the network came before it was done; nothing of it is saved). |
| `login_link` | `network`, `phone?`, `attempt?` | `{}` once linked and connected (account `ready`): WhatsApp, which logs in by linking tuimeta as a new device of the account (on the phone: Settings → Linked devices → Link a device). Without `phone`, `login_code` events with `qr` follow: the first at once and good for a minute, then a fresh one about every 20 seconds, for about 2½ minutes in all. With `phone` (the account's number in international form: digits, with an optional leading `+`, spaces and dashes), one `login_code` with `pairing` follows, to type on the phone under "Link with phone number instead". `attempt` is a number tuimeta picks for this login, given back in its `login_code` events and named by `cancel_login`. A newer `login_link`, `cancel_login` or `logout` for the network ends a waiting one, until the phone has confirmed the link: from then on it completes (only `logout` ends it), rather than leaving a device in the phone's list that nothing here can use. Errors: `bad_request` (not a linking network, `phone` isn't a number, or a device that still works is linked: log out first), `timeout` (nothing was scanned or typed in time), `cancelled`, `network`, `unsupported` (WhatsApp asked for something tuimeta can't do, like a passkey). |
| `cancel_login` | `network`, `attempt?` | `{}`: ends the waiting `login_link` of that attempt (any, without one), so no code shown for it works any more; it answers `cancelled`. A `login_link` of that attempt, or an older one, that comes after it isn't started, and answers `cancelled` too. A link of another attempt, or one the phone has already confirmed, goes on. |
| `logout` | `network` | `{}`. Disconnects and deletes everything stored for that network (for Messenger, its encrypted-chat device store too). For Messenger, Instagram and Threads it's local: it never ends the session on Meta's side, and the web session the cookies belong to stays valid until the user ends it themselves (in the browser, or in the site's list of logged-in devices). For WhatsApp, where tuimeta is a linked device of its own, it also unlinks that device, as "Log out" in the phone's list of linked devices does; the phone and any other linked devices stay logged in. |
| `load_chats` | `network?`, `limit` | `{"has_more": bool}`; sends `chat` (and `user`) events for up to `limit` more chats, newest activity first, beyond those already sent. tuimeta calls it again until `has_more` is false. A network's loads run one at a time, and one network's don't wait for another's. Threads has no chats: `{"has_more": false}`, and no events. |
| `history` | `chat_id`, `before?`, `after?`, `around?`, `limit` | `{"messages": [Message], "has_more": bool}`, oldest first. With no `before`/`after`/`around`: the newest. They are positions in time, not lookups: any id works, a message's or not (`ms << 8` order), so `before` gives messages with smaller ids, `after` larger ones, and `around` about `limit/2` on each side of it (including one with that id). WhatsApp keeps no history on its servers: a chat has what this device received since it was linked and what the phone sent it then, and older messages are asked of the phone, which must be online (a page may take up to 20 s; without an answer, what's here is the answer). A page whose messages would come to more than 6 MiB is cut short to fit a line, whole albums at a time and never below one message: from its far end (the oldest, or the newest for `after`; for `around`, the end farther from its message, which stays), and `has_more` is then true (for `around`, only when the older end was cut). |
| `get_message` | `chat_id`, `message_id` | `{"message": Message}` (e.g. one a reply answers that isn't loaded) |
| `send_text` | `chat_id`, `text`, `reply_to?` | `{"message_id": i64}` (temporary). The text goes as typed: all three networks read `*bold*`-style formatting themselves. |
| `send_files` | `chat_id`, `files`, `caption?`, `reply_to?` | `{"message_ids": [i64]}` (temporary). `files` are what tuimeta listed in the composer and checked on Enter: `[{"path", "dev", "ino", "size", "mtime_ns", "ctime_ns"}]`, the absolute path and the file's identity from its metadata, not following links (`dev`, `ino` and `ctime_ns` are 0 where the system has none, as on Windows). The helper opens each path without following a link, reads it once, now, from that open file, and sends nothing (`bad_request`: "A file changed since it was attached; attach it again.") unless every file is a regular file with exactly that identity (the zeros aside) and gives exactly `size` bytes. |
| `edit_text` | `chat_id`, `message_id`, `text` | `{}` |
| `delete` | `chat_id`, `message_id` | `{}`: unsends your message for everyone. |
| `react` | `chat_id`, `message_id`, `emoji?` | `{}`: sets your one reaction; `null` removes it. |
| `mark_read` | `chat_id`, `message_id` | `{}`: marks the chat read up to that message. tuimeta doesn't ask it for a chat with `request: true` (a receipt would tell a stranger you read them before you chose to answer); Messenger and Instagram answer `{}` for one and mark and send nothing, so it stays unread until it's accepted. |
| `typing` | `chat_id`, `typing` | `{}`. A chat's changes go out one at a time, in the order they were requested: one whose turn comes after a later-requested one went out is dropped (answered `{}`), so a start can't overtake its stop. |
| `download` | `file_id`, `priority` (`high`/`low`) | `{}`, then `file` events. |
| `search` | `network`, `query` | `{"results": [{"chat_id?", "user_id?", "title", "username?", "kind"}]}`: people and groups to start or open a chat with. On WhatsApp: the contacts and groups this device knows, and, for a query that is a phone number written with `+` and its country code, that number if it's on WhatsApp. Threads answers `unsupported`. |
| `open_dm` | `network`, `user_id` | `{"chat_id": i64}`: the chat with that person, made if needed. Threads answers `unsupported`. |
| `mute` | `chat_id`, `muted` | `{}` |
| `load_feed` | `network`, `limit`, `fresh?`, `width?`, `height?`, `feed?` | `{"posts": [Post], "has_more": bool}`: the next posts of the account's home feed, about `limit` of them (the network gives pages of its own size, so fewer or more can come), after those already given. `fresh: true` starts again from the top, as pulling the feed down does: the newest posts, whatever was loaded before. `width` and `height` are the largest box, in pixels, tuimeta draws a post's picture in, so each `thumbnail` is no bigger than it needs to be (see Post); without both above 0 there's no box, and a side past 8192 counts as 8192. Suggested posts come too, marked `suggested`; ads are left out. A network's loads, and its `load_post`s, run one at a time, in the order asked. `feed` picks Threads' feed: `for_you` (the default) or `following` (only the accounts you follow, newest first); switching starts it from the top, as `fresh` does, and Instagram ignores it. Instagram and Threads have one: Messenger and WhatsApp answer `unsupported`. Errors: `bad_request` (a `feed` that isn't one of those), `not_logged_in`, `network`, `checkpoint`, `cancelled` (a `cancel` named it, or on Threads a login or logout came while it loaded). |
| `load_comments` | `post_id`, `replies_of?`, `more?` | `{"comments": [Comment], "has_more": bool}`: the comments on a post that `load_feed` or `load_post` gave, a page at a time, in the network's order (the post's page shows its top comments first). Without `more`, the first page, again if asked again; with `more: true`, the page after the last one given. With `replies_of` (a `comment_id`), that comment's replies instead, oldest first, paged the same way (on Threads in Threads' order, which puts the best first). On Threads the comments are the post's replies, top ones first. A post's loads run one at a time, in the order asked. Errors: `not_found` (a post or comment this run didn't give), `not_logged_in`, `network`, `checkpoint`, `cancelled` (a `cancel` named it, or on Threads a login or logout came while it loaded). |
| `load_post` | `network`, `code`, `username?`, `width?`, `height?` | `{"post": Post}`: the post a link names, by the code in it: on Instagram `instagram.com/p/<code>/` (and `/reel/`, `/reels/`, `/tv/`, with or without a username before them), on Threads `threads.com/@<username>/post/<code>` (`username` is the one the link names; without it, the post is found by its code alone). `width` and `height` are `load_feed`'s. It's the same post `load_feed` would give, with the same `post_id` if it gave it (or gives it later), never `suggested` and without `context` or `continues`, and `load_comments` takes it like the feed's. Instagram's is asked for as the post's page on instagram.com asks (`/api/v1/media/<id>/info/`, the id the code stands for); Threads' is read from the post's page on threads.com, loaded as a browser opening the link loads it (the feed's and replies' requests after it still go as from the home page, with its tokens). A private account's post comes only if the account follows it, as on the website. It waits its turn in the network's line with `load_feed`, and a newer `load_post` for the network calls it off (tuimeta shows one linked post at a time), as a `cancel` naming it does: it answers `cancelled`, and if it was still waiting its turn, the network is never asked. Instagram and Threads have posts: Messenger and WhatsApp answer `unsupported`. Errors: `bad_request` (`code` isn't a network's post code, made of letters, digits, `-` and `_`, at most 64; `username` isn't a username), `not_found` (no such post, or not one the account can see: deleted, or a private account's; or nothing tuimeta can show), `cancelled` (a newer `load_post` for the network came first, a `cancel` named it, or on Threads a login or logout), `not_logged_in`, `network`, `checkpoint`. |
| `cancel` | `id` | `{}`: calls off the request of that `id`, as it's read, once tuimeta no longer waits for its answer. Only the loads can be called off: `load_feed`, `load_post` and `load_comments`, which only read. That request answers `cancelled`, whatever came of it. If it was still waiting its turn in its network's line, the network is never asked; if it had gone to the network already, it was asked all the same, and only its answer goes unread. An `id` that's through, unknown, or of a request that can't be called off is fine: `{}` all the same. Errors: `bad_request` (no `id`). |

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
- No URL from a message is ever fetched. `load_post` gets only a code (and
  a username) from a link tuimeta found, on the user's Enter, and the
  helper asks for that post as the network's own website would.
- The feed is only read: `load_feed`, `load_post` and `load_comments` send
  Instagram and Threads nothing about what was looked at (no seen posts,
  view, impression or video-play logging: on Threads the list of posts seen
  goes out empty, as from a page just opened, and none of the website's
  logging requests are made) and never like, repost, follow or reply.
  Files of its posts download only on `download`, like any other. On
  Threads, the only requests are the website's own pages (its home page,
  and a post's for `load_post`) and queries (GraphQL requests the website's
  code marks as queries, never a mutation) and, when Threads stops taking a
  query as it was, the website's own JavaScript, fetched without cookies,
  to find how it's asked now.
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
  and every WhatsApp chat, one Instagram chat with a link preview, and an
  Instagram group with links to a made-up reel and thread) and 60
  messages of history per chat over the last week, including replies,
  reactions, an edited message, a service message, an album of 3 photos and
  a file.
- Photos and avatars are small PNGs the helper draws and writes to
  `<data-dir>/fake/` on `download`, with a few `file` progress events.
- `send_text` / `send_files`: `message_sent` after 300 ms. In a dm, the other
  person starts `typing` 500 ms later and answers "echo: <text>" 1.5 s later.
- `mark_read`, `typing`, `react`, `edit_text`, `delete`, `mute` change the
  fake state and send the events the real network would.
- Instagram's `load_feed` gives 30 made-up posts from the last week, 10
  per page, then `has_more: false`; `fresh` starts again from the first.
  Among them: a carousel of three photos, a carousel of a photo and a
  video, videos, a post with no caption, one with a caption of many lines
  one with its likes hidden, and every fourth post `suggested`. A video's
  file is a short animated GIF the
  helper draws (`mime` `image/gif`), which a player plays like a video, and
  its `thumbnail` a PNG of its first frame. One video has its sound apart,
  as `audio`: a quiet two-second tone, a WAV file (`audio/wav`).
- Instagram's `load_comments` gives each made-up post as many comments as
  its `comments` says (at most 40), 10 per page: from several people, one
  of them verified, one comment of many lines, one with a mention and a
  tag, and some with replies (up to 12, 5 per page). The same post gives
  the same comments every time.
- Threads has no chats. Its `load_feed` gives 30 made-up posts, 10 per
  page (the `following` feed another 30, of fewer people), among them text
  alone (one of many lines, one at Threads' 500 characters), a photo, a
  carousel of four photos, a video, a quote of another post, a link with
  its card, a repost (with `context`), and a thread of three posts (the
  later two `continues`). Its `load_comments` gives replies as Instagram's
  gives comments, some with an `attachment`, and replies to replies.
- `load_post` finds a post of the made-up feeds by its code (Instagram's
  `FAKEPOST00` to `FAKEPOST29`, Threads' `FAKETHFY00`… and `FAKETHFL00`…,
  as their `url`s have them), with the id the feed gives it, and answers
  `not_found` for any other code.
- The run is deterministic apart from timing: the same ids, names and texts
  every time.
