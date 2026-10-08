use std::collections::{HashMap, HashSet};
use std::path::{Path, PathBuf};
use std::process::{self, Stdio};
use std::time::{Duration, SystemTime};

use anyhow::Result;
use crossterm::event::{Event, EventStream, KeyCode, KeyEvent, KeyEventKind, KeyModifiers};
use futures::StreamExt;
use ratatui::DefaultTerminal;
use ratatui::style::Style;
use ratatui::widgets::Block;
use ratatui_textarea::{DataCursor, TextArea};
use tokio::sync::mpsc::UnboundedReceiver;
use tokio::time::{Instant, sleep_until};

use crate::attach::{self, Attachment, Dropped};
use crate::chats::{Chats, List, Presence};
use crate::clipboard::{Clipboard, ClipboardEvent, Copied, Decoded, Paste, Pasted};
use crate::complete::{self, Completion};
use crate::config;
use crate::images::{ImageEvent, Images};
use crate::messages::{
    Editable, Editing, Link, MediaFile, OpenChat, Replied, SendState, link_host,
};
use crate::meta::{AccountState, Meta, MetaEvent, Network, Page};
use crate::notify::{self, Note, Notifications, Notifier};
use crate::picker::{ChatPicker, Choice};
use crate::reactions::{self, ReactMenu};
use crate::settings::{self, Settings, Side};
use crate::text;
use crate::theme::{Colors, Themes};
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

pub enum Screen {
    Login(Box<Login>),
    Main,
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
    LoggingOut,
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
        input.set_block(Block::bordered());
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
        matches!(self.step, LoginStep::Cookies { .. })
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
    /// When it came up. A `y` in the first moments was typed for whatever
    /// was on screen before, so it doesn't count.
    pub shown: Instant,
}

impl Confirm {
    pub fn new(title: impl Into<String>, lines: Vec<String>, action: Confirmed) -> Self {
        Self {
            title: title.into(),
            lines,
            site: None,
            action,
            shown: Instant::now(),
        }
    }
}

/// What `y` does in a [`Confirm`].
pub enum Confirmed {
    OpenFile(String),
    OpenLink(String),
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
            Confirmed::OpenFile(_) | Confirmed::OpenLink(_) => "open",
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
/// and in message blocks, Normal mode after sending) or picks a theme, and
/// saves it at once.
pub struct SettingsMenu {
    pub tab: HelpTab,
    /// First row shown on the shortcuts tab. Drawing keeps it in range.
    pub scroll: usize,
    /// Row on the settings tab: one of the checkboxes, then the themes from
    /// [`SettingsMenu::THEMES`] on.
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
    /// The "Normal mode after sending" row.
    pub const AFTER_SEND: usize = Self::BLOCK_GAPS + 1;
    /// The first theme's row. The themes come last, since the user's own
    /// can make a long list.
    pub const THEMES: usize = Self::AFTER_SEND + 1;

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
    /// Whom it's with, for the status bar and to match the helper's answer.
    pub request: String,
}

impl Finding {
    fn new(request: &str) -> Self {
        Self {
            request: request.to_string(),
        }
    }
}

/// What `:` runs. There are no abbreviations: only the full name runs, so a
/// typo can't log you out.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Command {
    Login,
    Logout,
}

impl Command {
    pub const ALL: [Command; 2] = [Command::Login, Command::Logout];

    pub fn name(self) -> &'static str {
        match self {
            Command::Login => "login",
            Command::Logout => "logout",
        }
    }

    pub fn about(self) -> &'static str {
        match self {
            Command::Login => "Log in to Messenger or Instagram, or log in again",
            Command::Logout => "Log out of the network of the chat under the cursor (asks first)",
        }
    }

    pub fn parse(text: &str) -> Option<Command> {
        Command::ALL.into_iter().find(|c| c.name() == text)
    }
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
    /// The colors of the theme in use.
    pub colors: Colors,
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
    /// The photo the last frame showed in the viewer, by file id.
    shown_in_viewer: Option<i32>,
    exit: bool,
}

impl App {
    pub fn new(
        meta: Meta,
        images: Images,
        clipboard: Clipboard,
        settings: Settings,
        settings_path: PathBuf,
    ) -> Self {
        let mut chats = Chats::default();
        chats.set_highlighted(&settings.highlighted_chats);
        let notify_with = settings
            .notifications
            .resolve(|name| std::env::var(name).ok());
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
            settings_path,
            colors: Colors::default(),
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
            shown_in_viewer: None,
            exit: false,
        };
        app.use_saved_theme();
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

    pub async fn run(
        mut self,
        terminal: &mut DefaultTerminal,
        mut events: UnboundedReceiver<MetaEvent>,
        mut image_events: UnboundedReceiver<ImageEvent>,
        mut clipboard: UnboundedReceiver<ClipboardEvent>,
    ) -> Result<()> {
        let mut keys = EventStream::new();
        let mut signals = quit_signals();
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
            // Sixel and iTerm2 pictures stay on screen until every cell of
            // them is drawn over, which tmux may skip for blank ones: the
            // viewer's photo once it closes.
            let in_viewer = self.photo_view.as_ref().map(|v| v.photo.file_id);
            if in_viewer != self.shown_in_viewer && self.images.paints_over() {
                let _ = terminal.clear();
            }
            self.shown_in_viewer = in_viewer;
            self.mark_seen();
            self.set_unread_chats(self.chats.unread_chats());
            self.send_notification();
            if let Some(query) = self
                .picker
                .as_mut()
                .and_then(|p| p.due_search(Instant::now()))
            {
                for network in self.ready_networks() {
                    self.meta.find_chats(network, query.clone());
                }
            }
            if failed.is_none()
                && let Err(e) = terminal.draw(|frame| ui::draw(frame, &mut self))
            {
                failed = Some(e.into());
                self.hang_up();
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
            // had to wait, and to search once typing in the `s` picker
            // pauses.
            let wake = [
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
                Some(event) = clipboard.recv() => self.on_clipboard(event),
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
        match failed {
            Some(e) => Err(e),
            None => Ok(()),
        }
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
        match event {
            Event::Key(key) => self.on_key(key),
            Event::FocusGained => {
                self.terminal_focused = true;
                self.focus_reported = true;
                // Back at tuimeta: what was waiting is on screen.
                self.notifier.clear();
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
            MetaEvent::LoggedIn { network, result } => {
                if let Screen::Login(login) = &mut self.screen
                    && matches!(login.step, LoginStep::Cookies { network: n } if n == network)
                {
                    match result {
                        // The account says when it's connected.
                        Ok(()) => login.input = Login::new(LoginStep::Cookies { network }).input,
                        Err(why) => {
                            login.busy = false;
                            login.error = Some(why);
                        }
                    }
                }
            }
            MetaEvent::Error(message) => match &mut self.screen {
                Screen::Login(login) => {
                    login.busy = false;
                    login.error = Some(message);
                }
                Screen::Main => self.status = Some(message),
            },
            MetaEvent::Gone(why) => {
                // Nothing more can come: say so, and leave once the user
                // has read it, or at once while quitting.
                if self.quit_deadline.is_some() {
                    self.exit = true;
                    return;
                }
                let why = format!("{why}. Quit and start tuimeta again; helper.log says more");
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
                    self.users
                        .entry(user_id)
                        .or_insert_with(|| text::clean(&info.title));
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
                    false => format!("Message not sent: {error}"),
                });
                if let Some(open) = self.open.as_mut().filter(|o| o.chat_id == chat_id) {
                    open.set_failed(old_id);
                }
            }
            MetaEvent::MessagesDeleted {
                chat_id,
                message_ids,
            } => {
                self.notifier.remove(chat_id, &message_ids);
                if self
                    .photo_view
                    .as_ref()
                    .is_some_and(|v| message_ids.iter().any(|&id| v.lost(chat_id, id, None)))
                {
                    self.photo_view = None;
                }
                if let Some(open) = self.open.as_mut().filter(|o| o.chat_id == chat_id) {
                    open.remove(&message_ids);
                }
                if self
                    .delete_menu
                    .as_ref()
                    .is_some_and(|m| message_ids.contains(&m.message_id))
                {
                    self.delete_menu = None;
                }
            }
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
                self.images.on_downloaded(file_id, path);
            }
            MetaEvent::ChatsFound {
                network,
                query,
                found,
            } => {
                if let Some(picker) = self.picker.as_mut() {
                    picker.set_found(network, &query, found);
                }
            }
            MetaEvent::ChatFound { request, found } => {
                // Dropped if another chat was opened meanwhile.
                if !self.finding.as_ref().is_some_and(|f| f.request == request) {
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
                    Err(why) => self.status = Some(why),
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
                LoginStep::Cookies { network }
                    if self
                        .accounts
                        .get(&network)
                        .is_some_and(|a| a.state == AccountState::Ready) =>
                {
                    self.screen = Screen::Main;
                    self.show_toast("Logged in", network.name());
                }
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
        self.loading_chats.remove(&network);
        self.loaded_chats.remove(&network);
        self.picker = None;
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
        if let Some(open) = self.open.as_mut().filter(|o| o.chat_id == message.chat_id) {
            open.upsert(message);
            // Edited to another photo, or to no photo at all.
            let msg = open.messages.get(&message.id);
            if self
                .photo_view
                .as_ref()
                .is_some_and(|v| v.lost(message.chat_id, message.id, msg))
            {
                self.photo_view = None;
            }
        }
        self.notify(message);
    }

    /// Tells the user about someone else's new message, unless the chat is
    /// muted, it came before tuimeta started, they see it anyway, or it was
    /// told already.
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
            || self.sees(chat_id)
        {
            return;
        }
        self.notified.insert(chat_id, message.id);
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
                KeyCode::Esc => {
                    let selected = Network::ALL.iter().position(|&n| n == network).unwrap_or(0);
                    let from_main = login.from_main;
                    let mut choose = Login::new(LoginStep::Choose { selected });
                    choose.from_main = from_main;
                    self.screen = Screen::Login(Box::new(choose));
                }
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
                    self.meta.login_cookies(network, value);
                }
                _ => {
                    login.input.input(key);
                }
            },
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
            let paths = open
                .attachments
                .iter()
                .map(|a| a.path.to_string_lossy().into_owned())
                .collect();
            self.meta.send_files(open.chat_id, paths, text, reply_to);
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
            && self.settings_menu.is_none()
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
        if self.focus_reported {
            self.terminal_focused
        } else {
            self.watching(chat_id)
        }
    }

    fn send_notification(&mut self) {
        let Some(alert) = self.notifier.due(Instant::now()) else {
            return;
        };
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
            _ => self.open.as_ref().map(|o| o.chat_id),
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
        // What each network lets a client end differs: Facebook's log-out
        // ends the session the cookies are, browser and all; Instagram has
        // nothing for a web session, so it lives on in the browser.
        let ends = match network {
            Network::Messenger => vec![
                "It logs out of Facebook with the cookies you gave,".to_string(),
                "which also logs out the browser you copied them from,".into(),
                "removes tuimeta's encrypted-chat device and deletes".into(),
                "what tuimeta keeps for Messenger on this computer.".into(),
            ],
            Network::Instagram => vec![
                "tuimeta disconnects and deletes what it keeps for".to_string(),
                "Instagram on this computer. The session itself ends".into(),
                "only once you log out in the browser you copied the".into(),
                "cookies from.".into(),
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
        self.open.as_ref().map(|o| Jump {
            chat_id: o.chat_id,
            message_id: o.selected,
        })
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
    }

    /// Leaves the chat open: you stop typing in it.
    fn leave_chat(&mut self) {
        self.set_typing(false);
        // A paste on its way was for these messages.
        self.pasting = false;
        if self.open.take().is_some() {
            self.images.clear();
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
        match targets.len() {
            0 => self.status = Some("Nothing to open in this message".into()),
            1 => self.open_target(targets.remove(0)),
            _ => {
                self.menu = Some(PickMenu {
                    action: MenuAction::Open,
                    targets,
                    selected: 0,
                })
            }
        }
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
                self.finding = Some(Finding::new(&name));
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

    /// Files open in their default app once downloaded; links in the browser.
    fn open_target(&mut self, target: Target) {
        match target {
            Target::Photo { message_id, file } => self.view_photo(message_id, file),
            Target::File(file) => {
                // The helper answers at once if the file is already downloaded.
                if self.opening.insert(file.id) {
                    self.meta.download(file.id);
                }
            }
            Target::Link(link) => self.open_link_outside(link),
            Target::Text(_) => {}
        }
    }

    /// Shows message `message_id`'s photo in the viewer; one that's no
    /// longer loaded opens in its app, as `o` in the viewer would.
    fn view_photo(&mut self, message_id: i64, file: MediaFile) {
        let view = self.open.as_ref().and_then(|open| {
            let msg = open.messages.get(&message_id)?;
            PhotoView::of(open.chat_id, message_id, msg)
        });
        match view {
            Some(view) => self.photo_view = Some(view),
            None => self.open_target(Target::File(file)),
        }
    }

    /// The photo viewer takes all keys while it's up.
    fn on_viewer_key(&mut self, key: KeyEvent) {
        let Some(view) = &self.photo_view else {
            return;
        };
        match key.code {
            // It closes first: the app comes up over tuimeta anyway, and a
            // warning about the file mustn't come up under the photo.
            KeyCode::Char('o') => {
                let file = view.file.clone();
                self.photo_view = None;
                self.open_target(Target::File(file));
            }
            KeyCode::Char('y') => self.copy_target(Target::File(view.file.clone())),
            KeyCode::Enter | KeyCode::Esc | KeyCode::Char('q') => self.photo_view = None,
            _ => {}
        }
    }

    /// Opens a link in the browser, asking first if its words say something
    /// other than where it goes.
    fn open_link_outside(&mut self, link: Link) {
        match link {
            Link {
                url,
                disguise: Some(shown),
            } => {
                // Browsers show other scripts' letters as such, so a look-alike
                // host can pass for a familiar one.
                let site = link_host(&url).map(|host| match host.is_ascii() {
                    true => host,
                    false => format!("{host} (has non-Latin letters)"),
                });
                let mut confirm = Confirm::new(
                    "Open this link?",
                    vec![
                        format!("The text says: {shown}"),
                        format!("Full address:  {url}"),
                    ],
                    Confirmed::OpenLink(url),
                );
                confirm.site = site;
                self.confirm = Some(confirm);
            }
            link => self.open_externally(&link.url),
        }
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
        self.confirm.is_some()
            || self.settings_menu.is_some()
            || self.delete_menu.is_some()
            || self.react_menu.is_some()
            || self.photo_view.is_some()
            || self.menu.is_some()
            || self.picker.is_some()
            || self.resizing.is_some()
            || self.prompt.is_some()
            || self.focus == Focus::Input
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
        let Some(msg) = open.cursor_id().and_then(|id| open.messages.get(&id)) else {
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
            _ => {
                self.menu = Some(PickMenu {
                    action: MenuAction::Copy,
                    targets,
                    selected: 0,
                })
            }
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
        match pasted.content {
            Ok(Paste::Files(paths)) => {
                // Copied files have absolute paths; anything else would be
                // looked for wherever tuimeta was started.
                let (paths, relative): (Vec<_>, Vec<_>) =
                    paths.into_iter().partition(|p| p.is_absolute());
                if !relative.is_empty() {
                    self.status = Some("Copied files without a full path were left out".into());
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
        self.toast = Some(Toast {
            title: title.into(),
            detail: detail.split_whitespace().collect::<Vec<_>>().join(" "),
            until: Instant::now() + TOAST_TIME,
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
                let last = SettingsMenu::THEMES + self.themes.list.len() - 1;
                menu.selected = menu.selected.saturating_add_signed(delta).min(last);
            }
        }
    }

    /// Enter or Space on the settings tab: picks the theme under the cursor,
    /// or turns the setting under it on or off. Saved at once.
    fn change_setting(&mut self) {
        let Some(menu) = self.settings_menu.as_mut() else {
            return;
        };
        let settings = &mut self.settings;
        match menu.selected {
            i if i >= SettingsMenu::THEMES => {
                let Some(theme) = self.themes.list.get(i - SettingsMenu::THEMES) else {
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
            SettingsMenu::AFTER_SEND => settings.normal_after_send = !settings.normal_after_send,
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
        if self.loading_chats.contains(&network) || self.loaded_chats.contains(&network) {
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
        let mute = !self.chats.muted(chat_id);
        self.meta.set_muted(chat_id, mute);
        self.chats.set_muted(chat_id, mute);
    }

    /// `H`: marks the selected chat so it's easy to find, or unmarks it.
    fn toggle_highlight(&mut self) {
        let Some(chat_id) = self.selected else {
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
        self.meta.close();
        // Nothing to wait for without a helper.
        if self.meta.is_detached() {
            self.exit = true;
        }
        self.quit_deadline = Some(Instant::now() + CLOSE_TIMEOUT);
    }
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
        let now = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map_or(0, |d| d.as_secs());
        let _ = process::Command::new("/usr/bin/xattr")
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
    use crate::meta::{ChatInfo, ChatKind, Message, Network};

    fn press(app: &mut App, code: KeyCode, modifiers: KeyModifiers) {
        app.on_key(KeyEvent::new(code, modifiers));
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
    fn the_chat_list_can_go_on_the_right_where_h_and_l_follow_it() {
        let dir = std::env::temp_dir().join(format!("tuimeta-side-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let images = Images::new(Picker::halfblocks(), unbounded_channel().0);
        let meta = Meta::detached(unbounded_channel().0);
        let mut app = crate::demo::demo_app(meta, images, &dir);
        app.focus = Focus::Chats;
        let none = KeyModifiers::NONE;
        let saved = || Settings::load(&settings::path(&dir)).unwrap();
        // The text left and right of the first pane's top right corner.
        let halves = |app: &mut App| {
            let top = screen(app).remove(0);
            let (left, right) = top.split_once('┐').unwrap();
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
            "{left}┐{right}"
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
                .all(|&id| app.chats.network(id) == Some(Network::Instagram))
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
            request: "Jo".into(),
            found: Ok(201),
        });
        assert_eq!(app.open.as_ref().unwrap().chat_id, 201);
    }

    #[test]
    fn a_chat_found_while_you_write_does_not_open_over_what_you_type() {
        let mut app = test_app("late-find");
        let chat_id = app.open.as_ref().unwrap().chat_id;
        app.focus = Focus::Input;
        app.composer.insert_str("see you at 5");
        app.finding = Some(Finding::new("Bob"));
        app.on_meta(MetaEvent::ChatFound {
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
        app.finding = Some(Finding::new("Bob"));
        press(&mut app, KeyCode::Esc, KeyModifiers::NONE);
        assert!(app.finding.is_none());
        assert!(app.focus == Focus::Messages, "only the lookup stopped");

        app.finding = Some(Finding::new("Bob"));
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
        let edge = |rows: &[String]| rows[0].chars().position(|c| c == '┐').unwrap();
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
        assert!(rows[0].contains("Photo"), "{rows:#?}");
        assert!(!rows.iter().any(|r| r.contains(&title)), "{rows:#?}");
        assert!(
            rows[rows.len() - 1].contains("o open in its app · y copy"),
            "{rows:#?}"
        );
    }
}
