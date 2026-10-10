//! Reactions on messages: what's shown under a bubble, and the `R` popup
//! that sets or takes back yours. Meta's networks give each person one
//! reaction per message, of any emoji.

use unicode_width::UnicodeWidthStr;

use crate::meta::ReactionInfo;
use crate::text;

/// Emoji per row in the `R` popup.
pub const COLUMNS: usize = 8;

/// What the `R` popup offers before a search: Messenger's six, then other
/// common ones. `/` searches every emoji.
pub const OFFERED: [&str; 32] = [
    "❤️", "😆", "😮", "😢", "😠", "👍", "👎", "🔥", "😂", "🥰", "😍", "🙏", "👏", "🎉", "💯", "😁",
    "🤔", "🤯", "😱", "🥲", "😭", "🙌", "👌", "✅", "👀", "💀", "🤣", "😅", "😊", "😘", "🫶", "💪",
];

/// Joins emoji like ❤‍🔥 or 👨‍💻 into one. Terminals that can't draw the
/// joined emoji draw each part, two columns each, where the layout has room
/// for one, which shifts the rest of the row.
const JOINER: char = '\u{200D}';

/// Asks for an emoji's colored, two-column look.
const EMOJI_STYLE: char = '\u{FE0F}';

/// One emoji on a message, and how many people added it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Reaction {
    pub emoji: String,
    pub count: i32,
    /// You added it.
    pub chosen: bool,
}

impl Reaction {
    /// How the reaction looks under a bubble.
    pub fn label(&self) -> String {
        shown(&self.emoji)
    }
}

/// The reactions on a message, in the helper's order (most added first).
pub fn from_meta(reactions: &[ReactionInfo]) -> Vec<Reaction> {
    reactions
        .iter()
        .filter(|r| r.count > 0)
        .filter_map(|r| {
            Some(Reaction {
                emoji: clean(&r.emoji)?,
                count: r.count,
                chosen: r.mine,
            })
        })
        .collect()
}

/// An emoji from someone else without anything that could upset the layout:
/// what [`text::clean`] leaves out, and white space. `None` if nothing is
/// left.
fn clean(emoji: &str) -> Option<String> {
    let mut keep = text::Keep::default();
    let clean: String = emoji
        .chars()
        .filter(|&c| !c.is_whitespace() && keep.keeps(c))
        .collect();
    (!clean.is_empty()).then_some(clean)
}

/// The reactions on all the photos of an album, which is drawn as one bubble:
/// counts of the same reaction add up, in the order they first appear.
pub fn merge<'a>(lists: impl IntoIterator<Item = &'a [Reaction]>) -> Vec<Reaction> {
    let mut out: Vec<Reaction> = Vec::new();
    for reaction in lists.into_iter().flatten() {
        match out.iter_mut().find(|r| r.emoji == reaction.emoji) {
            Some(r) => {
                r.count += reaction.count;
                r.chosen |= reaction.chosen;
            }
            None => out.push(reaction.clone()),
        }
    }
    out
}

/// The same emoji, whether or not either has the mark that asks for the
/// emoji look: ❤ and ❤️ are one reaction.
pub fn same(a: &str, b: &str) -> bool {
    let bare = |e: &str| e.replace(EMOJI_STYLE, "");
    bare(a) == bare(b)
}

/// An emoji as it's drawn. Some come, like ❤ and ✍, without the mark that
/// asks for their emoji look, so many terminals draw them as narrow symbols
/// and others as two-column emoji; with the mark, all agree on two. The
/// network is always sent the emoji as it gave it.
pub fn shown(emoji: &str) -> String {
    let mut chars = emoji.chars();
    match (chars.next(), chars.next()) {
        (Some(c), None) if emoji.width() == 1 => format!("{c}{EMOJI_STYLE}"),
        _ => emoji.to_string(),
    }
}

/// A count shortened: 999, 1K, 1.5K, 12K, 1.2M.
pub fn count_label(n: i32) -> String {
    let (unit, size) = match n {
        ..1000 => return n.to_string(),
        1000..1_000_000 => ("K", 1000),
        _ => ("M", 1_000_000),
    };
    let tenths = n / (size / 10);
    if tenths < 100 && tenths % 10 != 0 {
        format!("{}.{}{unit}", tenths / 10, tenths % 10)
    } else {
        format!("{}{unit}", n / size)
    }
}

/// The emoji's name and GitHub shortcodes, for the search and the popup.
pub fn about(emoji: &str) -> Option<&'static emojis::Emoji> {
    emojis::get(emoji).or_else(|| emojis::get(&format!("{emoji}{EMOJI_STYLE}")))
}

/// How well `emoji` matches a search, best first; `None` if it doesn't.
/// `query` is lowercase, without colons around it.
fn rank(emoji: &str, query: &str) -> Option<u8> {
    if emoji == query {
        return Some(0);
    }
    let found = about(emoji)?;
    let name = found.name().to_lowercase();
    let mut names = std::iter::once(name.as_str()).chain(found.shortcodes());
    if names.clone().any(|n| n == query) {
        Some(0)
    } else if names.clone().any(|n| {
        n.split(|c: char| !c.is_alphanumeric())
            .any(|word| word.starts_with(query))
    }) {
        Some(1)
    } else if names.any(|n| n.contains(query)) {
        Some(2)
    } else {
        None
    }
}

/// The emoji of `choices` whose name or shortcode has `query` in it: exact
/// names first, then ones with a word starting with it, then the rest, each
/// in the order given. All of them for an empty query.
pub fn search<'a>(choices: &'a [String], query: &str) -> Vec<&'a str> {
    let query = query.trim().trim_matches(':').to_lowercase();
    if query.is_empty() {
        return choices.iter().map(String::as_str).collect();
    }
    let mut found: Vec<(u8, &str)> = choices
        .iter()
        .filter_map(|e| rank(e, &query).map(|r| (r, e.as_str())))
        .collect();
    found.sort_by_key(|&(r, _)| r);
    found.into_iter().map(|(_, e)| e).collect()
}

/// Every emoji `/` in the popup searches: one of each, without skin tones,
/// and without joined ones (see [`JOINER`]), which terminals disagree on.
fn every_emoji() -> &'static [String] {
    static ALL: std::sync::OnceLock<Vec<String>> = std::sync::OnceLock::new();
    ALL.get_or_init(|| {
        emojis::iter()
            .map(|e| e.as_str())
            .filter(|e| !e.contains(JOINER))
            .map(String::from)
            .collect()
    })
}

/// The `R` popup: a grid of emoji for the message under the cursor. Enter
/// makes the one under the cursor yours, or takes it back if it was.
pub struct ReactMenu {
    pub message_id: i64,
    /// The message on one line, so it's clear which one gets the reaction.
    pub snippet: String,
    /// What can be picked without a search.
    pub choices: Option<Vec<String>>,
    /// What's typed after `/`; `None` when not searching.
    pub query: Option<String>,
    /// Index into [`ReactMenu::shown`].
    pub selected: usize,
    /// Where the cursor starts in the grid: on your reaction, so `R` then
    /// Enter takes it back.
    start: usize,
    /// First row of the grid shown. Drawing keeps the cursor's row in view.
    pub scroll: usize,
}

impl ReactMenu {
    pub fn new(message_id: i64, snippet: String) -> Self {
        Self {
            message_id,
            snippet,
            choices: None,
            query: None,
            selected: 0,
            start: 0,
            scroll: 0,
        }
    }

    /// Fills in what's offered, given the emoji you already put on the
    /// message, and puts the cursor on yours, first if it isn't among them.
    /// Joined emoji are left out (see [`JOINER`]), unless they're yours, so
    /// they can still be taken back.
    pub fn set_choices(&mut self, emoji: Vec<String>, yours: &[String]) {
        let mut choices: Vec<String> = emoji
            .into_iter()
            .filter(|e| !e.contains(JOINER) || yours.contains(e))
            .collect();
        for yours in yours.iter().rev() {
            if !choices.iter().any(|e| same(e, yours)) {
                choices.insert(0, yours.clone());
            }
        }
        self.start = choices
            .iter()
            .position(|e| yours.iter().any(|y| same(e, y)))
            .unwrap_or(0);
        self.selected = self.start;
        self.choices = Some(choices);
    }

    /// Back from the search to the whole grid, with the cursor where it
    /// started.
    pub fn leave_search(&mut self) {
        self.query = None;
        self.selected = self.start;
    }

    /// The emoji in the grid: the ones offered, or what the search finds
    /// among every emoji.
    pub fn shown(&self) -> Vec<&str> {
        match self.query.as_deref() {
            Some(query) if !query.trim().is_empty() => search(every_emoji(), query),
            _ => self
                .choices
                .as_deref()
                .unwrap_or_default()
                .iter()
                .map(String::as_str)
                .collect(),
        }
    }

    /// The emoji under the cursor.
    pub fn current(&self) -> Option<&str> {
        self.shown().get(self.selected).copied()
    }

    /// Moves the cursor `delta` places through the grid, stopping at its ends.
    pub fn move_by(&mut self, delta: isize) {
        let last = self.shown().len().saturating_sub(1);
        self.selected = self.selected.saturating_add_signed(delta).min(last);
    }

    /// Changes the search, and starts again from the best match.
    pub fn edit_query(&mut self, edit: impl FnOnce(&mut String)) {
        edit(self.query.get_or_insert_default());
        self.selected = 0;
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn strings(list: &[&str]) -> Vec<String> {
        list.iter().map(|s| s.to_string()).collect()
    }

    fn info(emoji: &str, count: i32, mine: bool) -> ReactionInfo {
        ReactionInfo {
            emoji: emoji.into(),
            count,
            mine,
        }
    }

    #[test]
    fn reactions_keep_their_order_and_lose_what_could_upset_the_layout() {
        let reactions = from_meta(&[
            info("👍", 3, true),
            info("\u{202E}❤\n", 1, false),
            info("🔥", 0, false),
            info("\u{200B}", 2, false),
        ]);
        let labels: Vec<_> = reactions
            .iter()
            .map(|r| (r.label(), r.count, r.chosen))
            .collect();
        assert_eq!(
            labels,
            [("👍".to_string(), 3, true), ("❤\u{FE0F}".into(), 1, false)]
        );
        assert_eq!(reactions[1].emoji, "❤", "sent back as it came");
    }

    #[test]
    fn narrow_emoji_are_drawn_two_columns_wide_like_the_rest() {
        for e in ["❤", "✍", "☃", "🕊"] {
            assert_eq!(shown(e).width(), 2, "{e}");
        }
        assert_eq!(shown("👍"), "👍");
        assert_eq!(shown("❤️"), "❤️");
        assert_eq!(shown("👨‍💻"), "👨‍💻");
    }

    #[test]
    fn counts_shorten() {
        let labels: Vec<_> = [7, 999, 1000, 1500, 9999, 12_345, 999_999, 1_200_000]
            .map(count_label)
            .into();
        assert_eq!(
            labels,
            ["7", "999", "1K", "1.5K", "9.9K", "12K", "999K", "1.2M"]
        );
    }

    #[test]
    fn an_album_adds_up_the_reactions_of_its_photos() {
        let reaction = |emoji: &str, count, chosen| Reaction {
            emoji: emoji.into(),
            count,
            chosen,
        };
        let first = [reaction("❤", 2, false)];
        let second = [reaction("🔥", 1, false), reaction("❤", 1, true)];
        let merged = merge([first.as_slice(), second.as_slice()]);
        assert_eq!(merged, [reaction("❤", 3, true), reaction("🔥", 1, false)]);
    }

    #[test]
    fn search_finds_emoji_by_name_or_shortcode_best_match_first() {
        let choices = strings(&["👍", "❤", "🔥", "💔", "😍", "❤‍🔥", "💯", "🆒"]);
        assert_eq!(search(&choices, "fire"), ["🔥", "❤‍🔥"]);
        assert_eq!(search(&choices, "heart"), ["❤", "💔", "😍", "❤‍🔥"]);
        assert_eq!(search(&choices, ":+1:"), ["👍"]);
        assert_eq!(search(&choices, "Thumbs"), ["👍"]);
        assert_eq!(search(&choices, "100"), ["💯"]);
        assert_eq!(search(&choices, "cool"), ["🆒"]);
        assert!(search(&choices, "zebra").is_empty());
        assert_eq!(search(&choices, " ").len(), choices.len());
    }

    #[test]
    fn a_search_looks_through_every_emoji_not_just_the_offered_ones() {
        let mut menu = ReactMenu::new(1, "hi".into());
        menu.set_choices(strings(&OFFERED), &[]);
        menu.edit_query(|q| q.push_str("zebra"));
        assert_eq!(menu.current(), Some("🦓"));
        assert!(
            menu.shown().iter().all(|e| !e.contains(JOINER)),
            "no joined emoji"
        );
    }

    #[test]
    fn your_reaction_is_offered_first_when_it_isnt_among_the_rest() {
        let mut menu = ReactMenu::new(1, "hi".into());
        menu.set_choices(strings(&["👍", "❤‍🔥", "👨‍💻"]), &strings(&["👨‍💻"]));
        assert_eq!(menu.shown(), ["👍", "👨‍💻"]);
        menu.set_choices(strings(&["👍", "❤"]), &strings(&["🦓"]));
        assert_eq!(menu.shown(), ["🦓", "👍", "❤"]);
        assert_eq!(menu.current(), Some("🦓"));
    }

    #[test]
    fn the_cursor_starts_on_your_reaction_and_goes_back_there_after_a_search() {
        let mut menu = ReactMenu::new(1, "hi".into());
        menu.set_choices(strings(&["👍", "❤", "🔥"]), &strings(&["🔥"]));
        assert_eq!(menu.current(), Some("🔥"));
        menu.edit_query(|q| q.push_str("thumbsup"));
        assert_eq!(menu.current(), Some("👍"));
        menu.leave_search();
        assert_eq!(menu.current(), Some("🔥"));

        menu.set_choices(strings(&["👍", "❤"]), &[]);
        assert_eq!(menu.current(), Some("👍"), "none of yours: the top");
    }

    #[test]
    fn the_cursor_stays_in_the_grid_and_a_new_search_starts_at_the_top() {
        let mut menu = ReactMenu::new(1, "hi".into());
        assert_eq!(menu.current(), None);
        menu.set_choices(strings(&["👍", "❤", "🔥", "💔"]), &[]);
        menu.move_by(COLUMNS as isize);
        assert_eq!(menu.current(), Some("💔"));
        menu.move_by(-(COLUMNS as isize));
        assert_eq!(menu.current(), Some("👍"));
        menu.move_by(2);
        menu.edit_query(|q| q.push_str("zzzzqq"));
        assert_eq!(menu.current(), None);
    }

    #[test]
    fn an_emoji_with_or_without_its_emoji_mark_is_the_same_reaction() {
        assert!(same("❤", "❤\u{FE0F}"));
        assert!(same("👍", "👍"));
        assert!(!same("❤", "💔"));
        let mut menu = ReactMenu::new(1, "hi".into());
        menu.set_choices(strings(&["👍", "❤\u{FE0F}"]), &strings(&["❤"]));
        assert_eq!(menu.shown(), ["👍", "❤\u{FE0F}"], "not offered twice");
        assert_eq!(menu.current(), Some("❤\u{FE0F}"));
    }

    #[test]
    fn the_offered_emoji_are_each_one_emoji() {
        for e in OFFERED {
            assert!(about(e).is_some(), "{e}");
        }
    }
}
