//! Matching text against what's typed after `/`: the chat list's filter,
//! and the highlighting of what matched.

use std::ops::Range;

/// Byte ranges of `text` where `query` appears, ignoring case. In order and
/// non-overlapping. An empty query matches nothing.
pub fn find(text: &str, query: &str) -> Vec<Range<usize>> {
    let query: Vec<char> = query.chars().flat_map(char::to_lowercase).collect();
    let mut out = Vec::new();
    if query.is_empty() {
        return out;
    }
    let mut next_free = 0;
    for (start, _) in text.char_indices() {
        if start < next_free {
            continue;
        }
        if let Some(end) = match_at(text, start, &query) {
            out.push(start..end);
            next_free = end;
        }
    }
    out
}

/// Where a match of the (lowercased) query starting at byte `start` ends.
fn match_at(text: &str, start: usize, query: &[char]) -> Option<usize> {
    let mut want = query.iter();
    let mut next = want.next();
    for (i, c) in text[start..].char_indices() {
        // Some characters lowercase to several, e.g. 'İ'.
        for lower in c.to_lowercase() {
            match next {
                Some(&q) if q == lower => next = want.next(),
                Some(_) => return None,
                None => break,
            }
        }
        if next.is_none() {
            return Some(start + i + c.len_utf8());
        }
    }
    None
}

#[cfg(test)]
mod tests {
    use super::*;

    fn found<'a>(text: &'a str, query: &str) -> Vec<&'a str> {
        find(text, query).into_iter().map(|r| &text[r]).collect()
    }

    #[test]
    fn matches_ignore_case_and_keep_the_original_text() {
        assert_eq!(
            found("Hello hello HELLO", "hello"),
            ["Hello", "hello", "HELLO"]
        );
        assert_eq!(found("Café au lait", "CAFÉ"), ["Café"]);
        assert_eq!(found("aaaa", "aa"), ["aa", "aa"], "no overlaps");
        assert!(found("abc", "").is_empty());
        assert!(found("abc", "abcd").is_empty());
    }

    #[test]
    fn ranges_are_byte_ranges_around_wide_characters() {
        let text = "🎉 Đà Nẵng 🎉";
        assert_eq!(found(text, "nẵng"), ["Nẵng"]);
        // 'İ' lowercases to two characters; the match still ends on a char boundary.
        assert_eq!(found("İstanbul", "i̇st"), ["İst"]);
    }
}
