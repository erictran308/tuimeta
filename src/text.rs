//! Text from other people, made safe to show and copy.

/// Characters left out of text from Telegram:
/// - control characters other than line breaks and tabs. The screen drops
///   them anyway, but copied text keeps them, and a hidden `\r` or escape
///   can run a command when pasted into a shell or vim.
/// - bidi overrides and isolates, which can make text read backwards:
///   "invoice<U+202E>fdp.exe" shows as `invoiceexe.pdf` in some terminals,
///   and the bidi marks.
/// - invisible characters that terminals and the layout disagree on: the
///   layout counts them as no columns, while some terminals move the cursor
///   on, which pushes the rest of the row out of place. These are the Hangul
///   fillers of "invisible" Telegram names, the soft hyphen, zero-width
///   spaces and the like. Joiners and variation selectors stay, since emoji
///   need them.
/// - the line and paragraph separators (U+2028, U+2029): not control
///   characters, but a terminal that breaks a line on them would push the
///   rest of a message body out of the bubble the layout drew for it.
/// - the private character kitty uses to place images, so text can't pose
///   as one.
/// - format characters that do nothing in a chat (Egyptian hieroglyph and
///   shorthand format controls), variation selectors past the sixteen emoji
///   use, and the invisible code points not assigned yet.
///
/// Joiners, variation selectors and tags stay or go by what's around them:
/// see [`Keep`].
pub fn is_hidden(c: char) -> bool {
    (c.is_control() && c != '\n' && c != '\t')
        || matches!(
            c,
            '\u{202A}'..='\u{202E}'
                | '\u{2066}'..='\u{2069}'
                | '\u{200E}'
                | '\u{200F}'
                | '\u{061C}'
                | '\u{00AD}'
                | '\u{034F}'
                | '\u{115F}'
                | '\u{1160}'
                | '\u{17B4}'
                | '\u{17B5}'
                | '\u{180E}'
                | '\u{200B}'
                | '\u{2028}'
                | '\u{2029}'
                | '\u{2060}'..='\u{2065}'
                | '\u{206A}'..='\u{206F}'
                | '\u{3164}'
                | '\u{FEFF}'
                | '\u{FFA0}'
                | '\u{FFF0}'..='\u{FFFB}'
                | '\u{13430}'..='\u{1343F}'
                | '\u{1BCA0}'..='\u{1BCA3}'
                | '\u{1D173}'..='\u{1D17A}'
                | '\u{E0000}'..='\u{E001F}'
                | '\u{E0080}'..='\u{E0FFF}'
                | '\u{10EEEE}'
        )
}

/// A character [`Keep`] keeps or drops by what's around it: a joiner, a
/// variation selector or a tag.
pub fn is_joining(c: char) -> bool {
    is_joiner(c) || is_selector(c) || is_tag(c)
}

fn is_joiner(c: char) -> bool {
    matches!(c, '\u{200C}' | '\u{200D}')
}

fn is_selector(c: char) -> bool {
    matches!(c, '\u{FE00}'..='\u{FE0F}')
}

fn is_tag(c: char) -> bool {
    matches!(c, '\u{E0020}'..='\u{E007F}')
}

/// The tags a flag's sequence takes, at most: a region's code, like `gbsct`.
const MAX_FLAG_TAGS: usize = 8;

/// What [`clean`] keeps, decided a character at a time in order: never
/// what [`is_hidden`] leaves out, and joiners, variation selectors and tags
/// only where they join something. A joiner follows something else than a
/// joiner, a selector something else than a joiner or a selector, and tags
/// spell out a flag after 🏴. Each takes no columns, so a run of them would
/// pad text invisibly: a file named `invoice.pdf` followed by hundreds of
/// them, then `.exe`, past where a line is cut.
#[derive(Default)]
pub struct Keep {
    /// The last character kept.
    last: Option<char>,
    /// Tags kept since the 🏴.
    tags: usize,
}

impl Keep {
    /// Whether `c`, after what came before, is kept.
    pub fn keeps(&mut self, c: char) -> bool {
        let last = self.last;
        let keep = !is_hidden(c)
            && match c {
                c if is_joiner(c) => last.is_some_and(|l| !is_joiner(l)),
                c if is_selector(c) => last.is_some_and(|l| !is_joiner(l) && !is_selector(l)),
                // The cancel tag ends a flag's tags.
                '\u{E007F}' => self.tags > 0,
                c if is_tag(c) => {
                    (last == Some('\u{1F3F4}') || self.tags > 0) && self.tags < MAX_FLAG_TAGS
                }
                _ => true,
            };
        if keep {
            self.tags = match c {
                '\u{E0020}'..='\u{E007E}' => self.tags + 1,
                _ => 0,
            };
            self.last = Some(c);
        }
        keep
    }
}

/// `text` without the characters [`is_hidden`] leaves out, nor joiners,
/// variation selectors and tags that join nothing ([`Keep`]).
pub fn clean(text: &str) -> String {
    let mut keep = Keep::default();
    text.chars().filter(|&c| keep.keeps(c)).collect()
}

/// The first `n` characters of `text`.
pub fn first_chars(text: &str, n: usize) -> &str {
    text.char_indices().nth(n).map_or(text, |(i, _)| &text[..i])
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn controls_and_bidi_overrides_are_dropped_but_line_breaks_and_tabs_stay() {
        assert_eq!(clean("invoice\u{202E}fdp.exe"), "invoicefdp.exe");
        assert_eq!(clean("a\u{1b}[2Jb\rc\u{7}d\u{9b}e"), "a[2Jbcde");
        assert_eq!(clean("one\ntwo\tthree"), "one\ntwo\tthree");
        assert_eq!(clean("עברית and العربية"), "עברית and العربية");
        // Line and paragraph separators go, so a body can't break its bubble.
        assert_eq!(clean("legit\u{2028}spoof\u{2029}more"), "legitspoofmore");
    }

    #[test]
    fn invisible_fillers_go_but_emoji_keep_their_joiners_and_selectors() {
        let filler = format!("x{}SPOOF", "\u{3164}".repeat(40));
        assert_eq!(clean(&filler), "xSPOOF");
        assert_eq!(clean("a\u{FFA0}\u{00AD}\u{200B}\u{FEFF}b"), "ab");
        let family = "👨\u{200D}👩\u{200D}👧";
        assert_eq!(clean(family), family);
        assert_eq!(clean("❤\u{FE0F}"), "❤\u{FE0F}");
        let england = "🏴\u{E0067}\u{E0062}\u{E0065}\u{E006E}\u{E0067}\u{E007F}";
        assert_eq!(clean(england), england);
    }

    #[test]
    fn joiners_selectors_and_tags_stay_only_where_they_join_something() {
        // A file name padded so its end falls past where a line is cut.
        let padded = format!("invoice.pdf{}.exe", "\u{200D}".repeat(300));
        assert_eq!(clean(&padded), "invoice.pdf\u{200D}.exe");
        let selectors = format!("a{}b", "\u{FE0F}".repeat(50));
        assert_eq!(clean(&selectors), "a\u{FE0F}b");
        assert_eq!(clean("\u{200D}\u{FE0F}a"), "a", "joining nothing");
        // Tags outside a flag hide words in copied text.
        assert_eq!(clean("pay\u{E0068}\u{E0069}\u{E007F} me"), "pay me");
        let long_flag = format!("🏴{}\u{E007F}", "\u{E0061}".repeat(20));
        assert_eq!(
            clean(&long_flag).chars().count(),
            1 + MAX_FLAG_TAGS + 1,
            "the flag, the most tags one takes, the cancel tag"
        );
        // What emoji need stays.
        for emoji in [
            "❤\u{FE0F}\u{200D}🔥",
            "1\u{FE0F}\u{20E3}",
            "👩\u{1F3FD}\u{200D}🚀",
            "🏴\u{E0067}\u{E0062}\u{E0073}\u{E0063}\u{E0074}\u{E007F}",
            "می\u{200C}خواهم",
        ] {
            assert_eq!(clean(emoji), emoji);
        }
    }

    #[test]
    fn invisible_format_characters_and_unassigned_ones_go() {
        assert_eq!(clean("葛\u{E0100}"), "葛", "variation selectors past 16");
        assert_eq!(clean("a\u{13430}\u{1BCA0}\u{2065}\u{FFF0}\u{E0002}b"), "ab");
    }

    #[test]
    fn first_chars_stops_on_a_character_boundary() {
        assert_eq!(first_chars("héllo", 2), "hé");
        assert_eq!(first_chars("hi", 5), "hi");
    }
}
