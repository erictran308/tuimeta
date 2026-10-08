//! Service messages: what the network says happened in a chat (someone
//! joined, was added or left, the group was renamed), drawn in the middle of
//! the chat like a date rather than in a bubble.

use crate::text;

/// Longest sentence kept, in characters.
const MAX_CHARS: usize = 300;

/// What a service message says: the network's own sentence, which names
/// people and titles they picked ("Alice named the group Trip").
#[derive(Clone, Debug, PartialEq)]
pub struct Service(String);

/// A piece of a service message's sentence, drawn in a style of its own.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Part {
    /// The network's sentence. It has names people picked in it, so it's
    /// drawn like a name, never muted like tuimeta's own words: a group
    /// named "Thu 8 Oct" can't pass for a date.
    Said(String),
}

impl Part {
    pub fn text(&self) -> &str {
        match self {
            Part::Said(text) => text,
        }
    }
}

/// The sentence as one string, for a notification.
pub fn text(parts: &[Part]) -> String {
    parts.iter().map(Part::text).collect()
}

impl Service {
    /// The service message the helper's `service` sentence is, on one line;
    /// `None` if there's nothing left of it once cleaned.
    pub fn of(sentence: Option<&str>) -> Option<Self> {
        let clean = text::clean(sentence?).replace(['\n', '\t'], " ");
        let clean = text::first_chars(clean.trim(), MAX_CHARS);
        (!clean.is_empty()).then(|| Service(clean.to_string()))
    }

    /// What happened, for the chat list.
    pub fn label(&self) -> String {
        self.0.clone()
    }

    /// What happened, as drawn.
    pub fn sentence(&self) -> Vec<Part> {
        vec![Part::Said(self.0.clone())]
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_networks_sentence_is_kept_on_one_clean_line() {
        let service = Service::of(Some("Alice\u{202E} named\nthe group Trip")).unwrap();
        assert_eq!(service.label(), "Alice named the group Trip");
        assert_eq!(
            service.sentence(),
            [Part::Said("Alice named the group Trip".into())]
        );
        assert_eq!(Service::of(Some(" \u{200B} ")), None);
        assert_eq!(Service::of(None), None);
        let long = "x".repeat(1000);
        assert_eq!(Service::of(Some(&long)).unwrap().label().len(), MAX_CHARS);
    }
}
