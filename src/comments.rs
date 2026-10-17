//! The comments on a post of the feed, in a pane beside it (`c`): the top
//! comments a page at a time as the cursor nears the end, and a comment's
//! replies under it, indented, once Enter asks for them. On Threads they're
//! the post's replies, and a reply's replies. Only read: nothing here likes
//! or answers.
//!
//! What people wrote is untrusted like a caption: names and text go through
//! [`text::clean`], and a username keeps only the letters Instagram allows
//! ([`crate::feed::username`]).

use std::collections::{HashMap, HashSet};

use crate::feed;
use crate::messages;
use crate::meta::CommentInfo;
use crate::text;

/// Comments asked for again when the cursor is this close to the last one.
const LOAD_AHEAD: usize = 3;
/// A comment is cut after this many characters, what Instagram allows.
const MAX_TEXT: usize = 2200;

/// A comment, made safe to show.
#[derive(Clone)]
pub struct Comment {
    pub id: i64,
    /// One line; may be empty.
    pub name: String,
    /// Without the `@`; empty if it had letters Instagram doesn't allow.
    pub username: String,
    pub verified: bool,
    /// Unix seconds.
    pub date: i64,
    /// Line breaks kept.
    pub text: String,
    pub likes: Option<i64>,
    /// Replies it has.
    pub replies: i64,
    /// The comment it answers, for a reply.
    pub reply_to: Option<i64>,
    /// What a Threads reply holds besides its text, as a label ("Photo").
    pub attachment: Option<&'static str>,
}

impl Comment {
    /// Who wrote it, as its header names them: their username, as
    /// Instagram shows comments, else their name.
    pub fn author(&self) -> &str {
        if self.username.is_empty() {
            &self.name
        } else {
            &self.username
        }
    }
}

impl From<&CommentInfo> for Comment {
    fn from(comment: &CommentInfo) -> Self {
        let text = text::clean(&comment.text);
        let text = match text::first_chars(&text, MAX_TEXT) {
            start if start.len() < text.len() => format!("{start}…"),
            start => start.to_string(),
        };
        Self {
            id: comment.id,
            name: messages::one_line(&comment.author.name),
            username: feed::username(&comment.author.username),
            verified: comment.author.verified,
            date: comment.date,
            text: text.trim_end().to_string(),
            likes: comment.likes.map(|n| n.max(0)),
            replies: comment.replies.max(0),
            reply_to: comment.reply_to,
            attachment: comment.attachment.as_deref().and_then(attachment),
        }
    }
}

/// The label of a Threads reply's attachment, if it's one the protocol
/// names.
fn attachment(kind: &str) -> Option<&'static str> {
    match kind {
        "photo" => Some("Photo"),
        "video" => Some("Video"),
        "carousel" => Some("Photos and videos"),
        _ => None,
    }
}

/// What Enter on a comment does with its replies.
#[derive(Debug, PartialEq, Eq)]
pub enum Replies {
    /// Ask for a page of comment `of`'s replies: the first, or with `more`
    /// the next.
    Load { of: i64, more: bool },
    /// They were shown, and now aren't.
    Hidden,
    /// It has none, or one is already on its way.
    Nothing,
}

/// The comments pane beside a post.
pub struct Comments {
    pub post_id: i64,
    /// Whose post it is, for the pane's title.
    pub author: String,
    /// What the pane calls them: comments, or Threads' replies.
    pub what: &'static str,
    /// Top comments in order, each followed by its replies when shown.
    pub rows: Vec<Comment>,
    ids: HashSet<i64>,
    /// The row the cursor is on.
    pub selected: usize,
    /// The first row drawn; the view keeps it so the selected one shows.
    pub top: usize,
    /// The load on its way: its number, and the comment whose replies it
    /// is. Answers to any other are old news.
    pub loading: Option<(u64, Option<i64>)>,
    loads: u64,
    /// The network has more top comments after the last one.
    pub has_more: bool,
    /// Comments whose replies are shown, and whether more of them come.
    pub open: HashMap<i64, bool>,
    /// Why the last load failed.
    pub error: Option<String>,
}

impl Comments {
    pub fn new(post_id: i64, author: &str) -> Self {
        Self {
            post_id,
            author: author.to_string(),
            what: "Comments",
            rows: Vec::new(),
            ids: HashSet::new(),
            selected: 0,
            top: 0,
            loading: None,
            loads: 0,
            has_more: true,
            open: HashMap::new(),
            error: None,
        }
    }

    /// Starts a load of top comments (`replies_of` `None`) or of a
    /// comment's replies, the first page or with `more` the next. Its
    /// number to ask the helper with, or `None` while another is on its
    /// way: one at a time, as the helper takes them.
    pub fn start_load(&mut self, replies_of: Option<i64>, more: bool) -> Option<u64> {
        if self.loading.is_some() || (replies_of.is_none() && more && !self.has_more) {
            return None;
        }
        self.loads += 1;
        self.loading = Some((self.loads, replies_of));
        self.error = None;
        Some(self.loads)
    }

    /// More top comments are wanted: the cursor is near the last.
    pub fn wants_more(&self) -> bool {
        self.has_more
            && !self.rows.is_empty()
            && self.loading.is_none()
            && self.error.is_none()
            && self.selected + LOAD_AHEAD >= self.rows.len()
    }

    /// The answer to load `load`. Top comments go after the last; replies
    /// after their comment and the replies already under it. Comments
    /// already shown aren't again. True if it was the load waited for.
    pub fn on_page(&mut self, load: u64, page: Result<(Vec<CommentInfo>, bool), String>) -> bool {
        let Some((_, replies_of)) = self.loading.filter(|(n, _)| *n == load) else {
            return false;
        };
        self.loading = None;
        let (comments, more) = match page {
            Ok(page) => page,
            Err(why) => {
                self.error = Some(messages::one_line(&why));
                return true;
            }
        };
        // A page that brought nothing new would be asked for again and
        // again.
        let more = more && !comments.is_empty();
        // Where they go: after the last top comment, or after the replies
        // already under theirs, unless that one's gone from the list.
        let at = match replies_of {
            None => self.rows.len(),
            Some(of) => {
                let Some(at) = self.rows.iter().position(|c| c.id == of) else {
                    return true;
                };
                at + 1
                    + self.rows[at + 1..]
                        .iter()
                        .take_while(|c| c.reply_to == Some(of))
                        .count()
            }
        };
        let new: Vec<Comment> = comments
            .iter()
            .filter(|c| self.ids.insert(c.id))
            .map(|c| Comment {
                // A top comment answers nothing, whatever it says; a
                // reply answers the comment whose replies were asked for.
                reply_to: replies_of,
                ..Comment::from(c)
            })
            .collect();
        self.rows.splice(at..at, new);
        match replies_of {
            None => self.has_more = more,
            Some(of) => {
                self.open.insert(of, more);
            }
        }
        true
    }

    /// Moves the cursor `delta` rows, within the comments.
    pub fn move_cursor(&mut self, delta: isize) {
        let last = self.rows.len().saturating_sub(1);
        self.selected = self.selected.saturating_add_signed(delta).min(last);
        // Moving on after a failed load tries again near the end.
        self.error = None;
    }

    pub fn selected_comment(&self) -> Option<&Comment> {
        self.rows.get(self.selected)
    }

    /// Enter on the selected comment: its replies shown, or hidden again.
    /// On a reply, the next page of its comment's replies, if there's more
    /// ("View more replies", under the last one shown).
    pub fn enter(&mut self) -> Replies {
        if self.loading.is_some() {
            return Replies::Nothing;
        }
        let Some(comment) = self.selected_comment() else {
            return Replies::Nothing;
        };
        let (id, replies, reply_to) = (comment.id, comment.replies, comment.reply_to);
        if let Some(parent) = reply_to {
            return match self.open.get(&parent) {
                Some(true) => Replies::Load {
                    of: parent,
                    more: true,
                },
                _ => Replies::Nothing,
            };
        }
        match self.open.get(&id) {
            None if replies > 0 => Replies::Load {
                of: id,
                more: false,
            },
            None => Replies::Nothing,
            Some(_) => {
                self.hide_replies(id);
                Replies::Hidden
            }
        }
    }

    /// Takes comment `of`'s replies off the list; Enter asks for them again.
    fn hide_replies(&mut self, of: i64) {
        self.open.remove(&of);
        let gone: Vec<i64> = self
            .rows
            .iter()
            .filter(|c| c.reply_to == Some(of))
            .map(|c| c.id)
            .collect();
        for id in &gone {
            self.ids.remove(id);
        }
        let before = self.rows[..self.selected.min(self.rows.len())]
            .iter()
            .filter(|c| c.reply_to == Some(of))
            .count();
        self.rows.retain(|c| c.reply_to != Some(of));
        self.selected -= before;
        self.top = self.top.min(self.selected);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn comment(id: i64, fields: serde_json::Value) -> CommentInfo {
        let mut comment = serde_json::json!({
            "id": id,
            "author": {"name": "Sam Rivera", "username": "sam.climbs"},
            "date": 1_759_900_000,
            "text": "so good",
            "likes": 4,
            "replies": 0
        });
        if let (Some(c), Some(fields)) = (comment.as_object_mut(), fields.as_object()) {
            for (key, value) in fields {
                c.insert(key.clone(), value.clone());
            }
        }
        serde_json::from_value(comment).unwrap()
    }

    fn page(ids: std::ops::RangeInclusive<i64>) -> Vec<CommentInfo> {
        ids.map(|id| comment(id, serde_json::json!({}))).collect()
    }

    fn shown(comments: &Comments) -> Vec<i64> {
        comments.rows.iter().map(|c| c.id).collect()
    }

    #[test]
    fn what_people_wrote_is_cleaned_and_a_look_alike_username_is_left_out() {
        let c = Comment::from(&comment(
            1,
            serde_json::json!({
                "author": {"name": "S\u{202E}am\nRivera", "username": "s\u{430}m"},
                "text": "nice\u{1b}[2J\nshot\n",
                "likes": -3
            }),
        ));
        assert_eq!(c.name, "Sam Rivera");
        assert_eq!((c.username.as_str(), c.author()), ("", "Sam Rivera"));
        assert_eq!(c.text, "nice[2J\nshot");
        assert_eq!(c.likes, Some(0));
        let long = Comment::from(&comment(2, serde_json::json!({"text": "x".repeat(3000)})));
        assert_eq!(long.text.chars().count(), MAX_TEXT + 1);
    }

    #[test]
    fn top_comments_load_a_page_at_a_time_without_repeats() {
        let mut comments = Comments::new(5, "alice");
        let first = comments.start_load(None, false).unwrap();
        assert_eq!(comments.start_load(None, true), None, "one at a time");
        comments.on_page(first, Ok((page(1..=10), true)));
        assert!(!comments.wants_more());
        comments.move_cursor(7);
        assert!(comments.wants_more());
        let next = comments.start_load(None, true).unwrap();
        comments.on_page(next, Ok((page(9..=14), false)));
        assert_eq!(shown(&comments), (1..=14).collect::<Vec<_>>());
        assert!(!comments.has_more && !comments.wants_more());
        assert_eq!(
            comments.start_load(None, true),
            None,
            "nothing after the end"
        );
        // An answer to a load that isn't waited for is old news.
        assert!(!comments.on_page(99, Ok((page(20..=21), true))));
    }

    #[test]
    fn enter_shows_replies_under_their_comment_then_more_then_hides_them() {
        let mut comments = Comments::new(5, "alice");
        let first = comments.start_load(None, false).unwrap();
        let mut top = page(1..=3);
        top[1] = comment(2, serde_json::json!({"replies": 4}));
        comments.on_page(first, Ok((top, false)));
        assert_eq!(
            comments.enter(),
            Replies::Nothing,
            "the first has no replies"
        );
        comments.move_cursor(1);
        assert_eq!(comments.enter(), Replies::Load { of: 2, more: false });
        let load = comments.start_load(Some(2), false).unwrap();
        comments.on_page(load, Ok((page(21..=22), true)));
        assert_eq!(shown(&comments), [1, 2, 21, 22, 3]);
        assert!(comments.rows[2].reply_to == Some(2));
        // On a reply, Enter asks for the next ones.
        comments.move_cursor(2);
        assert_eq!(comments.enter(), Replies::Load { of: 2, more: true });
        let load = comments.start_load(Some(2), true).unwrap();
        comments.on_page(load, Ok((page(23..=24), false)));
        assert_eq!(shown(&comments), [1, 2, 21, 22, 23, 24, 3]);
        assert_eq!(comments.enter(), Replies::Nothing, "all of them are there");
        comments.move_cursor(-2);
        assert_eq!(comments.selected_comment().unwrap().id, 2);
        assert_eq!(comments.enter(), Replies::Hidden);
        assert_eq!(shown(&comments), [1, 2, 3]);
        assert_eq!(comments.selected, 1);
        // Shown again, they're asked for again.
        assert_eq!(comments.enter(), Replies::Load { of: 2, more: false });
    }

    #[test]
    fn a_failed_load_says_why_and_moving_on_tries_again() {
        let mut comments = Comments::new(5, "alice");
        let first = comments.start_load(None, false).unwrap();
        comments.on_page(first, Ok((page(1..=4), true)));
        let next = comments.start_load(None, true).unwrap();
        comments.on_page(next, Err("Instagram\nsays slow down.".into()));
        assert_eq!(comments.error.as_deref(), Some("Instagram says slow down."));
        assert!(!comments.wants_more());
        comments.move_cursor(1);
        assert!(comments.wants_more());
    }
}
