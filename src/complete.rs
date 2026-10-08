//! Completing the word being typed in the composer: `:` and a few letters
//! puts in an emoji by its shortcode. Tab takes the suggestion under the
//! cursor.

/// Suggestions listed at once.
pub const MAX_SUGGESTIONS: usize = 6;
/// Letters after `:` before emoji are suggested: one finds too many.
const MIN_EMOJI_CHARS: usize = 2;

/// The word before the cursor that can be completed.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Word {
    /// What's typed after the `:`.
    pub query: String,
    /// Characters it takes, with its `:`, which Tab replaces.
    pub chars: usize,
}

/// The word that ends at character `col` of `line`, if it's one to
/// complete: `:` with at least two letters of a shortcode. It has to start
/// the line or follow a space, so `10:30` is left alone, and the cursor has
/// to be at its end.
pub fn word_at(line: &str, col: usize) -> Option<Word> {
    let chars: Vec<char> = line.chars().collect();
    if col > chars.len() || chars.get(col).is_some_and(|c| !c.is_whitespace()) {
        return None;
    }
    let start = chars[..col]
        .iter()
        .rposition(|c| c.is_whitespace())
        .map_or(0, |i| i + 1);
    let word: String = chars[start..col].iter().collect();
    let chars = col - start;
    let query = word.strip_prefix(':')?;
    let shortcode = query.chars().count() >= MIN_EMOJI_CHARS
        && query.starts_with(|c: char| c.is_ascii_alphabetic() || c == '+' || c == '-')
        && query
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '_' | '+' | '-'));
    shortcode.then(|| Word {
        query: query.to_ascii_lowercase(),
        chars,
    })
}

/// One row of suggestions.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Suggestion {
    pub label: String,
    /// A quieter note after it: the shortcode.
    pub detail: String,
    /// What Tab puts in place of the word.
    pub insert: String,
}

/// Emoji whose shortcode has the query in it: ones it starts first, then
/// ones with a word starting with it, then the rest, shortest first.
/// Joined emoji (👨‍💻) are left out, since terminals disagree on their width.
pub fn emoji(query: &str) -> Vec<Suggestion> {
    let mut found: Vec<(u8, usize, &str, &str)> = Vec::new();
    for emoji in emojis::iter() {
        if emoji.as_str().contains('\u{200D}') {
            continue;
        }
        let best = emoji
            .shortcodes()
            .filter_map(|code| {
                let rank = if code.starts_with(query) {
                    0
                } else if code.split('_').any(|word| word.starts_with(query)) {
                    1
                } else if code.contains(query) {
                    2
                } else {
                    return None;
                };
                Some((rank, code.len(), code))
            })
            .min();
        if let Some((rank, len, code)) = best {
            found.push((rank, len, code, emoji.as_str()));
        }
    }
    found.sort();
    found
        .into_iter()
        .take(MAX_SUGGESTIONS)
        .map(|(_, _, code, emoji)| Suggestion {
            label: emoji.to_string(),
            detail: format!(":{code}:"),
            insert: emoji.to_string(),
        })
        .collect()
}

/// Suggestions for the word being typed.
pub struct Completion {
    pub word: Word,
    pub items: Vec<Suggestion>,
    pub selected: usize,
}

impl Completion {
    pub fn new(word: Word) -> Self {
        Self {
            word,
            items: Vec::new(),
            selected: 0,
        }
    }

    /// A new word was typed: back to the top.
    pub fn retype(&mut self, word: Word) {
        self.word = word;
        self.selected = 0;
    }

    /// New suggestions, keeping the cursor among them.
    pub fn set_items(&mut self, items: Vec<Suggestion>) {
        self.selected = self.selected.min(items.len().saturating_sub(1));
        self.items = items;
    }

    pub fn move_by(&mut self, delta: isize) {
        let last = self.items.len().saturating_sub(1);
        self.selected = self.selected.saturating_add_signed(delta).min(last);
    }

    pub fn current(&self) -> Option<&Suggestion> {
        self.items.get(self.selected)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn only_a_word_starting_with_a_colon_at_the_cursor_is_completed() {
        let word = |line: &str| word_at(line, line.chars().count());
        assert_eq!(
            word("so :Smi"),
            Some(Word {
                query: "smi".into(),
                chars: 4,
            })
        );
        assert!(word("ok :+1").is_some());
        assert_eq!(word("so :s"), None, "one letter finds too many");
        assert_eq!(word("at 10:30"), None);
        assert_eq!(word("so :30"), None);
        assert_eq!(word("so :)"), None);
        assert_eq!(word("hi @al"), None, "no mentions");
        assert_eq!(word_at("so :smile now", 5), None, "the cursor is inside it");
    }

    #[test]
    fn emoji_whose_shortcode_starts_with_the_query_come_first() {
        let found = emoji("smile");
        assert_eq!(found[0].label, "😄");
        assert_eq!(found[0].detail, ":smile:");
        assert!(found.len() <= MAX_SUGGESTIONS);
        assert!(found.iter().all(|s| !s.insert.contains('\u{200D}')));
        assert_eq!(emoji("+1")[0].insert, "👍");
        assert!(emoji("zzzzqq").is_empty());
    }
}
