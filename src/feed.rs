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

use crate::messages::{self, MediaFile, Preview};
use crate::meta::{self, FeedPost, MediaKind, Network};
use crate::text;

/// Posts asked for at a time.
pub const PAGE: i32 = 10;
/// Posts after the selected one kept ready: within this many of the last
/// one loaded, the next page is asked for, and their photos download before
/// they're in view (`ui::feed::draw`), so moving on doesn't wait.
pub const AHEAD: usize = 5;
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
    /// Which photo or video of a carousel shows, by post id; the first
    /// when there's none.
    shown: HashMap<i64, usize>,
    /// The load on its way, by number: answers to another are old news.
    pub loading: Option<u64>,
    /// That load starts again from the top.
    fresh: bool,
    /// Counts loads, to number them.
    loads: u64,
    /// The network has more after the last post.
    pub has_more: bool,
    /// Threads: the accounts followed, rather than For you.
    pub following: bool,
    /// Why the last load failed.
    pub error: Option<String>,
}

impl Feed {
    pub fn new(network: Network) -> Self {
        Self {
            network,
            posts: Vec::new(),
            ids: HashSet::new(),
            selected: 0,
            top: 0,
            shown: HashMap::new(),
            loading: None,
            fresh: false,
            loads: 0,
            has_more: true,
            following: false,
            error: None,
        }
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
        self.loads += 1;
        self.loading = Some(self.loads);
        self.fresh = fresh;
        self.error = None;
        Some(self.loads)
    }

    /// The load on its way is `load`, and starts again from the top.
    pub fn is_fresh(&self, load: u64) -> bool {
        self.loading == Some(load) && self.fresh
    }

    /// The cursor is near enough the end for the next page.
    pub fn wants_more(&self) -> bool {
        self.has_more
            && self.loading.is_none()
            && self.error.is_none()
            && self.selected + AHEAD >= self.posts.len()
    }

    /// The answer to load `load`. A fresh one takes the place of what was
    /// there, the cursor back on the first post; a next page goes after it,
    /// without posts already there. True if it was the load waited for.
    pub fn on_page(&mut self, load: u64, page: Result<(Vec<FeedPost>, bool), String>) -> bool {
        if self.loading != Some(load) {
            return false;
        }
        self.loading = None;
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
        }
        for post in &posts {
            if self.ids.insert(post.id) {
                self.posts.push(Post::new(post, self.network));
            }
        }
        // A page that brought nothing new would be asked for again and
        // again.
        self.has_more = more && !posts.is_empty();
        true
    }

    /// Moves the cursor `delta` posts, within the feed.
    pub fn move_cursor(&mut self, delta: isize) {
        let last = self.posts.len().saturating_sub(1);
        self.selected = self.selected.saturating_add_signed(delta).min(last);
        // Moving on after a failed load tries again near the end.
        self.error = None;
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
        assert!(feed.wants_more(), "within {AHEAD} of the end");
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
}
