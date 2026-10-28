use std::collections::{HashMap, HashSet};
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::{Duration, SystemTime};

use anyhow::Result;
use crossterm::event::{Event, EventStream, KeyCode, KeyEvent, KeyEventKind, KeyModifiers};
use futures::StreamExt;
use ratatui::layout::Rect;
use ratatui::style::Style;
use ratatui_textarea::{DataCursor, TextArea};
use tokio::sync::mpsc::{UnboundedReceiver, UnboundedSender};
use tokio::time::{Instant, sleep_until};

use crate::attach::{self, Attachment, Dropped};
use crate::chats::{self, Chats, List, Presence};
use crate::chime::{self, Ringer, Sounds};
use crate::clipboard::{Clipboard, ClipboardEvent, Copied, Decoded, Paste, Pasted};
use crate::comments::{Comments, Replies};
use crate::complete::{self, Completion};
use crate::config;
use crate::feed::{self, Feed};
use crate::images::{self, ImageEvent, Images};
use crate::messages::{
    Editable, Editing, Link, MediaFile, OpenChat, Preview, Replied, SendState, link_host,
};
use crate::meta::{AccountState, CommentInfo, FeedPost, Meta, MetaEvent, Network, Page};
use crate::motion::{self, Motion};
use crate::notify::{self, Note, Notifications, Notifier};
use crate::pepper::{self, Pepper, Placed};
use crate::picker::{ChatPicker, Choice};
use crate::player::{self, Player, PlayerEvent};
use crate::reactions::{self, ReactMenu};
use crate::settings::{self, Settings, Side};
use crate::term::Terminal;
use crate::text;
use crate::theme::{Colors, Corners, Themes};
use crate::ui;
use crate::viewer::PhotoView;

/// Chats asked for at once, per network.
const CHAT_PAGE: i32 = 50;
/// Messages asked for per history page.
const HISTORY_PAGE: i32 = 50;
/// Keep asking for history until at least this many messages are loaded.
const MIN_LOADED: usize = 30;
/// Start loading the next page when the cursor gets this close to the end.
const LOAD_AHEAD: usize = 10;
/// Rows moved by Ctrl-d / Ctrl-u.
const HALF_PAGE: isize = 10;
/// Posts of the feed moved by Ctrl-d / Ctrl-u.
const FEED_JUMP: isize = 3;
/// How long to wait for the helper to disconnect on quit.
const CLOSE_TIMEOUT: Duration = Duration::from_secs(3);
/// How long a toast stays up.
const TOAST_TIME: Duration = Duration::from_secs(2);
/// A confirmation ignores `y` for this long after it comes up.
const CONFIRM_GRACE: Duration = Duration::from_millis(500);
/// Where the terminal never says when its window loses focus, no key for
/// this long counts as being away.
const IDLE_AFTER: Duration = Duration::from_secs(60);
/// Even in a window the terminal says has focus, nothing is marked read
/// after this long without a key: the screen can be left showing while
/// nobody is at it, or the connection behind it can be gone without the
/// terminal saying so.
const AWAY_AFTER: Duration = Duration::from_secs(5 * 60);
/// While typing, the chat is told again this often: others' apps stop
/// showing it after a few seconds without a repeat.
const TYPING_EVERY: Duration = Duration::from_secs(5);
/// The sound for new messages plays at most this often, so a burst of them
/// rings once.
const RING_GAP: Duration = Duration::from_secs(1);

pub enum Screen {
    Login(Box<Login>),
    Main,
}

/// A video of the feed playing over its post inside Pepper, or downloading
/// to.
pub struct Playing {
    pub post_id: i64,
    pub file_id: i32,
    /// The token its box is placed with ([`crate::pepper`]).
    token: String,
    /// Where the video is, once downloaded.
    video: Option<String>,
    /// Its sound, when that's a file of its own (Instagram's DASH videos),
    /// and where it is once downloaded.
    audio: Option<(i32, Option<String>)>,
    /// `None` while the files download.
    player: Option<Player>,
    pub paused: bool,
    pub muted: bool,
    /// Where it is and how long it is, in seconds, as pepper-play tells.
    pub position: Option<f64>,
    pub duration: Option<f64>,
    /// `f`: it fills the window, over everything but the status bar.
    pub full_screen: bool,
}

impl Playing {
    /// What the post's line says about it.
    pub fn label(&self) -> &'static str {
        match (&self.player, self.paused, self.muted) {
            (None, ..) => "loading…",
            (_, true, _) => "paused",
            (_, _, true) => "playing, muted",
            _ => "playing",
        }
    }
}

pub enum LoginStep {
    /// Waiting for the helper to say which accounts it has.
    Connecting,
    /// Which network to log in to; the cursor's row.
    Choose {
        selected: usize,
    },
    /// The cookies of a network, pasted from the browser.
    Cookies {
        network: Network,
    },
    /// Linking tuimeta as a new device of the account (WhatsApp), by a QR
    /// code or with the phone number.
    Link {
        network: Network,
        how: Linking,
    },
    LoggingOut,
}

/// How a link goes, and the code it's at.
pub enum Linking {
    /// A QR code for the phone to scan: `None` until the helper gives one.
    Qr(Option<String>),
    /// The account's phone number, being typed into the login's input.
    Phone,
    /// A code to type on the phone, for the number given: `None` until the
    /// helper gives one, then with when it stops working (unix seconds).
    Pairing {
        phone: String,
        code: Option<(String, i64)>,
    },
}

/// The longest QR text drawn: WhatsApp's are a few hundred characters, and
/// a QR code of more wouldn't fit any window.
const MAX_QR: usize = 1024;

/// A pairing code as the helper gave it, if it looks like one: letters,
/// digits and dashes, as many as a code has. Anything else is never shown.
fn pairing_code(code: &str) -> Option<String> {
    let code = text::clean(code);
    let code = code.trim();
    (!code.is_empty()
        && code.len() <= 16
        && code.chars().all(|c| c.is_ascii_alphanumeric() || c == '-'))
    .then(|| code.to_ascii_uppercase())
}

pub struct Login {
    pub step: LoginStep,
    pub input: TextArea<'static>,
    pub error: Option<String>,
    /// A request is in flight; Enter is ignored until the helper answers.
    pub busy: bool,
    /// Opened with `:login` while another account is in: Esc goes back to
    /// the chats.
    pub from_main: bool,
}

impl Login {
    pub fn new(step: LoginStep) -> Self {
        let mut input = TextArea::default();
        input.set_cursor_line_style(Style::default());
        // Cookies are the whole account: they never show, even while
        // they're pasted.
        if let LoginStep::Cookies { .. } = step {
            input.set_mask_char('•');
        }
        Self {
            step,
            input,
            error: None,
            busy: false,
            from_main: false,
        }
    }

    pub fn takes_input(&self) -> bool {
        matches!(
            self.step,
            LoginStep::Cookies { .. }
                | LoginStep::Link {
                    how: Linking::Phone,
                    ..
                }
        )
    }
}

/// An account on one of the networks, as the helper last said.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Account {
    pub state: AccountState,
    /// Your name there, once logged in.
    pub name: Option<String>,
    /// What to do about it, when it's in trouble.
    pub error: Option<String>,
}

/// Something in a message that Enter opens or `y` copies.
pub enum Target {
    File(MediaFile),
    /// Message `message_id`'s photo, which opens in the viewer.
    Photo {
        message_id: i64,
        file: MediaFile,
    },
    Link(Link),
    /// The whole text or caption; only copied.
    Text(String),
}

impl Target {
    pub fn label(&self) -> &str {
        match self {
            Target::File(file) | Target::Photo { file, .. } => &file.label,
            Target::Link(link) => &link.url,
            Target::Text(_) => "Whole message",
        }
    }
}

/// Asks before doing something that could hurt: opening a file that may
/// run code, a link whose words say something other than where it goes, or
/// logging out.
pub struct Confirm {
    pub title: String,
    /// Why it asks, and what exactly would happen.
    pub lines: Vec<String>,
    /// For a link: the site it goes to, on a line of its own. A site's name
    /// is at the end of its host, so a long one is cut from the left.
    pub site: Option<String>,
    pub action: Confirmed,
    /// When it came into view. A `y` in the first moments was typed for
    /// whatever was on screen before, so it doesn't count.
    pub shown: Instant,
    /// It has been in view since `shown`: on the chats screen, in a window
    /// with focus.
    in_view: bool,
}

impl Confirm {
    pub fn new(title: impl Into<String>, lines: Vec<String>, action: Confirmed) -> Self {
        Self {
            title: title.into(),
            lines,
            site: None,
            action,
            shown: Instant::now(),
            in_view: false,
        }
    }
}

/// What `y` does in a [`Confirm`].
pub enum Confirmed {
    OpenFile(String),
    OpenLink(String),
    /// Show this Instagram or Threads post here, as Enter on its link would.
    OpenPost(feed::PostLink),
    /// Edit this message, starting from this text, losing its formatting.
    Edit {
        id: i64,
        text: String,
    },
    /// Log out of this network on this computer.
    Logout(Network),
}

impl Confirmed {
    /// What `y` does, for the key hints.
    pub fn verb(&self) -> &'static str {
        match self {
            Confirmed::OpenFile(_) | Confirmed::OpenLink(_) | Confirmed::OpenPost(_) => "open",
            Confirmed::Edit { .. } => "edit",
            Confirmed::Logout(_) => "log out",
        }
    }
}

/// File types that open in a viewer or player, never as a program. Anything
/// else asks first: `.exe`, `.bat`, `.command`, `.jar`, `.terminal`, `.html`
/// and many more can run code when opened.
const SAFE_TO_OPEN: &[&str] = &[
    "jpg", "jpeg", "png", "gif", "webp", "heic", "heif", "avif", "bmp", "tif", "tiff", "mp4",
    "m4v", "mov", "mkv", "webm", "avi", "3gp", "mpg", "mpeg", "mp3", "m4a", "aac", "ogg", "oga",
    "opus", "wav", "flac", "pdf", "txt", "md", "epub", "docx", "xlsx", "pptx", "odt", "ods", "odp",
    "zip", "rar", "7z", "tar", "gz", "tgz",
];

/// Opening message `id`'s `file`: its photo shows in the viewer, anything
/// else opens in its app.
fn file_target(id: i64, msg: &crate::messages::Msg, file: MediaFile) -> Target {
    if msg.photo.as_ref().is_some_and(|p| p.file_id == file.id) {
        Target::Photo {
            message_id: id,
            file,
        }
    } else {
        Target::File(file)
    }
}

/// Whether a file opens in a viewer or player, never as a program; see
/// [`SAFE_TO_OPEN`].
fn safe_to_open(path: &str) -> bool {
    Path::new(path)
        .extension()
        .and_then(|e| e.to_str())
        .is_some_and(|e| SAFE_TO_OPEN.contains(&e.to_ascii_lowercase().as_str()))
}

/// Why a link written out in full can still pass for another site, if it
/// can: its site has letters of other scripts (also when written as
/// `xn--`), some of which look just like Latin ones, or a name before an
/// `@` reads as the site while the site is what comes after it.
fn look_alike(url: &str) -> Option<&'static str> {
    let host = link_host(url)?;
    let lower = url.to_ascii_lowercase();
    let rest = lower
        .strip_prefix("https:")
        .or_else(|| lower.strip_prefix("http:"))?
        .trim_start_matches(['/', '\\']);
    let authority = &rest[..rest.find(['/', '\\', '?', '#']).unwrap_or(rest.len())];
    if authority.contains('@') {
        Some("The part before the @ is not the site.")
    } else if other_scripts(&host) {
        Some("Its site has letters that can pass for Latin ones.")
    } else {
        None
    }
}

/// A host with letters other than Latin ones, as typed or as `xn--`.
fn other_scripts(host: &str) -> bool {
    !host.is_ascii() || host.split('.').any(|label| label.starts_with("xn--"))
}

/// What picking from a [`PickMenu`] does.
#[derive(Clone, Copy, PartialEq, Eq)]
pub enum MenuAction {
    Open,
    Copy,
}

/// Menu for picking what to open or copy when a message holds several things.
pub struct PickMenu {
    pub action: MenuAction,
    pub targets: Vec<Target>,
    pub selected: usize,
}

/// A note in the corner that something worked, gone after [`TOAST_TIME`].
pub struct Toast {
    pub title: String,
    /// What it was about, e.g. what got copied.
    pub detail: String,
    /// When it came up, to slide in from the right.
    pub shown: Instant,
    pub until: Instant,
}

/// The `d` popup, confirming which message is unsent.
pub struct DeleteMenu {
    pub message_id: i64,
    /// The message on one line, so it's clear which one goes.
    pub snippet: String,
}

/// The tabs of the `?` popup.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum HelpTab {
    Shortcuts,
    Settings,
}

/// The `?` popup: every keyboard shortcut, and the settings. On the settings
/// tab, Enter or Space ticks a checkbox (notifications, gaps in the chat list
/// and in message blocks, Normal mode after sending, the sound while focused
/// too), picks the sound for new messages, which plays it, or picks a theme,
/// and saves it at once.
pub struct SettingsMenu {
    pub tab: HelpTab,
    /// First row shown on the shortcuts tab. Drawing keeps it in range.
    pub scroll: usize,
    /// Row on the settings tab: one of the checkboxes, then the sounds from
    /// [`SettingsMenu::SOUNDS`] on and the themes from
    /// [`SettingsMenu::themes`] on.
    pub selected: usize,
    /// First line shown on the settings tab. Drawing moves it to show the
    /// selected row.
    pub settings_scroll: usize,
    /// How notifications went out when the popup opened, so turning them
    /// off and on again keeps the way, e.g. "bell".
    pub saved_notifications: Notifications,
}

impl SettingsMenu {
    /// The notifications row, the first one.
    pub const NOTIFICATIONS: usize = 0;
    /// The "gap between chats" row.
    pub const CHAT_GAPS: usize = Self::NOTIFICATIONS + 1;
    /// The "chat list on the right" row.
    pub const LIST_RIGHT: usize = Self::CHAT_GAPS + 1;
    /// The "gap between messages" row.
    pub const BLOCK_GAPS: usize = Self::LIST_RIGHT + 1;
    /// The "round corners" row.
    pub const CORNERS: usize = Self::BLOCK_GAPS + 1;
    /// The "round pills, with a Nerd Font" row.
    pub const PILLS: usize = Self::CORNERS + 1;
    /// The "animations" row.
    pub const ANIMATIONS: usize = Self::PILLS + 1;
    /// The "Normal mode after sending" row.
    pub const AFTER_SEND: usize = Self::ANIMATIONS + 1;
    /// The "wrap long lines in the composer" row.
    pub const WRAP_COMPOSER: usize = Self::AFTER_SEND + 1;
    /// The "sound while focused too" row, first under the sound's heading.
    pub const SOUND_FOCUSED: usize = Self::WRAP_COMPOSER + 1;
    /// The "no sound" row, the first of the sounds: the built-in ones and
    /// the user's follow it.
    pub const SOUNDS: usize = Self::SOUND_FOCUSED + 1;

    /// The first theme's row, after the sounds. The themes come last, since
    /// the user's own can make a long list.
    pub fn themes(sounds: &Sounds) -> usize {
        Self::SOUNDS + 1 + sounds.list.len()
    }

    /// Opens on `tab` with the cursor on the first setting.
    pub fn new(tab: HelpTab, settings: &Settings) -> Self {
        Self {
            tab,
            scroll: 0,
            selected: 0,
            settings_scroll: 0,
            saved_notifications: settings.notifications,
        }
    }
}

/// Places Ctrl-o and Ctrl-i go back to at most.
const MAX_JUMPS: usize = 100;

/// A place to come back to: a chat, and the message the cursor was on
/// (`None` for the newest, following new ones).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Jump {
    pub chat_id: i64,
    pub message_id: Option<i64>,
}

/// Where Ctrl-o goes back to and Ctrl-i forward to again, as vim's jump
/// list: chats you left for another, and replies `gd` left.
#[derive(Default)]
pub struct Jumps {
    back: Vec<Jump>,
    forward: Vec<Jump>,
}

impl Jumps {
    /// Leaving `from` for somewhere new: Ctrl-o comes back to it, and the
    /// places Ctrl-o had come back from are forgotten.
    pub fn leave(&mut self, from: Jump) {
        self.forward.clear();
        if self.back.last() != Some(&from) {
            self.back.push(from);
        }
        if self.back.len() > MAX_JUMPS {
            self.back.remove(0);
        }
    }

    /// Ctrl-o (`back`) or Ctrl-i: where to go from `here`, which the other
    /// one then comes back to.
    fn go(&mut self, back: bool, here: Option<Jump>) -> Option<Jump> {
        let (from, to) = if back {
            (&mut self.back, &mut self.forward)
        } else {
            (&mut self.forward, &mut self.back)
        };
        let next = from.pop()?;
        to.extend(here);
        Some(next)
    }

    /// Ctrl-o has somewhere to go.
    pub fn can_go_back(&self) -> bool {
        !self.back.is_empty()
    }

    /// Ctrl-i has somewhere to go.
    pub fn can_go_forward(&self) -> bool {
        !self.forward.is_empty()
    }

    /// Forgets the places in these chats, of a network logged out of.
    fn forget(&mut self, gone: impl Fn(i64) -> bool) {
        self.back.retain(|j| !gone(j.chat_id));
        self.forward.retain(|j| !gone(j.chat_id));
    }
}

/// What the status bar prompt is for: a `/` search through chat titles, a
/// `:` command, or the path of a file to attach (`a`).
#[derive(Clone, Copy, PartialEq, Eq)]
pub enum PromptKind {
    Chats,
    Command,
    Attach,
}

/// The prompt in the status bar. Searching chats filters the list as you
/// type; a command runs on Enter.
pub struct Prompt {
    pub kind: PromptKind,
    pub input: TextArea<'static>,
    /// What the last Tab found in the attach prompt, when it was more than
    /// one name. Typing clears it.
    pub completions: Vec<String>,
    /// In the `:` prompt, Tab goes through the commands that start with
    /// what was typed: this is what was typed, and which of them Tab put
    /// in. Typing clears it.
    pub tabbed: Option<(String, usize)>,
    /// The chat filter and cursor from before, which Esc puts back.
    previous_filter: String,
    previous_selected: Option<i64>,
}

impl Prompt {
    pub fn query(&self) -> String {
        self.input.lines().concat()
    }

    /// What was typed before the Tabs that went through completions.
    fn typed(&self) -> String {
        match &self.tabbed {
            Some((typed, _)) => typed.clone(),
            None => self.query(),
        }
    }

    /// Tab (`step` 1) or Shift-Tab (-1): puts in the next or previous of
    /// `options`, the ways to finish what was typed, round the end, as vim
    /// does. Enter still runs it.
    fn tab(&mut self, step: isize, typed: String, options: Vec<String>) {
        let at = self.tabbed.take().map(|(_, at)| at);
        let count = options.len() as isize;
        if count == 0 {
            return;
        }
        let next = match at {
            Some(at) => (at as isize + step).rem_euclid(count),
            None if step > 0 => 0,
            None => count - 1,
        } as usize;
        self.input = prompt_input(&options[next]);
        self.tabbed = Some((typed, next));
    }

    /// Tab in the `:` prompt: the commands that start with what was typed.
    fn complete_command(&mut self, step: isize) {
        let typed = self.typed();
        let options = Command::ALL
            .into_iter()
            .map(Command::name)
            .filter(|name| name.starts_with(typed.trim()))
            .map(String::from)
            .collect();
        self.tab(step, typed, options);
    }
}

/// A chat being looked up to open, with `s`: the chat with someone found.
pub struct Finding {
    /// Whom it's with, for the status bar.
    pub request: String,
    /// Who was picked: only the answer about them opens a chat. Names are
    /// their owners' to choose, so two people can share one, and an answer
    /// about someone picked before can come after a later pick.
    pub network: Network,
    pub user_id: i64,
}

impl Finding {
    fn new(request: &str, network: Network, user_id: i64) -> Self {
        Self {
            request: request.to_string(),
            network,
            user_id,
        }
    }
}

/// What `:` runs. There are no abbreviations: only the full name runs, so a
/// typo can't log you out.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Command {
    Login,
    Logout,
    Following,
    ForYou,
}

impl Command {
    pub const ALL: [Command; 4] = [
        Command::Login,
        Command::Logout,
        Command::Following,
        Command::ForYou,
    ];

    pub fn name(self) -> &'static str {
        match self {
            Command::Login => "login",
            Command::Logout => "logout",
            Command::Following => "following",
            Command::ForYou => "for-you",
        }
    }

    pub fn about(self) -> &'static str {
        match self {
            Command::Login => "Log in to another network, or to one again",
            Command::Logout => "Log out of the network of the chat under the cursor (asks first)",
            Command::Following => "Threads: the accounts you follow, newest first",
            Command::ForYou => "Threads: For you, the posts Threads picks",
        }
    }

    pub fn parse(text: &str) -> Option<Command> {
        Command::ALL.into_iter().find(|c| c.name() == text)
    }
}

/// What decides which pictures a frame draws: see [`must_clear`].
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
struct Layers {
    /// The login screen, which draws none.
    login: bool,
    /// A popup over the chat, under which the bubbles' pictures are held back.
    covered: bool,
    /// The viewer's photo, by file id, and the size it's zoomed to.
    viewer: Option<(i32, usize)>,
}

/// Whether to clear the terminal before drawing a frame with `now` after
/// one with `before`. Sixel and iTerm2 pictures (`paints_over`) stay on
/// screen until every cell of them is drawn over, which tmux may skip for
/// blank ones: so whenever pictures stop being drawn or move (the login
/// screen, a popup over the chat, the viewer's photo opening, closing,
/// changing size or making way for another), everything is drawn again,
/// and no sender's picture shows through the blank cells of a warning or
/// of the login screen.
fn must_clear(before: Option<Layers>, now: Layers, paints_over: bool) -> bool {
    paints_over && before.is_some_and(|before| before != now)
}

/// Ctrl-r's resize mode: the chat list's width before, in percent of the
/// window, which Esc puts back.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Resizing {
    List(u16),
}

/// Where keys go. `Input` is Insert mode; the others are Normal mode.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Focus {
    Chats,
    Messages,
    Input,
}

pub struct App {
    meta: Meta,
    pub screen: Screen,
    pub focus: Focus,
    pub chats: Chats,
    /// Your accounts, as the helper last said.
    pub accounts: HashMap<Network, Account>,
    /// Display names by user id, for message senders in groups.
    pub users: HashMap<i64, String>,
    /// Selected chat id, not index, so the cursor stays put when chats reorder.
    pub selected: Option<i64>,
    pub open: Option<OpenChat>,
    /// The message being written. Cleared when switching chats.
    pub composer: TextArea<'static>,
    pub images: Images,
    /// Files being downloaded to open in their default app when done.
    pub opening: HashSet<i32>,
    clipboard: Clipboard,
    /// Files being downloaded to copy when done, by file id.
    pub copying: HashMap<i32, MediaFile>,
    /// The clipboard is being read for `p`.
    pub pasting: bool,
    pub toast: Option<Toast>,
    /// Shown over everything when Enter or `y` finds several things.
    pub menu: Option<PickMenu>,
    /// The message of the open chat `menu` is about.
    menu_message: Option<i64>,
    /// Messages of the open chat that came in, changed, or came under the
    /// cursor through someone else's deletion in the last
    /// [`CONFIRM_GRACE`], and when: an Enter that quick was typed for what
    /// was there before.
    changed: HashMap<i64, Instant>,
    pub delete_menu: Option<DeleteMenu>,
    pub react_menu: Option<ReactMenu>,
    /// Enter on a photo: it, as big as the window allows.
    pub photo_view: Option<PhotoView>,
    /// `s` to find a chat to open.
    pub picker: Option<ChatPicker>,
    /// A chat being looked up to open. Opening another chat meanwhile
    /// drops the answer.
    pub finding: Option<Finding>,
    /// Suggestions for the `:emoji` being typed; only in Insert mode.
    pub completion: Option<Completion>,
    /// In resize mode (Ctrl-r): the chat list's width before, which Esc
    /// puts back.
    pub resizing: Option<Resizing>,
    pub confirm: Option<Confirm>,
    pub settings: Settings,
    settings_path: PathBuf,
    /// Every theme, read again whenever `?` opens, so changes to a file show.
    pub themes: Themes,
    /// Every sound for new messages, read again whenever `?` opens.
    pub sounds: Sounds,
    /// The sound for new messages, ready to play; `None` for none.
    sound: Option<Arc<[i16]>>,
    /// When the sound for new messages last played, for [`RING_GAP`].
    rang_at: Option<Instant>,
    /// tuimeta runs over SSH, so its sound would play on the other computer.
    pub over_ssh: bool,
    /// Plays the sound for new messages.
    ringer: Ringer,
    /// The colors of the theme in use.
    pub colors: Colors,
    /// Corners are drawn round (`Settings.corners`, `Auto` decided).
    pub rounded: bool,
    pub settings_menu: Option<SettingsMenu>,
    /// Shown in the status bar while typing a `/` search or a `:` command.
    pub prompt: Option<Prompt>,
    /// Networks with a `load_chats` call on its way.
    loading_chats: HashSet<Network>,
    /// Networks with every chat loaded.
    loaded_chats: HashSet<Network>,
    /// Where Ctrl-o and Ctrl-i go.
    pub jumps: Jumps,
    /// Last error, shown in the status bar until the next key press.
    pub status: Option<String>,
    /// First `g` of `gg` was pressed.
    pending_g: bool,
    /// Set once quitting started; we exit at this time even if the helper
    /// never stops.
    pub quit_deadline: Option<Instant>,
    /// The terminal window has focus. Terminals that don't report focus
    /// changes leave this on.
    terminal_focused: bool,
    /// The terminal has reported a focus change, so `terminal_focused` can
    /// be trusted.
    focus_reported: bool,
    /// The last key press or paste, or the window getting focus.
    last_input: Instant,
    /// The same moment by the wall clock. `Instant` stops while the computer
    /// sleeps, so after a wake only this one shows how long you were away.
    last_input_wall: SystemTime,
    /// The chat last told you're typing, and when. `None` once it was told
    /// you stopped.
    typing: Option<(i64, Instant)>,
    /// Tells the user about new messages while they're away from tuimeta.
    notifier: Notifier,
    /// How notifications reach this terminal (never `Auto`).
    notify_with: Notifications,
    /// Inside tmux, which passes codes on only when they're wrapped.
    in_tmux: bool,
    /// Counts notifications sent, to tell them apart.
    notifications_sent: u64,
    /// Messages from before this (unix time) aren't announced: they came in
    /// while tuimeta wasn't running.
    notify_since: i64,
    /// The newest message each chat had a notification for, so an edit or
    /// a reaction to it doesn't make another.
    notified: HashMap<i64, i64>,
    /// Unmuted chats with unread messages, shown in the window title.
    unread_chats: i32,
    /// What decided which pictures the last frame drew.
    drawn_layers: Option<Layers>,
    /// The terminal's cells changed size in pixels, so the next frame is
    /// drawn on a cleared screen: sixel and iTerm2 pictures drawn the old
    /// size would stay on it.
    cells_resized: bool,
    /// When what's animated started (`Settings.animations`).
    pub motion: Motion,
    /// The browser a login says tuimeta is (`TM_BROWSER`: Chrome and its full
    /// version), so the session looks like the browser its cookies came
    /// from. `None` leaves it to the helper.
    pub browser: Option<String>,
    /// Counts the logins asked for: an answer to an earlier one (a link
    /// given up with Esc) is old news.
    login_attempt: u64,
    /// A network's home feed (Instagram's or Threads'), while it's open
    /// instead of a chat.
    pub feed: Option<Feed>,
    /// The feeds as they were left, to come back to as they were.
    feeds_kept: HashMap<Network, Feed>,
    /// When a fresh page last took the place of the posts shown: an Enter
    /// that quick was meant for what was there before.
    feed_changed: Option<Instant>,
    /// `c`: the selected post's comments, in a popup over the feed.
    pub comments: Option<Comments>,
    /// The keys go to the comments pane, not to the post beside it.
    pub comments_focus: bool,
    /// Pepper and its player, when videos play inside the terminal.
    pub pepper: Option<Pepper>,
    /// The feed's video playing over its post, inside Pepper.
    pub playing: Option<Playing>,
    /// Where the last frame drew the playing video's post, for Pepper's
    /// box; `None` while it's out of view.
    pub video_box: Option<Rect>,
    /// Where the last frame drew the open chat, or the feed in its place,
    /// which a page of the feed is sized for (`ui::feed::picture_box`).
    pub chat_area: Rect,
    /// What Pepper was last told about the box.
    placed: Option<Placed>,
    /// Counts playbacks, so news about one that ended is told apart.
    plays: u64,
    player_tx: UnboundedSender<PlayerEvent>,
    player_rx: Option<UnboundedReceiver<PlayerEvent>>,
    exit: bool,
}

impl App {
    pub fn new(
        meta: Meta,
        images: Images,
        clipboard: Clipboard,
        ringer: Ringer,
        settings: Settings,
        settings_path: PathBuf,
    ) -> Self {
        let mut chats = Chats::default();
        chats.set_highlighted(&settings.highlighted_chats);
        let notify_with = settings
            .notifications
            .resolve(|name| std::env::var(name).ok());
        let rounded = settings.corners.rounded(|name| std::env::var(name).ok());
        let (player_tx, player_rx) = tokio::sync::mpsc::unbounded_channel();
        let mut app = Self {
            meta,
            screen: login_screen(LoginStep::Connecting),
            focus: Focus::Chats,
            chats,
            accounts: HashMap::new(),
            users: HashMap::new(),
            selected: None,
            open: None,
            composer: new_composer(),
            images,
            opening: HashSet::new(),
            clipboard,
            copying: HashMap::new(),
            pasting: false,
            toast: None,
            menu: None,
            menu_message: None,
            changed: HashMap::new(),
            delete_menu: None,
            react_menu: None,
            photo_view: None,
            picker: None,
            finding: None,
            completion: None,
            resizing: None,
            confirm: None,
            settings,
            themes: Themes::load(&settings_path.with_file_name("themes")),
            sounds: Sounds::load(&settings_path.with_file_name("sounds")),
            sound: None,
            rang_at: None,
            over_ssh: chime::over_ssh(|name| std::env::var(name).ok()),
            ringer,
            settings_path,
            colors: Colors::default(),
            rounded,
            settings_menu: None,
            prompt: None,
            loading_chats: HashSet::new(),
            loaded_chats: HashSet::new(),
            jumps: Jumps::default(),
            // Only development builds read `.env`; someone expecting it to
            // pick a separate data folder should know this one didn't.
            status: config::dotenv_ignored()
                .then(|| "./.env is ignored: only development builds read it".into()),
            pending_g: false,
            quit_deadline: None,
            terminal_focused: true,
            focus_reported: false,
            last_input: Instant::now(),
            last_input_wall: SystemTime::now(),
            typing: None,
            notifier: Notifier::default(),
            notify_with,
            in_tmux: std::env::var_os("TMUX").is_some(),
            notifications_sent: 0,
            notify_since: i64::MAX,
            notified: HashMap::new(),
            unread_chats: 0,
            drawn_layers: None,
            cells_resized: false,
            motion: Motion::default(),
            browser: None,
            login_attempt: 0,
            feed: None,
            feeds_kept: HashMap::new(),
            feed_changed: None,
            comments: None,
            comments_focus: false,
            pepper: None,
            playing: None,
            video_box: None,
            chat_area: Rect::default(),
            placed: None,
            plays: 0,
            player_tx,
            player_rx: Some(player_rx),
            exit: false,
        };
        app.use_saved_theme();
        app.use_saved_sound();
        app
    }

    /// Uses the theme the settings name, or the default one, saying why, if
    /// it can't be used: a broken file never leaves the app unreadable. The
    /// setting stays, so fixing the file is enough.
    fn use_saved_theme(&mut self) {
        match self.themes.colors(&self.settings.theme) {
            Ok(colors) => self.colors = colors,
            Err(e) => {
                self.colors = Colors::default();
                self.status = Some(format!("Theme not used: {e}"));
            }
        }
    }

    /// Gets the sound for new messages the settings name ready to play, or
    /// says why it can't be, leaving none. The setting stays, so fixing the
    /// file is enough.
    fn use_saved_sound(&mut self) {
        self.sound = None;
        let Some(id) = &self.settings.notification_sound else {
            return;
        };
        match self.sounds.samples(id) {
            Ok(samples) => self.sound = Some(samples),
            Err(e) => self.status = Some(format!("Sound not used: {e}")),
        }
    }

    pub async fn run(
        mut self,
        terminal: &mut Terminal,
        mut events: UnboundedReceiver<MetaEvent>,
        mut image_events: UnboundedReceiver<ImageEvent>,
        mut clipboard: UnboundedReceiver<ClipboardEvent>,
        mut rings: UnboundedReceiver<String>,
    ) -> Result<()> {
        let mut keys = EventStream::new();
        let mut signals = quit_signals();
        let mut players = self
            .player_rx
            .take()
            .unwrap_or_else(|| tokio::sync::mpsc::unbounded_channel().1);
        // What broke the terminal, if it went away: the app then closes as
        // `q` closes it, without drawing, and returns this.
        let mut failed: Option<anyhow::Error> = None;
        while !self.exit {
            if self
                .toast
                .as_ref()
                .is_some_and(|t| t.until <= Instant::now())
            {
                self.toast = None;
            }
            self.chats.refresh();
            if self
                .selected
                .is_none_or(|id| !self.chats.ids().contains(&id))
            {
                self.selected = self.chats.ids().first().copied();
            }
            let resized = std::mem::take(&mut self.cells_resized);
            if self.clear_before_drawing(self.images.paints_over()) || resized {
                let _ = terminal.clear();
            }
            self.confirm_in_view();
            self.mark_seen();
            self.set_unread_chats(self.chats.unread_chats());
            self.send_notification();
            if let Some(query) = self
                .picker
                .as_mut()
                .and_then(|p| p.due_search(Instant::now()))
            {
                // Threads has no one to write to.
                for network in self.ready_networks().into_iter().filter(|n| n.has_chats()) {
                    self.meta.find_chats(network, query.clone());
                }
            }
            // Before the frame, which may send images in their place, and
            // like it, not once writing to the terminal failed.
            if failed.is_none() {
                self.images.send_deletions(terminal);
            }
            if failed.is_none()
                && let Err(e) = terminal.draw(|frame| ui::draw(frame, &mut self))
            {
                failed = Some(e.into());
                self.hang_up();
            }
            // After the frame: where the video playing shows now.
            if failed.is_none() {
                notify::send(&self.video_placement());
            }
            // Start downloads/encodes for photos the frame showed but didn't have.
            self.images.fetch(&self.meta);
            if let Some(open) = self.open.as_mut() {
                for (reply, answered) in open.missing_replied() {
                    self.meta.get_replied_message(open.chat_id, reply, answered);
                }
            }

            let deadline = self.quit_deadline;
            // Wakes up to take the toast down, to send notifications that
            // had to wait, to search once typing in the `s` picker pauses,
            // and to move what's animated on.
            let open = self.open.as_ref().map(|o| o.chat_id);
            let moving = self.motion.next_frame(Instant::now(), open);
            let sliding = self
                .toast
                .as_ref()
                .filter(|_| self.slides_toast())
                .and_then(|t| motion::toast_frame(t.shown, t.until, Instant::now()));
            let wake = [
                moving,
                sliding,
                self.toast.as_ref().map(|t| t.until),
                self.notifier.next_at(),
                self.picker.as_ref().and_then(ChatPicker::search_at),
            ]
            .into_iter()
            .flatten()
            .min();
            tokio::select! {
                Some(event) = events.recv() => {
                    self.on_meta(event);
                    // Drain the backlog so a burst of events costs one redraw.
                    while let Ok(event) = events.try_recv() {
                        self.on_meta(event);
                    }
                }
                Some(event) = image_events.recv() => self.images.on_built(event),
                Some(event) = players.recv() => self.on_player(event),
                Some(event) = clipboard.recv() => self.on_clipboard(event),
                Some(why) = rings.recv() => {
                    self.status = Some(format!("Can't play the sound for new messages: {why}"));
                }
                Some(event) = keys.next(), if failed.is_none() => match event {
                    Ok(event) => self.on_terminal_event(event),
                    Err(e) => {
                        failed = Some(e.into());
                        self.hang_up();
                    }
                },
                Some(()) = signals.recv() => self.hang_up(),
                _ = sleep_until(deadline.unwrap_or_else(Instant::now)), if deadline.is_some() => break,
                _ = sleep_until(wake.unwrap_or_else(Instant::now)), if wake.is_some() => {}
                else => break,
            }
        }
        // Pepper's box goes with the video.
        self.stop_video();
        if failed.is_none() {
            notify::send(&self.video_placement());
        }
        // The terminal would keep the images after tuimeta is gone.
        self.images.drop_all();
        if failed.is_none() {
            self.images.send_deletions(terminal);
        }
        match failed {
            Some(e) => Err(e),
            None => Ok(()),
        }
    }

    /// A warning's grace counts from when it can be seen, before the next
    /// frame: not while the login screen hides it or the window is in the
    /// background, so one that came up meanwhile (a download finishing, say)
    /// still ignores a `y` at first.
    fn confirm_in_view(&mut self) {
        let visible = matches!(self.screen, Screen::Main) && self.terminal_focused;
        let Some(confirm) = self.confirm.as_mut() else {
            return;
        };
        if !visible {
            confirm.in_view = false;
        } else if !confirm.in_view {
            confirm.in_view = true;
            confirm.shown = Instant::now();
        }
    }

    /// What decides which pictures the next frame draws.
    fn layers(&self) -> Layers {
        Layers {
            login: matches!(self.screen, Screen::Login(_)),
            covered: self.popup_over_chat() || self.full_screen(),
            viewer: self.photo_view.as_ref().map(|v| (v.photo.file_id, v.zoom)),
        }
    }

    /// Whether the terminal is to be cleared before the next frame, which
    /// is then taken as drawn (see [`must_clear`]).
    fn clear_before_drawing(&mut self, paints_over: bool) -> bool {
        let now = self.layers();
        let before = self.drawn_layers.replace(now);
        must_clear(before, now, paints_over)
    }

    /// The terminal is gone (a closed window, a dropped SSH connection), or
    /// tuimeta was told to stop: nobody is reading any more, so nothing more
    /// is marked read, and it quits as `q` does.
    fn hang_up(&mut self) {
        self.terminal_focused = false;
        self.focus_reported = true;
        if self.quit_deadline.is_none() {
            self.quit();
        }
    }

    fn on_terminal_event(&mut self, event: Event) {
        if matches!(event, Event::Key(_) | Event::Paste(_) | Event::FocusGained) {
            self.last_input = Instant::now();
            self.last_input_wall = SystemTime::now();
        }
        // A window moved to another screen may have cells of another size
        // in pixels without changing its rows and columns. The terminal
        // then says it was resized, or at least, once it's clicked on,
        // that it has focus.
        if matches!(event, Event::Resize(..) | Event::FocusGained)
            && self.images.measured(images::measure_cells())
        {
            self.cells_resized = true;
        }
        match event {
            Event::Key(key) => self.on_key(key),
            Event::FocusGained => {
                self.terminal_focused = true;
                self.focus_reported = true;
                // Back at tuimeta: what was waiting is on screen.
                self.notifier.clear();
                // And where the keys are, shown by lights round the pane,
                // unless a popup has them.
                if self.settings.animations
                    && matches!(self.screen, Screen::Main)
                    && !self.popup_over_chat()
                {
                    self.motion.focused = Some(Instant::now());
                }
            }
            Event::FocusLost => {
                self.terminal_focused = false;
                self.focus_reported = true;
            }
            Event::Paste(text) if matches!(self.screen, Screen::Login(_)) => {
                if let Screen::Login(login) = &mut self.screen
                    && login.takes_input()
                {
                    // Cookies copied from a browser can come on several
                    // lines; they're one header.
                    let line = text.split_whitespace().collect::<Vec<_>>().join(" ");
                    login.input.insert_str(line);
                }
            }
            Event::Paste(text) if self.picker.is_some() => {
                // A pasted name goes into the search, on one line.
                let text = text::clean(&text).replace(['\n', '\t'], " ");
                if let Some(picker) = self.picker.as_mut() {
                    picker.edit_query(|q| q.push_str(text.trim()), Instant::now());
                }
            }
            Event::Paste(text) if self.prompt.is_some() => {
                if let Some(prompt) = self.prompt.as_mut() {
                    prompt.input.insert_str(text.replace(['\r', '\n'], " "));
                }
                self.on_prompt_edit();
            }
            // Files attached and Insert mode under the photo, out of sight,
            // would go out with the next Enter or two.
            Event::Paste(_) if self.photo_view.is_some() => {
                self.status = Some("Close the photo to paste".into());
            }
            Event::Paste(text) if matches!(self.focus, Focus::Input | Focus::Messages) => {
                self.on_paste(text)
            }
            _ => {}
        }
    }

    /// The networks logged in and connected.
    pub fn ready_networks(&self) -> Vec<Network> {
        Network::ALL
            .into_iter()
            .filter(|n| {
                self.accounts
                    .get(n)
                    .is_some_and(|a| a.state == AccountState::Ready)
            })
            .collect()
    }

    fn on_meta(&mut self, event: MetaEvent) {
        match event {
            MetaEvent::Hello => {}
            MetaEvent::Account {
                network,
                state,
                user_id,
                name,
                error,
            } => self.on_account(network, state, user_id, name, error),
            MetaEvent::LoggedIn {
                network,
                attempt,
                result,
            } => {
                if attempt != self.login_attempt {
                    return;
                }
                let Screen::Login(login) = &mut self.screen else {
                    return;
                };
                match &mut login.step {
                    LoginStep::Cookies { network: n } if *n == network => match result {
                        // The account says when it's connected.
                        Ok(()) => login.input = Login::new(LoginStep::Cookies { network }).input,
                        Err(why) => {
                            login.busy = false;
                            login.error = Some(text::clean(&why));
                        }
                    },
                    LoginStep::Link { network: n, how } if *n == network => {
                        if let Err(why) = result {
                            // The code shown works no more.
                            match how {
                                Linking::Qr(code) => *code = None,
                                Linking::Pairing { code, .. } => *code = None,
                                Linking::Phone => {}
                            }
                            login.busy = false;
                            login.error = Some(text::clean(&why));
                        }
                    }
                    _ => {}
                }
            }
            MetaEvent::LoginCode {
                network,
                attempt,
                qr,
                pairing,
                expires,
            } => {
                // A code for a link given up (Esc, `p`) is old news.
                if attempt == 0 || attempt == self.login_attempt {
                    self.on_login_code(network, qr, pairing, expires)
                }
            }
            // Not the login's answer, which is `LoggedIn` for its attempt:
            // it may be about another network or request, and a link goes
            // on waiting, its codes still shown and Esc still cancelling it.
            MetaEvent::Error(message) => match &mut self.screen {
                Screen::Login(login) => login.error = Some(text::clean(&message)),
                Screen::Main => self.status = Some(text::clean(&message)),
            },
            MetaEvent::Gone(why) => {
                // Nothing more can come: say so, and leave once the user
                // has read it, or at once while quitting.
                if self.quit_deadline.is_some() {
                    self.exit = true;
                    return;
                }
                let why = format!(
                    "{}. Quit and start tuimeta again; helper.log says more",
                    text::clean(&why)
                );
                match &mut self.screen {
                    Screen::Login(login) => {
                        login.busy = false;
                        login.error = Some(why);
                    }
                    Screen::Main => self.status = Some(why),
                }
            }
            MetaEvent::Chat(info) => {
                // A chat with one person is called by their name.
                if let Some(user_id) = info.user_id
                    && info.kind == crate::meta::ChatKind::Dm
                {
                    // One line, like every other name path: the title is a
                    // person's name here, and a sender's name can hold breaks.
                    self.users.entry(user_id).or_insert_with(|| {
                        text::clean(&info.title)
                            .split_whitespace()
                            .collect::<Vec<_>>()
                            .join(" ")
                    });
                }
                self.chats.upsert(&info);
            }
            MetaEvent::ChatRemoved(chat_id) => {
                self.chats.remove(chat_id);
                if self.open.as_ref().is_some_and(|o| o.chat_id == chat_id) {
                    self.close_chat_popups();
                    self.leave_chat();
                    self.focus = Focus::Chats;
                }
            }
            MetaEvent::User(user) => {
                let name = text::clean(&user.name);
                let name = name.split_whitespace().collect::<Vec<_>>().join(" ");
                self.users.insert(user.id, name);
                self.chats.set_username(user.id, user.username.as_deref());
                let now = unix_now();
                self.chats
                    .set_presence(user.id, user.active_at.map(|at| Presence::of(at, now)));
                if user.is_self {
                    self.chats.set_my_id(user.network, user.id);
                }
            }
            MetaEvent::Message(message) => self.on_message(&message),
            MetaEvent::MessageSent {
                chat_id,
                old_id,
                message,
            } => {
                self.chats.on_message(&message);
                if let Some(open) = self.open.as_mut().filter(|o| o.chat_id == chat_id) {
                    open.replace(old_id, &message);
                }
            }
            MetaEvent::MessageFailed {
                chat_id,
                old_id,
                error,
            } => {
                self.status = Some(match error.is_empty() {
                    true => "Message not sent".into(),
                    false => format!("Message not sent: {}", text::clean(&error)),
                });
                if let Some(open) = self.open.as_mut().filter(|o| o.chat_id == chat_id) {
                    open.set_failed(old_id);
                }
            }
            MetaEvent::MessagesDeleted {
                chat_id,
                message_ids,
            } => self.on_deleted(chat_id, &message_ids),
            MetaEvent::Read {
                chat_id,
                inbox,
                outbox,
                unread,
            } => {
                if let Some(inbox) = inbox {
                    self.chats.set_read_inbox(chat_id, inbox);
                    // Read on another device: nothing to tell any more.
                    self.notifier.remove_read(chat_id, inbox);
                }
                if let Some(outbox) = outbox {
                    self.chats.set_read_outbox(chat_id, outbox);
                }
                if let Some(unread) = unread {
                    self.chats.set_unread(chat_id, unread);
                }
            }
            MetaEvent::Typing {
                chat_id,
                user_id,
                typing,
            } => {
                if !self.chats.is_me(user_id) {
                    self.chats
                        .set_typing(chat_id, crate::messages::Sender::User(user_id), typing);
                }
            }
            MetaEvent::ChatsLoaded { network, all } => {
                for network in network.map_or(Network::ALL.to_vec(), |n| vec![n]) {
                    self.loading_chats.remove(&network);
                    if all {
                        self.loaded_chats.insert(network);
                    }
                }
            }
            MetaEvent::History {
                chat_id,
                page,
                messages,
            } => self.on_history(chat_id, page, messages),
            MetaEvent::Replied {
                chat_id,
                message_id,
                replied,
            } => {
                if let Some(open) = self.open.as_mut().filter(|o| o.chat_id == chat_id) {
                    open.set_replied(message_id, replied.as_deref());
                }
            }
            MetaEvent::Downloaded { file_id, path } => {
                if self.opening.remove(&file_id) {
                    match &path {
                        Some(path) => self.open_downloaded(path.clone()),
                        None => self.status = Some("Download failed".into()),
                    }
                }
                if let Some(file) = self.copying.remove(&file_id) {
                    match &path {
                        // Pasted in a file manager, the copy keeps the mark,
                        // as the file would if opened with Enter.
                        Some(path) => {
                            mark_downloaded(path);
                            self.copy_downloaded(file, path)
                        }
                        None => self.status = Some("Download failed".into()),
                    }
                }
                self.on_video_downloaded(file_id, path.as_deref());
                self.images.on_downloaded(file_id, path);
            }
            MetaEvent::Feed {
                network,
                load,
                page,
            } => self.on_feed_page(network, load, page),
            MetaEvent::Comments {
                post_id,
                load,
                page,
            } => self.on_comments_page(post_id, load, page),
            MetaEvent::Post {
                network,
                load,
                post,
            } => self.on_linked_post(network, load, post),
            MetaEvent::ChatsFound {
                network,
                query,
                found,
            } => {
                if let Some(picker) = self.picker.as_mut() {
                    picker.set_found(network, &query, found);
                }
            }
            MetaEvent::ChatFound {
                network,
                user_id,
                request,
                found,
            } => {
                // Dropped if another chat was opened meanwhile, or it's about
                // someone picked before the one being looked up.
                if !self
                    .finding
                    .as_ref()
                    .is_some_and(|f| f.network == network && f.user_id == user_id)
                {
                    return;
                }
                self.finding = None;
                // Not while the keys go somewhere (writing, a popup): what
                // was typed would land in the other chat, or a popup act
                // on it.
                if self.busy() {
                    if found.is_ok() {
                        self.status = Some(format!("Found {request}: press s to open it"));
                    }
                    return;
                }
                match found {
                    Ok(chat_id) => self.open_chat(chat_id),
                    Err(why) => self.status = Some(text::clean(&why)),
                }
            }
        }
    }

    /// An account's state changed: logged in, connected, logged out, or in
    /// trouble. The screen follows: the chats once one is in, the login
    /// screen once none is.
    fn on_account(
        &mut self,
        network: Network,
        state: AccountState,
        user_id: Option<i64>,
        name: Option<String>,
        error: Option<String>,
    ) {
        let was = self.accounts.get(&network).map(|a| a.state);
        let name = name.map(|n| text::clean(&n));
        self.accounts.insert(
            network,
            Account {
                state,
                name: name.clone(),
                error: error.clone().map(|e| text::clean(&e)),
            },
        );
        match state {
            AccountState::Ready => {
                if let Some(id) = user_id {
                    self.chats.set_my_id(network, id);
                    if let Some(name) = name {
                        self.users.insert(id, name);
                    }
                }
                self.chats.add_feed(network);
                if was != Some(AccountState::Ready) {
                    // Messages from before now came while tuimeta wasn't
                    // running, or another account was in.
                    self.notify_since = self.notify_since.min(unix_now());
                    self.loaded_chats.remove(&network);
                    self.load_more_chats(network);
                }
            }
            AccountState::LoggedOut => self.forget_network(network),
            AccountState::Connecting | AccountState::Error | AccountState::Unknown => {}
        }
        if state == AccountState::Error
            && let Some(error) = error
            && matches!(self.screen, Screen::Main)
        {
            self.status = Some(format!("{}: {}", network.name(), text::clean(&error)));
        }
        self.follow_accounts();
    }

    /// The login screen while no account is in, the chats once one is.
    fn follow_accounts(&mut self) {
        let ready = !self.ready_networks().is_empty();
        let known = Network::ALL.iter().all(|n| self.accounts.contains_key(n));
        let waiting = self
            .accounts
            .values()
            .any(|a| a.state == AccountState::Connecting);
        let error = self.accounts.values().find_map(|a| a.error.clone());
        match &mut self.screen {
            Screen::Login(login) => match login.step {
                LoginStep::Connecting | LoginStep::LoggingOut if ready => {
                    self.screen = Screen::Main
                }
                LoginStep::Connecting | LoginStep::LoggingOut if known && !waiting => {
                    let mut choose = Login::new(LoginStep::Choose { selected: 0 });
                    choose.error = error;
                    self.screen = Screen::Login(Box::new(choose));
                }
                LoginStep::Cookies { network } | LoginStep::Link { network, .. }
                    if self
                        .accounts
                        .get(&network)
                        .is_some_and(|a| a.state == AccountState::Ready) =>
                {
                    self.screen = Screen::Main;
                    self.show_toast("Logged in", network.name());
                }
                // A link says how it ended in its answer: the account may
                // still be in trouble from before while it waits.
                LoginStep::Cookies { network } if login.busy => {
                    if let Some(account) = self.accounts.get(&network)
                        && account.state == AccountState::Error
                    {
                        login.busy = false;
                        login.error = account.error.clone();
                    }
                }
                _ => {}
            },
            Screen::Main if !ready && !waiting && known => {
                let mut choose = Login::new(LoginStep::Choose { selected: 0 });
                choose.error = error;
                self.screen = Screen::Login(Box::new(choose));
                // A warning from the chats screen would come back after the
                // next login, about what's past, perhaps another account.
                self.confirm = None;
            }
            Screen::Main => {}
        }
    }

    /// A network was logged out of, here or elsewhere: its chats, and what
    /// was open of them, go.
    fn forget_network(&mut self, network: Network) {
        let gone = |chats: &Chats, id: i64| chats.network(id) == Some(network);
        if self
            .open
            .as_ref()
            .is_some_and(|o| gone(&self.chats, o.chat_id))
            || self.feed.as_ref().is_some_and(|f| f.network == network)
        {
            self.close_chat_popups();
            self.leave_chat();
            self.focus = Focus::Chats;
        }
        let chats = &self.chats;
        self.jumps.forget(|id| gone(chats, id));
        // Highlights are chat ids of the account.
        let highlighted: Vec<i64> = self
            .settings
            .highlighted_chats
            .iter()
            .copied()
            .filter(|&id| !gone(chats, id))
            .collect();
        if highlighted != self.settings.highlighted_chats {
            self.settings.highlighted_chats = highlighted;
            self.chats.set_highlighted(&self.settings.highlighted_chats);
            if let Err(e) = self.settings.save(&self.settings_path) {
                self.status = Some(format!("Couldn't save settings: {e:#}"));
            }
        }
        self.chats.forget(network);
        self.feeds_kept.remove(&network);
        self.loading_chats.remove(&network);
        self.loaded_chats.remove(&network);
        self.picker = None;
        // Its pictures, and where its downloads were, which the helper
        // deleted with the rest.
        self.images.forget();
        // Asking to log out of it is moot, and a `y` would log out of the
        // account linked next.
        if self
            .confirm
            .as_ref()
            .is_some_and(|c| matches!(c.action, Confirmed::Logout(n) if n == network))
        {
            self.confirm = None;
        }
    }

    fn on_history(
        &mut self,
        chat_id: i64,
        page: Page,
        messages: Option<Vec<crate::meta::Message>>,
    ) {
        // Ignore pages for a chat that was closed, or a request that was
        // replaced (e.g. by jumping elsewhere), while it was in flight.
        let Some(open) = self
            .open
            .as_mut()
            .filter(|o| o.chat_id == chat_id && o.loading == Some(page))
        else {
            return;
        };
        open.loading = None;
        let Some(messages) = messages else {
            return;
        };
        let got = messages.len();
        open.add_page(page, messages.iter().map(|m| (m.id, m.into())).collect());
        if open.messages.len() < MIN_LOADED && got > 0 {
            self.load_older_messages();
        }
    }

    /// A message from the helper: a new one, or a new copy of one.
    fn on_message(&mut self, message: &crate::meta::Message) {
        self.chats.on_message(message);
        let sender = crate::messages::Sender::User(message.sender_id);
        if !message.outgoing {
            self.chats.stop_typing(message.chat_id, sender);
        }
        // Nobody sees a message come in while they're away or a popup is
        // over the chat.
        let seen = self.present() && !self.popup_over_chat();
        if let Some(open) = self.open.as_mut().filter(|o| o.chat_id == message.chat_id) {
            let new = !open.messages.contains_key(&message.id);
            open.upsert(message);
            // Someone else's glows as it comes in, if it's in sight: seen,
            // and the view following the newest messages. A theme whose
            // bubbles are the background has none to glow, so nothing wakes
            // the run loop for it.
            let in_sight = seen && open.selected.is_none();
            let bubbles = self.colors.other_bubble != self.colors.bg;
            let glows = new && !message.outgoing && in_sight && bubbles && self.settings.animations;
            if glows && open.messages.contains_key(&message.id) {
                self.motion
                    .arrived(open.chat_id, message.id, Instant::now());
            }
            // Edited to another photo, or to no photo at all.
            let msg = open.messages.get(&message.id);
            if self
                .photo_view
                .as_ref()
                .is_some_and(|v| v.lost(message.chat_id, message.id, msg))
            {
                self.photo_view = None;
            }
            self.just_changed(message.id);
        }
        self.notify(message);
    }

    /// Messages were deleted, by their sender, on another device or when
    /// their time was up. What was about them goes with them: no open, copy
    /// or menu acts on a file whose message is gone, nor a popup on the
    /// message.
    fn on_deleted(&mut self, chat_id: i64, message_ids: &[i64]) {
        self.notifier.remove(chat_id, message_ids);
        if self
            .photo_view
            .as_ref()
            .is_some_and(|v| message_ids.iter().any(|&id| v.lost(chat_id, id, None)))
        {
            self.photo_view = None;
        }
        let Some(open) = self.open.as_mut().filter(|o| o.chat_id == chat_id) else {
            return;
        };
        let files: Vec<i32> = message_ids
            .iter()
            .filter_map(|id| open.messages.get(id))
            .flat_map(|m| {
                let file = m.file.as_ref().map(|f| f.id);
                file.into_iter().chain(m.photo.as_ref().map(|p| p.file_id))
            })
            .collect();
        for file in &files {
            self.opening.remove(file);
            self.copying.remove(file);
        }
        let editing = open
            .editing
            .as_ref()
            .is_some_and(|e| message_ids.contains(&e.id));
        // A cursor that was on one of them lands on another, which may
        // never have been on screen: Enter mustn't open it at once.
        let before = open.cursor_id();
        open.remove(message_ids);
        let after = open.cursor_id().filter(|&after| Some(after) != before);
        if let Some(id) = after {
            self.just_changed(id);
        }
        if editing {
            self.end_edit();
            self.status = Some("The message you were editing was deleted".into());
        }
        let about = |id: Option<i64>| id.is_some_and(|id| message_ids.contains(&id));
        if self.menu.is_some() && about(self.menu_message) {
            self.menu = None;
        }
        if about(self.delete_menu.as_ref().map(|m| m.message_id)) {
            self.delete_menu = None;
        }
        if about(self.react_menu.as_ref().map(|m| m.message_id)) {
            self.react_menu = None;
        }
    }

    /// Message `id` of the open chat just came in, changed, or moved under
    /// the cursor: Enter waits [`CONFIRM_GRACE`] before opening anything in
    /// it, as a [`Confirm`] does for `y`.
    fn just_changed(&mut self, id: i64) {
        let now = Instant::now();
        self.changed
            .retain(|_, at| now.duration_since(*at) < CONFIRM_GRACE);
        self.changed.insert(id, now);
    }

    /// Tells the user about someone else's new message, unless the chat is
    /// muted, it came before tuimeta started, or it was told already: with
    /// the sound for new messages as it comes, and with a notification
    /// unless they see it anyway.
    fn notify(&mut self, message: &crate::meta::Message) {
        let chat_id = message.chat_id;
        let new = self
            .notified
            .get(&chat_id)
            .is_none_or(|&last| message.id > last);
        if self.notify_with == Notifications::Off
            || message.outgoing
            || message.state != crate::meta::MessageState::Sent
            || message.date < self.notify_since
            || !new
            || message.id <= self.chats.read_inbox(chat_id)
            || self.chats.muted(chat_id)
            || self.chats.get(chat_id).is_none()
        {
            return;
        }
        self.notified.insert(chat_id, message.id);
        let seen = self.sees(chat_id);
        self.ring_for(seen);
        if seen {
            return;
        }
        // Notifications stay in the system's list, so an encrypted chat's
        // say neither who nor what.
        if self.chats.is_encrypted(chat_id) {
            let note = Note {
                id: message.id,
                chat_id,
                chat: "Encrypted chat".into(),
                text: "New message".into(),
                silent: false,
            };
            self.notifier.add(note, Instant::now());
            return;
        }
        let note = Note {
            id: message.id,
            chat_id,
            chat: self.chats.title(chat_id).unwrap_or("Message").to_string(),
            text: self.notification_text(message),
            silent: false,
        };
        self.notifier.add(note, Instant::now());
    }

    /// "Alice: see you at 5" in groups; just the text in chats with one
    /// person, where the chat's name says who.
    fn notification_text(&self, message: &crate::meta::Message) -> String {
        if let Some(service) = crate::service::Service::of(message.service.as_deref()) {
            return crate::service::text(&service.sentence());
        }
        let text = crate::chats::content_text(message);
        let private = self
            .chats
            .get(message.chat_id)
            .is_some_and(|c| c.is_private);
        match self.users.get(&message.sender_id) {
            Some(name) if !private => format!("{name}: {text}"),
            _ => text,
        }
    }

    fn on_key(&mut self, key: KeyEvent) {
        if key.kind != KeyEventKind::Press {
            return;
        }
        let ctrl = key.modifiers.contains(KeyModifiers::CONTROL);
        // Not while typing: in Insert mode, the prompt, or the picker's search.
        if ctrl
            && key.code == KeyCode::Char('c')
            && self.focus != Focus::Input
            && self.prompt.is_none()
            && self.picker.is_none()
        {
            self.quit();
            return;
        }
        self.status = None;
        match self.screen {
            Screen::Login(_) => self.on_login_key(key),
            Screen::Main if self.confirm.is_some() => self.on_confirm_key(key),
            Screen::Main if self.photo_view.is_some() => self.on_viewer_key(key),
            Screen::Main if self.settings_menu.is_some() => self.on_settings_key(key, ctrl),
            Screen::Main if self.delete_menu.is_some() => self.on_delete_key(key),
            Screen::Main if self.react_menu.is_some() => self.on_react_key(key, ctrl),
            Screen::Main if self.menu.is_some() => self.on_menu_key(key),
            Screen::Main if self.picker.is_some() => self.on_picker_key(key, ctrl),
            Screen::Main if self.resizing.is_some() => self.on_resize_key(key, ctrl),
            Screen::Main if self.prompt.is_some() => self.on_prompt_key(key, ctrl),
            Screen::Main if self.focus == Focus::Input => self.on_insert_key(key, ctrl),
            Screen::Main => self.on_normal_key(key, ctrl),
        }
        // Suggestions are part of Insert mode, and close with it.
        if self.focus != Focus::Input {
            self.completion = None;
        }
        // Writing or a popup means you moved on: a chat still being looked
        // up won't open over it.
        if self.busy() {
            self.finding = None;
        }
    }

    fn on_login_key(&mut self, key: KeyEvent) {
        let ready = !self.ready_networks().is_empty();
        let Screen::Login(login) = &mut self.screen else {
            return;
        };
        match login.step {
            LoginStep::Connecting | LoginStep::LoggingOut => {
                if key.code == KeyCode::Char('q') {
                    self.quit();
                }
            }
            LoginStep::Choose { selected } => {
                let last = Network::ALL.len() - 1;
                match key.code {
                    KeyCode::Char('j') | KeyCode::Down | KeyCode::Tab => {
                        login.step = LoginStep::Choose {
                            selected: (selected + 1).min(last),
                        };
                    }
                    KeyCode::Char('k') | KeyCode::Up | KeyCode::BackTab => {
                        login.step = LoginStep::Choose {
                            selected: selected.saturating_sub(1),
                        };
                    }
                    KeyCode::Enter | KeyCode::Char('l') => {
                        let network = Network::ALL[selected.min(last)];
                        let from_main = login.from_main;
                        if network.links() {
                            self.start_link(network, None, from_main);
                            return;
                        }
                        let mut cookies = Login::new(LoginStep::Cookies { network });
                        cookies.from_main = from_main;
                        self.screen = Screen::Login(Box::new(cookies));
                    }
                    KeyCode::Esc if login.from_main && ready => self.screen = Screen::Main,
                    KeyCode::Char('q') => self.quit(),
                    _ => {}
                }
            }
            LoginStep::Cookies { network } => match key.code {
                KeyCode::Esc => self.back_to_choose(network),
                KeyCode::Enter => {
                    let value = login.input.lines().concat().trim().to_string();
                    if value.is_empty() || login.busy {
                        return;
                    }
                    let missing: Vec<&str> = network
                        .cookie_names()
                        .iter()
                        .copied()
                        .filter(|name| !value.contains(name))
                        .collect();
                    if !missing.is_empty() {
                        login.error =
                            Some(format!("These cookies are missing: {}", missing.join(", ")));
                        return;
                    }
                    login.busy = true;
                    login.error = None;
                    self.login_attempt += 1;
                    self.meta.login_cookies(
                        network,
                        value,
                        self.browser.clone(),
                        self.login_attempt,
                    );
                }
                _ => {
                    login.input.input(key);
                }
            },
            LoginStep::Link { network, ref how } => {
                let typing = matches!(how, Linking::Phone);
                let phone = match how {
                    Linking::Pairing { phone, .. } => Some(phone.clone()),
                    _ => None,
                };
                self.on_link_key(key, network, typing, phone);
            }
        }
    }

    /// Keys while linking a device: `p` for the phone number instead of a
    /// QR code, Enter for a fresh code once one ran out, Esc to give up.
    /// `typing` is the phone number step; `phone` the number a code was
    /// asked for.
    fn on_link_key(
        &mut self,
        key: KeyEvent,
        network: Network,
        typing: bool,
        phone: Option<String>,
    ) {
        let Screen::Login(login) = &mut self.screen else {
            return;
        };
        let from_main = login.from_main;
        match key.code {
            KeyCode::Esc => {
                // A cancel names its attempt: for a link that already ended
                // it does nothing, and one still waiting must stop, whatever
                // the screen shows. On the number step, the `p` that opened
                // it has cancelled already.
                if !typing {
                    self.meta.cancel_login(network, self.login_attempt);
                }
                self.back_to_choose(network);
            }
            KeyCode::Enter if typing => {
                let number = login.input.lines().concat().trim().to_string();
                if number.is_empty() {
                    return;
                }
                self.start_link(network, Some(number), from_main);
            }
            _ if typing => {
                login.input.input(key);
            }
            KeyCode::Char('p') => {
                // The codes shown so far stop working.
                self.meta.cancel_login(network, self.login_attempt);
                let mut ask = Login::new(LoginStep::Link {
                    network,
                    how: Linking::Phone,
                });
                if let Some(number) = phone {
                    ask.input.insert_str(number);
                }
                ask.from_main = from_main;
                self.screen = Screen::Login(Box::new(ask));
            }
            KeyCode::Enter if !login.busy => self.start_link(network, phone, from_main),
            KeyCode::Char('q') => self.quit(),
            _ => {}
        }
    }

    /// Asks the helper to link `network` as a new device: by QR codes, or
    /// with the account's phone number by a code to type on the phone. The
    /// screen waits for the code.
    fn start_link(&mut self, network: Network, phone: Option<String>, from_main: bool) {
        let how = match &phone {
            Some(number) => Linking::Pairing {
                phone: number.clone(),
                code: None,
            },
            None => Linking::Qr(None),
        };
        let mut login = Login::new(LoginStep::Link { network, how });
        login.from_main = from_main;
        login.busy = true;
        self.screen = Screen::Login(Box::new(login));
        self.login_attempt += 1;
        self.meta.login_link(network, phone, self.login_attempt);
    }

    /// Back to picking a network, on the one left.
    fn back_to_choose(&mut self, network: Network) {
        let Screen::Login(login) = &self.screen else {
            return;
        };
        let selected = Network::ALL.iter().position(|&n| n == network).unwrap_or(0);
        let mut choose = Login::new(LoginStep::Choose { selected });
        choose.from_main = login.from_main;
        self.screen = Screen::Login(Box::new(choose));
    }

    /// A code to show while a link waits; one for another network, or once
    /// nothing waits, is old news.
    fn on_login_code(
        &mut self,
        network: Network,
        qr: Option<String>,
        pairing: Option<String>,
        expires: i64,
    ) {
        let Screen::Login(login) = &mut self.screen else {
            return;
        };
        if !login.busy {
            return;
        }
        let LoginStep::Link { network: n, how } = &mut login.step else {
            return;
        };
        if *n != network {
            return;
        }
        match how {
            Linking::Qr(code) => {
                if let Some(qr) = qr.filter(|qr| !qr.is_empty() && qr.len() <= MAX_QR) {
                    *code = Some(qr);
                }
            }
            Linking::Pairing { code, .. } => {
                if let Some(pairing) = pairing.as_deref().and_then(pairing_code) {
                    *code = Some((pairing, expires));
                }
            }
            Linking::Phone => {}
        }
    }

    /// `:login`: the login screen, to add the other network or log in again.
    fn ask_to_log_in(&mut self) {
        let selected = Network::ALL
            .iter()
            .position(|n| !self.ready_networks().contains(n))
            .unwrap_or(0);
        let mut login = Login::new(LoginStep::Choose { selected });
        login.from_main = true;
        self.screen = Screen::Login(Box::new(login));
    }

    /// Normal mode: every key is a command, nothing is typed.
    fn on_normal_key(&mut self, key: KeyEvent, ctrl: bool) {
        let pending_g = std::mem::take(&mut self.pending_g);
        if self.full_screen() && self.on_full_screen_key(key, ctrl) {
            return;
        }
        if self.focus == Focus::Messages
            && self.comments_focus
            && self.comments.is_some()
            && self.on_comments_key(key, ctrl, pending_g)
        {
            return;
        }
        if self.focus == Focus::Messages
            && self.feed.is_some()
            && self.on_feed_key(key, ctrl, pending_g)
        {
            return;
        }
        // `h` and `l` go toward the pane on that side of the screen, so they
        // swap when the chat list is on the right.
        let (to_chat, to_list) = match self.settings.chat_list_side {
            Side::Left => ('l', 'h'),
            Side::Right => ('h', 'l'),
        };
        // Motions work the same in both panes. In the message pane, down
        // (`j`, `G`) is newer and up (`k`, `gg`) is older, as on screen.
        let motion = match key.code {
            KeyCode::Char('j') | KeyCode::Down => Some(1),
            KeyCode::Char('k') | KeyCode::Up => Some(-1),
            KeyCode::Char('d') if ctrl => Some(HALF_PAGE),
            KeyCode::Char('u') if ctrl => Some(-HALF_PAGE),
            KeyCode::Char('g') if pending_g => Some(isize::MIN),
            KeyCode::Char('G') => Some(isize::MAX),
            _ => None,
        };
        if let Some(delta) = motion {
            match self.focus {
                Focus::Chats => self.move_chat_cursor(delta),
                Focus::Messages => self.move_message_cursor(delta),
                Focus::Input => {}
            }
            return;
        }
        match (self.focus, key.code) {
            // Before `r`, which replies.
            (_, KeyCode::Char('r')) if ctrl => {
                self.resizing = Some(Resizing::List(self.settings.chat_list_width));
            }
            (_, KeyCode::Char('g')) => self.pending_g = true,
            (_, KeyCode::Char('q')) => self.quit(),
            (_, KeyCode::Char('H')) => self.toggle_highlight(),
            (_, KeyCode::Char('?')) => {
                if let Some(dir) = self.themes.dir.clone() {
                    self.themes = Themes::load(&dir);
                }
                self.use_saved_theme();
                if let Some(dir) = self.sounds.dir.clone() {
                    self.sounds = Sounds::load(&dir);
                }
                self.use_saved_sound();
                self.settings_menu = Some(SettingsMenu::new(HelpTab::Shortcuts, &self.settings));
            }
            (_, KeyCode::Char(':')) => self.open_prompt(PromptKind::Command),
            (_, KeyCode::Char('o')) if ctrl => self.jump(true),
            // Before `i`, which writes. Terminals without the kitty keyboard
            // protocol send Ctrl-i as Tab, which goes forward in the chat too.
            (_, KeyCode::Char('i')) if ctrl => self.jump(false),
            (Focus::Messages, KeyCode::Tab) => self.jump(false),
            (Focus::Chats, KeyCode::Tab) => self.switch_list(1),
            (Focus::Chats, KeyCode::BackTab) => self.switch_list(-1),
            (_, KeyCode::Char('s')) => self.picker = Some(ChatPicker::new()),
            (Focus::Chats, KeyCode::Char('/')) => self.open_prompt(PromptKind::Chats),
            // Esc stops a lookup with `s`. In the chat pane it then ends an
            // edit, then removes the files, then ends a reply, before it
            // leaves the pane.
            (_, KeyCode::Esc) if self.finding.is_some() => self.finding = None,
            (Focus::Chats, KeyCode::Esc) => self.chats.set_filter(""),
            (Focus::Chats, KeyCode::Char('m')) if !ctrl => self.toggle_mute(),
            (Focus::Messages, KeyCode::Esc)
                if self.open.as_ref().is_some_and(|o| o.editing.is_some()) =>
            {
                self.end_edit();
            }
            (Focus::Messages, KeyCode::Esc)
                if self
                    .open
                    .as_ref()
                    .is_some_and(|o| !o.attachments.is_empty()) =>
            {
                if let Some(open) = self.open.as_mut() {
                    open.attachments.clear();
                    open.dropped = None;
                }
            }
            (Focus::Messages, KeyCode::Esc)
                if self.open.as_ref().is_some_and(|o| o.reply.is_some()) =>
            {
                if let Some(open) = self.open.as_mut() {
                    open.reply = None;
                }
            }
            (Focus::Messages, KeyCode::Char('r')) => self.reply_to_selected(),
            (Focus::Messages, KeyCode::Char('e')) => self.edit_selected(),
            (Focus::Messages, KeyCode::Char('y')) => self.copy_selected(),
            (Focus::Messages, KeyCode::Char('a')) => self.open_prompt(PromptKind::Attach),
            (Focus::Messages, KeyCode::Char('p')) => self.paste_clipboard(),
            (Focus::Messages, KeyCode::Char('d')) if pending_g => self.go_to_replied(),
            (Focus::Messages, KeyCode::Char('d')) => self.open_delete_menu(),
            (Focus::Messages, KeyCode::Char('R')) => self.open_react_menu(),
            (Focus::Messages, KeyCode::Char('X')) => self.remove_reaction(),
            (Focus::Chats, KeyCode::Enter) => self.open_selected_chat(),
            (Focus::Chats, KeyCode::Char(c)) if c == to_chat => self.open_selected_chat(),
            (Focus::Chats, KeyCode::Char('i')) => {
                self.open_selected_chat();
                self.start_writing();
            }
            (Focus::Messages, KeyCode::Char('i')) => self.start_writing(),
            (Focus::Messages, KeyCode::Enter) => self.open_selected_message(),
            (Focus::Messages, KeyCode::Esc) => self.focus = Focus::Chats,
            (Focus::Messages, KeyCode::Char(c)) if c == to_list => self.focus = Focus::Chats,
            _ => {}
        }
    }

    /// Insert mode: keys type into the composer.
    fn on_insert_key(&mut self, key: KeyEvent, ctrl: bool) {
        let alt = key.modifiers.contains(KeyModifiers::ALT);
        let shift = key.modifiers.contains(KeyModifiers::SHIFT);
        // While there are suggestions, Tab takes one and the arrows move
        // through them. Enter still sends: a suggestion taken by accident
        // would change the message.
        if let Some(completion) = self.completion.as_mut().filter(|c| !c.items.is_empty()) {
            match key.code {
                KeyCode::Tab => {
                    self.accept_completion();
                    return;
                }
                KeyCode::Up | KeyCode::BackTab => {
                    completion.move_by(-1);
                    return;
                }
                KeyCode::Down => {
                    completion.move_by(1);
                    return;
                }
                KeyCode::Char('p') if ctrl => {
                    completion.move_by(-1);
                    return;
                }
                KeyCode::Char('n') if ctrl => {
                    completion.move_by(1);
                    return;
                }
                _ => {}
            }
        }
        match key.code {
            // Ctrl-c leaves Insert mode like in vim, instead of quitting mid-sentence.
            KeyCode::Esc => self.leave_insert(),
            KeyCode::Char('c') if ctrl => self.leave_insert(),
            // Shift-Enter only arrives on terminals with the kitty keyboard protocol;
            // Alt-Enter and Ctrl-j work everywhere.
            KeyCode::Enter if alt || shift => {
                self.composer.insert_newline();
                self.on_composer_edit();
            }
            KeyCode::Char('j') if ctrl => {
                self.composer.insert_newline();
                self.on_composer_edit();
            }
            KeyCode::Enter => self.send(),
            // Ctrl-v pages down in the text area, which a composer this
            // small doesn't need.
            KeyCode::Char('v') if ctrl => self.paste_clipboard(),
            KeyCode::Char('z') if ctrl => self.undo_drop(),
            // Ctrl-u deletes back to the start of the line, as in a shell,
            // instead of the text area's undo.
            KeyCode::Char('u') if ctrl => {
                if self.composer.delete_line_by_head() {
                    self.on_composer_edit();
                }
            }
            _ => {
                if self.composer.input(key) {
                    self.on_composer_edit();
                }
            }
        }
        self.update_completion();
    }

    /// Looks at the word before the cursor after each key in Insert mode,
    /// and suggests emoji for `:smi`.
    fn update_completion(&mut self) {
        let DataCursor(row, col) = self.composer.cursor();
        let word = self
            .composer
            .lines()
            .get(row)
            .and_then(|line| complete::word_at(line, col));
        let Some(word) = word else {
            self.completion = None;
            return;
        };
        if self.completion.as_ref().is_some_and(|c| c.word == word) {
            return;
        }
        let completion = self
            .completion
            .get_or_insert_with(|| Completion::new(word.clone()));
        completion.retype(word.clone());
        completion.set_items(complete::emoji(&word.query));
    }

    /// Tab with suggestions up: puts the one under the cursor in place of
    /// the word being typed.
    fn accept_completion(&mut self) {
        let Some(completion) = self.completion.take() else {
            return;
        };
        let Some(suggestion) = completion.current() else {
            return;
        };
        for _ in 0..completion.word.chars {
            self.composer.delete_char();
        }
        self.composer.insert_str(&suggestion.insert);
        self.on_composer_edit();
    }

    /// `i`: Insert mode, in a chat you can write in.
    fn start_writing(&mut self) {
        let Some(open) = &self.open else {
            return;
        };
        if let Some(why) = self.cant_send(open.chat_id) {
            self.status = Some(why);
            return;
        }
        self.focus = Focus::Input;
    }

    /// Why nothing can be sent in a chat now, if it can't.
    fn cant_send(&self, chat_id: i64) -> Option<String> {
        let chat = self.chats.get(chat_id)?;
        let network = chat.network;
        if !self.ready_networks().contains(&network) {
            return Some(format!("{} isn't connected", network.name()));
        }
        (!chat.can_send).then(|| "You can't write in this chat".into())
    }

    fn leave_insert(&mut self) {
        self.focus = Focus::Messages;
        self.set_typing(false);
    }

    /// You're typing while the composer has text, and stopped once it's empty.
    /// An edit to a sent message isn't typing.
    fn on_composer_edit(&mut self) {
        let editing = self.open.as_ref().is_some_and(|o| o.editing.is_some());
        let typing = !editing && self.composer.lines().iter().any(|l| !l.trim().is_empty());
        self.set_typing(typing);
    }

    /// Tells the open chat whether you're typing: again every
    /// [`TYPING_EVERY`] while you are, and once when you stop. The only
    /// place typing goes out.
    fn set_typing(&mut self, typing: bool) {
        let chat = self.open.as_ref().map(|o| o.chat_id);
        if typing && let Some(chat_id) = chat {
            let told = self
                .typing
                .is_some_and(|(at, when)| at == chat_id && when.elapsed() < TYPING_EVERY);
            if !told {
                self.meta.send_typing(chat_id, true);
                self.typing = Some((chat_id, Instant::now()));
            }
        } else if let Some((chat_id, _)) = self.typing.take() {
            self.meta.send_typing(chat_id, false);
        }
    }

    fn send(&mut self) {
        if self.open.as_ref().is_some_and(|o| o.editing.is_some()) {
            self.save_edit();
            return;
        }
        let Some(chat_id) = self.open.as_ref().map(|o| o.chat_id) else {
            return;
        };
        let cant_send = self.cant_send(chat_id);
        let Some(open) = self.open.as_mut() else {
            return;
        };
        let text = self.composer.lines().join("\n");
        let text = text.trim().to_string();
        if text.is_empty() && open.attachments.is_empty() {
            return;
        }
        if let Some(why) = cant_send {
            self.status = Some(why);
            return;
        }
        if let Some(changed) = open.attachments.iter().find(|a| a.swapped()) {
            self.status = Some(format!(
                "{} changed since it was attached. Drop the files (Esc in Normal mode) and attach it again",
                changed.name
            ));
            return;
        }
        let reply_to = open.reply.take().map(|r| r.id);
        if open.attachments.is_empty() {
            self.meta.send_text(open.chat_id, text, reply_to);
        } else {
            // What was checked, not only where: the helper reads the files
            // later, in another process, and sends none unless each is still
            // the one listed.
            let files = open.attachments.iter().map(Attachment::to_send).collect();
            self.meta.send_files(open.chat_id, files, text, reply_to);
            open.attachments.clear();
            open.dropped = None;
        }
        self.composer = new_composer();
        // The message arriving ends the typing status for everyone.
        self.typing = None;
        if self.settings.normal_after_send {
            self.focus = Focus::Messages;
        }
        // Jump to the bottom to watch it arrive.
        self.jump_to_newest();
    }

    fn open_prompt(&mut self, kind: PromptKind) {
        let input = prompt_input("");
        // Files go into a chat, so one has to be open.
        if kind == PromptKind::Attach && !self.can_attach() {
            return;
        }
        self.prompt = Some(Prompt {
            kind,
            input,
            completions: Vec::new(),
            tabbed: None,
            previous_filter: self.chats.filter().to_string(),
            previous_selected: self.selected,
        });
    }

    /// The prompt takes all keys while it's up.
    fn on_prompt_key(&mut self, key: KeyEvent, ctrl: bool) {
        let Some(prompt) = self.prompt.as_mut() else {
            return;
        };
        match key.code {
            KeyCode::Esc => self.close_prompt(false),
            KeyCode::Char('c') if ctrl => self.close_prompt(false),
            // Ctrl-m and Ctrl-j would add a line to the one-line prompt.
            KeyCode::Enter => self.close_prompt(true),
            KeyCode::Char('m' | 'j') if ctrl => self.close_prompt(true),
            // Like vim, backspace on an empty prompt closes it.
            KeyCode::Backspace if prompt.query().is_empty() => self.close_prompt(false),
            KeyCode::Tab if prompt.kind == PromptKind::Attach => {
                let completion = attach::complete(&prompt.query());
                prompt.input = prompt_input(&completion.text);
                prompt.completions = completion.matches;
            }
            KeyCode::Tab if prompt.kind == PromptKind::Command => prompt.complete_command(1),
            KeyCode::BackTab if prompt.kind == PromptKind::Command => prompt.complete_command(-1),
            _ => {
                prompt.input.input(key);
                prompt.completions.clear();
                prompt.tabbed = None;
                self.on_prompt_edit();
            }
        }
    }

    /// The chat list filters as you type, with the cursor on the top match.
    fn on_prompt_edit(&mut self) {
        let Some(prompt) = self.prompt.as_ref() else {
            return;
        };
        if prompt.kind == PromptKind::Chats {
            self.chats.set_filter(prompt.query().trim());
            self.chats.refresh();
            self.selected = self.chats.ids().first().copied();
        }
    }

    /// Enter (`submit`) runs it; Esc puts things back as they were.
    fn close_prompt(&mut self, submit: bool) {
        let Some(prompt) = self.prompt.take() else {
            return;
        };
        let query = prompt.query().trim().to_string();
        match prompt.kind {
            PromptKind::Chats => {
                if submit && (query.is_empty() || !self.chats.ids().is_empty()) {
                    return;
                }
                if submit {
                    self.status = Some(format!("No chats match \"{query}\""));
                }
                self.chats.set_filter(&prompt.previous_filter);
                self.selected = prompt.previous_selected;
            }
            PromptKind::Attach if !submit || query.is_empty() => {}
            PromptKind::Attach => {
                let path = attach::expand_home(&query);
                // Not even looked at, as with a paste: that would hand the
                // server the user's Windows login hash.
                if attach::on_another_machine(&path) {
                    self.status = Some("Files on another machine can't be attached".into());
                    return;
                }
                // A file dropped on the prompt comes quoted.
                let paths = match attach::pasted_paths(&query) {
                    Some(paths) if !path.exists() => paths,
                    _ => vec![path],
                };
                self.attach(paths, None);
            }
            PromptKind::Command if !submit || query.is_empty() => {}
            PromptKind::Command => match Command::parse(&query) {
                Some(Command::Login) => self.ask_to_log_in(),
                Some(Command::Logout) => self.ask_to_log_out(),
                Some(Command::Following) => self.show_threads_feed(true),
                Some(Command::ForYou) => self.show_threads_feed(false),
                None => self.status = Some(format!("Not a command: {query}")),
            },
        }
    }

    /// Sends a read receipt for the newest incoming message once you can
    /// see it: the chat pane and the terminal window have focus, and the view
    /// is on the newest message (see [`App::watching`]). It marks the whole
    /// chat read. The only place read receipts go out.
    fn mark_seen(&mut self) {
        let watched = self.open.as_ref().map(|o| o.chat_id);
        if !watched.is_some_and(|id| self.watching(id)) {
            return;
        }
        // A message request isn't accepted yet: a receipt would tell a
        // stranger the account is here and read them before the user chose
        // to answer. It goes once the chat is accepted (replying accepts it).
        if watched.is_some_and(|id| self.chats.get(id).is_some_and(|c| c.request)) {
            return;
        }
        let Some(open) = self.open.as_mut() else {
            return;
        };
        let newest = open
            .messages
            .iter()
            .rev()
            .find(|(_, m)| !m.outgoing && m.state == SendState::Sent);
        if let Some((&id, _)) = newest
            && id > open.seen
        {
            open.seen = id;
            self.meta.mark_read(open.chat_id, id);
        }
    }

    /// How long since the last key, by whichever clock says longer: the
    /// monotonic one doesn't count time the computer spent asleep.
    fn idle(&self) -> Duration {
        let wall = SystemTime::now()
            .duration_since(self.last_input_wall)
            .unwrap_or_default();
        self.last_input.elapsed().max(wall)
    }

    /// The chat is open in front of the user, on its newest message, so new
    /// ones are seen as they arrive. Where the terminal never says when its
    /// window loses focus (tmux without `focus-events`, a detached session),
    /// no key press for [`IDLE_AFTER`] counts as the user being away, so
    /// messages aren't marked read, and do notify, while nobody is there.
    /// A window the terminal says has focus gets [`AWAY_AFTER`].
    fn watching(&self, chat_id: i64) -> bool {
        matches!(self.screen, Screen::Main)
            && self.present()
            && matches!(self.focus, Focus::Messages | Focus::Input)
            && !self.popup_over_chat()
            && self.open.as_ref().is_some_and(|o| {
                o.chat_id == chat_id
                    && o.at_newest
                    && o.selected.is_none()
                    // Going to an older message: what arrives meanwhile
                    // isn't what's about to be on screen.
                    && !matches!(o.loading, Some(Page::Around(_)))
            })
    }

    /// Someone is at tuimeta: its window has focus, and a key was pressed
    /// recently enough (see [`App::watching`]).
    fn present(&self) -> bool {
        self.terminal_focused && self.idle() < self.away_after()
    }

    /// How long without a key counts as away: [`IDLE_AFTER`] where the
    /// terminal never says when its window loses focus, [`AWAY_AFTER`]
    /// where it does.
    fn away_after(&self) -> Duration {
        if self.focus_reported {
            AWAY_AFTER
        } else {
            IDLE_AFTER
        }
    }

    /// The user would see a new message in this chat without being told:
    /// tuimeta's window has focus. Where the terminal never reports focus,
    /// only the chat being read counts.
    fn sees(&self, chat_id: i64) -> bool {
        // The photo viewer hides every chat.
        if self.photo_view.is_some() {
            return false;
        }
        if self.focus_reported {
            self.terminal_focused
        } else {
            self.watching(chat_id)
        }
    }

    /// Plays the sound for a new message as it comes, whatever its
    /// notification does: unless the user sees it come and didn't ask for
    /// one then. A burst rings once, at most once per [`RING_GAP`]. Over SSH
    /// it would play on the other computer, so it doesn't.
    fn ring_for(&mut self, seen: bool) {
        let now = Instant::now();
        if (seen && !self.settings.sound_when_focused)
            || self.over_ssh
            || self.rang_at.is_some_and(|at| now < at + RING_GAP)
        {
            return;
        }
        let Some(sound) = &self.sound else {
            return;
        };
        self.rang_at = Some(now);
        self.ringer.ring(Arc::clone(sound));
    }

    fn send_notification(&mut self) {
        let Some(mut alert) = self.notifier.due(Instant::now()) else {
            return;
        };
        // tuimeta's own sound went as the messages came (`App::ring_for`),
        // so the terminal makes none of its own.
        if self.sound.is_some() && !self.over_ssh {
            alert.silent = true;
        }
        self.notifications_sent += 1;
        let id = self.notifications_sent;
        if let Some(code) = notify::escape(self.notify_with, &alert, id, self.in_tmux) {
            notify::send(&code);
        }
    }

    fn set_unread_chats(&mut self, count: i32) {
        if count != self.unread_chats {
            self.unread_chats = count;
            notify::send(&notify::title(count));
        }
    }

    /// The chat a command is about: the selected one in the list, or else
    /// the open one.
    fn command_chat(&self) -> Option<i64> {
        match self.focus {
            Focus::Chats => self.selected,
            _ => self
                .open
                .as_ref()
                .map(|o| o.chat_id)
                .or(self.feed.as_ref().and_then(|f| chats::feed_id(f.network))),
        }
    }

    /// `:logout` asks first, naming the network: the one of the chat the
    /// command is about, or the only one logged in.
    fn ask_to_log_out(&mut self) {
        let ready = self.ready_networks();
        let network = self
            .command_chat()
            .and_then(|id| self.chats.network(id))
            .or_else(|| (ready.len() == 1).then(|| ready[0]))
            .or_else(|| {
                // In trouble, but still holding a session.
                Network::ALL.into_iter().find(|n| {
                    self.accounts
                        .get(n)
                        .is_some_and(|a| a.state != AccountState::LoggedOut)
                })
            });
        let Some(network) = network else {
            self.status = Some("Not logged in anywhere".into());
            return;
        };
        let name = network.name();
        // Logout is local on Messenger, Instagram and Threads: tuimeta drops the
        // connection and deletes what it keeps, but never ends the web
        // session the cookies belong to, so the browser you copied them from
        // stays logged in. On WhatsApp tuimeta is a linked device of its
        // own, which it unlinks.
        let ends = match network {
            Network::Messenger => vec![
                "tuimeta disconnects and deletes what it keeps for".to_string(),
                "Messenger on this computer, its encrypted-chat".into(),
                "device included. Your Facebook session stays logged".into(),
                "in: end it yourself in the browser, or in Facebook's".into(),
                "list of logged-in devices.".into(),
            ],
            Network::Instagram => vec![
                "tuimeta disconnects and deletes what it keeps for".to_string(),
                "Instagram on this computer. The session itself ends".into(),
                "only once you log out in the browser you copied the".into(),
                "cookies from.".into(),
            ],
            Network::Threads => vec![
                "tuimeta deletes what it keeps for Threads on this".to_string(),
                "computer. The session itself ends only once you log".into(),
                "out in the browser you copied the cookies from.".into(),
            ],
            Network::WhatsApp => vec![
                "tuimeta unlinks itself from your WhatsApp account and".to_string(),
                "deletes what it keeps for WhatsApp on this computer,".into(),
                "the messages it kept included. Your phone and your".into(),
                "other linked devices stay logged in.".into(),
            ],
        };
        self.confirm = Some(Confirm::new(
            format!("Log out of {name}?"),
            ends,
            Confirmed::Logout(network),
        ));
    }

    /// `:logout`, once asked: the helper ends the session and deletes what
    /// it keeps; the account then says it's logged out.
    fn log_out(&mut self, network: Network) {
        self.meta.log_out(network);
        if self.ready_networks() == [network] {
            self.screen = login_screen(LoginStep::LoggingOut);
        }
    }

    /// Puts the cursor on a message, loading the history around it if it's
    /// not loaded.
    fn jump_to_message(&mut self, id: i64) {
        let Some(open) = self.open.as_mut() else {
            return;
        };
        if open.messages.contains_key(&id) {
            open.selected = Some(id);
            // A jump still loading elsewhere would move the cursor away.
            if matches!(open.loading, Some(Page::Around(_))) {
                open.loading = None;
            }
            return;
        }
        // The page replaces the loaded messages when it arrives.
        let page = Page::Around(id);
        open.loading = Some(page);
        self.meta.load_history(open.chat_id, page, HISTORY_PAGE);
    }

    /// Back to following the newest message, reloading if an older part of
    /// the chat is shown.
    fn jump_to_newest(&mut self) {
        let Some(open) = self.open.as_mut() else {
            return;
        };
        open.selected = None;
        if open.at_newest {
            return;
        }
        open.messages.clear();
        open.scroll = None;
        open.at_newest = true;
        open.all_loaded = false;
        open.loading = None;
        self.load_older_messages();
    }

    fn open_selected_chat(&mut self) {
        if let Some(chat_id) = self.selected {
            self.open_chat(chat_id);
        }
    }

    /// Opens a chat. Ctrl-o comes back to the chat before.
    fn open_chat(&mut self, chat_id: i64) {
        if let Some(here) = self.here().filter(|h| h.chat_id != chat_id) {
            self.jumps.leave(here);
        }
        self.enter_chat(chat_id);
    }

    /// Where the cursor is: the open chat, and the message it's on.
    fn here(&self) -> Option<Jump> {
        let feed = self
            .feed
            .as_ref()
            .and_then(|f| chats::feed_id(f.network))
            .map(|chat_id| Jump {
                chat_id,
                message_id: None,
            });
        self.open
            .as_ref()
            .map(|o| Jump {
                chat_id: o.chat_id,
                message_id: o.selected,
            })
            .or(feed)
    }

    /// Ctrl-o (`back`) or Ctrl-i: to the chat or message left before, or
    /// forward again to where Ctrl-o came from.
    fn jump(&mut self, back: bool) {
        let here = self.here();
        match self.jumps.go(back, here) {
            Some(to) => {
                if self.open.as_ref().is_none_or(|o| o.chat_id != to.chat_id) {
                    self.enter_chat(to.chat_id);
                }
                if self.open.as_ref().is_none_or(|o| o.chat_id != to.chat_id) {
                    return;
                }
                self.focus = Focus::Messages;
                match to.message_id {
                    Some(id) => self.jump_to_message(id),
                    None => self.jump_to_newest(),
                }
            }
            None if back => self.status = Some("Nothing to go back to".into()),
            None => self.status = Some("Nothing to go forward to".into()),
        }
    }

    /// [`App::open_chat`], without Ctrl-o coming back to the chat before.
    fn enter_chat(&mut self, chat_id: i64) {
        self.close_chat_popups();
        if self.chats.get(chat_id).is_none() {
            self.status = Some("That chat isn't in the list any more".into());
            return;
        }
        if self.selected != Some(chat_id) {
            // The list's cursor goes to it, even if the filter hid it.
            if !self.chats.ids().contains(&chat_id) {
                self.chats.set_filter("");
            }
            self.selected = Some(chat_id);
        }
        self.focus = Focus::Messages;
        if let Some(network) = chats::feed_network(chat_id) {
            self.open_feed(network);
            return;
        }
        if self.open.as_ref().is_some_and(|o| o.chat_id == chat_id) {
            return;
        }
        self.leave_chat();
        self.chats.opened(chat_id);
        self.open = Some(OpenChat::new(chat_id));
        self.composer = new_composer();
        self.load_older_messages();
    }

    /// Popups about a message of the chat before are no use in another.
    fn close_chat_popups(&mut self) {
        // A lookup still on its way would open another chat over this one.
        self.finding = None;
        self.menu = None;
        self.delete_menu = None;
        self.react_menu = None;
        self.photo_view = None;
        self.close_comments();
    }

    /// Leaves the chat open: you stop typing in it.
    fn leave_chat(&mut self) {
        self.set_typing(false);
        // A paste on its way was for these messages.
        self.pasting = false;
        self.changed.clear();
        if self.open.take().is_some() {
            self.images.clear();
        }
        // Its video stops; the posts are kept to come back to, but not a
        // linked post's: the feed's row opens the feed.
        if let Some(feed) = self.feed.take() {
            self.stop_video();
            self.close_comments();
            self.images.clear();
            if feed.linked.is_none() {
                self.feeds_kept.insert(feed.network, feed);
            } else if let Some(request) = feed.request {
                // Nobody waits for the post any more: the helper needn't
                // ask the network for it.
                self.meta.cancel(request);
            }
        }
    }

    fn open_selected_message(&mut self) {
        let Some(open) = self.open.as_ref() else {
            return;
        };
        let Some((&id, msg)) = open
            .cursor_id()
            .and_then(|id| open.messages.get_key_value(&id))
        else {
            return;
        };
        let file = msg.file.clone().map(|file| file_target(id, msg, file));
        let mut targets: Vec<Target> = file.into_iter().collect();
        targets.extend(msg.links.iter().cloned().map(Target::Link));
        // A message that came in or changed a moment ago, maybe just as the
        // key went down, isn't what the user meant to open.
        let just_changed = self
            .changed
            .get(&id)
            .is_some_and(|at| at.elapsed() < CONFIRM_GRACE);
        match targets.len() {
            0 => self.status = Some("Nothing to open in this message".into()),
            _ if just_changed => {
                self.status =
                    Some("It just came in or changed: press Enter again to open it".into());
            }
            1 => self.open_target(targets.remove(0)),
            _ => self.show_menu(id, MenuAction::Open, targets),
        }
    }

    /// The menu of what to open or copy in message `id`, the first item
    /// picked.
    fn show_menu(&mut self, id: i64, action: MenuAction, targets: Vec<Target>) {
        self.menu = Some(PickMenu {
            action,
            targets,
            selected: 0,
        });
        self.menu_message = Some(id);
    }

    /// `r`: answer the message under the cursor. Goes straight to Insert mode,
    /// keeping whatever was already typed.
    fn reply_to_selected(&mut self) {
        // Its draft and reply come back, and the new reply replaces that one.
        self.end_edit();
        let Some(open) = self.open.as_mut() else {
            return;
        };
        let Some((&id, msg)) = open
            .cursor_id()
            .and_then(|id| open.messages.get_key_value(&id))
        else {
            return;
        };
        if msg.state != SendState::Sent {
            self.status = Some("Can't reply to a message that isn't sent".into());
            return;
        }
        open.reply = Some(Replied::new(id, msg));
        self.focus = Focus::Input;
    }

    /// `e`: edits the message under the cursor in the composer, while the
    /// network allows. Formatting the text had is lost, so it asks first.
    fn edit_selected(&mut self) {
        let Some(open) = &self.open else {
            return;
        };
        let Some(id) = open.edit_target() else {
            return;
        };
        if let Some(why) = open.cant_edit(id, unix_now()) {
            self.status = Some(why.into());
            return;
        }
        let Some(msg) = open.messages.get(&id) else {
            return;
        };
        let text = msg.source_text.clone();
        if msg.formatted {
            self.confirm = Some(Confirm::new(
                "Edit and lose its formatting?",
                vec![
                    "It has formatting (bold, links behind words…)".into(),
                    "that an edit, sent as plain text, would lose.".into(),
                ],
                Confirmed::Edit { id, text },
            ));
            return;
        }
        self.start_edit(id, text);
    }

    /// Puts the message's text in the composer, keeping what was there to
    /// give back when the edit is done.
    fn start_edit(&mut self, id: i64, text: String) {
        self.end_edit();
        let Some(open) = self.open.as_mut() else {
            return;
        };
        let Some(msg) = open.messages.get(&id) else {
            return;
        };
        open.editing = Some(Editing {
            id,
            snippet: msg.snippet(),
            original: text.clone(),
            editable: msg.editable,
            draft: self.composer.lines().join("\n"),
            reply: open.reply.take(),
            attachments: std::mem::take(&mut open.attachments),
        });
        open.dropped = None;
        self.set_typing(false);
        self.composer = new_composer();
        self.composer.insert_str(text);
        self.focus = Focus::Input;
    }

    /// Ends an edit, saved or not: the draft and reply from before come back.
    fn end_edit(&mut self) {
        let Some(editing) = self.open.as_mut().and_then(|o| o.editing.take()) else {
            return;
        };
        self.composer = new_composer();
        self.composer.insert_str(editing.draft);
        if let Some(open) = self.open.as_mut() {
            open.reply = editing.reply;
            open.attachments = editing.attachments;
        }
    }

    /// Enter while editing: sends the new text, unless nothing changed.
    fn save_edit(&mut self) {
        let Some(open) = &self.open else {
            return;
        };
        let Some(editing) = &open.editing else {
            return;
        };
        let text = self.composer.lines().join("\n");
        let text = text.trim();
        if text.is_empty() {
            self.status = Some("A message can't be empty (d unsends it)".into());
            return;
        }
        if text != editing.original.trim() && editing.editable == Editable::Text {
            if let Some(why) = open.cant_edit(editing.id, unix_now()) {
                self.status = Some(why.into());
                return;
            }
            self.meta
                .edit_text(open.chat_id, editing.id, text.to_string());
        }
        self.end_edit();
        if self.settings.normal_after_send {
            self.focus = Focus::Messages;
        }
    }

    /// `d`: asks before unsending the message under the cursor, which only
    /// works for your own.
    fn open_delete_menu(&mut self) {
        let Some(open) = &self.open else {
            return;
        };
        let Some((&message_id, msg)) = open
            .cursor_id()
            .and_then(|id| open.messages.get_key_value(&id))
        else {
            return;
        };
        if msg.state == SendState::Pending {
            self.status = Some("Wait until it's sent".into());
            return;
        }
        if !msg.deletable {
            self.status = Some("You can only unsend your own messages".into());
            return;
        }
        self.delete_menu = Some(DeleteMenu {
            message_id,
            snippet: msg.snippet(),
        });
    }

    /// The delete popup takes all keys while it's up: Enter unsends.
    fn on_delete_key(&mut self, key: KeyEvent) {
        let Some(menu) = self.delete_menu.as_ref() else {
            return;
        };
        match key.code {
            KeyCode::Enter | KeyCode::Char('l' | '1') => {
                let message_id = menu.message_id;
                self.delete_menu = None;
                if let Some(open) = &self.open {
                    self.meta.delete_message(open.chat_id, message_id);
                }
            }
            KeyCode::Esc | KeyCode::Char('q' | 'h') => self.delete_menu = None,
            _ => {}
        }
    }

    /// `R`: the emoji to react to the message under the cursor with.
    fn open_react_menu(&mut self) {
        let Some(open) = &self.open else {
            return;
        };
        let Some((&message_id, msg)) = open
            .react_target()
            .and_then(|id| open.messages.get_key_value(&id))
        else {
            return;
        };
        match msg.state {
            SendState::Pending => self.status = Some("Wait until it's sent".into()),
            SendState::Failed => self.status = Some("This message wasn't sent".into()),
            SendState::Sent => {
                let yours: Vec<String> = open
                    .your_reaction(message_id)
                    .into_iter()
                    .map(|(_, emoji)| emoji)
                    .collect();
                let mut menu = ReactMenu::new(message_id, msg.snippet());
                let offered = reactions::OFFERED.iter().map(|e| e.to_string()).collect();
                menu.set_choices(offered, &yours);
                self.react_menu = Some(menu);
            }
        }
    }

    /// The reaction popup takes all keys while it's up. In the grid, `h/j/k/l`
    /// move; after `/`, keys type the search and the arrows move.
    fn on_react_key(&mut self, key: KeyEvent, ctrl: bool) {
        let Some(menu) = self.react_menu.as_mut() else {
            return;
        };
        let row = reactions::COLUMNS as isize;
        let searching = menu.query.is_some();
        match key.code {
            KeyCode::Enter => self.react_with_selected(),
            KeyCode::Left => menu.move_by(-1),
            KeyCode::Right | KeyCode::Tab => menu.move_by(1),
            KeyCode::BackTab => menu.move_by(-1),
            KeyCode::Up => menu.move_by(-row),
            KeyCode::Down => menu.move_by(row),
            KeyCode::Char('n') if ctrl => menu.move_by(1),
            KeyCode::Char('p') if ctrl => menu.move_by(-1),
            // Esc, or Backspace on an empty search, leaves the search first.
            KeyCode::Esc if searching => menu.leave_search(),
            KeyCode::Backspace if menu.query.as_ref().is_some_and(|q| q.is_empty()) => {
                menu.leave_search();
            }
            KeyCode::Backspace if searching => menu.edit_query(|q| {
                q.pop();
            }),
            KeyCode::Char('u' | 'w') if ctrl && searching => menu.edit_query(String::clear),
            KeyCode::Char(c) if searching && !ctrl => menu.edit_query(|q| q.push(c)),
            KeyCode::Char('h') => menu.move_by(-1),
            KeyCode::Char('l') => menu.move_by(1),
            KeyCode::Char('k') => menu.move_by(-row),
            KeyCode::Char('j') => menu.move_by(row),
            KeyCode::Char('/') => menu.edit_query(|_| {}),
            KeyCode::Char('X') => {
                self.react_menu = None;
                self.remove_reaction();
            }
            KeyCode::Esc | KeyCode::Char('q' | 'R') => self.react_menu = None,
            _ => {}
        }
    }

    /// Enter in the reaction popup: makes the emoji under the cursor your
    /// reaction (you have one at most), or takes it back if it already is,
    /// and closes the popup.
    fn react_with_selected(&mut self) {
        // Nothing to pick while nothing matches.
        let Some((message_id, emoji)) = self
            .react_menu
            .as_ref()
            .and_then(|m| Some((m.message_id, m.current()?.to_string())))
        else {
            return;
        };
        self.react_menu = None;
        let Some(open) = &self.open else {
            return;
        };
        // An album is one message on the network: its reaction is on one
        // of its parts, whichever.
        match open.your_reaction(message_id) {
            Some((on, yours)) if reactions::same(&yours, &emoji) => {
                self.meta.react(open.chat_id, on, None)
            }
            _ => self.meta.react(open.chat_id, message_id, Some(&emoji)),
        }
    }

    /// The picker takes all keys while it's up: they type the search, and
    /// the arrows (or Ctrl-n / Ctrl-p, Tab) move.
    fn on_picker_key(&mut self, key: KeyEvent, ctrl: bool) {
        let choices = self
            .picker
            .as_ref()
            .map_or(Vec::new(), |p| p.choices(&self.chats));
        let Some(picker) = self.picker.as_mut() else {
            return;
        };
        let now = Instant::now();
        match key.code {
            KeyCode::Enter => self.pick_chat(),
            KeyCode::Esc => self.picker = None,
            KeyCode::Char('c') if ctrl => self.picker = None,
            KeyCode::Up | KeyCode::BackTab => picker.move_by(-1, &choices),
            KeyCode::Down | KeyCode::Tab => picker.move_by(1, &choices),
            KeyCode::Char('p') if ctrl => picker.move_by(-1, &choices),
            KeyCode::Char('n') if ctrl => picker.move_by(1, &choices),
            KeyCode::PageUp => picker.move_by(-HALF_PAGE, &choices),
            KeyCode::PageDown => picker.move_by(HALF_PAGE, &choices),
            KeyCode::Backspace => picker.edit_query(
                |q| {
                    q.pop();
                },
                now,
            ),
            KeyCode::Char('u' | 'w') if ctrl => picker.edit_query(String::clear, now),
            KeyCode::Char(c) if !ctrl => picker.edit_query(|q| q.push(c), now),
            _ => {}
        }
    }

    /// Enter in the picker: opens the chat, or starts one with someone
    /// found.
    fn pick_chat(&mut self) {
        let Some(picker) = self.picker.as_ref() else {
            return;
        };
        let Some(choice) = picker.current(&picker.choices(&self.chats)) else {
            return;
        };
        self.picker = None;
        match choice {
            Choice::Chat(id) => self.open_chat(id),
            Choice::Person {
                network,
                user_id,
                name,
                ..
            } => {
                self.finding = Some(Finding::new(&name, network, user_id));
                self.meta.open_dm(network, user_id, name);
            }
        }
    }

    /// `X`: takes back your reaction on the message under the cursor,
    /// without the popup.
    fn remove_reaction(&mut self) {
        let Some(open) = &self.open else {
            return;
        };
        match open.cursor_id().and_then(|id| open.your_reaction(id)) {
            Some((message_id, _)) => self.meta.react(open.chat_id, message_id, None),
            None => self.status = Some("You haven't reacted to this message".into()),
        }
    }

    /// Resize mode takes the keys: `h` and `l` move the line between the
    /// chat list and the chat as you press them, `=` puts it back where it
    /// starts out, Enter keeps it and Esc puts back where it was.
    fn on_resize_key(&mut self, key: KeyEvent, ctrl: bool) {
        let Some(Resizing::List(before)) = self.resizing else {
            return;
        };
        // `h` and `l` move the line between the panes that way.
        let left = match self.settings.chat_list_side {
            Side::Left => -1,
            Side::Right => 1,
        };
        let settings = &mut self.settings;
        match key.code {
            KeyCode::Char('h') | KeyCode::Left => settings.resize_list(left),
            KeyCode::Char('l') | KeyCode::Right => settings.resize_list(-left),
            KeyCode::Char('=') => settings.chat_list_width = settings::DEFAULT_LIST_WIDTH,
            KeyCode::Esc => {
                settings.chat_list_width = before;
                self.resizing = None;
            }
            KeyCode::Enter => self.end_resize(),
            KeyCode::Char('r') if ctrl => self.end_resize(),
            _ => {}
        }
    }

    /// Leaves resize mode with the panes as they are, saved for next time.
    fn end_resize(&mut self) {
        let changed = match self.resizing.take() {
            Some(Resizing::List(width)) => width != self.settings.chat_list_width,
            None => false,
        };
        if changed && let Err(e) = self.settings.save(&self.settings_path) {
            self.status = Some(format!("Couldn't save settings: {e:#}"));
        }
    }

    /// `gd`: from a reply to the message it answers, leaving the reply for
    /// Ctrl-o to come back to.
    fn go_to_replied(&mut self) {
        let Some(open) = self.open.as_ref() else {
            return;
        };
        match open.replied_jump() {
            Ok((from, to)) => {
                self.jumps.leave(Jump {
                    chat_id: open.chat_id,
                    message_id: Some(from),
                });
                self.jump_to_message(to);
            }
            Err(why) => self.status = Some(why.into()),
        }
    }

    /// Files open in their default app once downloaded; links in the
    /// browser, but a link to an Instagram or Threads post here, in its
    /// feed's place, while that account is in. Either asks first if the
    /// link's words say something other than where it goes.
    fn open_target(&mut self, target: Target) {
        match target {
            Target::Photo { message_id, file } => self.view_photo(message_id, file),
            Target::File(file) => {
                // The helper answers at once if the file is already downloaded.
                if self.opening.insert(file.id) {
                    self.meta.download(file.id);
                }
            }
            Target::Link(link) => match feed::post_link(&link.url)
                .filter(|post| self.ready_networks().contains(&post.network))
            {
                Some(post) => match link.disguise {
                    Some(shown) => self.ask_to_open(
                        "Open this post?",
                        format!("The text says: {shown}"),
                        link.url,
                        Confirmed::OpenPost(post),
                    ),
                    None => self.open_linked_post(post),
                },
                None => self.open_link_outside(link),
            },
            Target::Text(_) => {}
        }
    }

    /// Shows message `message_id`'s photo in the viewer; one that's no
    /// longer loaded opens in its app, as `o` in the viewer would.
    fn view_photo(&mut self, message_id: i64, file: MediaFile) {
        let Some(open) = self.open.as_mut() else {
            return;
        };
        let view = match open.messages.get(&message_id) {
            Some(msg) => PhotoView::of(open.chat_id, message_id, msg),
            None => {
                self.open_target(Target::File(file));
                return;
            }
        };
        let Some(view) = view else {
            self.status = Some("This photo can't be shown here".into());
            return;
        };
        // The cursor stays on it rather than following new messages: what
        // arrives meanwhile is hidden under the photo, so it isn't marked
        // read, and an Enter after closing it isn't on something unseen.
        if open.selected.is_none() {
            open.selected = Some(message_id);
        }
        self.photo_view = Some(view);
    }

    /// The photo viewer takes all keys while it's up.
    fn on_viewer_key(&mut self, key: KeyEvent) {
        let Some(view) = self.photo_view.as_mut() else {
            return;
        };
        // Ctrl-o, out of habit for going back, isn't `o`, which hands the
        // photo to another app.
        if key
            .modifiers
            .intersects(KeyModifiers::CONTROL | KeyModifiers::ALT)
        {
            return;
        }
        match key.code {
            KeyCode::Char('h') | KeyCode::Left => self.view_next_photo(false),
            KeyCode::Char('l') | KeyCode::Right => self.view_next_photo(true),
            KeyCode::Char('j' | '+' | '=') => view.zoom_in(),
            KeyCode::Char('k' | '-') => view.zoom_out(),
            // It closes first: the app comes up over tuimeta anyway, and a
            // warning about the file mustn't come up under the photo.
            KeyCode::Char('o') => {
                let file = view.file.clone();
                self.photo_view = None;
                self.open_target(Target::File(file));
            }
            KeyCode::Char('y') => {
                let file = view.file.clone();
                self.copy_target(Target::File(file));
            }
            KeyCode::Enter | KeyCode::Esc | KeyCode::Char('q') => self.photo_view = None,
            _ => {}
        }
    }

    /// `h` / `l` in the viewer: the photo before or after the one shown,
    /// among the messages loaded. Past those, more are loaded, for `h` or
    /// `l` again.
    fn view_next_photo(&mut self, newer: bool) {
        let Some(view) = &self.photo_view else {
            return;
        };
        if chats::is_feed(view.chat_id) {
            self.view_next_feed_photo(view.message_id, view.photo.file_id, newer);
            return;
        }
        let Some(open) = self.open.as_ref().filter(|o| o.chat_id == view.chat_id) else {
            return;
        };
        let id = view.message_id;
        let photo =
            |(&id, msg): (&i64, &crate::messages::Msg)| PhotoView::of(open.chat_id, id, msg);
        let next = match newer {
            true => open.messages.range(id + 1..).find_map(photo),
            false => open.messages.range(..id).rev().find_map(photo),
        };
        if next.is_some() {
            self.photo_view = next;
            return;
        }
        match (newer, open.all_loaded, open.at_newest) {
            (false, false, _) => {
                self.status = Some("Loading older messages…".into());
                self.load_older_messages();
            }
            (true, _, false) => {
                self.status = Some("Loading newer messages…".into());
                self.load_newer_messages();
            }
            (false, true, _) => self.status = Some("No older photos in this chat".into()),
            (true, _, true) => self.status = Some("No newer photos in this chat".into()),
        }
    }

    /// Opens a link in the browser, asking first if its words say something
    /// other than where it goes, or its address can pass for another.
    fn open_link_outside(&mut self, link: Link) {
        let Link { url, disguise, .. } = link;
        let why = match disguise {
            Some(shown) => format!("The text says: {shown}"),
            None => match look_alike(&url) {
                Some(why) => why.to_string(),
                None => return self.open_externally(&url),
            },
        };
        self.ask_to_open(
            "Open this link?",
            why,
            url.clone(),
            Confirmed::OpenLink(url),
        );
    }

    /// Asks before `action` opens `url`, saying `why` and showing the
    /// address in full, with its site on a line of its own.
    fn ask_to_open(&mut self, title: &str, why: String, url: String, action: Confirmed) {
        // Browsers show other scripts' letters as such, so a look-alike
        // host can pass for a familiar one.
        let site = link_host(&url).map(|host| match other_scripts(&host) {
            false => host,
            true => format!("{host} (has non-Latin letters)"),
        });
        let mut confirm = Confirm::new(title, vec![why, format!("Full address:  {url}")], action);
        confirm.site = site;
        self.confirm = Some(confirm);
    }

    /// A file finished downloading for Enter: opened at once if it's a
    /// type that can't run code, else only after a `y`.
    fn open_downloaded(&mut self, path: String) {
        mark_downloaded(&path);
        if safe_to_open(&path) {
            self.open_externally(&path);
            return;
        }
        // The file name is the sender's.
        let name = text::clean(
            &Path::new(&path)
                .file_name()
                .map(|n| n.to_string_lossy().into_owned())
                .unwrap_or_default(),
        );
        // A slow download can finish while you're busy with something else:
        // a warning popping up then would take keys meant for that.
        if self.busy() {
            self.status = Some(format!(
                "{name} downloaded: press Enter on it again to open it"
            ));
            return;
        }
        self.confirm = Some(Confirm::new(
            format!("Open {name}?"),
            vec![
                "Files like this can run programs on your computer.".into(),
                "Only open it if you trust whoever sent it.".into(),
            ],
            Confirmed::OpenFile(path),
        ));
    }

    /// A popup, a prompt or the composer is taking keys.
    fn busy(&self) -> bool {
        self.popup_over_chat() || self.focus == Focus::Input
    }

    /// A modal layer is up over the chat: a confirm, menu, picker, prompt,
    /// the photo viewer, settings or the resize bar. These cover the view
    /// and take every key, so the user isn't reading the chat behind them —
    /// which is why `watching` (read receipts) must treat them as not seen,
    /// not just the settings menu. So does the list of emoji suggestions,
    /// drawn over the bottom of the messages, where the newest one is.
    pub fn popup_over_chat(&self) -> bool {
        let suggesting = self.focus == Focus::Input
            && self
                .completion
                .as_ref()
                .is_some_and(|c| !c.items.is_empty());
        suggesting || self.popup_up() || self.resizing.is_some() || self.prompt.is_some()
    }

    /// One of the popups is up: a confirm, menu, picker, the photo viewer
    /// or settings. A popup fades in as it comes up.
    pub fn popup_up(&self) -> bool {
        self.confirm.is_some()
            || self.settings_menu.is_some()
            || self.delete_menu.is_some()
            || self.react_menu.is_some()
            || self.photo_view.is_some()
            || self.menu.is_some()
            || self.picker.is_some()
    }

    /// The toast slides in and out. Not where sixel or iTerm2 pictures may
    /// be under it: a picture's cells uncovered a frame at a time would
    /// send it to the terminal again each frame, over the toast.
    pub fn slides_toast(&self) -> bool {
        self.settings.animations && !self.images.paints_over()
    }

    fn open_externally(&mut self, target: &str) {
        if let Err(e) = open_externally(target) {
            self.status = Some(format!("Couldn't open it: {e}"));
        }
    }

    /// The confirmation takes all keys while it's up. Only `y` goes ahead, so
    /// an Enter pressed out of habit can't.
    fn on_confirm_key(&mut self, key: KeyEvent) {
        match key.code {
            KeyCode::Char('y')
                if self
                    .confirm
                    .as_ref()
                    .is_some_and(|c| c.shown.elapsed() >= CONFIRM_GRACE) =>
            {
                if let Some(confirm) = self.confirm.take() {
                    match confirm.action {
                        Confirmed::OpenFile(target) | Confirmed::OpenLink(target) => {
                            self.open_externally(&target)
                        }
                        // Its account may have gone meanwhile: then it's any link.
                        Confirmed::OpenPost(post) => {
                            match self.ready_networks().contains(&post.network) {
                                true => self.open_linked_post(post),
                                false => self.open_externally(&post.url),
                            }
                        }
                        Confirmed::Edit { id, text } => self.start_edit(id, text),
                        Confirmed::Logout(network) => self.log_out(network),
                    }
                }
            }
            KeyCode::Char('n' | 'q') | KeyCode::Esc => self.confirm = None,
            _ => {}
        }
    }

    /// `y`: copies what's in the message under the cursor. With links or
    /// media as well as text, a menu asks which.
    fn copy_selected(&mut self) {
        let Some(open) = &self.open else {
            return;
        };
        let Some((&id, msg)) = open
            .cursor_id()
            .and_then(|id| open.messages.get_key_value(&id))
        else {
            return;
        };
        let mut targets = Vec::new();
        if !msg.source_text.is_empty() {
            targets.push(Target::Text(msg.source_text.clone()));
        }
        targets.extend(msg.links.iter().cloned().map(Target::Link));
        targets.extend(msg.file.clone().map(Target::File));
        match targets.len() {
            0 => self.status = Some("Nothing to copy in this message".into()),
            1 => self.copy_target(targets.remove(0)),
            _ => self.show_menu(id, MenuAction::Copy, targets),
        }
    }

    /// Text and links are copied at once. Media is downloaded first (the
    /// helper answers at once if it already is), then copied by
    /// `copy_downloaded`.
    fn copy_target(&mut self, target: Target) {
        let text = match target {
            Target::Text(text) => text,
            Target::Link(link) => link.url,
            Target::File(file) | Target::Photo { file, .. } => {
                let id = file.id;
                if self.copying.insert(id, file).is_none() {
                    self.meta.download(id);
                }
                return;
            }
        };
        match self.clipboard.copy_text(&text) {
            Ok(Copied::System) => self.show_toast("Copied", &text),
            Ok(Copied::Terminal) => self.show_toast("Sent to the terminal's clipboard", &text),
            Err(e) => self.status = Some(format!("Couldn't copy: {e}")),
        }
    }

    /// Photos are copied as images, after decoding off the UI thread; other
    /// files as files, so pasting attaches them.
    fn copy_downloaded(&mut self, file: MediaFile, path: &str) {
        if file.photo {
            self.clipboard.decode_image(path.to_string(), file.label);
            return;
        }
        match self.clipboard.copy_file(Path::new(path)) {
            Ok(()) => self.show_toast("Copied", &file.label),
            Err(e) => self.status = Some(format!("Couldn't copy: {e}")),
        }
    }

    fn on_clipboard(&mut self, event: ClipboardEvent) {
        match event {
            ClipboardEvent::Decoded(decoded) => self.on_decoded(decoded),
            ClipboardEvent::Pasted(pasted) => self.on_pasted(pasted),
        }
    }

    /// Whether files can go with the next message: a chat is open, and its
    /// composer isn't editing a message, which can't take any. Says why not
    /// in the status bar.
    fn can_attach(&mut self) -> bool {
        match &self.open {
            None => false,
            Some(open) if open.editing.is_some() => {
                self.status = Some("Files can't be added to an edit".into());
                false
            }
            Some(_) => true,
        }
    }

    /// Adds files to the next message and goes to Insert mode for the
    /// caption. `pasted` is the paste they came from, for Ctrl-z. Files that
    /// can't be sent are left out, and the status bar says why. Returns how
    /// many were added.
    fn attach(&mut self, paths: Vec<PathBuf>, pasted: Option<String>) -> usize {
        if !self.can_attach() {
            return 0;
        }
        let Some(open) = self.open.as_mut() else {
            return 0;
        };
        let mut added = 0;
        for path in paths {
            match Attachment::new(&path) {
                Ok(attachment) => {
                    open.attachments.push(attachment);
                    added += 1;
                }
                Err(e) => self.status = Some(e),
            }
        }
        if added == 0 {
            return 0;
        }
        open.dropped = pasted.map(|text| Dropped { text, count: added });
        self.focus = Focus::Input;
        added
    }

    /// A paste into the chat (Cmd-V, or files dropped on the window). Paths
    /// to files are attached, which Ctrl-z undoes; other text is typed in
    /// Insert mode. While editing, it's all text.
    fn on_paste(&mut self, text: String) {
        let editing = self.open.as_ref().is_some_and(|o| o.editing.is_some());
        if !editing && let Some(paths) = attach::pasted_paths(&text) {
            self.attach(paths, Some(text));
            return;
        }
        if self.focus == Focus::Input {
            self.composer.insert_str(text.replace('\r', ""));
            self.on_composer_edit();
        }
    }

    /// `p` and Ctrl-v: what's on the system clipboard goes in the message,
    /// once it's been read off the UI thread (`on_pasted`).
    fn paste_clipboard(&mut self) {
        let Some(open) = &self.open else {
            return;
        };
        self.clipboard.paste(open.chat_id);
        self.pasting = true;
    }

    fn on_pasted(&mut self, pasted: Pasted) {
        self.pasting = false;
        // Not into another chat than the one it was meant for.
        if self
            .open
            .as_ref()
            .is_none_or(|o| o.chat_id != pasted.chat_id)
        {
            return;
        }
        // Nor into Insert mode once the keys went to the chats, where an
        // Enter meant to open one would send it.
        if !matches!(self.focus, Focus::Messages | Focus::Input) {
            self.status = Some("The paste came after you left the messages: p pastes again".into());
            return;
        }
        // Nor under the photo viewer, out of sight.
        if self.photo_view.is_some() {
            self.status = Some("The paste came while a photo was open: p pastes again".into());
            return;
        }
        match pasted.content {
            Ok(Paste::Files(paths)) => {
                // Copied files have absolute paths; anything else would be
                // looked for wherever tuimeta was started. A `\\server\share`
                // path is absolute but on another machine, and even stat-ing
                // it hands that server the user's login hash, so it's dropped
                // unlooked-at too (same rule as a dropped/text paste, via
                // attach::on_another_machine).
                let total = paths.len();
                let paths: Vec<_> = paths
                    .into_iter()
                    .filter(|p| p.is_absolute() && !attach::on_another_machine(p))
                    .collect();
                if paths.len() != total {
                    self.status = Some(
                        "Copied files without a full path, or on another machine, were left out"
                            .into(),
                    );
                }
                self.attach(paths, None);
            }
            Ok(Paste::Image(path)) => {
                // Not the made-up name it was saved under.
                if self.attach(vec![path], None) == 1
                    && let Some(last) = self.open.as_mut().and_then(|o| o.attachments.last_mut())
                {
                    last.name = "Pasted image".into();
                }
            }
            Ok(Paste::Text(text)) => {
                self.focus = Focus::Input;
                self.on_paste(text);
            }
            Err(e) => self.status = Some(e),
        }
    }

    /// Ctrl-z: files a paste just attached go back to being the text that
    /// was pasted, for a path that was meant to be sent as words.
    fn undo_drop(&mut self) {
        let Some(text) = self.open.as_mut().and_then(OpenChat::undo_drop) else {
            return;
        };
        self.composer.insert_str(text.replace('\r', ""));
        self.on_composer_edit();
    }

    fn on_decoded(&mut self, decoded: Decoded) {
        let result = decoded
            .image
            .and_then(|image| self.clipboard.copy_image(image).map_err(|e| e.to_string()));
        match result {
            Ok(()) => self.show_toast("Copied", &decoded.label),
            Err(e) => self.status = Some(format!("Couldn't copy: {e}")),
        }
    }

    fn show_toast(&mut self, title: &str, detail: &str) {
        let now = Instant::now();
        // One already up stays where it is, rather than slide in again: in
        // place, or on its way out, from where it got to.
        let shown = self
            .toast
            .as_ref()
            .filter(|t| t.until > now)
            .map_or(now, |t| {
                let going = now.checked_sub(t.until - now).unwrap_or(t.shown);
                t.shown.max(going)
            });
        self.toast = Some(Toast {
            title: title.into(),
            detail: detail.split_whitespace().collect::<Vec<_>>().join(" "),
            shown,
            until: now + TOAST_TIME,
        });
    }

    /// The open menu takes all keys while it's up.
    fn on_menu_key(&mut self, key: KeyEvent) {
        let Some(menu) = self.menu.as_mut() else {
            return;
        };
        let last = menu.targets.len() - 1;
        match key.code {
            KeyCode::Char('j') | KeyCode::Down => menu.selected = (menu.selected + 1).min(last),
            KeyCode::Char('k') | KeyCode::Up => menu.selected = menu.selected.saturating_sub(1),
            KeyCode::Enter | KeyCode::Char('l') => {
                let index = menu.selected;
                self.pick(index);
            }
            // 1-9 pick an item directly.
            KeyCode::Char(c @ '1'..='9') => {
                let index = c as usize - '1' as usize;
                if index <= last {
                    self.pick(index);
                }
            }
            KeyCode::Esc | KeyCode::Char('q' | 'h') => self.menu = None,
            _ => {}
        }
    }

    /// Opens or copies item `index` of the menu, and closes it.
    fn pick(&mut self, index: usize) {
        let Some(mut menu) = self.menu.take() else {
            return;
        };
        let target = menu.targets.swap_remove(index);
        match menu.action {
            MenuAction::Open => self.open_target(target),
            MenuAction::Copy => self.copy_target(target),
        }
    }

    /// The `?` popup takes all keys while it's up. Tab (or h/l) switches
    /// between the shortcuts and the settings. On the settings, Enter or
    /// Space changes the one under the cursor and saves it at once.
    fn on_settings_key(&mut self, key: KeyEvent, ctrl: bool) {
        let Some(menu) = self.settings_menu.as_mut() else {
            return;
        };
        match key.code {
            KeyCode::Tab | KeyCode::BackTab | KeyCode::Char('h' | 'l') => {
                menu.tab = match menu.tab {
                    HelpTab::Shortcuts => HelpTab::Settings,
                    HelpTab::Settings => HelpTab::Shortcuts,
                };
                return;
            }
            KeyCode::Enter | KeyCode::Char(' ') if menu.tab == HelpTab::Settings => {
                self.change_setting();
                return;
            }
            KeyCode::Esc | KeyCode::Char('q' | '?') => {
                self.settings_menu = None;
                return;
            }
            _ => {}
        }
        let delta = match key.code {
            KeyCode::Char('j') | KeyCode::Down => 1,
            KeyCode::Char('k') | KeyCode::Up => -1,
            KeyCode::Char('d') if ctrl => HALF_PAGE,
            KeyCode::Char('u') if ctrl => -HALF_PAGE,
            KeyCode::Char('g') => isize::MIN,
            KeyCode::Char('G') => isize::MAX,
            _ => return,
        };
        match menu.tab {
            // Drawing stops it at the end of the list.
            HelpTab::Shortcuts => menu.scroll = menu.scroll.saturating_add_signed(delta),
            HelpTab::Settings => {
                let last = SettingsMenu::themes(&self.sounds) + self.themes.list.len() - 1;
                menu.selected = menu.selected.saturating_add_signed(delta).min(last);
            }
        }
    }

    /// Enter or Space on the settings tab: picks the sound or the theme
    /// under the cursor, or turns the setting under it on or off. Saved at
    /// once.
    fn change_setting(&mut self) {
        let Some(menu) = self.settings_menu.as_mut() else {
            return;
        };
        let settings = &mut self.settings;
        let themes = SettingsMenu::themes(&self.sounds);
        match menu.selected {
            i if i >= themes => {
                let Some(theme) = self.themes.list.get(i - themes) else {
                    return;
                };
                match &theme.colors {
                    Ok(colors) => {
                        self.colors = *colors;
                        settings.theme = theme.id.clone();
                    }
                    Err(e) => {
                        self.status = Some(format!("Theme not used: {e}"));
                        return;
                    }
                }
            }
            i if i >= SettingsMenu::SOUNDS => match i
                .checked_sub(SettingsMenu::SOUNDS + 1)
                .and_then(|n| self.sounds.list.get(n))
            {
                // The first row: no sound.
                None => {
                    self.sound = None;
                    settings.notification_sound = None;
                }
                Some(sound) => match self.sounds.samples(&sound.id) {
                    Ok(samples) => {
                        // Played, to hear what it's like.
                        if self.over_ssh {
                            self.status = Some(
                                "Over SSH, tuimeta plays no sound: it would play on the other computer"
                                    .into(),
                            );
                        } else {
                            self.ringer.ring(Arc::clone(&samples));
                        }
                        self.sound = Some(samples);
                        settings.notification_sound = Some(sound.id.clone());
                    }
                    Err(e) => {
                        self.status = Some(format!("Sound not used: {e}"));
                        return;
                    }
                },
            },
            SettingsMenu::NOTIFICATIONS => {
                // Back on, they go out the way they did before, e.g. "bell".
                let on = match menu.saved_notifications {
                    Notifications::Off => Notifications::Auto,
                    saved => saved,
                };
                let next = match settings.notifications {
                    Notifications::Off => on,
                    _ => Notifications::Off,
                };
                self.set_notifications(next);
            }
            SettingsMenu::CHAT_GAPS => settings.chat_gaps = !settings.chat_gaps,
            SettingsMenu::LIST_RIGHT => {
                settings.chat_list_side = match settings.chat_list_side {
                    Side::Left => Side::Right,
                    Side::Right => Side::Left,
                };
            }
            SettingsMenu::BLOCK_GAPS => settings.block_gaps = !settings.block_gaps,
            SettingsMenu::CORNERS => {
                // Picked for good either way: a terminal the guess got
                // wrong stays as the user set it.
                self.rounded = !self.rounded;
                settings.corners = match self.rounded {
                    true => Corners::Rounded,
                    false => Corners::Square,
                };
            }
            SettingsMenu::PILLS => settings.nerd_font = !settings.nerd_font,
            SettingsMenu::ANIMATIONS => {
                settings.animations = !settings.animations;
                // Nothing left moving to wake the run loop for.
                if !settings.animations {
                    self.motion.stop();
                }
            }
            SettingsMenu::AFTER_SEND => settings.normal_after_send = !settings.normal_after_send,
            SettingsMenu::WRAP_COMPOSER => settings.wrap_composer = !settings.wrap_composer,
            SettingsMenu::SOUND_FOCUSED => {
                settings.sound_when_focused = !settings.sound_when_focused;
            }
            _ => return,
        }
        if let Err(e) = self.settings.save(&self.settings_path) {
            self.status = Some(format!("Settings not saved: {e:#}"));
        }
    }

    /// Takes effect at once; the settings popup saves it.
    fn set_notifications(&mut self, notifications: Notifications) {
        self.settings.notifications = notifications;
        self.notify_with = notifications.resolve(|name| std::env::var(name).ok());
        if notifications == Notifications::Off {
            self.notifier.clear();
        }
    }

    fn load_older_messages(&mut self) {
        let Some(open) = self.open.as_mut() else {
            return;
        };
        if open.loading.is_some() || open.all_loaded {
            return;
        }
        let page = open.oldest_id().map_or(Page::Latest, Page::Older);
        open.loading = Some(page);
        self.meta.load_history(open.chat_id, page, HISTORY_PAGE);
    }

    /// Only needed after jumping to an old message.
    fn load_newer_messages(&mut self) {
        let Some(open) = self.open.as_mut() else {
            return;
        };
        if open.loading.is_some() || open.at_newest {
            return;
        }
        let Some(newest) = open.newest_id() else {
            return;
        };
        let page = Page::Newer(newest);
        open.loading = Some(page);
        self.meta.load_history(open.chat_id, page, HISTORY_PAGE);
    }

    fn move_message_cursor(&mut self, delta: isize) {
        let Some(open) = self.open.as_mut() else {
            return;
        };
        // `G` goes to the real newest message, not the newest loaded one.
        if delta == isize::MAX && !open.at_newest {
            self.jump_to_newest();
            return;
        }
        let Some(index) = open.move_cursor(delta) else {
            return;
        };
        let len = open.messages.len();
        if index < LOAD_AHEAD {
            self.load_older_messages();
        }
        if index + LOAD_AHEAD >= len {
            self.load_newer_messages();
        }
    }

    /// Moves the chat list cursor by `delta` rows, clamped to the list. Near
    /// its end, more chats are asked for.
    fn move_chat_cursor(&mut self, delta: isize) {
        let ids = self.chats.ids();
        if ids.is_empty() {
            return;
        }
        let last = ids.len() - 1;
        let index = match self
            .selected
            .and_then(|id| ids.iter().position(|&x| x == id))
        {
            Some(current) => current.saturating_add_signed(delta).min(last),
            None => 0,
        };
        self.selected = Some(ids[index]);

        if index + LOAD_AHEAD >= last {
            let networks = match self.chats.shown() {
                List::Network(network) => vec![network],
                List::Main | List::Archive => self.ready_networks(),
            };
            for network in networks {
                self.load_more_chats(network);
            }
        }
    }

    /// Asks for the next page of a network's chats. Only as the list is
    /// read: asking for every chat at once is what a script would do.
    fn load_more_chats(&mut self, network: Network) {
        // Threads has only its feed.
        if !network.has_chats()
            || self.loading_chats.contains(&network)
            || self.loaded_chats.contains(&network)
        {
            return;
        }
        self.loading_chats.insert(network);
        self.meta.load_chats(Some(network), CHAT_PAGE);
    }

    /// Chats are still loading.
    pub fn chats_loading(&self) -> bool {
        !self.loading_chats.is_empty()
    }

    /// Tab (`step` 1) / Shift-Tab (-1) in the chat list: the next or
    /// previous tab, round the end, with the cursor on its first chat.
    fn switch_list(&mut self, step: isize) {
        if self.chats.tabs().is_empty() {
            self.status = Some("One network and no archive: nothing to switch to".into());
            return;
        }
        let list = self.chats.next_list(step);
        self.chats.show(list);
        self.chats.refresh();
        self.selected = self.chats.ids().first().copied();
    }

    /// `m` in the list: mutes the selected chat, or unmutes it, on the
    /// network. It shows at once.
    fn toggle_mute(&mut self) {
        let Some(chat_id) = self.selected else {
            return;
        };
        if chats::is_feed(chat_id) {
            self.status = Some("The feed has no notifications to mute".into());
            return;
        }
        let mute = !self.chats.muted(chat_id);
        self.meta.set_muted(chat_id, mute);
        self.chats.set_muted(chat_id, mute);
    }

    /// `H`: marks the selected chat so it's easy to find, or unmarks it.
    fn toggle_highlight(&mut self) {
        let Some(chat_id) = self.selected.filter(|&id| !chats::is_feed(id)) else {
            return;
        };
        self.settings.highlighted_chats = self.chats.toggle_highlight(chat_id);
        if let Err(e) = self.settings.save(&self.settings_path) {
            self.status = Some(format!("Highlight not saved: {e:#}"));
        }
    }

    /// Closes the helper's stdin, after which it disconnects without telling
    /// anyone anything and stops; a second `q` doesn't wait for it.
    fn quit(&mut self) {
        if self.quit_deadline.is_some() {
            self.exit = true;
            return;
        }
        self.set_typing(false);
        self.stop_video();
        self.meta.close();
        // Nothing to wait for without a helper.
        if self.meta.is_detached() {
            self.exit = true;
        }
        self.quit_deadline = Some(Instant::now() + CLOSE_TIMEOUT);
    }
}

/// The networks' home feeds, and their videos inside Pepper.
impl App {
    /// Opens `network`'s feed in the chat's place: as it was left, or its
    /// first page.
    fn open_feed(&mut self, network: Network) {
        if self
            .feed
            .as_ref()
            .is_some_and(|f| f.network == network && f.linked.is_none())
        {
            return;
        }
        // The chat, or the other network's feed, is kept as it was.
        self.leave_chat();
        let feed = match self.feeds_kept.remove(&network) {
            Some(feed) => feed,
            None => self.new_feed(network, false),
        };
        self.feed = Some(feed);
    }

    /// `network`'s feed, its first page asked for: Threads' of the accounts
    /// followed, with `following`.
    fn new_feed(&mut self, network: Network, following: bool) -> Feed {
        let picture = self.feed_picture(network);
        let mut feed = Feed::new(network);
        feed.following = following;
        if let Some(load) = feed.start_load(false) {
            feed.request =
                Some(
                    self.meta
                        .load_feed(network, feed::PAGE, false, picture, following, load),
                );
        }
        feed
    }

    /// Enter on a link to an Instagram or Threads post: the post alone, in
    /// its network's feed's place, loaded as the feed's posts are (only
    /// read). Esc goes on to the feed as it was left, Ctrl-o back to the
    /// chat.
    fn open_linked_post(&mut self, link: feed::PostLink) {
        let Some(row) = chats::feed_id(link.network) else {
            return;
        };
        if let Some(here) = self.here() {
            self.jumps.leave(here);
        }
        self.close_chat_popups();
        // The chat, or the feed, is kept as it was.
        self.leave_chat();
        // The list's cursor goes to the feed's row: in its network's tab when
        // the list of every chat hasn't it, and even if the filter hid it.
        self.chats.reveal(row);
        self.selected = Some(row);
        self.focus = Focus::Messages;
        self.feed = Some(Feed::linked(link));
        self.load_linked_post();
    }

    /// Asks for the linked post shown, again if it's there.
    fn load_linked_post(&mut self) {
        let Some(network) = self
            .feed
            .as_ref()
            .filter(|f| f.linked.is_some())
            .map(|f| f.network)
        else {
            return;
        };
        let picture = self.feed_picture(network);
        let Some(feed) = self.feed.as_mut() else {
            return;
        };
        if let Some(load) = feed.start_load(true)
            && let Some(link) = &feed.linked
        {
            let request =
                self.meta
                    .load_post(network, &link.code, link.username.as_deref(), picture, load);
            if let Some(old) = feed.sent(request) {
                self.meta.cancel(old);
            }
        }
    }

    /// The linked post came, or why it didn't.
    fn on_linked_post(&mut self, network: Network, load: u64, post: Result<Box<FeedPost>, String>) {
        let Some(feed) = self
            .feed
            .as_mut()
            .filter(|f| f.network == network && f.linked.is_some())
        else {
            return;
        };
        let failed = post.as_ref().err().map(|why| text::clean(why));
        if !feed.on_post(load, post.map(|post| *post)) {
            return;
        }
        if let Some(why) = failed {
            self.status = Some(format!("Post: {why}"));
            return;
        }
        // It's under the cursor now, maybe just as a key went down.
        self.stop_video();
        self.close_comments();
        self.feed_changed = Some(Instant::now());
    }

    /// Esc on a linked post: its network's feed, as it was left.
    fn back_to_feed(&mut self) {
        if let Some(network) = self
            .feed
            .as_ref()
            .filter(|f| f.linked.is_some())
            .map(|f| f.network)
        {
            self.open_feed(network);
        }
    }

    /// A key in the feed's pane, before the keys every pane has. False if
    /// it's one of those: quitting, help, commands, the picker, jumps,
    /// going back to the list.
    fn on_feed_key(&mut self, key: KeyEvent, ctrl: bool, pending_g: bool) -> bool {
        let to_list = match self.settings.chat_list_side {
            Side::Left => 'h',
            Side::Right => 'l',
        };
        let linked = self.feed.as_ref().is_some_and(|f| f.linked.is_some());
        let motion = match key.code {
            KeyCode::Char('j') | KeyCode::Down => Some(1),
            KeyCode::Char('k') | KeyCode::Up => Some(-1),
            KeyCode::Char('d') if ctrl => Some(FEED_JUMP),
            KeyCode::Char('u') if ctrl => Some(-FEED_JUMP),
            KeyCode::Char('g') if pending_g => Some(isize::MIN),
            KeyCode::Char('G') => Some(isize::MAX),
            _ => None,
        };
        if let Some(delta) = motion {
            self.move_feed_cursor(delta);
            return true;
        }
        match key.code {
            _ if ctrl => return false,
            KeyCode::Char('[') | KeyCode::Left => self.step_feed_item(-1),
            KeyCode::Char(']') | KeyCode::Right => self.step_feed_item(1),
            KeyCode::Enter => self.open_feed_item(),
            KeyCode::Char(' ') => self.pause_video(),
            KeyCode::Char('m') => self.mute_video(),
            KeyCode::Char(',') => self.seek_video(-player::SEEK_STEP),
            KeyCode::Char('.') => self.seek_video(player::SEEK_STEP),
            KeyCode::Char('o') => self.open_post_outside(),
            KeyCode::Char('y') => self.copy_post_link(),
            KeyCode::Char('r') => self.reload_feed(),
            KeyCode::Char('f') => self.full_screen_video(),
            KeyCode::Char('c') => self.toggle_comments(),
            // The comments pane is on the post's right.
            KeyCode::Char('l') if self.comments.is_some() && !self.comments_focus => {
                self.comments_focus = true
            }
            KeyCode::Esc if self.playing.is_some() => self.stop_video(),
            KeyCode::Esc if linked => self.back_to_feed(),
            KeyCode::Char('q' | '?' | ':' | 's' | 'g') | KeyCode::Esc | KeyCode::Tab => {
                return false;
            }
            KeyCode::Char(c) if c == to_list => return false,
            // A chat's keys (writing, replying, reacting) have nothing to
            // act on here.
            _ => {}
        }
        true
    }

    /// Moves the feed's cursor `delta` posts. A video playing stops with
    /// its post left; near the end, the next page is asked for.
    fn move_feed_cursor(&mut self, delta: isize) {
        let Some(feed) = self.feed.as_mut() else {
            return;
        };
        let before = feed.selected;
        feed.move_cursor(delta);
        // The video and the comments are the post's.
        if feed.selected != before {
            self.stop_video();
            self.close_comments();
        }
        self.load_more_feed();
    }

    /// `[` / `]`: the photo or video before or after in the post's
    /// carousel.
    fn step_feed_item(&mut self, step: isize) {
        let Some(feed) = self.feed.as_mut() else {
            return;
        };
        if feed.step_item(step) {
            self.stop_video();
        } else if feed.selected_post().is_some_and(|p| p.media.len() < 2) {
            self.status = Some("This post has one photo or video".into());
        }
    }

    /// The next page, if the cursor is near the end and one isn't on its
    /// way. Only as the feed is read: loading all of it is what a script
    /// would do.
    fn load_more_feed(&mut self) {
        let Some(network) = self.feed.as_ref().map(|f| f.network) else {
            return;
        };
        let picture = self.feed_picture(network);
        let Some(feed) = self.feed.as_mut() else {
            return;
        };
        if feed.wants_more()
            && let Some(load) = feed.start_load(false)
        {
            let request = self.meta.load_feed(
                feed.network,
                feed::PAGE,
                false,
                picture,
                feed.following,
                load,
            );
            if let Some(old) = feed.sent(request) {
                self.meta.cancel(old);
            }
        }
    }

    /// `r`: the newest posts again, or the linked post.
    fn reload_feed(&mut self) {
        if self.feed.as_ref().is_some_and(|f| f.linked.is_some()) {
            return self.load_linked_post();
        }
        let Some(network) = self.feed.as_ref().map(|f| f.network) else {
            return;
        };
        let picture = self.feed_picture(network);
        let Some(feed) = self.feed.as_mut() else {
            return;
        };
        if let Some(load) = feed.start_load(true) {
            let request = self.meta.load_feed(
                feed.network,
                feed::PAGE,
                true,
                picture,
                feed.following,
                load,
            );
            // A next page on its way would go after the posts these replace.
            if let Some(old) = feed.sent(request) {
                self.meta.cancel(old);
            }
        }
    }

    /// `:following` / `:for-you`: Threads' feed of the accounts followed,
    /// newest first, or For you again, from the top; opened first if it
    /// isn't.
    fn show_threads_feed(&mut self, following: bool) {
        let open = self
            .feed
            .as_ref()
            .is_some_and(|f| f.network == Network::Threads && f.linked.is_none());
        if !open {
            if self.chats.get(chats::THREADS_FEED).is_none() {
                self.status = Some("Threads isn't logged in: :login logs in".into());
                return;
            }
            // Made here unless it was left, so the first page asked for is
            // this one, not For you's, called off at once.
            if !self.feeds_kept.contains_key(&Network::Threads) {
                let feed = self.new_feed(Network::Threads, following);
                self.feeds_kept.insert(Network::Threads, feed);
            }
            // Its row, in Threads' tab when the list of every chat hasn't it.
            self.chats.reveal(chats::THREADS_FEED);
            self.open_chat(chats::THREADS_FEED);
        }
        let picture = self.feed_picture(Network::Threads);
        let Some(feed) = self.feed.as_mut().filter(|f| f.network == Network::Threads) else {
            return;
        };
        if feed.following == following {
            if open {
                let which = if following { "Following" } else { "For you" };
                self.status = Some(format!("This is {which} already: r reloads it"));
            }
            return;
        }
        feed.following = following;
        // Whatever was on its way was the other feed's.
        feed.loading = None;
        if let Some(load) = feed.start_load(true) {
            let request = self.meta.load_feed(
                Network::Threads,
                feed::PAGE,
                true,
                picture,
                feed.following,
                load,
            );
            if let Some(old) = feed.sent(request) {
                self.meta.cancel(old);
            }
        }
        self.stop_video();
        self.close_comments();
    }

    /// The largest box a post's picture is drawn in, in pixels, for
    /// `load_feed`: photos that come no bigger than they're shown are
    /// downloaded and decoded sooner, in less memory. Threads draws them
    /// smaller, side by side.
    fn feed_picture(&self, network: Network) -> Option<(u32, u32)> {
        let font = self.images.font_size();
        match network {
            Network::Threads => ui::threads::picture_box(self.chat_area, font),
            _ => ui::feed::picture_box(self.chat_area, font),
        }
    }

    /// A page of the feed came, or why it didn't. One for a feed left for
    /// a chat (or a linked post) goes in it, for when it's opened again.
    fn on_feed_page(
        &mut self,
        network: Network,
        load: u64,
        page: Result<(Vec<FeedPost>, bool), String>,
    ) {
        let open = self
            .feed
            .as_ref()
            .is_some_and(|f| f.network == network && f.linked.is_none());
        let feed = match open {
            true => self.feed.as_mut(),
            false => self.feeds_kept.get_mut(&network),
        };
        let Some(feed) = feed else {
            return;
        };
        let fresh = feed.is_fresh(load);
        let failed = page.as_ref().err().map(|why| text::clean(why));
        if !feed.on_page(load, page) || !open {
            return;
        }
        if let Some(why) = failed {
            self.status = Some(format!("Feed: {why}"));
            return;
        }
        if fresh {
            // Another post is under the cursor now.
            self.stop_video();
            self.close_comments();
            self.feed_changed = Some(Instant::now());
        }
        self.load_more_feed();
    }

    /// Enter on a post: its photo in the viewer; its video over the post
    /// inside Pepper, or in the system's player anywhere else; on a Threads
    /// post with no photo or video but a link's card, the link, as a chat's
    /// links open.
    fn open_feed_item(&mut self) {
        let Some(feed) = self.feed.as_ref() else {
            return;
        };
        let network = feed.network;
        let card = feed
            .selected_post()
            .filter(|p| p.media.is_empty())
            .and_then(|p| p.link.as_ref())
            .map(|card| Link::from(card.url.as_str()));
        let Some((post, item)) = feed.shown_item() else {
            match card {
                Some(link)
                    if !Self::feed_just_changed(
                        &mut self.feed_changed,
                        &mut self.status,
                        "Enter",
                    ) =>
                {
                    self.open_link_outside(link)
                }
                Some(_) => {}
                None => self.status = Some("Nothing to open in this post".into()),
            }
            return;
        };
        // The posts were replaced a moment ago, maybe just as the key went
        // down: the one under the cursor isn't what the user meant to open.
        if Self::feed_just_changed(&mut self.feed_changed, &mut self.status, "Enter") {
            return;
        }
        let Some(file) = item.file.clone() else {
            self.status = Some(match item.video {
                true => format!(
                    "This video can't be played here: o opens it on {}",
                    network.site()
                ),
                false => "This post can't be opened here".into(),
            });
            return;
        };
        let (post_id, audio) = (post.id, item.audio);
        if !item.video {
            self.photo_view = feed_photo(network, post_id, item);
            return;
        }
        if self
            .playing
            .as_ref()
            .is_some_and(|p| p.post_id == post_id && p.file_id == file.id)
        {
            self.pause_video();
            return;
        }
        let Some(token) = self.pepper.as_ref().and_then(|_| pepper::new_token()) else {
            self.open_target(Target::File(file));
            if audio.is_some() {
                self.status = Some(
                    "It opens without its sound: Instagram sends that apart, and only Pepper plays the two together here"
                        .into(),
                );
            }
            return;
        };
        self.stop_video();
        self.playing = Some(Playing {
            post_id,
            file_id: file.id,
            token,
            video: None,
            audio: audio.map(|id| (id, None)),
            player: None,
            paused: false,
            muted: false,
            position: None,
            duration: None,
            full_screen: false,
        });
        // The helper answers at once if they're already downloaded.
        self.meta.download(file.id);
        if let Some(audio) = audio {
            self.meta.download(audio);
        }
    }

    /// The posts were replaced a moment ago, maybe just as `again` went
    /// down: the one under the cursor isn't what the user meant to open, so
    /// it asks for that key again. Fields rather than `self`, so the post can
    /// stay borrowed.
    fn feed_just_changed(
        changed: &mut Option<Instant>,
        status: &mut Option<String>,
        again: &str,
    ) -> bool {
        let just = changed
            .take()
            .is_some_and(|at| at.elapsed() < CONFIRM_GRACE);
        if just {
            *status = Some(format!(
                "The feed just changed: press {again} again to open it"
            ));
        }
        just
    }

    /// `h` / `l` in the viewer over the feed: the photo before or after,
    /// through the carousels and the posts, loading more past the last.
    fn view_next_feed_photo(&mut self, post_id: i64, file_id: i32, newer: bool) {
        let Some(feed) = self.feed.as_mut() else {
            return;
        };
        let (before, network) = (feed.selected, feed.network);
        match feed.next_photo(post_id, file_id, newer) {
            Some((post_id, item)) => {
                if feed.selected != before {
                    self.stop_video();
                    self.close_comments();
                }
                if let Some(view) = feed_photo(network, post_id, &item) {
                    self.photo_view = Some(view);
                }
                self.load_more_feed();
            }
            None if newer && feed.has_more => {
                self.status = Some("Loading more posts…".into());
                feed.selected = feed.posts.len().saturating_sub(1);
                self.load_more_feed();
            }
            None if newer => self.status = Some("No more photos in the feed".into()),
            None => self.status = Some("No photos before this one".into()),
        }
    }

    /// `o`: the post on its network's website, where it can be liked or
    /// answered.
    fn open_post_outside(&mut self) {
        match self.feed_link() {
            Some(link) => self.open_link_outside(link),
            None => self.status = Some("This post has no link".into()),
        }
    }

    /// `y`: copies the post's link.
    fn copy_post_link(&mut self) {
        match self.feed_link() {
            Some(link) => self.copy_target(Target::Link(link)),
            None => self.status = Some("This post has no link".into()),
        }
    }

    /// `c`: the selected post's comments (Threads' replies), in a pane on
    /// its right, which takes the keys; `c` again closes it. The post stays
    /// in view, and its video plays on.
    fn toggle_comments(&mut self) {
        let Some(feed) = self.feed.as_ref() else {
            return;
        };
        let network = feed.network;
        let Some(post) = feed.selected_post() else {
            return;
        };
        if self.comments.as_ref().is_some_and(|c| c.post_id == post.id) {
            self.close_comments();
            return;
        }
        let author = match post.username.is_empty() {
            true => post.name.clone(),
            false => format!("@{}", post.username),
        };
        let mut comments = Comments::new(post.id, &author);
        if network == Network::Threads {
            comments.what = "Replies";
        }
        if let Some(load) = comments.start_load(None, false) {
            comments.request = Some(self.meta.load_comments(post.id, None, false, load));
        }
        self.comments = Some(comments);
        self.comments_focus = true;
    }

    fn close_comments(&mut self) {
        // Nobody waits for the page on its way any more: the helper needn't
        // ask the network for it.
        if let Some(request) = self.comments.take().and_then(|c| c.request) {
            self.meta.cancel(request);
        }
        self.comments_focus = false;
    }

    /// A key in the comments pane, while it has the keys. False if it's one
    /// every pane has (quitting, help, commands, the picker, jumps, going to
    /// the chat list), for those to take; the rest act on the comments, and
    /// Space and `m` on the video playing beside them.
    fn on_comments_key(&mut self, key: KeyEvent, ctrl: bool, pending_g: bool) -> bool {
        let to_list = match self.settings.chat_list_side {
            Side::Left => 'h',
            Side::Right => 'l',
        };
        let Some(comments) = self.comments.as_mut() else {
            return false;
        };
        let motion = match key.code {
            KeyCode::Char('j') | KeyCode::Down => Some(1),
            KeyCode::Char('k') | KeyCode::Up => Some(-1),
            KeyCode::Char('d') if ctrl => Some(HALF_PAGE / 2),
            KeyCode::Char('u') if ctrl => Some(-HALF_PAGE / 2),
            KeyCode::Char('g') if pending_g => Some(isize::MIN),
            KeyCode::Char('G') => Some(isize::MAX),
            _ => None,
        };
        if let Some(delta) = motion {
            comments.move_cursor(delta);
            self.load_more_comments();
            return true;
        }
        match key.code {
            _ if ctrl => return false,
            // `gg`, as in every pane.
            KeyCode::Char('g') => self.pending_g = true,
            // One page at a time, as the helper takes them.
            KeyCode::Enter if comments.loading.is_some() => {
                self.status = Some("Still loading: press Enter again in a moment".into());
            }
            KeyCode::Enter => {
                if let Replies::Load { of, more } = comments.enter()
                    && let Some(load) = comments.start_load(Some(of), more)
                {
                    comments.request =
                        Some(
                            self.meta
                                .load_comments(comments.post_id, Some(of), more, load),
                        );
                }
            }
            KeyCode::Char('y') => {
                if let Some(text) = comments.selected_comment().map(|c| c.text.clone()) {
                    self.copy_target(Target::Text(text));
                }
            }
            KeyCode::Char('o') => self.open_post_outside(),
            KeyCode::Char(' ') => self.pause_video(),
            KeyCode::Char('m') => self.mute_video(),
            KeyCode::Char(',') => self.seek_video(-player::SEEK_STEP),
            KeyCode::Char('.') => self.seek_video(player::SEEK_STEP),
            // The post is on the pane's left.
            KeyCode::Char('h') => self.comments_focus = false,
            KeyCode::Char('f') => self.full_screen_video(),
            KeyCode::Esc | KeyCode::Char('c') => self.close_comments(),
            KeyCode::Char(c) if c == to_list => return false,
            KeyCode::Char('q' | '?' | ':' | 's') | KeyCode::Tab => return false,
            _ => {}
        }
        true
    }

    /// The next page of top comments, once the cursor nears the last.
    fn load_more_comments(&mut self) {
        let Some(comments) = self.comments.as_mut() else {
            return;
        };
        if comments.wants_more()
            && let Some(load) = comments.start_load(None, true)
        {
            comments.request = Some(self.meta.load_comments(comments.post_id, None, true, load));
        }
    }

    /// A page of comments or replies came, or why it didn't.
    fn on_comments_page(
        &mut self,
        post_id: i64,
        load: u64,
        page: Result<(Vec<CommentInfo>, bool), String>,
    ) {
        let Some(comments) = self.comments.as_mut().filter(|c| c.post_id == post_id) else {
            return;
        };
        let failed = page.as_ref().err().map(|why| text::clean(why));
        if !comments.on_page(load, page) {
            return;
        }
        match failed {
            Some(why) => self.status = Some(format!("Comments: {why}")),
            None => self.load_more_comments(),
        }
    }

    fn feed_link(&self) -> Option<Link> {
        let feed = self.feed.as_ref()?;
        // A linked post that didn't come is still at its link.
        let url = feed
            .selected_post()
            .and_then(|p| p.url.as_deref())
            .or(feed.linked.as_ref().map(|l| l.url.as_str()))?;
        Some(Link::from(url))
    }

    /// A download finished: if it's the video waiting to play, or its
    /// sound, it plays once both are there. A sound that didn't come leaves
    /// it to play without.
    fn on_video_downloaded(&mut self, file_id: i32, path: Option<&str>) {
        let Some(playing) = self.playing.as_mut().filter(|p| p.player.is_none()) else {
            return;
        };
        let sound = playing.audio.as_ref().is_some_and(|(id, _)| *id == file_id);
        match path {
            Some(path) if file_id == playing.file_id => playing.video = Some(path.into()),
            Some(path) if sound => playing.audio = Some((file_id, Some(path.into()))),
            None if file_id == playing.file_id => {
                self.playing = None;
                self.status = Some("Couldn't download the video".into());
                return;
            }
            None if sound => {
                playing.audio = None;
                self.status = Some("The video's sound didn't download: it plays without".into());
            }
            _ => return,
        }
        let Some(video) = playing.video.clone() else {
            return;
        };
        let audio = match &playing.audio {
            Some((_, None)) => return,
            Some((_, Some(path))) => Some(path.clone()),
            None => None,
        };
        let Some(pepper) = self.pepper.as_ref() else {
            self.playing = None;
            return;
        };
        self.plays += 1;
        match Player::start(
            pepper,
            &playing.token,
            Path::new(&video),
            audio.as_deref().map(Path::new),
            self.plays,
            self.player_tx.clone(),
        ) {
            Ok(player) => playing.player = Some(player),
            Err(e) => {
                self.playing = None;
                self.status = Some(format!("Can't play the video: {e:#}"));
            }
        }
    }

    /// News from pepper-play about the video playing; any other is over.
    fn on_player(&mut self, event: PlayerEvent) {
        let now = self
            .playing
            .as_ref()
            .and_then(|p| p.player.as_ref())
            .map(|p| p.play);
        match event {
            PlayerEvent::Failed { play, why } if now == Some(play) => {
                self.status = Some(format!("Can't play the video: {why}"));
                self.stop_video();
            }
            PlayerEvent::Position { play, seconds } if now == Some(play) => {
                if let Some(playing) = self.playing.as_mut() {
                    playing.position = Some(seconds);
                }
            }
            PlayerEvent::Duration { play, seconds } if now == Some(play) => {
                if let Some(playing) = self.playing.as_mut() {
                    playing.duration = Some(seconds);
                }
            }
            PlayerEvent::Exited { play } if now == Some(play) => self.playing = None,
            _ => {}
        }
    }

    /// Stops the video playing, or waiting to: its player goes, and with it
    /// Pepper's box after the next frame.
    fn stop_video(&mut self) {
        self.playing = None;
    }

    /// Space: pauses the video playing, or plays it on.
    fn pause_video(&mut self) {
        match self.playing.as_mut() {
            Some(playing) if playing.player.is_some() => {
                if let Some(player) = &playing.player {
                    player.toggle_pause();
                }
                playing.paused = !playing.paused;
            }
            Some(_) => {}
            None => self.status = Some("No video playing: Enter plays one".into()),
        }
    }

    /// `,` / `.`: the video playing [`player::SEEK_STEP`] seconds back or on.
    fn seek_video(&mut self, seconds: i32) {
        match self.playing.as_ref().and_then(|p| p.player.as_ref()) {
            Some(player) => player.seek(seconds),
            None => self.status = Some("No video playing: Enter plays one".into()),
        }
    }

    /// `m`: the video's sound off, or on again.
    fn mute_video(&mut self) {
        match self.playing.as_mut() {
            Some(playing) if playing.player.is_some() => {
                if let Some(player) = &playing.player {
                    player.toggle_mute();
                }
                playing.muted = !playing.muted;
            }
            Some(_) => {}
            None => self.status = Some("No video playing".into()),
        }
    }

    /// The video playing fills the window (`f`).
    pub fn full_screen(&self) -> bool {
        self.playing.as_ref().is_some_and(|p| p.full_screen)
    }

    /// `f`: the video under the cursor fills the window, playing as Enter
    /// plays it (anywhere but Pepper, in the system's player, which has a
    /// full screen of its own); `f` again puts it back over its post.
    fn full_screen_video(&mut self) {
        if let Some(playing) = self.playing.as_mut() {
            playing.full_screen = !playing.full_screen;
            return;
        }
        let video = self
            .feed
            .as_ref()
            .and_then(|f| f.shown_item())
            .is_some_and(|(_, item)| item.video);
        if !video {
            self.status = Some("No video here to play full screen".into());
            return;
        }
        // As Enter asks again, but for `f`, so the second one fills the
        // window too.
        if Self::feed_just_changed(&mut self.feed_changed, &mut self.status, "f") {
            return;
        }
        self.open_feed_item();
        if let Some(playing) = self.playing.as_mut() {
            playing.full_screen = true;
        }
    }

    /// A key while the video fills the window: its own, and quitting, help
    /// and commands; nothing else, since what it would act on is under the
    /// video. False for those three, for Normal mode to take.
    fn on_full_screen_key(&mut self, key: KeyEvent, ctrl: bool) -> bool {
        match key.code {
            _ if ctrl => {}
            KeyCode::Char('f') | KeyCode::Esc => self.full_screen_video(),
            KeyCode::Char(' ') => self.pause_video(),
            KeyCode::Char('m') => self.mute_video(),
            KeyCode::Char(',') => self.seek_video(-player::SEEK_STEP),
            KeyCode::Char('.') => self.seek_video(player::SEEK_STEP),
            KeyCode::Char('q' | '?' | ':') => return false,
            _ => {}
        }
        true
    }

    /// What to tell Pepper after a frame: where the video playing shows now
    /// (`OSC 777;pepper`), or that it's gone, if that changed. Under a
    /// popup, or with its post out of view, it's taken away: it would cover
    /// what's drawn there. Empty when nothing changed.
    pub fn video_placement(&mut self) -> String {
        let shown = matches!(self.screen, Screen::Main) && !self.popup_over_chat();
        let wanted = self
            .playing
            .as_ref()
            .filter(|p| p.player.is_some())
            .zip(self.video_box.filter(|_| shown))
            .map(|(p, area)| Placed {
                token: p.token.clone(),
                area,
            });
        let out = pepper::tell(self.placed.as_ref(), wanted.as_ref());
        self.placed = wanted;
        out
    }
}

/// The viewer on a photo of `network`'s feed's post `post_id`.
fn feed_photo(network: Network, post_id: i64, item: &feed::Item) -> Option<PhotoView> {
    let file = item.file.clone().filter(|_| !item.video)?;
    let (width, height) = item
        .still
        .as_ref()
        .map_or((1080, 1080), |s| (s.width, s.height));
    Some(PhotoView {
        chat_id: chats::feed_id(network)?,
        message_id: post_id,
        photo: Preview {
            file_id: file.id,
            width,
            height,
            thumbnail: None,
            sticker: false,
        },
        // The feed's smaller copy shows until the viewer's is downloaded.
        stand_in: item.still.clone().filter(|s| s.file_id != file.id),
        file,
        zoom: crate::viewer::FILLING,
    })
}

/// SIGHUP (the terminal closed, an SSH connection dropped) and SIGTERM, one
/// message each, so tuimeta stops the helper before it goes.
fn quit_signals() -> UnboundedReceiver<()> {
    let (tx, rx) = tokio::sync::mpsc::unbounded_channel();
    #[cfg(unix)]
    tokio::spawn(async move {
        use tokio::signal::unix::{SignalKind, signal};
        let (Ok(mut hangup), Ok(mut terminate)) = (
            signal(SignalKind::hangup()),
            signal(SignalKind::terminate()),
        ) else {
            return;
        };
        loop {
            tokio::select! {
                _ = hangup.recv() => {}
                _ = terminate.recv() => {}
            }
            if tx.send(()).is_err() {
                return;
            }
        }
    });
    #[cfg(not(unix))]
    drop(tx);
    rx
}

/// Hands a file path or web link to the system: the default app for a file
/// (Preview for images on macOS, QuickTime for video…), the browser for a link.
/// No shell is involved, so a `&` or `|` in a link stays part of it: Windows
/// uses ShellExecute rather than `cmd /C start`, which would run what follows.
fn open_externally(target: &str) -> std::io::Result<()> {
    // `open` on macOS waits for LaunchServices, which can take a moment, and
    // reports nothing useful, so it gets a thread of its own.
    if cfg!(target_os = "macos") {
        let target = target.to_string();
        std::thread::spawn(move || open::that_detached(target));
        Ok(())
    } else {
        open::that_detached(target)
    }
}

/// Marks a downloaded file as coming from the internet, as browsers do, so
/// the system's own checks apply when it's opened: Gatekeeper on macOS,
/// SmartScreen and Office's Protected View on Windows. Best effort.
fn mark_downloaded(path: &str) {
    #[cfg(target_os = "macos")]
    {
        use std::process::{Command, Stdio};
        let now = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map_or(0, |d| d.as_secs());
        let _ = Command::new("/usr/bin/xattr")
            .args([
                "-w",
                "com.apple.quarantine",
                &format!("0081;{now:x};tuimeta;"),
                path,
            ])
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .status();
    }
    #[cfg(windows)]
    {
        let _ = std::fs::write(
            format!("{path}:Zone.Identifier"),
            "[ZoneTransfer]\r\nZoneId=3\r\n",
        );
    }
    #[cfg(not(any(target_os = "macos", windows)))]
    let _ = path;
}

/// A one-line prompt input holding `text`, with the cursor at its end.
fn prompt_input(text: &str) -> TextArea<'static> {
    let mut input = TextArea::new(vec![text.to_string()]);
    input.set_cursor_line_style(Style::default());
    input.set_cursor_style(Style::default().reversed());
    input.move_cursor(ratatui_textarea::CursorMove::End);
    input
}

fn login_screen(step: LoginStep) -> Screen {
    Screen::Login(Box::new(Login::new(step)))
}

pub fn new_composer() -> TextArea<'static> {
    let mut composer = TextArea::default();
    composer.set_cursor_line_style(Style::default());
    composer
}

/// Seconds since 1970, like the dates the helper gives.
fn unix_now() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |d| d.as_secs() as i64)
}

#[cfg(test)]
mod tests {
    use ratatui::Terminal;
    use ratatui::backend::TestBackend;
    use ratatui::style::Color;
    use ratatui_image::picker::Picker;
    use tokio::sync::mpsc::unbounded_channel;

    use super::*;
    use crate::chats::FEED;
    use crate::meta::{ChatInfo, ChatKind, Message, Network};

    fn press(app: &mut App, code: KeyCode, modifiers: KeyModifiers) {
        app.on_key(KeyEvent::new(code, modifiers));
    }

    /// Types `:name` and runs it.
    fn command(app: &mut App, name: &str) {
        press(app, KeyCode::Char(':'), KeyModifiers::NONE);
        for c in name.chars() {
            press(app, KeyCode::Char(c), KeyModifiers::NONE);
        }
        press(app, KeyCode::Enter, KeyModifiers::NONE);
    }

    /// The screen's rows as text.
    fn screen(app: &mut App) -> Vec<String> {
        screen_of(app, 100)
    }

    fn screen_of(app: &mut App, width: u16) -> Vec<String> {
        let mut terminal = Terminal::new(TestBackend::new(width, 20)).unwrap();
        terminal.draw(|f| ui::draw(f, app)).unwrap();
        let buf = terminal.backend().buffer();
        (0..buf.area.height)
            .map(|y| (0..buf.area.width).map(|x| buf[(x, y)].symbol()).collect())
            .collect()
    }

    /// The demo app, with its data in a temp folder of its own.
    fn test_app(tag: &str) -> App {
        // One folder per test, reused by later runs rather than piling up.
        let dir = std::env::temp_dir().join(format!("tuimeta-test-{tag}"));
        std::fs::create_dir_all(&dir).unwrap();
        let images = Images::new(Picker::halfblocks(), unbounded_channel().0);
        crate::demo::demo_app(Meta::detached(unbounded_channel().0), images, &dir)
    }

    /// The requests the app made, by method.
    fn sent(app: &App) -> Vec<String> {
        app.meta.sent_methods()
    }

    /// The params of the requests made with `method`.
    fn sent_with(app: &App, method: &str) -> Vec<serde_json::Value> {
        app.meta
            .sent()
            .into_iter()
            .filter(|(m, _)| m == method)
            .map(|(_, params)| params)
            .collect()
    }

    /// A message as the helper sends it.
    fn message(fields: serde_json::Value) -> Message {
        crate::messages::tests::meta_message(fields)
    }

    /// A message from someone else, just now, in the open chat.
    fn incoming(app: &App, id: i64, text: &str) -> Message {
        let chat_id = app.open.as_ref().unwrap().chat_id;
        message(
            serde_json::json!({"id": id, "chat_id": chat_id, "sender_id": 2,
            "date": unix_now(), "text": text}),
        )
    }

    #[test]
    fn the_look_rows_change_only_their_own_settings() {
        let dir = std::env::temp_dir().join(format!("tuimeta-look-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let images = Images::new(Picker::halfblocks(), unbounded_channel().0);
        let meta = Meta::detached(unbounded_channel().0);
        let mut app = crate::demo::demo_app(meta, images, &dir);
        let none = KeyModifiers::NONE;
        let saved = || Settings::load(&settings::path(&dir)).unwrap();
        press(&mut app, KeyCode::Char('?'), none);
        press(&mut app, KeyCode::Tab, none);
        // In the order drawn, between the gaps and the composer's rows.
        let selected = |app: &App| app.settings_menu.as_ref().unwrap().selected;
        app.settings_menu.as_mut().unwrap().selected = SettingsMenu::BLOCK_GAPS;
        press(&mut app, KeyCode::Char('j'), none);
        assert_eq!(selected(&app), SettingsMenu::CORNERS);
        let rounded = app.rounded;
        press(&mut app, KeyCode::Enter, none);
        assert_eq!(app.rounded, !rounded, "at once");
        let expected = if rounded {
            Corners::Square
        } else {
            Corners::Rounded
        };
        assert_eq!(saved().corners, expected, "kept, whatever the terminal");

        press(&mut app, KeyCode::Char('j'), none);
        assert_eq!(selected(&app), SettingsMenu::PILLS);
        press(&mut app, KeyCode::Enter, none);
        assert!(saved().nerd_font);
        let status = screen(&mut app).pop().unwrap();
        assert!(status.starts_with("\u{e0b6}NORMAL\u{e0b4}"), "{status}");

        let after = saved();
        assert!(!after.normal_after_send, "{after:?}");
        assert!(after.block_gaps, "{after:?}");
        std::fs::remove_dir_all(&dir).unwrap();
    }

    #[test]
    fn getting_focus_back_runs_lights_round_the_pane_with_the_keys() {
        let dir = std::env::temp_dir().join(format!("tuimeta-lights-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let images = Images::new(Picker::halfblocks(), unbounded_channel().0);
        let meta = Meta::detached(unbounded_channel().0);
        let mut app = crate::demo::demo_app(meta, images, &dir);
        app.focus = Focus::Chats;
        // The chat list's bottom left corner, where no light starts.
        let corner = |app: &mut App| {
            let mut terminal = Terminal::new(TestBackend::new(100, 20)).unwrap();
            terminal.draw(|f| ui::draw(f, app)).unwrap();
            terminal.backend().buffer()[(0, 18)].fg
        };
        assert_eq!(corner(&mut app), app.colors.accent);

        app.on_terminal_event(Event::FocusLost);
        assert_eq!(app.motion.focused, None);
        app.on_terminal_event(Event::FocusGained);
        assert!(app.motion.focused.is_some());
        // Held at its start, however slowly the test runs.
        app.motion.focused = Some(Instant::now() + Duration::from_secs(60));
        assert_eq!(corner(&mut app), app.colors.border, "not come round yet");

        // With a popup up, the keys are in it, and the pane stays as it is.
        press(&mut app, KeyCode::Char('?'), KeyModifiers::NONE);
        assert_eq!(corner(&mut app), app.colors.accent);
        press(&mut app, KeyCode::Esc, KeyModifiers::NONE);

        app.motion.focused = None;
        app.settings.animations = false;
        app.on_terminal_event(Event::FocusGained);
        assert_eq!(app.motion.focused, None, "turned off in ?");
        std::fs::remove_dir_all(&dir).unwrap();
    }

    #[test]
    fn the_lights_run_round_a_feed_with_the_keys_too() {
        let dir = std::env::temp_dir().join(format!("tuimeta-feed-lights-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let images = Images::new(Picker::halfblocks(), unbounded_channel().0);
        let meta = Meta::detached(unbounded_channel().0);
        let mut app = crate::demo::demo_app(meta, images, &dir);
        app.open = None;
        app.feed = Some(crate::feed::Feed::new(Network::Instagram));
        app.focus = Focus::Messages;
        // The feed's bottom left corner, where no light starts.
        let corner = |app: &mut App| {
            let mut terminal = Terminal::new(TestBackend::new(100, 20)).unwrap();
            terminal.draw(|f| ui::draw(f, app)).unwrap();
            let pane = app.chat_area;
            terminal.backend().buffer()[(pane.x, pane.bottom() - 1)].fg
        };
        assert_eq!(corner(&mut app), app.colors.accent);
        app.on_terminal_event(Event::FocusGained);
        assert!(app.motion.focused.is_some());
        app.motion.focused = Some(Instant::now() + Duration::from_secs(60));
        assert_eq!(corner(&mut app), app.colors.border, "not come round yet");
        std::fs::remove_dir_all(&dir).unwrap();
    }

    #[test]
    fn a_toast_replaced_while_up_stays_in_place() {
        let dir = std::env::temp_dir().join(format!("tuimeta-toast-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let images = Images::new(Picker::halfblocks(), unbounded_channel().0);
        let meta = Meta::detached(unbounded_channel().0);
        let mut app = crate::demo::demo_app(meta, images, &dir);
        app.show_toast("Copied", "first");
        let shown = app.toast.as_ref().unwrap().shown;
        app.show_toast("Copied", "second");
        let toast = app.toast.as_ref().unwrap();
        assert_eq!(toast.detail, "second");
        assert_eq!(toast.shown, shown, "not sliding in again");

        // One on its way out comes back in from where it got to.
        let now = Instant::now();
        let until = now + motion::SLIDE / 2;
        app.toast.as_mut().unwrap().until = until;
        let out = motion::toast_out(shown, until, now);
        app.show_toast("Copied", "third");
        let back = motion::toast_out(app.toast.as_ref().unwrap().shown, until + TOAST_TIME, now);
        assert!((back - out).abs() < 0.1, "{out} then {back}");
        std::fs::remove_dir_all(&dir).unwrap();
    }

    #[test]
    fn the_chat_list_can_go_on_the_right_where_h_and_l_follow_it() {
        let dir = std::env::temp_dir().join(format!("tuimeta-side-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let images = Images::new(Picker::halfblocks(), unbounded_channel().0);
        let meta = Meta::detached(unbounded_channel().0);
        let mut app = crate::demo::demo_app(meta, images, &dir);
        app.focus = Focus::Chats;
        let none = KeyModifiers::NONE;
        let saved = || Settings::load(&settings::path(&dir)).unwrap();
        // The text left and right of the first pane's top right corner,
        // round or square as the terminal running the tests has them.
        let halves = |app: &mut App| {
            let top = screen(app).remove(0);
            let (left, right) = top.split_once(['╮', '┐']).unwrap();
            (left.to_string(), right.to_string())
        };

        press(&mut app, KeyCode::Char('?'), none);
        press(&mut app, KeyCode::Tab, none);
        app.settings_menu.as_mut().unwrap().selected = SettingsMenu::LIST_RIGHT;
        press(&mut app, KeyCode::Char(' '), none);
        assert_eq!(saved().chat_list_side, Side::Right, "saved at once");
        press(&mut app, KeyCode::Enter, none);
        assert_eq!(saved().chat_list_side, Side::Left, "Enter toggles too");
        press(&mut app, KeyCode::Enter, none);
        press(&mut app, KeyCode::Esc, none);
        assert!(app.settings_menu.is_none());
        let (left, right) = halves(&mut app);
        assert!(
            left.contains("Weekend Hike") && right.contains("Chats ("),
            "{left}╮{right}"
        );

        press(&mut app, KeyCode::Char('l'), none);
        assert!(app.focus == Focus::Chats, "l points away from the chat");
        press(&mut app, KeyCode::Char('h'), none);
        assert!(
            app.focus == Focus::Messages,
            "h goes to the chat, on the left"
        );
        let status = screen_of(&mut app, 300).pop().unwrap();
        assert!(
            status.contains("l back") && !status.contains("h back"),
            "{status}"
        );
        press(&mut app, KeyCode::Char('l'), none);
        assert!(app.focus == Focus::Chats, "l goes back to the list");

        // h moves the line left, which widens a list on the right.
        press(&mut app, KeyCode::Char('r'), KeyModifiers::CONTROL);
        press(&mut app, KeyCode::Char('h'), none);
        assert_eq!(app.settings.list_width(), 40);
        press(&mut app, KeyCode::Esc, none);
        std::fs::remove_dir_all(&dir).unwrap();
    }

    #[test]
    fn the_newest_message_is_marked_read_only_while_you_look_at_it() {
        let mut app = test_app("read");
        let chat_id = app.open.as_ref().unwrap().chat_id;
        app.focus = Focus::Chats;
        app.on_meta(MetaEvent::Message(Box::new(incoming(&app, 1 << 60, "hi"))));
        app.mark_seen();
        assert!(sent(&app).is_empty(), "the chat list has the keys");

        app.focus = Focus::Messages;
        app.settings_menu = Some(SettingsMenu::new(HelpTab::Shortcuts, &app.settings));
        app.mark_seen();
        assert!(sent(&app).is_empty(), "a popup covers it");
        app.settings_menu = None;

        // A popup the user opens without leaving the newest message (the
        // picker, a prompt, the resize bar) still covers the chat: a message
        // that arrives behind it isn't seen, so no receipt goes out.
        app.picker = Some(crate::picker::ChatPicker::new());
        app.mark_seen();
        assert!(sent(&app).is_empty(), "the finder covers it");
        app.picker = None;

        app.open.as_mut().unwrap().selected = Some(SUNNY_IN_DEMO);
        app.mark_seen();
        assert!(sent(&app).is_empty(), "the cursor is on an older message");
        app.open.as_mut().unwrap().selected = None;

        app.mark_seen();
        app.mark_seen();
        let read = sent_with(&app, "mark_read");
        assert_eq!(read.len(), 1, "once: {read:?}");
        assert_eq!(read[0]["chat_id"], chat_id);
        assert_eq!(read[0]["message_id"], 1_i64 << 60, "up to the newest");

        // Your own messages aren't read receipts' business.
        let mut mine = incoming(&app, (1 << 60) + 1, "me");
        mine.outgoing = true;
        app.on_meta(MetaEvent::Message(Box::new(mine)));
        app.mark_seen();
        assert_eq!(sent_with(&app, "mark_read").len(), 1);
    }

    /// A message the demo's hike chat has, older than the newest.
    const SUNNY_IN_DEMO: i64 = 1;

    #[test]
    fn nothing_is_marked_read_under_the_emoji_suggestions() {
        let mut app = test_app("read-suggest");
        let chat_id = app.open.as_ref().unwrap().chat_id;
        app.focus = Focus::Input;
        for c in "so :smi".chars() {
            press(&mut app, KeyCode::Char(c), KeyModifiers::NONE);
        }
        assert!(app.completion.as_ref().is_some_and(|c| !c.items.is_empty()));
        assert!(!app.watching(chat_id), "they cover the newest message");
        app.on_meta(MetaEvent::Message(Box::new(incoming(&app, 1 << 60, "hi"))));
        app.mark_seen();
        assert!(sent_with(&app, "mark_read").is_empty());
        press(&mut app, KeyCode::Char(' '), KeyModifiers::NONE);
        assert!(app.completion.as_ref().is_none_or(|c| c.items.is_empty()));
        app.mark_seen();
        assert_eq!(sent_with(&app, "mark_read").len(), 1, "once they're gone");
    }

    #[test]
    fn pictures_that_paint_over_cells_are_cleared_whenever_something_stops_drawing_them() {
        let mut app = test_app("clear-pictures");
        let none = KeyModifiers::NONE;
        app.focus = Focus::Messages;
        let clears = |app: &mut App| app.clear_before_drawing(true);
        assert!(!clears(&mut app), "the first frame has nothing to clear");
        assert!(!clears(&mut app), "nor one like the last");

        let link = || Confirmed::OpenLink("https://example.com".into());
        for _ in 0..2 {
            app.confirm = Some(Confirm::new("Open this link?", vec![], link()));
            assert!(clears(&mut app), "a warning over the chat");
            assert!(!clears(&mut app));
            app.confirm = None;
            assert!(clears(&mut app), "the pictures come back");
        }
        press(&mut app, KeyCode::Char('R'), none);
        assert!(app.react_menu.is_some());
        assert!(clears(&mut app), "the reaction popup");
        press(&mut app, KeyCode::Esc, none);
        assert!(clears(&mut app));

        app.ask_to_log_in();
        assert!(clears(&mut app), "the login screen");
        app.screen = Screen::Main;
        assert!(clears(&mut app));

        let (_, id) = photo_message(&mut app);
        press(&mut app, KeyCode::Enter, none);
        assert!(clears(&mut app), "the viewer");
        press(&mut app, KeyCode::Char('k'), none);
        assert!(clears(&mut app), "zoomed");
        assert!(!clears(&mut app));
        press(&mut app, KeyCode::Esc, none);
        assert!(clears(&mut app));
        assert_eq!(app.open.as_ref().unwrap().selected, Some(id));

        // Pictures drawn in cells (half blocks) go with them.
        app.confirm = Some(Confirm::new("Open this link?", vec![], link()));
        assert!(!app.clear_before_drawing(false));
        assert!(!must_clear(None, app.layers(), true));
    }

    #[test]
    fn a_message_request_is_read_without_telling_the_sender_until_it_is_accepted() {
        let mut app = test_app("request-read");
        let chat = |request: bool| {
            let info: ChatInfo = serde_json::from_value(serde_json::json!({
                "id": 900, "network": "messenger", "kind": "dm", "title": "Stranger",
                "user_id": 901, "request": request,
            }))
            .unwrap();
            MetaEvent::Chat(Box::new(info))
        };
        app.on_meta(chat(true));
        app.open_chat(900);
        let hello = message(serde_json::json!({"id": 1_i64 << 40, "chat_id": 900,
            "sender_id": 901, "date": unix_now() - 3600, "text": "hello"}));
        app.on_meta(MetaEvent::History {
            chat_id: 900,
            page: Page::Latest,
            messages: Some(vec![hello]),
        });
        assert!(app.watching(900), "on screen, as any chat");
        app.mark_seen();
        assert!(sent_with(&app, "mark_read").is_empty());

        // Accepted (on the phone, or by replying): now it's read.
        app.on_meta(chat(false));
        app.mark_seen();
        assert_eq!(
            sent_with(&app, "mark_read"),
            [serde_json::json!({"chat_id": 900, "message_id": 1_i64 << 40})]
        );
    }

    #[test]
    fn without_focus_reports_a_minute_without_a_key_counts_as_away() {
        let mut app = test_app("away");
        app.focus = Focus::Messages;
        let chat = app.open.as_ref().unwrap().chat_id;
        assert!(
            app.watching(chat) && app.sees(chat),
            "a key was just pressed"
        );

        // tmux without focus-events, or a detached session.
        // By the wall clock: Instant can't go back past boot on Windows.
        app.last_input_wall = SystemTime::now() - (IDLE_AFTER + Duration::from_secs(1));
        assert!(!app.watching(chat), "new messages aren't marked read");
        assert!(!app.sees(chat), "and they notify");

        // A terminal that reports focus is believed instead, for a while.
        app.focus_reported = true;
        assert!(app.watching(chat) && app.sees(chat));
        app.last_input_wall = SystemTime::now() - AWAY_AFTER;
        assert!(
            !app.watching(chat),
            "not for good: the screen may be left on"
        );
        assert!(app.sees(chat), "the window has focus, so no notification");
        app.last_input_wall = SystemTime::now();
        app.terminal_focused = false;
        assert!(!app.watching(chat) && !app.sees(chat));
    }

    #[test]
    fn time_the_computer_slept_counts_as_time_away() {
        let mut app = test_app("sleep");
        app.focus = Focus::Messages;
        let chat = app.open.as_ref().unwrap().chat_id;
        // A key ten seconds before a two-hour sleep: the monotonic clock
        // didn't run while asleep, the wall clock did.
        app.last_input = Instant::now();
        app.last_input_wall = SystemTime::now() - Duration::from_secs(2 * 3600);
        assert!(!app.watching(chat), "nothing is marked read on waking");

        // A wall clock set back doesn't make you away.
        app.last_input_wall = SystemTime::now() + Duration::from_secs(3600);
        assert!(app.watching(chat));
    }

    #[test]
    fn once_the_terminal_is_gone_nothing_more_is_marked_read() {
        let mut app = test_app("hang-up");
        app.focus = Focus::Messages;
        let chat = app.open.as_ref().unwrap().chat_id;
        assert!(app.watching(chat));
        app.hang_up();
        assert!(!app.watching(chat) && !app.sees(chat));
        assert!(app.exit, "without a helper there's nothing to wait for");
    }

    #[test]
    fn nothing_is_marked_read_while_a_jump_to_an_older_message_loads() {
        let mut app = test_app("jump-read");
        app.focus = Focus::Messages;
        let chat = app.open.as_ref().unwrap().chat_id;
        assert!(app.watching(chat));
        // What arrives meanwhile would be inserted at the bottom, unseen.
        app.open.as_mut().unwrap().loading = Some(Page::Around(3));
        assert!(!app.watching(chat));
    }

    #[test]
    fn typing_goes_out_while_the_composer_has_text_and_stops_once_it_is_left() {
        let mut app = test_app("typing");
        app.focus = Focus::Messages;
        let none = KeyModifiers::NONE;
        press(&mut app, KeyCode::Char('i'), none);
        assert!(sent(&app).is_empty(), "nothing typed yet");
        press(&mut app, KeyCode::Char('h'), none);
        press(&mut app, KeyCode::Char('i'), none);
        let typing = sent_with(&app, "typing");
        assert_eq!(typing.len(), 1, "once, not per key: {typing:?}");
        assert_eq!(typing[0]["typing"], true);

        press(&mut app, KeyCode::Esc, none);
        let typing = sent_with(&app, "typing");
        assert_eq!(typing.len(), 2);
        assert_eq!(typing[1]["typing"], false, "stopped on leaving Insert mode");

        // An edit to a message isn't typing.
        app.meta = Meta::detached(unbounded_channel().0);
        let id = app.open.as_ref().unwrap().newest_id().unwrap();
        app.start_edit(id, "On my way!".into());
        press(&mut app, KeyCode::Char('!'), none);
        assert!(sent_with(&app, "typing").is_empty());
    }

    #[test]
    fn enter_sends_the_text_answering_the_reply_and_follows_it() {
        let mut app = test_app("send");
        let chat_id = app.open.as_ref().unwrap().chat_id;
        app.focus = Focus::Messages;
        let none = KeyModifiers::NONE;
        app.open.as_mut().unwrap().selected = Some(SUNNY_IN_DEMO);
        press(&mut app, KeyCode::Char('r'), none);
        assert!(app.focus == Focus::Input);
        for c in "see you".chars() {
            press(&mut app, KeyCode::Char(c), none);
        }
        press(&mut app, KeyCode::Enter, none);
        let text = sent_with(&app, "send_text");
        assert_eq!(text.len(), 1);
        assert_eq!(text[0]["chat_id"], chat_id);
        assert_eq!(text[0]["text"], "see you");
        assert_eq!(text[0]["reply_to"], SUNNY_IN_DEMO);
        assert!(app.composer.is_empty());
        assert_eq!(app.open.as_ref().unwrap().selected, None, "following");
        assert!(app.open.as_ref().unwrap().reply.is_none());
    }

    #[test]
    fn files_go_to_the_helper_with_what_was_checked_and_not_if_one_was_swapped() {
        let mut app = test_app("send-files");
        app.focus = Focus::Messages;
        let none = KeyModifiers::NONE;
        let dir = std::env::temp_dir().join(format!("tuimeta-send-files-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let dir = std::fs::canonicalize(&dir).unwrap();
        let (notes, report) = (dir.join("notes.txt"), dir.join("report.pdf"));
        std::fs::write(&notes, b"mine to send").unwrap();
        std::fs::write(&report, b"%PDF-1.7").unwrap();

        // Swapped after it was listed: refused, and the composer keeps it.
        assert_eq!(app.attach(vec![report.clone()], None), 1);
        std::fs::remove_file(&report).unwrap();
        std::fs::write(&report, b"%PDF-1.7").unwrap();
        press(&mut app, KeyCode::Enter, none);
        assert!(sent_with(&app, "send_files").is_empty());
        let status = app.status.clone().unwrap_or_default();
        assert!(status.contains("report.pdf changed"), "{status}");
        app.open.as_mut().unwrap().attachments.clear();

        assert_eq!(app.attach(vec![notes.clone()], None), 1);
        let listed = app.open.as_ref().unwrap().attachments[0].to_send();
        press(&mut app, KeyCode::Enter, none);
        let sent = sent_with(&app, "send_files");
        assert_eq!(sent.len(), 1);
        assert!(sent[0].get("paths").is_none(), "not by path alone");
        assert_eq!(
            sent[0]["files"],
            serde_json::json!([serde_json::to_value(&listed).unwrap()])
        );
        assert_eq!(
            sent[0]["files"][0]["path"],
            notes.to_string_lossy().as_ref()
        );
        assert!(app.open.as_ref().unwrap().attachments.is_empty());
        std::fs::remove_dir_all(&dir).unwrap();
    }

    #[test]
    fn a_message_you_sent_takes_its_place_and_one_that_failed_says_so() {
        let mut app = test_app("sent");
        let chat_id = app.open.as_ref().unwrap().chat_id;
        let pending = message(serde_json::json!({"id": 1_i64 << 61, "chat_id": chat_id,
            "sender_id": 1, "outgoing": true, "date": unix_now(), "text": "hi",
            "state": "pending"}));
        app.on_meta(MetaEvent::Message(Box::new(pending)));
        let ids = |app: &App| app.open.as_ref().unwrap().newest_id();
        assert_eq!(ids(&app), Some(1 << 61));
        let sent = message(
            serde_json::json!({"id": (1_i64 << 61) + 5, "chat_id": chat_id,
            "sender_id": 1, "outgoing": true, "date": unix_now(), "text": "hi"}),
        );
        app.on_meta(MetaEvent::MessageSent {
            chat_id,
            old_id: 1 << 61,
            message: Box::new(sent),
        });
        assert_eq!(ids(&app), Some((1 << 61) + 5));
        app.on_meta(MetaEvent::MessageFailed {
            chat_id,
            old_id: (1 << 61) + 5,
            error: "Too fast".into(),
        });
        let open = app.open.as_ref().unwrap();
        assert_eq!(open.messages[&((1 << 61) + 5)].state, SendState::Failed);
        assert_eq!(app.status.as_deref(), Some("Message not sent: Too fast"));
    }

    #[test]
    fn d_unsends_only_your_own_messages_after_enter() {
        let mut app = test_app("unsend");
        let chat_id = app.open.as_ref().unwrap().chat_id;
        app.focus = Focus::Messages;
        let none = KeyModifiers::NONE;
        app.open.as_mut().unwrap().selected = Some(SUNNY_IN_DEMO);
        press(&mut app, KeyCode::Char('d'), none);
        assert!(app.delete_menu.is_none());
        assert_eq!(
            app.status.as_deref(),
            Some("You can only unsend your own messages")
        );

        let mine = app.open.as_ref().unwrap().newest_id().unwrap();
        app.open.as_mut().unwrap().selected = Some(mine);
        press(&mut app, KeyCode::Char('d'), none);
        assert!(app.delete_menu.is_some());
        assert!(sent(&app).is_empty(), "nothing until Enter");
        press(&mut app, KeyCode::Enter, none);
        assert_eq!(
            sent_with(&app, "delete"),
            [serde_json::json!({"chat_id": chat_id, "message_id": mine})]
        );

        // The helper says it's gone.
        app.on_meta(MetaEvent::MessagesDeleted {
            chat_id,
            message_ids: vec![mine],
        });
        assert!(!app.open.as_ref().unwrap().messages.contains_key(&mine));
    }

    #[test]
    fn r_makes_an_emoji_your_reaction_and_enter_on_yours_takes_it_back() {
        let mut app = test_app("react");
        let chat_id = app.open.as_ref().unwrap().chat_id;
        app.focus = Focus::Messages;
        let none = KeyModifiers::NONE;
        // The demo's Sunny message has your 👍.
        app.open.as_mut().unwrap().selected = Some(SUNNY_IN_DEMO);
        press(&mut app, KeyCode::Char('R'), none);
        let menu = app.react_menu.as_ref().unwrap();
        assert_eq!(menu.current(), Some("👍"), "the cursor starts on yours");
        press(&mut app, KeyCode::Enter, none);
        assert_eq!(
            sent_with(&app, "react"),
            [serde_json::json!({"chat_id": chat_id, "message_id": SUNNY_IN_DEMO, "emoji": null})]
        );

        press(&mut app, KeyCode::Char('R'), none);
        press(&mut app, KeyCode::Char('l'), none);
        let other = app
            .react_menu
            .as_ref()
            .unwrap()
            .current()
            .unwrap()
            .to_string();
        press(&mut app, KeyCode::Enter, none);
        assert_eq!(sent_with(&app, "react")[1]["emoji"], other.as_str());

        press(&mut app, KeyCode::Char('X'), none);
        assert_eq!(
            sent_with(&app, "react")[2]["emoji"],
            serde_json::Value::Null
        );
    }

    #[test]
    fn e_edits_your_text_and_an_untouched_edit_sends_nothing() {
        let mut app = test_app("edit");
        let chat_id = app.open.as_ref().unwrap().chat_id;
        app.focus = Focus::Messages;
        let none = KeyModifiers::NONE;
        let mine = app.open.as_ref().unwrap().newest_id().unwrap();
        app.open
            .as_mut()
            .unwrap()
            .messages
            .get_mut(&mine)
            .unwrap()
            .editable_until = Some(unix_now() + 60);
        press(&mut app, KeyCode::Char('e'), none);
        assert!(app.focus == Focus::Input);
        assert_eq!(app.composer.lines(), ["On my way!"]);
        press(&mut app, KeyCode::Enter, none);
        assert!(sent_with(&app, "edit_text").is_empty(), "nothing changed");

        app.focus = Focus::Messages;
        press(&mut app, KeyCode::Char('e'), none);
        press(&mut app, KeyCode::Backspace, none);
        press(&mut app, KeyCode::Enter, none);
        assert_eq!(
            sent_with(&app, "edit_text"),
            [serde_json::json!({"chat_id": chat_id, "message_id": mine, "text": "On my way"})]
        );

        // Too late: the network allows a while after sending.
        app.open
            .as_mut()
            .unwrap()
            .messages
            .get_mut(&mine)
            .unwrap()
            .editable_until = Some(unix_now() - 1);
        app.focus = Focus::Messages;
        press(&mut app, KeyCode::Char('e'), none);
        assert_eq!(
            app.status.as_deref(),
            Some("It's too late to edit this message")
        );
    }

    #[test]
    fn a_sound_picked_in_the_settings_plays_and_is_saved() {
        let dir = std::env::temp_dir().join(format!("tuimeta-sound-{}", std::process::id()));
        std::fs::create_dir_all(dir.join("sounds")).unwrap();
        std::fs::write(dir.join("sounds/broken.wav"), b"RIFF, and no more").unwrap();
        let images = Images::new(Picker::halfblocks(), unbounded_channel().0);
        let meta = Meta::detached(unbounded_channel().0);
        let mut app = crate::demo::demo_app(meta, images, &dir);
        app.over_ssh = false;
        let pick = |app: &mut App, row| {
            if app.settings_menu.is_none() {
                press(app, KeyCode::Char('?'), KeyModifiers::NONE);
                press(app, KeyCode::Tab, KeyModifiers::NONE);
            }
            app.settings_menu.as_mut().unwrap().selected = row;
            press(app, KeyCode::Enter, KeyModifiers::NONE);
        };
        // No sound, Ding, then Chime.
        pick(&mut app, SettingsMenu::SOUNDS + 2);
        assert_eq!(app.settings.notification_sound.as_deref(), Some("chime"));
        assert!(app.ringer.rang(), "played, to hear it");
        let saved = Settings::load(&settings::path(&dir)).unwrap();
        assert_eq!(saved.notification_sound.as_deref(), Some("chime"));

        // One that can't be used says why, and the last one stays.
        let broken = app.sounds.list.iter().position(|s| s.id == "broken");
        pick(&mut app, SettingsMenu::SOUNDS + 1 + broken.unwrap());
        let status = app.status.take().unwrap_or_default();
        assert!(
            status.starts_with("Sound not used: broken.wav: "),
            "{status}"
        );
        assert_eq!(app.settings.notification_sound.as_deref(), Some("chime"));
        assert!(app.sound.is_some());

        pick(&mut app, SettingsMenu::SOUNDS);
        assert_eq!(app.settings.notification_sound, None);
        assert!(app.sound.is_none());
        std::fs::remove_dir_all(&dir).unwrap();
    }

    #[test]
    fn the_sound_rings_for_each_new_message_at_most_once_a_second() {
        let mut app = test_app("ring");
        app.over_ssh = false;
        app.sound = Some(vec![100; 480].into());
        let quiet = || Ringer::new(unbounded_channel().0, chime::Output::Nowhere);
        let ring = |app: &mut App, seen| {
            app.ringer = quiet();
            app.rang_at = None;
            app.ring_for(seen);
            app.ringer.rang()
        };
        assert!(ring(&mut app, false));
        assert!(!ring(&mut app, true), "seen while focused");
        app.settings.sound_when_focused = true;
        assert!(ring(&mut app, true), "asked for while focused too");
        app.over_ssh = true;
        assert!(!ring(&mut app, false), "it'd play on the other computer");
        app.over_ssh = false;
        app.settings.sound_when_focused = false;

        // A burst rings once.
        assert!(ring(&mut app, false));
        app.ringer = quiet();
        app.ring_for(false);
        assert!(!app.ringer.rang(), "within a second of the last");
        app.rang_at = app.rang_at.map(|at| at - RING_GAP);
        app.ring_for(false);
        assert!(app.ringer.rang());

        // Each new message rings, though once a chat had a notification,
        // its next ones are held back for a while.
        app.notify_with = Notifications::Bell;
        app.notify_since = 0;
        app.focus = Focus::Chats;
        (app.focus_reported, app.terminal_focused) = (true, false);
        let chat_id = app.open.as_ref().unwrap().chat_id;
        let arrives = |app: &mut App, id, text| {
            (app.ringer, app.rang_at) = (quiet(), None);
            let message = incoming(app, id, text);
            app.on_meta(MetaEvent::Message(Box::new(message)));
            app.ringer.rang()
        };
        for (n, id) in [1 << 60, (1 << 60) + 1].into_iter().enumerate() {
            assert!(arrives(&mut app, id, "hi"), "message {n}");
            assert!(app.notifier.due(Instant::now()).is_some() == (n == 0));
        }
        assert!(!arrives(&mut app, (1 << 60) + 1, "hi!"), "an edit");
        app.chats.set_muted(chat_id, true);
        assert!(!arrives(&mut app, (1 << 60) + 2, "x"), "a muted chat");
        app.chats.set_muted(chat_id, false);

        // Seen as it came, it doesn't ring again when it changes later.
        app.terminal_focused = true;
        assert!(!arrives(&mut app, (1 << 60) + 3, "y"));
        app.terminal_focused = false;
        assert!(!arrives(&mut app, (1 << 60) + 3, "y!"), "told already");
    }

    #[test]
    fn someone_elses_message_notifies_unless_you_see_it_or_the_chat_is_muted() {
        let mut app = test_app("notify");
        app.notify_with = Notifications::Bell;
        app.notify_since = 0;
        app.focus = Focus::Chats;
        app.focus_reported = true;
        app.terminal_focused = false;
        let chat_id = app.open.as_ref().unwrap().chat_id;
        let now = Instant::now();
        app.on_meta(MetaEvent::Message(Box::new(incoming(&app, 1 << 60, "hi"))));
        assert!(app.notifier.next_at().is_none() && app.notifier.due(now).is_some());

        // An edit of the same message doesn't notify again.
        app.on_meta(MetaEvent::Message(Box::new(incoming(&app, 1 << 60, "hi!"))));
        assert!(app.notifier.due(now + Duration::from_secs(60)).is_none());

        app.chats.set_muted(chat_id, true);
        app.on_meta(MetaEvent::Message(Box::new(incoming(
            &app,
            (1 << 60) + 1,
            "x",
        ))));
        assert!(
            app.notifier.due(now + Duration::from_secs(120)).is_none(),
            "muted"
        );
        app.chats.set_muted(chat_id, false);

        // Read on the phone before it went out.
        app.on_meta(MetaEvent::Message(Box::new(incoming(
            &app,
            (1 << 60) + 2,
            "y",
        ))));
        app.on_meta(MetaEvent::Read {
            chat_id,
            inbox: Some((1 << 60) + 2),
            outbox: None,
            unread: Some(0),
        });
        assert!(app.notifier.due(now + Duration::from_secs(180)).is_none());

        app.terminal_focused = true;
        app.on_meta(MetaEvent::Message(Box::new(incoming(
            &app,
            (1 << 60) + 3,
            "z",
        ))));
        assert!(
            app.notifier.due(now + Duration::from_secs(240)).is_none(),
            "the window has focus"
        );
    }

    #[test]
    fn the_login_screen_comes_up_until_an_account_is_in() {
        let mut app = test_app("login");
        app.accounts.clear();
        app.screen = login_screen(LoginStep::Connecting);
        let account = |app: &mut App, network, state| {
            app.on_meta(MetaEvent::Account {
                network,
                state,
                user_id: Some(1),
                name: Some("Sam".into()),
                error: None,
            })
        };
        account(&mut app, Network::Messenger, AccountState::LoggedOut);
        assert!(
            matches!(&app.screen, Screen::Login(l) if matches!(l.step, LoginStep::Connecting)),
            "waiting to hear about Instagram"
        );
        account(&mut app, Network::Instagram, AccountState::LoggedOut);
        assert!(
            matches!(&app.screen, Screen::Login(l) if matches!(l.step, LoginStep::Connecting)),
            "waiting to hear about WhatsApp"
        );
        account(&mut app, Network::WhatsApp, AccountState::LoggedOut);
        assert!(
            matches!(&app.screen, Screen::Login(l) if matches!(l.step, LoginStep::Connecting)),
            "waiting to hear about Threads"
        );
        account(&mut app, Network::Threads, AccountState::LoggedOut);
        assert!(
            matches!(&app.screen, Screen::Login(l) if matches!(l.step, LoginStep::Choose { .. }))
        );

        let none = KeyModifiers::NONE;
        press(&mut app, KeyCode::Char('j'), none);
        press(&mut app, KeyCode::Enter, none);
        assert!(matches!(&app.screen,
            Screen::Login(l) if matches!(l.step, LoginStep::Cookies { network: Network::Instagram })));
        app.on_terminal_event(Event::Paste("sessionid=a;\nds_user_id=b".into()));
        press(&mut app, KeyCode::Enter, none);
        let Screen::Login(login) = &app.screen else {
            panic!("still logging in");
        };
        assert_eq!(
            login.error.as_deref(),
            Some("These cookies are missing: csrftoken")
        );
        assert!(sent(&app).is_empty(), "not sent without them");

        app.on_terminal_event(Event::Paste("; csrftoken=c".into()));
        press(&mut app, KeyCode::Enter, none);
        let login = sent_with(&app, "login_cookies");
        assert_eq!(login[0]["network"], "instagram");
        assert_eq!(
            login[0]["cookies"],
            "sessionid=a; ds_user_id=b; csrftoken=c"
        );

        app.on_meta(MetaEvent::LoggedIn {
            network: Network::Instagram,
            attempt: app.login_attempt - 1,
            result: Err("An answer to a login given up.".into()),
        });
        let Screen::Login(login) = &app.screen else {
            panic!("still logging in");
        };
        assert!(login.busy, "old news");
        app.on_meta(MetaEvent::LoggedIn {
            network: Network::Instagram,
            attempt: app.login_attempt,
            result: Err("Instagram wants you to confirm it's you.".into()),
        });
        let Screen::Login(login) = &app.screen else {
            panic!("still logging in");
        };
        assert!(!login.busy);
        assert_eq!(
            login.error.as_deref(),
            Some("Instagram wants you to confirm it's you.")
        );

        account(&mut app, Network::Instagram, AccountState::Ready);
        assert!(matches!(app.screen, Screen::Main));
        assert_eq!(sent_with(&app, "load_chats")[0]["network"], "instagram");
    }

    /// The login screen picking a network, as when nobody is logged in.
    fn choosing(app: &mut App, network: Network) {
        let selected = Network::ALL.iter().position(|&n| n == network).unwrap();
        app.screen = login_screen(LoginStep::Choose { selected });
    }

    /// The link step's code, if one shows.
    fn link_code(app: &App) -> Option<String> {
        match &app.screen {
            Screen::Login(l) => match &l.step {
                LoginStep::Link {
                    how: Linking::Qr(code),
                    ..
                } => code.clone(),
                LoginStep::Link {
                    how: Linking::Pairing { code, .. },
                    ..
                } => code.as_ref().map(|(code, _)| code.clone()),
                _ => None,
            },
            Screen::Main => None,
        }
    }

    fn code_for(network: Network, qr: Option<&str>, pairing: Option<&str>) -> MetaEvent {
        MetaEvent::LoginCode {
            network,
            attempt: 0,
            qr: qr.map(Into::into),
            pairing: pairing.map(Into::into),
            expires: 1_700_000_060,
        }
    }

    #[test]
    fn a_code_for_a_link_given_up_never_shows_on_the_next_one() {
        let mut app = test_app("link-stale");
        let none = KeyModifiers::NONE;
        choosing(&mut app, Network::WhatsApp);
        press(&mut app, KeyCode::Enter, none);
        let first = app.login_attempt;
        press(&mut app, KeyCode::Char('p'), none);
        for c in "+1 555 010 0100".chars() {
            press(&mut app, KeyCode::Char(c), none);
        }
        press(&mut app, KeyCode::Enter, none);
        assert!(app.login_attempt > first);
        assert_eq!(
            sent_with(&app, "cancel_login"),
            [serde_json::json!({"network": "whatsapp", "attempt": first})],
            "only the first attempt is given up"
        );
        assert_eq!(
            sent_with(&app, "login_link")[1]["attempt"],
            app.login_attempt
        );
        app.on_meta(MetaEvent::LoginCode {
            network: Network::WhatsApp,
            attempt: first,
            qr: None,
            pairing: Some("OLDC-ODE1".into()),
            expires: 1_700_000_060,
        });
        assert_eq!(link_code(&app), None, "the old attempt's code showed");
        app.on_meta(MetaEvent::LoginCode {
            network: Network::WhatsApp,
            attempt: app.login_attempt,
            qr: None,
            pairing: Some("NEWC-ODE2".into()),
            expires: 1_700_000_060,
        });
        assert_eq!(link_code(&app).as_deref(), Some("NEWC-ODE2"));
    }

    #[test]
    fn enter_on_whatsapp_asks_for_a_qr_code_shows_it_and_esc_gives_it_up() {
        let mut app = test_app("link-qr");
        let none = KeyModifiers::NONE;
        choosing(&mut app, Network::WhatsApp);
        press(&mut app, KeyCode::Enter, none);
        let link = sent_with(&app, "login_link");
        assert_eq!(link.len(), 1);
        assert_eq!(link[0]["network"], "whatsapp");
        assert!(link[0]["phone"].is_null(), "a QR code first");
        assert!(sent_with(&app, "login_cookies").is_empty());
        assert!(matches!(&app.screen,
            Screen::Login(l) if l.busy && matches!(l.step, LoginStep::Link { how: Linking::Qr(None), .. })));
        assert!(screen(&mut app).join("\n").contains("Waiting for a code"));

        app.on_meta(code_for(Network::Instagram, Some("2@other"), None));
        assert_eq!(link_code(&app), None, "another network's code");
        app.on_meta(code_for(Network::WhatsApp, None, Some("ABCD-EFGH")));
        assert_eq!(link_code(&app), None, "a pairing code while scanning");
        app.on_meta(code_for(Network::WhatsApp, Some("2@first"), None));
        app.on_meta(code_for(Network::WhatsApp, Some("2@second"), None));
        assert_eq!(link_code(&app).as_deref(), Some("2@second"), "the newest");

        press(&mut app, KeyCode::Esc, none);
        assert_eq!(
            sent_with(&app, "cancel_login"),
            [serde_json::json!({"network": "whatsapp", "attempt": app.login_attempt})]
        );
        assert!(matches!(&app.screen,
            Screen::Login(l) if matches!(l.step, LoginStep::Choose { selected: 2 })));
        app.on_meta(code_for(Network::WhatsApp, Some("2@late"), None));
        assert!(
            matches!(&app.screen,
            Screen::Login(l) if matches!(l.step, LoginStep::Choose { .. })),
            "nothing waits"
        );

        // Linking again: the first link's late answer is old news.
        press(&mut app, KeyCode::Enter, none);
        app.on_meta(MetaEvent::LoggedIn {
            network: Network::WhatsApp,
            attempt: app.login_attempt - 1,
            result: Err("Linking was cancelled.".into()),
        });
        let Screen::Login(login) = &app.screen else {
            panic!("still linking");
        };
        assert!(login.busy && login.error.is_none());
    }

    #[test]
    fn an_error_about_something_else_leaves_a_link_waiting_and_esc_or_p_still_cancel_it() {
        let mut app = test_app("link-error");
        let none = KeyModifiers::NONE;
        let other_error = || {
            MetaEvent::Error(
                "Encrypted chats couldn't connect;\u{1b}[2J they'll be tried again.".into(),
            )
        };
        for leave in [KeyCode::Esc, KeyCode::Char('p')] {
            app.meta = Meta::detached(unbounded_channel().0);
            choosing(&mut app, Network::WhatsApp);
            press(&mut app, KeyCode::Enter, none);
            app.on_meta(code_for(Network::WhatsApp, Some("2@code-A"), None));
            app.on_meta(other_error());
            let Screen::Login(login) = &app.screen else {
                panic!("still linking");
            };
            assert!(login.busy, "the link still waits");
            assert_eq!(
                login.error.as_deref(),
                Some("Encrypted chats couldn't connect;[2J they'll be tried again."),
                "said, cleaned"
            );
            app.on_meta(code_for(Network::WhatsApp, Some("2@code-B"), None));
            assert_eq!(
                link_code(&app).as_deref(),
                Some("2@code-B"),
                "its codes show"
            );

            press(&mut app, leave, none);
            assert_eq!(
                sent_with(&app, "cancel_login"),
                [serde_json::json!({"network": "whatsapp", "attempt": app.login_attempt})],
                "{leave:?} gives the link up"
            );
        }
    }

    #[test]
    fn a_link_that_ran_out_says_why_and_enter_asks_for_a_fresh_code() {
        let mut app = test_app("link-timeout");
        let none = KeyModifiers::NONE;
        app.accounts.remove(&Network::WhatsApp);
        choosing(&mut app, Network::WhatsApp);
        press(&mut app, KeyCode::Enter, none);
        app.on_meta(code_for(Network::WhatsApp, Some("2@code"), None));
        app.on_meta(MetaEvent::LoggedIn {
            network: Network::WhatsApp,
            attempt: app.login_attempt,
            result: Err("Nothing was scanned in time;\u{1b}[2J try again.".into()),
        });
        let Screen::Login(login) = &app.screen else {
            panic!("still linking");
        };
        assert!(!login.busy);
        assert_eq!(
            login.error.as_deref(),
            Some("Nothing was scanned in time;[2J try again.")
        );
        assert_eq!(link_code(&app), None, "the old code works no more");
        let rows = screen(&mut app).join("\n");
        assert!(rows.contains("Enter new code"), "{rows}");
        assert!(rows.contains("No code now"), "{rows}");

        press(&mut app, KeyCode::Enter, none);
        assert_eq!(sent_with(&app, "login_link").len(), 2);
        let Screen::Login(login) = &app.screen else {
            panic!("still linking");
        };
        assert!(login.busy && login.error.is_none());

        app.on_meta(MetaEvent::Account {
            network: Network::WhatsApp,
            state: AccountState::Ready,
            user_id: Some(77),
            name: Some("+1 555 010 0100".into()),
            error: None,
        });
        assert!(matches!(app.screen, Screen::Main));
        assert_eq!(app.toast.as_ref().unwrap().detail, "WhatsApp");
        assert!(
            sent_with(&app, "load_chats")
                .iter()
                .any(|p| p["network"] == "whatsapp")
        );
    }

    #[test]
    fn the_phone_number_way_sends_the_number_and_shows_the_code_to_type() {
        let mut app = test_app("link-phone");
        let none = KeyModifiers::NONE;
        choosing(&mut app, Network::WhatsApp);
        press(&mut app, KeyCode::Enter, none);
        press(&mut app, KeyCode::Char('p'), none);
        assert_eq!(
            sent_with(&app, "cancel_login").len(),
            1,
            "the QR codes stop"
        );
        let Screen::Login(login) = &app.screen else {
            panic!("still linking");
        };
        assert!(login.takes_input() && !login.busy);

        press(&mut app, KeyCode::Enter, none);
        assert_eq!(sent_with(&app, "login_link").len(), 1, "no number yet");
        app.on_terminal_event(Event::Paste("+1 555\n010 0100".into()));
        press(&mut app, KeyCode::Char('p'), none);
        press(&mut app, KeyCode::Backspace, none);
        assert!(
            screen(&mut app).join("\n").contains("+1 555 010 0100"),
            "typed in the open"
        );
        press(&mut app, KeyCode::Enter, none);
        let link = sent_with(&app, "login_link");
        assert_eq!(link[1]["phone"], "+1 555 010 0100");

        app.on_meta(code_for(Network::WhatsApp, Some("2@qr"), None));
        assert_eq!(link_code(&app), None, "a QR code while typing a code");
        app.on_meta(code_for(Network::WhatsApp, None, Some("\u{1b}[31mEVIL")));
        assert_eq!(link_code(&app), None, "not a code");
        app.on_meta(code_for(Network::WhatsApp, None, Some("abcd-efgh")));
        assert_eq!(link_code(&app).as_deref(), Some("ABCD-EFGH"));

        // `p` again takes the number back, to change it.
        press(&mut app, KeyCode::Char('p'), none);
        assert_eq!(sent_with(&app, "cancel_login").len(), 2);
        let Screen::Login(login) = &app.screen else {
            panic!("still linking");
        };
        assert_eq!(login.input.lines().concat(), "+1 555 010 0100");
        press(&mut app, KeyCode::Esc, none);
        assert_eq!(
            sent_with(&app, "cancel_login").len(),
            2,
            "nothing waited while typing"
        );
        assert!(matches!(&app.screen,
            Screen::Login(l) if matches!(l.step, LoginStep::Choose { selected: 2 })));
    }

    #[test]
    fn a_login_names_the_browser_tm_browser_gave() {
        let mut app = test_app("login-browser");
        let none = KeyModifiers::NONE;
        let log_in = |app: &mut App| {
            app.screen = Screen::Login(Box::new(Login::new(LoginStep::Cookies {
                network: Network::Messenger,
            })));
            app.on_terminal_event(Event::Paste("c_user=1; xs=2; datr=3".into()));
            press(app, KeyCode::Enter, none);
        };
        log_in(&mut app);
        assert!(sent_with(&app, "login_cookies")[0]["browser"].is_null());

        app.browser = Some("Chrome 150.0.7712.45".into());
        log_in(&mut app);
        assert_eq!(
            sent_with(&app, "login_cookies")[1]["browser"],
            "Chrome 150.0.7712.45"
        );
    }

    #[test]
    fn logging_out_asks_names_the_network_and_forgets_its_chats() {
        let mut app = test_app("logout");
        let none = KeyModifiers::NONE;
        app.focus = Focus::Messages;
        press(&mut app, KeyCode::Char(':'), none);
        press(&mut app, KeyCode::Char('l'), none);
        press(&mut app, KeyCode::Char('o'), none);
        press(&mut app, KeyCode::Char('g'), none);
        press(&mut app, KeyCode::Char('o'), none);
        press(&mut app, KeyCode::Tab, none);
        assert_eq!(app.prompt.as_ref().unwrap().query(), "logout");
        press(&mut app, KeyCode::Enter, none);
        let confirm = app.confirm.as_mut().expect("asks");
        assert_eq!(confirm.title, "Log out of Messenger?", "the open chat's");
        confirm.shown = Instant::now().checked_sub(CONFIRM_GRACE).unwrap();
        press(&mut app, KeyCode::Char('y'), none);
        assert_eq!(sent_with(&app, "logout")[0]["network"], "messenger");

        app.on_meta(MetaEvent::Account {
            network: Network::Messenger,
            state: AccountState::LoggedOut,
            user_id: None,
            name: None,
            error: None,
        });
        assert!(app.open.is_none(), "its chat closed");
        assert!(matches!(app.screen, Screen::Main), "Instagram is still in");
        app.chats.refresh();
        assert!(
            app.chats
                .ids()
                .iter()
                .all(|&id| app.chats.network(id) != Some(Network::Messenger))
        );
    }

    #[test]
    fn logging_out_of_whatsapp_says_it_unlinks_tuimeta_and_not_the_phone() {
        let mut app = test_app("logout-whatsapp");
        let none = KeyModifiers::NONE;
        app.accounts.retain(|&n, _| n == Network::WhatsApp);
        app.open = None;
        app.selected = None;
        app.focus = Focus::Chats;
        app.ask_to_log_out();
        let confirm = app.confirm.as_ref().expect("asks");
        assert_eq!(confirm.title, "Log out of WhatsApp?");
        let said = confirm.lines.join(" ");
        assert!(
            said.contains("unlinks itself from your WhatsApp account"),
            "{said}"
        );
        assert!(
            said.contains("Your phone and your other linked devices stay"),
            "{said}"
        );
        let rows = screen(&mut app).join("\n");
        assert!(
            rows.contains("unlinks itself from your WhatsApp account and"),
            "{rows}"
        );
        assert!(
            rows.contains("other linked devices stay logged in."),
            "{rows}"
        );
        app.confirm.as_mut().unwrap().shown = Instant::now().checked_sub(CONFIRM_GRACE).unwrap();
        press(&mut app, KeyCode::Char('y'), none);
        assert_eq!(sent_with(&app, "logout")[0]["network"], "whatsapp");
        assert!(
            matches!(&app.screen,
            Screen::Login(l) if matches!(l.step, LoginStep::LoggingOut)),
            "the only one in"
        );
    }

    #[test]
    fn chats_and_people_from_the_helper_fill_the_list() {
        let mut app = test_app("chats");
        let info: ChatInfo = serde_json::from_value(serde_json::json!({
            "id": 900, "network": "instagram", "kind": "dm", "title": "New\u{202E} Friend",
            "user_id": 901, "order": i64::MAX, "unread": 1,
        }))
        .unwrap();
        app.on_meta(MetaEvent::Chat(Box::new(info)));
        app.chats.refresh();
        assert_eq!(app.chats.ids()[0], 900, "unread and newest");
        assert_eq!(
            app.users[&901], "New Friend",
            "named after the chat, cleaned"
        );
        assert!(app.chats.get(900).unwrap().is_private);

        app.on_meta(MetaEvent::Typing {
            chat_id: 900,
            user_id: 901,
            typing: true,
        });
        assert_eq!(app.chats.get(900).unwrap().activity.len(), 1);
        app.on_meta(MetaEvent::ChatRemoved(900));
        assert!(app.chats.get(900).is_none());
        let _ = ChatKind::Dm;
    }

    #[test]
    fn the_helper_stopping_is_said_in_the_status_bar() {
        let mut app = test_app("gone");
        app.on_meta(MetaEvent::Gone("tuimeta-helper stopped".into()));
        let status = app.status.clone().unwrap();
        assert!(status.starts_with("tuimeta-helper stopped."), "{status}");
        assert!(!app.exit, "the user reads it first");
        app.quit();
        app.on_meta(MetaEvent::Gone("tuimeta-helper stopped".into()));
        assert!(app.exit);
    }

    #[test]
    fn s_finds_people_on_both_networks_and_opens_a_chat_with_one() {
        let mut app = test_app("find");
        let none = KeyModifiers::NONE;
        press(&mut app, KeyCode::Char('s'), none);
        for c in "jo".chars() {
            press(&mut app, KeyCode::Char(c), none);
        }
        let due = app
            .picker
            .as_mut()
            .unwrap()
            .due_search(Instant::now() + crate::picker::SEARCH_AFTER)
            .unwrap();
        assert_eq!(due, "jo");
        let found = crate::meta::Found {
            chat_id: None,
            user_id: Some(77),
            title: "Jo".into(),
            username: Some("jo.climbs".into()),
            kind: ChatKind::Dm,
        };
        app.on_meta(MetaEvent::ChatsFound {
            network: Network::Instagram,
            query: "jo".into(),
            found: vec![found],
        });
        let rows = screen(&mut app).join("\n");
        assert!(rows.contains("@jo.climbs · Instagram"), "{rows}");
        // The demo has no chat with "jo" in its title.
        press(&mut app, KeyCode::Enter, none);
        assert_eq!(
            sent_with(&app, "open_dm"),
            [serde_json::json!({"network": "instagram", "user_id": 77})]
        );
        assert_eq!(app.finding.as_ref().map(|f| f.request.as_str()), Some("Jo"));
        app.on_meta(MetaEvent::ChatFound {
            network: Network::Instagram,
            user_id: 77,
            request: "Jo".into(),
            found: Ok(201),
        });
        assert_eq!(app.open.as_ref().unwrap().chat_id, 201);
    }

    /// Searches the picker for `query` and delivers `found` from Instagram.
    fn search_finds(app: &mut App, query: &str, found: Vec<crate::meta::Found>) {
        press(app, KeyCode::Char('s'), KeyModifiers::NONE);
        for c in query.chars() {
            press(app, KeyCode::Char(c), KeyModifiers::NONE);
        }
        let picker = app.picker.as_mut().unwrap();
        picker.due_search(Instant::now() + crate::picker::SEARCH_AFTER);
        app.on_meta(MetaEvent::ChatsFound {
            network: Network::Instagram,
            query: query.into(),
            found,
        });
    }

    #[test]
    fn a_late_answer_about_someone_picked_before_opens_nothing_even_under_the_same_name() {
        let mut app = test_app("find-same-name");
        let none = KeyModifiers::NONE;
        let person = |user_id, username: &str| crate::meta::Found {
            chat_id: None,
            user_id: Some(user_id),
            title: "Jo".into(),
            username: Some(username.into()),
            kind: ChatKind::Dm,
        };
        let both = || vec![person(77, "jo.first"), person(78, "jo.second")];
        let chat = |id, user_id| {
            let info: ChatInfo = serde_json::from_value(serde_json::json!({
                "id": id, "network": "instagram", "kind": "dm", "title": "Jo",
                "user_id": user_id,
            }))
            .unwrap();
            MetaEvent::Chat(Box::new(info))
        };
        let answer = |user_id, chat_id| MetaEvent::ChatFound {
            network: Network::Instagram,
            user_id,
            request: "Jo".into(),
            found: Ok(chat_id),
        };
        let before = app.open.as_ref().unwrap().chat_id;

        // Jo the first, then, changing one's mind, Jo the second.
        search_finds(&mut app, "jo", both());
        press(&mut app, KeyCode::Enter, none);
        search_finds(&mut app, "jo", both());
        press(&mut app, KeyCode::Down, none);
        press(&mut app, KeyCode::Enter, none);
        let asked: Vec<_> = sent_with(&app, "open_dm")
            .iter()
            .map(|p| p["user_id"].clone())
            .collect();
        assert_eq!(asked, [77, 78]);

        // The first lookup answers last but one.
        app.on_meta(chat(301, 77));
        app.on_meta(answer(77, 301));
        assert_eq!(
            app.open.as_ref().unwrap().chat_id,
            before,
            "not Jo the first"
        );
        assert!(app.finding.is_some(), "still looking up Jo the second");
        app.on_meta(chat(302, 78));
        app.on_meta(answer(78, 302));
        assert_eq!(app.open.as_ref().unwrap().chat_id, 302);
    }

    #[test]
    fn a_chat_found_while_you_write_does_not_open_over_what_you_type() {
        let mut app = test_app("late-find");
        let chat_id = app.open.as_ref().unwrap().chat_id;
        app.focus = Focus::Input;
        app.composer.insert_str("see you at 5");
        app.finding = Some(Finding::new("Bob", Network::Messenger, 5));
        app.on_meta(MetaEvent::ChatFound {
            network: Network::Messenger,
            user_id: 5,
            request: "Bob".into(),
            found: Ok(201),
        });
        assert_eq!(app.open.as_ref().unwrap().chat_id, chat_id);
        assert!(app.focus == Focus::Input);
        assert_eq!(app.composer.lines(), ["see you at 5"]);
        assert!(app.finding.is_none());
        assert_eq!(app.status.as_deref(), Some("Found Bob: press s to open it"));
    }

    #[test]
    fn writing_a_popup_or_esc_stops_a_lookup() {
        let mut app = test_app("stop-find");
        app.focus = Focus::Messages;
        app.finding = Some(Finding::new("Bob", Network::Messenger, 5));
        press(&mut app, KeyCode::Esc, KeyModifiers::NONE);
        assert!(app.finding.is_none());
        assert!(app.focus == Focus::Messages, "only the lookup stopped");

        app.finding = Some(Finding::new("Bob", Network::Messenger, 5));
        press(&mut app, KeyCode::Char('i'), KeyModifiers::NONE);
        assert!(app.finding.is_none(), "writing in this chat instead");
    }

    #[test]
    fn tab_and_shift_tab_in_the_chat_list_go_round_the_networks() {
        let mut app = test_app("tabs");
        app.focus = Focus::Chats;
        let none = KeyModifiers::NONE;
        press(&mut app, KeyCode::Tab, none);
        assert_eq!(app.chats.shown(), List::Network(Network::Messenger));
        app.chats.refresh();
        assert!(
            app.chats
                .ids()
                .iter()
                .all(|&id| app.chats.network(id) == Some(Network::Messenger))
        );
        press(&mut app, KeyCode::Tab, none);
        assert_eq!(app.chats.shown(), List::Network(Network::Instagram));
        press(&mut app, KeyCode::Tab, none);
        assert_eq!(app.chats.shown(), List::Network(Network::WhatsApp));
        press(&mut app, KeyCode::Tab, none);
        assert_eq!(app.chats.shown(), List::Archive);
        press(&mut app, KeyCode::Tab, none);
        assert_eq!(app.chats.shown(), List::Main, "round the end");
        press(&mut app, KeyCode::BackTab, none);
        assert_eq!(app.chats.shown(), List::Archive);
        let first = app.chats.ids()[0];
        assert_eq!(app.selected, Some(first), "on the tab's first chat");
    }

    #[test]
    fn a_theme_is_used_and_saved_as_soon_as_it_is_picked() {
        let dir = std::env::temp_dir().join(format!("tuimeta-theme-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let images = Images::new(Picker::halfblocks(), unbounded_channel().0);
        let meta = Meta::detached(unbounded_channel().0);
        let mut app = crate::demo::demo_app(meta, images, &dir);
        let none = KeyModifiers::NONE;
        press(&mut app, KeyCode::Char('?'), none);
        press(&mut app, KeyCode::Tab, none);
        // The themes are the last rows.
        press(&mut app, KeyCode::Char('G'), none);
        assert_eq!(app.settings.theme, "mocha", "moving doesn't change it");
        press(&mut app, KeyCode::Char(' '), none);
        assert_eq!(app.settings.theme, "rose-pine");
        assert_eq!(app.colors, app.themes.colors("rose-pine").unwrap());
        let saved = Settings::load(&settings::path(&dir)).unwrap();
        assert_eq!(saved.theme, "rose-pine");
        press(&mut app, KeyCode::Char('q'), none);
        assert_eq!(app.settings.theme, "rose-pine", "closing keeps it");
        std::fs::remove_dir_all(&dir).unwrap();
    }

    #[test]
    fn themes_in_the_data_folder_are_read_again_when_the_popup_opens() {
        let dir = std::env::temp_dir().join(format!("tuimeta-own-theme-{}", std::process::id()));
        std::fs::create_dir_all(dir.join("themes")).unwrap();
        let images = Images::new(Picker::halfblocks(), unbounded_channel().0);
        let meta = Meta::detached(unbounded_channel().0);
        let mut app = crate::demo::demo_app(meta, images, &dir);
        let none = KeyModifiers::NONE;
        let file = dir.join("themes/mine.toml");
        app.settings.theme = "mine".into();
        std::fs::write(&file, "inherits = \"mocha\"\n[colors]\nbg = \"#000000\"").unwrap();
        press(&mut app, KeyCode::Char('?'), none);
        assert_eq!(app.colors.bg, Color::Rgb(0, 0, 0));
        assert_eq!(app.status, None);
        press(&mut app, KeyCode::Esc, none);

        // A broken file leaves the default theme in use, saying why.
        std::fs::write(&file, "inherits = 3").unwrap();
        press(&mut app, KeyCode::Char('?'), none);
        assert_eq!(app.colors, Colors::default());
        let status = app.status.clone().unwrap();
        assert!(status.contains("mine.toml: line 1"), "{status}");
        assert_eq!(app.settings.theme, "mine", "kept, for when it's fixed");
        std::fs::remove_dir_all(&dir).unwrap();
    }

    #[test]
    fn ctrl_r_resizes_the_panes_and_enter_keeps_it_for_next_time() {
        let dir = std::env::temp_dir().join(format!("tuimeta-resize-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let images = Images::new(Picker::halfblocks(), unbounded_channel().0);
        let meta = Meta::detached(unbounded_channel().0);
        let mut app = crate::demo::demo_app(meta, images, &dir);
        app.focus = Focus::Messages;
        // Where the chat list's top right corner is.
        let edge = |rows: &[String]| {
            rows[0]
                .chars()
                .position(|c| matches!(c, '╮' | '┐'))
                .unwrap()
        };
        let none = KeyModifiers::NONE;
        assert_eq!(edge(&screen(&mut app)), 34, "35% of 100 columns");

        press(&mut app, KeyCode::Char('r'), KeyModifiers::CONTROL);
        assert!(app.open.as_ref().unwrap().reply.is_none(), "not a reply");
        press(&mut app, KeyCode::Char('l'), none);
        press(&mut app, KeyCode::Char('l'), none);
        let rows = screen(&mut app);
        assert_eq!(edge(&rows), 44, "wider as you press");
        let status = rows.last().unwrap();
        assert!(
            status.contains(" RESIZE ") && status.contains("chat list 45%"),
            "{status}"
        );

        press(&mut app, KeyCode::Enter, none);
        assert!(app.resizing.is_none());
        let saved = Settings::load(&settings::path(&dir)).unwrap();
        assert_eq!(saved.chat_list_width, 45);

        press(&mut app, KeyCode::Char('r'), KeyModifiers::CONTROL);
        press(&mut app, KeyCode::Char('h'), none);
        assert_eq!(edge(&screen(&mut app)), 39);
        press(&mut app, KeyCode::Esc, none);
        assert_eq!(edge(&screen(&mut app)), 44, "Esc puts it back");
        assert!(app.focus == Focus::Messages, "and stays in the chat");
        std::fs::remove_dir_all(&dir).unwrap();
    }

    #[test]
    fn only_files_that_cant_run_code_open_without_asking() {
        for path in [
            "/d/photo.JPG",
            "/d/clip.mp4",
            "/d/report.pdf",
            "/d/notes.txt",
        ] {
            assert!(safe_to_open(path), "{path}");
        }
        for path in [
            "/d/setup.exe",
            "/d/run.bat",
            "/d/x.command",
            "/d/x.terminal",
            "/d/app.jar",
            "/d/page.html",
            "/d/invoice.pdf.exe",
            "/d/no-extension",
            "/d/data.csv",
        ] {
            assert!(!safe_to_open(path), "{path}");
        }
    }

    #[test]
    fn ctrl_o_and_ctrl_i_go_back_and_forward_like_vims_jump_list() {
        let at = |chat_id, message_id| Jump {
            chat_id,
            message_id,
        };
        let mut jumps = Jumps::default();
        jumps.leave(at(1, None));
        jumps.leave(at(2, Some(5)));
        jumps.leave(at(2, Some(5)));
        assert_eq!(jumps.go(true, Some(at(3, None))), Some(at(2, Some(5))));
        assert_eq!(jumps.go(true, Some(at(2, Some(5)))), Some(at(1, None)));
        assert_eq!(
            jumps.go(true, Some(at(1, None))),
            None,
            "the same place once"
        );
        assert_eq!(jumps.go(false, Some(at(1, None))), Some(at(2, Some(5))));
        assert!(jumps.can_go_back() && jumps.can_go_forward());

        // Going somewhere new forgets where Ctrl-i would have gone.
        jumps.leave(at(2, Some(5)));
        assert!(!jumps.can_go_forward());
        for id in 0..MAX_JUMPS as i64 * 2 {
            jumps.leave(at(id, None));
        }
        assert_eq!(jumps.back.len(), MAX_JUMPS);
    }

    #[test]
    fn gd_then_ctrl_o_and_ctrl_i_move_between_a_reply_and_what_it_answers() {
        let mut app = test_app("jumps");
        app.focus = Focus::Messages;
        let (none, ctrl) = (KeyModifiers::NONE, KeyModifiers::CONTROL);
        let reply = app
            .open
            .as_ref()
            .unwrap()
            .messages
            .iter()
            .find_map(|(&id, m)| {
                let to = m.reply_to.as_ref()?.message_id?;
                Some((id, to))
            });
        let (from, to) = reply.expect("the demo has a reply");
        let cursor = |app: &App| app.open.as_ref().unwrap().selected;
        app.open.as_mut().unwrap().selected = Some(from);

        press(&mut app, KeyCode::Char('g'), none);
        press(&mut app, KeyCode::Char('d'), none);
        assert_eq!(cursor(&app), Some(to));
        press(&mut app, KeyCode::Char('o'), ctrl);
        assert_eq!(cursor(&app), Some(from));
        // Ctrl-i, from a terminal with the kitty keyboard protocol.
        press(&mut app, KeyCode::Char('i'), ctrl);
        assert_eq!(cursor(&app), Some(to));
        assert!(app.focus == Focus::Messages, "not Insert mode");
        // And as most terminals send it.
        press(&mut app, KeyCode::Char('o'), ctrl);
        press(&mut app, KeyCode::Tab, none);
        assert_eq!(cursor(&app), Some(to));
        press(&mut app, KeyCode::Tab, none);
        assert_eq!(app.status.as_deref(), Some("Nothing to go forward to"));
    }

    #[test]
    fn tab_completes_a_command_and_goes_on_to_the_next_that_fits() {
        let mut app = test_app("command-tab");
        let none = KeyModifiers::NONE;
        let typed = |app: &App| app.prompt.as_ref().unwrap().query();
        press(&mut app, KeyCode::Char(':'), none);
        press(&mut app, KeyCode::Char('l'), none);
        press(&mut app, KeyCode::Tab, none);
        assert_eq!(typed(&app), "login");
        let rows = screen(&mut app).join("\n");
        assert!(rows.contains("Commands · Tab completes"), "{rows}");
        press(&mut app, KeyCode::Tab, none);
        assert_eq!(typed(&app), "logout");
        press(&mut app, KeyCode::Tab, none);
        assert_eq!(typed(&app), "login", "round the end");
        press(&mut app, KeyCode::BackTab, none);
        assert_eq!(typed(&app), "logout");

        app.prompt = None;
        press(&mut app, KeyCode::Char(':'), none);
        press(&mut app, KeyCode::Char('x'), none);
        press(&mut app, KeyCode::Tab, none);
        assert_eq!(typed(&app), "x", "nothing fits");
        assert!(app.prompt.is_some(), "and nothing ran");
    }

    #[test]
    fn ctrl_u_deletes_back_to_the_start_of_the_line_and_then_the_line_break() {
        let mut app = test_app("ctrl-u");
        let chat_id = app.open.as_ref().unwrap().chat_id;
        app.focus = Focus::Input;
        // Typing was already told, so this test sends nothing.
        app.typing = Some((chat_id, Instant::now()));
        app.composer.insert_str("first line");
        app.composer.insert_newline();
        app.composer.insert_str("second line");
        app.composer
            .move_cursor(ratatui_textarea::CursorMove::WordBack);
        press(&mut app, KeyCode::Char('u'), KeyModifiers::CONTROL);
        assert_eq!(app.composer.lines(), ["first line", "line"]);

        press(&mut app, KeyCode::Char('u'), KeyModifiers::CONTROL);
        assert_eq!(app.composer.lines(), ["first lineline"]);
        assert!(app.focus == Focus::Input);
    }

    #[test]
    fn long_lines_in_the_composer_wrap_and_it_grows_with_them_unless_turned_off() {
        let mut app = test_app("wrap");
        app.focus = Focus::Input;
        app.composer
            .insert_str("zebra crossing ".repeat(12).trim_end());
        let count = |screen: &[String]| -> usize {
            screen.iter().map(|r| r.matches("zebra").count()).sum()
        };
        let rows = |screen: &[String]| screen.iter().filter(|r| r.contains("zebra")).count();

        assert!(app.settings.wrap_composer, "on unless turned off");
        let wrapped = screen(&mut app);
        assert_eq!(count(&wrapped), 12, "all of it shows: {wrapped:#?}");
        assert!(rows(&wrapped) >= 3, "{wrapped:#?}");
        let last = wrapped.iter().rposition(|r| r.contains("zebra")).unwrap();
        assert!(
            wrapped[last + 1].contains(['╰', '└']),
            "no blank row under the text: {wrapped:#?}"
        );

        // Turned off, the line keeps to one row, scrolled to the cursor.
        app.focus = Focus::Messages;
        press(&mut app, KeyCode::Char('?'), KeyModifiers::NONE);
        press(&mut app, KeyCode::Tab, KeyModifiers::NONE);
        app.settings_menu.as_mut().unwrap().selected = SettingsMenu::WRAP_COMPOSER;
        press(&mut app, KeyCode::Enter, KeyModifiers::NONE);
        assert!(!app.settings.wrap_composer);
        assert!(
            !Settings::load(&app.settings_path).unwrap().wrap_composer,
            "saved"
        );
        app.settings_menu = None;
        app.focus = Focus::Input;
        let one_row = screen(&mut app);
        assert_eq!(rows(&one_row), 1, "{one_row:#?}");
        assert!(count(&one_row) < 12, "{one_row:#?}");
    }

    #[test]
    fn colon_and_a_shortcode_suggest_emoji_and_tab_puts_one_in() {
        let mut app = test_app("emoji");
        app.focus = Focus::Input;
        let none = KeyModifiers::NONE;
        for c in "so :smi".chars() {
            press(&mut app, KeyCode::Char(c), none);
        }
        let completion = app.completion.as_ref().expect("suggestions");
        assert_eq!(completion.items[0].label, "😄");
        press(&mut app, KeyCode::Tab, none);
        assert_eq!(app.composer.lines(), ["so 😄"]);
        assert!(app.completion.is_none());
    }

    #[test]
    fn a_y_typed_just_before_a_warning_came_up_does_not_answer_it() {
        let mut app = test_app("grace");
        let edit = Confirmed::Edit {
            id: -1,
            text: String::new(),
        };
        app.confirm = Some(Confirm::new("Edit?", vec![], edit));
        press(&mut app, KeyCode::Char('y'), KeyModifiers::NONE);
        assert!(app.confirm.is_some(), "too soon to count");
        let confirm = app.confirm.as_mut().unwrap();
        confirm.shown = Instant::now().checked_sub(CONFIRM_GRACE).unwrap();
        press(&mut app, KeyCode::Char('y'), KeyModifiers::NONE);
        assert!(
            app.confirm.is_none(),
            "answered once it has been up a moment"
        );
    }

    #[test]
    fn a_warnings_grace_counts_from_when_it_can_be_seen() {
        let mut app = test_app("grace-in-view");
        let long_ago = || Instant::now().checked_sub(CONFIRM_GRACE * 2).unwrap();
        let edit = || Confirmed::Edit {
            id: -1,
            text: String::new(),
        };
        // Up for a while, but under the login screen.
        app.confirm = Some(Confirm::new("Edit?", vec![], edit()));
        app.screen = login_screen(LoginStep::Connecting);
        app.confirm_in_view();
        app.confirm.as_mut().unwrap().shown = long_ago();
        app.screen = Screen::Main;
        app.confirm_in_view();
        press(&mut app, KeyCode::Char('y'), KeyModifiers::NONE);
        assert!(app.confirm.is_some(), "only just in view");

        // In a window in the background.
        app.confirm.as_mut().unwrap().shown = long_ago();
        app.on_terminal_event(Event::FocusLost);
        app.confirm_in_view();
        app.on_terminal_event(Event::FocusGained);
        app.confirm_in_view();
        press(&mut app, KeyCode::Char('y'), KeyModifiers::NONE);
        assert!(app.confirm.is_some(), "only just back in view");

        app.confirm.as_mut().unwrap().shown = long_ago();
        app.confirm_in_view();
        press(&mut app, KeyCode::Char('y'), KeyModifiers::NONE);
        assert!(app.confirm.is_none(), "seen long enough");
    }

    #[test]
    fn a_warning_from_the_chats_does_not_come_back_after_logging_in_again() {
        let mut app = test_app("confirm-relogin");
        for network in [Network::Messenger, Network::Instagram, Network::Threads] {
            app.accounts.insert(
                network,
                Account {
                    state: AccountState::LoggedOut,
                    name: None,
                    error: None,
                },
            );
        }
        app.open = None;
        app.focus = Focus::Chats;
        app.ask_to_log_out();
        assert!(app.confirm.is_some());
        // The phone unlinked tuimeta meanwhile.
        app.on_meta(MetaEvent::Account {
            network: Network::WhatsApp,
            state: AccountState::LoggedOut,
            user_id: None,
            name: None,
            error: None,
        });
        assert!(matches!(app.screen, Screen::Login(_)));
        assert!(app.confirm.is_none(), "its logout is moot");

        // Any other warning goes too when the login screen takes over.
        app.screen = Screen::Main;
        app.confirm = Some(Confirm::new(
            "Open report.pdf.exe?",
            vec![],
            Confirmed::OpenFile("/nonexistent/report.pdf.exe".into()),
        ));
        app.follow_accounts();
        assert!(matches!(app.screen, Screen::Login(_)));
        assert!(app.confirm.is_none());
    }

    #[test]
    fn a_written_out_link_that_can_pass_for_another_site_asks_first() {
        for url in [
            "https://paypal.com@evil.example/login",
            "https://xn--pypal-4ve.com/",
            "https://p\u{0430}ypal.com/",
        ] {
            assert!(look_alike(url).is_some(), "{url}");
        }
        for url in [
            "https://example.com/a@b",
            "https://example.com/?to=x@y",
            "https://sub.example.com:8443/",
            "https://example.com/xn--path",
        ] {
            assert_eq!(look_alike(url), None, "{url}");
        }

        let mut app = test_app("look-alike");
        app.open_link_outside(Link::from("https://paypal.com@evil.example/login"));
        let confirm = app.confirm.take().expect("asks");
        assert_eq!(confirm.site.as_deref(), Some("evil.example"));
        assert_eq!(confirm.lines[0], "The part before the @ is not the site.");
        app.open_link_outside(Link::from("https://xn--pypal-4ve.com/"));
        let confirm = app.confirm.take().expect("asks");
        assert_eq!(
            confirm.site.as_deref(),
            Some("xn--pypal-4ve.com (has non-Latin letters)")
        );
    }

    #[cfg(unix)]
    #[test]
    fn the_attach_prompt_never_looks_at_a_file_on_another_machine() {
        let mut app = test_app("attach-unc");
        app.focus = Focus::Messages;
        // `//etc/hosts` is /etc/hosts on Unix; on Windows it's a server.
        press(&mut app, KeyCode::Char('a'), KeyModifiers::NONE);
        for c in "//etc/hosts".chars() {
            press(&mut app, KeyCode::Char(c), KeyModifiers::NONE);
        }
        press(&mut app, KeyCode::Enter, KeyModifiers::NONE);
        assert!(app.open.as_ref().unwrap().attachments.is_empty());
        assert_eq!(
            app.status.as_deref(),
            Some("Files on another machine can't be attached")
        );
    }

    #[test]
    fn what_the_helper_says_is_shown_without_control_characters() {
        let mut app = test_app("helper-text");
        let chat_id = app.open.as_ref().unwrap().chat_id;
        app.on_meta(MetaEvent::MessageFailed {
            chat_id,
            old_id: 1,
            error: "Too\u{1b}[2J big\u{202e}".into(),
        });
        assert_eq!(app.status.as_deref(), Some("Message not sent: Too[2J big"));
        app.on_meta(MetaEvent::Gone("stopped\u{7}".into()));
        let status = app.status.clone().unwrap();
        assert!(status.starts_with("stopped. Quit"), "{status:?}");
    }

    #[test]
    fn a_download_finishing_while_you_type_waits_for_enter_instead_of_asking() {
        let mut app = test_app("busy");
        let dir = std::env::temp_dir().join("tuimeta-no-such-folder");
        let downloaded = |app: &mut App, name: &str| {
            app.opening.insert(77);
            let path = dir.join(name).to_string_lossy().into_owned();
            app.on_meta(MetaEvent::Downloaded {
                file_id: 77,
                path: Some(path),
            });
        };
        app.focus = Focus::Input;
        downloaded(&mut app, "run me.sh");
        assert!(app.confirm.is_none(), "no popup over the composer");
        let status = app.status.clone().unwrap_or_default();
        assert!(status.contains("run me.sh downloaded"), "{status}");

        app.focus = Focus::Messages;
        // Not a safe type, so nothing is opened.
        downloaded(&mut app, "evil\u{202e}txt.sh");
        let title = &app
            .confirm
            .as_ref()
            .expect("asks when nothing else is up")
            .title;
        assert!(
            !title.contains('\u{202e}'),
            "the sender's name is cleaned: {title:?}"
        );
    }

    /// A file from someone else, just now, in the open chat.
    fn incoming_file(app: &App, id: i64, file_id: i32, text: &str) -> Message {
        let chat_id = app.open.as_ref().unwrap().chat_id;
        message(
            serde_json::json!({"id": id, "chat_id": chat_id, "sender_id": 2,
            "date": unix_now(), "text": text, "media": {"kind": "file", "file_id": file_id,
            "name": "report.pdf", "mime": "application/pdf", "size": 1000}}),
        )
    }

    /// A link behind other words, which opens only after a `y`.
    fn incoming_disguised(app: &App, id: i64, url: &str) -> Message {
        let chat_id = app.open.as_ref().unwrap().chat_id;
        message(
            serde_json::json!({"id": id, "chat_id": chat_id, "sender_id": 2,
            "date": unix_now(), "text": "the doc",
            "entities": [{"offset": 4, "length": 3, "type": "link", "url": url}]}),
        )
    }

    /// As if message `id` had been there, unchanged, for a while.
    fn settled(app: &mut App, id: i64) {
        if let Some(at) = app.changed.get_mut(&id) {
            *at = Instant::now().checked_sub(CONFIRM_GRACE).unwrap();
        }
    }

    fn opened_link(app: &App) -> Option<String> {
        app.confirm.as_ref().and_then(|c| match &c.action {
            Confirmed::OpenLink(url) => Some(url.clone()),
            _ => None,
        })
    }

    #[test]
    fn enter_on_a_message_that_just_came_in_or_changed_waits_for_a_second_enter() {
        let mut app = test_app("enter-grace");
        let none = KeyModifiers::NONE;
        app.focus = Focus::Messages;
        let chat_id = app.open.as_ref().unwrap().chat_id;
        let base = app.open.as_ref().unwrap().newest_id().unwrap() + 1_000_000;
        let wait = "It just came in or changed: press Enter again to open it";

        // A file arrives under the cursor, which follows the newest.
        app.on_meta(MetaEvent::Message(Box::new(incoming_file(
            &app, base, 4242, "",
        ))));
        press(&mut app, KeyCode::Enter, none);
        assert_eq!(app.status.as_deref(), Some(wait));
        assert!(sent_with(&app, "download").is_empty() && app.opening.is_empty());
        settled(&mut app, base);
        press(&mut app, KeyCode::Enter, none);
        assert_eq!(sent_with(&app, "download")[0]["file_id"], 4242);

        // A message picked a while ago is edited to another link, and
        // another message comes right after: the edit still counts.
        let picked = base + 1;
        app.on_meta(MetaEvent::Message(Box::new(incoming_disguised(
            &app,
            picked,
            "https://docs.example/a",
        ))));
        settled(&mut app, picked);
        app.open.as_mut().unwrap().selected = Some(picked);
        app.on_meta(MetaEvent::Message(Box::new(incoming_disguised(
            &app,
            picked,
            "https://evil.example/b",
        ))));
        app.on_meta(MetaEvent::Message(Box::new(incoming_file(
            &app,
            base + 2,
            4343,
            "ok",
        ))));
        press(&mut app, KeyCode::Enter, none);
        assert_eq!(app.status.as_deref(), Some(wait));
        assert!(app.confirm.is_none());
        settled(&mut app, picked);
        press(&mut app, KeyCode::Enter, none);
        assert_eq!(opened_link(&app).as_deref(), Some("https://evil.example/b"));
        app.confirm = None;

        // The picked message is unsent: the cursor lands on another, which
        // may never have been on screen.
        settled(&mut app, base + 2);
        app.on_meta(MetaEvent::MessagesDeleted {
            chat_id,
            message_ids: vec![picked],
        });
        let landed = app.open.as_ref().unwrap().cursor_id().unwrap();
        assert_ne!(landed, picked);
        press(&mut app, KeyCode::Enter, none);
        assert_eq!(app.status.as_deref(), Some(wait));
        assert!(app.confirm.is_none() && app.menu.is_none());
        assert_eq!(sent_with(&app, "download").len(), 1, "nothing more");
    }

    #[test]
    fn a_deleted_messages_files_popups_and_edit_go_with_it() {
        let mut app = test_app("deleted-file");
        let none = KeyModifiers::NONE;
        app.focus = Focus::Messages;
        let chat_id = app.open.as_ref().unwrap().chat_id;
        let id = app.open.as_ref().unwrap().newest_id().unwrap() + 1_000_000;
        app.on_meta(MetaEvent::Message(Box::new(incoming_file(
            &app,
            id,
            4601,
            "the doc https://docs.example/a",
        ))));
        settled(&mut app, id);

        // Enter, then the file from its menu; then the copy menu.
        press(&mut app, KeyCode::Enter, none);
        press(&mut app, KeyCode::Char('1'), none);
        assert!(app.opening.contains(&4601));
        let file = app.open.as_ref().unwrap().messages[&id]
            .file
            .clone()
            .unwrap();
        app.copying.insert(4601, file);
        press(&mut app, KeyCode::Char('y'), none);
        assert!(app.menu.is_some());
        let downloads = sent_with(&app, "download").len();

        // Another message going leaves them be.
        app.on_meta(MetaEvent::MessagesDeleted {
            chat_id,
            message_ids: vec![SUNNY_IN_DEMO],
        });
        assert!(app.menu.is_some() && app.opening.contains(&4601));

        app.on_meta(MetaEvent::MessagesDeleted {
            chat_id,
            message_ids: vec![id],
        });
        assert!(app.menu.is_none(), "its copy menu");
        assert!(app.opening.is_empty() && app.copying.is_empty());
        app.on_meta(MetaEvent::Downloaded {
            file_id: 4601,
            path: Some("/nonexistent/report.exe".into()),
        });
        assert!(app.confirm.is_none(), "nothing opens");
        assert_eq!(sent_with(&app, "download").len(), downloads);

        // Its reaction popup and an edit of it close too.
        let mine = app.open.as_ref().unwrap().newest_id().unwrap();
        app.open.as_mut().unwrap().selected = Some(mine);
        press(&mut app, KeyCode::Char('R'), none);
        assert!(app.react_menu.is_some());
        app.on_meta(MetaEvent::MessagesDeleted {
            chat_id,
            message_ids: vec![mine],
        });
        assert!(app.react_menu.is_none());
        let mine = app
            .open
            .as_ref()
            .unwrap()
            .messages
            .iter()
            .rev()
            .find(|(_, m)| m.outgoing);
        let mine = *mine.expect("another of yours").0;
        app.composer.insert_str("a draft");
        app.start_edit(mine, "On my way".into());
        app.on_meta(MetaEvent::MessagesDeleted {
            chat_id,
            message_ids: vec![mine],
        });
        assert!(app.open.as_ref().unwrap().editing.is_none());
        assert_eq!(app.composer.lines(), ["a draft"], "the draft is back");
        assert_eq!(
            app.status.as_deref(),
            Some("The message you were editing was deleted")
        );
    }

    /// The demo's sunrise photo, under the cursor, with its message id.
    fn photo_message(app: &mut App) -> (i64, i64) {
        let open = app.open.as_mut().unwrap();
        let id = open
            .messages
            .iter()
            .find(|(_, m)| m.photo.is_some())
            .map(|(&id, _)| id)
            .unwrap();
        open.selected = Some(id);
        (open.chat_id, id)
    }

    /// Makes message `id` a photo: file `file_id` in its bubble, the next
    /// one at its largest.
    fn make_photo(app: &mut App, id: i64, file_id: i32) {
        let open = app.open.as_mut().unwrap();
        let msg = open.messages.get_mut(&id).unwrap();
        msg.links.clear();
        let preview = |file_id, width, height| crate::messages::Preview {
            file_id,
            width,
            height,
            thumbnail: None,
            sticker: false,
        };
        msg.preview = Some(preview(file_id, 800, 600));
        msg.photo = Some(preview(file_id + 1, 2560, 1920));
        msg.file = Some(MediaFile {
            id: file_id + 1,
            label: "Photo".into(),
            photo: true,
        });
    }

    #[test]
    fn h_and_l_in_the_viewer_go_to_the_photo_before_and_after_it() {
        let mut app = test_app("viewer-next");
        let none = KeyModifiers::NONE;
        let open = app.open.as_mut().unwrap();
        // Everything is loaded: nothing is asked of the helper.
        open.all_loaded = true;
        open.at_newest = true;
        for msg in open.messages.values_mut() {
            msg.photo = None;
        }
        let (chat_id, ids) = (
            open.chat_id,
            open.messages.keys().copied().collect::<Vec<_>>(),
        );
        // Not next to each other, and the newer one the newest.
        let (older, newer) = (ids[1], ids[ids.len() - 1]);
        assert!(ids.len() > 3);
        make_photo(&mut app, older, 50);
        make_photo(&mut app, newer, 60);
        let open = app.open.as_ref().unwrap();
        app.photo_view = PhotoView::of(chat_id, newer, &open.messages[&newer]);
        let shown = |app: &App| app.photo_view.as_ref().map(|v| v.message_id);

        press(&mut app, KeyCode::Char('k'), none);
        press(&mut app, KeyCode::Char('h'), none);
        assert_eq!(
            shown(&app),
            Some(older),
            "past the messages that aren't photos"
        );
        assert_eq!(
            app.photo_view.as_ref().unwrap().zoom,
            crate::viewer::FILLING,
            "filling the window again"
        );
        press(&mut app, KeyCode::Char('h'), none);
        assert_eq!(shown(&app), Some(older));
        assert_eq!(app.status.as_deref(), Some("No older photos in this chat"));
        press(&mut app, KeyCode::Right, none);
        assert_eq!(shown(&app), Some(newer));
        press(&mut app, KeyCode::Char('l'), none);
        assert_eq!(shown(&app), Some(newer));
        assert_eq!(app.status.as_deref(), Some("No newer photos in this chat"));
        assert!(sent_with(&app, "history").is_empty());

        // Past the messages loaded, older ones are asked for.
        app.open.as_mut().unwrap().all_loaded = false;
        press(&mut app, KeyCode::Char('h'), none);
        press(&mut app, KeyCode::Char('h'), none);
        assert_eq!(shown(&app), Some(older));
        assert_eq!(app.status.as_deref(), Some("Loading older messages…"));
        assert_eq!(sent_with(&app, "history")[0]["before"], ids[0]);
    }

    #[test]
    fn j_and_k_in_the_viewer_zoom_out_and_back_in_never_past_filling_the_window() {
        let mut app = test_app("viewer-zoom");
        app.focus = Focus::Messages;
        let none = KeyModifiers::NONE;
        let (_, id) = photo_message(&mut app);
        make_photo(&mut app, id, 40);
        press(&mut app, KeyCode::Enter, none);
        let zoom = |app: &App| app.photo_view.as_ref().unwrap().zoom;
        // It opens filling the window: there's no more to zoom in.
        screen(&mut app);
        press(&mut app, KeyCode::Char('j'), none);
        assert_eq!(zoom(&app), crate::viewer::FILLING);
        press(&mut app, KeyCode::Char('k'), none);
        assert_eq!(zoom(&app), crate::viewer::FILLING - 1);
        for _ in 0..10 {
            press(&mut app, KeyCode::Char('-'), none);
        }
        assert_eq!(zoom(&app), 0);
        let rows = screen(&mut app);
        assert!(rows[0].contains("Photo · 25% · loading…"), "{rows:#?}");
        for _ in 0..10 {
            press(&mut app, KeyCode::Char('j'), none);
        }
        assert_eq!(zoom(&app), crate::viewer::FILLING);
        assert!(screen(&mut app)[0].contains("Photo · 100%"));
    }

    #[test]
    fn what_arrives_while_a_photo_is_open_is_neither_marked_read_nor_kept_quiet() {
        let mut app = test_app("viewer-unseen");
        app.focus = Focus::Messages;
        let open = app.open.as_mut().unwrap();
        let (chat_id, newest) = (open.chat_id, open.newest_id().unwrap());
        make_photo(&mut app, newest, 40);
        // Following new messages, as when a photo just came in.
        let open = app.open.as_mut().unwrap();
        open.selected = None;
        open.at_newest = true;
        assert!(app.watching(chat_id));

        press(&mut app, KeyCode::Enter, KeyModifiers::NONE);
        assert!(app.photo_view.is_some());
        assert_eq!(
            app.open.as_ref().unwrap().selected,
            Some(newest),
            "the cursor stays on the photo"
        );
        assert!(!app.watching(chat_id));
        app.open.as_mut().unwrap().selected = None;
        assert!(!app.watching(chat_id), "the photo hides the chat");
        assert!(!app.sees(chat_id), "so it's notified");
        app.focus_reported = true;
        assert!(!app.sees(chat_id), "even in a focused window");
    }

    #[test]
    fn pastes_wait_until_the_photo_is_closed() {
        let mut app = test_app("viewer-paste");
        app.focus = Focus::Messages;
        let (chat_id, _) = photo_message(&mut app);
        press(&mut app, KeyCode::Enter, KeyModifiers::NONE);
        let file = std::env::temp_dir().join("tuimeta-test-viewer-paste.txt");
        std::fs::write(&file, b"notes").unwrap();
        app.on_terminal_event(Event::Paste(file.to_string_lossy().into_owned()));
        assert_eq!(
            app.status.take().as_deref(),
            Some("Close the photo to paste")
        );
        app.on_pasted(Pasted {
            chat_id,
            content: Ok(Paste::Text("the screenshot".into())),
        });
        assert!(app.status.take().is_some());
        assert!(app.focus == Focus::Messages, "not Insert mode under it");
        assert!(app.composer.is_empty());
        assert!(app.open.as_ref().unwrap().attachments.is_empty());
    }

    #[test]
    fn enter_on_a_photo_shows_it_in_the_viewer_not_in_another_app() {
        let mut app = test_app("viewer-enter");
        app.focus = Focus::Messages;
        let none = KeyModifiers::NONE;
        let (chat_id, id) = photo_message(&mut app);
        press(&mut app, KeyCode::Enter, none);
        let view = app.photo_view.as_ref().expect("the viewer is up");
        assert_eq!((view.chat_id, view.message_id), (chat_id, id));
        assert!(app.opening.is_empty(), "nothing waits for another app");
        assert!(app.busy(), "a download finishing won't pop up over it");

        // It takes the keys.
        press(&mut app, KeyCode::Char('k'), none);
        assert_eq!(app.open.as_ref().unwrap().selected, Some(id));
        press(&mut app, KeyCode::Char('o'), KeyModifiers::CONTROL);
        assert!(app.photo_view.is_some(), "Ctrl-o isn't o");
        assert!(app.opening.is_empty());
        for close in [KeyCode::Esc, KeyCode::Char('q'), KeyCode::Enter] {
            assert!(app.photo_view.is_some());
            press(&mut app, close, none);
            assert!(app.photo_view.is_none(), "{close:?} closes it");
            press(&mut app, KeyCode::Enter, none);
        }
    }

    #[test]
    fn o_in_the_viewer_closes_it_and_opens_the_photo_in_its_app_as_enter_did() {
        let mut app = test_app("viewer-o");
        app.focus = Focus::Messages;
        photo_message(&mut app);
        press(&mut app, KeyCode::Enter, KeyModifiers::NONE);
        let file = app.photo_view.as_ref().unwrap().file.id;
        press(&mut app, KeyCode::Char('o'), KeyModifiers::NONE);
        assert!(app.photo_view.is_none());
        assert!(app.opening.contains(&file), "opens once downloaded");
        assert_eq!(sent_with(&app, "download")[0]["file_id"], file);
        // A file that could run code still asks first.
        app.on_meta(MetaEvent::Downloaded {
            file_id: file,
            path: Some("/nonexistent/photo.exe".into()),
        });
        assert!(matches!(
            app.confirm.as_ref().map(|c| &c.action),
            Some(Confirmed::OpenFile(_))
        ));
    }

    #[test]
    fn the_viewer_closes_once_its_message_is_deleted_or_edited_to_another_photo() {
        let mut app = test_app("viewer-gone");
        app.focus = Focus::Messages;
        let (chat_id, id) = photo_message(&mut app);
        press(&mut app, KeyCode::Enter, KeyModifiers::NONE);
        app.on_meta(MetaEvent::MessagesDeleted {
            chat_id,
            message_ids: vec![id + 1000],
        });
        assert!(app.photo_view.is_some(), "another message went");
        app.on_meta(MetaEvent::MessagesDeleted {
            chat_id,
            message_ids: vec![id],
        });
        assert!(app.photo_view.is_none());

        app.open = Some(crate::demo::tests_hike());
        let (_, id) = photo_message(&mut app);
        press(&mut app, KeyCode::Enter, KeyModifiers::NONE);
        let edited = message(serde_json::json!({"id": id, "chat_id": chat_id,
            "sender_id": 4, "text": "no photo now"}));
        app.on_meta(MetaEvent::Message(Box::new(edited)));
        assert!(app.photo_view.is_none());
    }

    #[test]
    fn the_viewer_covers_the_chats_and_messages_and_says_its_keys() {
        let mut app = test_app("viewer-draw");
        app.focus = Focus::Messages;
        photo_message(&mut app);
        let chat_id = app.open.as_ref().unwrap().chat_id;
        let title = app.chats.title(chat_id).unwrap().to_string();
        assert!(screen(&mut app).iter().any(|r| r.contains(&title)));
        press(&mut app, KeyCode::Enter, KeyModifiers::NONE);
        let rows = screen(&mut app);
        assert!(rows[0].contains("Photo · 100% · loading…"), "{rows:#?}");
        assert!(!rows.iter().any(|r| r.contains(&title)), "{rows:#?}");
        assert!(rows.iter().any(|r| r.contains("Loading…")), "{rows:#?}");
        assert!(
            rows[rows.len() - 1].contains("o open in its app · y copy"),
            "{rows:#?}"
        );
    }

    /// The Instagram account comes in, as the helper says it.
    fn instagram_in(app: &mut App) {
        app.on_meta(MetaEvent::Account {
            network: Network::Instagram,
            state: AccountState::Ready,
            user_id: None,
            name: None,
            error: None,
        });
    }

    /// Opens the feed from its row in the Instagram tab.
    fn open_the_feed(app: &mut App) {
        instagram_in(app);
        app.focus = Focus::Chats;
        app.chats.show(List::Network(Network::Instagram));
        app.chats.refresh();
        app.selected = Some(FEED);
        press(app, KeyCode::Enter, KeyModifiers::NONE);
    }

    /// The page the feed waits for comes.
    fn feed_page(app: &mut App, posts: Vec<FeedPost>, more: bool) {
        let load = app
            .feed
            .as_ref()
            .unwrap()
            .loading
            .expect("a load on its way");
        app.on_meta(MetaEvent::Feed {
            network: Network::Instagram,
            load,
            page: Ok((posts, more)),
        });
    }

    fn post(id: i64, fields: serde_json::Value) -> FeedPost {
        crate::feed::tests::post(id, fields)
    }

    /// A post of one video, file 7 with its cover 8.
    fn video_post(id: i64) -> FeedPost {
        post(
            id,
            serde_json::json!({"media": [{"kind": "video", "file_id": 7, "duration": 15,
                "width": 720, "height": 1280, "mime": "video/mp4",
                "thumbnail": {"file_id": 8, "width": 1080, "height": 1920}}]}),
        )
    }

    #[test]
    fn the_feed_is_the_first_row_of_the_instagram_tab_and_opens_in_the_chats_place() {
        let mut app = test_app("feed-row");
        let none = KeyModifiers::NONE;
        instagram_in(&mut app);
        app.chats.refresh();
        assert!(
            !app.chats.ids().contains(&FEED),
            "not among every network's chats while Instagram has a tab"
        );
        app.chats.show(List::Network(Network::Instagram));
        app.chats.refresh();
        assert_eq!(app.chats.ids().first(), Some(&FEED));
        app.selected = Some(FEED);
        app.focus = Focus::Chats;
        assert!(screen(&mut app).iter().any(|r| r.contains("Feed")));
        // Nothing that acts on a chat goes to the helper for it.
        press(&mut app, KeyCode::Char('m'), none);
        press(&mut app, KeyCode::Char('H'), none);
        assert!(sent_with(&app, "mute").is_empty());
        assert!(!app.settings.highlighted_chats.contains(&FEED));
        press(&mut app, KeyCode::Enter, none);
        assert!(app.open.is_none() && app.feed.is_some());
        assert_eq!(app.focus, Focus::Messages);
        let asked = sent_with(&app, "load_feed");
        assert_eq!(asked.len(), 1);
        assert_eq!(asked[0]["network"], "instagram");
        assert_eq!(asked[0]["fresh"], false);
        let rows = screen(&mut app);
        assert!(
            rows.iter().any(|r| r.contains("Loading the feed…")),
            "{rows:#?}"
        );
        // A chat's keys do nothing here.
        for key in ['i', 'r', 'R', 'd', 'e', 'a', 'p'] {
            if key != 'r' {
                press(&mut app, KeyCode::Char(key), none);
            }
        }
        assert_eq!(app.focus, Focus::Messages);
        assert!(app.prompt.is_none() && app.react_menu.is_none() && app.delete_menu.is_none());
        assert!(
            !sent(&app)
                .iter()
                .any(|m| ["mark_read", "typing", "send_text"].contains(&m.as_str()))
        );
        // Back to the list, and the chat opened from it takes the feed's place.
        press(&mut app, KeyCode::Char('h'), none);
        assert_eq!(app.focus, Focus::Chats);
    }

    #[test]
    fn the_feed_loads_as_it_is_read_and_r_starts_again_from_the_newest() {
        let mut app = test_app("feed-pages");
        let none = KeyModifiers::NONE;
        open_the_feed(&mut app);
        feed_page(
            &mut app,
            (1..=10).map(|i| post(i, serde_json::json!({}))).collect(),
            true,
        );
        let rows = screen(&mut app);
        assert!(
            rows.iter()
                .any(|r| r.contains("Alice Example @alice.example")),
            "{rows:#?}"
        );
        assert!(
            rows.iter().any(|r| r.contains("12 likes · 3 comments")),
            "{rows:#?}"
        );
        assert!(rows.iter().any(|r| r.contains("sunset")), "{rows:#?}");
        for _ in 0..4 {
            press(&mut app, KeyCode::Char('j'), none);
        }
        assert_eq!(app.feed.as_ref().unwrap().selected, 4);
        assert_eq!(sent_with(&app, "load_feed").len(), 1);
        press(&mut app, KeyCode::Char('j'), none);
        assert_eq!(
            sent_with(&app, "load_feed").len(),
            2,
            "the next page, near the end"
        );
        press(&mut app, KeyCode::Char('j'), none);
        assert_eq!(sent_with(&app, "load_feed").len(), 2, "one at a time");
        press(&mut app, KeyCode::Char('r'), none);
        let asked = sent_with(&app, "load_feed");
        assert_eq!(asked.len(), 3);
        assert_eq!(asked[2]["fresh"], true);
        // Sized for the pane the last frame drew, once there was one.
        assert!(asked[0].get("width").is_none(), "{:?}", asked[0]);
        let (width, height) = ui::feed::picture_box(app.chat_area, app.images.font_size()).unwrap();
        for page in &asked[1..] {
            assert_eq!(
                (page["width"].as_u64(), page["height"].as_u64()),
                (Some(width.into()), Some(height.into()))
            );
        }
        feed_page(
            &mut app,
            vec![post(30, serde_json::json!({"caption": "newest"}))],
            true,
        );
        let feed = app.feed.as_ref().unwrap();
        assert_eq!((feed.posts.len(), feed.selected), (1, 0));
        // The post under the cursor just changed: Enter asks again.
        press(&mut app, KeyCode::Enter, none);
        assert!(app.photo_view.is_none());
        assert!(app.status.as_ref().unwrap().contains("Enter again"));
        press(&mut app, KeyCode::Enter, none);
        assert!(app.photo_view.is_some());
    }

    #[test]
    fn the_photos_of_the_next_ten_posts_download_before_they_are_in_view() {
        let mut app = test_app("feed-ahead");
        open_the_feed(&mut app);
        feed_page(
            &mut app,
            (1..=15).map(|i| post(i, serde_json::json!({}))).collect(),
            true,
        );
        // What a frame asks for, as `App::run` fetches after each.
        let downloads = |app: &mut App| -> Vec<String> {
            screen(app);
            app.images.fetch(&app.meta);
            sent_with(app, "download")
                .iter()
                .map(|p| format!("{} {}", p["file_id"], p["priority"].as_str().unwrap()))
                .collect()
        };
        // The first two posts are in view; the next ten download behind them.
        let asked = downloads(&mut app);
        let behind: Vec<String> = (3..=11).map(|i| format!("{} low", i * 10)).collect();
        assert_eq!(asked[..2], ["10 high", "20 high"]);
        assert_eq!(asked[2..], behind);
        press(&mut app, KeyCode::Char('j'), KeyModifiers::NONE);
        let asked = downloads(&mut app);
        assert_eq!(
            asked[11..],
            ["30 high", "120 low"],
            "in view now, it goes first"
        );
    }

    #[test]
    fn a_video_outside_pepper_shows_its_cover_and_enter_opens_it_in_the_systems_player() {
        let mut app = test_app("feed-video-outside");
        open_the_feed(&mut app);
        feed_page(&mut app, vec![video_post(1)], false);
        let rows = screen(&mut app);
        assert!(rows.iter().any(|r| r.contains("video 0:15")), "{rows:#?}");
        press(&mut app, KeyCode::Enter, KeyModifiers::NONE);
        assert!(app.playing.is_none());
        assert!(app.opening.contains(&7), "opens in its app once downloaded");
        assert_eq!(sent_with(&app, "download")[0]["file_id"], 7);
    }

    #[test]
    fn inside_pepper_a_video_plays_over_its_post_and_goes_under_a_popup_or_with_its_post() {
        let mut app = test_app("feed-video-pepper");
        let none = KeyModifiers::NONE;
        app.pepper = Some(Pepper {
            port: "x.1".into(),
            play: "/nonexistent/pepper-play".into(),
        });
        open_the_feed(&mut app);
        feed_page(
            &mut app,
            vec![video_post(1), post(2, serde_json::json!({}))],
            false,
        );
        press(&mut app, KeyCode::Enter, none);
        assert_eq!(sent_with(&app, "download")[0]["file_id"], 7);
        assert!(app.opening.is_empty(), "not handed to another app");
        assert!(
            screen(&mut app)
                .iter()
                .any(|r| r.contains("video loading…"))
        );
        assert_eq!(
            app.video_placement(),
            "",
            "nothing to show while it downloads"
        );
        // As if pepper-play started on the downloaded file.
        let playing = app.playing.as_mut().unwrap();
        let token = playing.token.clone();
        assert_eq!(token.len(), 32);
        playing.player = Some(Player::pretend(&token, Path::new("/data/7.mp4"), None, 1));
        let rows = screen(&mut app);
        assert!(
            rows.iter().any(|r| r.contains("video playing")),
            "{rows:#?}"
        );
        let area = app.video_box.expect("its post is in view");
        assert_eq!(app.video_placement(), pepper::place(&token, area));
        assert_eq!(app.video_placement(), "", "said once");
        // Its seek bar, under it, once pepper-play says where it is.
        app.on_player(PlayerEvent::Duration {
            play: 1,
            seconds: 15.0,
        });
        app.on_player(PlayerEvent::Position {
            play: 1,
            seconds: 6.0,
        });
        // News of another playback is old news.
        app.on_player(PlayerEvent::Position {
            play: 9,
            seconds: 1.0,
        });
        let rows = screen(&mut app);
        let under = &rows[usize::from(area.bottom())];
        // A video this narrow gets the times alone (`ui::feed::seek_bar`).
        assert!(under.contains("0:06") && under.contains("0:15"), "{under}");
        // `,` and `.` jump back and on.
        press(&mut app, KeyCode::Char(','), none);
        press(&mut app, KeyCode::Char('.'), none);
        let sent = app
            .playing
            .as_ref()
            .unwrap()
            .player
            .as_ref()
            .unwrap()
            .sent
            .clone();
        let seeks: Vec<String> = sent
            .lock()
            .unwrap()
            .iter()
            .filter(|l| l.contains("\"seek\""))
            .cloned()
            .collect();
        assert!(
            seeks[0].contains(r#"["seek",-5,"relative+exact"]"#),
            "{seeks:?}"
        );
        assert!(
            seeks[1].contains(r#"["seek",5,"relative+exact"]"#),
            "{seeks:?}"
        );
        press(&mut app, KeyCode::Char(' '), none);
        let sent = app
            .playing
            .as_ref()
            .unwrap()
            .player
            .as_ref()
            .unwrap()
            .sent
            .clone();
        assert!(
            sent.lock()
                .unwrap()
                .iter()
                .any(|l| l.contains(r#"["cycle","pause"]"#))
        );
        assert!(screen(&mut app).iter().any(|r| r.contains("video paused")));
        // A popup over it takes the box away until it closes.
        press(&mut app, KeyCode::Char('?'), none);
        screen(&mut app);
        assert_eq!(app.video_placement(), pepper::remove(&token));
        press(&mut app, KeyCode::Esc, none);
        screen(&mut app);
        assert_eq!(app.video_placement(), pepper::place(&token, area));
        // Its post left, it stops.
        press(&mut app, KeyCode::Char('j'), none);
        assert!(app.playing.is_none());
        screen(&mut app);
        assert_eq!(app.video_placement(), pepper::remove(&token));
        // A player that can't start says why.
        press(&mut app, KeyCode::Char('k'), none);
        press(&mut app, KeyCode::Enter, none);
        app.on_meta(MetaEvent::Downloaded {
            file_id: 7,
            path: Some("/data/7.mp4".into()),
        });
        assert!(app.playing.is_none());
        assert!(
            app.status
                .as_ref()
                .unwrap()
                .contains("Can't play the video")
        );
    }

    #[test]
    fn f_right_after_the_feed_changed_asks_for_f_again() {
        let mut app = test_app("feed-full-screen-changed");
        let none = KeyModifiers::NONE;
        app.pepper = Some(Pepper {
            port: "x.1".into(),
            play: "/nonexistent/pepper-play".into(),
        });
        open_the_feed(&mut app);
        feed_page(&mut app, vec![post(1, serde_json::json!({}))], true);
        // A fresh page takes its place, a video now under the cursor.
        press(&mut app, KeyCode::Char('r'), none);
        feed_page(&mut app, vec![video_post(2)], false);
        press(&mut app, KeyCode::Char('f'), none);
        assert!(app.playing.is_none());
        assert!(app.status.take().unwrap().contains("press f again"));
        press(&mut app, KeyCode::Char('f'), none);
        assert!(app.full_screen(), "the second fills the window");
    }

    #[test]
    fn f_plays_a_video_filling_the_window_and_again_puts_it_back_over_its_post() {
        let mut app = test_app("feed-video-full-screen");
        let none = KeyModifiers::NONE;
        app.pepper = Some(Pepper {
            port: "x.1".into(),
            play: "/nonexistent/pepper-play".into(),
        });
        open_the_feed(&mut app);
        feed_page(
            &mut app,
            vec![post(1, serde_json::json!({})), video_post(2)],
            false,
        );
        // A photo has nothing to play.
        press(&mut app, KeyCode::Char('f'), none);
        assert!(app.playing.is_none());
        assert!(app.status.take().unwrap().contains("No video here"));
        press(&mut app, KeyCode::Char('j'), none);
        press(&mut app, KeyCode::Char('f'), none);
        assert!(app.full_screen(), "it plays at once, filling the window");
        assert!(
            sent_with(&app, "download")
                .iter()
                .any(|p| p["file_id"] == 7)
        );
        let rows = screen(&mut app);
        assert!(
            rows.iter().any(|r| r.contains("Video loading…")),
            "{rows:#?}"
        );
        assert!(
            !rows.iter().any(|r| r.contains("Instagram · Feed")),
            "over the list and the feed: {rows:#?}"
        );
        // As if pepper-play started on the downloaded file.
        let playing = app.playing.as_mut().unwrap();
        let token = playing.token.clone();
        playing.player = Some(Player::pretend(&token, Path::new("/data/7.mp4"), None, 1));
        screen(&mut app);
        let full = app.video_box.expect("its box");
        // A tall video: every row but the seek bar's and the status bar.
        assert_eq!((full.y, full.height), (0, 18));
        assert_eq!(app.video_placement(), pepper::place(&token, full));
        app.on_player(PlayerEvent::Duration {
            play: 1,
            seconds: 15.0,
        });
        app.on_player(PlayerEvent::Position {
            play: 1,
            seconds: 6.0,
        });
        let rows = screen(&mut app);
        let bar = &rows[18];
        assert!(
            bar.contains("0:06 ━") && bar.contains("0:15") && bar.contains("playing"),
            "{bar}"
        );
        // A toast is said there too, rather than under the video.
        app.show_toast("Copied", "the link");
        let rows = screen(&mut app);
        assert!(rows[18].contains("✓ Copied the link"), "{}", rows[18]);
        assert!(!rows[..18].iter().any(|r| r.contains("Copied")), "no box");
        app.toast = None;
        // Its own keys work; what's under it waits.
        press(&mut app, KeyCode::Char(' '), none);
        assert!(app.playing.as_ref().unwrap().paused);
        press(&mut app, KeyCode::Char('j'), none);
        assert_eq!(app.feed.as_ref().unwrap().selected, 1, "still its post");
        assert!(app.full_screen());
        // A popup takes the box away until it closes.
        press(&mut app, KeyCode::Char('?'), none);
        screen(&mut app);
        assert_eq!(app.video_placement(), pepper::remove(&token));
        press(&mut app, KeyCode::Esc, none);
        screen(&mut app);
        assert_eq!(app.video_placement(), pepper::place(&token, full));
        // `f` puts it back over its post, playing on: the box only moves.
        press(&mut app, KeyCode::Char('f'), none);
        assert!(!app.full_screen() && app.playing.is_some());
        let rows = screen(&mut app);
        assert!(rows.iter().any(|r| r.contains("Instagram · Feed")));
        let small = app.video_box.expect("its post is in view");
        assert!(small.height < full.height);
        assert_eq!(app.video_placement(), pepper::place(&token, small));
        // So does Esc, without stopping it; from the comments too.
        press(&mut app, KeyCode::Char('c'), none);
        press(&mut app, KeyCode::Char('f'), none);
        assert!(app.full_screen());
        press(&mut app, KeyCode::Esc, none);
        assert!(!app.full_screen() && app.playing.is_some());
        assert!(app.comments.is_some(), "still open beside it");
        // Gone with the video.
        press(&mut app, KeyCode::Char('f'), none);
        app.on_player(PlayerEvent::Exited { play: 1 });
        assert!(!app.full_screen());
        let rows = screen(&mut app);
        assert!(rows.iter().any(|r| r.contains("Instagram · Feed")));
    }

    #[test]
    fn a_video_whose_sound_comes_apart_plays_once_both_are_downloaded_or_without_it() {
        let mut app = test_app("feed-video-sound");
        let none = KeyModifiers::NONE;
        app.pepper = Some(Pepper {
            port: "x.1".into(),
            play: "/nonexistent/pepper-play".into(),
        });
        open_the_feed(&mut app);
        let dash = post(
            1,
            serde_json::json!({"media": [{"kind": "video", "file_id": 7, "audio": 9,
                "duration": 15, "thumbnail": {"file_id": 8, "width": 1080, "height": 1920}}]}),
        );
        let unplayable = post(
            2,
            serde_json::json!({"media": [{"kind": "video", "file_id": 0,
                "thumbnail": {"file_id": 10, "width": 1080, "height": 1920}}]}),
        );
        feed_page(&mut app, vec![dash, unplayable], false);
        press(&mut app, KeyCode::Enter, none);
        let asked: Vec<i64> = sent_with(&app, "download")
            .iter()
            .filter_map(|p| p["file_id"].as_i64())
            .collect();
        assert_eq!(asked, [7, 9], "the picture and its sound");
        let downloaded = |app: &mut App, file_id, path: Option<&str>| {
            app.on_meta(MetaEvent::Downloaded {
                file_id,
                path: path.map(String::from),
            })
        };
        downloaded(&mut app, 7, Some("/data/7.mp4"));
        assert!(
            app.playing.is_some() && app.status.is_none(),
            "waits for its sound"
        );
        downloaded(&mut app, 9, Some("/data/9.m4a"));
        // pepper-play isn't there in tests: trying it says so.
        assert!(app.playing.is_none());
        assert!(app.status.take().unwrap().contains("Can't play the video"));
        // A sound that doesn't come: the picture plays without it.
        press(&mut app, KeyCode::Enter, none);
        downloaded(&mut app, 9, None);
        assert!(app.status.take().unwrap().contains("sound didn't download"));
        assert!(app.playing.as_ref().is_some_and(|p| p.audio.is_none()));
        downloaded(&mut app, 7, Some("/data/7.mp4"));
        assert!(app.status.take().unwrap().contains("Can't play the video"));
        // A video that can't be had shows its post, and says where to watch it.
        press(&mut app, KeyCode::Char('j'), none);
        press(&mut app, KeyCode::Enter, none);
        assert!(app.playing.is_none());
        assert!(
            app.status
                .as_ref()
                .unwrap()
                .contains("o opens it on instagram.com")
        );
    }

    #[test]
    fn a_feed_photo_shows_in_the_viewer_where_h_and_l_go_through_carousels_and_posts() {
        let mut app = test_app("feed-viewer");
        let none = KeyModifiers::NONE;
        open_the_feed(&mut app);
        let carousel = post(
            1,
            serde_json::json!({"media": [{"kind": "photo", "file_id": 11},
                {"kind": "photo", "file_id": 13}]}),
        );
        feed_page(
            &mut app,
            vec![carousel, post(2, serde_json::json!({}))],
            false,
        );
        assert!(screen(&mut app).iter().any(|r| r.contains("1/2 ●○")));
        press(&mut app, KeyCode::Char(']'), none);
        assert!(screen(&mut app).iter().any(|r| r.contains("2/2 ○●")));
        press(&mut app, KeyCode::Char('['), none);
        press(&mut app, KeyCode::Enter, none);
        let shown = |app: &App| app.photo_view.as_ref().map(|v| v.photo.file_id);
        assert_eq!(shown(&app), Some(11));
        assert_eq!(app.photo_view.as_ref().unwrap().chat_id, FEED);
        press(&mut app, KeyCode::Char('l'), none);
        assert_eq!(shown(&app), Some(13));
        press(&mut app, KeyCode::Char('l'), none);
        assert_eq!(shown(&app), Some(20));
        assert_eq!(app.feed.as_ref().unwrap().selected, 1);
        press(&mut app, KeyCode::Char('l'), none);
        assert_eq!(shown(&app), Some(20));
        assert!(app.status.as_ref().unwrap().contains("No more photos"));
        press(&mut app, KeyCode::Char('h'), none);
        assert_eq!(shown(&app), Some(13));
        press(&mut app, KeyCode::Esc, none);
        assert!(app.photo_view.is_none() && app.feed.is_some());
    }

    /// A comment as the helper sends it.
    fn comment(id: i64, fields: serde_json::Value) -> CommentInfo {
        let mut c = serde_json::json!({"id": id, "author": {"name": "Sam Rivera",
            "username": "sam.climbs"}, "date": unix_now(), "text": format!("comment {id}")});
        if let (Some(c), Some(fields)) = (c.as_object_mut(), fields.as_object()) {
            for (key, value) in fields {
                c.insert(key.clone(), value.clone());
            }
        }
        serde_json::from_value(c).unwrap()
    }

    /// The comments page the popup waits for comes.
    fn comments_page(app: &mut App, comments: Vec<CommentInfo>, more: bool) {
        let popup = app.comments.as_ref().unwrap();
        let (load, _) = popup.loading.expect("a load on its way");
        let post_id = popup.post_id;
        app.on_meta(MetaEvent::Comments {
            post_id,
            load,
            page: Ok((comments, more)),
        });
    }

    #[test]
    fn c_shows_a_posts_comments_beside_it_while_its_video_plays_on() {
        let mut app = test_app("feed-comments");
        let none = KeyModifiers::NONE;
        app.pepper = Some(Pepper {
            port: "x.1".into(),
            play: "/nonexistent/pepper-play".into(),
        });
        open_the_feed(&mut app);
        feed_page(
            &mut app,
            vec![post(1, serde_json::json!({})), video_post(2)],
            false,
        );
        press(&mut app, KeyCode::Char('j'), none);
        // Its video playing, as if pepper-play started.
        press(&mut app, KeyCode::Enter, none);
        let playing = app.playing.as_mut().unwrap();
        let token = playing.token.clone();
        playing.player = Some(Player::pretend(&token, Path::new("/data/7.mp4"), None, 1));
        screen_of(&mut app, 160);
        app.video_placement();

        press(&mut app, KeyCode::Char('c'), none);
        let asked = sent_with(&app, "load_comments");
        assert_eq!(asked.len(), 1);
        assert_eq!(
            (asked[0]["post_id"].as_i64(), asked[0]["more"].as_bool()),
            (Some(2), Some(false))
        );
        assert!(asked[0].get("replies_of").is_none());
        assert!(app.comments_focus && !app.popup_over_chat());
        let rows = screen_of(&mut app, 160);
        let pane = rows[0]
            .find("Comments · @alice.example")
            .expect("the pane's title");
        // The video stays, beside the pane: moved, not taken away.
        let area = app.video_box.expect("the post is still in view");
        assert_eq!(app.video_placement(), pepper::place(&token, area));
        assert!(usize::from(area.right()) < rows[0][..pane].chars().count());
        assert!(!app.playing.as_ref().unwrap().paused, "it plays on");
        // Space pauses it from the comments too.
        press(&mut app, KeyCode::Char(' '), none);
        assert!(app.playing.as_ref().unwrap().paused);
        // Enter while the page loads says so, and asks for nothing.
        press(&mut app, KeyCode::Enter, none);
        assert!(app.status.take().unwrap().contains("Still loading"));
        comments_page(
            &mut app,
            vec![
                comment(10, serde_json::json!({})),
                comment(11, serde_json::json!({"replies": 2})),
            ],
            false,
        );
        let rows = screen_of(&mut app, 160);
        assert!(rows.iter().any(|r| r.contains("comment 10")), "{rows:#?}");
        assert!(
            rows.iter().any(|r| r.contains("View 2 replies")),
            "{rows:#?}"
        );
        // `j` moves through the comments, not the posts.
        press(&mut app, KeyCode::Char('j'), none);
        assert_eq!(app.feed.as_ref().unwrap().selected, 1);
        press(&mut app, KeyCode::Enter, none);
        let asked = sent_with(&app, "load_comments");
        assert_eq!(asked.len(), 2);
        assert_eq!(asked[1]["replies_of"], 11);
        comments_page(
            &mut app,
            vec![comment(20, serde_json::json!({"text": "a reply"}))],
            false,
        );
        assert!(
            screen_of(&mut app, 160)
                .iter()
                .any(|r| r.contains("a reply"))
        );
        press(&mut app, KeyCode::Enter, none);
        assert!(
            !screen_of(&mut app, 160)
                .iter()
                .any(|r| r.contains("a reply")),
            "hidden again"
        );
        // `h` to the post and `l` back; another post closes them.
        press(&mut app, KeyCode::Char('h'), none);
        assert!(!app.comments_focus && app.comments.is_some());
        assert_eq!(app.focus, Focus::Messages, "the post, not the chat list");
        press(&mut app, KeyCode::Char('l'), none);
        assert!(app.comments_focus);
        press(&mut app, KeyCode::Char('h'), none);
        press(&mut app, KeyCode::Char('k'), none);
        assert!(app.comments.is_none() && !app.comments_focus);
        assert!(app.playing.is_none(), "its post left, the video stops");
        // `c` again, then Esc, closes them.
        press(&mut app, KeyCode::Char('c'), none);
        assert!(app.comments.is_some());
        press(&mut app, KeyCode::Esc, none);
        assert!(app.comments.is_none() && app.feed.is_some());
        // An answer for comments closed is dropped.
        app.on_meta(MetaEvent::Comments {
            post_id: 1,
            load: 1,
            page: Ok((vec![comment(30, serde_json::json!({}))], false)),
        });
        assert!(app.comments.is_none());
        // Leaving the feed closes them too.
        press(&mut app, KeyCode::Char('c'), none);
        let chat = app
            .chats
            .ids()
            .iter()
            .copied()
            .find(|&id| id != FEED)
            .unwrap();
        app.open_chat(chat);
        assert!(app.comments.is_none());
    }

    #[test]
    fn o_opens_the_post_on_instagram_and_y_copies_its_link() {
        let mut app = test_app("feed-link");
        open_the_feed(&mut app);
        feed_page(&mut app, vec![post(1, serde_json::json!({}))], false);
        assert_eq!(
            app.feed_link().map(|l| l.url).as_deref(),
            Some("https://www.instagram.com/p/ABC/")
        );
        assert!(
            app.confirm.is_none(),
            "an instagram.com link opens without asking"
        );
    }

    #[test]
    fn the_feed_and_its_row_go_once_instagram_is_logged_out_of() {
        let mut app = test_app("feed-logout");
        open_the_feed(&mut app);
        feed_page(&mut app, vec![post(1, serde_json::json!({}))], false);
        app.on_meta(MetaEvent::Account {
            network: Network::Instagram,
            state: AccountState::LoggedOut,
            user_id: None,
            name: None,
            error: None,
        });
        assert!(app.feed.is_none() && app.feeds_kept.is_empty());
        assert_eq!(app.focus, Focus::Chats);
        app.chats.refresh();
        assert!(app.chats.get(FEED).is_none());
    }

    #[test]
    fn a_chat_opened_from_the_feed_keeps_the_feed_to_come_back_to() {
        let mut app = test_app("feed-kept");
        open_the_feed(&mut app);
        feed_page(
            &mut app,
            (1..=3).map(|i| post(i, serde_json::json!({}))).collect(),
            false,
        );
        press(&mut app, KeyCode::Char('j'), KeyModifiers::NONE);
        let chat = app
            .chats
            .ids()
            .iter()
            .copied()
            .find(|&id| id != FEED)
            .unwrap();
        app.open_chat(chat);
        assert!(app.feed.is_none() && app.open.is_some());
        // Ctrl-o comes back to it as it was.
        press(&mut app, KeyCode::Char('o'), KeyModifiers::CONTROL);
        let feed = app.feed.as_ref().expect("back to the feed");
        assert_eq!((feed.posts.len(), feed.selected), (3, 1));
        assert_eq!(sent_with(&app, "load_feed").len(), 1, "not loaded again");
        press(&mut app, KeyCode::Char('i'), KeyModifiers::CONTROL);
        assert_eq!(app.open.as_ref().map(|o| o.chat_id), Some(chat));
    }

    /// The Threads account comes in, as the helper says it.
    fn threads_in(app: &mut App) {
        app.on_meta(MetaEvent::Account {
            network: Network::Threads,
            state: AccountState::Ready,
            user_id: None,
            name: Some("alice.example".into()),
            error: None,
        });
    }

    /// Opens Threads' feed from its row in the Threads tab.
    fn open_threads(app: &mut App) {
        threads_in(app);
        app.focus = Focus::Chats;
        app.chats.show(List::Network(Network::Threads));
        app.chats.refresh();
        app.selected = Some(chats::THREADS_FEED);
        press(app, KeyCode::Enter, KeyModifiers::NONE);
    }

    /// The page Threads' feed waits for comes.
    fn threads_page(app: &mut App, posts: Vec<FeedPost>, more: bool) {
        let load = app
            .feed
            .as_ref()
            .unwrap()
            .loading
            .expect("a load on its way");
        app.on_meta(MetaEvent::Feed {
            network: Network::Threads,
            load,
            page: Ok((posts, more)),
        });
    }

    /// A Threads post of text alone, with `fields` besides.
    fn thread(id: i64, fields: serde_json::Value) -> FeedPost {
        let mut all = serde_json::json!({
            "media": [], "caption": "words, no photo", "reposts": 2, "shares": 5,
            "url": "https://www.threads.com/@alice.example/post/ABC"
        });
        if let (Some(all), Some(fields)) = (all.as_object_mut(), fields.as_object()) {
            for (key, value) in fields {
                all.insert(key.clone(), value.clone());
            }
        }
        post(id, all)
    }

    #[test]
    fn threads_has_a_tab_with_its_feed_and_no_chats_to_load() {
        let mut app = test_app("threads-row");
        threads_in(&mut app);
        assert!(
            sent_with(&app, "load_chats")
                .iter()
                .all(|p| p["network"] != "threads"),
            "Threads has no chats"
        );
        app.chats.refresh();
        assert!(app.chats.tabs().iter().any(|t| t.name == "Threads"));
        app.chats.show(List::Network(Network::Threads));
        app.chats.refresh();
        assert_eq!(app.chats.ids(), &[chats::THREADS_FEED]);
        // With Instagram in too, each feed is in its own tab.
        instagram_in(&mut app);
        app.chats.show(List::Network(Network::Instagram));
        app.chats.refresh();
        assert_eq!(app.chats.ids().first(), Some(&FEED));
        assert!(!app.chats.ids().contains(&chats::THREADS_FEED));
        // Nothing that acts on a chat goes to the helper for it.
        app.chats.show(List::Network(Network::Threads));
        app.chats.refresh();
        app.selected = Some(chats::THREADS_FEED);
        app.focus = Focus::Chats;
        press(&mut app, KeyCode::Char('m'), KeyModifiers::NONE);
        press(&mut app, KeyCode::Char('H'), KeyModifiers::NONE);
        assert!(sent_with(&app, "mute").is_empty());
        assert!(app.settings.highlighted_chats.is_empty());
    }

    #[test]
    fn threads_feed_opens_for_you_and_following_switches_to_the_accounts_followed() {
        let mut app = test_app("threads-feed");
        open_threads(&mut app);
        let asked = sent_with(&app, "load_feed");
        assert_eq!(asked.len(), 1);
        assert_eq!(
            (&asked[0]["network"], &asked[0]["feed"]),
            (&serde_json::json!("threads"), &serde_json::json!("for_you"))
        );
        threads_page(
            &mut app,
            (1..=3).map(|i| thread(i, serde_json::json!({}))).collect(),
            true,
        );
        let rows = screen(&mut app);
        assert!(
            rows.iter().any(|r| r.contains("Threads · For you")),
            "{rows:#?}"
        );
        assert!(
            rows.iter().any(|r| r.contains("words, no photo")),
            "{rows:#?}"
        );
        assert!(
            rows.iter()
                .any(|r| r.contains("12 likes · 3 replies · 2 reposts · 5 shares")),
            "{rows:#?}"
        );
        command(&mut app, "following");
        let asked = sent_with(&app, "load_feed");
        let last = asked.last().unwrap();
        assert_eq!(
            (&last["feed"], &last["fresh"]),
            (&serde_json::json!("following"), &serde_json::json!(true))
        );
        threads_page(
            &mut app,
            vec![thread(9, serde_json::json!({"caption": "followed"}))],
            false,
        );
        let feed = app.feed.as_ref().unwrap();
        assert!(feed.following);
        assert_eq!(feed.posts.iter().map(|p| p.id).collect::<Vec<_>>(), vec![9]);
        assert!(
            screen(&mut app)
                .iter()
                .any(|r| r.contains("Threads · Following"))
        );
        // Back to For you.
        command(&mut app, "for-you");
        assert_eq!(
            sent_with(&app, "load_feed").last().unwrap()["feed"],
            "for_you"
        );
    }

    #[test]
    fn following_and_for_you_open_threads_feed_from_anywhere() {
        let mut app = test_app("threads-commands");
        command(&mut app, "following");
        assert!(
            app.status
                .take()
                .unwrap()
                .contains("Threads isn't logged in")
        );
        assert!(sent_with(&app, "load_feed").is_empty());
        threads_in(&mut app);
        // From the demo's open chat.
        app.focus = Focus::Messages;
        command(&mut app, "following");
        assert!(app.open.is_none(), "in the chat's place");
        let feed = app.feed.as_ref().expect("Threads' feed");
        assert!(feed.network == Network::Threads && feed.following);
        assert_eq!(app.selected, Some(chats::THREADS_FEED));
        let asked = sent_with(&app, "load_feed");
        assert_eq!(asked.len(), 1, "only the page asked for: {asked:?}");
        assert_eq!(asked[0]["feed"], "following");
        assert!(
            sent_with(&app, "cancel").is_empty(),
            "no For you to call off"
        );
        let loads = asked.len();
        command(&mut app, "following");
        assert!(app.status.take().unwrap().contains("Following already"));
        assert_eq!(sent_with(&app, "load_feed").len(), loads, "nothing asked");
    }

    #[test]
    fn each_feed_is_kept_as_it_was_while_the_other_is_open() {
        let mut app = test_app("two-feeds");
        open_threads(&mut app);
        threads_page(
            &mut app,
            (1..=3).map(|i| thread(i, serde_json::json!({}))).collect(),
            true,
        );
        press(&mut app, KeyCode::Char('j'), KeyModifiers::NONE);
        open_the_feed(&mut app);
        assert_eq!(app.feed.as_ref().unwrap().network, Network::Instagram);
        feed_page(&mut app, vec![post(20, serde_json::json!({}))], false);
        // Back to Threads: as it was, not loaded again.
        let loads = sent_with(&app, "load_feed").len();
        app.focus = Focus::Chats;
        app.chats.show(List::Network(Network::Threads));
        app.chats.refresh();
        app.selected = Some(chats::THREADS_FEED);
        press(&mut app, KeyCode::Enter, KeyModifiers::NONE);
        let feed = app.feed.as_ref().unwrap();
        assert_eq!(
            (feed.network, feed.selected, feed.posts.len()),
            (Network::Threads, 1, 3)
        );
        assert_eq!(sent_with(&app, "load_feed").len(), loads);
        assert_eq!(app.here().map(|h| h.chat_id), Some(chats::THREADS_FEED));
        // Logging out of Instagram forgets its feed only.
        app.on_meta(MetaEvent::Account {
            network: Network::Instagram,
            state: AccountState::LoggedOut,
            user_id: None,
            name: None,
            error: None,
        });
        assert!(app.feed.is_some() && !app.feeds_kept.contains_key(&Network::Instagram));
    }

    #[test]
    fn c_shows_a_threads_posts_replies_and_enter_on_a_link_card_asks_before_a_look_alike() {
        let mut app = test_app("threads-replies");
        open_threads(&mut app);
        threads_page(
            &mut app,
            vec![thread(
                1,
                serde_json::json!({"link_preview": {"url": "https://paypal.com@evil.example/login",
                                                      "title": "Your account"}}),
            )],
            false,
        );
        press(&mut app, KeyCode::Char('c'), KeyModifiers::NONE);
        let comments = app.comments.as_ref().expect("the replies pane");
        assert_eq!(comments.what, "Replies");
        assert_eq!(sent_with(&app, "load_comments")[0]["post_id"], 1);
        let load = comments.loading.unwrap().0;
        app.on_meta(MetaEvent::Comments {
            post_id: 1,
            load,
            page: Ok((
                vec![
                    serde_json::from_value(serde_json::json!({"id": 5,
                        "author": {"username": "sam"}, "text": "look", "attachment": "photo"}))
                    .unwrap(),
                ],
                false,
            )),
        });
        let rows = screen(&mut app);
        assert!(
            rows.iter().any(|r| r.contains("Replies · @alice.example")),
            "{rows:#?}"
        );
        assert!(rows.iter().any(|r| r.contains("[Photo]")), "{rows:#?}");
        press(&mut app, KeyCode::Char('c'), KeyModifiers::NONE);
        // A text post's Enter is its link, which asks first when its address
        // has someone's name before its host.
        app.feed_changed = None;
        press(&mut app, KeyCode::Enter, KeyModifiers::NONE);
        let confirm = app.confirm.as_ref().expect("asks before opening it");
        assert_eq!(confirm.site.as_deref(), Some("evil.example"));
    }

    /// A message from someone else in the open chat, settled under the
    /// cursor, with `text`.
    fn settled_message(app: &mut App, text: &str) {
        let id = app.open.as_ref().unwrap().newest_id().unwrap_or(1 << 40) + 1_000_000;
        app.on_meta(MetaEvent::Message(Box::new(incoming(app, id, text))));
        settled(app, id);
        app.focus = Focus::Messages;
    }

    #[test]
    fn enter_on_an_instagram_posts_link_shows_the_post_here_and_esc_goes_on_to_the_feed() {
        let mut app = test_app("linked-post");
        let none = KeyModifiers::NONE;
        instagram_in(&mut app);
        let chat_id = app.open.as_ref().unwrap().chat_id;
        settled_message(
            &mut app,
            "look https://www.instagram.com/reel/DAbc_12-x/?igsh=abc",
        );
        press(&mut app, KeyCode::Enter, none);
        assert!(app.confirm.is_none() && app.open.is_none());
        let feed = app.feed.as_ref().expect("the post in the chat's place");
        assert_eq!(
            feed.linked.as_ref().map(|l| l.code.as_str()),
            Some("DAbc_12-x")
        );
        assert_eq!(app.selected, Some(FEED));
        let asked = sent_with(&app, "load_post");
        assert_eq!(
            (
                &asked[0]["network"],
                &asked[0]["code"],
                asked[0].get("username")
            ),
            (
                &serde_json::json!("instagram"),
                &serde_json::json!("DAbc_12-x"),
                None
            )
        );
        assert!(sent_with(&app, "load_feed").is_empty(), "not the feed yet");
        let rows = screen(&mut app);
        assert!(
            rows.iter().any(|r| r.contains("Loading the post…")),
            "{rows:#?}"
        );
        // An answer to another load is old news.
        let load = app.feed.as_ref().unwrap().loading.unwrap();
        app.on_meta(MetaEvent::Post {
            network: Network::Instagram,
            load: load + 1,
            post: Err("late".into()),
        });
        assert!(app.feed.as_ref().unwrap().loading.is_some());
        app.on_meta(MetaEvent::Post {
            network: Network::Instagram,
            load,
            post: Ok(Box::new(post(
                30,
                serde_json::json!({"caption": "the reel"}),
            ))),
        });
        let rows = screen(&mut app);
        for shown in ["Instagram · Post", "the reel", "Esc goes on to the feed"] {
            assert!(rows.iter().any(|r| r.contains(shown)), "{shown}: {rows:#?}");
        }
        // Its comments open as a feed post's.
        press(&mut app, KeyCode::Char('c'), none);
        assert_eq!(sent_with(&app, "load_comments")[0]["post_id"], 30);
        press(&mut app, KeyCode::Esc, none);
        assert!(app.comments.is_none(), "Esc closes them first");
        // `r` asks for it again, not for the feed.
        press(&mut app, KeyCode::Char('r'), none);
        assert_eq!(sent_with(&app, "load_post").len(), 2);
        assert!(sent_with(&app, "load_feed").is_empty());
        // Esc: the feed itself, its first page.
        press(&mut app, KeyCode::Esc, none);
        let feed = app.feed.as_ref().unwrap();
        assert!(feed.linked.is_none() && feed.network == Network::Instagram);
        assert_eq!(sent_with(&app, "load_feed").len(), 1);
        // Ctrl-o goes back to the chat it was opened from.
        press(&mut app, KeyCode::Char('o'), KeyModifiers::CONTROL);
        assert_eq!(app.open.as_ref().map(|o| o.chat_id), Some(chat_id));
        assert!(app.feeds_kept.contains_key(&Network::Instagram));
    }

    #[test]
    fn a_linked_post_leaves_the_feed_as_it_was_and_goes_when_left() {
        let mut app = test_app("linked-keeps-feed");
        let none = KeyModifiers::NONE;
        let chat_id = app.open.as_ref().unwrap().chat_id;
        open_the_feed(&mut app);
        feed_page(
            &mut app,
            (1..=3).map(|i| post(i, serde_json::json!({}))).collect(),
            true,
        );
        press(&mut app, KeyCode::Char('j'), none);
        // Back in the chat, a link to one.
        app.focus = Focus::Chats;
        app.chats.show(List::Main);
        app.chats.refresh();
        app.selected = Some(chat_id);
        press(&mut app, KeyCode::Enter, none);
        settled_message(&mut app, "https://instagram.com/maya.lens/p/DAbc12/");
        press(&mut app, KeyCode::Enter, none);
        assert_eq!(
            app.feed
                .as_ref()
                .and_then(|f| f.linked.as_ref())
                .map(|l| l.code.as_str()),
            Some("DAbc12")
        );
        // A page for the feed meanwhile goes to the feed, not the post.
        let kept = app
            .feeds_kept
            .get(&Network::Instagram)
            .expect("the feed, kept");
        assert_eq!((kept.selected, kept.posts.len()), (1, 3));
        // The feed's row opens the feed as it was; the post is gone.
        app.focus = Focus::Chats;
        app.chats.show(List::Network(Network::Instagram));
        app.chats.refresh();
        app.selected = Some(FEED);
        let loads = sent_with(&app, "load_feed").len();
        press(&mut app, KeyCode::Enter, none);
        let feed = app.feed.as_ref().unwrap();
        assert!(feed.linked.is_none());
        assert_eq!((feed.selected, feed.posts.len()), (1, 3));
        assert_eq!(sent_with(&app, "load_feed").len(), loads);
    }

    #[test]
    fn a_threads_link_names_its_author_and_a_post_that_cant_come_says_why() {
        let mut app = test_app("linked-thread");
        let none = KeyModifiers::NONE;
        threads_in(&mut app);
        settled_message(&mut app, "https://www.threads.net/@maya.lens/post/C8xYz-1");
        press(&mut app, KeyCode::Enter, none);
        let asked = sent_with(&app, "load_post");
        assert_eq!(
            (
                &asked[0]["network"],
                &asked[0]["code"],
                &asked[0]["username"]
            ),
            (
                &serde_json::json!("threads"),
                &serde_json::json!("C8xYz-1"),
                &serde_json::json!("maya.lens")
            )
        );
        assert_eq!(app.selected, Some(chats::THREADS_FEED));
        let load = app.feed.as_ref().unwrap().loading.unwrap();
        app.on_meta(MetaEvent::Post {
            network: Network::Threads,
            load,
            post: Err("Threads has no post of this link that this account can see; o opens it on threads.com.".into()),
        });
        let rows = screen(&mut app);
        assert!(
            rows.iter().any(|r| r.contains("Threads · Post")),
            "{rows:#?}"
        );
        assert!(
            rows.iter().any(|r| r.contains("Esc goes on to the feed")),
            "{rows:#?}"
        );
        // `o` would open the link itself.
        assert_eq!(
            app.feed_link().map(|l| l.url),
            Some("https://www.threads.net/@maya.lens/post/C8xYz-1".to_string())
        );
        // `f` is for a video, and this post has none.
        press(&mut app, KeyCode::Char('f'), none);
        assert!(app.status.take().unwrap().contains("No video here"));
        assert!(sent_with(&app, "load_feed").is_empty());
        // `:following` goes on to the feed, the accounts followed.
        command(&mut app, "following");
        let feed = app.feed.as_ref().unwrap();
        assert!(feed.linked.is_none() && feed.following);
        assert_eq!(
            sent_with(&app, "load_feed").last().unwrap()["feed"],
            "following"
        );
    }

    #[test]
    fn a_post_link_of_a_network_not_logged_into_opens_as_any_link() {
        let mut app = test_app("linked-logged-out");
        app.focus = Focus::Messages;
        let id = app.open.as_ref().unwrap().newest_id().unwrap() + 1_000_000;
        // The demo is logged into Instagram, not Threads.
        let url = "https://www.threads.com/@maya.lens/post/C8xYz";
        app.on_meta(MetaEvent::Message(Box::new(incoming_disguised(
            &app, id, url,
        ))));
        settled(&mut app, id);
        press(&mut app, KeyCode::Enter, KeyModifiers::NONE);
        assert!(app.feed.is_none() && sent_with(&app, "load_post").is_empty());
        assert_eq!(opened_link(&app).as_deref(), Some(url));
    }

    #[test]
    fn a_linked_post_left_before_it_came_is_called_off_and_one_that_came_isnt() {
        let mut app = test_app("linked-left");
        let none = KeyModifiers::NONE;
        let called_off = |app: &App| sent_with(app, "cancel");
        let request = |app: &App| app.feed.as_ref().unwrap().request.unwrap();
        instagram_in(&mut app);
        settled_message(&mut app, "https://www.instagram.com/p/LEFT1/");
        // Esc on to the feed while it loads.
        press(&mut app, KeyCode::Enter, none);
        let first = request(&app);
        press(&mut app, KeyCode::Esc, none);
        assert_eq!(called_off(&app), [serde_json::json!({"id": first})]);
        // The feed itself is kept, its page still on its way.
        press(&mut app, KeyCode::Char('o'), KeyModifiers::CONTROL);
        assert!(app.open.is_some());
        assert_eq!(called_off(&app).len(), 1);
        // Ctrl-o back to the chat while it loads. (Each time back, the chat
        // waits for its history, which nothing here answers: the link comes
        // again.)
        settled_message(&mut app, "https://www.instagram.com/p/LEFT2/");
        press(&mut app, KeyCode::Enter, none);
        let second = request(&app);
        assert_ne!(first, second);
        press(&mut app, KeyCode::Char('o'), KeyModifiers::CONTROL);
        assert_eq!(called_off(&app)[1], serde_json::json!({"id": second}));
        // Once it came, leaving it calls nothing off.
        settled_message(&mut app, "https://www.instagram.com/p/CAME3/");
        press(&mut app, KeyCode::Enter, none);
        let load = app.feed.as_ref().unwrap().loading.unwrap();
        app.on_meta(MetaEvent::Post {
            network: Network::Instagram,
            load,
            post: Ok(Box::new(post(5, serde_json::json!({})))),
        });
        assert_eq!(app.feed.as_ref().unwrap().request, None);
        press(&mut app, KeyCode::Char('o'), KeyModifiers::CONTROL);
        assert!(app.open.is_some());
        assert_eq!(called_off(&app).len(), 2);
    }

    #[test]
    fn comments_closed_before_their_page_came_and_a_page_r_replaced_are_called_off() {
        let mut app = test_app("comments-left");
        let none = KeyModifiers::NONE;
        let called_off = |app: &App| sent_with(app, "cancel");
        open_the_feed(&mut app);
        // `r` while the first page loads: that page isn't wanted any more.
        let first = app.feed.as_ref().unwrap().request.unwrap();
        press(&mut app, KeyCode::Char('r'), none);
        assert_eq!(called_off(&app), [serde_json::json!({"id": first})]);
        feed_page(&mut app, vec![post(1, serde_json::json!({}))], false);
        // The comments, closed while their first page loads.
        press(&mut app, KeyCode::Char('c'), none);
        let page = app.comments.as_ref().unwrap().request.unwrap();
        press(&mut app, KeyCode::Esc, none);
        assert!(app.comments.is_none());
        assert_eq!(called_off(&app)[1], serde_json::json!({"id": page}));
        // Once it came, closing them calls nothing off.
        press(&mut app, KeyCode::Char('c'), none);
        comments_page(&mut app, vec![comment(10, serde_json::json!({}))], false);
        press(&mut app, KeyCode::Esc, none);
        assert_eq!(called_off(&app).len(), 2);
    }

    #[test]
    fn a_late_answer_for_the_last_link_isnt_taken_for_the_next() {
        let mut app = test_app("linked-late");
        let none = KeyModifiers::NONE;
        instagram_in(&mut app);
        settled_message(&mut app, "https://www.instagram.com/p/FIRST1/");
        press(&mut app, KeyCode::Enter, none);
        let first = app.feed.as_ref().unwrap().loading.unwrap();
        // Back to the chat before it came, and on to another link.
        press(&mut app, KeyCode::Char('o'), KeyModifiers::CONTROL);
        assert!(app.open.is_some());
        settled_message(&mut app, "https://www.instagram.com/p/NEXT2/");
        app.open.as_mut().unwrap().selected = None;
        press(&mut app, KeyCode::Enter, none);
        app.on_meta(MetaEvent::Post {
            network: Network::Instagram,
            load: first,
            post: Ok(Box::new(post(
                1,
                serde_json::json!({"caption": "the first"}),
            ))),
        });
        let feed = app.feed.as_ref().unwrap();
        assert_eq!(feed.linked.as_ref().map(|l| l.code.as_str()), Some("NEXT2"));
        assert!(
            feed.posts.is_empty() && feed.loading.is_some(),
            "still waiting for its own"
        );
    }

    #[test]
    fn a_post_link_behind_other_words_asks_before_showing_the_post() {
        let mut app = test_app("linked-disguised");
        let none = KeyModifiers::NONE;
        instagram_in(&mut app);
        app.focus = Focus::Messages;
        let id = app.open.as_ref().unwrap().newest_id().unwrap() + 1_000_000;
        let url = "https://www.instagram.com/p/SCAM1/";
        app.on_meta(MetaEvent::Message(Box::new(incoming_disguised(
            &app, id, url,
        ))));
        settled(&mut app, id);
        press(&mut app, KeyCode::Enter, none);
        let confirm = app.confirm.as_mut().expect("asks first");
        assert_eq!(confirm.title, "Open this post?");
        assert_eq!(confirm.lines[0], "The text says: doc");
        assert_eq!(confirm.site.as_deref(), Some("www.instagram.com"));
        assert!(app.feed.is_none() && sent_with(&app, "load_post").is_empty());
        app.confirm.as_mut().unwrap().shown = Instant::now().checked_sub(CONFIRM_GRACE).unwrap();
        press(&mut app, KeyCode::Char('y'), none);
        assert_eq!(
            app.feed
                .as_ref()
                .and_then(|f| f.linked.as_ref())
                .map(|l| l.code.as_str()),
            Some("SCAM1")
        );
        assert_eq!(sent_with(&app, "load_post").len(), 1);
    }

    #[test]
    fn a_post_link_puts_the_cursor_on_the_feeds_row_in_its_tab_and_keeps_the_filter() {
        let mut app = test_app("linked-tab");
        instagram_in(&mut app);
        // Three networks: the list of every chat has no feed row.
        app.chats.set_filter("fee");
        app.chats.refresh();
        assert!(!app.chats.ids().contains(&FEED));
        settled_message(&mut app, "https://www.instagram.com/p/DAbc12/");
        press(&mut app, KeyCode::Enter, KeyModifiers::NONE);
        assert_eq!(app.chats.shown(), List::Network(Network::Instagram));
        assert_eq!(app.chats.filter(), "fee");
        assert!(app.chats.ids().contains(&FEED));
        assert_eq!(app.selected, Some(FEED));
    }

    #[test]
    fn in_the_comments_one_g_waits_for_another_as_in_every_pane() {
        let mut app = test_app("comments-gg");
        let none = KeyModifiers::NONE;
        open_the_feed(&mut app);
        feed_page(&mut app, vec![post(1, serde_json::json!({}))], false);
        press(&mut app, KeyCode::Char('c'), none);
        comments_page(
            &mut app,
            (10..=13)
                .map(|i| comment(i, serde_json::json!({})))
                .collect(),
            false,
        );
        press(&mut app, KeyCode::Char('G'), none);
        let last = app.comments.as_ref().unwrap().selected;
        assert!(last > 0);
        press(&mut app, KeyCode::Char('g'), none);
        assert_eq!(app.comments.as_ref().unwrap().selected, last, "not yet");
        press(&mut app, KeyCode::Char('g'), none);
        assert_eq!(app.comments.as_ref().unwrap().selected, 0);
    }
}
