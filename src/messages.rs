use std::collections::{BTreeMap, HashMap, HashSet};
use std::ops::Range;

use crate::attach::{Attachment, Dropped, size_label};
use crate::chats::media_label;
use crate::images::Thumbnail;
use crate::meta::{self, EntityKind, MediaKind, MessageState, Page};
use crate::reactions::{self, Reaction};
use crate::service::Service;
use crate::text;

const MAX_FOLLOWED: usize = 1000;
const MAX_LOADED: usize = 2 * MAX_FOLLOWED;

/// Who sent a message. On Meta's networks, always a person.
#[derive(Clone, Copy, PartialEq, Eq, Debug, Hash)]
pub enum Sender {
    User(i64),
}

#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum SendState {
    Sent,
    Pending,
    Failed,
}

impl From<MessageState> for SendState {
    fn from(state: MessageState) -> Self {
        match state {
            MessageState::Sent => SendState::Sent,
            MessageState::Pending => SendState::Pending,
            MessageState::Failed => SendState::Failed,
        }
    }
}

#[derive(Clone)]
pub struct Preview {
    pub file_id: i32,
    pub width: u32,
    pub height: u32,
    /// A blurry version shown until the picture is ready; only `--demo`
    /// has them.
    pub thumbnail: Option<Thumbnail>,
    pub sticker: bool,
}

/// The size a picture that doesn't say its own is drawn at, rather than as
/// one pixel: stickers are 512px a side, and anything else gets a square
/// big enough to take a photo's whole room.
const STICKER_SIZE: (u32, u32) = (512, 512);
const PICTURE_SIZE: (u32, u32) = (1024, 1024);

/// `size`, unless the helper didn't know a side (it sends 0 then).
fn known_or(size: (u32, u32), default: (u32, u32)) -> (u32, u32) {
    match size {
        (0, _) | (_, 0) => default,
        size => size,
    }
}

impl Preview {
    /// A picture the helper can download, `sized` as the media says when
    /// the picture itself doesn't.
    fn of(photo: &meta::PhotoRef, sized: (u32, u32)) -> Option<Self> {
        if photo.file_id <= 0 {
            return None;
        }
        let (width, height) = known_or((photo.width, photo.height), sized);
        Some(Self {
            file_id: photo.file_id,
            width: width.max(1),
            height: height.max(1),
            thumbnail: None,
            sticker: false,
        })
    }
}

pub struct Msg {
    pub sender: Sender,
    pub outgoing: bool,
    /// Unix timestamp.
    pub date: i32,
    /// Message text; with a preview, just the caption (plus a video's length).
    pub text: String,
    /// The text or caption as sent, without labels like "[File]". What `y` copies.
    pub source_text: String,
    pub preview: Option<Preview>,
    /// The file Enter opens: the full photo, the video, the document…
    pub file: Option<MediaFile>,
    /// A photo at its largest, the same file as `file`: Enter shows it in
    /// the viewer.
    pub photo: Option<Preview>,
    /// Web links in the text or caption, in order, without duplicates.
    pub links: Vec<Link>,
    /// Byte ranges of `text` that are links, to underline.
    pub link_ranges: Vec<Range<usize>>,
    /// Formatting the sender picked (bold, code…), by byte range of `text`,
    /// in order and not overlapping.
    pub styles: Vec<Styled>,
    /// It was forwarded from another chat.
    pub forwarded: bool,
    /// The network's preview of a link in the text.
    pub card: Option<Card>,
    pub state: SendState,
    /// Set when this message is a reply.
    pub reply_to: Option<ReplyTo>,
    /// What `e` can change.
    pub editable: Editable,
    /// Until when it can be edited, in unix seconds: the network allows a
    /// while after sending.
    pub editable_until: Option<i64>,
    /// You can unsend it.
    pub deletable: bool,
    /// The text has formatting (bold, links behind words…) that an edit,
    /// which sends plain text, would lose.
    pub formatted: bool,
    /// Changed after it was sent.
    pub edited: bool,
    /// Messages sent together as an album share this id; 0 for the rest.
    pub album: i64,
    /// Reactions people added, most added first.
    pub reactions: Vec<Reaction>,
    /// What happened in the chat, when it's a service message: drawn in the
    /// middle, like a date, not in a bubble.
    pub service: Option<Service>,
}

/// What `e` can change in a message.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Editable {
    /// No words to edit: a photo, a sticker, a file…
    No,
    /// A text message.
    Text,
}

/// A message being edited in the composer, set with `e`.
pub struct Editing {
    pub id: i64,
    /// What it said, for the bar over the composer.
    pub snippet: String,
    /// What the composer started with. Enter sends nothing if it's still
    /// that.
    pub original: String,
    pub editable: Editable,
    /// What the composer held before, put back once the edit is saved or
    /// cancelled.
    pub draft: String,
    pub reply: Option<Replied>,
    /// Files waiting to be sent, put back too: an edit can't add any.
    pub attachments: Vec<Attachment>,
}

impl Msg {
    /// The message on one line: its text, or else what it holds ("Photo").
    pub fn snippet(&self) -> String {
        let text = one_line(&self.text);
        match &self.file {
            _ if !text.is_empty() => text,
            Some(file) => file.label.clone(),
            None => "Message".into(),
        }
    }

    /// Takes what a new copy of the message shows. What only this app knows
    /// stays: nothing, for now, but replies keep their place.
    fn set_body(&mut self, new: Msg) {
        *self = new;
    }
}

/// How part of a message's text looks, from the formatting its sender picked.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct Format {
    pub bold: bool,
    pub italic: bool,
    pub strike: bool,
    /// Inline code or a code block.
    pub code: bool,
    /// A block quote.
    pub quote: bool,
}

/// A stretch of text with one [`Format`].
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Styled {
    pub range: Range<usize>,
    pub format: Format,
}

/// The kinds of formatting a [`Format`] has, for counting how many entities
/// of each cover a spot.
#[derive(Clone, Copy)]
enum Mark {
    Bold,
    Italic,
    Strike,
    Code,
    Quote,
}

const MARKS: usize = 5;

impl Mark {
    fn of(kind: EntityKind) -> Option<Self> {
        Some(match kind {
            EntityKind::Bold => Mark::Bold,
            EntityKind::Italic => Mark::Italic,
            EntityKind::Strike => Mark::Strike,
            EntityKind::Code | EntityKind::Pre => Mark::Code,
            EntityKind::Quote => Mark::Quote,
            _ => return None,
        })
    }
}

impl Format {
    /// The formatting where `open[mark]` entities of each kind are open.
    fn of(open: &[u32; MARKS]) -> Self {
        let on = |mark: Mark| open[mark as usize] > 0;
        Format {
            bold: on(Mark::Bold),
            italic: on(Mark::Italic),
            strike: on(Mark::Strike),
            code: on(Mark::Code),
            quote: on(Mark::Quote),
        }
    }
}

/// The formatting in a text, as byte ranges. Entities can nest (bold inside
/// italic) and overlap, so they're cut into stretches that each look one way.
fn styles(entities: &[meta::Entity], offsets: &Utf16Offsets) -> Vec<Styled> {
    // Where each entity starts (+1) and ends (-1).
    let mut edges = Vec::new();
    for entity in entities {
        let Some(mark) = Mark::of(entity.kind) else {
            continue;
        };
        let (start, end) = offsets.range(entity);
        if start < end {
            edges.push((start, true, mark));
            edges.push((end, false, mark));
        }
    }
    edges.sort_by_key(|&(at, _, _)| at);
    let mut open = [0u32; MARKS];
    let mut out: Vec<Styled> = Vec::new();
    let mut from = 0;
    for (at, starts, mark) in edges {
        if at > from {
            let format = Format::of(&open);
            if format != Format::default() {
                match out.last_mut() {
                    Some(last) if last.range.end == from && last.format == format => {
                        last.range.end = at;
                    }
                    _ => out.push(Styled {
                        range: from..at,
                        format,
                    }),
                }
            }
            from = at;
        }
        let count = &mut open[mark as usize];
        *count = if starts {
            count.saturating_add(1)
        } else {
            count.saturating_sub(1)
        };
    }
    out
}

/// What a link in a message leads to, as the network previews it under the
/// text: the page's title and the start of its description, beside a small
/// picture if it has one.
#[derive(Clone)]
pub struct Card {
    /// Where the link really goes, read from its address: the name a page
    /// gives itself could be anyone's. Always the host of one of the
    /// message's own links, which Enter opens.
    pub host: String,
    pub title: String,
    pub description: String,
    /// The page's picture.
    pub image: Option<Preview>,
}

impl Card {
    fn new(preview: &meta::LinkPreview) -> Option<Self> {
        let host = link_host(&web_url(&preview.url)?)?;
        let title = one_line(&preview.title);
        let description = one_line(&preview.description);
        if title.is_empty() && description.is_empty() {
            return None;
        }
        let image = preview
            .image
            .as_ref()
            .and_then(|p| Preview::of(p, PICTURE_SIZE));
        Some(Card {
            host: text::clean(&host),
            title,
            description,
            image,
        })
    }
}

/// The text on one line, at most [`SNIPPET_CHARS`] long: a quote or a popup
/// shows only its start, and a sender's 20000 characters would be measured
/// again on every frame. Cut, it ends with `…`, so a name padded with blank
/// characters can't pass for one that ends where it seems to.
pub fn one_line(text: &str) -> String {
    let line = text::clean(text)
        .split_whitespace()
        .collect::<Vec<_>>()
        .join(" ");
    match text::first_chars(&line, SNIPPET_CHARS) {
        start if start.len() < line.len() => format!("{start}…"),
        start => start.to_string(),
    }
}

/// How much of a message a one-line snippet keeps.
const SNIPPET_CHARS: usize = 300;

/// A web link in a message.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Link {
    pub url: String,
    /// The words the link hides behind, when they aren't the URL itself.
    /// Opening it asks first and shows where it really goes, since
    /// `https://bank.com` can lead anywhere.
    pub disguise: Option<String>,
}

impl From<&str> for Link {
    fn from(url: &str) -> Self {
        Link {
            url: url.into(),
            disguise: None,
        }
    }
}

/// What a reply answers.
pub struct ReplyTo {
    /// The answered message, when the network says which.
    pub message_id: Option<i64>,
    /// Who sent it and a line of what it said, when the network says.
    pub quoted: Option<Replied>,
}

/// A message being replied to: who sent it and a line of what it said. For
/// the composer, it's copied when `r` is pressed, so the reply bar still
/// shows it after another part of the history loads.
#[derive(Clone)]
pub struct Replied {
    pub id: i64,
    pub sender: Sender,
    pub outgoing: bool,
    pub snippet: String,
}

impl Replied {
    pub fn new(id: i64, msg: &Msg) -> Self {
        Self {
            id,
            sender: msg.sender,
            outgoing: msg.outgoing,
            snippet: msg.snippet(),
        }
    }
}

/// A message's downloadable file, with what to call it in the open menu.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct MediaFile {
    pub id: i32,
    pub label: String,
    /// A photo: copied as an image, not as a file.
    pub photo: bool,
}

/// Where media seen only once is: on the phone.
pub const ON_PHONE: &str = "open it on your phone";

/// What the bubble shows for a message's media: the preview, the file
/// Enter opens, the largest photo, and the label that goes before the
/// caption, if any.
struct Shown {
    preview: Option<Preview>,
    file: Option<MediaFile>,
    photo: Option<Preview>,
    label: Option<String>,
}

fn shown_media(media: &meta::Media) -> Shown {
    // WhatsApp's stickers often don't say their size.
    let default = match media.kind {
        MediaKind::Sticker => STICKER_SIZE,
        _ => PICTURE_SIZE,
    };
    let size = known_or((media.width, media.height), default);
    let thumbnail = media.thumbnail.as_ref().and_then(|t| Preview::of(t, size));
    let file = |label: String, photo: bool| {
        (media.file_id > 0).then_some(MediaFile {
            id: media.file_id,
            label,
            photo,
        })
    };
    let labeled = |label: String| Shown {
        preview: None,
        file: file(label.clone(), false),
        photo: None,
        label: Some(format!("[{label}]")),
    };
    // Seen only once: on the phone, where the sender meant it to be. It's
    // never offered as a file, which another app would keep.
    if media.view_once {
        let what = media_label(media);
        return Shown {
            preview: None,
            file: None,
            photo: None,
            label: Some(format!("[{what} · {ON_PHONE}]")),
        };
    }
    match media.kind {
        MediaKind::Photo => {
            let full = Preview::of(
                &meta::PhotoRef {
                    file_id: media.file_id,
                    width: media.width,
                    height: media.height,
                },
                size,
            );
            let preview = thumbnail.or_else(|| full.clone());
            match preview {
                Some(preview) => Shown {
                    preview: Some(preview),
                    file: file("Photo".into(), true),
                    photo: full,
                    label: None,
                },
                None => labeled("Photo".into()),
            }
        }
        MediaKind::Video | MediaKind::Gif => {
            let name = match media.kind {
                MediaKind::Gif => "GIF".to_string(),
                _ => format!("Video {}", duration(media.duration)),
            };
            match thumbnail {
                Some(preview) => Shown {
                    preview: Some(preview),
                    file: file(name, false),
                    photo: None,
                    label: (media.kind == MediaKind::Video)
                        .then(|| format!("▶ {}", duration(media.duration))),
                },
                None => labeled(name),
            }
        }
        MediaKind::Sticker => {
            let still = thumbnail.or_else(|| {
                let image = media
                    .mime
                    .as_deref()
                    .is_some_and(|m| m.starts_with("image/"));
                let whole = meta::PhotoRef {
                    file_id: media.file_id,
                    width: media.width,
                    height: media.height,
                };
                image.then(|| Preview::of(&whole, size)).flatten()
            });
            match still {
                Some(mut preview) => {
                    preview.sticker = true;
                    Shown {
                        preview: Some(preview),
                        file: file("Sticker".into(), false),
                        photo: None,
                        label: None,
                    }
                }
                None => labeled("Sticker".into()),
            }
        }
        MediaKind::Voice => labeled(format!("Voice message {}", duration(media.duration))),
        MediaKind::Audio | MediaKind::File | MediaKind::Unknown => {
            let label = media_label(media);
            let label = match media.kind {
                MediaKind::Audio => match media.name.as_deref().map(one_line) {
                    Some(name) if !name.is_empty() => format!("Audio: {name}"),
                    _ => label,
                },
                _ => label,
            };
            match media.size {
                size if size > 0 => labeled(format!("{label} · {}", size_label(size as u64))),
                _ => labeled(label),
            }
        }
    }
}

/// Tabs become spaces and what [`text::clean`] leaves out goes (hidden
/// characters, `\r` among them, and joiners that join nothing), so terminal
/// widths add up. Link and formatting ranges move along with the text.
fn normalize<'a>(text: &str, ranges: impl IntoIterator<Item = &'a mut Range<usize>>) -> String {
    if !text.contains(|c| c == '\t' || text::is_hidden(c) || text::is_joining(c)) {
        return text.to_string();
    }
    let mut out = String::with_capacity(text.len());
    // Old byte offset -> new byte offset.
    let mut map = vec![0; text.len() + 1];
    let mut keep = text::Keep::default();
    for (i, c) in text.char_indices() {
        map[i..i + c.len_utf8()].fill(out.len());
        match c {
            _ if !keep.keeps(c) => {}
            '\t' => out.push_str("    "),
            c => out.push(c),
        }
    }
    map[text.len()] = out.len();
    for range in ranges {
        *range = map[range.start]..map[range.end];
    }
    out
}

/// Web links in a text, with the byte range they cover: the network's link
/// entities (links hidden behind words among them), and addresses written
/// out in the text. Only http(s), so a crafted link can't get the OS to
/// open a local file or app.
fn links(
    text: &str,
    entities: &[meta::Entity],
    offsets: &Utf16Offsets,
) -> Vec<(Link, Range<usize>)> {
    let mut out: Vec<(Link, Range<usize>)> = Vec::new();
    for entity in entities.iter().filter(|e| e.kind == EntityKind::Link) {
        let (start, end) = offsets.range(entity);
        // A bad entity mustn't crash the app.
        let Some(shown) = text.get(start..end).filter(|s| !s.is_empty()) else {
            continue;
        };
        let url = entity.url.as_deref().unwrap_or(shown);
        if let Some(url) = web_url(url) {
            let disguise = (!same_place(shown, &url)).then(|| one_line(shown));
            out.push((Link { url, disguise }, start..end));
        }
    }
    // A written-out address inside a link entity is that link. Found by
    // the entities sorted by start, with the furthest end so far, so a
    // message with thousands of either takes no longer than its length.
    let mut taken: Vec<Range<usize>> = out.iter().map(|(_, r)| r.clone()).collect();
    taken.sort_by_key(|r| r.start);
    let reach: Vec<usize> = taken
        .iter()
        .scan(0, |end, r| {
            *end = r.end.max(*end);
            Some(*end)
        })
        .collect();
    for range in written_urls(text) {
        // The entities starting before this address ends, and whether one
        // of them runs past its start.
        let before = taken.partition_point(|r| r.start < range.end);
        if before > 0 && reach[before - 1] > range.start {
            continue;
        }
        if let Some(url) = web_url(&text[range.clone()]) {
            out.push((
                Link {
                    url,
                    disguise: None,
                },
                range,
            ));
        }
    }
    out.sort_by_key(|(_, r)| r.start);
    out
}

/// Where addresses are written out in a text: words starting with
/// `http://`, `https://` or `www.`, without the punctuation that ends a
/// sentence after them.
fn written_urls(text: &str) -> Vec<Range<usize>> {
    let mut out = Vec::new();
    let mut at = 0;
    for word in text.split_inclusive(char::is_whitespace) {
        let start = at;
        at += word.len();
        let word = word.trim_end();
        let lower = word.to_ascii_lowercase();
        let skip = word.len() - word.trim_start_matches(['(', '<', '"', '\'']).len();
        let lower = &lower[skip..];
        if !(lower.starts_with("https://")
            || lower.starts_with("http://")
            || lower.starts_with("www."))
        {
            continue;
        }
        let trimmed =
            word[skip..].trim_end_matches(['.', ',', '!', '?', ')', '>', '"', '\'', ':', ';']);
        if trimmed.len() > 4 {
            out.push(start + skip..start + skip + trimmed.len());
        }
    }
    out
}

/// Whether link text spells out the URL it leads to, give or take `www.`,
/// a trailing slash, the host's case and a scheme the text leaves out. A
/// path's case counts (`bit.ly/AbC` isn't `bit.ly/abc`), and so does a
/// scheme the text names: `https://` mustn't lead to `http://`.
pub fn same_place(shown: &str, url: &str) -> bool {
    let parts = |s: &str| {
        let s = s.trim();
        let lower = s.to_ascii_lowercase();
        let (scheme, rest) = match () {
            _ if lower.starts_with("https://") => (Some("https"), &s[8..]),
            _ if lower.starts_with("http://") => (Some("http"), &s[7..]),
            _ => (None, s),
        };
        let end = rest.find(['/', '?', '#']).unwrap_or(rest.len());
        let host = rest[..end].to_lowercase();
        let host = host.strip_prefix("www.").unwrap_or(&host);
        let path = rest[end..].trim_end_matches('/');
        (scheme, format!("{host}{path}"))
    };
    let (shown_scheme, shown) = parts(shown);
    let (url_scheme, url) = parts(url);
    shown == url && shown_scheme.is_none_or(|scheme| Some(scheme) == url_scheme)
}

/// Where a text's UTF-16 offsets are in its bytes: entity offsets count
/// UTF-16 code units, not bytes or chars. Worked out once per message, so
/// converting one takes time in step with its size, however many entities
/// a sender gives it.
struct Utf16Offsets {
    /// The byte offset of each UTF-16 offset.
    at: Vec<usize>,
    len: usize,
}

impl Utf16Offsets {
    fn new(text: &str) -> Self {
        let mut at = Vec::with_capacity(text.len() + 1);
        for (i, c) in text.char_indices() {
            at.push(i);
            // An offset inside a surrogate pair moves on to the next character.
            for _ in 1..c.len_utf16() {
                at.push(i + c.len_utf8());
            }
        }
        Self {
            at,
            len: text.len(),
        }
    }

    /// The byte offset of a UTF-16 offset, clamped to the text.
    fn byte(&self, utf16: i32) -> usize {
        self.at
            .get(utf16.max(0) as usize)
            .copied()
            .unwrap_or(self.len)
    }

    /// The bytes an entity covers, clamped to the text.
    fn range(&self, entity: &meta::Entity) -> (usize, usize) {
        let start = self.byte(entity.offset);
        let end = self.byte(entity.offset.saturating_add(entity.length));
        (start, end)
    }
}

/// More entities than any message written by hand has: the rest are left
/// out, so their text shows plain and a link among them isn't one.
const MAX_ENTITIES: usize = 2000;

/// `example.com/x` becomes `https://example.com/x`; other schemes are dropped.
/// A `\` before the query is written as the `/` browsers read it as.
pub fn web_url(url: &str) -> Option<String> {
    let url = text::clean(url);
    let url = url.trim();
    // A link with a line break in it would be copied as several lines, and
    // no web address has spaces.
    if url.contains(char::is_whitespace) {
        return None;
    }
    // Browsers read `https://bank.com\@evil.example` as going to bank.com,
    // like `link_host`, but the system's opener may not: macOS reads it as
    // going to evil.example. Written the browser way, every reader agrees,
    // and the host tuimeta shows is where it goes.
    let query = url.find(['?', '#']).unwrap_or(url.len());
    let url = format!("{}{}", url[..query].replace('\\', "/"), &url[query..]);
    let url = url.as_str();
    let lower = url.to_ascii_lowercase();
    if lower.starts_with("https://") || lower.starts_with("http://") {
        Some(url.to_string())
    } else if !url.is_empty() && !url.contains("://") && !url.contains(':') {
        Some(format!("https://{url}"))
    } else {
        None
    }
}

/// The host a web link really goes to, read the way browsers do: past the
/// scheme and any slashes, up to the first `/`, `\\`, `?` or `#`, after the
/// last `@` and without the port. Lowercased.
pub fn link_host(url: &str) -> Option<String> {
    link_parts(url).map(|(host, _)| host)
}

/// A web link's host, as [`link_host`] reads it, and what comes after it:
/// the path, query and fragment, as written.
pub fn link_parts(url: &str) -> Option<(String, &str)> {
    let lower = url.to_ascii_lowercase();
    let rest = lower
        .strip_prefix("https:")
        .or_else(|| lower.strip_prefix("http:"))?;
    let rest = rest.trim_start_matches(['/', '\\']);
    let end = rest.find(['/', '\\', '?', '#']).unwrap_or(rest.len());
    let authority = &rest[..end];
    let host = authority
        .rsplit_once('@')
        .map_or(authority, |(_, host)| host);
    let host = match host.find(']') {
        Some(end) if host.starts_with('[') => &host[..=end],
        _ => host.rsplit_once(':').map_or(host, |(host, _)| host),
    };
    // Lowercasing ASCII keeps every byte where it was.
    let after = &url[url.len() - (rest.len() - end)..];
    (!host.is_empty()).then(|| (host.to_string(), after))
}

/// `1:05`, or `1:02:05` past an hour.
pub fn duration(seconds: i32) -> String {
    let seconds = seconds.max(0);
    let (h, m, s) = (seconds / 3600, seconds / 60 % 60, seconds % 60);
    if h > 0 {
        format!("{h}:{m:02}:{s:02}")
    } else {
        format!("{m}:{s:02}")
    }
}

impl From<&meta::Message> for Msg {
    fn from(message: &meta::Message) -> Self {
        let sender = Sender::User(message.sender_id);
        let service = Service::of(message.service.as_deref());
        let shown = message.media.as_ref().map(shown_media);
        let source = text::clean(&message.text);
        // The caption goes under the media's label, if it has one.
        let (mut text, shift) = match shown.as_ref().and_then(|s| s.label.clone()) {
            Some(label) if message.text.is_empty() => (label, None),
            Some(label) => {
                let shift = label.len() + 1;
                (format!("{label}\n{}", message.text), Some(shift))
            }
            None => (message.text.clone(), Some(0)),
        };
        if shown.is_none()
            && let Some(what) = &message.unsupported
        {
            let what = format!("[{}]", one_line(what).trim_matches(['[', ']']));
            match text.is_empty() {
                true => text = what,
                false => text = format!("{what}\n{text}"),
            }
        }
        let entities = &message.entities[..message.entities.len().min(MAX_ENTITIES)];
        let offsets = Utf16Offsets::new(&message.text);
        let found = links(&message.text, entities, &offsets);
        let mut link_ranges = Vec::new();
        let mut style_ranges = Vec::new();
        // The ranges are of the text as sent, which shows under a media
        // label but not under a label for what can't be shown, nor in place
        // of a service message's sentence.
        if let Some(shift) = shift
            .filter(|_| (message.unsupported.is_none() || shown.is_some()) && service.is_none())
        {
            link_ranges = found
                .iter()
                .map(|(_, r)| r.start + shift..r.end + shift)
                .collect();
            style_ranges = styles(entities, &offsets);
            for styled in &mut style_ranges {
                styled.range = styled.range.start + shift..styled.range.end + shift;
            }
        }
        let mut link_list: Vec<Link> = Vec::new();
        let mut listed: HashMap<String, usize> = HashMap::new();
        for (link, _) in found {
            match listed.get(&link.url) {
                // The same URL also behind other words keeps its warning,
                // whichever came first.
                Some(&at) => {
                    let seen = &mut link_list[at];
                    if seen.disguise.is_none() {
                        seen.disguise = link.disguise;
                    }
                }
                None => {
                    listed.insert(link.url.clone(), link_list.len());
                    link_list.push(link);
                }
            }
        }
        // A sender may attach a preview of another page than the links in
        // the text, and Enter opens those links, not the preview's: a
        // preview of paypal.com under a link to a look-alike would vouch for
        // it. So it shows only when it's of one of the text's own links.
        let bare = |host: &str| host.strip_prefix("www.").unwrap_or(host).to_string();
        let card = message
            .link_preview
            .as_ref()
            .and_then(Card::new)
            .filter(|card| {
                link_list
                    .iter()
                    .filter_map(|l| link_host(&l.url))
                    .any(|host| bare(&text::clean(&host)) == bare(&card.host))
            });
        let ranges = link_ranges
            .iter_mut()
            .chain(style_ranges.iter_mut().map(|s| &mut s.range));
        let text = normalize(&text, ranges);
        // Formatting on nothing but hidden characters is gone with them.
        style_ranges.retain(|s| !s.range.is_empty());
        let formatted = message
            .entities
            .iter()
            .any(|e| Mark::of(e.kind).is_some() || (e.kind == EntityKind::Link && e.url.is_some()));
        let editable = match (&message.media, message.editable_until) {
            (None, Some(_)) if message.service.is_none() => Editable::Text,
            _ => Editable::No,
        };
        let reply_to = message.reply_to.as_ref().map(|r| ReplyTo {
            message_id: r.message_id,
            quoted: r.text.as_deref().map(|quoted| Replied {
                id: r.message_id.unwrap_or(0),
                sender: Sender::User(r.sender_id.unwrap_or(0)),
                outgoing: false,
                snippet: one_line(quoted),
            }),
        });
        let (preview, file, photo) = match shown {
            Some(s) => (s.preview, s.file, s.photo),
            None => (None, None, None),
        };
        Self {
            sender,
            outgoing: message.outgoing,
            date: message.date.clamp(0, i32::MAX as i64) as i32,
            text: match &service {
                Some(service) => service.label(),
                None => text,
            },
            source_text: source,
            preview,
            file,
            photo,
            links: link_list,
            link_ranges,
            styles: style_ranges,
            forwarded: message.forwarded,
            card,
            state: message.state.into(),
            reply_to,
            editable,
            editable_until: message.editable_until,
            deletable: message.deletable,
            formatted,
            edited: message.edited,
            album: message.album,
            reactions: reactions::from_meta(&message.reactions),
            service,
        }
    }
}

/// A replied message that isn't loaded, asked of the helper.
pub enum Fetched {
    Loading,
    Found(Replied),
    /// It couldn't be found: it was deleted, or unsent.
    Missing,
}

/// The top of the message view: `offset` lines into the block of `msg_id`.
/// Stored by message rather than line so it survives older messages loading above.
#[derive(Clone, Copy)]
pub struct ScrollAnchor {
    pub msg_id: i64,
    pub offset: usize,
}

/// The loaded messages are one unbroken stretch of the history. It usually
/// runs up to the newest message, but jumping to an old message starts a new
/// stretch around it, and scrolling down then loads the newer ones.
pub struct OpenChat {
    pub chat_id: i64,
    /// The newest message a read receipt was sent for.
    pub seen: i64,
    pub messages: BTreeMap<i64, Msg>,
    /// Message under the cursor. `None` means "the newest one, and follow new arrivals".
    pub selected: Option<i64>,
    /// Scroll position from the last frame; the UI keeps it up to date.
    pub scroll: Option<ScrollAnchor>,
    /// The history request in flight. Pages for any other request are
    /// stale and get dropped.
    pub loading: Option<Page>,
    /// There's nothing older than the oldest loaded message.
    pub all_loaded: bool,
    /// The loaded messages reach the newest one, so new arrivals join them.
    pub at_newest: bool,
    /// Set with `r`; the next message sent answers this one.
    pub reply: Option<Replied>,
    /// Set with `e`; Enter saves the composer's text into this message.
    pub editing: Option<Editing>,
    /// Files the next message sends, with the composer's text as caption.
    pub attachments: Vec<Attachment>,
    /// The paste that added the last attachments, while Ctrl-z can turn
    /// them back into text.
    pub dropped: Option<Dropped>,
    /// What replies answer when it isn't among the loaded messages, by the
    /// id of the reply.
    pub replied: HashMap<i64, Fetched>,
}

impl OpenChat {
    pub fn new(chat_id: i64) -> Self {
        Self {
            chat_id,
            seen: 0,
            messages: BTreeMap::new(),
            selected: None,
            scroll: None,
            loading: None,
            all_loaded: false,
            at_newest: true,
            reply: None,
            editing: None,
            attachments: Vec::new(),
            dropped: None,
            replied: HashMap::new(),
        }
    }

    /// Ctrl-z after a paste of file paths: takes back the files it
    /// attached, and gives the text that was pasted.
    pub fn undo_drop(&mut self) -> Option<String> {
        let dropped = self.dropped.take()?;
        let kept = self.attachments.len().saturating_sub(dropped.count);
        self.attachments.truncate(kept);
        Some(dropped.text)
    }

    /// What `e` edits: the message under the cursor, or in an album of
    /// photos, the part with the text, since it shows under the last photo
    /// wherever the cursor is.
    pub fn edit_target(&self) -> Option<i64> {
        let id = self.cursor_id()?;
        let mut captioned = self
            .bubble(id)
            .into_iter()
            .filter(|(_, m)| !m.source_text.is_empty())
            .map(|(id, _)| id);
        match (captioned.next(), captioned.next()) {
            (Some(holder), None) => Some(holder),
            _ => Some(id),
        }
    }

    /// The message under the cursor: the selected one, else the newest.
    pub fn cursor_id(&self) -> Option<i64> {
        self.selected.or_else(|| self.newest_id())
    }

    /// Why message `id` can't be edited at `now` (unix seconds), if it
    /// can't: the network says whose it is and until when.
    pub fn cant_edit(&self, id: i64, now: i64) -> Option<&'static str> {
        let msg = self.messages.get(&id)?;
        match msg.state {
            SendState::Pending => Some("Wait until it's sent"),
            SendState::Failed => Some("This message wasn't sent"),
            SendState::Sent if !msg.outgoing => Some("You can only edit your own messages"),
            SendState::Sent if msg.editable == Editable::No => {
                Some("Only text messages can be edited")
            }
            SendState::Sent if msg.editable_until.is_some_and(|until| until < now) => {
                Some("It's too late to edit this message")
            }
            SendState::Sent => None,
        }
    }

    /// The messages drawn as one bubble with message `id`: its album of
    /// photos or videos, or just itself. Albums of files show each file as
    /// its own message.
    pub fn bubble(&self, id: i64) -> Vec<(i64, &Msg)> {
        let Some(msg) = self.messages.get(&id) else {
            return Vec::new();
        };
        if msg.album != 0 {
            let photos: Vec<(i64, &Msg)> = self
                .messages
                .iter()
                .filter(|(_, m)| m.album == msg.album)
                .map(|(&id, m)| (id, m))
                .collect();
            if photos
                .iter()
                .all(|(_, m)| m.preview.as_ref().is_some_and(|p| !p.sticker))
            {
                return photos;
            }
        }
        vec![(id, msg)]
    }

    /// What `R` reacts to: the message under the cursor. An album of photos
    /// is one message on the network, whose reactions are on its first part.
    pub fn react_target(&self) -> Option<i64> {
        let bubble = self.bubble(self.cursor_id()?);
        bubble
            .iter()
            .find(|(_, m)| !m.reactions.is_empty())
            .or(bubble.first())
            .map(|&(id, _)| id)
    }

    /// Your reaction on message `id`, or on its album, with the part it's
    /// on: what the `R` popup marks as yours, and `X` takes back.
    pub fn your_reaction(&self, id: i64) -> Option<(i64, String)> {
        self.bubble(id).into_iter().find_map(|(id, m)| {
            m.reactions
                .iter()
                .find(|r| r.chosen)
                .map(|r| (id, r.emoji.clone()))
        })
    }

    /// Where `gd` goes from the message under the cursor: (the reply, the
    /// message it answers), or why it can't go anywhere.
    pub fn replied_jump(&self) -> Result<(i64, i64), &'static str> {
        let from = self.cursor_id().ok_or("No message selected")?;
        let reply = self
            .messages
            .get(&from)
            .and_then(|m| m.reply_to.as_ref())
            .ok_or("Not a reply")?;
        let to = reply
            .message_id
            .ok_or("The network didn't say which message it answers")?;
        if !self.messages.contains_key(&to)
            && matches!(self.replied.get(&from), Some(Fetched::Missing))
        {
            return Err("The message it answers was deleted");
        }
        Ok((from, to))
    }

    /// Loaded replies whose answered message isn't loaded, isn't quoted by
    /// the network and hasn't been asked for yet, as (reply, answered).
    /// They're marked as loading; the caller asks the helper.
    pub fn missing_replied(&mut self) -> Vec<(i64, i64)> {
        let missing: Vec<(i64, i64)> = self
            .messages
            .iter()
            .filter(|(id, msg)| msg.state == SendState::Sent && !self.replied.contains_key(id))
            .filter_map(|(&id, msg)| {
                let reply = msg.reply_to.as_ref()?;
                let answered = reply.message_id?;
                let known = reply.quoted.is_some() || self.messages.contains_key(&answered);
                (!known).then_some((id, answered))
            })
            .collect();
        for &(id, _) in &missing {
            self.replied.insert(id, Fetched::Loading);
        }
        missing
    }

    /// The helper's answer for what reply `reply_id` answers.
    pub fn set_replied(&mut self, reply_id: i64, replied: Option<&meta::Message>) {
        let fetched = match replied {
            Some(message) => Fetched::Found(Replied::new(message.id, &message.into())),
            None => Fetched::Missing,
        };
        self.replied.insert(reply_id, fetched);
    }

    /// A message the helper sent: a new one, or a new copy of one that's
    /// loaded (edited, reacted to, sent). A new one joins while the newest
    /// messages are loaded. One that arrives late, older than the newest
    /// loaded, joins at its place among the loaded ones; anything older
    /// loads with its page.
    pub fn upsert(&mut self, message: &meta::Message) {
        if let Some(msg) = self.messages.get_mut(&message.id) {
            msg.set_body(Msg::from(message));
            if let Some(reply) = self.reply.as_mut().filter(|r| r.id == message.id) {
                reply.snippet = msg.snippet();
            }
            return;
        }
        let newer = self.newest_id().is_none_or(|newest| message.id > newest);
        // Messages don't arrive in id order: one can cross yours in flight
        // (yours is reported first, under an id from this computer's clock)
        // or be held back by the network. The loaded messages are one
        // unbroken stretch, so a late one belongs among them, and shown
        // there: the next read receipt covers everything up to the newest.
        let inside = !newer
            && (self.all_loaded || self.oldest_id().is_some_and(|oldest| message.id > oldest));
        if (self.at_newest && newer) || inside {
            self.add_new(message.id, Msg::from(message));
        }
    }

    /// Adds a message that just arrived. While following new messages,
    /// only the newest [`MAX_FOLLOWED`] stay loaded (older ones load again on
    /// scrolling up), so a busy or spammed group can't grow memory and the
    /// layout done every frame without end.
    fn add_new(&mut self, id: i64, msg: Msg) {
        self.messages.insert(id, msg);
        // Not mid-request: a page must still join up.
        if self.loading.is_some() {
            return;
        }
        let loaded = self.messages.len();
        if self.selected.is_none() {
            while self.messages.len() > MAX_FOLLOWED {
                self.messages.pop_first();
                self.all_loaded = false;
            }
        } else if self.messages.len() > MAX_LOADED {
            // Reading older messages while new ones pour in: stop taking
            // them, as after a jump into the past. They load again on the way
            // down.
            while self.messages.len() > MAX_LOADED
                && self.messages.last_key_value().map(|(&id, _)| id) != self.selected
            {
                self.messages.pop_last();
            }
            self.at_newest = false;
        }
        if self.messages.len() < loaded {
            self.prune_replied();
        }
    }

    /// Forgets what unloaded replies answer.
    fn prune_replied(&mut self) {
        let messages = &self.messages;
        self.replied.retain(|id, _| messages.contains_key(id));
    }

    /// Adds a page of history, as (message id, message) pairs. A `Latest` or
    /// `Around` page replaces what was loaded, since it may not join up with it.
    pub fn add_page(&mut self, page: Page, messages: Vec<(i64, Msg)>) {
        let (oldest, newest) = (self.oldest_id(), self.newest_id());
        match page {
            Page::Latest => {
                // Keep anything that arrived after the page was put together.
                let page_newest = messages.iter().map(|(id, _)| *id).max();
                self.messages.retain(|&id, _| Some(id) > page_newest);
                self.at_newest = true;
                self.all_loaded = messages.is_empty();
            }
            Page::Around(target) => {
                if messages.is_empty() {
                    return;
                }
                self.messages.clear();
                self.scroll = None;
                self.at_newest = false;
                self.all_loaded = false;
                self.messages.extend(messages);
                // The target, or the closest message if it's gone.
                self.selected = self
                    .messages
                    .range(..=target)
                    .next_back()
                    .or_else(|| self.messages.first_key_value())
                    .map(|(&id, _)| id);
                self.prune_replied();
                return;
            }
            Page::Older(_) => {
                self.all_loaded = !messages
                    .iter()
                    .any(|(id, _)| oldest.is_none_or(|o| *id < o));
            }
            Page::Newer(_) => {
                self.at_newest = !messages
                    .iter()
                    .any(|(id, _)| newest.is_none_or(|n| *id > n));
            }
        }
        self.messages.extend(messages);
        self.prune_replied();
    }

    /// Swaps a message sent under a temporary id for the network's copy.
    pub fn replace(&mut self, old_id: i64, message: &meta::Message) {
        // Not loaded if an older part of the chat is shown; it loads with the rest.
        if self.messages.remove(&old_id).is_none() {
            return;
        }
        if self.selected == Some(old_id) {
            self.selected = Some(message.id);
        }
        // A reply to a message that was still sending goes to its real id.
        if let Some(reply) = self.reply.as_mut().filter(|r| r.id == old_id) {
            reply.id = message.id;
        }
        self.messages.insert(message.id, Msg::from(message));
    }

    /// A message you sent didn't go: it stays, marked as not sent.
    pub fn set_failed(&mut self, id: i64) {
        if let Some(msg) = self.messages.get_mut(&id) {
            msg.state = SendState::Failed;
        }
    }

    /// Messages that were unsent or deleted. The cursor on one of them moves
    /// to the message next to it, and replies to them say it was deleted.
    pub fn remove(&mut self, message_ids: &[i64]) {
        for &id in message_ids {
            self.messages.remove(&id);
            // The cursor stays about where you left it, on the next message
            // up (or down). Following the newest again marks read what
            // arrived below meanwhile, unseen: that's your move (`j` onto
            // the newest, `G`), not the sender's.
            if self.selected == Some(id) {
                self.selected = self
                    .messages
                    .range(..id)
                    .next_back()
                    .or_else(|| self.messages.range(id..).next())
                    .map(|(&id, _)| id);
            }
            if self.reply.as_ref().is_some_and(|r| r.id == id) {
                self.reply = None;
            }
        }
        // What replies quoted of them goes too: it was taken back.
        let gone: HashSet<i64> = message_ids.iter().copied().collect();
        for (&reply_id, msg) in &mut self.messages {
            if let Some(reply) = msg.reply_to.as_mut()
                && reply
                    .message_id
                    .is_some_and(|answered| gone.contains(&answered))
            {
                reply.quoted = None;
                self.replied.insert(reply_id, Fetched::Missing);
            }
        }
        for fetched in self.replied.values_mut() {
            if matches!(fetched, Fetched::Found(replied) if gone.contains(&replied.id)) {
                *fetched = Fetched::Missing;
            }
        }
    }

    pub fn oldest_id(&self) -> Option<i64> {
        self.messages.keys().next().copied()
    }

    pub fn newest_id(&self) -> Option<i64> {
        self.messages.keys().next_back().copied()
    }

    /// Moves the cursor `delta` messages (negative = older), clamped to what's
    /// loaded. Returns the new position counted from the oldest message.
    pub fn move_cursor(&mut self, delta: isize) -> Option<usize> {
        let ids: Vec<i64> = self.messages.keys().copied().collect();
        let last = ids.len().checked_sub(1)?;
        let current = self
            .selected
            .and_then(|id| ids.binary_search(&id).ok())
            .unwrap_or(last);
        let index = current.saturating_add_signed(delta).min(last);
        // Landing on the newest message switches back to following.
        self.selected = (index != last || !self.at_newest).then(|| ids[index]);
        Some(index)
    }
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;

    /// A message as the helper sends it, from JSON fields.
    pub fn meta_message(fields: serde_json::Value) -> meta::Message {
        let mut value = serde_json::json!({"id": 1, "chat_id": 1, "sender_id": 7, "date": 0});
        for (key, field) in fields.as_object().unwrap() {
            value[key] = field.clone();
        }
        serde_json::from_value(value).unwrap()
    }

    fn entity(offset: i32, length: i32, kind: &str) -> serde_json::Value {
        serde_json::json!({"offset": offset, "length": length, "type": kind})
    }

    fn link(offset: i32, length: i32, url: &str) -> serde_json::Value {
        serde_json::json!({"offset": offset, "length": length, "type": "link", "url": url})
    }

    fn msg(fields: serde_json::Value) -> Msg {
        Msg::from(&meta_message(fields))
    }

    /// A page of plain messages with these ids.
    pub fn page(ids: impl IntoIterator<Item = i64>) -> Vec<(i64, Msg)> {
        ids.into_iter()
            .map(|id| {
                let text = format!("message {id}");
                (id, msg(serde_json::json!({"id": id, "text": text})))
            })
            .collect()
    }

    fn ids(open: &OpenChat) -> Vec<i64> {
        open.messages.keys().copied().collect()
    }

    #[test]
    fn videos_preview_their_still_and_open_the_video() {
        let msg = msg(
            serde_json::json!({"text": "look", "media": {"kind": "video",
            "file_id": 10, "duration": 65, "width": 1920, "height": 1080,
            "thumbnail": {"file_id": 9, "width": 320, "height": 180}}}),
        );
        assert_eq!(msg.preview.as_ref().unwrap().file_id, 9);
        assert_eq!(msg.file.as_ref().unwrap().id, 10);
        assert_eq!(msg.text, "▶ 1:05\nlook");
        assert!(msg.photo.is_none(), "the viewer is for photos");

        let bare = super::tests::msg(serde_json::json!({"media": {"kind": "video",
            "file_id": 10, "duration": 5}}));
        assert!(bare.preview.is_none());
        assert_eq!(bare.text, "[Video 0:05]");
    }

    #[test]
    fn photos_show_their_thumbnail_in_the_bubble_and_the_whole_in_the_viewer() {
        let msg = msg(serde_json::json!({"media": {"kind": "photo", "file_id": 21,
            "width": 1280, "height": 960, "thumbnail": {"file_id": 22, "width": 320,
            "height": 240}}}));
        assert_eq!(msg.preview.as_ref().unwrap().file_id, 22);
        let photo = msg.photo.as_ref().unwrap();
        assert_eq!((photo.file_id, photo.width), (21, 1280));
        assert_eq!(
            msg.file,
            Some(MediaFile {
                id: 21,
                label: "Photo".into(),
                photo: true,
            })
        );
        assert_eq!(msg.text, "", "no caption, no text");
        assert_eq!(msg.snippet(), "Photo");
    }

    #[test]
    fn media_seen_only_once_is_left_to_the_phone_and_never_a_file() {
        let msg = msg(
            serde_json::json!({"text": "for you", "media": {"kind": "photo",
            "file_id": 0, "view_once": true}}),
        );
        assert!(msg.file.is_none() && msg.preview.is_none() && msg.photo.is_none());
        assert_eq!(msg.text, format!("[View-once photo · {ON_PHONE}]\nfor you"));
    }

    #[test]
    fn stickers_show_their_still_or_their_label() {
        let still = msg(
            serde_json::json!({"media": {"kind": "sticker", "file_id": 3,
            "mime": "image/webp", "width": 512, "height": 512}}),
        );
        let preview = still.preview.as_ref().unwrap();
        assert!(preview.sticker);
        assert_eq!(preview.file_id, 3);
        let animated = msg(
            serde_json::json!({"media": {"kind": "sticker", "file_id": 3,
            "mime": "application/x-tgsticker"}}),
        );
        assert!(animated.preview.is_none());
        assert_eq!(animated.text, "[Sticker]");
    }

    #[test]
    fn a_picture_without_a_size_is_drawn_at_a_usual_one() {
        let size = |p: &Preview| (p.width, p.height);
        let sticker = msg(
            serde_json::json!({"media": {"kind": "sticker", "file_id": 3,
            "mime": "image/webp", "thumbnail": {"file_id": 3, "width": 0, "height": 0}}}),
        );
        let preview = sticker.preview.as_ref().unwrap();
        assert!(preview.sticker);
        assert_eq!(size(preview), STICKER_SIZE);
        let bare = msg(
            serde_json::json!({"media": {"kind": "sticker", "file_id": 3,
            "mime": "image/webp"}}),
        );
        assert_eq!(size(bare.preview.as_ref().unwrap()), STICKER_SIZE);

        let photo = msg(serde_json::json!({"media": {"kind": "photo", "file_id": 4,
            "mime": "image/jpeg", "thumbnail": {"file_id": 4, "width": 0, "height": 0}}}));
        assert_eq!(size(photo.preview.as_ref().unwrap()), PICTURE_SIZE);
        assert_eq!(size(photo.photo.as_ref().unwrap()), PICTURE_SIZE);
        let video = msg(serde_json::json!({"media": {"kind": "video", "file_id": 5,
            "thumbnail": {"file_id": 6}}}));
        assert_eq!(size(video.preview.as_ref().unwrap()), PICTURE_SIZE);
        let card = msg(serde_json::json!({"text": "https://example.com",
            "link_preview": {"url": "https://example.com", "title": "Example",
            "image": {"file_id": 7}}}));
        assert_eq!(
            size(card.card.unwrap().image.as_ref().unwrap()),
            PICTURE_SIZE
        );

        // The media's own size still counts when only the still lacks one.
        let sized = msg(serde_json::json!({"media": {"kind": "photo", "file_id": 4,
            "width": 1280, "height": 960, "thumbnail": {"file_id": 8}}}));
        assert_eq!(size(sized.preview.as_ref().unwrap()), (1280, 960));
    }

    #[test]
    fn files_and_voice_messages_are_labeled_with_the_caption_under() {
        let file = msg(
            serde_json::json!({"text": "the plan", "media": {"kind": "file",
            "file_id": 4, "name": "plan\u{202E}.pdf"}}),
        );
        assert_eq!(file.text, "[File: plan.pdf]\nthe plan");
        assert_eq!(file.file.as_ref().unwrap().label, "File: plan.pdf");
        let sized = msg(serde_json::json!({"media": {"kind": "file", "file_id": 4,
            "name": "a.zip", "size": 2_200_000}}));
        assert_eq!(sized.text, "[File: a.zip · 2.1 MB]");
        assert_eq!(file.source_text, "the plan", "what y copies");
        let voice = msg(serde_json::json!({"media": {"kind": "voice", "file_id": 5,
            "duration": 12, "mime": "audio/mp4"}}));
        assert_eq!(voice.text, "[Voice message 0:12]");
    }

    #[test]
    fn links_come_from_the_networks_entities_and_written_out_addresses() {
        // "🎉" is 2 UTF-16 units, so the hidden link starts at offset 8, not 7.
        let text = "🎉 see: docs, then https://x.dev twice: https://x.dev. And www.a.example";
        let at = |needle: &str| {
            let byte = text.find(needle).unwrap();
            text[..byte].encode_utf16().count() as i32
        };
        let msg = msg(serde_json::json!({"text": text, "entities": [
            link(at("docs"), 4, "https://docs.rs"),
            link(0, 2, "file:///etc/passwd"),
        ]}));
        let urls: Vec<&str> = msg.links.iter().map(|l| l.url.as_str()).collect();
        assert_eq!(
            urls,
            ["https://docs.rs", "https://x.dev", "https://www.a.example"],
            "file:// dropped, duplicates once, scheme added"
        );
        assert_eq!(msg.links[0].disguise.as_deref(), Some("docs"));
        let shown: Vec<&str> = msg
            .link_ranges
            .iter()
            .map(|r| &msg.text[r.clone()])
            .collect();
        assert_eq!(
            shown,
            ["docs", "https://x.dev", "https://x.dev", "www.a.example"],
            "the full stop isn't part of the link"
        );
    }

    #[test]
    fn links_hidden_behind_other_words_are_marked_and_bad_entities_skipped() {
        let msg = msg(
            serde_json::json!({"text": "bank.com Example.com/ here", "entities": [
                link(0, 8, "https://evil.example"),
                link(9, 12, "https://www.example.com"),
                link(22, 4, "https://elsewhere.dev"),
                link(5, -3, "https://a.b"),
                link(30, 4, "https://c.d"),
            ]}),
        );
        assert_eq!(
            msg.links,
            [
                Link {
                    url: "https://evil.example".into(),
                    disguise: Some("bank.com".into()),
                },
                Link::from("https://www.example.com"),
                Link {
                    url: "https://elsewhere.dev".into(),
                    disguise: Some("here".into()),
                },
            ]
        );
    }

    #[test]
    fn a_hidden_link_keeps_its_warning_when_its_url_is_also_shown_plainly() {
        let msg = msg(
            serde_json::json!({"text": "https://evil.example then bank.com",
            "entities": [link(26, 8, "https://evil.example")]}),
        );
        assert_eq!(
            msg.links,
            [Link {
                url: "https://evil.example".into(),
                disguise: Some("bank.com".into()),
            }]
        );
    }

    #[test]
    fn a_links_host_is_read_the_way_browsers_read_it() {
        let host = |url| link_host(url);
        assert_eq!(
            host("https://bank.com.secure-login.evil.example/x").as_deref(),
            Some("bank.com.secure-login.evil.example")
        );
        assert_eq!(
            host("https://bank.com@evil.example/").as_deref(),
            Some("evil.example")
        );
        assert_eq!(
            host("https://evil.example\\@bank.com/").as_deref(),
            Some("evil.example")
        );
        assert_eq!(
            host("HTTPS:///Evil.Example:8443?x").as_deref(),
            Some("evil.example")
        );
        assert_eq!(host("http://[::1]:80/").as_deref(), Some("[::1]"));
        assert_eq!(host("https:///"), None);
        assert_eq!(host("ftp://a.example"), None);
    }

    #[test]
    fn a_backslash_before_the_query_is_written_as_the_slash_browsers_read() {
        let tricky = "https://paypal.com\\@evil.example/signin";
        assert_eq!(
            web_url(tricky).as_deref(),
            Some("https://paypal.com/@evil.example/signin"),
            "what the system's opener gets goes where the card says"
        );
        assert_eq!(
            web_url("https://a.example/x?q=a\\b#c\\d").as_deref(),
            Some("https://a.example/x?q=a\\b#c\\d"),
            "the query is left alone"
        );
        let msg = msg(serde_json::json!({"text": format!("see {tricky}"),
            "link_preview": {"url": "https://paypal.com", "title": "PayPal"}}));
        assert_eq!(
            msg.links,
            [Link::from("https://paypal.com/@evil.example/signin")]
        );
        assert_eq!(msg.card.unwrap().host, "paypal.com");
    }

    #[test]
    fn link_text_names_the_same_place_only_with_the_paths_case_and_its_scheme() {
        assert!(same_place("Example.com/", "https://www.example.com"));
        assert!(same_place("HTTPS://Bit.ly/AbC", "https://bit.ly/AbC"));
        assert!(
            same_place("bit.ly/AbC", "http://bit.ly/AbC"),
            "no scheme named"
        );
        assert!(!same_place("https://bit.ly/AbC", "https://bit.ly/abc"));
        assert!(!same_place("https://bank.example", "http://bank.example"));
    }

    #[test]
    fn a_links_parts_are_its_real_host_and_what_follows() {
        assert_eq!(
            link_parts("https://Bank.com@Evil.example:443/A?b#c"),
            Some(("evil.example".to_string(), "/A?b#c"))
        );
        assert_eq!(
            link_parts("http://a.example"),
            Some(("a.example".to_string(), ""))
        );
        assert_eq!(link_parts("ftp://a.example/x"), None);
    }

    #[test]
    fn urls_with_spaces_or_line_breaks_are_not_links() {
        assert_eq!(web_url("https://a.example/x y"), None);
        assert_eq!(web_url("a.example/\nx"), None);
        assert_eq!(
            web_url("a.example/x").as_deref(),
            Some("https://a.example/x")
        );
    }

    fn styled(msg: &Msg) -> Vec<(&str, Format)> {
        msg.styles
            .iter()
            .map(|s| (&msg.text[s.range.clone()], s.format))
            .collect()
    }

    #[test]
    fn nested_and_overlapping_formatting_is_cut_into_stretches() {
        let msg = msg(
            serde_json::json!({"text": "bold and italic code", "entities": [
                entity(0, 15, "bold"),
                entity(5, 10, "italic"),
                entity(16, 4, "code"),
                entity(2, 0, "strike"),
            ]}),
        );
        let bold = Format {
            bold: true,
            ..Format::default()
        };
        let both = Format {
            italic: true,
            ..bold
        };
        let code = Format {
            code: true,
            ..Format::default()
        };
        assert_eq!(
            styled(&msg),
            [("bold ", bold), ("and italic", both), ("code", code)]
        );
        assert!(msg.formatted, "an edit would lose it");
    }

    #[test]
    fn caption_formatting_lines_up_after_a_label_and_expanded_tabs() {
        let msg = msg(
            serde_json::json!({"text": "a\tbold", "entities": [entity(2, 4, "bold")],
            "media": {"kind": "file", "file_id": 4, "name": "x.txt"}}),
        );
        assert_eq!(msg.text, "[File: x.txt]\na    bold");
        let shown: Vec<&str> = styled(&msg).into_iter().map(|(s, _)| s).collect();
        assert_eq!(shown, ["bold"]);
    }

    #[test]
    fn a_link_preview_shows_only_for_a_link_in_the_message() {
        let preview = serde_json::json!({"url": "https://www.example.com/post",
            "title": "A post", "description": "about\nthings",
            "image": {"file_id": 30, "width": 600, "height": 315}});
        let msg_with =
            |text: &str| msg(serde_json::json!({"text": text, "link_preview": preview.clone()}));
        let card = msg_with("see https://example.com/post").card.unwrap();
        assert_eq!(card.host, "www.example.com");
        assert_eq!(card.description, "about things");
        assert_eq!(card.image.unwrap().file_id, 30);
        assert!(msg_with("see https://paypa1.example").card.is_none());
        assert!(msg_with("no links").card.is_none());
    }

    #[test]
    fn service_messages_say_what_happened_instead_of_text() {
        let msg = msg(serde_json::json!({"service": "Alice named the group Trip"}));
        assert_eq!(msg.text, "Alice named the group Trip");
        assert!(msg.service.is_some());
        assert_eq!(msg.editable, Editable::No);

        // Text sent along with it, against the protocol, has no say in how
        // the sentence is drawn: its ranges would cut the sentence anywhere.
        let odd = super::tests::msg(serde_json::json!({"service": "Zoë left",
            "text": "🎉🎉 https://a.example", "entities": [entity(1, 2, "bold")]}));
        assert_eq!(odd.text, "Zoë left");
        assert!(odd.styles.is_empty() && odd.link_ranges.is_empty());
    }

    /// The byte offset of a UTF-16 offset, found by walking the text from
    /// its start: what [`Utf16Offsets`] must agree with.
    fn walked(text: &str, utf16: i32) -> usize {
        let mut units = 0;
        for (i, c) in text.char_indices() {
            if units >= utf16.max(0) as usize {
                return i;
            }
            units += c.len_utf16();
        }
        text.len()
    }

    #[test]
    fn utf16_offsets_land_where_walking_the_text_does() {
        let texts = [
            "",
            "plain",
            "héllo wörld",
            "日本語のテキスト",
            "a🎉b😀",
            "👨\u{200D}👩\u{200D}👧!",
        ];
        for text in texts {
            let offsets = Utf16Offsets::new(text);
            let units = text.encode_utf16().count() as i32;
            for utf16 in -3..units + 4 {
                assert_eq!(
                    offsets.byte(utf16),
                    walked(text, utf16),
                    "{text:?} at {utf16}"
                );
            }
        }
        assert_eq!(Utf16Offsets::new("ab").byte(i32::MAX), 2);
    }

    #[test]
    fn a_message_with_thousands_of_formatting_runs_converts_quickly() {
        // What a sender's `*a* _b_ ` repeated to WhatsApp's length becomes.
        let runs = 8192;
        let text = "a b ".repeat(runs);
        let entities: Vec<serde_json::Value> = (0..runs as i32)
            .flat_map(|i| [entity(i * 4, 1, "bold"), entity(i * 4 + 2, 1, "italic")])
            .collect();
        let message = meta_message(serde_json::json!({"text": text, "entities": entities}));
        let started = std::time::Instant::now();
        let msg = Msg::from(&message);
        let took = started.elapsed();
        assert!(
            took < std::time::Duration::from_millis(500),
            "took {took:?}: the text was walked again for every entity"
        );
        assert_eq!(
            msg.styles.len(),
            MAX_ENTITIES,
            "entities past the most a message keeps are left out"
        );
    }

    #[test]
    fn what_cant_be_shown_is_named() {
        let msg = msg(serde_json::json!({"unsupported": "[Poll]", "text": "Lunch?"}));
        assert_eq!(msg.text, "[Poll]\nLunch?");
        assert!(msg.link_ranges.is_empty());
    }

    #[test]
    fn only_your_sent_text_messages_can_be_edited_while_the_network_allows() {
        let mut open = OpenChat::new(1);
        let mine = |id: i64, fields: serde_json::Value| {
            let mut fields = fields;
            fields["id"] = id.into();
            fields["outgoing"] = true.into();
            (id, msg(fields))
        };
        open.messages.extend([
            mine(1, serde_json::json!({"text": "hi", "editable_until": 100})),
            mine(
                2,
                serde_json::json!({"text": "hi", "editable_until": 100, "state": "pending"}),
            ),
            mine(
                3,
                serde_json::json!({"media": {"kind": "photo", "file_id": 1}}),
            ),
            (4, msg(serde_json::json!({"id": 4, "text": "theirs"}))),
        ]);
        assert_eq!(open.cant_edit(1, 50), None);
        assert_eq!(
            open.cant_edit(1, 150),
            Some("It's too late to edit this message")
        );
        assert_eq!(open.cant_edit(2, 50), Some("Wait until it's sent"));
        assert_eq!(
            open.cant_edit(3, 50),
            Some("Only text messages can be edited")
        );
        assert_eq!(
            open.cant_edit(4, 50),
            Some("You can only edit your own messages")
        );
    }

    #[test]
    fn a_new_copy_of_a_message_replaces_it_and_new_ones_join_only_at_the_newest() {
        let mut open = OpenChat::new(1);
        open.add_page(Page::Latest, page(10..13));
        open.upsert(&meta_message(
            serde_json::json!({"id": 11, "text": "edited",
            "edited": true}),
        ));
        assert_eq!(open.messages[&11].text, "edited");
        assert!(open.messages[&11].edited);

        open.upsert(&meta_message(serde_json::json!({"id": 20, "text": "new"})));
        assert_eq!(ids(&open), [10, 11, 12, 20]);
        open.upsert(&meta_message(serde_json::json!({"id": 5, "text": "older"})));
        assert_eq!(
            ids(&open),
            [10, 11, 12, 20],
            "older ones come with their page"
        );

        open.at_newest = false;
        open.upsert(&meta_message(
            serde_json::json!({"id": 30, "text": "newer"}),
        ));
        assert_eq!(
            ids(&open),
            [10, 11, 12, 20],
            "not while an old stretch is loaded"
        );
        open.upsert(&meta_message(serde_json::json!({"id": 15, "text": "late"})));
        assert_eq!(
            ids(&open),
            [10, 11, 12, 15, 20],
            "but inside it, at its place"
        );

        open.all_loaded = true;
        open.upsert(&meta_message(serde_json::json!({"id": 5, "text": "older"})));
        assert_eq!(
            ids(&open),
            [5, 10, 11, 12, 15, 20],
            "the whole chat is loaded, so it's the first"
        );
    }

    #[test]
    fn a_message_that_crossed_yours_in_flight_is_shown_at_its_place() {
        let mut open = OpenChat::new(1);
        open.add_page(Page::Latest, page(10..13));
        // Yours is reported first, under an id from this computer's clock…
        open.upsert(&meta_message(serde_json::json!({"id": 50, "text": "mine",
            "outgoing": true, "state": "pending"})));
        // …then theirs, which the network stamped a moment before.
        open.upsert(&meta_message(
            serde_json::json!({"id": 40, "text": "theirs"}),
        ));
        assert_eq!(
            ids(&open),
            [10, 11, 12, 40, 50],
            "a read receipt for anything newer covers it, so it's on screen"
        );
    }

    #[test]
    fn a_busy_chat_keeps_only_the_newest_messages_while_following_them() {
        let mut open = OpenChat::new(1);
        open.add_page(Page::Latest, page(0..10));
        for (id, msg) in page(10..MAX_FOLLOWED as i64 + 50) {
            open.add_new(id, msg);
        }
        assert_eq!(open.messages.len(), MAX_FOLLOWED);
        assert_eq!(open.newest_id(), Some(MAX_FOLLOWED as i64 + 49));
        assert!(
            !open.all_loaded,
            "the dropped ones load again on scrolling up"
        );

        // Reading further up, nothing goes, up to a point.
        open.selected = open.oldest_id();
        for (id, msg) in page(5000..5010) {
            open.add_new(id, msg);
        }
        assert_eq!(open.messages.len(), MAX_FOLLOWED + 10);
        for (id, msg) in page(6000..6000 + MAX_LOADED as i64) {
            open.add_new(id, msg);
        }
        assert_eq!(open.messages.len(), MAX_LOADED, "then new ones wait");
        assert!(!open.at_newest, "and load again on the way down");
        assert_eq!(open.selected, open.oldest_id(), "the cursor stays put");
    }

    #[test]
    fn what_replies_answer_is_forgotten_with_the_replies() {
        let mut open = OpenChat::new(1);
        open.add_page(Page::Latest, page(0..10));
        open.set_replied(3, None);
        open.set_replied(4, None);
        for (id, msg) in page(10..MAX_FOLLOWED as i64 + 50) {
            open.add_new(id, msg);
        }
        assert!(open.replied.is_empty(), "replies 3 and 4 were unloaded");
    }

    #[test]
    fn jumping_to_an_old_message_loads_around_it() {
        let mut open = OpenChat::new(1);
        open.add_page(Page::Latest, page(90..100));
        assert!(open.at_newest);

        open.add_page(Page::Around(20), page(15..26));
        assert_eq!(
            ids(&open),
            (15..26).collect::<Vec<_>>(),
            "replaces the rest"
        );
        assert_eq!(open.selected, Some(20));
        assert!(!open.at_newest);

        // At the end of what's loaded, the cursor stays put instead of following.
        open.move_cursor(isize::MAX);
        assert_eq!(open.selected, Some(25));

        open.add_page(Page::Newer(25), page(25..40));
        assert!(!open.at_newest, "more came, there may be more still");
        open.add_page(Page::Newer(39), page(39..40));
        assert!(open.at_newest, "nothing newer: caught up");
        open.move_cursor(isize::MAX);
        assert_eq!(open.selected, None, "follows new messages again");
    }

    #[test]
    fn a_jump_to_a_deleted_message_lands_next_to_it() {
        let mut open = OpenChat::new(1);
        open.add_page(Page::Around(20), page([17, 18, 22, 23]));
        assert_eq!(open.selected, Some(18));
        open.add_page(Page::Around(5), page([8, 9]));
        assert_eq!(open.selected, Some(8));
    }

    #[test]
    fn older_pages_tell_when_the_start_is_reached() {
        let mut open = OpenChat::new(1);
        open.add_page(Page::Latest, page(50..60));
        open.add_page(Page::Older(50), page(40..50));
        assert!(!open.all_loaded);
        open.add_page(Page::Older(40), page([]));
        assert!(open.all_loaded);
    }

    #[test]
    fn the_latest_page_keeps_messages_that_arrived_meanwhile() {
        let mut open = OpenChat::new(1);
        // Stale messages from an old stretch, plus one that just arrived.
        open.messages.extend(page([3, 4, 101]));
        open.add_page(Page::Latest, page(90..100));
        assert_eq!(ids(&open), (90..100).chain([101]).collect::<Vec<_>>());
    }

    #[test]
    fn snippets_put_a_message_on_one_line_or_name_what_it_holds() {
        let mut msgs = page([1]);
        let msg = &mut msgs[0].1;
        msg.text = "first line\n\nsecond\tline ".into();
        assert_eq!(msg.snippet(), "first line second line");
    }

    #[test]
    fn a_one_line_cut_short_says_so() {
        // Braille blanks look like nothing but take room.
        let name = format!("invoice.pdf{}.exe", "\u{2800}".repeat(SNIPPET_CHARS));
        let line = one_line(&name);
        assert!(
            line.starts_with("invoice.pdf") && line.ends_with('…'),
            "{line}"
        );
        assert_eq!(line.chars().count(), SNIPPET_CHARS + 1);
        assert_eq!(one_line("short"), "short");
    }

    #[test]
    fn a_message_padded_with_joiners_shows_what_they_hide() {
        let text = format!("pay\u{200D}{} now", "\u{200D}".repeat(500));
        let msg = msg(serde_json::json!({"text": text, "entities": [entity(0, 3, "bold")]}));
        assert_eq!(msg.text, "pay\u{200D} now");
        assert_eq!(
            styled(&msg),
            [(
                "pay",
                Format {
                    bold: true,
                    ..Format::default()
                }
            )]
        );
    }

    fn album(open: &mut OpenChat, parts: &[i64]) {
        for &id in parts {
            let msg = open.messages.get_mut(&id).unwrap();
            msg.album = parts[0];
            msg.source_text = String::new();
            msg.preview = Some(Preview {
                file_id: id as i32,
                width: 10,
                height: 10,
                thumbnail: None,
                sticker: false,
            });
        }
    }

    #[test]
    fn e_in_an_album_edits_the_part_with_the_text() {
        let mut open = OpenChat::new(1);
        open.messages = page([1, 2, 3, 4]).into_iter().collect();
        album(&mut open, &[1, 2, 3]);
        open.messages.get_mut(&3).unwrap().source_text = "the view".into();
        open.selected = Some(1);
        assert_eq!(open.edit_target(), Some(3));
        open.selected = Some(4);
        assert_eq!(open.edit_target(), Some(4), "not in the album");
    }

    #[test]
    fn r_in_an_album_reacts_on_its_first_part_and_x_finds_yours_on_any() {
        let mut open = OpenChat::new(1);
        open.messages = page([1, 2, 3, 4]).into_iter().collect();
        album(&mut open, &[1, 2, 3]);
        open.selected = Some(3);
        assert_eq!(open.react_target(), Some(1));
        let mine = Reaction {
            emoji: "🔥".into(),
            count: 2,
            chosen: true,
        };
        open.messages.get_mut(&2).unwrap().reactions = vec![mine];
        assert_eq!(open.react_target(), Some(2), "where it already has some");
        assert_eq!(open.your_reaction(3), Some((2, "🔥".to_string())));
        assert_eq!(open.your_reaction(4), None, "not in the album");
    }

    #[test]
    fn ctrl_z_takes_back_only_the_files_the_paste_attached() {
        let file = |name: &str| Attachment {
            path: name.into(),
            name: name.into(),
            size: 1,
            kind: crate::attach::Kind::File,
            identity: Default::default(),
            image_id: 0,
        };
        let mut open = OpenChat::new(1);
        open.attachments = vec![file("chosen.pdf"), file("a.txt"), file("b.txt")];
        open.dropped = Some(Dropped {
            text: "/tmp/a.txt /tmp/b.txt".into(),
            count: 2,
        });
        assert_eq!(open.undo_drop().as_deref(), Some("/tmp/a.txt /tmp/b.txt"));
        let names: Vec<&str> = open.attachments.iter().map(|a| a.name.as_str()).collect();
        assert_eq!(names, ["chosen.pdf"], "the file attached before stays");
        assert_eq!(open.undo_drop(), None, "only once");
    }

    #[test]
    fn deleting_the_message_being_replied_to_ends_the_reply() {
        let mut open = OpenChat::new(1);
        open.add_page(Page::Latest, page(1..5));
        open.reply = Some(Replied::new(3, &open.messages[&3]));
        assert_eq!(open.reply.as_ref().unwrap().snippet, "message 3");

        open.remove(&[2]);
        assert!(open.reply.is_some(), "another message went");
        open.remove(&[3]);
        assert!(open.reply.is_none());
    }

    fn reply(messages: &mut [(i64, Msg)], at: usize, message_id: Option<i64>, quoted: bool) {
        messages[at].1.reply_to = Some(ReplyTo {
            message_id,
            quoted: quoted.then(|| Replied {
                id: message_id.unwrap_or(0),
                sender: Sender::User(2),
                outgoing: false,
                snippet: "the network's quote".into(),
            }),
        });
    }

    #[test]
    fn replied_messages_that_arent_loaded_or_quoted_are_asked_for_once() {
        let mut open = OpenChat::new(1);
        let mut messages = page(10..15);
        reply(&mut messages, 1, Some(10), false); // 11 answers 10, which is loaded
        reply(&mut messages, 2, Some(3), false); // 12 answers an older message
        reply(&mut messages, 3, Some(4), true); // 13 quotes what it answers
        reply(&mut messages, 4, None, false); // 14: the network doesn't say
        open.add_page(Page::Latest, messages);

        assert_eq!(open.missing_replied(), [(12, 3)]);
        assert!(open.missing_replied().is_empty(), "already asked");

        open.set_replied(12, None);
        assert!(matches!(open.replied[&12], Fetched::Missing));

        // Once it's deleted, the reply says so without asking.
        open.remove(&[10]);
        assert!(open.missing_replied().is_empty());
        assert!(matches!(open.replied[&11], Fetched::Missing));
    }

    #[test]
    fn a_deleted_messages_quote_goes_from_the_replies_to_it() {
        let mut open = OpenChat::new(1);
        let mut messages = page(10..15);
        reply(&mut messages, 1, Some(3), true); // 11 quotes 3, not loaded
        reply(&mut messages, 2, Some(4), false); // 12 answers 4, fetched
        reply(&mut messages, 3, Some(5), true); // 13 quotes 5, which stays
        open.add_page(Page::Latest, messages);
        assert_eq!(open.missing_replied(), [(12, 4)]);
        let found = meta_message(serde_json::json!({"id": 4, "text": "secret"}));
        open.set_replied(12, Some(&found));

        open.remove(&[3, 4]);
        assert!(
            open.messages[&11]
                .reply_to
                .as_ref()
                .unwrap()
                .quoted
                .is_none()
        );
        assert!(matches!(open.replied[&11], Fetched::Missing));
        assert!(matches!(open.replied[&12], Fetched::Missing));
        assert!(
            open.messages[&13]
                .reply_to
                .as_ref()
                .unwrap()
                .quoted
                .is_some()
        );
        assert!(!open.replied.contains_key(&13));
    }

    #[test]
    fn deleting_the_message_under_the_cursor_moves_it_next_door_not_to_the_newest() {
        let mut open = OpenChat::new(1);
        open.add_page(Page::Latest, page(1..6));
        // Reading up the chat, or looking at a photo there.
        open.selected = Some(3);
        open.remove(&[3]);
        assert_eq!(open.selected, Some(2), "the one above, which you've seen");
        open.remove(&[1, 2]);
        assert_eq!(open.selected, Some(4), "else the one below");
        open.remove(&[4, 5]);
        assert_eq!(open.selected, None, "nothing left");

        let mut open = OpenChat::new(1);
        open.add_page(Page::Latest, page(1..6));
        open.remove(&[5]);
        assert_eq!(open.selected, None, "still following the newest");
    }

    #[test]
    fn gd_goes_from_a_reply_to_the_message_it_answers() {
        let mut open = OpenChat::new(1);
        let mut messages = page(10..15);
        reply(&mut messages, 1, Some(10), false);
        reply(&mut messages, 2, Some(3), true); // not loaded
        reply(&mut messages, 3, None, false);
        reply(&mut messages, 4, Some(4), false); // the newest, answering a deleted message
        open.add_page(Page::Latest, messages);
        open.replied.insert(14, Fetched::Missing);

        let from = |open: &mut OpenChat, id| {
            open.selected = Some(id);
            open.replied_jump()
        };
        assert_eq!(from(&mut open, 11), Ok((11, 10)));
        assert_eq!(from(&mut open, 12), Ok((12, 3)), "loads around it");
        assert_eq!(
            from(&mut open, 13),
            Err("The network didn't say which message it answers")
        );
        assert_eq!(from(&mut open, 10), Err("Not a reply"));
        open.selected = None;
        assert_eq!(
            open.replied_jump(),
            Err("The message it answers was deleted"),
            "no selection means the newest"
        );
    }

    #[test]
    fn a_sent_message_takes_its_final_id_and_a_failed_one_stays_marked() {
        let mut open = OpenChat::new(1);
        open.add_page(Page::Latest, page(1..3));
        open.upsert(&meta_message(serde_json::json!({"id": 50, "text": "hi",
            "outgoing": true, "state": "pending"})));
        open.selected = Some(50);
        open.reply = Some(Replied::new(50, &open.messages[&50]));
        open.replace(
            50,
            &meta_message(serde_json::json!({"id": 60, "text": "hi",
            "outgoing": true})),
        );
        assert_eq!(ids(&open), [1, 2, 60]);
        assert_eq!(open.selected, Some(60));
        assert_eq!(open.reply.as_ref().unwrap().id, 60);
        assert_eq!(open.messages[&60].state, SendState::Sent);

        open.set_failed(60);
        assert_eq!(open.messages[&60].state, SendState::Failed);
    }

    #[test]
    fn durations_read_like_a_clock() {
        assert_eq!(duration(5), "0:05");
        assert_eq!(duration(65), "1:05");
        assert_eq!(duration(3725), "1:02:05");
        assert_eq!(duration(-3), "0:00");
    }
}
