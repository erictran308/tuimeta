//! The chat list, rebuilt from the helper's events.
//!
//! The helper gives every chat an `order` (its last activity), a network and
//! whether it's archived. The list shown is the chats of one tab (both
//! networks, one of them, or the archive), sorted by (order, chat id)
//! descending. On top of that, chats with unread messages come first. A `/`
//! search narrows the list to chats whose title matches.

use std::collections::{HashMap, HashSet};

use crate::images::Thumbnail;
use crate::messages::Sender;
use crate::meta::{self, ChatInfo, ChatKind, MediaKind, Network};
use crate::search;
use crate::service::Service;
use crate::text;

pub struct Chat {
    pub title: String,
    pub network: Network,
    /// Channel posts all come from the channel, so they show no sender name.
    /// Messenger, Instagram and WhatsApp chats have none, so it's always
    /// false.
    pub is_channel: bool,
    /// A one-on-one chat, where only the other person can be typing.
    pub is_private: bool,
    pub unread: i32,
    /// Your messages up to this id have been read: by the other person, or
    /// by anyone in a group.
    pub read_outbox: i64,
    /// You read the messages up to this id.
    read_inbox: i64,
    /// One-line summary of the last message, e.g. "You: see you at 5".
    pub preview: String,
    /// Its place in the list: its last activity, in milliseconds.
    order: i64,
    archived: bool,
    pub photo: Option<ChatPhoto>,
    /// Who is typing right now, in the order they started, and what
    /// they're doing, e.g. "typing".
    pub activity: Vec<(Sender, &'static str)>,
    /// The person or group the chat is with.
    pub peer: Option<Peer>,
    muted: bool,
    /// End-to-end encrypted chats (every WhatsApp chat, Messenger's
    /// encrypted ones), which show a 🔒.
    pub encrypted: bool,
    /// A message request you haven't accepted.
    pub request: bool,
    /// You can write in it.
    pub can_send: bool,
}

/// A list of chats: everything, one network's, or the archive.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Hash)]
pub enum List {
    #[default]
    Main,
    Archive,
    Network(Network),
}

/// A tab over the chat list.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Tab {
    pub list: List,
    pub name: String,
    /// Unread chats in it that aren't muted.
    pub unread: i32,
}

/// What the tab of every chat is called.
const ALL_CHATS: &str = "All";
/// And the archive's.
const ARCHIVE: &str = "Archive";

impl Chat {
    /// The chat is in a list.
    fn in_list(&self, list: List) -> bool {
        match list {
            List::Main => !self.archived,
            List::Archive => self.archived,
            List::Network(network) => !self.archived && self.network == network,
        }
    }
}

/// Whom a chat is with: a person, or a group.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub enum Peer {
    User(i64),
    Group,
}

/// A chat's photo, for its avatar in the list.
#[derive(Clone)]
pub struct ChatPhoto {
    pub file_id: i32,
    /// Where it is on disk, once downloaded.
    pub path: Option<String>,
    /// A blurry version shown until the photo is ready; the helper sends
    /// none, but `--demo` does.
    pub thumbnail: Option<Thumbnail>,
}

impl ChatPhoto {
    fn new(photo: &meta::PhotoRef) -> Self {
        Self {
            file_id: photo.file_id,
            path: None,
            thumbnail: None,
        }
    }
}

#[derive(Default)]
pub struct Chats {
    by_id: HashMap<i64, Chat>,
    sorted: Vec<i64>,
    dirty: bool,
    /// Your own user id on each network.
    my_ids: HashMap<Network, i64>,
    /// An unread chat that was opened. It stays with the unread chats after
    /// it's read, until another chat is opened, so it doesn't jump away while
    /// you read it.
    held: Option<i64>,
    /// Chats you highlighted with `H`. Saved in the settings file.
    highlighted: HashSet<i64>,
    /// Only chats whose title contains this, in any case, are listed.
    filter: String,
    /// Chats in the list shown, before the filter.
    total: usize,
    /// The list shown.
    shown: List,
    /// The @username of people who have one, by user id.
    usernames: HashMap<i64, String>,
    /// When people were last active, by user id.
    presence: HashMap<i64, Presence>,
}

/// When someone was last active, as far as the network tells. Times are
/// unix timestamps.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Presence {
    /// Active now, until this time unless the network says it again.
    Online(i32),
    Offline(i32),
}

/// How long someone counts as active after the network last said so.
const ACTIVE_FOR: i64 = 120;

impl Presence {
    /// From the helper's `active_at`, as of `now`.
    pub fn of(active_at: i64, now: i64) -> Self {
        let until = active_at + ACTIVE_FOR;
        if until > now {
            Presence::Online(until as i32)
        } else {
            Presence::Offline(active_at as i32)
        }
    }
}

/// What the title of a chat with one person says about them.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Seen {
    Person(Presence),
}

impl Chats {
    /// A chat is new or changed: the helper always sends all of it. What
    /// only this app keeps (who is typing, a downloaded photo) stays.
    pub fn upsert(&mut self, info: &ChatInfo) {
        let old = self.by_id.remove(&info.id);
        let photo = info.photo.as_ref().map(|p| {
            let mut photo = ChatPhoto::new(p);
            if let Some(old) = old.as_ref().and_then(|c| c.photo.as_ref())
                && old.file_id == photo.file_id
            {
                photo.path.clone_from(&old.path);
            }
            photo
        });
        let peer = match info.kind {
            ChatKind::Dm => info.user_id.map(Peer::User),
            _ => Some(Peer::Group),
        };
        let chat = Chat {
            title: one_line(&info.title),
            network: info.network,
            is_channel: false,
            is_private: info.kind == ChatKind::Dm,
            unread: info.unread,
            read_outbox: info.read_outbox,
            read_inbox: info.read_inbox,
            preview: info
                .last_message
                .as_deref()
                .map(preview)
                .unwrap_or_default(),
            order: info.order,
            archived: info.archived,
            photo,
            activity: old.map(|c| c.activity).unwrap_or_default(),
            peer,
            muted: info.muted,
            encrypted: info.encrypted,
            request: info.request,
            can_send: info.can_send,
        };
        self.by_id.insert(info.id, chat);
        self.dirty = true;
    }

    /// A chat left the list: deleted, or you left it.
    pub fn remove(&mut self, chat_id: i64) {
        if self.by_id.remove(&chat_id).is_some() {
            self.dirty = true;
        }
    }

    /// A message arrived: the chat's preview and place follow it at once,
    /// whether or not the helper sends the chat again.
    pub fn on_message(&mut self, message: &meta::Message) {
        let Some(chat) = self.by_id.get_mut(&message.chat_id) else {
            return;
        };
        let order = message.date.saturating_mul(1000);
        if order >= chat.order {
            chat.order = order;
            chat.preview = preview(message);
            self.dirty = true;
        }
    }

    /// Someone started or stopped typing. The helper sends the stop itself
    /// when the network doesn't.
    pub fn set_typing(&mut self, chat_id: i64, sender: Sender, typing: bool) {
        let Some(chat) = self.by_id.get_mut(&chat_id) else {
            return;
        };
        let at = chat.activity.iter().position(|(s, _)| *s == sender);
        match (at, typing) {
            (Some(i), true) => chat.activity[i] = (sender, "typing"),
            (Some(i), false) => {
                chat.activity.remove(i);
            }
            (None, true) => chat.activity.push((sender, "typing")),
            (None, false) => {}
        }
    }

    /// The message from `sender` arrived: they're no longer typing it.
    pub fn stop_typing(&mut self, chat_id: i64, sender: Sender) {
        self.set_typing(chat_id, sender, false);
    }

    pub fn set_unread(&mut self, chat_id: i64, unread: i32) {
        if let Some(chat) = self.by_id.get_mut(&chat_id) {
            chat.unread = unread;
            self.dirty = true;
        }
    }

    pub fn set_read_inbox(&mut self, chat_id: i64, message_id: i64) {
        if let Some(chat) = self.by_id.get_mut(&chat_id) {
            chat.read_inbox = chat.read_inbox.max(message_id);
        }
    }

    /// The last message you read in a chat.
    pub fn read_inbox(&self, chat_id: i64) -> i64 {
        self.by_id.get(&chat_id).map_or(0, |c| c.read_inbox)
    }

    pub fn set_read_outbox(&mut self, chat_id: i64, message_id: i64) {
        if let Some(chat) = self.by_id.get_mut(&chat_id) {
            chat.read_outbox = chat.read_outbox.max(message_id);
        }
    }

    /// Which of the theme's name colors the chat's badge has, when it has
    /// no photo: picked by its id, so it stays the same.
    pub fn accent(&self, chat_id: i64) -> usize {
        chat_id.rem_euclid(7) as usize
    }

    /// Call when a chat is opened, before it's marked as read.
    pub fn opened(&mut self, chat_id: i64) {
        self.held = Some(chat_id).filter(|id| self.by_id.get(id).is_some_and(|c| c.unread > 0));
        self.dirty = true;
    }

    /// Re-sorts after updates. Call once per batch, before reading `ids`.
    pub fn refresh(&mut self) {
        if !self.dirty {
            return;
        }
        let list = self.shown;
        self.sorted = self
            .by_id
            .iter()
            .filter(|(_, chat)| chat.in_list(list))
            .map(|(&id, _)| id)
            .collect();
        self.total = self.sorted.len();
        if !self.filter.is_empty() {
            let mut sorted = std::mem::take(&mut self.sorted);
            sorted.retain(|&id| {
                let title = self.title(id).unwrap_or_default();
                !search::find(title, &self.filter).is_empty()
            });
            self.sorted = sorted;
        }
        // Unread chats come first.
        let (by_id, held) = (&self.by_id, self.held);
        self.sorted.sort_unstable_by_key(|&id| {
            let chat = &by_id[&id];
            let unread = chat.unread > 0 || held == Some(id);
            std::cmp::Reverse((unread, chat.order, id))
        });
        self.dirty = false;
    }

    /// Lists only chats whose title contains `query`; empty lists them all.
    pub fn set_filter(&mut self, query: &str) {
        if self.filter != query {
            self.filter = query.to_string();
            self.dirty = true;
        }
    }

    pub fn filter(&self) -> &str {
        &self.filter
    }

    /// How many chats are in the list shown, filtered out or not.
    pub fn total(&self) -> usize {
        self.total
    }

    /// The list shown.
    pub fn shown(&self) -> List {
        self.shown
    }

    /// Shows another list. The `/` filter stays.
    pub fn show(&mut self, list: List) {
        if self.shown != list {
            self.shown = list;
            self.dirty = true;
        }
    }

    /// The tabs over the list: all chats, then each network with chats once
    /// there are two, then the archive once it has chats. With only one of
    /// them there are none.
    pub fn tabs(&self) -> Vec<Tab> {
        let unread = |list: List| {
            self.by_id
                .values()
                .filter(|c| c.in_list(list) && c.unread > 0 && !c.muted)
                .count() as i32
        };
        let tab = |list, name: &str| Tab {
            list,
            name: name.to_string(),
            unread: unread(list),
        };
        let mut tabs = vec![tab(List::Main, ALL_CHATS)];
        let networks: Vec<Network> = Network::ALL
            .into_iter()
            .filter(|&n| self.by_id.values().any(|c| c.network == n))
            .collect();
        if networks.len() > 1 {
            tabs.extend(networks.iter().map(|&n| tab(List::Network(n), n.name())));
        }
        if self.by_id.values().any(|c| c.archived) || self.shown == List::Archive {
            tabs.push(tab(List::Archive, ARCHIVE));
        }
        if tabs.len() == 1 { Vec::new() } else { tabs }
    }

    /// The list `step` tabs away from the one shown, round the end.
    pub fn next_list(&self, step: isize) -> List {
        let tabs = self.tabs();
        let Some(at) = tabs.iter().position(|t| t.list == self.shown) else {
            return List::Main;
        };
        let at = (at as isize + step).rem_euclid(tabs.len() as isize);
        tabs[at as usize].list
    }

    /// Unread unmuted chats in every list, for the window's title.
    pub fn unread_chats(&self) -> i32 {
        self.by_id
            .values()
            .filter(|c| c.unread > 0 && !c.muted)
            .count() as i32
    }

    pub fn set_my_id(&mut self, network: Network, id: i64) {
        self.my_ids.insert(network, id);
    }

    /// Forgets a network's chats and your id on it, once logged out.
    pub fn forget(&mut self, network: Network) {
        self.my_ids.remove(&network);
        self.by_id.retain(|_, c| c.network != network);
        if self.shown == List::Network(network) {
            self.shown = List::Main;
        }
        self.dirty = true;
    }

    /// The person is you, on either network.
    pub fn is_me(&self, user_id: i64) -> bool {
        self.my_ids.values().any(|&id| id == user_id)
    }

    pub fn is_highlighted(&self, chat_id: i64) -> bool {
        self.highlighted.contains(&chat_id)
    }

    /// Highlights the chat, or removes its highlight. Returns all highlighted
    /// chats, sorted, for saving.
    pub fn toggle_highlight(&mut self, chat_id: i64) -> Vec<i64> {
        if !self.highlighted.remove(&chat_id) {
            self.highlighted.insert(chat_id);
        }
        let mut ids: Vec<i64> = self.highlighted.iter().copied().collect();
        ids.sort_unstable();
        ids
    }

    pub fn set_highlighted(&mut self, chat_ids: &[i64]) {
        self.highlighted = chat_ids.iter().copied().collect();
    }

    /// A person's username, from the helper's `user` events.
    pub fn set_username(&mut self, user_id: i64, username: Option<&str>) {
        match username.map(one_line).filter(|u| !u.is_empty()) {
            Some(name) => self.usernames.insert(user_id, name),
            None => self.usernames.remove(&user_id),
        };
    }

    /// The username of the person a chat is with, without an @.
    pub fn username(&self, chat_id: i64) -> Option<&str> {
        self.user_username(self.person(chat_id)?)
    }

    /// The username of a person, whether or not you have a chat with them.
    pub fn user_username(&self, user_id: i64) -> Option<&str> {
        self.usernames.get(&user_id).map(String::as_str)
    }

    /// The chat's notifications are off.
    pub fn muted(&self, chat_id: i64) -> bool {
        self.by_id.get(&chat_id).is_some_and(|c| c.muted)
    }

    pub fn set_muted(&mut self, chat_id: i64, muted: bool) {
        if let Some(chat) = self.by_id.get_mut(&chat_id) {
            chat.muted = muted;
        }
    }

    /// When someone was last active, from the helper's `user` events.
    pub fn set_presence(&mut self, user_id: i64, presence: Option<Presence>) {
        match presence {
            Some(presence) => self.presence.insert(user_id, presence),
            None => self.presence.remove(&user_id),
        };
    }

    /// What to say about the person a one-on-one chat is with; `None` for
    /// groups, and when the network doesn't say.
    pub fn seen(&self, chat_id: i64) -> Option<Seen> {
        let user_id = self.person(chat_id)?;
        self.presence.get(&user_id).copied().map(Seen::Person)
    }

    /// The chat is end-to-end encrypted.
    pub fn is_encrypted(&self, chat_id: i64) -> bool {
        self.by_id.get(&chat_id).is_some_and(|c| c.encrypted)
    }

    /// The network a chat is on.
    pub fn network(&self, chat_id: i64) -> Option<Network> {
        self.by_id.get(&chat_id).map(|c| c.network)
    }

    /// The person a chat with one person is with.
    pub fn person(&self, chat_id: i64) -> Option<i64> {
        match self.by_id.get(&chat_id)?.peer? {
            Peer::User(id) => Some(id),
            Peer::Group => None,
        }
    }

    /// Chats that aren't archived whose name or username contains `query`,
    /// by their last activity, for the chat picker. Unread chats don't go
    /// first, unlike in the list. An empty query matches them all.
    pub fn matching(&self, query: &str) -> Vec<i64> {
        let query = query.trim();
        let username = query.strip_prefix('@').unwrap_or(query);
        let mut ids: Vec<i64> = self
            .by_id
            .iter()
            .filter(|(_, chat)| chat.in_list(List::Main))
            .map(|(&id, _)| id)
            .filter(|&id| {
                username.is_empty()
                    || !search::find(self.title(id).unwrap_or_default(), query).is_empty()
                    || self
                        .username(id)
                        .is_some_and(|name| !search::find(name, username).is_empty())
            })
            .collect();
        ids.sort_unstable_by_key(|&id| std::cmp::Reverse((self.by_id[&id].order, id)));
        ids
    }

    pub fn title(&self, chat_id: i64) -> Option<&str> {
        self.by_id.get(&chat_id).map(|c| c.title.as_str())
    }

    /// Chat ids in display order.
    pub fn ids(&self) -> &[i64] {
        &self.sorted
    }

    pub fn get(&self, chat_id: i64) -> Option<&Chat> {
        self.by_id.get(&chat_id)
    }
}

impl Chats {
    /// Adds a chat that didn't come from the helper, below the others, for
    /// `--demo` and tests.
    pub fn add_local(&mut self, id: i64, title: &str, photo: Option<ChatPhoto>) -> &mut Chat {
        let order = 1000 - self.by_id.len() as i64;
        self.dirty = true;
        let mut chat = Chat::local(title, order);
        chat.photo = photo;
        self.by_id.entry(id).insert_entry(chat).into_mut()
    }

    /// Moves a chat added with [`Chats::add_local`] to a network, or the
    /// archive.
    pub fn place_local(&mut self, chat_id: i64, network: Network, archived: bool) {
        if let Some(chat) = self.by_id.get_mut(&chat_id) {
            chat.network = network;
            chat.archived = archived;
            self.dirty = true;
        }
    }
}

impl Chat {
    /// A chat with nothing in it yet, at this order.
    fn local(title: &str, order: i64) -> Self {
        Chat {
            title: title.into(),
            network: Network::Messenger,
            is_channel: false,
            is_private: false,
            unread: 0,
            read_outbox: 0,
            read_inbox: 0,
            preview: String::new(),
            order,
            archived: false,
            photo: None,
            activity: Vec::new(),
            peer: None,
            muted: false,
            encrypted: false,
            request: false,
            can_send: true,
        }
    }
}

/// A name or title from someone else on one clean line, capped like a
/// preview: a chat title is drawn (and so re-measured) every frame, and a
/// sender can make it any length. Cut, it ends with `…`, so a name padded
/// with blank characters can't pass for one that ends where it seems to.
fn one_line(text: &str) -> String {
    let clean = text::clean(text);
    let line = clean.split_whitespace().collect::<Vec<_>>().join(" ");
    match text::first_chars(&line, PREVIEW_CHARS) {
        start if start.len() < line.len() => format!("{start}…"),
        start => start.to_string(),
    }
}

/// How much of the last message a chat list row keeps.
const PREVIEW_CHARS: usize = 300;

/// One line of a message for the chat list: "You: see you at 5".
fn preview(message: &meta::Message) -> String {
    let text = snippet(message);
    if message.outgoing && message.service.is_none() {
        format!("You: {text}")
    } else {
        text
    }
}

/// One line of a message for a list, without who sent it.
pub fn snippet(message: &meta::Message) -> String {
    let text = match Service::of(message.service.as_deref()) {
        Some(service) => format!("[{}]", service.label()),
        None => text::clean(&content_text(message)),
    };
    // Only the start fits in a list row, and it's drawn every frame.
    text::first_chars(&text, PREVIEW_CHARS).replace(['\n', '\t'], " ")
}

/// What a message's media is called in a list: `[Photo]`, `[File: a.pdf]`.
pub fn media_label(media: &meta::Media) -> String {
    let view_once = |what: &str| format!("View-once {what}");
    match media.kind {
        MediaKind::Photo if media.view_once => view_once("photo"),
        MediaKind::Video if media.view_once => view_once("video"),
        MediaKind::Photo => "Photo".into(),
        MediaKind::Video => "Video".into(),
        MediaKind::Gif => "GIF".into(),
        MediaKind::Sticker => "Sticker".into(),
        MediaKind::Audio => "Audio".into(),
        MediaKind::Voice => "Voice message".into(),
        MediaKind::File => match media.name.as_deref().map(one_line) {
            Some(name) if !name.is_empty() => format!("File: {name}"),
            _ => "File".into(),
        },
        MediaKind::Unknown => "Attachment".into(),
    }
}

/// Plain-text rendering of a message body; media becomes a `[Label]`, and
/// what can't be shown its own label.
pub fn content_text(message: &meta::Message) -> String {
    let label = match (&message.media, &message.unsupported) {
        (Some(media), _) => Some(media_label(media)),
        (None, Some(what)) => Some(one_line(what).trim_matches(['[', ']']).to_string()),
        (None, None) => None,
    };
    match label {
        Some(label) if message.text.is_empty() => format!("[{label}]"),
        Some(label) => format!("[{label}] {}", message.text),
        None => message.text.clone(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Chats with the given (id, order, unread), as the helper would send
    /// them.
    fn chats(list: &[(i64, i64, i32)]) -> Chats {
        let mut chats = Chats::default();
        for &(id, order, unread) in list {
            let mut chat = Chat::local(&format!("chat {id}"), order);
            chat.unread = unread;
            chats.by_id.insert(id, chat);
        }
        chats.dirty = true;
        chats.refresh();
        chats
    }

    fn info(id: i64, network: Network) -> ChatInfo {
        ChatInfo {
            id,
            network,
            kind: ChatKind::Dm,
            title: "Alice\u{202E}  Example".into(),
            user_id: Some(34),
            photo: Some(meta::PhotoRef {
                file_id: 5,
                width: 160,
                height: 160,
            }),
            order: 1_759_912_345_678,
            unread: 2,
            muted: false,
            archived: false,
            encrypted: true,
            request: false,
            can_send: true,
            read_inbox: 0,
            read_outbox: 0,
            last_message: None,
        }
    }

    fn message(chat_id: i64, date: i64, text: &str) -> meta::Message {
        serde_json::from_value(serde_json::json!({
            "id": date << 18, "chat_id": chat_id, "date": date, "text": text, "outgoing": true,
        }))
        .unwrap()
    }

    #[test]
    fn a_name_cut_short_ends_with_an_ellipsis() {
        // A title pushed on far past what a row shows, to hide its end.
        let padded = format!("Bank support{}evil", "\u{2800}".repeat(400));
        let cut = one_line(&padded);
        assert!(cut.starts_with("Bank support"), "{cut}");
        assert!(cut.ends_with('…'), "{cut}");
        assert_eq!(cut.chars().count(), PREVIEW_CHARS + 1);
        let long = "a".repeat(PREVIEW_CHARS + 1);
        assert_eq!(one_line(&long), format!("{}…", &long[..PREVIEW_CHARS]));
        let fits = "a".repeat(PREVIEW_CHARS);
        assert_eq!(one_line(&fits), fits, "nothing cut, nothing added");
        assert_eq!(one_line("  Jo\n  Smith "), "Jo Smith");
    }

    #[test]
    fn a_chat_from_the_helper_replaces_the_old_but_keeps_its_downloaded_photo() {
        let mut list = Chats::default();
        list.upsert(&info(12, Network::Messenger));
        if let Some(photo) = list.by_id.get_mut(&12).and_then(|c| c.photo.as_mut()) {
            photo.path = Some("/files/5.jpg".into());
        }
        list.set_typing(12, Sender::User(34), true);
        let mut changed = info(12, Network::Messenger);
        changed.unread = 0;
        list.upsert(&changed);
        let chat = list.get(12).unwrap();
        assert_eq!(chat.title, "Alice Example", "cleaned onto one line");
        assert_eq!(chat.unread, 0);
        assert_eq!(
            chat.photo.as_ref().unwrap().path.as_deref(),
            Some("/files/5.jpg")
        );
        assert_eq!(chat.activity.len(), 1, "still typing");
        assert!(list.is_encrypted(12));
        assert_eq!(list.person(12), Some(34));
    }

    #[test]
    fn a_new_message_moves_its_chat_up_and_becomes_its_preview() {
        let mut list = chats(&[(1, 50_000, 0), (2, 40_000, 0)]);
        list.on_message(&message(2, 60, "see you at 5"));
        list.refresh();
        assert_eq!(list.ids(), [2, 1]);
        assert_eq!(list.get(2).unwrap().preview, "You: see you at 5");

        list.on_message(&message(2, 10, "an old one"));
        assert_eq!(
            list.get(2).unwrap().preview,
            "You: see you at 5",
            "an older message loading doesn't"
        );
    }

    #[test]
    fn typing_lasts_until_the_helper_says_it_stopped() {
        let mut list = chats(&[(1, 50, 0)]);
        let activity = |list: &Chats| list.get(1).unwrap().activity.clone();
        list.set_typing(1, Sender::User(7), true);
        list.set_typing(1, Sender::User(8), true);
        list.set_typing(1, Sender::User(7), true);
        assert_eq!(
            activity(&list),
            [(Sender::User(7), "typing"), (Sender::User(8), "typing")]
        );
        list.stop_typing(1, Sender::User(7));
        assert_eq!(activity(&list), [(Sender::User(8), "typing")]);
    }

    #[test]
    fn unread_chats_come_first_then_by_last_activity() {
        let mut list = chats(&[(1, 50, 0), (2, 40, 3), (3, 30, 0), (4, 20, 1)]);
        assert_eq!(list.ids(), [2, 4, 1, 3]);

        list.set_unread(3, 2);
        list.refresh();
        assert_eq!(list.ids(), [2, 3, 4, 1], "a new message moves it up");
    }

    #[test]
    fn an_opened_chat_keeps_its_place_until_another_is_opened() {
        let mut list = chats(&[(1, 50, 0), (2, 40, 3), (3, 30, 1)]);
        list.opened(3);
        list.set_unread(3, 0); // read while open
        list.refresh();
        assert_eq!(list.ids(), [2, 3, 1], "stays with the unread chats");

        list.opened(2);
        list.refresh();
        assert_eq!(list.ids(), [2, 1, 3], "drops to its place once you move on");
    }

    #[test]
    fn the_filter_keeps_matching_titles_in_order() {
        let mut list = chats(&[(1, 50, 0), (2, 40, 3), (3, 30, 0)]);
        for (id, title) in [(1, "Alice"), (2, "Bob"), (3, "alina")] {
            list.by_id.get_mut(&id).unwrap().title = title.into();
        }

        list.set_filter("ALI");
        list.refresh();
        assert_eq!(list.ids(), [1, 3]);
        assert_eq!(list.total(), 3);

        list.set_filter("");
        list.refresh();
        assert_eq!(list.ids(), [2, 1, 3]);
    }

    #[test]
    fn tabs_split_the_networks_once_there_are_two_and_the_archive_comes_last() {
        let mut list = chats(&[(1, 50, 0), (2, 40, 1), (3, 30, 0)]);
        assert!(list.tabs().is_empty(), "one network, no archive: no tabs");

        list.place_local(2, Network::Instagram, false);
        list.place_local(3, Network::Messenger, true);
        let tabs = list.tabs();
        let names: Vec<&str> = tabs.iter().map(|t| t.name.as_str()).collect();
        assert_eq!(names, ["All", "Messenger", "Instagram", "Archive"]);
        assert_eq!(tabs[2].unread, 1);

        list.show(List::Network(Network::Instagram));
        list.refresh();
        assert_eq!(list.ids(), [2]);
        list.show(List::Archive);
        list.refresh();
        assert_eq!(list.ids(), [3]);
        assert_eq!(list.next_list(1), List::Main, "round the end");

        list.forget(Network::Instagram);
        list.show(List::Network(Network::Instagram));
        list.forget(Network::Instagram);
        assert_eq!(
            list.shown(),
            List::Main,
            "a network logged out of can't stay shown"
        );
    }

    #[test]
    fn highlights_toggle() {
        let mut list = chats(&[(1, 50, 0), (2, 40, 0)]);
        assert_eq!(list.toggle_highlight(2), [2]);
        assert_eq!(list.toggle_highlight(1), [1, 2]);
        assert!(list.is_highlighted(2));
        assert_eq!(list.toggle_highlight(2), [1]);
        assert!(!list.is_highlighted(2));
    }

    #[test]
    fn media_and_what_cant_be_shown_get_labels_with_the_text_after() {
        let mut msg = message(1, 1, "look");
        msg.media = Some(
            serde_json::from_value(serde_json::json!({"kind": "file", "name": "a\nb.pdf"}))
                .unwrap(),
        );
        assert_eq!(content_text(&msg), "[File: a b.pdf] look");
        msg.media = Some(
            serde_json::from_value(serde_json::json!({"kind": "photo", "view_once": true}))
                .unwrap(),
        );
        msg.text.clear();
        assert_eq!(content_text(&msg), "[View-once photo]");
        msg.media = None;
        msg.unsupported = Some("[Poll]".into());
        assert_eq!(content_text(&msg), "[Poll]");
    }

    #[test]
    fn someone_active_within_two_minutes_counts_as_active() {
        assert_eq!(Presence::of(1000, 1060), Presence::Online(1120));
        assert_eq!(Presence::of(1000, 2000), Presence::Offline(1000));
    }
}
