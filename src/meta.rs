//! The connection to `tuimeta-helper`, the separate program that speaks
//! Messenger, Instagram and WhatsApp (see `helper/PROTOCOL.md`).
//!
//! The helper runs as a child process. Requests go to its stdin as JSON
//! lines; its answers and events come back on its stdout. Each request runs
//! as its own tokio task, so the UI never waits on the network, and what it
//! brings back reaches the app as a [`MetaEvent`], as TDLib's did in tuigram.

use std::collections::{HashMap, HashSet};
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use anyhow::{Context, Result, anyhow, bail};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use tokio::io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader};
use tokio::sync::mpsc::{UnboundedSender, unbounded_channel};
use tokio::sync::oneshot;

use crate::config;

/// The protocol version this build speaks; the helper says its own first.
const PROTOCOL_VERSION: u32 = 3;
/// How long the helper may take to say hello before it counts as broken.
const HELLO_TIMEOUT: Duration = Duration::from_secs(10);
/// The longest line read from the helper. A history page of photos is far
/// below it; anything longer is a broken helper, not a message.
const MAX_LINE: usize = 16 << 20;

/// One of Meta's networks. One account each.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash, PartialOrd, Ord, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Network {
    Messenger,
    Instagram,
    WhatsApp,
}

impl Network {
    pub const ALL: [Network; 3] = [Network::Messenger, Network::Instagram, Network::WhatsApp];

    pub fn name(self) -> &'static str {
        match self {
            Network::Messenger => "Messenger",
            Network::Instagram => "Instagram",
            Network::WhatsApp => "WhatsApp",
        }
    }

    /// The network logs in by linking tuimeta as a new device of the
    /// account, with a code the phone scans or takes, instead of cookies.
    pub fn links(self) -> bool {
        self == Network::WhatsApp
    }

    /// The cookies login needs, as the login screen asks for them; none
    /// for a network that [`links`](Self::links).
    pub fn cookie_names(self) -> &'static [&'static str] {
        match self {
            Network::Messenger => &["c_user", "xs", "datr"],
            Network::Instagram => &["sessionid", "ds_user_id", "csrftoken"],
            Network::WhatsApp => &[],
        }
    }

    /// Where the login comes from: the site to copy the cookies from, or
    /// for WhatsApp the app on the phone that links tuimeta.
    pub fn site(self) -> &'static str {
        match self {
            Network::Messenger => "facebook.com",
            Network::Instagram => "instagram.com",
            Network::WhatsApp => "WhatsApp on your phone",
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum AccountState {
    LoggedOut,
    Connecting,
    Ready,
    Error,
    #[serde(other)]
    Unknown,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ChatKind {
    Dm,
    Group,
    #[serde(other)]
    Other,
}

/// A picture the helper can download: its file and size.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Deserialize)]
pub struct PhotoRef {
    pub file_id: i32,
    #[serde(default)]
    pub width: u32,
    #[serde(default)]
    pub height: u32,
}

#[derive(Clone, Debug, Deserialize)]
pub struct ChatInfo {
    pub id: i64,
    pub network: Network,
    pub kind: ChatKind,
    pub title: String,
    #[serde(default)]
    pub user_id: Option<i64>,
    #[serde(default)]
    pub photo: Option<PhotoRef>,
    #[serde(default)]
    pub order: i64,
    #[serde(default)]
    pub unread: i32,
    #[serde(default)]
    pub muted: bool,
    #[serde(default)]
    pub archived: bool,
    #[serde(default)]
    pub encrypted: bool,
    #[serde(default)]
    pub request: bool,
    #[serde(default = "yes")]
    pub can_send: bool,
    #[serde(default)]
    pub read_inbox: i64,
    #[serde(default)]
    pub read_outbox: i64,
    /// A last message that doesn't read leaves the chat without one,
    /// rather than the chat unread.
    #[serde(default, deserialize_with = "lenient")]
    pub last_message: Option<Box<Message>>,
}

fn yes() -> bool {
    true
}

/// A field that's `None` when it doesn't read, instead of failing what
/// holds it.
fn lenient<'de, D, T>(deserializer: D) -> Result<Option<T>, D::Error>
where
    D: serde::Deserializer<'de>,
    T: serde::de::DeserializeOwned,
{
    let value = Value::deserialize(deserializer)?;
    Ok(serde_json::from_value(value).ok())
}

#[derive(Clone, Debug, Deserialize)]
pub struct UserInfo {
    pub id: i64,
    pub network: Network,
    pub name: String,
    #[serde(default)]
    pub username: Option<String>,
    #[serde(default)]
    pub is_self: bool,
    /// Unix seconds when last seen active, if the network says.
    #[serde(default)]
    pub active_at: Option<i64>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum EntityKind {
    Bold,
    Italic,
    Strike,
    Code,
    Pre,
    Quote,
    Link,
    Mention,
    #[serde(other)]
    Unknown,
}

/// Formatting over part of a message's text, in UTF-16 code units as in
/// TDLib.
#[derive(Clone, Debug, Deserialize)]
pub struct Entity {
    pub offset: i32,
    pub length: i32,
    #[serde(rename = "type")]
    pub kind: EntityKind,
    #[serde(default)]
    pub url: Option<String>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum MediaKind {
    Photo,
    Video,
    Gif,
    Sticker,
    Audio,
    Voice,
    File,
    #[serde(other)]
    Unknown,
}

#[derive(Clone, Debug, Deserialize)]
pub struct Media {
    pub kind: MediaKind,
    /// The whole file; 0 when there's none to have (view once, still
    /// sending).
    #[serde(default)]
    pub file_id: i32,
    #[serde(default)]
    pub thumbnail: Option<PhotoRef>,
    #[serde(default)]
    pub width: u32,
    #[serde(default)]
    pub height: u32,
    #[serde(default)]
    pub duration: i32,
    #[serde(default)]
    pub name: Option<String>,
    #[serde(default)]
    pub mime: Option<String>,
    #[serde(default)]
    pub size: i64,
    #[serde(default)]
    pub view_once: bool,
}

#[derive(Clone, Debug, Deserialize)]
pub struct ReplyInfo {
    #[serde(default)]
    pub message_id: Option<i64>,
    #[serde(default)]
    pub sender_id: Option<i64>,
    #[serde(default)]
    pub text: Option<String>,
}

#[derive(Clone, Debug, PartialEq, Eq, Deserialize)]
pub struct ReactionInfo {
    pub emoji: String,
    pub count: i32,
    #[serde(default)]
    pub mine: bool,
}

#[derive(Clone, Debug, Deserialize)]
pub struct LinkPreview {
    pub url: String,
    #[serde(default)]
    pub title: String,
    #[serde(default)]
    pub description: String,
    #[serde(default)]
    pub image: Option<PhotoRef>,
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum MessageState {
    #[default]
    Sent,
    Pending,
    Failed,
}

#[derive(Clone, Debug, Deserialize)]
pub struct Message {
    pub id: i64,
    pub chat_id: i64,
    #[serde(default)]
    pub sender_id: i64,
    #[serde(default)]
    pub outgoing: bool,
    #[serde(default)]
    pub date: i64,
    #[serde(default)]
    pub text: String,
    #[serde(default)]
    pub entities: Vec<Entity>,
    #[serde(default)]
    pub album: i64,
    #[serde(default)]
    pub media: Option<Media>,
    #[serde(default)]
    pub reply_to: Option<ReplyInfo>,
    #[serde(default)]
    pub forwarded: bool,
    #[serde(default)]
    pub reactions: Vec<ReactionInfo>,
    #[serde(default)]
    pub edited: bool,
    /// Unix seconds; set on your own text messages while they can be edited.
    #[serde(default)]
    pub editable_until: Option<i64>,
    #[serde(default)]
    pub deletable: bool,
    #[serde(default)]
    pub state: MessageState,
    #[serde(default)]
    pub service: Option<String>,
    #[serde(default)]
    pub link_preview: Option<LinkPreview>,
    #[serde(default)]
    pub unsupported: Option<String>,
}

#[derive(Clone, Debug, Deserialize)]
pub struct FileInfo {
    pub id: i32,
    #[serde(default)]
    pub done: bool,
    #[serde(default)]
    pub path: Option<String>,
    #[serde(default)]
    pub error: Option<String>,
}

/// Someone or a group found by the `s` picker's search.
#[derive(Clone, Debug, PartialEq, Eq, Deserialize)]
pub struct Found {
    #[serde(default)]
    pub chat_id: Option<i64>,
    #[serde(default)]
    pub user_id: Option<i64>,
    pub title: String,
    #[serde(default)]
    pub username: Option<String>,
    pub kind: ChatKind,
}

/// Which part of a chat's history to fetch.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Page {
    /// The newest messages.
    Latest,
    /// Messages older than this one.
    Older(i64),
    /// This message and newer ones.
    Newer(i64),
    /// This message and the ones on both sides of it.
    Around(i64),
}

pub enum MetaEvent {
    /// The helper is up and speaks our protocol.
    Hello,
    /// An account's state changed: logged out, connecting, ready…
    Account {
        network: Network,
        state: AccountState,
        user_id: Option<i64>,
        name: Option<String>,
        /// What to do about it, when `state` is `Error`.
        error: Option<String>,
    },
    Chat(Box<ChatInfo>),
    ChatRemoved(i64),
    User(Box<UserInfo>),
    /// A new message, or a known one changed (edits, reactions, sending
    /// state): the whole message, which replaces the old one.
    Message(Box<Message>),
    /// A message you sent was accepted; it had the temporary id `old_id`.
    MessageSent {
        chat_id: i64,
        old_id: i64,
        message: Box<Message>,
    },
    MessageFailed {
        chat_id: i64,
        old_id: i64,
        error: String,
    },
    MessagesDeleted {
        chat_id: i64,
        message_ids: Vec<i64>,
    },
    /// You read up to `inbox` elsewhere; the other side read yours up to
    /// `outbox`.
    Read {
        chat_id: i64,
        inbox: Option<i64>,
        outbox: Option<i64>,
        unread: Option<i32>,
    },
    Typing {
        chat_id: i64,
        user_id: i64,
        typing: bool,
    },
    /// A request failed, or the helper has something to say.
    Error(String),
    /// A `load_chats` call finished; `all` once every chat is loaded. A
    /// failure was sent before as `Error`.
    ChatsLoaded {
        network: Option<Network>,
        all: bool,
    },
    /// A page of history, oldest first. `None` if the request failed.
    History {
        chat_id: i64,
        page: Page,
        messages: Option<Vec<Message>>,
    },
    /// The message `message_id` replies to; `None` if it couldn't be had.
    Replied {
        chat_id: i64,
        message_id: i64,
        replied: Option<Box<Message>>,
    },
    /// A download finished; `path` is `None` if it failed.
    Downloaded {
        file_id: i32,
        path: Option<String>,
    },
    /// Whom a search in the `s` picker found. Failures find nobody.
    ChatsFound {
        network: Network,
        query: String,
        found: Vec<Found>,
    },
    /// A chat looked up to open, by what was looked up; or why it can't be.
    ChatFound {
        request: String,
        found: Result<i64, String>,
    },
    /// The answer to a login, the `attempt`th the app asked for: `Err` says
    /// why it didn't work.
    LoggedIn {
        network: Network,
        attempt: u64,
        result: Result<(), String>,
    },
    /// While a link waits (`login_link`): the code to show, which replaces
    /// the one before. `qr` is drawn as a QR code for the phone to scan,
    /// `pairing` typed on the phone; `expires` is unix seconds.
    LoginCode {
        network: Network,
        /// The `login_link` it's for (0: the helper didn't say).
        attempt: u64,
        qr: Option<String>,
        pairing: Option<String>,
        expires: i64,
    },
    /// The helper stopped or broke the protocol; nothing more will come.
    Gone(String),
}

/// What the helper answered to one request.
type Answer = Result<Value, String>;

/// A handle on the helper. Cheap to clone; every clone talks to the same
/// process.
#[derive(Clone)]
pub struct Meta {
    inner: Arc<Inner>,
}

struct Inner {
    next_id: AtomicU64,
    /// Lines for the helper's stdin; `None` closes it. `None` itself when
    /// there's no helper (`--demo`, tests).
    out: Option<UnboundedSender<Option<String>>>,
    pending: Mutex<HashMap<u64, oneshot::Sender<Answer>>>,
    tx: UnboundedSender<MetaEvent>,
    /// Files asked for quietly (chat photos), whose failures aren't shown.
    quiet: Mutex<HashSet<i32>>,
    /// Every request made, by method and params, for tests to check what
    /// went out.
    #[cfg(test)]
    sent: Mutex<Vec<(String, Value)>>,
}

impl Meta {
    /// Starts the helper and checks that it speaks this protocol.
    pub async fn start(
        helper: &Path,
        data_dir: &Path,
        fake: bool,
        tx: UnboundedSender<MetaEvent>,
    ) -> Result<Self> {
        let mut command = tokio::process::Command::new(helper);
        command
            .arg("--data-dir")
            .arg(data_dir)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            // Its output would land on the screen; it logs to its own file.
            .stderr(Stdio::null())
            .kill_on_drop(true);
        if fake {
            command.arg("--fake");
        }
        let mut child = command
            .spawn()
            .with_context(|| format!("cannot start {}", config::shown(helper)))?;
        let stdin = child.stdin.take().context("no stdin for the helper")?;
        let stdout = child.stdout.take().context("no stdout from the helper")?;
        let mut lines = BufReader::new(stdout);

        // Its first line says which protocol it speaks.
        let mut first = Vec::new();
        let read = tokio::time::timeout(HELLO_TIMEOUT, lines.read_until(b'\n', &mut first))
            .await
            .map_err(|_| anyhow!("tuimeta-helper didn't answer"))?;
        if read? == 0 {
            bail!("tuimeta-helper stopped as it started; see helper.log in the data folder");
        }
        check_hello(&first)?;

        let (out_tx, out_rx) = unbounded_channel::<Option<String>>();
        let inner = Arc::new(Inner {
            next_id: AtomicU64::new(1),
            out: Some(out_tx),
            pending: Mutex::new(HashMap::new()),
            tx,
            quiet: Mutex::new(HashSet::new()),
            #[cfg(test)]
            sent: Mutex::new(Vec::new()),
        });
        tokio::spawn(write_lines(stdin, out_rx));
        tokio::spawn(read_lines(lines, Arc::downgrade(&inner), child));
        let _ = inner.tx.send(MetaEvent::Hello);
        Ok(Self { inner })
    }

    /// No helper at all, for `--demo` and tests: requests go nowhere and are
    /// never answered.
    pub fn detached(tx: UnboundedSender<MetaEvent>) -> Self {
        Self {
            inner: Arc::new(Inner {
                next_id: AtomicU64::new(1),
                out: None,
                pending: Mutex::new(HashMap::new()),
                tx,
                quiet: Mutex::new(HashSet::new()),
                #[cfg(test)]
                sent: Mutex::new(Vec::new()),
            }),
        }
    }

    /// The requests made so far, by method, with their params.
    #[cfg(test)]
    pub fn sent(&self) -> Vec<(String, Value)> {
        self.inner.sent.lock().unwrap().clone()
    }

    /// The methods of the requests made so far.
    #[cfg(test)]
    pub fn sent_methods(&self) -> Vec<String> {
        self.sent().into_iter().map(|(method, _)| method).collect()
    }

    /// Sends a request; the answer comes when the helper gives it. `Err` is
    /// what to tell the user.
    fn call(&self, method: &str, params: Value) -> impl Future<Output = Answer> + Send + 'static {
        #[cfg(test)]
        self.inner
            .sent
            .lock()
            .unwrap()
            .push((method.to_string(), params.clone()));
        let (answer_tx, answer_rx) = oneshot::channel();
        let sent = match &self.inner.out {
            Some(out) => {
                let id = self.inner.next_id.fetch_add(1, Ordering::Relaxed);
                self.inner.pending.lock().unwrap().insert(id, answer_tx);
                let line = json!({"id": id, "method": method, "params": params}).to_string();
                out.send(Some(line)).is_ok()
            }
            None => false,
        };
        async move {
            if !sent {
                return Err("tuimeta-helper isn't running".into());
            }
            answer_rx
                .await
                .unwrap_or_else(|_| Err("tuimeta-helper stopped".into()))
        }
    }

    /// Sends a request whose answer only matters if it's an error, which is
    /// shown.
    fn spawn(&self, method: &str, params: Value) {
        let answer = self.call(method, params);
        // Without a helper nothing will answer: nothing to wait for.
        if self.is_detached() {
            return;
        }
        let tx = self.inner.tx.clone();
        tokio::spawn(async move {
            if let Err(e) = answer.await {
                let _ = tx.send(MetaEvent::Error(e));
            }
        });
    }

    /// Sends a request whose answer turns into an event.
    fn then(
        &self,
        method: &str,
        params: Value,
        event: impl FnOnce(Answer) -> Option<MetaEvent> + Send + 'static,
    ) {
        let answer = self.call(method, params);
        if self.is_detached() {
            return;
        }
        let tx = self.inner.tx.clone();
        tokio::spawn(async move {
            if let Some(event) = event(answer.await) {
                let _ = tx.send(event);
            }
        });
    }

    /// Logs in with the cookies the user pasted, as the browser they came
    /// from (`None`: the one the helper's libraries say), answered by
    /// [`MetaEvent::LoggedIn`] for `attempt`.
    pub fn login_cookies(
        &self,
        network: Network,
        cookies: String,
        browser: Option<String>,
        attempt: u64,
    ) {
        self.then(
            "login_cookies",
            json!({"network": network, "cookies": cookies, "browser": browser}),
            move |answer| {
                Some(MetaEvent::LoggedIn {
                    network,
                    attempt,
                    result: answer.map(|_| ()),
                })
            },
        );
    }

    /// Links tuimeta as a new device of the account (WhatsApp): QR codes to
    /// scan come as [`MetaEvent::LoginCode`]s, or with the account's phone
    /// number, one code to type on the phone. Answered by
    /// [`MetaEvent::LoggedIn`] for `attempt` once linked, or not.
    pub fn login_link(&self, network: Network, phone: Option<String>, attempt: u64) {
        self.then(
            "login_link",
            json!({"network": network, "phone": phone, "attempt": attempt}),
            move |answer| {
                Some(MetaEvent::LoggedIn {
                    network,
                    attempt,
                    result: answer.map(|_| ()),
                })
            },
        );
    }

    /// Stops the link of `attempt` if it still waits, so no code shown for
    /// it works any more. The link itself then answers that it was
    /// cancelled, which nobody waits for by then. A newer attempt is never
    /// stopped by it, whichever request the helper handles first.
    pub fn cancel_login(&self, network: Network, attempt: u64) {
        self.then(
            "cancel_login",
            json!({"network": network, "attempt": attempt}),
            |_| None,
        );
    }

    /// Ends the session on the network and deletes what's kept for it.
    pub fn log_out(&self, network: Network) {
        self.spawn("logout", json!({"network": network}));
    }

    /// Asks for the next `limit` chats of a network, or of both. They arrive
    /// as [`MetaEvent::Chat`]s, then [`MetaEvent::ChatsLoaded`].
    pub fn load_chats(&self, network: Option<Network>, limit: i32) {
        let tx = self.inner.tx.clone();
        self.then(
            "load_chats",
            json!({"network": network, "limit": limit}),
            move |answer| {
                let all = match answer {
                    Ok(result) => !result["has_more"].as_bool().unwrap_or(false),
                    Err(e) => {
                        let _ = tx.send(MetaEvent::Error(e));
                        false
                    }
                };
                Some(MetaEvent::ChatsLoaded { network, all })
            },
        );
    }

    /// Fetches a page of history, up to `limit` messages.
    pub fn load_history(&self, chat_id: i64, page: Page, limit: i32) {
        let mut params = json!({"chat_id": chat_id, "limit": limit});
        match page {
            Page::Latest => {}
            Page::Older(id) => params["before"] = id.into(),
            // `after` leaves the message itself out, and a page that starts
            // at it is wanted.
            Page::Newer(id) => params["after"] = (id - 1).into(),
            Page::Around(id) => params["around"] = id.into(),
        }
        let tx = self.inner.tx.clone();
        self.then("history", params, move |answer| {
            let messages = match answer {
                // One message that doesn't read (a number out of range,
                // say) costs that message, not the page around it.
                Ok(mut result) => serde_json::from_value::<Vec<Value>>(result["messages"].take())
                    .ok()
                    .map(|list| {
                        list.into_iter()
                            .filter_map(|m| serde_json::from_value(m).ok())
                            .collect()
                    }),
                Err(e) => {
                    let _ = tx.send(MetaEvent::Error(e));
                    None
                }
            };
            Some(MetaEvent::History {
                chat_id,
                page,
                messages,
            })
        });
    }

    /// Fetches one message, the one a reply answers.
    pub fn get_replied_message(&self, chat_id: i64, message_id: i64, replied_id: i64) {
        self.then(
            "get_message",
            json!({"chat_id": chat_id, "message_id": replied_id}),
            move |answer| {
                let replied = answer
                    .ok()
                    .and_then(|mut result| serde_json::from_value(result["message"].take()).ok())
                    .map(Box::new);
                Some(MetaEvent::Replied {
                    chat_id,
                    message_id,
                    replied,
                })
            },
        );
    }

    /// Sends text as typed, answering `reply_to` if set. The message shows
    /// as pending at once, as a [`MetaEvent::Message`].
    pub fn send_text(&self, chat_id: i64, text: String, reply_to: Option<i64>) {
        self.spawn(
            "send_text",
            json!({"chat_id": chat_id, "text": text, "reply_to": reply_to}),
        );
    }

    /// Sends files, with the caption under the last.
    pub fn send_files(
        &self,
        chat_id: i64,
        paths: Vec<String>,
        caption: String,
        reply_to: Option<i64>,
    ) {
        let caption = (!caption.is_empty()).then_some(caption);
        self.spawn(
            "send_files",
            json!({"chat_id": chat_id, "paths": paths, "caption": caption, "reply_to": reply_to}),
        );
    }

    pub fn edit_text(&self, chat_id: i64, message_id: i64, text: String) {
        self.spawn(
            "edit_text",
            json!({"chat_id": chat_id, "message_id": message_id, "text": text}),
        );
    }

    /// Unsends your message, for everyone.
    pub fn delete_message(&self, chat_id: i64, message_id: i64) {
        self.spawn(
            "delete",
            json!({"chat_id": chat_id, "message_id": message_id}),
        );
    }

    /// Sets your reaction to a message (you have one at most), or removes it.
    pub fn react(&self, chat_id: i64, message_id: i64, emoji: Option<&str>) {
        self.spawn(
            "react",
            json!({"chat_id": chat_id, "message_id": message_id, "emoji": emoji}),
        );
    }

    /// Marks a chat read up to this message, which tells the others you saw
    /// it. Only `App::mark_seen` calls it.
    pub fn mark_read(&self, chat_id: i64, message_id: i64) {
        self.spawn(
            "mark_read",
            json!({"chat_id": chat_id, "message_id": message_id}),
        );
    }

    /// Tells the chat you're typing, or that you stopped. Only
    /// `App::set_typing` calls it. Failing is harmless, so it isn't shown.
    pub fn send_typing(&self, chat_id: i64, typing: bool) {
        self.then(
            "typing",
            json!({"chat_id": chat_id, "typing": typing}),
            |_| None,
        );
    }

    /// Downloads a file; [`MetaEvent::Downloaded`] says when it's there.
    pub fn download(&self, file_id: i32) {
        self.inner.quiet.lock().unwrap().remove(&file_id);
        self.fetch_file(file_id, "high");
    }

    /// Like [`download`](Self::download), for files nobody asked for (chat
    /// photos): they wait behind other downloads, and a failure isn't shown.
    pub fn download_quiet(&self, file_id: i32) {
        self.inner.quiet.lock().unwrap().insert(file_id);
        self.fetch_file(file_id, "low");
    }

    fn fetch_file(&self, file_id: i32, priority: &str) {
        let tx = self.inner.tx.clone();
        let quiet = Arc::clone(&self.inner);
        self.then(
            "download",
            json!({"file_id": file_id, "priority": priority}),
            // Progress comes as `file` events; only a refusal ends it here.
            move |answer| {
                let error = answer.err()?;
                if !quiet.quiet.lock().unwrap().contains(&file_id) {
                    let _ = tx.send(MetaEvent::Error(error));
                }
                Some(MetaEvent::Downloaded {
                    file_id,
                    path: None,
                })
            },
        );
    }

    /// Searches a network for people and groups, for the `s` picker.
    pub fn find_chats(&self, network: Network, query: String) {
        self.then(
            "search",
            json!({"network": network, "query": query}),
            move |answer| {
                let found = answer
                    .ok()
                    .and_then(|mut result| serde_json::from_value(result["results"].take()).ok())
                    .unwrap_or_default();
                Some(MetaEvent::ChatsFound {
                    network,
                    query,
                    found,
                })
            },
        );
    }

    /// Looks up the chat with a person, made if needed, answered by
    /// [`MetaEvent::ChatFound`] for `request`.
    pub fn open_dm(&self, network: Network, user_id: i64, request: String) {
        self.then(
            "open_dm",
            json!({"network": network, "user_id": user_id}),
            move |answer| {
                let found = answer.and_then(|result| {
                    result["chat_id"]
                        .as_i64()
                        .ok_or_else(|| "The helper gave no chat.".to_string())
                });
                Some(MetaEvent::ChatFound { request, found })
            },
        );
    }

    pub fn set_muted(&self, chat_id: i64, muted: bool) {
        self.spawn("mute", json!({"chat_id": chat_id, "muted": muted}));
    }

    /// Closes the helper's stdin, after which it disconnects quietly and
    /// exits; [`MetaEvent::Gone`] follows.
    pub fn close(&self) {
        if let Some(out) = &self.inner.out {
            let _ = out.send(None);
        }
    }

    /// The helper isn't there: `--demo`, tests, or it stopped.
    pub fn is_detached(&self) -> bool {
        self.inner.out.is_none()
    }
}

/// Where the helper is: next to tuimeta, or in development builds where
/// `TM_HELPER` says.
pub fn helper_path() -> Result<PathBuf> {
    if cfg!(debug_assertions)
        && let Some(path) = config::var("TM_HELPER")
    {
        return Ok(PathBuf::from(path));
    }
    let name = if cfg!(windows) {
        "tuimeta-helper.exe"
    } else {
        "tuimeta-helper"
    };
    let exe = std::env::current_exe().context("cannot tell where tuimeta is")?;
    let path = exe
        .parent()
        .context("cannot tell where tuimeta is")?
        .join(name);
    if !path.is_file() {
        bail!(
            "{name} isn't next to tuimeta ({}). It speaks Messenger, Instagram and \
             WhatsApp for it; build it with `go build -o ../target/debug/{name} .` in helper/",
            config::shown(&path)
        );
    }
    Ok(path)
}

fn check_hello(line: &[u8]) -> Result<()> {
    #[derive(Deserialize)]
    struct Hello {
        event: String,
        version: u32,
    }
    let hello: Hello =
        serde_json::from_slice(line).context("tuimeta-helper said something unexpected")?;
    if hello.event != "hello" {
        bail!("tuimeta-helper said something unexpected");
    }
    if hello.version != PROTOCOL_VERSION {
        bail!(
            "tuimeta-helper speaks protocol {}, and this tuimeta {PROTOCOL_VERSION}: \
             install the two from the same release",
            hello.version
        );
    }
    Ok(())
}

async fn write_lines(
    mut stdin: tokio::process::ChildStdin,
    mut lines: tokio::sync::mpsc::UnboundedReceiver<Option<String>>,
) {
    while let Some(Some(mut line)) = lines.recv().await {
        line.push('\n');
        if stdin.write_all(line.as_bytes()).await.is_err() {
            break;
        }
    }
    // Dropping stdin tells the helper to finish.
}

async fn read_lines(
    mut lines: BufReader<tokio::process::ChildStdout>,
    inner: std::sync::Weak<Inner>,
    mut child: tokio::process::Child,
) {
    let mut line = Vec::new();
    let why = loop {
        line.clear();
        match AsyncReadExt::take(&mut lines, MAX_LINE as u64 + 1)
            .read_until(b'\n', &mut line)
            .await
        {
            Ok(0) => break "tuimeta-helper stopped".to_string(),
            Ok(_) if line.len() > MAX_LINE => break "tuimeta-helper sent too much".to_string(),
            Ok(_) => {}
            Err(_) => break "tuimeta-helper stopped".to_string(),
        }
        let Some(inner) = inner.upgrade() else {
            return;
        };
        if let Err(e) = on_line(&inner, &line) {
            break e;
        }
    };
    let _ = child.wait().await;
    if let Some(inner) = inner.upgrade() {
        // Whatever was still waiting gets its error now.
        inner.pending.lock().unwrap().clear();
        let _ = inner.tx.send(MetaEvent::Gone(why));
    }
}

/// Handles one line from the helper. `Err` means it broke the protocol.
fn on_line(inner: &Inner, line: &[u8]) -> Result<(), String> {
    let mut value: Value = serde_json::from_slice(line)
        .map_err(|_| "tuimeta-helper said something unexpected".to_string())?;
    if let Some(id) = value.get("id").and_then(Value::as_u64) {
        let answer = match value.get_mut("error") {
            Some(error) if !error.is_null() => Err(error["message"]
                .as_str()
                .unwrap_or("Something went wrong.")
                .to_string()),
            _ => Ok(value
                .get_mut("result")
                .map(Value::take)
                .unwrap_or(Value::Null)),
        };
        if let Some(waiting) = inner.pending.lock().unwrap().remove(&id) {
            let _ = waiting.send(answer);
        }
        return Ok(());
    }
    if let Some(event) = event(value, inner) {
        let _ = inner.tx.send(event);
    }
    Ok(())
}

/// The event a line from the helper is, or `None` for one that isn't known
/// or can't be read: a newer helper may say more than this build knows.
fn event(mut value: Value, inner: &Inner) -> Option<MetaEvent> {
    fn take<T: serde::de::DeserializeOwned>(value: &mut Value, field: &str) -> Option<T> {
        serde_json::from_value(value.get_mut(field)?.take()).ok()
    }
    let kind = value.get("event")?.as_str()?.to_string();
    Some(match kind.as_str() {
        "account" => MetaEvent::Account {
            network: take(&mut value, "network")?,
            state: take(&mut value, "state")?,
            user_id: take(&mut value, "user_id"),
            name: take(&mut value, "name"),
            error: take(&mut value, "error"),
        },
        "chat" => MetaEvent::Chat(take(&mut value, "chat")?),
        "chat_removed" => MetaEvent::ChatRemoved(take(&mut value, "chat_id")?),
        "user" => MetaEvent::User(take(&mut value, "user")?),
        "message" => MetaEvent::Message(take(&mut value, "message")?),
        "message_sent" => MetaEvent::MessageSent {
            chat_id: take(&mut value, "chat_id")?,
            old_id: take(&mut value, "old_id")?,
            message: take(&mut value, "message")?,
        },
        "message_failed" => MetaEvent::MessageFailed {
            chat_id: take(&mut value, "chat_id")?,
            old_id: take(&mut value, "old_id")?,
            error: take(&mut value, "error").unwrap_or_default(),
        },
        "message_deleted" => MetaEvent::MessagesDeleted {
            chat_id: take(&mut value, "chat_id")?,
            message_ids: take(&mut value, "message_ids")?,
        },
        "read" => MetaEvent::Read {
            chat_id: take(&mut value, "chat_id")?,
            inbox: take(&mut value, "inbox"),
            outbox: take(&mut value, "outbox"),
            unread: take(&mut value, "unread"),
        },
        "typing" => MetaEvent::Typing {
            chat_id: take(&mut value, "chat_id")?,
            user_id: take(&mut value, "user_id")?,
            typing: take(&mut value, "typing")?,
        },
        "file" => {
            let file: FileInfo = take(&mut value, "file")?;
            if file.done && file.path.is_some() {
                MetaEvent::Downloaded {
                    file_id: file.id,
                    path: file.path,
                }
            } else {
                // Progress, which nothing shows yet, says nothing here.
                let error = file.error?;
                if !inner.quiet.lock().unwrap().contains(&file.id) {
                    let _ = inner.tx.send(MetaEvent::Error(error));
                }
                MetaEvent::Downloaded {
                    file_id: file.id,
                    path: None,
                }
            }
        }
        "error" => MetaEvent::Error(take(&mut value, "message")?),
        "login_code" => MetaEvent::LoginCode {
            network: take(&mut value, "network")?,
            attempt: take(&mut value, "attempt").unwrap_or_default(),
            qr: take(&mut value, "qr"),
            pairing: take(&mut value, "pairing"),
            expires: take(&mut value, "expires").unwrap_or_default(),
        },
        _ => return None,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Runs the real helper in `--fake` mode, built into `target/debug`
    /// (or where `TM_HELPER` says), through a whole session. Not run by
    /// default, since it needs the Go build:
    /// `(cd helper && go build -o ../target/debug/tuimeta-helper .)`, then
    /// `cargo test -- --ignored`.
    #[tokio::test]
    #[ignore]
    async fn the_fake_helper_speaks_this_protocol_from_login_to_quit() {
        let helper = std::env::var("TM_HELPER")
            .map(PathBuf::from)
            .unwrap_or_else(|_| {
                Path::new(env!("CARGO_MANIFEST_DIR")).join("target/debug/tuimeta-helper")
            });
        assert!(
            helper.is_file(),
            "build the helper first: {}",
            helper.display()
        );
        let dir = std::env::temp_dir().join(format!("tuimeta-fake-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        let dir = std::path::absolute(&dir).unwrap();
        let (tx, mut rx) = unbounded_channel();
        let meta = Meta::start(&helper, &dir, true, tx).await.unwrap();

        // The next event that `pick` takes, skipping the rest.
        async fn next<T>(
            rx: &mut tokio::sync::mpsc::UnboundedReceiver<MetaEvent>,
            mut pick: impl FnMut(MetaEvent) -> Option<T>,
        ) -> T {
            let wait = async {
                loop {
                    let event = rx.recv().await.expect("the helper went quiet");
                    if let Some(found) = pick(event) {
                        return found;
                    }
                }
            };
            tokio::time::timeout(Duration::from_secs(10), wait)
                .await
                .expect("timed out")
        }

        let state = next(&mut rx, |e| match e {
            MetaEvent::Account {
                network: Network::Messenger,
                state,
                ..
            } => Some(state),
            _ => None,
        })
        .await;
        assert_eq!(state, AccountState::LoggedOut);

        meta.login_cookies(
            Network::Messenger,
            "c_user=1; xs=2; datr=3".into(),
            Some("Safari 18.6".into()),
            1,
        );
        let refused = next(&mut rx, |e| match e {
            MetaEvent::LoggedIn { result, .. } => Some(result),
            _ => None,
        })
        .await;
        assert!(refused.is_err(), "only Chrome can be named");
        meta.login_cookies(Network::Messenger, "c_user=1; xs=2".into(), None, 2);
        let refused = next(&mut rx, |e| match e {
            MetaEvent::LoggedIn { result, .. } => Some(result),
            _ => None,
        })
        .await;
        assert!(refused.is_err(), "datr is missing");
        meta.login_cookies(
            Network::Messenger,
            "c_user=1; xs=2; datr=3".into(),
            Some("Chrome 150.0.7712.45".into()),
            3,
        );
        let me = next(&mut rx, |e| match e {
            MetaEvent::Account {
                network: Network::Messenger,
                state: AccountState::Ready,
                user_id,
                ..
            } => user_id,
            _ => None,
        })
        .await;

        meta.load_chats(Some(Network::Messenger), 50);
        let mut chats = Vec::new();
        next(&mut rx, |e| match e {
            MetaEvent::Chat(chat) => {
                chats.push(*chat);
                None
            }
            MetaEvent::ChatsLoaded { .. } => Some(()),
            _ => None,
        })
        .await;
        assert!(!chats.is_empty());
        assert!(chats.iter().all(|c| c.network == Network::Messenger));
        let dm = chats
            .iter()
            .find(|c| c.kind == ChatKind::Dm && !c.encrypted)
            .expect("a chat with one person");

        // A page of history, every message readable as a bubble.
        meta.load_history(dm.id, Page::Latest, 50);
        let page = next(&mut rx, |e| match e {
            MetaEvent::History { messages, .. } => Some(messages.expect("a page")),
            _ => None,
        })
        .await;
        assert!(!page.is_empty());
        assert!(page.windows(2).all(|w| w[0].id < w[1].id), "oldest first");
        let msgs: Vec<crate::messages::Msg> = page.iter().map(Into::into).collect();
        assert!(msgs.iter().any(|m| !m.text.is_empty()));
        let oldest = page[0].id;
        meta.load_history(dm.id, Page::Older(oldest), 50);
        let older = next(&mut rx, |e| match e {
            MetaEvent::History { messages, .. } => Some(messages.expect("a page")),
            _ => None,
        })
        .await;
        assert!(older.iter().all(|m| m.id < oldest));

        // Sending: pending at once, then sent, then they type and answer.
        meta.send_text(dm.id, "hello there".into(), None);
        let pending = next(&mut rx, |e| match e {
            MetaEvent::Message(m) if m.outgoing => Some(m),
            _ => None,
        })
        .await;
        assert_eq!(pending.state, MessageState::Pending);
        assert_eq!(pending.sender_id, me);
        let sent = next(&mut rx, |e| match e {
            MetaEvent::MessageSent {
                old_id, message, ..
            } => Some((old_id, message)),
            _ => None,
        })
        .await;
        assert_eq!(sent.0, pending.id);
        assert!(sent.1.id >= pending.id || sent.1.state == MessageState::Sent);
        next(&mut rx, |e| match e {
            MetaEvent::Typing { typing: true, .. } => Some(()),
            _ => None,
        })
        .await;
        let echo = next(&mut rx, |e| match e {
            MetaEvent::Message(m) if !m.outgoing => Some(m),
            _ => None,
        })
        .await;
        assert_eq!(echo.text, "echo: hello there");

        // A photo downloads to a file only the user can read.
        let photo = page
            .iter()
            .chain(&older)
            .find_map(|m| m.media.as_ref().filter(|m| m.file_id > 0))
            .map(|m| m.file_id)
            .expect("a photo or file");
        meta.download(photo);
        let path = next(&mut rx, |e| match e {
            MetaEvent::Downloaded { file_id, path } if file_id == photo => Some(path),
            _ => None,
        })
        .await
        .expect("downloaded");
        assert!(Path::new(&path).is_file());
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let mode = std::fs::metadata(&path).unwrap().permissions().mode();
            assert_eq!(mode & 0o077, 0, "{mode:o}");
        }

        // WhatsApp links a device instead: a code to type on the phone,
        // given up, then a QR code, which the fake takes as scanned.
        meta.login_link(Network::WhatsApp, Some("+1 555 010 0100".into()), 4);
        let pairing = next(&mut rx, |e| match e {
            MetaEvent::LoginCode {
                network: Network::WhatsApp,
                pairing,
                ..
            } => pairing,
            _ => None,
        })
        .await;
        assert_eq!(pairing, "FAKE-C0DE");
        meta.cancel_login(Network::WhatsApp, 4);
        let given_up = next(&mut rx, |e| match e {
            MetaEvent::LoggedIn {
                network: Network::WhatsApp,
                attempt: 4,
                result,
            } => Some(result),
            _ => None,
        })
        .await;
        assert!(given_up.is_err(), "cancelled");
        meta.login_link(Network::WhatsApp, None, 5);
        let qr = next(&mut rx, |e| match e {
            MetaEvent::LoginCode {
                network: Network::WhatsApp,
                qr,
                expires,
                ..
            } => qr.filter(|_| expires > 0),
            _ => None,
        })
        .await;
        assert!(qr.starts_with("2@fake"), "{qr}");
        // Linked: the account is ready and the link answers, in either
        // order.
        let (mut linked, mut ready) = (None, false);
        next(&mut rx, |e| {
            match e {
                MetaEvent::LoggedIn {
                    network: Network::WhatsApp,
                    attempt: 5,
                    result,
                } => linked = Some(result),
                MetaEvent::Account {
                    network: Network::WhatsApp,
                    state: AccountState::Ready,
                    ..
                } => ready = true,
                _ => {}
            }
            (linked.is_some() && ready).then_some(())
        })
        .await;
        assert_eq!(linked, Some(Ok(())));
        meta.load_chats(Some(Network::WhatsApp), 50);
        let mut chats = Vec::new();
        next(&mut rx, |e| match e {
            MetaEvent::Chat(chat) => {
                chats.push(*chat);
                None
            }
            MetaEvent::ChatsLoaded { .. } => Some(()),
            _ => None,
        })
        .await;
        assert!(!chats.is_empty());
        assert!(
            chats
                .iter()
                .all(|c| c.network == Network::WhatsApp && c.encrypted)
        );

        meta.close();
        next(&mut rx, |e| matches!(e, MetaEvent::Gone(_)).then_some(())).await;
        std::fs::remove_dir_all(&dir).unwrap();
    }

    fn inner() -> (Inner, tokio::sync::mpsc::UnboundedReceiver<MetaEvent>) {
        let (tx, rx) = unbounded_channel();
        let inner = Inner {
            next_id: AtomicU64::new(1),
            out: None,
            pending: Mutex::new(HashMap::new()),
            tx,
            quiet: Mutex::new(HashSet::new()),
            sent: Mutex::new(Vec::new()),
        };
        (inner, rx)
    }

    #[test]
    fn only_a_helper_speaking_this_protocol_is_used() {
        check_hello(br#"{"event":"hello","version":3,"helper":"0.1.0"}"#).unwrap();
        let newer = check_hello(br#"{"event":"hello","version":4}"#).unwrap_err();
        assert!(newer.to_string().contains("same release"), "{newer}");
        let older = check_hello(br#"{"event":"hello","version":2}"#).unwrap_err();
        assert!(older.to_string().contains("same release"), "{older}");
        assert!(check_hello(b"not json").is_err());
        assert!(check_hello(br#"{"event":"chat","version":1}"#).is_err());
    }

    #[test]
    fn an_answer_goes_to_the_request_that_asked() {
        let (inner, _rx) = inner();
        let (tx, mut rx) = oneshot::channel();
        inner.pending.lock().unwrap().insert(7, tx);
        on_line(&inner, br#"{"id":7,"result":{"message_id":5}}"#).unwrap();
        assert_eq!(rx.try_recv().unwrap().unwrap()["message_id"], 5);

        let (tx, mut rx) = oneshot::channel();
        inner.pending.lock().unwrap().insert(8, tx);
        on_line(
            &inner,
            br#"{"id":8,"error":{"code":"not_found","message":"No such chat."}}"#,
        )
        .unwrap();
        assert_eq!(rx.try_recv().unwrap().unwrap_err(), "No such chat.");
    }

    #[test]
    fn events_a_newer_helper_adds_are_skipped_and_broken_lines_stop_it() {
        let (inner, mut rx) = inner();
        on_line(&inner, br#"{"event":"something_new","x":1}"#).unwrap();
        assert!(rx.try_recv().is_err());
        assert!(on_line(&inner, b"{oops").is_err());
    }

    #[test]
    fn a_message_event_reads_every_field_the_protocol_has() {
        let (inner, mut rx) = inner();
        let line = r#"{"event":"message","message":{"id":449,"chat_id":12,"sender_id":34,
            "outgoing":false,"date":1759912345,"text":"see you","entities":[{"offset":0,
            "length":3,"type":"bold"},{"offset":4,"length":3,"type":"wobbly"}],"album":0,
            "media":{"kind":"photo","file_id":21,"thumbnail":{"file_id":22,"width":320,
            "height":240},"width":1280,"height":960},"reply_to":{"message_id":300,"text":"when?"},
            "reactions":[{"emoji":"❤️","count":2,"mine":true}],"edited":true,
            "editable_until":1759913245,"deletable":true,"state":"sent"}}"#;
        let line: Vec<u8> = line.bytes().filter(|&b| b != b'\n').collect();
        on_line(&inner, &line).unwrap();
        let Ok(MetaEvent::Message(message)) = rx.try_recv() else {
            panic!("no message");
        };
        assert_eq!(message.id, 449);
        assert_eq!(message.entities[0].kind, EntityKind::Bold);
        assert_eq!(message.entities[1].kind, EntityKind::Unknown);
        let media = message.media.unwrap();
        assert_eq!(media.kind, MediaKind::Photo);
        assert_eq!(media.thumbnail.unwrap().file_id, 22);
        assert_eq!(message.reply_to.unwrap().message_id, Some(300));
        assert_eq!(message.reactions[0].emoji, "❤️");
        assert!(message.edited);
    }

    #[test]
    fn a_login_code_reads_the_qr_or_the_pairing_code_and_whatsapp_is_a_network() {
        let (inner, mut rx) = inner();
        on_line(
            &inner,
            br#"{"event":"login_code","network":"whatsapp","qr":"2@abc,def","expires":1700000060}"#,
        )
        .unwrap();
        let Ok(MetaEvent::LoginCode {
            network,
            attempt,
            qr,
            pairing,
            expires,
        }) = rx.try_recv()
        else {
            panic!("no code");
        };
        assert_eq!(network, Network::WhatsApp);
        assert_eq!(attempt, 0, "a helper that doesn't say");
        assert_eq!(qr.as_deref(), Some("2@abc,def"));
        assert_eq!(pairing, None);
        assert_eq!(expires, 1700000060);

        on_line(
            &inner,
            br#"{"event":"login_code","network":"whatsapp","pairing":"ABCD-EFGH","expires":1}"#,
        )
        .unwrap();
        assert!(matches!(
            rx.try_recv(),
            Ok(MetaEvent::LoginCode { qr: None, pairing: Some(p), .. }) if p == "ABCD-EFGH"
        ));
        on_line(
            &inner,
            br#"{"event":"login_code","network":"myspace","qr":"2@x","expires":1}"#,
        )
        .unwrap();
        assert!(rx.try_recv().is_err(), "a network this build doesn't know");

        assert_eq!(Network::ALL.len(), 3);
        assert!(Network::WhatsApp.links() && !Network::Messenger.links());
        assert!(Network::WhatsApp.cookie_names().is_empty());
        assert_eq!(serde_json::to_value(Network::WhatsApp).unwrap(), "whatsapp");
    }

    #[test]
    fn a_link_asks_by_qr_or_by_number_and_can_be_given_up() {
        let meta = Meta::detached(unbounded_channel().0);
        meta.login_link(Network::WhatsApp, None, 1);
        meta.login_link(Network::WhatsApp, Some("+1 555 010 0100".into()), 2);
        meta.cancel_login(Network::WhatsApp, 2);
        let sent = meta.sent();
        assert_eq!(sent[0].0, "login_link");
        assert_eq!(sent[0].1["network"], "whatsapp");
        assert!(sent[0].1["phone"].is_null());
        assert_eq!(sent[1].1["phone"], "+1 555 010 0100");
        assert_eq!(
            sent[2],
            (
                "cancel_login".into(),
                json!({"network": "whatsapp", "attempt": 2})
            )
        );
    }

    #[test]
    fn a_finished_download_says_where_and_a_quiet_failure_says_nothing() {
        let (inner, mut rx) = inner();
        on_line(
            &inner,
            br#"{"event":"file","file":{"id":3,"done":true,"path":"/x/3.jpg"}}"#,
        )
        .unwrap();
        assert!(matches!(
            rx.try_recv(),
            Ok(MetaEvent::Downloaded {
                file_id: 3,
                path: Some(_)
            })
        ));
        on_line(
            &inner,
            br#"{"event":"file","file":{"id":4,"downloaded":10}}"#,
        )
        .unwrap();
        assert!(rx.try_recv().is_err(), "progress alone shows nothing");

        inner.quiet.lock().unwrap().insert(5);
        on_line(
            &inner,
            br#"{"event":"file","file":{"id":5,"error":"gone"}}"#,
        )
        .unwrap();
        assert!(matches!(
            rx.try_recv(),
            Ok(MetaEvent::Downloaded {
                file_id: 5,
                path: None
            })
        ));
        assert!(rx.try_recv().is_err(), "a chat photo's failure isn't shown");
    }
}
