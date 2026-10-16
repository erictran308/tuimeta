//! The feed: its posts one under another, each its author over its photo or
//! video (a carousel's one at a time), then its likes and comments and its
//! caption. The selected post has a bar down its left and more of its
//! caption. Inside Pepper, a video playing shows over its cover, in the box
//! [`draw`] returns.

use chrono::{Datelike, Local, TimeZone};
use ratatui::Frame;
use ratatui::layout::Rect;
use ratatui::style::{Style, Stylize};
use ratatui::text::{Line, Span};
use ratatui::widgets::Paragraph;
use ratatui_image::FontSize;
use ratatui_image::sliced::{SignedPosition, SlicedImage};
use unicode_width::UnicodeWidthStr;

use super::{border, bordered, truncate};
use crate::feed::{Feed, Item, Post};
use crate::images::Images;
use crate::messages::{Preview, duration};
use crate::theme::Colors;

/// Caption lines of a post that isn't selected, and of the one that is.
const CAPTION_LINES: usize = 2;
const SELECTED_CAPTION_LINES: usize = 8;
/// Rows a post takes besides its photo and caption: the author, a blank one
/// on each side of the photo, the line of counts, and a blank one after it.
const FRAME_ROWS: u16 = 5;
/// The icons of the line of counts, from a Nerd Font's Font Awesome set, all
/// one column and drawn alike. Without a Nerd Font the line says it in words
/// instead: emoji and text symbols come in different sizes and styles.
pub(super) const HEART: &str = "\u{f004}";
pub(super) const COMMENT: &str = "\u{f075}";
const PLAY: &str = "\u{f04b}";
/// The widest a post is drawn, in columns, centered in a wider pane:
/// instagram.com's feed is a 470 px column, about 60 cells of 8 px, so a
/// wide window shows more posts rather than bigger ones.
const MAX_POST_COLS: u16 = 60;
/// The fewest rows a photo gets, however small the window.
const MIN_MEDIA_ROWS: u16 = 4;
/// What the verified mark says, on a background of its own so a name
/// ending in a check can't pass for it.
pub(super) const VERIFIED: &str = " ✓ ";

/// What the feed pane shows, from the app's state.
pub struct View<'a> {
    pub feed: &'a mut Feed,
    pub focused: bool,
    /// A popup is over the pane.
    pub covered: bool,
    /// The post and file of the video playing, or waiting to, and what to
    /// say about it ("playing", "paused"…).
    pub playing: Option<(i64, i32, &'static str)>,
    /// Where that video is and how long it is, in seconds, once its player
    /// says: the seek bar under it.
    pub progress: Option<(f64, f64)>,
}

/// Draws the feed in `area`. Returns where the video playing is to show,
/// when its post is in view.
pub fn draw(
    frame: &mut Frame,
    area: Rect,
    view: View,
    images: &mut Images,
    colors: &Colors,
) -> Option<Rect> {
    let feed = view.feed;
    let mut title = format!(" {} · Feed ", feed.network.name());
    if feed.loading.is_some() {
        title.push_str("loading… ");
    }
    let block = bordered(colors)
        .title(title)
        .border_style(border(view.focused, colors));
    let inner = block.inner(area);
    frame.render_widget(block, area);
    if feed.posts.is_empty() {
        let what = match (&feed.error, feed.loading.is_some(), feed.has_more) {
            (_, true, _) => "Loading the feed…".to_string(),
            (Some(why), ..) => format!("{why}\nPress r to try again"),
            _ => "Nothing in the feed yet".to_string(),
        };
        let rows = what.lines().count() as u16;
        let middle = Rect {
            y: inner.y + inner.height.saturating_sub(rows) / 2,
            height: rows.min(inner.height),
            ..inner
        };
        frame.render_widget(Paragraph::new(what).fg(colors.muted).centered(), middle);
        return None;
    }
    // A bar and a space on the left, a space on the right, in a column no
    // wider than MAX_POST_COLS, centered.
    let width = inner.width.saturating_sub(3).min(MAX_POST_COLS);
    let column = Rect {
        x: inner.x + inner.width.saturating_sub(width + 3) / 2,
        width: (width + 3).min(inner.width),
        ..inner
    };
    let font = images.font_size();
    let max_rows = inner
        .height
        .saturating_sub(FRAME_ROWS + CAPTION_LINES as u16)
        .max(MIN_MEDIA_ROWS);
    let layout = |feed: &Feed, i: usize| {
        let post = &feed.posts[i];
        let item = post.media.get(feed.shown_index(post));
        let caption = caption_lines(post, width.into(), i == feed.selected);
        let media = media_cells(item, width, max_rows, font);
        (media, caption)
    };
    let height = |feed: &Feed, i: usize| {
        let ((_, rows), caption) = layout(feed, i);
        FRAME_ROWS + rows + caption.len() as u16
    };
    // The selected post in view, all of it if it fits.
    feed.selected = feed.selected.min(feed.posts.len() - 1);
    feed.top = feed.top.min(feed.selected);
    while feed.top < feed.selected
        && (feed.top..=feed.selected)
            .map(|i| u32::from(height(feed, i)))
            .sum::<u32>()
            > u32::from(inner.height)
    {
        feed.top += 1;
    }

    let now = Local::now().timestamp();
    let mut video_box = None;
    let mut y: u16 = 0;
    let mut i = feed.top;
    while i < feed.posts.len() && y < inner.height {
        let ((cols, rows), caption) = layout(feed, i);
        let post = &feed.posts[i];
        let index = feed.shown_index(post);
        let item = post.media.get(index);
        let selected = i == feed.selected;
        let bar = match selected {
            true => Span::from("▌ ").fg(colors.accent),
            false => Span::from("  "),
        };
        let mut lines = vec![header(post, bar.clone(), width.into(), now, colors)];
        lines.extend((0..rows + 2).map(|_| Line::from(bar.clone())));
        let playing = item
            .and_then(|it| it.file.as_ref())
            .and_then(|f| {
                view.playing
                    .filter(|&(p, file, _)| (p, file) == (post.id, f.id))
            })
            .map(|(.., label)| label);
        lines.push(counts(
            post,
            item,
            index,
            playing,
            bar.clone(),
            width.into(),
            colors,
        ));
        lines.extend(
            caption
                .into_iter()
                .map(|text| Line::from(vec![bar.clone(), Span::from(text).fg(colors.fg)])),
        );
        lines.push(Line::default());
        let total = lines.len() as u16;
        let shown = total.min(inner.height - y);
        let at = Rect {
            y: inner.y + y,
            height: shown,
            ..column
        };
        frame.render_widget(Paragraph::new(lines), at);
        // The photo, centered under the author and the blank row after.
        let x = 2 + width.saturating_sub(cols) / 2;
        let media_y = y + 2;
        if let Some(still) = item.and_then(|it| it.still.as_ref()) {
            draw_still(
                frame,
                column,
                still,
                (x, media_y),
                (cols, rows),
                images,
                view.covered,
                colors,
            );
        } else if item.is_some_and(|it| it.video) && media_y + rows / 2 < inner.height {
            let row = Rect::new(column.x + x, inner.y + media_y + rows / 2, cols, 1);
            frame.render_widget(Line::from("▶ Video").fg(colors.subtle).centered(), row);
        }
        // Pepper's box can't be cut by the pane's edge: all of it, or none.
        if playing.is_some() && media_y + rows <= inner.height {
            video_box = Some(Rect::new(column.x + x, inner.y + media_y, cols, rows));
        }
        // The seek bar, in the blank row right under it.
        if playing.is_some()
            && let Some((position, length)) = view.progress
            && media_y + rows < inner.height
        {
            let row = Rect::new(column.x + x, inner.y + media_y + rows, cols, 1);
            frame.render_widget(seek_bar(position, length, cols.into(), colors), row);
        }
        y = y.saturating_add(total);
        i += 1;
    }
    if i == feed.posts.len() && y < inner.height {
        let end = match (&feed.error, feed.loading.is_some(), feed.has_more) {
            (_, true, _) => Some("Loading more…".to_string()),
            (Some(why), ..) => Some(format!("Couldn't load more: {why}")),
            (None, false, false) => Some("You're all caught up".to_string()),
            _ => None,
        };
        if let Some(end) = end {
            let row = Rect::new(inner.x, inner.y + y, inner.width, 1);
            frame.render_widget(
                Line::from(truncate(&end, inner.width.into()))
                    .fg(colors.muted)
                    .centered(),
                row,
            );
        }
    }
    video_box
}

/// `▌ Alice Example ✓ @alice.example · 3h`
fn header(
    post: &Post,
    bar: Span<'static>,
    width: usize,
    now: i64,
    colors: &Colors,
) -> Line<'static> {
    let mut when = format!(" · {}", ago(post.date, now));
    if post.suggested {
        when.push_str(" · Suggested for you");
    }
    let handle = match post.username.is_empty() || post.name.is_empty() {
        true => String::new(),
        false => format!(" @{}", post.username),
    };
    let mark = if post.verified {
        VERIFIED.width() + 1
    } else {
        0
    };
    let name = truncate(
        post.author(),
        width.saturating_sub(mark + when.width()).max(1),
    );
    let room = width.saturating_sub(name.width() + mark + when.width());
    let mut spans = vec![bar, Span::from(name).fg(colors.fg).bold()];
    if post.verified {
        spans.push(Span::from(" "));
        spans.extend(super::pill(
            Span::from(VERIFIED).fg(colors.bg).bg(colors.primary),
            Style::new(),
            colors,
        ));
    }
    spans.push(Span::from(truncate(&handle, room)).fg(colors.subtle));
    spans.push(Span::from(when).fg(colors.muted));
    Line::from(spans)
}

/// `▌  120   8   0:15            2/3 ●○○` with a Nerd Font,
/// `▌ 120 likes · 8 comments · video 0:15  2/3 ●○○` without.
#[allow(clippy::too_many_arguments)]
fn counts(
    post: &Post,
    item: Option<&Item>,
    index: usize,
    playing: Option<&str>,
    bar: Span<'static>,
    width: usize,
    colors: &Colors,
) -> Line<'static> {
    let icons = colors.pills;
    let said = |n: i64, icon: &str, one: &str, many: &str| match icons {
        true => format!("{icon} {}", count(n)),
        false => format!("{} {}", count(n), if n == 1 { one } else { many }),
    };
    let mut left = Vec::new();
    if let Some(likes) = post.likes {
        left.push(said(likes, HEART, "like", "likes"));
    }
    if let Some(comments) = post.comments {
        left.push(said(comments, COMMENT, "comment", "comments"));
    }
    let mut video = None;
    if let Some(item) = item.filter(|it| it.video) {
        let what = match playing {
            Some(label) => label.to_string(),
            None => duration(item.duration),
        };
        video = Some(match icons {
            true => format!("{PLAY} {what}"),
            false => format!("video {what}"),
        });
    }
    let gap = if icons { "  " } else { " · " };
    let left = left.join(gap);
    let right = match post.media.len() {
        0 | 1 => String::new(),
        n => {
            let dots: String = (0..n.min(10))
                .map(|i| if i == index { '●' } else { '○' })
                .collect();
            format!("{}/{n} {dots}", index + 1)
        }
    };
    let video_text = match (video, left.is_empty()) {
        (Some(v), true) => v,
        (Some(v), false) => format!("{gap}{v}"),
        (None, _) => String::new(),
    };
    let used = left.width() + video_text.width() + right.width();
    let pad = width.saturating_sub(used);
    Line::from(vec![
        bar,
        Span::from(left).fg(colors.subtle),
        Span::from(video_text).fg(colors.accent),
        Span::from(" ".repeat(pad)),
        Span::from(right).fg(colors.muted),
    ])
}

/// `0:12 ━━━━━━●───────── 0:30`: where the video playing is, as wide as
/// the video; only the times when that's too narrow for a bar.
fn seek_bar(position: f64, length: f64, width: usize, colors: &Colors) -> Line<'static> {
    let position = position.clamp(0.0, length.max(0.0));
    let (at, all) = (
        duration(position.floor() as i32),
        duration(length.ceil() as i32),
    );
    let room = width.saturating_sub(at.width() + all.width() + 2);
    if room < 4 {
        return Line::from(format!("{at} / {all}")).fg(colors.subtle);
    }
    let share = if length > 0.0 { position / length } else { 0.0 };
    // The knob takes a column of its own.
    let played = ((room - 1) as f64 * share).round() as usize;
    Line::from(vec![
        Span::from(format!("{at} ")).fg(colors.subtle),
        Span::from("━".repeat(played)).fg(colors.accent),
        Span::from("●").fg(colors.accent),
        Span::from("─".repeat(room - 1 - played)).fg(colors.muted),
        Span::from(format!(" {all}")).fg(colors.subtle),
    ])
}

/// 980, 12.3K, 4.5M.
pub(super) fn count(n: i64) -> String {
    match n {
        ..1_000 => n.to_string(),
        1_000..1_000_000 => trimmed(n as f64 / 1e3, "K"),
        _ => trimmed(n as f64 / 1e6, "M"),
    }
}

fn trimmed(n: f64, unit: &str) -> String {
    let text = if n < 100.0 {
        format!("{n:.1}")
    } else {
        format!("{n:.0}")
    };
    format!("{}{unit}", text.trim_end_matches(".0"))
}

/// How long ago, as Instagram says it: `now`, `5m`, `3h`, `2d`, then the
/// date.
pub(super) fn ago(date: i64, now: i64) -> String {
    let since = (now - date).max(0);
    match since {
        ..60 => "now".into(),
        60..3600 => format!("{}m", since / 60),
        3600..86_400 => format!("{}h", since / 3600),
        86_400..604_800 => format!("{}d", since / 86_400),
        _ => match Local.timestamp_opt(date, 0).single() {
            Some(at) if at.year() == Local::now().year() => at.format("%-d %b").to_string(),
            Some(at) => at.format("%-d %b %Y").to_string(),
            None => String::new(),
        },
    }
}

/// The caption, wrapped to `width`, at most a few lines (more for the
/// selected post), the last ending with `…` if there's more.
fn caption_lines(post: &Post, width: usize, selected: bool) -> Vec<String> {
    if post.caption.is_empty() || width < 2 {
        return Vec::new();
    }
    let max = if selected {
        SELECTED_CAPTION_LINES
    } else {
        CAPTION_LINES
    };
    let options = textwrap::Options::new(width).break_words(true);
    let mut lines: Vec<String> = Vec::new();
    let mut more = false;
    'out: for line in post.caption.lines() {
        for piece in textwrap::wrap(line, &options) {
            if lines.len() == max {
                more = true;
                break 'out;
            }
            lines.push(piece.replace('\t', " "));
        }
    }
    if more && let Some(last) = lines.last_mut() {
        *last = format!("{}…", truncate(last, width - 1).trim_end_matches('…'));
    }
    lines
}

/// The cells a post's photo takes: as wide as `width`, rows from its shape
/// (cells are taller than wide), at most `max_rows`, narrower then.
fn media_cells(item: Option<&Item>, width: u16, max_rows: u16, font: FontSize) -> (u16, u16) {
    let Some(item) = item else {
        return (0, 0);
    };
    let (pw, ph) = item
        .still
        .as_ref()
        .map_or((16.0, 9.0), |s| (f64::from(s.width), f64::from(s.height)));
    let (fw, fh) = (f64::from(font.width.max(1)), f64::from(font.height.max(1)));
    let mut cols = f64::from(width.max(1));
    let mut rows = cols * fw * ph / pw / fh;
    if rows > f64::from(max_rows) {
        rows = f64::from(max_rows);
        cols = rows * fh * pw / ph / fw;
    }
    (
        (cols.round() as u16).clamp(1, width.max(1)),
        (rows.round() as u16).max(1),
    )
}

/// Paints a photo at `at` (inside `area`) over `size` cells, or says it's
/// coming. Under a popup, sixel and iTerm2 pictures wait, as in a chat.
#[allow(clippy::too_many_arguments)]
fn draw_still(
    frame: &mut Frame,
    area: Rect,
    still: &Preview,
    at: (u16, u16),
    size: (u16, u16),
    images: &mut Images,
    covered: bool,
    colors: &Colors,
) {
    let (cols, rows) = size;
    images.want(still, cols, rows);
    if covered && images.paints_over() {
        return;
    }
    if let Some(image) = images.get(still, cols, rows) {
        let position = SignedPosition::from((at.0 as i16, at.1 as i16));
        frame.render_widget(SlicedImage::new(image, position), area);
        return;
    }
    let middle = at.1 + rows / 2;
    if middle < area.height {
        let label = match images.is_broken(still) {
            true => "Preview unavailable",
            false => "Loading…",
        };
        let row = Rect::new(area.x + at.0, area.y + middle, cols.min(area.width), 1);
        frame.render_widget(Line::from(label).fg(colors.subtle).centered(), row);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn counts_and_times_read_as_instagram_writes_them() {
        assert_eq!(count(980), "980");
        assert_eq!(count(12_345), "12.3K");
        assert_eq!(count(2_000), "2K");
        assert_eq!(count(456_000), "456K");
        assert_eq!(count(4_500_000), "4.5M");
        let now = 1_760_000_000;
        assert_eq!(ago(now - 10, now), "now");
        assert_eq!(ago(now - 300, now), "5m");
        assert_eq!(ago(now - 3 * 3600, now), "3h");
        assert_eq!(ago(now - 2 * 86_400, now), "2d");
        assert_eq!(ago(now + 50, now), "now", "a clock ahead of ours");
    }

    #[test]
    fn the_seek_bar_shows_where_the_video_is_as_wide_as_it() {
        let text = |position: f64, length: f64, width: usize| -> String {
            let line = seek_bar(position, length, width, &Colors::default());
            line.spans.iter().map(|s| s.content.as_ref()).collect()
        };
        // 20 columns: the times and their spaces take 10, the bar the rest.
        let bar = |played: usize| format!("{}●{}", "━".repeat(played), "─".repeat(9 - played));
        assert_eq!(text(0.0, 30.0, 20), format!("0:00 {} 0:30", bar(0)));
        assert_eq!(text(15.0, 30.0, 20), format!("0:15 {} 0:30", bar(5)));
        assert_eq!(text(30.0, 30.0, 20), format!("0:30 {} 0:30", bar(9)));
        // Past the end (a late position), or no length yet.
        assert_eq!(text(31.0, 30.0, 20), text(30.0, 30.0, 20));
        assert_eq!(text(5.0, 0.0, 20), format!("0:00 {} 0:00", bar(0)));
        for (position, width) in [(0.0, 20), (12.3, 33), (29.9, 61)] {
            assert_eq!(
                text(position, 30.0, width).width(),
                width,
                "{position} {width}"
            );
        }
        // Too narrow for a bar: the times alone.
        assert_eq!(text(12.0, 30.0, 12), "0:12 / 0:30");
    }

    #[test]
    fn a_suggested_post_says_so_after_when_it_was_posted() {
        let suggested = |yes: bool| {
            Post::from(&crate::feed::tests::post(
                1,
                serde_json::json!({"suggested": yes, "date": 1_000}),
            ))
        };
        let text = |post: &Post| -> String {
            let line = header(
                post,
                Span::from(""),
                80,
                1_000 + 3 * 3600,
                &Colors::default(),
            );
            line.spans.iter().map(|s| s.content.as_ref()).collect()
        };
        assert_eq!(
            text(&suggested(true)),
            "Alice Example @alice.example · 3h · Suggested for you"
        );
        assert_eq!(text(&suggested(false)), "Alice Example @alice.example · 3h");
    }

    #[test]
    fn the_counts_have_matching_icons_with_a_nerd_font_and_words_without() {
        let post = Post::from(&crate::feed::tests::post(
            1,
            serde_json::json!({"likes": 1, "comments": 107, "media": [
                {"kind": "video", "file_id": 7, "duration": 15}]}),
        ));
        let text = |pills: bool| -> String {
            let colors = Colors {
                pills,
                ..Colors::default()
            };
            let line = counts(
                &post,
                post.media.first(),
                0,
                None,
                Span::from(""),
                60,
                &colors,
            );
            line.spans.iter().map(|s| s.content.as_ref()).collect()
        };
        assert_eq!(text(false).trim_end(), "1 like · 107 comments · video 0:15");
        assert_eq!(
            text(true).trim_end(),
            format!("{HEART} 1  {COMMENT} 107  {PLAY} 0:15")
        );
        // Every icon takes one column, as the layout counts it.
        for icon in [HEART, COMMENT, PLAY] {
            assert_eq!(icon.width(), 1);
        }
    }

    #[test]
    fn a_wide_pane_centers_a_narrow_column_with_a_blank_row_under_the_author() {
        use ratatui::Terminal;
        use ratatui::backend::TestBackend;
        use ratatui_image::picker::Picker;

        let mut feed = Feed::new(crate::meta::Network::Instagram);
        let load = feed.start_load(false).unwrap();
        let posts = vec![crate::feed::tests::post(1, serde_json::json!({}))];
        feed.on_page(load, Ok((posts, false)));
        let mut images = Images::new(
            Picker::halfblocks(),
            tokio::sync::mpsc::unbounded_channel().0,
        );
        let mut terminal = Terminal::new(TestBackend::new(160, 40)).unwrap();
        terminal
            .draw(|frame| {
                let view = View {
                    feed: &mut feed,
                    focused: true,
                    covered: false,
                    playing: None,
                    progress: None,
                };
                draw(frame, frame.area(), view, &mut images, &Colors::default());
            })
            .unwrap();
        let buf = terminal.backend().buffer();
        let row = |y: u16| -> String { (0..160).map(|x| buf[(x, y)].symbol()).collect() };
        let author = row(1);
        let bar = author.find('▌').expect("the selected post's bar");
        // Inside the border, (158 - 63) / 2 columns in.
        assert_eq!(author[..bar].chars().count(), 1 + 47, "{author}");
        assert!(author.contains("Alice Example @alice.example"), "{author}");
        assert_eq!(
            row(2).trim_matches(['│', ' ', '▌']),
            "",
            "a blank row under the author"
        );
        // The picture has a blank row under it too, then the counts, said in
        // words without a Nerd Font.
        let counts = (3..39)
            .find(|&y| row(y).contains("12 likes · 3 comments"))
            .unwrap();
        assert_eq!(
            row(counts - 1).trim_matches(['│', ' ', '▌']),
            "",
            "{}",
            row(counts - 1)
        );
        // The author, a blank row, the picture, a blank row, the counts.
        let max_rows = 38 - (FRAME_ROWS + CAPTION_LINES as u16);
        let item = feed.posts[0].media.first();
        let (_, rows) = media_cells(item, 60, max_rows, images.font_size());
        assert_eq!(counts, 1 + 1 + rows + 1 + 1);
        // Nothing of the post past its column.
        let column_end = 1 + 47 + 63;
        for y in 1..39 {
            let rest: String = row(y).chars().skip(column_end).collect();
            assert_eq!(rest.trim_end_matches(['│', ' ']), "", "row {y}: {}", row(y));
        }
    }
}
