//! The comments pane, beside the post they're on: each comment's author and
//! when, what they wrote, then its likes and replies; replies indented
//! under their comment. The selected comment has a bar down its left and
//! all of its text. The post stays in view on the pane's left, its video
//! playing.

use chrono::Local;
use ratatui::Frame;
use ratatui::layout::Rect;
use ratatui::style::{Style, Stylize};
use ratatui::text::{Line, Span};
use ratatui::widgets::Paragraph;
use unicode_width::UnicodeWidthStr;

use super::feed::{HEART, VERIFIED, ago, count};
use super::{border, bordered, truncate};
use crate::comments::{Comment, Comments};
use crate::theme::Colors;

/// The widest the pane is: comments read better in a narrow column, and
/// the post beside them keeps the rest.
const MAX_WIDTH: u16 = 64;
/// The narrowest, unless the window is narrower still.
const MIN_WIDTH: u16 = 32;
/// Lines of a comment's text shown, unless it's the selected one.
const TEXT_LINES: usize = 4;
/// And of the selected one's.
const SELECTED_TEXT_LINES: usize = 40;
/// How far in replies are.
const REPLY_INDENT: usize = 4;

/// The feed's pane split for comments: the post on the left, the comments
/// pane on the right, about two fifths of the width.
pub fn split(area: Rect) -> [Rect; 2] {
    let width = (area.width * 2 / 5)
        .clamp(MIN_WIDTH, MAX_WIDTH)
        .min(area.width / 2);
    let post = Rect {
        width: area.width - width,
        ..area
    };
    let comments = Rect {
        x: area.x + post.width,
        width,
        ..area
    };
    [post, comments]
}

/// Draws the comments pane in `area`; `focused` when the keys go to it.
pub fn draw(
    frame: &mut Frame,
    area: Rect,
    comments: &mut Comments,
    focused: bool,
    colors: &Colors,
) {
    let title = format!(" {} · {} ", comments.what, truncate(&comments.author, 40));
    let block = bordered(colors)
        .title(title)
        .border_style(border(focused, colors));
    let inner = block.inner(area);
    frame.render_widget(block, area);
    if comments.rows.is_empty() {
        let what = match (&comments.error, comments.loading.is_some()) {
            (_, true) => "Loading the comments…".to_string(),
            (Some(why), _) => why.clone(),
            (None, false) => "No comments yet".to_string(),
        };
        let middle = Rect {
            y: inner.y + inner.height / 2,
            height: 1.min(inner.height),
            ..inner
        };
        frame.render_widget(
            Line::from(truncate(&what, inner.width.into()))
                .fg(colors.muted)
                .centered(),
            middle,
        );
        return;
    }
    // A bar and a space on the left, a space on the right.
    let text_width = usize::from(inner.width.saturating_sub(3));
    let lines_of = |comments: &Comments, i: usize| {
        comment_lines(comments, i, text_width, Local::now().timestamp(), colors)
    };
    comments.selected = comments.selected.min(comments.rows.len() - 1);
    comments.top = comments.top.min(comments.selected);
    while comments.top < comments.selected
        && (comments.top..=comments.selected)
            .map(|i| lines_of(comments, i).len())
            .sum::<usize>()
            > usize::from(inner.height)
    {
        comments.top += 1;
    }
    let mut lines = Vec::new();
    for i in comments.top..comments.rows.len() {
        if lines.len() >= usize::from(inner.height) {
            break;
        }
        lines.extend(lines_of(comments, i));
    }
    if lines.len() < usize::from(inner.height) {
        let end = match (&comments.error, &comments.loading, comments.has_more) {
            (_, Some((_, None)), _) => Some("Loading more…".to_string()),
            (Some(why), ..) => Some(format!("Couldn't load more: {why}")),
            _ => None,
        };
        if let Some(end) = end {
            lines.push(
                Line::from(truncate(&end, inner.width.into()))
                    .fg(colors.muted)
                    .centered(),
            );
        }
    }
    lines.truncate(usize::from(inner.height));
    frame.render_widget(Paragraph::new(lines), inner);
}

/// Row `i`'s lines: its header, its text, its likes and replies, and a
/// blank line.
fn comment_lines(
    comments: &Comments,
    i: usize,
    width: usize,
    now: i64,
    colors: &Colors,
) -> Vec<Line<'static>> {
    let comment = &comments.rows[i];
    let selected = i == comments.selected;
    let bar = match selected {
        true => Span::from("▌ ").fg(colors.accent),
        false => Span::from("  "),
    };
    let indent = if comment.reply_to.is_some() {
        REPLY_INDENT
    } else {
        0
    };
    let lead = vec![bar, Span::from(" ".repeat(indent))];
    let width = width.saturating_sub(indent).max(1);
    let line = |spans: Vec<Span<'static>>| {
        let mut all = lead.clone();
        all.extend(spans);
        Line::from(all)
    };
    let mut lines = vec![line(header(comment, width, now, colors))];
    let max = if selected {
        SELECTED_TEXT_LINES
    } else {
        TEXT_LINES
    };
    lines.extend(
        text_lines(&comment.text, width, max)
            .into_iter()
            .map(|text| line(vec![Span::from(text).fg(colors.fg)])),
    );
    let meta = meta(comments, i, colors.pills);
    lines.push(line(vec![Span::from(meta).fg(colors.subtle)]));
    lines.push(Line::default());
    lines
}

/// `sam.climbs ✓ · 3h`
fn header(comment: &Comment, width: usize, now: i64, colors: &Colors) -> Vec<Span<'static>> {
    let when = format!(" · {}", ago(comment.date, now));
    let mark = if comment.verified {
        VERIFIED.width() + 1
    } else {
        0
    };
    let name = truncate(
        comment.author(),
        width.saturating_sub(mark + when.width()).max(1),
    );
    let mut spans = vec![Span::from(name).fg(colors.fg).bold()];
    if comment.verified {
        spans.push(Span::from(" "));
        spans.extend(super::pill(
            Span::from(VERIFIED).fg(colors.bg).bg(colors.primary),
            Style::new(),
            colors,
        ));
    }
    spans.push(Span::from(when).fg(colors.muted));
    spans
}

/// ` 4 · View 3 replies`, in words without a Nerd Font, as the feed's
/// counts are.
fn meta(comments: &Comments, i: usize, icons: bool) -> String {
    let comment = &comments.rows[i];
    let mut parts = Vec::new();
    // A Threads reply's photo or video isn't shown here, only named.
    if let Some(what) = comment.attachment {
        parts.push(format!("[{what}]"));
    }
    if let Some(likes) = comment.likes.filter(|&n| n > 0) {
        parts.push(match icons {
            true => format!("{HEART} {}", count(likes)),
            false if likes == 1 => "1 like".to_string(),
            false => format!("{} likes", count(likes)),
        });
    }
    let loading = |id: i64| comments.loading.is_some_and(|(_, of)| of == Some(id));
    match comment.reply_to {
        None if loading(comment.id) => parts.push("Loading replies…".into()),
        None if comments.open.contains_key(&comment.id) => parts.push("Hide replies".into()),
        None if comment.replies == 1 => parts.push("View 1 reply".into()),
        None if comment.replies > 1 => {
            parts.push(format!("View {} replies", count(comment.replies)))
        }
        // The last reply shown, with more after it.
        Some(parent)
            if comments.open.get(&parent) == Some(&true)
                && comments
                    .rows
                    .get(i + 1)
                    .is_none_or(|next| next.reply_to != Some(parent)) =>
        {
            parts.push(match loading(parent) {
                true => "Loading replies…".into(),
                false => "View more replies".into(),
            });
        }
        _ => {}
    }
    parts.join(" · ")
}

/// The text, wrapped to `width`, at most `max` lines, the last ending with
/// `…` if there's more.
fn text_lines(text: &str, width: usize, max: usize) -> Vec<String> {
    let options = textwrap::Options::new(width).break_words(true);
    let mut lines: Vec<String> = Vec::new();
    let mut more = false;
    'out: for line in text.lines() {
        for piece in textwrap::wrap(line, &options) {
            if lines.len() == max {
                more = true;
                break 'out;
            }
            lines.push(piece.replace('\t', " "));
        }
    }
    if more && let Some(last) = lines.last_mut() {
        *last = format!(
            "{}…",
            truncate(last, width.saturating_sub(1)).trim_end_matches('…')
        );
    }
    lines
}

#[cfg(test)]
mod tests {
    use ratatui::Terminal;
    use ratatui::backend::TestBackend;

    use super::*;
    use crate::meta::CommentInfo;

    fn comment(id: i64, fields: serde_json::Value) -> CommentInfo {
        let mut c = serde_json::json!({"id": id, "author": {"name": "Sam Rivera",
            "username": "sam.climbs"}, "date": 0, "text": "so good", "likes": 4});
        if let (Some(c), Some(fields)) = (c.as_object_mut(), fields.as_object()) {
            for (key, value) in fields {
                c.insert(key.clone(), value.clone());
            }
        }
        serde_json::from_value(c).unwrap()
    }

    fn rows(comments: &mut Comments) -> Vec<String> {
        let mut terminal = Terminal::new(TestBackend::new(80, 30)).unwrap();
        terminal
            .draw(|f| draw(f, f.area(), comments, true, &Colors::default()))
            .unwrap();
        let buf = terminal.backend().buffer();
        (0..buf.area.height)
            .map(|y| (0..buf.area.width).map(|x| buf[(x, y)].symbol()).collect())
            .collect()
    }

    #[test]
    fn the_pane_takes_about_two_fifths_on_the_right_within_its_bounds() {
        let [post, pane] = split(Rect::new(30, 0, 130, 40));
        assert_eq!((post.x, post.width, pane.x, pane.width), (30, 78, 108, 52));
        assert_eq!(pane.right(), 160);
        // Narrow: at least MIN_WIDTH, never more than half.
        assert_eq!(split(Rect::new(0, 0, 70, 40))[1].width, MIN_WIDTH);
        assert_eq!(split(Rect::new(0, 0, 50, 40))[1].width, 25);
        // Wide: at most MAX_WIDTH.
        assert_eq!(split(Rect::new(0, 0, 300, 40))[1].width, MAX_WIDTH);
    }

    #[test]
    fn comments_show_who_wrote_them_and_replies_sit_indented_under_theirs() {
        let mut comments = Comments::new(5, "alice.example");
        assert!(
            rows(&mut comments)
                .iter()
                .any(|r| r.contains("No comments yet"))
        );
        let load = comments.start_load(None, false).unwrap();
        assert!(
            rows(&mut comments)
                .iter()
                .any(|r| r.contains("Loading the comments…"))
        );
        let page = vec![
            comment(1, serde_json::json!({"replies": 2, "text": "first!"})),
            comment(
                2,
                serde_json::json!({"author": {"username": "trail.notes", "verified": true},
                "likes": 1}),
            ),
        ];
        comments.on_page(load, Ok((page, true)));
        let shown = rows(&mut comments);
        assert!(shown[0].contains("Comments · alice.example"), "{shown:#?}");
        let at = |text: &str, rows: &[String]| rows.iter().position(|r| r.contains(text));
        let first = at("sam.climbs", &shown).unwrap();
        assert!(shown[first].contains("▌ sam.climbs · "), "{}", shown[first]);
        assert!(shown[first + 1].contains("first!"));
        assert!(
            shown[first + 2].contains("4 likes · View 2 replies"),
            "{}",
            shown[first + 2]
        );
        assert!(
            shown.iter().any(|r| r.contains("trail.notes  ✓  ·")),
            "{shown:#?}"
        );
        assert!(shown.iter().any(|r| r.contains("1 like")), "{shown:#?}");
        // Its replies, indented under it.
        let load = comments.start_load(Some(1), false).unwrap();
        assert!(
            rows(&mut comments)
                .iter()
                .any(|r| r.contains("Loading replies…"))
        );
        comments.on_page(
            load,
            Ok((
                vec![comment(10, serde_json::json!({"text": "a reply"}))],
                true,
            )),
        );
        let shown = rows(&mut comments);
        let reply = at("a reply", &shown).unwrap();
        let column = |row: &str, text: &str| row.find(text).map(|b| row[..b].chars().count());
        assert_eq!(
            column(&shown[reply], "a reply"),
            column(&shown[first + 1], "first!").map(|c| c + REPLY_INDENT)
        );
        assert!(shown.iter().any(|r| r.contains("Hide replies")));
        assert!(
            shown.iter().any(|r| r.contains("View more replies")),
            "{shown:#?}"
        );
    }
}
