//! The chat picker: `s` finds a chat, or anyone on Messenger or Instagram,
//! to open.

use std::time::Duration;

use tokio::time::Instant;

use crate::chats::Chats;
use crate::messages::one_line;
use crate::meta::{ChatKind, Found, Network};

/// How long typing has to pause before the networks are searched, so a
/// name typed quickly is one search, not one per letter.
pub const SEARCH_AFTER: Duration = Duration::from_millis(400);
/// The networks are only searched for at least this many characters: fewer
/// find nothing useful.
const MIN_SEARCH_CHARS: usize = 2;

/// One row of the picker.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Choice {
    /// A chat you have, or a group a search found.
    Chat(i64),
    /// Someone a search found, with no chat yet: picking them starts one.
    Person {
        network: Network,
        user_id: i64,
        name: String,
        username: Option<String>,
    },
}

pub struct ChatPicker {
    pub query: String,
    /// The row under the cursor, by what it is rather than where: a chat
    /// moving up when a message arrives mustn't put another under the
    /// cursor just as Enter is pressed. `None` for the top row.
    current: Option<Choice>,
    /// First row shown. Drawing keeps the cursor in view.
    pub scroll: usize,
    /// When to search the networks for the query, once typing pauses.
    search_at: Option<Instant>,
    /// What the last search was for, and what each network found.
    searched: String,
    found: Vec<(Network, Vec<Choice>)>,
    /// A search is on its way, or waiting for typing to pause.
    pub searching: bool,
}

impl ChatPicker {
    pub fn new() -> Self {
        Self {
            query: String::new(),
            current: None,
            scroll: 0,
            search_at: None,
            searched: String::new(),
            found: Vec::new(),
            searching: false,
        }
    }

    /// Changes the query, and starts again from the top. The networks are
    /// searched once typing pauses.
    pub fn edit_query(&mut self, edit: impl FnOnce(&mut String), now: Instant) {
        edit(&mut self.query);
        self.current = None;
        let wanted = search_text(&self.query);
        if wanted.chars().count() < MIN_SEARCH_CHARS {
            self.search_at = None;
            self.searching = false;
        } else if wanted != self.searched {
            self.search_at = Some(now + SEARCH_AFTER);
            self.searching = true;
        }
    }

    /// When to wake up to search the networks.
    pub fn search_at(&self) -> Option<Instant> {
        self.search_at
    }

    /// What to search the networks for, once typing has paused long enough.
    pub fn due_search(&mut self, now: Instant) -> Option<String> {
        self.search_at.filter(|&at| at <= now)?;
        self.search_at = None;
        self.searched = search_text(&self.query).to_string();
        self.found.clear();
        Some(self.searched.clone())
    }

    /// What a network found for `query`, unless the query changed since.
    pub fn set_found(&mut self, network: Network, query: &str, found: Vec<Found>) {
        if query != self.searched {
            return;
        }
        let choices = found
            .into_iter()
            .filter_map(|f| match (f.kind, f.chat_id, f.user_id) {
                (_, Some(chat_id), _) => Some(Choice::Chat(chat_id)),
                (ChatKind::Dm, None, Some(user_id)) => Some(Choice::Person {
                    network,
                    user_id,
                    name: one_line(&f.title),
                    username: f.username.as_deref().map(one_line),
                }),
                _ => None,
            })
            .collect();
        self.found.retain(|(n, _)| *n != network);
        self.found.push((network, choices));
        self.found.sort_by_key(|(n, _)| *n);
        self.searching = self.search_at.is_some();
    }

    /// The rows: your chats matching the query, then whom the networks
    /// found that you have no chat with.
    pub fn choices(&self, chats: &Chats) -> Vec<Choice> {
        let mine = chats.matching(&self.query);
        let mut out: Vec<Choice> = mine.iter().copied().map(Choice::Chat).collect();
        // The query may have changed since the search; what it found only
        // shows while it still fits.
        if self.searched == search_text(&self.query) {
            for choice in self.found.iter().flat_map(|(_, found)| found) {
                let known = match choice {
                    Choice::Chat(id) => mine.contains(id),
                    Choice::Person { user_id, .. } => mine
                        .iter()
                        .any(|&chat| chats.person(chat) == Some(*user_id)),
                };
                if !known && !out.contains(choice) {
                    out.push(choice.clone());
                }
            }
        }
        out
    }

    /// Where the cursor is in `choices`: on the row it was put on, or the
    /// top one if that's gone.
    pub fn selected(&self, choices: &[Choice]) -> usize {
        self.current
            .as_ref()
            .and_then(|current| choices.iter().position(|c| c == current))
            .unwrap_or(0)
    }

    /// The row under the cursor, what Enter picks.
    pub fn current(&self, choices: &[Choice]) -> Option<Choice> {
        choices.get(self.selected(choices)).cloned()
    }

    /// Moves the cursor `delta` rows through `choices`, stopping at the ends.
    pub fn move_by(&mut self, delta: isize, choices: &[Choice]) {
        let at = self
            .selected(choices)
            .saturating_add_signed(delta)
            .min(choices.len().saturating_sub(1));
        self.current = choices.get(at).cloned();
    }
}

/// The query as searched: without spaces around it or the @ of a username.
fn search_text(query: &str) -> &str {
    let query = query.trim();
    query.strip_prefix('@').unwrap_or(query)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn picker_with_chats() -> (ChatPicker, Chats) {
        let mut chats = Chats::default();
        chats.add_local(1, "Alice", None);
        chats.add_local(2, "Bob", None);
        chats.add_local(3, "Alina's group", None);
        chats.refresh();
        (ChatPicker::new(), chats)
    }

    fn person(id: i64, name: &str) -> Found {
        Found {
            chat_id: None,
            user_id: Some(id),
            title: name.into(),
            username: None,
            kind: ChatKind::Dm,
        }
    }

    #[test]
    fn the_networks_are_searched_once_typing_pauses_and_stale_results_are_dropped() {
        let (mut picker, chats) = picker_with_chats();
        let start = Instant::now();
        picker.edit_query(|q| q.push('a'), start);
        assert_eq!(picker.search_at(), None, "one letter finds nothing useful");
        picker.edit_query(|q| q.push('l'), start);
        assert_eq!(picker.due_search(start), None, "still typing");
        assert!(picker.searching);
        let later = start + SEARCH_AFTER;
        assert_eq!(picker.due_search(later).as_deref(), Some("al"));
        assert_eq!(picker.due_search(later), None, "once");

        // Groups found by chat, people you have no chat with by name; one
        // you have a chat with is already listed.
        let group = Found {
            chat_id: Some(50),
            user_id: None,
            title: "Alps".into(),
            username: None,
            kind: ChatKind::Group,
        };
        picker.set_found(Network::Instagram, "al", vec![person(60, "Al\nPacino")]);
        picker.set_found(Network::Messenger, "al", vec![group, person(61, "Alan")]);
        assert!(!picker.searching);
        let choices = picker.choices(&chats);
        assert_eq!(
            choices[..3],
            [Choice::Chat(1), Choice::Chat(3), Choice::Chat(50)]
        );
        assert!(
            matches!(&choices[3], Choice::Person { user_id: 61, .. }),
            "Messenger first"
        );
        assert!(
            matches!(&choices[4], Choice::Person { user_id: 60, name, .. } if name == "Al Pacino")
        );

        // An answer for an older query is dropped.
        picker.edit_query(|q| q.push('i'), later);
        picker.set_found(Network::Messenger, "al", vec![person(70, "Alfred")]);
        assert_eq!(picker.choices(&chats), [Choice::Chat(1), Choice::Chat(3)]);
    }

    #[test]
    fn the_cursor_stays_on_its_chat_when_the_list_reorders() {
        let (mut picker, mut chats) = picker_with_chats();
        let choices = picker.choices(&chats);
        assert_eq!(choices, [Choice::Chat(1), Choice::Chat(2), Choice::Chat(3)]);
        picker.move_by(1, &choices);
        assert_eq!(picker.current(&choices), Some(Choice::Chat(2)));

        // A message in chat 3 moves it to the top.
        let moved: crate::meta::Message = serde_json::from_value(serde_json::json!({
            "id": 1, "chat_id": 3, "date": 5_000_000,
        }))
        .unwrap();
        chats.on_message(&moved);
        chats.refresh();
        let choices = picker.choices(&chats);
        assert_eq!(choices[1], Choice::Chat(1));
        assert_eq!(picker.selected(&choices), 2);
        assert_eq!(picker.current(&choices), Some(Choice::Chat(2)), "still Bob");

        picker.edit_query(|q| q.push('a'), Instant::now());
        let choices = picker.choices(&chats);
        assert_eq!(
            picker.selected(&choices),
            0,
            "a new search starts at the top"
        );
    }
}
