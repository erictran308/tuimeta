//! A network's home feed, opened from its row at the top of its chats:
//! Instagram's or Threads' posts, in the network's order, a page at a time
//! as the cursor nears the end. One post is selected; a carousel shows one
//! of its photos or videos at a time (Threads draws them side by side).
//!
//! Everything in a post is the author's: names, captions and the lines
//! over posts go through [`text::clean`], a username keeps only the
//! letters Instagram allows in one, so it can't pass for another, the
//! post's link is kept only if it goes to its network's website, and a
//! link card's only if it's a web address, shown by its host.

use std::collections::{HashMap, HashSet};
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::Duration;

use tokio::time::Instant;

use crate::messages::{self, MediaFile, Preview};
use crate::meta::{self, FeedPost, FeedView, MediaKind, Network};
use crate::motion::{GLIDE_ROWS, Glide};
use crate::text;

/// Posts asked for at a time.
pub const PAGE: i32 = 10;
/// Within this many posts of the last one loaded, the next page is asked
/// for, so moving on doesn't wait; not sooner, since the feed is loaded
/// only as it's read.
pub const MORE_WITHIN: usize = 5;
/// Posts after the selected one whose pictures download before they're in
/// view (`ui::feed::draw`), of those loaded: files, which take no memory.
pub const AHEAD: usize = 10;
/// Of those, the posts whose pictures are also made into images before
/// they're in view: few, since each takes memory, but enough for the next
/// post's to be there when `j` gets to it.
pub const MADE_AHEAD: usize = 3;
/// Posts Ctrl-d and Ctrl-u move the cursor by. Further than this is a jump
/// (`gg`, `G`), which the view doesn't slide through: that would only make
/// images of posts gone by in a moment.
pub const JUMP: isize = 3;
/// A post in view for less than this isn't told about: threads.com's limit.
const MIN_VIEW: Duration = Duration::from_millis(250);
/// The most posts kept to tell about, the oldest going first: as many as one
/// `load_feed` takes.
const MAX_VIEWS: usize = 200;
/// Pages in a row that may bring nothing new before the feed counts as
/// ended: each is up to three requests in the helper, and a key held down
/// at the end would otherwise go on asking.
const MAX_EMPTY_PAGES: u8 = 3;
/// A caption is cut after this many characters, which is what Instagram
/// allows.
const MAX_CAPTION: usize = 2200;
/// The longest username Instagram allows.
const MAX_USERNAME: usize = 30;
/// The posts of a carousel taken, as many as Instagram allows.
const MAX_ITEMS: usize = 20;
/// The line over a Threads post is cut after this many characters.
const MAX_CONTEXT: usize = 120;
/// And a link card's title.
const MAX_TITLE: usize = 300;
/// The longest code of a post in a link the helper takes.
const MAX_CODE: usize = 64;

/// The last number a load of a feed, a post or comments was given. One count
/// for the run, not one per feed or pane: a feed made again (another link,
/// a new login) would count from 1 again and take a late answer to the old
/// one's load for its own.
static LOADS: AtomicU64 = AtomicU64::new(0);

/// A number no load has had yet.
pub fn next_load() -> u64 {
    LOADS.fetch_add(1, Ordering::Relaxed) + 1
}

/// A post of the feed, made safe to show.
#[derive(Clone)]
pub struct Post {
    pub id: i64,
    /// The author's name on one line; may be empty.
    pub name: String,
    /// Without the `@`; empty if it had letters Instagram doesn't allow.
    pub username: String,
    pub verified: bool,
    /// Unix seconds.
    pub date: i64,
    /// Line breaks kept.
    pub caption: String,
    pub media: Vec<Item>,
    pub likes: Option<i64>,
    pub comments: Option<i64>,
    /// The post on its network's website, for `o`.
    pub url: Option<String>,
    /// From an account the user doesn't follow: Instagram's suggestion.
    pub suggested: bool,
    /// The author's picture, small.
    pub avatar: Option<Preview>,
    /// Threads: reposts and quotes of it, and times it was sent on.
    pub reposts: Option<i64>,
    pub shares: Option<i64>,
    /// Threads: the line over the post ("sam reposted"), on one line.
    pub context: Option<String>,
    /// Threads: it goes on from the post before it, in one thread.
    pub continues: bool,
    /// Threads: the post it quotes, without one of its own.
    pub quoted: Option<Box<Post>>,
    /// Threads: the card of a link in its text.
    pub link: Option<LinkCard>,
}

/// The card of a link in a post: where it goes, and what it says it is.
#[derive(Clone)]
pub struct LinkCard {
    /// A web address (`messages::web_url`).
    pub url: String,
    /// Where it goes, as the card shows it.
    pub host: String,
    /// One line; may be empty.
    pub title: String,
    pub image: Option<Preview>,
}

/// A photo or video of a post.
#[derive(Clone)]
pub struct Item {
    pub video: bool,
    /// What's drawn: the photo, or the video's cover.
    pub still: Option<Preview>,
    /// What Enter shows or plays.
    pub file: Option<MediaFile>,
    /// A video's sound, by file id, when it's a file of its own: `file` is
    /// then the picture alone, and a player plays the two together.
    pub audio: Option<i32>,
    /// Seconds, for a video.
    pub duration: i32,
}

impl Post {
    /// The author as the post's header names them: their name, else
    /// their username.
    pub fn author(&self) -> &str {
        if self.name.is_empty() {
            &self.username
        } else {
            &self.name
        }
    }
}

impl From<&FeedPost> for Post {
    /// An Instagram post.
    fn from(post: &FeedPost) -> Self {
        Post::new(post, Network::Instagram)
    }
}

impl Post {
    /// `network`'s post, made safe to show.
    pub fn new(post: &FeedPost, network: Network) -> Self {
        let caption = cut(&text::clean(&post.caption), MAX_CAPTION);
        let context = post
            .context
            .as_deref()
            .map(|c| cut(&messages::one_line(c), MAX_CONTEXT))
            .filter(|c| !c.is_empty());
        Self {
            id: post.id,
            name: messages::one_line(&post.author.name),
            username: username(&post.author.username),
            verified: post.author.verified,
            date: post.date,
            caption: caption.trim_end().to_string(),
            media: post.media.iter().filter_map(item).take(MAX_ITEMS).collect(),
            likes: post.likes.map(|n| n.max(0)),
            comments: post.comments.map(|n| n.max(0)),
            url: post_url(&post.url, network),
            suggested: post.suggested,
            avatar: post.author.photo.as_ref().and_then(|p| {
                (p.file_id > 0).then_some(Preview {
                    file_id: p.file_id,
                    width: p.width.max(1),
                    height: p.height.max(1),
                    thumbnail: None,
                    sticker: false,
                })
            }),
            reposts: post.reposts.map(|n| n.max(0)),
            shares: post.shares.map(|n| n.max(0)),
            context,
            continues: post.continues,
            // A quote's own quote isn't drawn.
            quoted: post.quoted.as_deref().map(|q| {
                let mut quoted = Post::new(q, network);
                quoted.quoted = None;
                quoted.context = None;
                quoted.continues = false;
                Box::new(quoted)
            }),
            link: post.link_preview.as_ref().and_then(link_card),
        }
    }
}

/// `text` cut after `max` characters, with `…` when it was longer.
fn cut(text: &str, max: usize) -> String {
    match text::first_chars(text, max) {
        start if start.len() < text.len() => format!("{start}…"),
        start => start.to_string(),
    }
}

/// A username as Instagram allows them: letters, digits, `.` and `_`, at
/// most 30. Anything else, which would let a name like `аlice` (with a
/// Cyrillic `а`) pass for `alice`, leaves it out.
pub(crate) fn username(name: &str) -> String {
    let name = name.trim().trim_start_matches('@');
    let allowed = |c: char| c.is_ascii_alphanumeric() || c == '.' || c == '_';
    if name.is_empty() || name.chars().count() > MAX_USERNAME || !name.chars().all(allowed) {
        return String::new();
    }
    name.to_string()
}

/// `url` if it's a web address on `network`'s website: instagram.com, or
/// threads.com.
fn post_url(url: &str, network: Network) -> Option<String> {
    let url = messages::web_url(url)?;
    let host = messages::link_host(&url)?;
    let ours = match network {
        Network::Threads => matches!(host.as_str(), "www.threads.com" | "threads.com"),
        _ => matches!(host.as_str(), "www.instagram.com" | "instagram.com"),
    };
    (url.starts_with("https://") && ours).then_some(url)
}

/// A link to an Instagram or Threads post, by what names the post in it.
#[derive(Clone, Debug, PartialEq)]
pub struct PostLink {
    pub network: Network,
    /// The post's code, as the link has it.
    pub code: String,
    /// Threads: the author the link names, if it does.
    pub username: Option<String>,
    /// The link itself, a web address, for `o` and `y`.
    pub url: String,
}

/// `url` if it's a link to an Instagram or Threads post: on instagram.com
/// `/p/<code>`, `/reel/<code>`, `/reels/<code>` or `/tv/<code>`, with an
/// account's name before it or not; on threads.com (or threads.net)
/// `/@<username>/post/<code>` or `/t/<code>`. Only on the sites
/// themselves, with nothing else before the path (no `@`, no port), so the
/// post shown is the one a browser would open.
pub fn post_link(url: &str) -> Option<PostLink> {
    let url = messages::web_url(url)?;
    let lower = url.to_ascii_lowercase();
    let rest = lower
        .strip_prefix("https://")
        .or_else(|| lower.strip_prefix("http://"))?;
    let end = rest.find(['/', '?', '#']).unwrap_or(rest.len());
    let network = match &rest[..end] {
        "instagram.com" | "www.instagram.com" | "m.instagram.com" => Network::Instagram,
        "threads.com" | "www.threads.com" | "threads.net" | "www.threads.net" => Network::Threads,
        _ => return None,
    };
    // The path as written: codes are told apart by case.
    let path = &url[url.len() - rest.len() + end..];
    let path = &path[..path.find(['?', '#']).unwrap_or(path.len())];
    let parts: Vec<&str> = path.split('/').filter(|p| !p.is_empty()).collect();
    let post_kind = |kind: &str| matches!(kind, "p" | "reel" | "reels" | "tv");
    // A name before the kind is an account's, not a page of the site's own
    // (`/share/p/…` is a share link, whose code isn't the post's).
    let account =
        |name: &str| username(name) == name && !matches!(name, "share" | "stories" | "explore");
    let (code, username) = match (network, parts.as_slice()) {
        (Network::Instagram, [kind, code, ..]) if post_kind(kind) => (*code, None),
        (Network::Instagram, [name, kind, code, ..]) if post_kind(kind) && account(name) => {
            (*code, None)
        }
        (Network::Threads, [name, "post", code, ..]) => {
            let name = name.strip_prefix('@')?;
            (*code, Some(username(name)).filter(|u| u == name))
        }
        (Network::Threads, ["t", code, ..]) => (*code, None),
        _ => return None,
    };
    let allowed = |c: char| c.is_ascii_alphanumeric() || c == '_' || c == '-';
    if code.is_empty() || code.len() > MAX_CODE || !code.chars().all(allowed) {
        return None;
    }
    Some(PostLink {
        network,
        code: code.to_string(),
        username,
        url,
    })
}

/// A link card, if its address is a web address.
fn link_card(preview: &meta::LinkPreview) -> Option<LinkCard> {
    let url = messages::web_url(&preview.url)?;
    let host = messages::link_host(&url)?;
    let image = preview.image.as_ref().and_then(|p| {
        (p.file_id > 0 && p.width > 0 && p.height > 0).then_some(Preview {
            file_id: p.file_id,
            width: p.width,
            height: p.height,
            thumbnail: None,
            sticker: false,
        })
    });
    Some(LinkCard {
        url,
        host,
        title: cut(&messages::one_line(&preview.title), MAX_TITLE),
        image,
    })
}

/// A photo or video of a post, as drawn; anything else is left out.
fn item(media: &meta::Media) -> Option<Item> {
    let video = match media.kind {
        MediaKind::Photo => false,
        MediaKind::Video | MediaKind::Gif => true,
        _ => return None,
    };
    let still = |file_id: i32, width: u32, height: u32| {
        (file_id > 0).then_some(Preview {
            file_id,
            // A side the helper didn't know: square, as most posts are.
            width: if width > 0 && height > 0 { width } else { 1080 },
            height: if width > 0 && height > 0 {
                height
            } else {
                1080
            },
            thumbnail: None,
            sticker: false,
        })
    };
    let cover = media
        .thumbnail
        .as_ref()
        .and_then(|t| match (t.width, t.height) {
            (0, _) | (_, 0) => still(t.file_id, media.width, media.height),
            (w, h) => still(t.file_id, w, h),
        });
    let file = (media.file_id > 0).then(|| MediaFile {
        id: media.file_id,
        label: match video {
            true => format!("Video {}", messages::duration(media.duration)),
            false => "Photo".into(),
        },
        photo: !video,
    });
    let still = match video {
        true => cover,
        false => cover.or_else(|| still(media.file_id, media.width, media.height)),
    };
    let audio = (video && file.is_some() && media.audio > 0).then_some(media.audio);
    (still.is_some() || file.is_some()).then_some(Item {
        video,
        still,
        file,
        audio,
        duration: media.duration.max(0),
    })
}

/// A network's home feed, as far as it's loaded.
pub struct Feed {
    pub network: Network,
    pub posts: Vec<Post>,
    ids: HashSet<i64>,
    /// The post the cursor is on, by index.
    pub selected: usize,
    /// The first post drawn, by index; the view keeps it so the selected
    /// one shows.
    pub top: usize,
    /// The view on its way to where `top` puts it.
    glide: Option<Glide>,
    /// The cursor moved since the last frame, little enough for the view to
    /// slide after it.
    may_glide: bool,
    /// Which photo or video of a carousel shows, by post id; the first
    /// when there's none.
    shown: HashMap<i64, usize>,
    /// The load on its way, by number: answers to another are old news.
    pub loading: Option<u64>,
    /// The helper's id for that load, to call it off with.
    pub request: Option<u64>,
    /// That load starts again from the top.
    fresh: bool,
    /// The network has more after the last post.
    pub has_more: bool,
    /// The last page brought nothing new though there's more (Threads
    /// sends empty pages at times): the next is asked for when the cursor
    /// moves again, not at once and again and again.
    stalled: bool,
    /// Pages in a row that brought nothing new, up to `MAX_EMPTY_PAGES`.
    empty_pages: u8,
    /// So many did that the feed was left there, though the network said
    /// there's more.
    gave_up: bool,
    /// Threads: the accounts followed, rather than For you.
    pub following: bool,
    /// Why the last load failed.
    pub error: Option<String>,
    /// The one post a link names, instead of the feed: Esc goes on to the
    /// feed.
    pub linked: Option<PostLink>,
    /// The posts the last frame showed enough of to count as in view
    /// (`ui::threads::draw`), until [`Feed::note_views`] takes them.
    pub drawn: Vec<i64>,
    /// The posts in view, and since when.
    in_view: Vec<(i64, Instant)>,
    /// The posts that left view since the last load, for the next to tell.
    views: Vec<FeedView>,
    /// The posts here are the other feed's, until this one's first page takes
    /// their place: `:for-you` keeps Following's on screen until it comes.
    others_posts: bool,
}

impl Feed {
    pub fn new(network: Network) -> Self {
        Self {
            network,
            posts: Vec::new(),
            ids: HashSet::new(),
            selected: 0,
            top: 0,
            glide: None,
            may_glide: false,
            shown: HashMap::new(),
            loading: None,
            request: None,
            fresh: false,
            has_more: true,
            stalled: false,
            empty_pages: 0,
            gave_up: false,
            following: false,
            error: None,
            linked: None,
            drawn: Vec::new(),
            in_view: Vec::new(),
            views: Vec::new(),
            others_posts: false,
        }
    }

    /// This feed tells its network what's in view: Threads' For you, as
    /// threads.com does, whose pages thin out without it. Nothing else does:
    /// not threads.com's Following or a post's own page, nor Instagram here.
    fn reports_views(&self) -> bool {
        self.network == Network::Threads
            && !self.following
            && self.linked.is_none()
            && !self.others_posts
    }

    /// Brings what's in view up to date after a frame. A post is timed from
    /// when it comes into view to when it leaves it, as on threads.com, so
    /// one still in view isn't told about yet. `watched` is false when nobody
    /// can be looking: nothing is in view then, and `until` is when that
    /// began.
    pub fn note_views(&mut self, watched: bool, until: Instant) {
        let drawn = std::mem::take(&mut self.drawn);
        let shown = match watched && self.reports_views() {
            true => drawn,
            false => Vec::new(),
        };
        let mut left = Vec::new();
        self.in_view.retain(|&(id, since)| {
            let stays = shown.contains(&id);
            if !stays {
                left.push((id, since));
            }
            stays
        });
        for (id, since) in left {
            self.add_view(id, until.saturating_duration_since(since));
        }
        for id in shown {
            if !self.in_view.iter().any(|&(post, _)| post == id) {
                self.in_view.push((id, until));
            }
        }
    }

    /// Keeps how long a post just stayed in view: its newest time, as on
    /// threads.com.
    fn add_view(&mut self, post_id: i64, time: Duration) {
        if time < MIN_VIEW {
            return;
        }
        let ms = u64::try_from(time.as_millis()).unwrap_or(u64::MAX);
        if let Some(view) = self.views.iter_mut().find(|v| v.post_id == post_id) {
            view.ms = ms;
            return;
        }
        if self.views.len() >= MAX_VIEWS {
            self.views.remove(0);
        }
        self.views.push(FeedView { post_id, ms });
    }

    /// What the next load is to tell the network, and it's told only once.
    pub fn take_views(&mut self) -> Vec<FeedView> {
        std::mem::take(&mut self.views)
    }

    /// Going to the other feed: what's in view and what was is forgotten, as
    /// threads.com's page does, and the posts still showing are the other
    /// feed's until a page of this one takes their place.
    pub fn switched(&mut self) {
        self.drawn.clear();
        self.in_view.clear();
        self.views.clear();
        self.others_posts = true;
    }

    /// The post `link` names, alone: loaded with [`Feed::start_load`]
    /// (always fresh) and [`Feed::on_post`], with nothing after it.
    pub fn linked(link: PostLink) -> Self {
        let network = link.network;
        Self {
            has_more: false,
            linked: Some(link),
            ..Self::new(network)
        }
    }

    /// The answer to load `load` of a linked post: it, in the place of what
    /// was there. True if it was the load waited for.
    pub fn on_post(&mut self, load: u64, post: Result<FeedPost, String>) -> bool {
        self.on_page(load, post.map(|post| (vec![post], false)))
    }

    /// Starts a load: the next page, or with `fresh` the newest posts
    /// again. Returns its number to ask the helper with, or `None` for a
    /// next page that isn't wanted: one is on its way, or there's no more.
    /// A fresh load always starts, and the answer to any before it is
    /// dropped.
    pub fn start_load(&mut self, fresh: bool) -> Option<u64> {
        if !fresh && (self.loading.is_some() || !self.has_more) {
            return None;
        }
        // A key held down would queue one after another.
        if fresh && self.loading.is_some() && self.fresh {
            return None;
        }
        let load = next_load();
        self.loading = Some(load);
        self.fresh = fresh;
        self.error = None;
        Some(load)
    }

    /// The load just started went to the helper as `request`. Returns the
    /// one it took the place of, if one was on its way: nobody waits for it
    /// now.
    pub fn sent(&mut self, request: u64) -> Option<u64> {
        self.request.replace(request)
    }

    /// The load on its way is `load`, and starts again from the top.
    pub fn is_fresh(&self, load: u64) -> bool {
        self.loading == Some(load) && self.fresh
    }

    /// The cursor is near enough the end for the next page.
    pub fn wants_more(&self) -> bool {
        self.has_more
            && !self.stalled
            && self.loading.is_none()
            && self.error.is_none()
            && self.selected + MORE_WITHIN >= self.posts.len()
    }
    /// The last page brought nothing new, though the network says there's
    /// more: moving the cursor asks again.
    pub fn stalled(&self) -> bool {
        self.stalled && self.has_more
    }

    /// Pages in a row brought nothing new, so many that the feed was left
    /// there, though the network said there's more.
    pub fn gave_up(&self) -> bool {
        self.gave_up
    }

    /// The answer to load `load`. A fresh one takes the place of what was
    /// there, the cursor back on the first post; a next page goes after it,
    /// without posts already there. A post keeps its line to the one above
    /// only if that's a post of its own thread. True if it was the load waited
    /// for.
    pub fn on_page(&mut self, load: u64, page: Result<(Vec<FeedPost>, bool), String>) -> bool {
        if self.loading != Some(load) {
            return false;
        }
        self.loading = None;
        self.request = None;
        let (posts, more) = match page {
            Ok(page) => page,
            Err(why) => {
                self.error = Some(messages::one_line(&why));
                return true;
            }
        };
        if std::mem::take(&mut self.fresh) {
            self.posts.clear();
            self.ids.clear();
            self.shown.clear();
            self.selected = 0;
            self.top = 0;
            self.glide = None;
            self.may_glide = false;
            self.empty_pages = 0;
            self.others_posts = false;
        }
        let before = self.posts.len();
        // A post dropped as already there can leave the next one under another
        // thread's post, where its line would join two unrelated posts.
        let mut thread = Vec::new();
        for post in &posts {
            if !post.continues {
                thread.clear();
            }
            let goes_on = self
                .posts
                .last()
                .is_some_and(|above| thread.contains(&above.id));
            thread.push(post.id);
            if self.ids.insert(post.id) {
                let mut post = Post::new(post, self.network);
                post.continues = goes_on;
                self.posts.push(post);
            }
        }
        self.stalled = self.posts.len() == before;
        self.empty_pages = if self.stalled {
            self.empty_pages.saturating_add(1)
        } else {
            0
        };
        self.has_more = more && self.empty_pages < MAX_EMPTY_PAGES;
        self.gave_up = more && !self.has_more;
        true
    }

    /// Moves the cursor `delta` posts, within the feed.
    pub fn move_cursor(&mut self, delta: isize) {
        let last = self.posts.len().saturating_sub(1);
        self.selected = self.selected.saturating_add_signed(delta).min(last);
        self.may_glide = delta.unsigned_abs() <= JUMP.unsigned_abs();
        // Moving on after a failed load tries again near the end, and so
        // does moving down after one that brought nothing new.
        if delta > 0 {
            self.ask_again();
        } else {
            self.error = None;
        }
    }

    /// When a frame has put `top` where the view rests now, from `before`:
    /// how many rows off that place to draw the posts this frame, more than
    /// 0 being lower down. It's 0 while the view isn't on its way, and
    /// always with no `now` (see `ui::feed::View::glide_at`). The view sets
    /// out only after a short cursor move that took `top` elsewhere, from
    /// at most [`GLIDE_ROWS`] rows away and no more than `pane`, the rest of
    /// the way at once: a whole pane moving is dizzying, and it makes no
    /// images of all it would go past. `height` is the rows a post takes.
    pub fn glide_rows(
        &mut self,
        before: usize,
        pane: u16,
        now: Option<Instant>,
        height: impl Fn(&Self, usize) -> u16,
    ) -> i32 {
        let moved = std::mem::take(&mut self.may_glide);
        let Some(now) = now else {
            self.glide = None;
            return 0;
        };
        let mut rows = self.glide.as_mut().map_or(0, |g| g.show(now));
        if moved && before != self.top {
            let most = i32::from(pane).min(GLIDE_ROWS);
            let mut between = 0;
            for i in self.top.min(before)..self.top.max(before) {
                between += i32::from(height(self, i));
                if between >= most {
                    break;
                }
            }
            // Pressed again on the way, it goes on from where it got to.
            rows += if self.top > before { between } else { -between };
            rows = rows.clamp(-most, most);
            self.glide = Some(Glide::new(rows, now));
        }
        if rows == 0 {
            self.glide = None;
        }
        rows
    }

    /// The post a frame starts drawing at and the row it starts on, with
    /// the view `rows` off its place: above the pane (less than 0) when its
    /// top is cut off, and with nothing above it left out altogether.
    pub fn first_drawn(&self, rows: i32, height: impl Fn(&Self, usize) -> u16) -> (usize, i32) {
        let (mut i, mut y) = (self.top, rows);
        while y > 0 && i > 0 {
            i -= 1;
            y -= i32::from(height(self, i));
        }
        while y < 0 && i + 1 < self.posts.len() {
            let rows = i32::from(height(self, i));
            if y + rows > 0 {
                break;
            }
            y += rows;
            i += 1;
        }
        (i, y)
    }

    /// The view is sliding, or may set out with the next frame.
    pub fn sliding(&self) -> bool {
        self.may_glide || self.glide.is_some()
    }

    /// When to draw again, while the view slides.
    pub fn glide_frame(&self, now: Instant) -> Option<Instant> {
        self.glide.and_then(|g| g.next_frame(now))
    }

    /// Asks again for what's after the last post, as moving down past it
    /// does: after a failed load, or one that brought nothing new.
    pub fn ask_again(&mut self) {
        self.error = None;
        self.stalled = false;
    }

    pub fn selected_post(&self) -> Option<&Post> {
        self.posts.get(self.selected)
    }

    /// The index of the photo or video of `post` that shows.
    pub fn shown_index(&self, post: &Post) -> usize {
        self.shown
            .get(&post.id)
            .copied()
            .unwrap_or(0)
            .min(post.media.len().saturating_sub(1))
    }

    /// The photo or video of the selected post that shows.
    pub fn shown_item(&self) -> Option<(&Post, &Item)> {
        let post = self.selected_post()?;
        Some((post, post.media.get(self.shown_index(post))?))
    }

    /// Shows the photo or video `step` away in the selected post's
    /// carousel. False if there's none that way.
    pub fn step_item(&mut self, step: isize) -> bool {
        let Some(post) = self.selected_post() else {
            return false;
        };
        let (id, at, count) = (post.id, self.shown_index(post), post.media.len());
        match at.checked_add_signed(step).filter(|&i| i < count) {
            Some(next) => {
                self.shown.insert(id, next);
                true
            }
            None => false,
        }
    }

    /// The photo after (or before) the one in post `post_id` whose file is
    /// `file_id`, through the carousels and then the posts loaded, for the
    /// photo viewer's `l` and `h`. The cursor goes to its post, and its
    /// carousel to it.
    pub fn next_photo(&mut self, post_id: i64, file_id: i32, newer: bool) -> Option<(i64, Item)> {
        let photos: Vec<(usize, usize)> = self
            .posts
            .iter()
            .enumerate()
            .flat_map(|(p, post)| {
                post.media
                    .iter()
                    .enumerate()
                    .filter(|(_, item)| !item.video && item.file.is_some())
                    .map(move |(i, _)| (p, i))
            })
            .collect();
        let at = photos.iter().position(|&(p, i)| {
            let post = &self.posts[p];
            post.id == post_id && post.media[i].file.as_ref().is_some_and(|f| f.id == file_id)
        })?;
        let next = match newer {
            true => photos.get(at + 1)?,
            false => photos.get(at.checked_sub(1)?)?,
        };
        let (p, i) = *next;
        self.selected = p;
        let post = &self.posts[p];
        self.shown.insert(post.id, i);
        Some((post.id, post.media[i].clone()))
    }
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;

    fn feed_of(posts: i64) -> Feed {
        let mut feed = Feed::new(Network::Instagram);
        let load = feed.start_load(false).unwrap();
        let posts = (1..=posts).map(|id| post(id, serde_json::json!({})));
        feed.on_page(load, Ok((posts.collect(), false)));
        feed
    }

    fn ten_rows(_: &Feed, _: usize) -> u16 {
        10
    }

    #[test]
    fn a_short_move_eases_the_view_the_rows_it_went_and_from_glide_rows_away_at_most() {
        use crate::motion::GLIDE;
        let mut feed = feed_of(8);
        let now = Instant::now();
        feed.move_cursor(1);
        feed.top = 1;
        assert_eq!(feed.glide_rows(0, 30, Some(now), ten_rows), 10);
        assert_eq!(feed.first_drawn(10, ten_rows), (0, 0), "where it was");
        assert!(feed.glide_frame(now).is_some());
        assert!(
            feed.glide_frame(now + GLIDE * 2).is_some(),
            "drawn rows off: one more frame shows it there"
        );

        let now = now + GLIDE;
        feed.move_cursor(-1);
        feed.top = 0;
        assert_eq!(feed.glide_rows(1, 30, Some(now), ten_rows), -10, "back up");
        assert_eq!(feed.first_drawn(-10, ten_rows), (1, 0), "where it was");

        let now = now + GLIDE;
        feed.move_cursor(JUMP);
        feed.top = 3;
        assert_eq!(
            feed.glide_rows(0, 30, Some(now), ten_rows),
            GLIDE_ROWS,
            "the rest of the way at once"
        );

        let now = now + GLIDE;
        feed.move_cursor(1);
        feed.top = 4;
        assert_eq!(
            feed.glide_rows(3, 8, Some(now), ten_rows),
            8,
            "no more than the pane"
        );
    }

    #[test]
    fn a_frame_starts_at_the_first_post_any_of_which_is_in_the_pane() {
        let mut feed = feed_of(8);
        feed.top = 3;
        assert_eq!(feed.first_drawn(0, ten_rows), (3, 0), "at rest");
        assert_eq!(feed.first_drawn(15, ten_rows), (1, -5), "the post cut off");
        assert_eq!(feed.first_drawn(-25, ten_rows), (5, -5));
        feed.top = 0;
        assert_eq!(feed.first_drawn(5, ten_rows), (0, 5), "nothing above");
    }

    #[test]
    fn pressed_again_on_the_way_the_view_goes_on_from_where_it_got_to() {
        use crate::motion::GLIDE;
        let mut feed = feed_of(8);
        let now = Instant::now();
        feed.move_cursor(1);
        feed.top = 1;
        assert_eq!(feed.glide_rows(0, 30, Some(now), ten_rows), 10);
        let halfway = now + GLIDE / 2;
        feed.move_cursor(1);
        feed.top = 2;
        assert_eq!(feed.glide_rows(1, 30, Some(halfway), ten_rows), 11);
        assert_eq!(
            feed.glide_rows(2, 30, Some(halfway), ten_rows),
            11,
            "a frame with no key press goes on as it was"
        );
    }

    #[test]
    fn the_view_goes_at_once_after_a_jump_a_new_page_or_with_no_time_to_slide_in() {
        let mut feed = feed_of(8);
        let now = Instant::now();
        feed.move_cursor(isize::MAX);
        feed.top = 7;
        assert_eq!(feed.glide_rows(0, 30, Some(now), ten_rows), 0, "G");
        assert_eq!(feed.glide_frame(now), None);

        feed.top = 6;
        assert_eq!(
            feed.glide_rows(7, 30, Some(now), ten_rows),
            0,
            "no key took it there: the window changed"
        );

        feed.move_cursor(-1);
        feed.top = 5;
        assert_eq!(feed.glide_rows(6, 30, None, ten_rows), 0);

        feed.move_cursor(-1);
        feed.top = 4;
        assert_ne!(feed.glide_rows(5, 30, Some(now), ten_rows), 0);
        assert_eq!(
            feed.glide_rows(4, 30, None, ten_rows),
            0,
            "a popup stops it where it would rest"
        );
        assert_eq!(feed.glide_frame(now), None);

        feed.move_cursor(-1);
        feed.top = 3;
        assert_ne!(feed.glide_rows(4, 30, Some(now), ten_rows), 0);
        let load = feed.start_load(true).unwrap();
        feed.on_page(load, Ok((vec![post(9, serde_json::json!({}))], false)));
        assert_eq!(feed.glide_frame(now), None, "its rows were of posts gone");
    }

    /// A post as the helper sends it: a photo by Alice, with `fields`
    /// instead of what it has.
    pub(crate) fn post(id: i64, fields: serde_json::Value) -> FeedPost {
        let mut post = serde_json::json!({
            "id": id,
            "author": {"name": "Alice Example", "username": "alice.example"},
            "date": 1_759_900_000,
            "caption": "sunset",
            "media": [{"kind": "photo", "file_id": id as i32 * 10, "width": 1080, "height": 1350,
                        "thumbnail": {"file_id": id as i32 * 10, "width": 1080, "height": 1350}}],
            "likes": 12,
            "comments": 3,
            "url": "https://www.instagram.com/p/ABC/"
        });
        if let (Some(post), Some(fields)) = (post.as_object_mut(), fields.as_object()) {
            for (key, value) in fields {
                post.insert(key.clone(), value.clone());
            }
        }
        serde_json::from_value(post).unwrap()
    }

    #[test]
    fn what_the_author_wrote_is_cleaned_and_only_instagram_links_are_kept() {
        let p = Post::from(&post(
            1,
            serde_json::json!({
                "author": {"name": "Al\u{202E}ice\nExample", "username": "\u{430}lice"},
                "caption": "line one\u{1b}[2J\nline two\n",
                "url": "https://evil.example/p/ABC/",
                "likes": -5
            }),
        ));
        assert_eq!(p.name, "Alice Example");
        assert_eq!(
            p.username, "",
            "a Cyrillic letter can't pass for a Latin one"
        );
        assert_eq!(p.caption, "line one[2J\nline two");
        assert_eq!(p.url, None);
        assert_eq!(p.likes, Some(0));
        let p = Post::from(&post(
            2,
            serde_json::json!({"author": {"username": "@sam_climbs.2"}}),
        ));
        assert_eq!(
            (p.author(), p.username.as_str()),
            ("sam_climbs.2", "sam_climbs.2")
        );
        assert_eq!(p.url.as_deref(), Some("https://www.instagram.com/p/ABC/"));
        for url in [
            "http://www.instagram.com/p/A/",
            "https://www.instagram.com.evil.example/p/A/",
        ] {
            assert_eq!(post_url(url, Network::Instagram), None, "{url}");
        }
        let long = "x".repeat(MAX_CAPTION + 50);
        let p = Post::from(&post(3, serde_json::json!({"caption": long})));
        assert_eq!(p.caption.chars().count(), MAX_CAPTION + 1);
        assert!(p.caption.ends_with('…'));
    }

    #[test]
    fn a_threads_post_keeps_threads_links_and_its_quote_card_and_line_are_made_safe() {
        let p = Post::new(
            &post(
                1,
                serde_json::json!({
                    "media": [],
                    "url": "https://www.threads.com/@alice.example/post/ABC",
                    "context": "sam\u{202E}reposted\nand more",
                    "continues": true,
                    "reposts": -3, "shares": 18,
                    "author": {"username": "alice.example",
                               "photo": {"file_id": 40, "width": 150, "height": 150}},
                    "quoted": {"id": 0, "author": {"name": "Sam\u{1b}[2J", "username": "sam"},
                               "caption": "quoted", "media": [], "url": "https://evil.example/",
                               "quoted": {"id": 0, "author": {}, "caption": "deeper"},
                               "context": "no"},
                    "link_preview": {"url": "https://example.com/a?b=c", "title": "A\npage",
                                     "image": {"file_id": 41, "width": 600, "height": 315}}
                }),
            ),
            Network::Threads,
        );
        assert!(p.media.is_empty(), "text alone");
        assert_eq!(
            p.url.as_deref(),
            Some("https://www.threads.com/@alice.example/post/ABC")
        );
        assert_eq!(p.context.as_deref(), Some("samreposted and more"));
        assert!(p.continues);
        assert_eq!((p.reposts, p.shares), (Some(0), Some(18)));
        assert_eq!(p.avatar.as_ref().map(|a| a.file_id), Some(40));
        let quoted = p.quoted.as_ref().expect("the quote");
        assert_eq!(quoted.name, "Sam[2J");
        assert_eq!(quoted.url, None, "only threads.com");
        assert!(quoted.quoted.is_none() && quoted.context.is_none());
        let card = p.link.as_ref().expect("the card");
        assert_eq!(
            (card.host.as_str(), card.title.as_str()),
            ("example.com", "A page")
        );
        assert_eq!(card.image.as_ref().map(|i| i.file_id), Some(41));
        // Instagram's addresses aren't Threads', nor the other way round.
        for (url, network) in [
            ("https://www.instagram.com/p/A/", Network::Threads),
            ("https://www.threads.com/@a/post/A", Network::Instagram),
            ("http://www.threads.com/@a/post/A", Network::Threads),
            (
                "https://www.threads.com.evil.example/@a/post/A",
                Network::Threads,
            ),
        ] {
            assert_eq!(post_url(url, network), None, "{url}");
        }
        // A card that isn't a web address isn't kept.
        let p = Post::new(
            &post(
                2,
                serde_json::json!({"link_preview": {"url": "javascript:alert(1)", "title": "x"}}),
            ),
            Network::Threads,
        );
        assert!(p.link.is_none());
    }

    #[test]
    fn a_video_shows_its_cover_and_plays_its_file_and_other_media_is_left_out() {
        let p = Post::from(&post(
            1,
            serde_json::json!({"media": [
                {"kind": "video", "file_id": 7, "duration": 15, "width": 720, "height": 1280,
                 "mime": "video/mp4", "thumbnail": {"file_id": 8, "width": 1080, "height": 1920}},
                {"kind": "photo", "file_id": 9},
                {"kind": "sticker", "file_id": 10},
                {"kind": "video", "file_id": 11},
                {"nonsense": true},
                {"kind": "video", "file_id": 12, "audio": 13},
                {"kind": "video", "file_id": 0, "audio": 15,
                 "thumbnail": {"file_id": 14, "width": 1080, "height": 1920}}
            ]}),
        ));
        assert_eq!(p.media.len(), 5);
        assert_eq!(p.media[0].audio, None, "its sound is in its file");
        assert_eq!(p.media[3].audio, Some(13), "its sound apart");
        // A video that can't be had still shows its cover, and nothing plays.
        let unplayable = &p.media[4];
        assert!(unplayable.file.is_none() && unplayable.audio.is_none());
        assert_eq!(unplayable.still.as_ref().map(|s| s.file_id), Some(14));
        let video = &p.media[0];
        assert!(video.video);
        assert_eq!(
            video.still.as_ref().map(|s| (s.file_id, s.height)),
            Some((8, 1920))
        );
        assert_eq!(
            video.file.as_ref().map(|f| (f.id, f.label.as_str())),
            Some((7, "Video 0:15"))
        );
        let photo = &p.media[1];
        assert_eq!(
            photo.still.as_ref().map(|s| (s.file_id, s.width)),
            Some((9, 1080))
        );
        assert!(photo.file.as_ref().is_some_and(|f| f.photo));
        // A video with no cover still plays.
        assert!(p.media[2].still.is_none());
    }

    #[test]
    fn pages_go_after_each_other_without_repeats_and_a_fresh_load_starts_over() {
        let mut feed = Feed::new(Network::Instagram);
        let first = feed.start_load(false).unwrap();
        assert_eq!(feed.start_load(false), None, "one at a time");
        assert!(feed.on_page(
            first,
            Ok((
                (1..=7).map(|i| post(i, serde_json::json!({}))).collect(),
                true
            ))
        ));
        assert_eq!(feed.posts.len(), 7);
        feed.move_cursor(1);
        assert!(!feed.wants_more());
        feed.move_cursor(1);
        assert!(feed.wants_more(), "within {MORE_WITHIN} of the end");
        let second = feed.start_load(false).unwrap();
        feed.on_page(
            second,
            Ok((
                (6..=10).map(|i| post(i, serde_json::json!({}))).collect(),
                true,
            )),
        );
        let ids: Vec<i64> = feed.posts.iter().map(|p| p.id).collect();
        assert_eq!(ids, [1, 2, 3, 4, 5, 6, 7, 8, 9, 10]);
        // A fresh load drops the answer of the one before it.
        let stale = feed.start_load(false).unwrap();
        let fresh = feed.start_load(true).unwrap();
        assert!(feed.is_fresh(fresh) && !feed.is_fresh(stale));
        assert_eq!(feed.start_load(true), None, "one fresh load at a time");
        assert!(!feed.on_page(stale, Ok((vec![post(99, serde_json::json!({}))], true))));
        feed.on_page(
            fresh,
            Ok((
                vec![
                    post(20, serde_json::json!({})),
                    post(1, serde_json::json!({})),
                ],
                false,
            )),
        );
        let ids: Vec<i64> = feed.posts.iter().map(|p| p.id).collect();
        assert_eq!(ids, [20, 1]);
        assert_eq!(feed.selected, 0);
        assert!(!feed.has_more && !feed.wants_more());
        assert_eq!(feed.start_load(false), None, "nothing after the end");
    }

    #[test]
    fn a_post_is_told_about_once_it_left_view_with_how_long_it_stayed() {
        let mut feed = Feed::new(Network::Threads);
        let start = Instant::now();
        let at = |ms: u64| start + Duration::from_millis(ms);
        for (drawn, ms) in [
            (vec![1, 2], 0),
            (vec![2, 3], 1800),
            (vec![3], 1900),
            // Post 3 came and went too fast to count.
            (vec![], 2000),
        ] {
            feed.drawn = drawn;
            feed.note_views(true, at(ms));
        }
        assert_eq!(
            feed.take_views(),
            [
                FeedView {
                    post_id: 1,
                    ms: 1800
                },
                FeedView {
                    post_id: 2,
                    ms: 1900
                },
            ]
        );
        assert!(feed.take_views().is_empty(), "told once");
        // In view again, and the newest time goes out; one still in view waits.
        feed.drawn = vec![4];
        feed.note_views(true, at(3000));
        feed.drawn = vec![1, 4];
        feed.note_views(true, at(4000));
        assert!(feed.take_views().is_empty());
        feed.drawn = vec![4];
        feed.note_views(true, at(4500));
        assert_eq!(
            feed.take_views(),
            [FeedView {
                post_id: 1,
                ms: 500
            }]
        );
    }

    #[test]
    fn nobody_watching_ends_a_posts_time_in_view_when_they_stopped() {
        let mut feed = Feed::new(Network::Threads);
        let start = Instant::now();
        feed.drawn = vec![1];
        feed.note_views(true, start);
        // Away since a second in, seen a long while later.
        feed.drawn = vec![1];
        feed.note_views(false, start + Duration::from_secs(1));
        assert_eq!(
            feed.take_views(),
            [FeedView {
                post_id: 1,
                ms: 1000
            }]
        );
        // And nothing's timed until someone's back.
        feed.drawn = vec![1];
        feed.note_views(false, start + Duration::from_secs(50));
        feed.drawn = vec![];
        feed.note_views(true, start + Duration::from_secs(60));
        assert!(feed.take_views().is_empty());
    }

    #[test]
    fn only_threads_for_you_tells_what_was_in_view() {
        let linked = PostLink {
            network: Network::Threads,
            code: "ABC".into(),
            username: None,
            url: "https://www.threads.com/post/ABC".into(),
        };
        let mut instagram = Feed::new(Network::Instagram);
        let mut following = Feed::new(Network::Threads);
        following.following = true;
        let mut post = Feed::linked(linked);
        let start = Instant::now();
        for feed in [&mut instagram, &mut following, &mut post] {
            feed.drawn = vec![1];
            feed.note_views(true, start);
            feed.note_views(true, start + Duration::from_secs(5));
            assert!(feed.take_views().is_empty());
        }
    }

    #[test]
    fn the_posts_kept_to_tell_about_are_bounded() {
        let mut feed = Feed::new(Network::Threads);
        let start = Instant::now();
        for id in 0..(MAX_VIEWS as i64 + 3) {
            feed.add_view(id, Duration::from_secs(1));
        }
        let views = feed.take_views();
        assert_eq!(views.len(), MAX_VIEWS);
        assert_eq!(views[0].post_id, 3, "the oldest go first");
        feed.drawn = vec![9];
        feed.note_views(true, start);
        feed.add_view(1, Duration::from_secs(1));
        feed.switched();
        feed.note_views(true, start + Duration::from_secs(9));
        assert!(feed.take_views().is_empty(), "forgotten with the feed");
    }

    #[test]
    fn a_post_keeps_its_line_only_under_a_post_of_its_own_thread() {
        let mut feed = Feed::new(Network::Threads);
        let head = |id| post(id, serde_json::json!({}));
        let next = |id| post(id, serde_json::json!({"continues": true}));
        for page in [
            vec![head(1), head(2)],
            // Post 1 again with replies: what's above them is post 2, another
            // thread's.
            vec![head(1), next(3), next(4)],
            // Again with one more: what's above it is post 4, of its thread.
            vec![head(1), next(3), next(4), next(5)],
            // One already there in the middle of a thread.
            vec![head(6), next(3), next(7)],
            // A post alone, then the first of a thread, in one page.
            vec![head(8), head(8), next(9)],
        ] {
            let load = feed.start_load(false).unwrap();
            feed.on_page(load, Ok((page, true)));
        }
        let lines: Vec<(i64, bool)> = feed.posts.iter().map(|p| (p.id, p.continues)).collect();
        assert_eq!(
            lines,
            [
                (1, false),
                (2, false),
                (3, false),
                (4, true),
                (5, true),
                (6, false),
                (7, true),
                (8, false),
                (9, true),
            ]
        );
    }

    #[test]
    fn a_page_with_nothing_new_waits_for_the_cursor_to_move_and_isnt_the_end() {
        let mut feed = Feed::new(Network::Threads);
        let load = feed.start_load(false).unwrap();
        feed.on_page(load, Ok((vec![post(1, serde_json::json!({}))], true)));
        assert!(feed.wants_more() && !feed.stalled());
        // Empty, and then only what's there already: more, but not at once,
        // nor when the cursor goes up; going down, or the viewer going on
        // past the last photo, asks again.
        for page in [vec![], vec![post(1, serde_json::json!({}))]] {
            let load = feed.start_load(false).unwrap();
            feed.on_page(load, Ok((page, true)));
            assert!(feed.has_more && feed.stalled() && !feed.wants_more());
            feed.move_cursor(-1);
            assert!(feed.stalled() && !feed.wants_more());
            feed.move_cursor(1);
            assert!(feed.wants_more() && !feed.stalled());
        }
        // Something new, and the count of pages without starts over.
        let load = feed.start_load(false).unwrap();
        feed.on_page(load, Ok((vec![post(2, serde_json::json!({}))], true)));
        assert_eq!(feed.posts.len(), 2);
        assert!(feed.wants_more() && !feed.stalled());
    }

    #[test]
    fn so_many_pages_in_a_row_with_nothing_new_end_the_feed() {
        let mut feed = Feed::new(Network::Threads);
        for n in 1..=MAX_EMPTY_PAGES {
            let load = feed.start_load(false).unwrap();
            feed.on_page(load, Ok((vec![], true)));
            assert_eq!(feed.has_more, n < MAX_EMPTY_PAGES);
            feed.ask_again();
        }
        assert!(!feed.wants_more() && !feed.stalled() && feed.gave_up());
        assert_eq!(feed.start_load(false), None, "nothing after the end");
        // Loaded again from the top, it counts again.
        let load = feed.start_load(true).unwrap();
        feed.on_page(load, Ok((vec![], true)));
        assert!(feed.has_more && feed.stalled() && !feed.gave_up());
    }

    #[test]
    fn a_failed_load_says_why_and_moving_on_tries_again() {
        let mut feed = Feed::new(Network::Instagram);
        let load = feed.start_load(false).unwrap();
        feed.on_page(load, Err("Instagram\nwants you to log in again.".into()));
        assert_eq!(
            feed.error.as_deref(),
            Some("Instagram wants you to log in again.")
        );
        assert!(!feed.wants_more());
        feed.move_cursor(1);
        assert!(feed.wants_more());
    }

    #[test]
    fn a_carousel_steps_through_its_photos_and_the_viewer_through_every_photo() {
        let mut feed = Feed::new(Network::Instagram);
        let load = feed.start_load(false).unwrap();
        let carousel = post(
            1,
            serde_json::json!({"media": [
                {"kind": "photo", "file_id": 11}, {"kind": "video", "file_id": 12}, {"kind": "photo", "file_id": 13}
            ]}),
        );
        feed.on_page(
            load,
            Ok((vec![carousel, post(2, serde_json::json!({}))], false)),
        );
        assert!(!feed.step_item(-1));
        assert!(feed.step_item(1));
        assert!(feed.shown_item().unwrap().1.video);
        assert!(feed.step_item(1));
        assert!(!feed.step_item(1));
        // The viewer skips the video, and goes on to the next post.
        let (post_id, item) = feed.next_photo(1, 11, true).unwrap();
        assert_eq!((post_id, item.file.unwrap().id), (1, 13));
        let (post_id, item) = feed.next_photo(1, 13, true).unwrap();
        assert_eq!((post_id, item.file.unwrap().id), (2, 20));
        assert_eq!(feed.selected, 1);
        assert!(feed.next_photo(2, 20, true).is_none());
        let (post_id, _) = feed.next_photo(2, 20, false).unwrap();
        assert_eq!((post_id, feed.selected), (1, 0));
        assert_eq!(feed.shown_item().unwrap().1.file.as_ref().unwrap().id, 13);
    }

    #[test]
    fn a_link_to_an_instagram_or_threads_post_names_the_post() {
        let link = |url: &str| post_link(url).map(|l| (l.network, l.code, l.username));
        let ig = |code: &str| Some((Network::Instagram, code.to_string(), None));
        for (url, code) in [
            ("https://www.instagram.com/p/DAbc_12-x/", "DAbc_12-x"),
            ("https://instagram.com/reel/DAbc12?igsh=MTc4", "DAbc12"),
            ("http://m.instagram.com/reels/DAbc12/", "DAbc12"),
            ("https://www.instagram.com/tv/DAbc12#top", "DAbc12"),
            ("https://www.instagram.com/maya.lens/p/DAbc12/", "DAbc12"),
            (
                "https://www.instagram.com/p/DAbc12/c/17900000000000001/",
                "DAbc12",
            ),
            ("HTTPS://WWW.INSTAGRAM.COM/p/DAbc12/", "DAbc12"),
            ("www.instagram.com/p/DAbc12", "DAbc12"),
        ] {
            assert_eq!(link(url), ig(code), "{url}");
        }
        assert_eq!(
            link("https://www.threads.com/@maya.lens/post/C8xYz-1"),
            Some((Network::Threads, "C8xYz-1".into(), Some("maya.lens".into())))
        );
        assert_eq!(
            link("https://threads.net/t/C8xYz?xmt=1"),
            Some((Network::Threads, "C8xYz".into(), None))
        );
        // A name that isn't one is left out; the code still names the post.
        assert_eq!(
            link("https://www.threads.com/@ma%79a/post/C8xYz"),
            Some((Network::Threads, "C8xYz".into(), None))
        );
        assert_eq!(
            post_link("https://www.instagram.com/p/DAbc12/?igsh=1").map(|l| l.url),
            Some("https://www.instagram.com/p/DAbc12/?igsh=1".into()),
            "the link itself, for o"
        );
        for url in [
            // Not the site itself, or something before its host.
            "https://instagram.com.evil.example/p/DAbc12/",
            "https://evil.example/www.instagram.com/p/DAbc12/",
            "https://instagram.com@evil.example/p/DAbc12/",
            "https://evil.example@www.instagram.com/p/DAbc12/",
            "https://www.instagram.com:8443/p/DAbc12/",
            "https://l.instagram.com/?u=https%3A%2F%2Fwww.instagram.com%2Fp%2FDAbc12",
            "https://www.facebook.com/p/DAbc12/",
            "javascript:alert(1)//www.instagram.com/p/DAbc12/",
            // Not a post's page.
            "https://www.instagram.com/maya.lens/",
            "https://www.instagram.com/stories/maya.lens/3400000000000000001/",
            "https://www.instagram.com/share/p/BAxyzShare/",
            "https://www.instagram.com/explore/p/DAbc12/",
            "https://www.threads.com/@maya.lens",
            "https://www.threads.com/maya.lens/post/C8xYz",
            "https://www.instagram.com/@maya.lens/post/C8xYz",
            // Not a code.
            "https://www.instagram.com/p/DA%62c/",
            "https://www.instagram.com/p/DA.bc/",
            "https://www.instagram.com/p/",
        ] {
            assert_eq!(link(url), None, "{url}");
        }
        let long = format!("https://www.instagram.com/p/{}/", "a".repeat(MAX_CODE + 1));
        assert_eq!(link(&long), None);
    }

    #[test]
    fn a_linked_post_is_the_feed_of_one_post() {
        let link = post_link("https://www.threads.com/@alice.example/post/ABC").unwrap();
        let mut feed = Feed::linked(link.clone());
        assert_eq!(
            (feed.network, feed.linked.as_ref()),
            (Network::Threads, Some(&link))
        );
        assert!(!feed.wants_more(), "nothing after it");
        let load = feed.start_load(true).unwrap();
        assert!(!feed.on_post(load + 1, Err("old news".into())));
        assert!(feed.on_post(load, Ok(post(4, serde_json::json!({})))));
        assert_eq!(feed.posts.len(), 1);
        assert!(!feed.has_more && !feed.wants_more());
        // Asked for again, it takes the place of what was there.
        let load = feed.start_load(true).unwrap();
        assert!(feed.on_post(load, Ok(post(4, serde_json::json!({"caption": "edited"})))));
        assert_eq!(
            feed.posts
                .iter()
                .map(|p| p.caption.as_str())
                .collect::<Vec<_>>(),
            ["edited"]
        );
        let load = feed.start_load(true).unwrap();
        assert!(feed.on_post(load, Err("gone".into())));
        assert_eq!((feed.posts.len(), feed.error.as_deref()), (1, Some("gone")));
    }

    #[test]
    fn a_feed_made_again_doesnt_take_an_answer_meant_for_the_last_one() {
        let mut old = Feed::linked(post_link("https://www.instagram.com/p/FIRST1/").unwrap());
        let late = old.start_load(true).unwrap();
        let mut new = Feed::linked(post_link("https://www.instagram.com/p/NEXT2/").unwrap());
        let load = new.start_load(true).unwrap();
        assert_ne!(late, load);
        assert!(!new.on_post(late, Ok(post(1, serde_json::json!({})))));
        assert!(new.posts.is_empty() && new.loading == Some(load));
    }
}
