//! The shortcuts tab of the `?` popup.

use ratatui::Frame;
use ratatui::layout::Rect;
use ratatui::style::Stylize;
use ratatui::text::{Line, Span};
use ratatui::widgets::Paragraph;
use unicode_width::UnicodeWidthStr;

use crate::theme::Colors;

/// Every keyboard shortcut, grouped by where it works. Keep this in step
/// with the key handling in `app.rs` (and the status bar hints).
const SHORTCUTS: &[(&str, &[(&str, &str)])] = &[
    (
        "Everywhere",
        &[
            ("?", "This help, and settings"),
            (
                ":",
                "Type a command: login (add a network, or log in again) or logout",
            ),
            (
                "s",
                "Find a chat, or someone on Messenger, Instagram or WhatsApp to write to",
            ),
            ("H", "Highlight or unhighlight the selected chat"),
            (
                "Ctrl-o / Ctrl-i",
                "Back to the chat or reply you left / forward again",
            ),
            (
                "Ctrl-r",
                "Resize the panes: h / l, = as at first, Enter keeps, Esc cancels",
            ),
            ("q", "Quit (press again to stop waiting)"),
            ("Ctrl-c", "Quit, except while writing"),
        ],
    ),
    (
        "Chat list",
        &[
            ("j / k", "Move down / up"),
            ("gg / G", "First / last chat"),
            ("Ctrl-d / Ctrl-u", "Half a page down / up"),
            (
                "Enter / l",
                "Open the chat (Enter / h with the list on the right)",
            ),
            ("i", "Open the chat and write"),
            (
                "Tab / Shift-Tab",
                "Next / previous tab: all chats, each network, the archive",
            ),
            ("/", "Filter chats by name"),
            ("m", "Mute or unmute the chat (on the network)"),
            ("Esc", "Clear the filter"),
        ],
    ),
    (
        "Messages",
        &[
            ("j / k", "Newer / older message"),
            ("gg / G", "Oldest / newest message"),
            ("Ctrl-d / Ctrl-u", "Half a page newer / older"),
            ("Enter", "Show the photo, or open the file or link"),
            ("y", "Copy the text, a link, or the photo or file"),
            ("r", "Reply"),
            ("e", "Edit your message, while the network allows"),
            (
                "R",
                "React, or take your reaction back (/ finds any emoji by name)",
            ),
            ("X", "Take your reaction back"),
            ("d", "Unsend your message, for everyone"),
            ("gd", "Go to the message a reply answers"),
            (
                "Tab",
                "Forward again, as Ctrl-i (most terminals send Ctrl-i as Tab)",
            ),
            ("i", "Write a message"),
            ("a", "Attach a file by its path (Tab completes it)"),
            ("p", "Paste a photo, files or text from the clipboard"),
            (
                "Esc",
                "Cancel the edit, remove the files, cancel the reply, or go back",
            ),
            ("h", "Back to the chat list (l with the list on the right)"),
        ],
    ),
    (
        "Writing",
        &[
            ("Enter", "Send, or save the edit"),
            ("Alt-Enter / Ctrl-j", "New line"),
            ("Ctrl-u", "Delete back to the start of the line"),
            (
                "*bold*",
                "Formatting, as the networks read it: _italic_ ~strike~ `code`",
            ),
            (":smile", "Suggests emoji; Tab puts one in"),
            ("Ctrl-v", "Paste a photo, files or text from the clipboard"),
            (
                "Drop a file",
                "Attach it; the text you write is its caption",
            ),
            (
                "Ctrl-z",
                "Turn files just pasted back into their path as text",
            ),
            ("Esc / Ctrl-c", "Back to Normal mode"),
        ],
    ),
    (
        "Photo viewer",
        &[
            ("h / l", "The photo before / after it in the chat"),
            (
                "j / k",
                "Zoom in / out (also + / -), up to filling the window",
            ),
            ("o", "Open the photo in your computer's viewer"),
            ("y", "Copy the photo"),
            ("Enter / Esc / q", "Close"),
        ],
    ),
    (
        "Command prompt",
        &[
            ("Enter", "Keep the chat filter, or run the command"),
            (
                "Tab / Shift-Tab",
                "Complete a command (again for the next one), or the path of a file to attach",
            ),
            ("Esc / Ctrl-c", "Cancel"),
        ],
    ),
    (
        "Find popup",
        &[
            ("Type", "Search your chats, and every network for people"),
            ("Up / Down", "Move (also Ctrl-p / Ctrl-n, Tab)"),
            ("Enter", "Open it, or start a chat"),
            ("Esc", "Cancel"),
        ],
    ),
    (
        "Menus and popups",
        &[
            ("j / k", "Move"),
            ("Enter", "Choose"),
            ("1-9", "Choose by number"),
            (
                "Space",
                "Turn a setting on or off, or use a theme (Enter too); saved at once",
            ),
            ("Esc / q", "Close"),
            ("Tab / h / l", "Switch tabs in this popup"),
            (
                "y / n",
                "Go ahead or not, when asked about a file, a link or logging out",
            ),
        ],
    ),
    (
        "Logging in (:login)",
        &[
            ("j / k", "Pick a network"),
            ("Enter", "Log in to it, or ask WhatsApp for a new code"),
            (
                "p",
                "WhatsApp: link with your phone number instead of a QR code",
            ),
            ("Esc", "Back, and stop a link that waits"),
        ],
    ),
];

/// The longest key, which sets where the descriptions start.
fn key_width() -> usize {
    SHORTCUTS
        .iter()
        .flat_map(|(_, keys)| keys.iter())
        .map(|(key, _)| key.width())
        .max()
        .unwrap_or(0)
}

/// The shortcuts as lines: a heading per group, then each key and what it does.
fn lines(colors: &Colors) -> Vec<Line<'static>> {
    let key_width = key_width();
    let mut lines = Vec::new();
    for (i, (group, keys)) in SHORTCUTS.iter().enumerate() {
        if i > 0 {
            lines.push(Line::default());
        }
        lines.push(Line::from(format!(" {group}")).fg(colors.accent).bold());
        for (key, what) in keys.iter() {
            lines.push(Line::from(vec![
                Span::from(format!("   {key:<key_width$}  ")).fg(colors.primary),
                Span::from(*what),
            ]));
        }
    }
    lines
}

/// Columns the widest line takes, plus one for the scrollbar, for sizing the
/// popup.
pub fn width() -> usize {
    let what = SHORTCUTS
        .iter()
        .flat_map(|(_, keys)| keys.iter())
        .map(|(_, what)| what.width())
        .max()
        .unwrap_or(0);
    // Indent, the keys, a gap, then the descriptions.
    3 + key_width() + 2 + what + 1
}

/// Rows the list takes, for sizing the popup.
pub fn height() -> usize {
    SHORTCUTS
        .iter()
        .map(|(_, keys)| keys.len() + 2)
        .sum::<usize>()
        - 1
}

/// Draws the list from row `scroll`, first pulling `scroll` back if it's
/// past the end.
pub fn draw(frame: &mut Frame, area: Rect, scroll: &mut usize, colors: &Colors) {
    let lines = lines(colors);
    let max = lines.len().saturating_sub(usize::from(area.height));
    *scroll = (*scroll).min(max);
    frame.render_widget(Paragraph::new(lines).scroll((*scroll as u16, 0)), area);
    super::scrollbar(frame, area, max, *scroll, colors);
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn height_counts_every_line() {
        assert_eq!(height(), lines(&Colors::default()).len());
    }

    #[test]
    fn width_leaves_room_for_every_line_and_the_scrollbar() {
        let widest = lines(&Colors::default())
            .iter()
            .map(Line::width)
            .max()
            .unwrap();
        assert_eq!(width(), widest + 1);
    }
}
