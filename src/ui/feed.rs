//! The feed: its posts one under another, each its author over its photo or
//! video (a carousel's one at a time), then its likes and comments and its
//! caption. The selected post has a bar down its left and more of its
//! caption. Inside Pepper, a video playing shows over its cover, in the box
//! [`draw`] returns, or filling the window ([`draw_full_screen`]).

use chrono::{Datelike, Local, TimeZone};
use ratatui::Frame;
use ratatui::layout::{Constraint, Layout, Margin, Rect};
use ratatui::style::{Style, Stylize};
use ratatui::text::{Line, Span};
use ratatui::widgets::{Block, Paragraph};
use ratatui_image::FontSize;
use ratatui_image::sliced::{SignedPosition, SlicedImage};
use tokio::time::Instant;
use unicode_width::UnicodeWidthStr;

use super::{Cover, border, bordered, truncate};
use crate::feed::{self, Feed, Item, Post};
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
pub(super) const PLAY: &str = "\u{f04b}";
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
    /// The time of this frame, when the view may slide to where the cursor
    /// took it: not under a popup, with smooth scrolling off, or where a
    /// picture would be sent again for every row it moves (sixel, iTerm2).
    pub glide_at: Option<Instant>,
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
    let what = if feed.linked.is_some() {
        "Post"
    } else {
        "Feed"
    };
    let mut title = format!(" {} · {what} ", feed.network.name());
    if feed.loading.is_some() {
        title.push_str("loading… ");
    }
    let block = bordered(colors)
        .title(title)
        .border_style(border(view.focused, colors));
    let inner = block.inner(area);
    frame.render_widget(block, area);
    if feed.posts.is_empty() {
        let what = empty_feed(feed, "Nothing in the feed yet");
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
    let (width, max_rows) = media_room(inner);
    let column = Rect {
        x: inner.x + inner.width.saturating_sub(width + 3) / 2,
        width: (width + 3).min(inner.width),
        ..inner
    };
    let font = images.font_size();
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
    let before = feed.top;
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
    let off = feed.glide_rows(before, inner.height, view.glide_at, height);
    let pane = i32::from(inner.height);

    let now = Local::now().timestamp();
    let mut video_box = None;
    let (mut i, mut y) = feed.first_drawn(off, height);
    while i < feed.posts.len() && y < pane {
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
        let (at, above) = post_rows(column, y, total);
        frame.render_widget(Paragraph::new(lines).scroll((above, 0)), at);
        // The photo, centered under the author and the blank row after.
        let x = 2 + width.saturating_sub(cols) / 2;
        let media_y = y + 2;
        let media_end = media_y + i32::from(rows);
        let middle = media_y + i32::from(rows / 2);
        if let Some(still) = item.and_then(|it| it.still.as_ref()) {
            // One slid past the pane's top isn't asked for.
            if media_end > 0 {
                draw_still(
                    frame,
                    column,
                    still,
                    (x, media_y as i16),
                    (cols, rows),
                    images,
                    view.covered,
                    colors,
                );
            }
        } else if item.is_some_and(|it| it.video) && (0..pane).contains(&middle) {
            let row = Rect::new(column.x + x, inner.y + middle as u16, cols, 1);
            frame.render_widget(Line::from("▶ Video").fg(colors.subtle).centered(), row);
        }
        // Pepper's box can't be cut by the pane's edge: all of it, or none,
        // and it would be told of every row of a slide: it waits for the
        // view to rest.
        if off == 0 && playing.is_some() && media_end <= pane {
            video_box = Some(Rect::new(
                column.x + x,
                inner.y + media_y as u16,
                cols,
                rows,
            ));
        }
        // The seek bar, in the blank row right under it.
        if off == 0
            && playing.is_some()
            && let Some((position, length)) = view.progress
            && media_end < pane
        {
            let row = Rect::new(column.x + x, inner.y + media_end as u16, cols, 1);
            frame.render_widget(seek_bar(position, length, cols.into(), colors), row);
        }
        y += i32::from(total);
        i += 1;
    }
    // Where the view is going comes first, though it's not in the pane yet.
    if off != 0 {
        let (mut i, mut y) = (feed.top, 0);
        while i < feed.posts.len() && y < inner.height {
            let post = &feed.posts[i];
            let item = post.media.get(feed.shown_index(post));
            if let Some(still) = item.and_then(|it| it.still.as_ref()) {
                let ((cols, rows), _) = layout(feed, i);
                images.want(still, cols, rows);
            }
            y = y.saturating_add(height(feed, i));
            i += 1;
        }
    }
    // The next posts' photos download before they're in view, and the
    // nearest few are made into images at the size they'll be drawn
    // (feed::AHEAD, feed::MADE_AHEAD).
    let ahead = feed.posts.iter().skip(feed.selected + 1).take(feed::AHEAD);
    for (n, post) in ahead.enumerate() {
        let item = post.media.get(feed.shown_index(post));
        if let Some(still) = item.and_then(|it| it.still.as_ref()) {
            match n < feed::MADE_AHEAD {
                true => {
                    let (cols, rows) = media_cells(item, width, max_rows, font);
                    images.prepare(still, cols, rows);
                }
                false => images.prefetch(still),
            }
        }
    }
    if i == feed.posts.len() && (0..pane).contains(&y) {
        let end = feed_end(feed);
        if let Some(end) = end {
            let row = Rect::new(inner.x, inner.y + y as u16, inner.width, 1);
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

/// The video playing, filling the window (`f`).
pub struct FullScreen<'a> {
    /// Its post's photo or video, whose shape it keeps.
    pub item: Option<&'a Item>,
    /// What it's doing: "playing", "paused"…
    pub label: &'static str,
    /// Where it is and how long it is, in seconds, once its player says.
    pub progress: Option<(f64, f64)>,
    /// The toast's title and detail, said on the seek bar's row instead of
    /// over the video, which would hide it.
    pub toast: Option<(&'a str, &'a str)>,
}

/// Draws the video playing over all of `area`: its box as large as its
/// shape allows, centered, with the seek bar and what it's doing on the
/// last row. Returns the box, for Pepper; `None` with no room for one.
pub fn draw_full_screen(
    frame: &mut Frame,
    area: Rect,
    shown: FullScreen,
    font: FontSize,
    colors: &Colors,
) -> Option<Rect> {
    frame.render_widget(Cover, area);
    frame.render_widget(
        Block::new().style(Style::new().fg(colors.fg).bg(colors.bg)),
        area,
    );
    let [room, last] = Layout::vertical([Constraint::Fill(1), Constraint::Length(1)]).areas(area);
    if room.is_empty() {
        return None;
    }
    let (cols, rows) = match shown.item {
        Some(item) => media_cells(Some(item), room.width, room.height, font),
        // Its shape unknown: all the room, which the player fills keeping it.
        None => (room.width, room.height),
    };
    let rows = rows.min(room.height);
    let video = Rect::new(
        room.x + (room.width - cols) / 2,
        room.y + (room.height - rows) / 2,
        cols,
        rows,
    );
    // Under Pepper's box once it plays; seen while it loads, or when a
    // popup takes the box away.
    let middle = Rect {
        y: video.y + video.height / 2,
        height: 1,
        ..video
    };
    frame.render_widget(
        Line::from(format!("Video {}", shown.label))
            .fg(colors.subtle)
            .centered(),
        middle,
    );
    // A column of room on each side, as under a post.
    let bar = last.inner(Margin::new(1, 0));
    let (said, color) = match shown.toast {
        Some((title, detail)) => {
            let toast = format!("✓ {title} {detail}");
            let toast = truncate(&toast, usize::from(bar.width / 2));
            (format!("  {toast}"), colors.success)
        }
        None => (format!("  {}", shown.label), colors.accent),
    };
    let mut line = match shown.progress {
        Some((position, length)) => {
            let width = usize::from(bar.width).saturating_sub(said.width());
            seek_bar(position, length, width, colors)
        }
        None => Line::default(),
    };
    line.push_span(Span::from(said).fg(color));
    frame.render_widget(line.right_aligned(), bar);
    Some(video)
}

/// What an empty feed's pane says: loading, why it didn't, that nothing
/// came this time, or `nothing`.
pub(super) fn empty_feed(feed: &Feed, nothing: &str) -> String {
    match (&feed.error, feed.loading.is_some(), &feed.linked) {
        (_, true, Some(_)) => "Loading the post…".to_string(),
        (_, true, None) => "Loading the feed…".to_string(),
        (Some(why), _, Some(_)) => format!("{why}\nr tries again · Esc goes on to the feed"),
        (Some(why), _, None) => format!("{why}\nPress r to try again"),
        _ if feed.stalled() => "Nothing came this time\nj asks again".to_string(),
        _ => nothing.to_string(),
    }
}

/// What's said under the last post: more coming, why they didn't, that
/// none came this time or for so long it stopped asking, that there are
/// none, or, under a post a link opened, where Esc goes.
pub(super) fn feed_end(feed: &Feed) -> Option<String> {
    match (&feed.error, feed.loading.is_some(), feed.has_more) {
        _ if feed.linked.is_some() => Some("Esc goes on to the feed".to_string()),
        (_, true, _) => Some("Loading more…".to_string()),
        (Some(why), ..) => Some(format!("Couldn't load more: {why}")),
        _ if feed.stalled() => Some("Nothing new came · j asks again".to_string()),
        _ if feed.gave_up() => Some("Nothing new came · r loads the feed again".to_string()),
        (None, false, false) => Some("You're all caught up".to_string()),
        _ => None,
    }
}

/// The room a post's photo has inside the feed's pane `inner`: the columns
/// it may be wide and the rows it may be tall.
fn media_room(inner: Rect) -> (u16, u16) {
    let width = inner.width.saturating_sub(3).min(MAX_POST_COLS);
    let rows = inner
        .height
        .saturating_sub(FRAME_ROWS + CAPTION_LINES as u16)
        .max(MIN_MEDIA_ROWS);
    (width, rows)
}

/// The largest box, in pixels, a post's picture is drawn in when the feed
/// takes `area` (inside its border), so `load_feed` sends no bigger one
/// than that needs. `None` before anything was drawn there.
pub fn picture_box(area: Rect, font: FontSize) -> Option<(u32, u32)> {
    let (cols, rows) = media_room(area.inner(Margin::new(1, 1)));
    let size = (
        u32::from(cols) * u32::from(font.width),
        u32::from(rows) * u32::from(font.height),
    );
    (size.0 > 0 && size.1 > 0).then_some(size)
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
pub(super) fn seek_bar(position: f64, length: f64, width: usize, colors: &Colors) -> Line<'static> {
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

/// The part of `column` a post of `total` rows takes when it starts at row
/// `y` of it (above it, less than 0, while the view slides), and how many
/// of the post's rows that leaves out at the top.
pub(super) fn post_rows(column: Rect, y: i32, total: u16) -> (Rect, u16) {
    let above = (-y).clamp(0, i32::from(total)) as u16;
    let top = y.clamp(0, i32::from(column.height)) as u16;
    let at = Rect {
        y: column.y + top,
        height: (total - above).min(column.height - top),
        ..column
    };
    (at, above)
}

/// Paints a photo at `at` (inside `area`) over `size` cells, or says it's
/// coming. Under a popup, sixel and iTerm2 pictures wait, as in a chat.
#[allow(clippy::too_many_arguments)]
pub(super) fn draw_still(
    frame: &mut Frame,
    area: Rect,
    still: &Preview,
    at: (u16, i16),
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
    if let Some(image) = images.get(still, cols, rows, covered) {
        let position = SignedPosition::from((at.0 as i16, at.1));
        frame.render_widget(SlicedImage::new(image, position), area);
        return;
    }
    let middle = at.1 + (rows / 2) as i16;
    if (0..area.height as i16).contains(&middle) {
        let label = match images.is_broken(still) {
            true => "Preview unavailable",
            false => "Loading…",
        };
        let row = Rect::new(
            area.x + at.0,
            area.y + middle as u16,
            cols.min(area.width),
            1,
        );
        frame.render_widget(Line::from(label).fg(colors.subtle).centered(), row);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// `n` posts, by "Post 1", "Post 2"…, each `media` shows.
    fn feed_of(n: i64, media: serde_json::Value) -> (Feed, Images) {
        use ratatui_image::picker::Picker;
        let mut feed = Feed::new(crate::meta::Network::Instagram);
        let load = feed.start_load(false).unwrap();
        let posts = (1..=n).map(|id| {
            let name = format!("Post {id}");
            let mut fields = serde_json::json!({"author": {"name": name, "username": "someone"}});
            if media.is_array() {
                fields["media"] = media.clone();
            }
            crate::feed::tests::post(id, fields)
        });
        feed.on_page(load, Ok((posts.collect(), false)));
        let images = Images::new(
            Picker::halfblocks(),
            tokio::sync::mpsc::unbounded_channel().0,
        );
        (feed, images)
    }

    /// Draws the feed in 24 rows, which one post nearly fills: what's on
    /// the screen's rows, and where the video's box went.
    fn drawn(
        feed: &mut Feed,
        images: &mut Images,
        glide_at: Option<Instant>,
        playing: Option<(i64, i32, &'static str)>,
    ) -> (Vec<String>, Option<Rect>) {
        use ratatui::Terminal;
        use ratatui::backend::TestBackend;
        let (cols, rows) = (80, 26);
        let mut terminal = Terminal::new(TestBackend::new(cols, rows)).unwrap();
        let mut video = None;
        terminal
            .draw(|frame| {
                let view = View {
                    feed: &mut *feed,
                    focused: true,
                    covered: false,
                    playing,
                    progress: None,
                    glide_at,
                };
                video = draw(frame, frame.area(), view, images, &Colors::default());
            })
            .unwrap();
        let buf = terminal.backend().buffer();
        let row = |y: u16| (0..cols).map(|x| buf[(x, y)].symbol()).collect();
        ((0..rows).map(row).collect(), video)
    }

    fn row_of(rows: &[String], text: &str) -> Option<usize> {
        rows.iter().position(|r| r.contains(text))
    }

    #[test]
    fn the_view_jumps_most_of_the_way_then_eases_onto_the_post_and_rests_there() {
        use crate::motion::{GLIDE, GLIDE_ROWS};
        let (mut feed, mut images) = feed_of(3, serde_json::Value::Null);
        let start = Instant::now();
        let (rest, _) = drawn(&mut feed, &mut images, Some(start), None);
        assert_eq!(row_of(&rest, "Post 1"), Some(1));
        let far = row_of(&rest, "Post 2").unwrap() - 1;

        feed.move_cursor(1);
        let (first, _) = drawn(&mut feed, &mut images, Some(start), None);
        let settle = GLIDE_ROWS as usize;
        assert!(far > settle, "a post further away than a settle");
        assert_eq!(row_of(&first, "Post 2"), Some(1 + settle), "{first:#?}");
        assert_eq!(row_of(&first, "Post 1"), None, "already above the pane");
        assert_eq!(feed.top, 1, "where it's going");

        let halfway = start + GLIDE / 2;
        let (half, _) = drawn(&mut feed, &mut images, Some(halfway), None);
        let left = (GLIDE_ROWS as f32 / 8.0).round() as usize;
        assert_eq!(row_of(&half, "Post 2"), Some(1 + left), "{half:#?}");
        assert_eq!(
            row_of(&half, "Post 1"),
            None,
            "its author is above the pane"
        );
        assert!(feed.glide_frame(halfway).is_some());

        let end = start + GLIDE;
        let (settled, _) = drawn(&mut feed, &mut images, Some(end), None);
        assert_eq!(row_of(&settled, "Post 2"), Some(1));
        assert_eq!(feed.glide_frame(end), None, "no more frames");
    }

    #[test]
    fn going_back_up_the_post_above_comes_down_from_behind_the_panes_top() {
        use crate::motion::{GLIDE, GLIDE_ROWS};
        let (mut feed, mut images) = feed_of(3, serde_json::Value::Null);
        let (rest, _) = drawn(&mut feed, &mut images, None, None);
        let far = row_of(&rest, "Post 2").unwrap() - 1;
        let counts = row_of(&rest, "12 likes").unwrap() - 1;
        feed.move_cursor(1);
        drawn(&mut feed, &mut images, None, None);

        let start = Instant::now();
        feed.move_cursor(-1);
        let (first, _) = drawn(&mut feed, &mut images, Some(start), None);
        assert_eq!(
            row_of(&first, "Post 2"),
            Some(1 + far - GLIDE_ROWS as usize),
            "most of the way at once"
        );

        let (half, _) = drawn(&mut feed, &mut images, Some(start + GLIDE / 2), None);
        let left = (GLIDE_ROWS as f32 / 8.0).round() as usize;
        assert_eq!(row_of(&half, "Post 1"), None, "still behind the top");
        assert_eq!(
            row_of(&half, "12 likes"),
            Some(1 + counts - left),
            "the rows of it that show are its last ones: {half:#?}"
        );
        assert_eq!(row_of(&half, "Post 2"), Some(1 + far - left));

        let (settled, _) = drawn(&mut feed, &mut images, Some(start + GLIDE), None);
        assert_eq!(row_of(&settled, "Post 1"), Some(1));
    }

    #[test]
    fn with_no_time_to_slide_in_the_post_is_there_at_once() {
        let (mut feed, mut images) = feed_of(3, serde_json::Value::Null);
        drawn(&mut feed, &mut images, None, None);
        feed.move_cursor(1);
        let (rows, _) = drawn(&mut feed, &mut images, None, None);
        assert_eq!(row_of(&rows, "Post 2"), Some(1));
        assert_eq!(feed.glide_frame(Instant::now()), None);
    }

    #[test]
    fn a_video_gets_its_box_only_once_the_view_rests() {
        use crate::motion::GLIDE;
        let media = serde_json::json!([{"kind": "video", "file_id": 7, "duration": 15}]);
        let (mut feed, mut images) = feed_of(3, media);
        let start = Instant::now();
        drawn(&mut feed, &mut images, Some(start), None);
        feed.move_cursor(1);
        let playing = Some((2, 7, "playing"));
        let (_, on_the_way) = drawn(&mut feed, &mut images, Some(start), playing);
        assert_eq!(on_the_way, None);
        let (_, there) = drawn(&mut feed, &mut images, Some(start + GLIDE), playing);
        assert!(there.is_some());
    }

    #[test]
    fn full_screen_a_video_keeps_its_shape_and_one_of_no_known_shape_takes_all_the_room() {
        use ratatui::Terminal;
        use ratatui::backend::TestBackend;
        let colors = Colors::default();
        let font = FontSize::new(10, 20);
        let mut terminal = Terminal::new(TestBackend::new(80, 21)).unwrap();
        let mut draw = |item: Option<&Item>| {
            let mut video = None;
            terminal
                .draw(|f| {
                    let shown = FullScreen {
                        item,
                        label: "loading…",
                        progress: None,
                        toast: None,
                    };
                    video = draw_full_screen(f, f.area(), shown, font, &colors);
                })
                .unwrap();
            video
        };
        // A tall one: every row but the last, centered.
        let tall = Item {
            video: true,
            still: Some(Preview {
                file_id: 8,
                width: 1080,
                height: 1920,
                thumbnail: None,
                sticker: false,
            }),
            file: None,
            audio: None,
            duration: 15,
        };
        let video = draw(Some(&tall)).unwrap();
        assert_eq!((video.y, video.height), (0, 20));
        assert!(video.width < 80 && video.x == (80 - video.width) / 2);
        assert_eq!(draw(None), Some(Rect::new(0, 0, 80, 20)));
    }

    #[test]
    fn a_post_is_drawn_no_bigger_than_the_box_its_picture_is_asked_for_in() {
        let font = FontSize::new(10, 20);
        // Inside the border 68 × 28: 60 columns at most, and 28 rows less
        // the author, the counts and two lines of caption.
        let area = Rect::new(0, 0, 70, 30);
        assert_eq!(picture_box(area, font), Some((600, 420)));
        assert_eq!(picture_box(Rect::default(), font), None, "nothing drawn");
        let photo = |width: u32, height: u32| Item {
            video: false,
            still: Some(Preview {
                file_id: 1,
                width,
                height,
                thumbnail: None,
                sticker: false,
            }),
            file: None,
            audio: None,
            duration: 0,
        };
        for (w, h) in [(1080, 1350), (1080, 566), (1080, 1920)] {
            let (cols, rows) = media_cells(Some(&photo(w, h)), 60, 21, font);
            assert!(
                u32::from(cols) * 10 <= 600 && u32::from(rows) * 20 <= 420,
                "{w}×{h}"
            );
        }
    }

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
                    glide_at: None,
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

    #[test]
    fn the_end_of_the_feed_says_whether_more_may_come() {
        let mut feed = Feed::new(crate::meta::Network::Threads);
        let load = feed.start_load(false).unwrap();
        let posts = vec![crate::feed::tests::post(1, serde_json::json!({}))];
        feed.on_page(load, Ok((posts, true)));
        assert_eq!(feed_end(&feed), None);
        let load = feed.start_load(false).unwrap();
        assert_eq!(feed_end(&feed).as_deref(), Some("Loading more…"));
        feed.on_page(load, Ok((vec![], true)));
        assert_eq!(
            feed_end(&feed).as_deref(),
            Some("Nothing new came · j asks again")
        );
        // Empty pages in a row until it stops asking, then the end.
        for _ in 0..2 {
            feed.move_cursor(1);
            let load = feed.start_load(false).unwrap();
            feed.on_page(load, Ok((vec![], true)));
        }
        assert_eq!(
            feed_end(&feed).as_deref(),
            Some("Nothing new came · r loads the feed again")
        );
        let load = feed.start_load(true).unwrap();
        let posts = vec![crate::feed::tests::post(1, serde_json::json!({}))];
        feed.on_page(load, Ok((posts, false)));
        assert_eq!(feed_end(&feed).as_deref(), Some("You're all caught up"));
    }

    #[test]
    fn an_empty_feed_says_when_nothing_came_but_more_may() {
        let mut feed = Feed::new(crate::meta::Network::Threads);
        let load = feed.start_load(true).unwrap();
        feed.on_page(load, Ok((vec![], true)));
        assert_eq!(
            empty_feed(&feed, "No posts"),
            "Nothing came this time\nj asks again"
        );
        let load = feed.start_load(true).unwrap();
        feed.on_page(load, Ok((vec![], false)));
        assert_eq!(empty_feed(&feed, "No posts"), "No posts");
    }
}
