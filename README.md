<div align="center">

# tuimeta

**Messenger, Instagram and WhatsApp at the speed of your keyboard.**

A terminal client (TUI) for your own Facebook Messenger, Instagram and WhatsApp chats, with vim keys, inline photos and reactions.<br>
The same app as [tuigram](https://github.com/erictran308/tuigram), for Meta's messengers.

</div>

> [!WARNING]
> **tuimeta isn't made or allowed by Meta.** Meta offers no way for an app like this to use a personal account, so it speaks Messenger's, Instagram's and WhatsApp's own protocols, through [mautrix-meta](https://github.com/mautrix/meta) and [whatsmeow](https://github.com/tulir/whatsmeow). Using it is against Meta's terms, and **an account can be locked, checkpointed or banned for it**. Use it like a person (no bulk messages, no messages to people who don't know you), turn on two-factor authentication, and try it with an account you can afford to lose first.

> [!IMPORTANT]
> **For personal use only.** tuimeta is for reaching your own Messenger, Instagram and WhatsApp chats from your own accounts. It is not for automation, scraping, bulk or unsolicited messaging, bots, collecting other people's data, or any commercial or business use. See the [disclaimer](#disclaimer) below.

---

## What it does

- **Three networks, one list.** Log in to Messenger, Instagram, WhatsApp, or any of them. Tabs over the chat list go round All, each network you're in, and the archive, each with its unread chats.
- **Vim all the way.** Normal mode to move around (`j`/`k`, `gg`/`G`, `Ctrl-d`/`Ctrl-u`), `i` to write, `Esc` to stop.
- **WhatsApp, linked like WhatsApp Web.** Scan a QR code with your phone (or type a code on it), and tuimeta becomes one of your linked devices. It has the chats your phone sends it when you link it and everything after, and asks your phone for older messages as you scroll up (the phone has to be online for that: WhatsApp keeps no history on its servers). Disappearing messages disappear here too, and view-once photos stay on your phone.
- **Encrypted chats.** Every WhatsApp chat, and Messenger's end-to-end encrypted chats, are marked with a 🔒 that a name can't fake, and their notifications never show what was said. tuimeta joins your account as a new device, so it has the encrypted messages sent from the moment you log in on. Older encrypted history isn't there, and there's nowhere to enter the 6-digit PIN Messenger asks for to restore it: that PIN unlocks Messenger's encrypted backup, which the libraries tuimeta builds on can't open. Read older encrypted messages on your phone or at facebook.com.
- **Photos inline.** Real images in kitty, Ghostty, WezTerm and iTerm2, and block-character previews in any other terminal. `Enter` on a photo shows it as big as the window allows: `h` / `l` go through the chat's photos, `j` / `k` zoom in and out, and `o` opens it in your default app.
- **Reactions.** `R` offers Messenger's reactions and common emoji, and `/` finds any emoji by name. You have one reaction per message: `Enter` on yours, or `X`, takes it back.
- **Replies, edits, unsend.** `r` replies, `gd` jumps to what a reply answers, `e` edits your message while the network allows, `d` unsends it.
- **Send files.** `a` attaches a file by path, `p` or `Ctrl-v` pastes copied files or an image, and dropping files on the window attaches them.
- **Find anyone.** `s` searches your chats, and the networks for people to start a chat with (on WhatsApp, your contacts, or any number on WhatsApp typed with `+` and its country code).
- **Privacy first.** Read receipts go out only when you're actually looking at the newest message, and never for a message request you haven't accepted; typing only while you type, and tuimeta never sets an "active now" or "online" status (so on WhatsApp it never moves your last seen, and you'll rarely see others typing: WhatsApp tells only devices that say they're online). (Instagram works out "Active now" on its own servers and may count tuimeta's connection: turn off Activity Status in Instagram's settings to be sure.) Links that hide where they go or whose address can pass for another site's, and files that could run code, ask before opening. A file you attach goes only if it's still the file the composer listed.

**What tuigram has that tuimeta doesn't**, because Meta's messengers don't have it, or tuimeta can't reach it yet: secret chats with timers (Messenger's encryption is per chat instead, and the disappearing-messages timer of WhatsApp chats and Messenger's encrypted ones is set on the phone, though messages still disappear here when it says), forum topics, bots' buttons, polls and locations (shown as `[Poll]` and `[Location]`), stickers to send, calls, pinned messages, voice messages played in the terminal (they open in your player), searching a chat's history, forwarding, and folders.

## Get started

tuimeta is two programs: `tuimeta`, the app (Rust, MIT), and `tuimeta-helper`, the separate program that speaks Meta's protocols (Go, AGPL). The app starts the helper and talks to it over a pipe; the helper must be next to the app.

tuimeta is distributed on GitHub only and built from source — it is **not** published to crates.io, so there is no `cargo install`. Clone the repo and build both programs:

```sh
git clone https://github.com/erictran308/tuimeta && cd tuimeta
cargo build --release
(cd helper && go build -o ../target/release/tuimeta-helper .)
./target/release/tuimeta
```

You need Rust 1.88 or newer and Go 1.27.

### Logging in

tuimeta logs in to Messenger and Instagram with your browser's session cookies, so it never sees your password:

1. Log in to [facebook.com](https://www.facebook.com) (for Messenger) or [instagram.com](https://www.instagram.com) in your browser.
2. Open the developer tools: Application (Chrome, Edge) or Storage (Firefox) → Cookies → the site.
3. Copy the cookies tuimeta asks for: `c_user`, `xs` and `datr` for Messenger; `sessionid`, `ds_user_id` and `csrftoken` for Instagram.
4. Paste them into tuimeta as `name=value; name=value; …`, or paste a cookie export (the JSON that cookie-export extensions make).

These cookies are your whole session: anyone who has them is logged in as you. tuimeta keeps them only in its data folder, readable by you alone. It's the same session as the browser's: logging out there ends tuimeta's too. `:logout` in tuimeta is local — it disconnects and deletes everything tuimeta kept for that network (for Messenger, its encrypted-chat device store too), but it does **not** end the session on Meta's side. Your Facebook or Instagram session stays logged in until you end it yourself, in the browser you copied the cookies from or in the site's list of logged-in devices.

`:login` adds another network, or logs in again.

#### WhatsApp

WhatsApp has no cookies: tuimeta links itself to your account as a new device, the way WhatsApp Web does.

1. Pick WhatsApp on the login screen. tuimeta shows a QR code (the window needs to be about 72×40 for it).
2. On your phone, open WhatsApp → Settings → Linked devices → Link a device, and scan it.

Or press `p`, type your phone number with its country code, and type the code tuimeta shows on your phone (Link a device → Link with phone number instead). If WhatsApp asks to confirm with a passkey, tuimeta can't do that: try the other way.

Your phone lists tuimeta as Chrome on this computer's system, like WhatsApp Web. People who aren't in your phone's address book show with a "~" before the name they gave WhatsApp, and their number next to it, as on the phone, so nobody can pass for "Mum" by calling themselves that. `:logout` unlinks it (your phone and other linked devices stay logged in) and deletes everything tuimeta kept for WhatsApp; you can also log it out from the phone's list, and WhatsApp drops linked devices left unused for weeks.

#### Log in as your own Chrome

Left alone, tuimeta tells Facebook and Instagram it's Chrome 141 on Linux, as every client built on mautrix-meta (Beeper's included) does. If you copy the cookies from Chrome on the computer tuimeta runs on, you can have it say it's that Chrome instead, so the session doesn't look like it moved to a second device:

1. Open `chrome://version` in Chrome and note the version on its first line, like `150.0.7712.45`.
2. Before logging in, set `TM_BROWSER` to `Chrome` and that version: `export TM_BROWSER="Chrome 150.0.7712.45"`.
3. Log in. The cookie screen says which browser it will log in as.

tuimeta then sends that Chrome's user agent and client hints, for this computer's system, on everything it sends to Meta. Only Chrome works: tuimeta's connections are built the way Chrome's are, so naming Safari or Firefox would make it stand out more, and tuimeta refuses it. The browser is saved with the session, so changing `TM_BROWSER` later does nothing until you `:logout` and log in again. When Chrome updates itself, the session keeps saying the version it logged in with.

### Try it first

`tuimeta --demo` shows made-up chats without starting anything (keys `1`–`9` switch scenes, `t` the theme). `tuimeta --fake` runs the whole app with made-up accounts and no network at all: log in with any text that names the cookies, like `c_user=1; xs=2; datr=3`; WhatsApp's made-up code links by itself 3 seconds after it shows.

## Keys

The status bar lists the keys for wherever you are, and `?` lists them all.

| Where | Key | What |
|---|---|---|
| Anywhere | `q` / `Ctrl-c` | Quit |
| | `?` | Shortcuts and settings |
| | `s` | Find a chat or a person |
| | `:` | Commands: `login`, `logout` |
| Linking WhatsApp | `p` | Use your phone number instead of the QR code |
| | `Ctrl-o` / `Ctrl-i` | Back and forward through chats and replies |
| | `Ctrl-r` | Resize the chat list |
| Chat list | `j` / `k`, `gg` / `G` | Move |
| | `Enter` / `l` | Open the chat |
| | `i` | Open it and write |
| | `Tab` / `Shift-Tab` | Next or previous tab |
| | `/` | Filter chats by title |
| | `m` | Mute or unmute |
| | `H` | Highlight the chat |
| Messages | `j` / `k` | Newer, older |
| | `Enter` | Show the photo as big as the window allows (`h` / `l` the one before / after it, `j` / `k` zoom in / out, up to filling the window, `o` opens it in your default app, `y` copies it, `Esc` closes it), or open the file or link |
| | `r` | Reply |
| | `e` | Edit your message |
| | `d` | Unsend your message |
| | `R` / `X` | React / take your reaction back |
| | `y` | Copy |
| | `a` / `p` | Attach a file / paste |
| | `gd` | Go to the message a reply answers |
| Writing | `Enter` | Send |
| | `Alt-Enter` / `Ctrl-j` | New line |
| | `:` and a few letters, `Tab` | An emoji by its shortcode |
| | `Esc` | Back to Normal mode |

## Your data

Everything is in one folder, `tuimeta --help` says where (`TM_DATA_DIR` moves it, to a folder only you can change: on Windows, one inside your user folder): your settings, your own themes, and in `helper/` the sessions, downloaded files and the helper's log. For Messenger's encrypted chats it also keeps the device's keys and the encrypted messages it received, the newest 3000 per chat (tuimeta can't restore Messenger's PIN-locked backup, so these are all it has of them). For WhatsApp it keeps the linked device's keys and the chats and messages it has, the newest 3000 per chat, since WhatsApp keeps none on its servers. In both, disappearing messages are deleted when their time is up. The folder is readable by you alone, but not encrypted, and backups (Time Machine and the like) copy it like everything else: anyone who can read your files can read those messages and act as those devices until you remove them from your accounts. Logging out of a network deletes what's kept for it, and `helper/ids.json.bad`, a list of ids the helper set aside as unreadable, which may hold any network's.

The log never holds what people wrote, their names, or your cookies.

## Themes

`?` → Settings picks one of the built-in themes. Your own go in the `themes` folder of the data folder, as TOML: see [tuigram's README](https://github.com/erictran308/tuigram#themes) for the format, which is the same.

Panes and popups have round corners. In terminals whose font can't draw them (the Linux console, the old Windows console), they're square instead. If they look broken in yours, turn off **Round corners** in `?` → Settings, or set `corners` in `settings.toml` to `"square"` (or `"rounded"`; the default `"auto"` picks for your terminal).

If your terminal uses a [Nerd Font](https://www.nerdfonts.com), turn on **Round pills** there too (`nerd_font = true`): unread counts, reactions, the tab you're on and the mode in the status bar get round ends. Other fonts show a box for them, so it's off at first.

## Development

```sh
cargo test                  # the app's tests
cargo clippy --all-targets
(cd helper && go test ./... && go vet ./...)
cargo run -- --fake         # needs target/debug/tuimeta-helper (see helper/README.md)
```

Never point a development build at your real account to try a change: `--fake` and `--demo` exist for that. Development builds also read `TM_*` settings from a `.env` in the repository's root (see `.env.example`; `TM_HELPER` must be an absolute path), never from the folder they're run in; release builds read no `.env`. `helper/PROTOCOL.md` is the contract between the two programs.

## Disclaimer

tuimeta is an unofficial, independent hobby project. It is **not** affiliated with, endorsed by, sponsored by, or connected to Meta Platforms, Inc., Facebook, Messenger, Instagram or WhatsApp in any way. "Facebook", "Messenger", "Instagram", "WhatsApp" and "Meta" are trademarks of Meta Platforms, Inc., used here only to say what tuimeta talks to.

Using tuimeta breaks Meta's terms of service, and Meta may lock, checkpoint, restrict or permanently ban any account that uses it. **You use tuimeta entirely at your own risk.** You are responsible for your own account, your own data, and for complying with all applicable laws and the terms of every service you connect to.

The software is provided "as is", without warranty of any kind, express or implied, including but not limited to the warranties of merchantability, fitness for a particular purpose and non-infringement. To the fullest extent permitted by law, the authors and contributors are **not liable** for any claim, damages, account loss or ban, data loss, or other liability arising from the software or its use, whether in contract, tort or otherwise. See the [MIT license](LICENSE) for the full terms covering the app.

For personal use only: do not use tuimeta for automation, scraping, bulk or unsolicited messaging, bots, harvesting other people's data, or any commercial purpose.

## License

The app, everything outside `helper/`, is [MIT](LICENSE). The helper, in `helper/`, is [AGPL-3.0-or-later](helper/LICENSE), as mautrix-meta, which it's built on, is. They're separate programs that talk over a pipe; if you distribute them together, the helper's source (including any changes) has to be offered under the AGPL.
