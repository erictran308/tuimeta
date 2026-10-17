//! Threads' feed, drawn as threads.com draws it: each post its author's
//! picture on the left, their username and when beside it, the text under
//! that, then its photos and videos side by side, its likes, replies,
//! reposts and shares, and a line before the next post. A thread of several
//! posts has a line down from each picture to the next one's instead. A
//! quoted post and a link's card are boxes under the text. Inside Pepper, a
//! video playing shows over its cover, in the box [`draw`] returns.

use chrono::Local;
use ratatui::Frame;
use ratatui::layout::{Margin, Rect};
use ratatui::style::{Style, Stylize};
use ratatui::text::{Line, Span};
use ratatui::widgets::Paragraph;
use ratatui_image::FontSize;
use ratatui_image::sliced::{SignedPosition, SlicedImage};
use unicode_width::UnicodeWidthStr;

use super::feed::{
    COMMENT, HEART, PLAY, VERIFIED, View, ago, count, draw_still, empty_feed, feed_end, seek_bar,
};
use super::{border, bordered, truncate};
use crate::feed::{self, Feed, Item, LinkCard, Post};
use crate::images::Images;
use crate::messages::{Preview, duration};
use crate::theme::Colors;

/// The widest a post is drawn, in columns, centered in a wider pane:
/// threads.com's column is about 600 px.
const MAX_COLS: u16 = 72;
/// The selection bar and its space, on every post's left.
const BAR: u16 = 2;
/// The author's picture, about square in most fonts, and the space after it.
const AVATAR_COLS: u16 = 4;
const AVATAR_ROWS: u16 = 2;
const GUTTER: u16 = AVATAR_COLS + 1;
/// The most rows a post's photos and videos take, and the fewest, however
/// small the window.
const MAX_MEDIA_ROWS: u16 = 16;
const MIN_MEDIA_ROWS: u16 = 5;
/// Rows of the pane kept for the rest of a post when its photos are as
/// tall as they may be.
const OTHER_ROWS: u16 = 10;
/// Lines of a post's text shown, unless it's the selected one; and of the
/// selected one's.
const TEXT_LINES: usize = 6;
const SELECTED_TEXT_LINES: usize = 40;
/// Lines of a quoted post's text.
const QUOTE_LINES: usize = 3;
const SELECTED_QUOTE_LINES: usize = 8;
/// Columns between photos side by side.
const MEDIA_GAP: u16 = 1;
/// Nerd Font icons for reposts and shares, from the same Font Awesome set
/// as the feed's heart and bubble.
const REPOST: &str = "\u{f079}";
const SHARE: &str = "\u{f1d8}";

/// Draws Threads' feed in `area`. Returns where the video playing is to
/// show, when it's in view.
pub fn draw(
    frame: &mut Frame,
    area: Rect,
    view: View,
    images: &mut Images,
    colors: &Colors,
) -> Option<Rect> {
    let feed = view.feed;
    let which = match (&feed.linked, feed.following) {
        (Some(_), _) => "Post",
        (None, true) => "Following",
        (None, false) => "For you",
    };
    let mut title = format!(" Threads · {which} ");
    if feed.loading.is_some() {
        title.push_str("loading… ");
    }
    let block = bordered(colors)
        .title(title)
        .border_style(border(view.focused, colors));
    let inner = block.inner(area);
    frame.render_widget(block, area);
    if feed.posts.is_empty() {
        let what = empty_feed(
            feed,
            "Nothing in the feed yet\nf switches between For you and Following",
        );
        let rows = what.lines().count() as u16;
        let middle = Rect {
            y: inner.y + inner.height.saturating_sub(rows) / 2,
            height: rows.min(inner.height),
            ..inner
        };
        frame.render_widget(Paragraph::new(what).fg(colors.muted).centered(), middle);
        return None;
    }
    let (width, max_rows) = room(inner);
    let column_width = (width + BAR + GUTTER + 1).min(inner.width);
    let column = Rect {
        x: inner.x + (inner.width - column_width) / 2,
        width: column_width,
        ..inner
    };
    let font = images.font_size();
    let now = Local::now().timestamp();
    let shape = Shape {
        width,
        max_rows,
        font,
        now,
    };
    let lay = |feed: &Feed, i: usize| {
        let post = &feed.posts[i];
        let next_continues = feed.posts.get(i + 1).is_some_and(|p| p.continues);
        let playing = view
            .playing
            .filter(|&(p, ..)| p == post.id)
            .and_then(|(_, file, label)| {
                let item = post.media.get(feed.shown_index(post))?;
                (item.file.as_ref()?.id == file).then_some(label)
            });
        layout(
            post,
            feed.shown_index(post),
            i == feed.selected,
            next_continues,
            playing,
            &shape,
            colors,
        )
    };
    // The selected post in view, all of it if it fits.
    feed.selected = feed.selected.min(feed.posts.len() - 1);
    feed.top = feed.top.min(feed.selected);
    while feed.top < feed.selected
        && (feed.top..=feed.selected)
            .map(|i| lay(feed, i).lines.len() as u32)
            .sum::<u32>()
            > u32::from(inner.height)
    {
        feed.top += 1;
    }

    let mut video_box = None;
    let mut y: u16 = 0;
    let mut i = feed.top;
    while i < feed.posts.len() && y < inner.height {
        let drawn = lay(feed, i);
        let total = drawn.lines.len() as u16;
        let shown = total.min(inner.height - y);
        let at = Rect {
            y: inner.y + y,
            height: shown,
            ..column
        };
        frame.render_widget(Paragraph::new(drawn.lines), at);
        for picture in &drawn.pictures {
            if y + picture.y >= inner.height {
                continue;
            }
            let position = (picture.x, y + picture.y);
            match picture.avatar {
                true => draw_avatar(
                    frame,
                    column,
                    &picture.still,
                    position,
                    images,
                    view.covered,
                ),
                false => draw_still(
                    frame,
                    column,
                    &picture.still,
                    position,
                    (picture.cols, picture.rows),
                    images,
                    view.covered,
                    colors,
                ),
            }
        }
        // Pepper's box can't be cut by the pane's edge: all of it, or none.
        if let Some(spot) = drawn.playing
            && y + spot.y + spot.height <= inner.height
        {
            video_box = Some(Rect::new(
                column.x + spot.x,
                inner.y + y + spot.y,
                spot.width,
                spot.height,
            ));
            // The seek bar, in the blank row right under it.
            if let Some((position, length)) = view.progress
                && y + spot.y + spot.height < inner.height
            {
                let row = Rect::new(
                    column.x + spot.x,
                    inner.y + y + spot.y + spot.height,
                    spot.width,
                    1,
                );
                frame.render_widget(seek_bar(position, length, spot.width.into(), colors), row);
            }
        }
        y = y.saturating_add(total);
        i += 1;
    }
    // The next posts' pictures download before they're in view
    // (feed::AHEAD).
    for post in feed.posts.iter().skip(feed.selected + 1).take(feed::AHEAD) {
        let item = post.media.get(feed.shown_index(post));
        for still in item
            .and_then(|it| it.still.as_ref())
            .into_iter()
            .chain(post.avatar.as_ref())
        {
            images.prefetch(still);
        }
    }
    if i == feed.posts.len() && y < inner.height {
        let end = feed_end(feed);
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

/// The room a post has inside the pane `inner`: the columns its text and
/// photos may be wide, right of its author's picture, and the rows its
/// photos may be tall.
fn room(inner: Rect) -> (u16, u16) {
    let width = inner
        .width
        .min(MAX_COLS)
        .saturating_sub(BAR + GUTTER + 1)
        .max(1);
    let rows = inner
        .height
        .saturating_sub(OTHER_ROWS)
        .clamp(MIN_MEDIA_ROWS, MAX_MEDIA_ROWS);
    (width, rows)
}

/// The largest box, in pixels, a post's picture is drawn in when the feed
/// takes `area` (inside its border), so `load_feed` sends no bigger one
/// than that needs. `None` before anything was drawn there.
pub fn picture_box(area: Rect, font: FontSize) -> Option<(u32, u32)> {
    if area.width < 3 || area.height < 3 {
        return None;
    }
    let (cols, rows) = room(area.inner(Margin::new(1, 1)));
    let size = (
        u32::from(cols) * u32::from(font.width),
        u32::from(rows) * u32::from(font.height),
    );
    (size.0 > 0 && size.1 > 0).then_some(size)
}

/// What every post of a frame is drawn in.
struct Shape {
    /// Columns right of the author's picture.
    width: u16,
    /// Rows the photos may take.
    max_rows: u16,
    font: FontSize,
    now: i64,
}

/// A post as drawn: its lines, and the pictures over them.
struct Drawn {
    lines: Vec<Line<'static>>,
    pictures: Vec<Picture>,
    /// Where the video playing is, from the post's top left.
    playing: Option<Rect>,
}

/// A picture over a post's lines, from its top left.
struct Picture {
    still: Preview,
    x: u16,
    y: u16,
    cols: u16,
    rows: u16,
    avatar: bool,
}

/// Post `post` as drawn, `shown` the photo or video of its carousel under
/// the cursor. `next_continues` when the post after it goes on from it, in
/// one thread: a line goes down from its author's picture instead of one
/// across before the next post.
fn layout(
    post: &Post,
    shown: usize,
    selected: bool,
    next_continues: bool,
    playing: Option<&'static str>,
    shape: &Shape,
    colors: &Colors,
) -> Drawn {
    let width = usize::from(shape.width);
    let bar = match selected {
        true => Span::from("▌ ").fg(colors.accent),
        false => Span::from("  "),
    };
    let mut lines: Vec<Line<'static>> = Vec::new();
    let mut pictures = Vec::new();
    if let Some(context) = &post.context {
        let icon = if context.contains("repost") {
            "↻ "
        } else {
            ""
        };
        let text = truncate(&format!("{icon}{context}"), width);
        lines.push(Line::from(vec![
            bar.clone(),
            Span::from(" ".repeat(GUTTER.into())),
            Span::from(text).fg(colors.muted),
        ]));
    }
    let header_row = lines.len() as u16;
    if let Some(avatar) = &post.avatar {
        pictures.push(Picture {
            still: avatar.clone(),
            x: BAR,
            y: header_row,
            cols: AVATAR_COLS,
            rows: AVATAR_ROWS,
            avatar: true,
        });
    }
    let mut body: Vec<Vec<Span<'static>>> = vec![header(post, width, shape.now, colors)];
    let text_max = if selected {
        SELECTED_TEXT_LINES
    } else {
        TEXT_LINES
    };
    body.extend(
        wrapped(&post.caption, width, text_max)
            .into_iter()
            .map(|text| vec![Span::from(text).fg(colors.fg)]),
    );
    // The author's picture takes two rows.
    while body.len() < usize::from(AVATAR_ROWS) {
        body.push(Vec::new());
    }
    if let Some(quoted) = &post.quoted {
        let max = if selected {
            SELECTED_QUOTE_LINES
        } else {
            QUOTE_LINES
        };
        body.extend(boxed(
            quote_lines(quoted, width, max, shape.now, colors),
            width,
            colors,
        ));
    }
    if let Some(card) = &post.link {
        // Its picture, when it has one, over its title, as wide as the box
        // lets it and no taller than half a post's photos.
        let mut lines = Vec::new();
        if let Some(image) = &card.image {
            let inside = shape.width.saturating_sub(4).max(1);
            let (cols, rows) = fit(image, inside, (shape.max_rows / 2).max(3), shape.font);
            pictures.push(Picture {
                still: image.clone(),
                // Inside the box's left edge and space.
                x: BAR + GUTTER + 2,
                y: header_row + body.len() as u16 + 1,
                cols,
                rows,
                avatar: false,
            });
            lines.extend((0..rows).map(|_| Vec::new()));
        }
        lines.extend(card_lines(card, width, colors));
        body.extend(boxed(lines, width, colors));
    }
    let media_top = header_row + body.len() as u16;
    let row = media_row(post, shown, shape);
    let mut playing_at = None;
    if !row.items.is_empty() {
        // What isn't a picture, by its column from the text's left: a video
        // with no cover says so, and the row says how many more there are
        // past its right edge.
        let mut labels: Vec<(u16, String)> = Vec::new();
        let mut x = 0;
        for &(index, cols) in &row.items {
            let item = &post.media[index];
            if let Some(still) = &item.still {
                pictures.push(Picture {
                    still: still.clone(),
                    x: BAR + GUTTER + x,
                    y: media_top,
                    cols,
                    rows: row.rows,
                    avatar: false,
                });
            } else {
                let label = truncate("▶ Video", cols.into());
                let pad = cols.saturating_sub(label.width() as u16) / 2;
                labels.push((x + pad, label));
            }
            if index == shown && playing.is_some() {
                playing_at = Some(Rect::new(BAR + GUTTER + x, media_top, cols, row.rows));
            }
            x += cols + MEDIA_GAP;
        }
        let more = format!("+{}", row.hidden);
        if row.hidden > 0 && usize::from(x) + more.width() <= width {
            labels.push((x, more));
        }
        for r in 0..row.rows {
            let mut spans = Vec::new();
            if r == row.rows / 2 {
                let mut at = 0;
                for (column, label) in &labels {
                    spans.push(Span::from(" ".repeat(usize::from(column - at))));
                    spans.push(Span::from(label.clone()).fg(colors.subtle));
                    at = column + label.width() as u16;
                }
            }
            body.push(spans);
        }
        // The seek bar's row, under a video, and space before the counts.
        body.push(Vec::new());
    }
    body.push(counts(post, shown, playing, width, colors));
    // Before the next post: a line across, or with a thread going on, the
    // line down from the author's picture.
    let thread_col = usize::from(AVATAR_COLS / 2) - 1;
    for (r, spans) in body.into_iter().enumerate() {
        let down = next_continues && r >= usize::from(AVATAR_ROWS);
        let gutter = match down {
            true => Span::from(format!(
                "{}│{}",
                " ".repeat(thread_col),
                " ".repeat(usize::from(GUTTER) - thread_col - 1)
            ))
            .fg(colors.border),
            false => Span::from(" ".repeat(GUTTER.into())),
        };
        let mut line = vec![bar.clone(), gutter];
        line.extend(spans);
        lines.push(Line::from(line));
    }
    lines.push(match next_continues {
        true => Line::from(vec![
            bar.clone(),
            Span::from(format!("{}│", " ".repeat(thread_col))).fg(colors.border),
        ]),
        false => Line::from("─".repeat(usize::from(BAR + GUTTER) + width)).fg(colors.border),
    });
    Drawn {
        lines,
        pictures,
        playing: playing_at,
    }
}

/// `alice.example ✓ 23h`: the username, as Threads shows it, else the name.
fn header(post: &Post, width: usize, now: i64, colors: &Colors) -> Vec<Span<'static>> {
    let who = match post.username.is_empty() {
        true => post.name.as_str(),
        false => post.username.as_str(),
    };
    let when = format!(" {}", ago(post.date, now));
    let mark = if post.verified {
        VERIFIED.width() + 1
    } else {
        0
    };
    let name = truncate(who, width.saturating_sub(mark + when.width()).max(1));
    let mut spans = vec![Span::from(name).fg(colors.fg).bold()];
    if post.verified {
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

/// A quoted post's lines: whose it is and when, its text, and what else it
/// holds.
fn quote_lines(
    quoted: &Post,
    width: usize,
    max: usize,
    now: i64,
    colors: &Colors,
) -> Vec<Vec<Span<'static>>> {
    let inside = width.saturating_sub(4).max(1);
    let mut lines = vec![header(quoted, inside, now, colors)];
    lines.extend(
        wrapped(&quoted.caption, inside, max)
            .into_iter()
            .map(|text| vec![Span::from(text).fg(colors.fg)]),
    );
    if let Some(what) = media_label(&quoted.media) {
        lines.push(vec![Span::from(what).fg(colors.subtle)]);
    }
    lines
}

/// The cells a picture takes at most `cols` wide and `max_rows` tall, in
/// its shape (cells are taller than wide).
fn fit(still: &Preview, cols: u16, max_rows: u16, font: FontSize) -> (u16, u16) {
    let (fw, fh) = (f64::from(font.width.max(1)), f64::from(font.height.max(1)));
    let (pw, ph) = (
        f64::from(still.width.max(1)),
        f64::from(still.height.max(1)),
    );
    let mut width = f64::from(cols.max(1));
    let mut rows = width * fw * ph / pw / fh;
    if rows > f64::from(max_rows) {
        rows = f64::from(max_rows);
        width = rows * fh * pw / ph / fw;
    }
    (
        (width.round() as u16).clamp(1, cols.max(1)),
        (rows.round() as u16).max(1),
    )
}

/// A link card's lines: its title, then where it goes, by host.
fn card_lines(card: &LinkCard, width: usize, colors: &Colors) -> Vec<Vec<Span<'static>>> {
    let inside = width.saturating_sub(4).max(1);
    let mut lines: Vec<Vec<Span<'static>>> = wrapped(&card.title, inside, 2)
        .into_iter()
        .map(|text| vec![Span::from(text).fg(colors.fg).bold()])
        .collect();
    // The host on its own line, cut from the left, so the end that says
    // where it really goes stays.
    lines.push(vec![
        Span::from(super::truncate_start(&card.host, inside)).fg(colors.subtle),
    ]);
    lines
}

/// `lines` in a box as wide as `width`.
fn boxed(lines: Vec<Vec<Span<'static>>>, width: usize, colors: &Colors) -> Vec<Vec<Span<'static>>> {
    let (tl, tr, bl, br) = match colors.rounded {
        true => ('╭', '╮', '╰', '╯'),
        false => ('┌', '┐', '└', '┘'),
    };
    let inside = width.saturating_sub(4);
    let edge = |left: char, right: char| {
        vec![
            Span::from(format!(
                "{left}{}{right}",
                "─".repeat(width.saturating_sub(2))
            ))
            .fg(colors.border),
        ]
    };
    let mut out = vec![edge(tl, tr)];
    for spans in lines {
        let used: usize = spans.iter().map(|s| s.content.width()).sum();
        let mut line = vec![Span::from("│ ").fg(colors.border)];
        line.extend(spans);
        line.push(Span::from(" ".repeat(inside.saturating_sub(used))));
        line.push(Span::from(" │").fg(colors.border));
        out.push(line);
    }
    out.push(edge(bl, br));
    out
}

/// `2 photos`, `Video 0:15`, `3 photos and videos`, for a quote.
fn media_label(media: &[Item]) -> Option<String> {
    let videos = media.iter().filter(|m| m.video).count();
    match (media.len(), videos) {
        (0, _) => None,
        (1, 1) => Some(format!("▶ Video {}", duration(media[0].duration))),
        (1, _) => Some("Photo".into()),
        (n, 0) => Some(format!("{n} photos")),
        (n, v) if v == n => Some(format!("{n} videos")),
        (n, _) => Some(format!("{n} photos and videos")),
    }
}

/// A post's photos and videos side by side, as tall as each other: which
/// show (by index, with their columns), how tall, and how many more are
/// past the right edge.
struct MediaRow {
    items: Vec<(usize, u16)>,
    rows: u16,
    hidden: usize,
}

/// The row of `post`'s photos and videos that shows `shown`: from the
/// first, or from where `shown` is the last that fits whole. A picture
/// is never cut at the edge (some terminals squash one that is), so those
/// that don't fit are counted instead. One alone is as wide as it may be.
fn media_row(post: &Post, shown: usize, shape: &Shape) -> MediaRow {
    let none = MediaRow {
        items: Vec::new(),
        rows: 0,
        hidden: 0,
    };
    if post.media.is_empty() {
        return none;
    }
    let (fw, fh) = (
        f64::from(shape.font.width.max(1)),
        f64::from(shape.font.height.max(1)),
    );
    let shape_of = |item: &Item| {
        item.still
            .as_ref()
            .filter(|s| s.width > 0 && s.height > 0)
            .map_or(4.0 / 5.0, |s| f64::from(s.width) / f64::from(s.height))
    };
    let width = f64::from(shape.width.max(1));
    let max_rows = f64::from(shape.max_rows.max(1));
    let rows = match post.media.len() {
        // As tall as it is at the full width, at most max_rows.
        1 => (width * fw / shape_of(&post.media[0]) / fh).clamp(1.0, max_rows),
        _ => max_rows,
    };
    let cols: Vec<u16> = post
        .media
        .iter()
        .map(|item| ((rows * fh * shape_of(item) / fw).round() as u16).clamp(2, shape.width.max(2)))
        .collect();
    let fits = |from: usize, to: usize| {
        cols[from..=to].iter().map(|&c| u32::from(c)).sum::<u32>()
            + u32::from(MEDIA_GAP) * (to - from) as u32
            <= u32::from(shape.width)
    };
    let shown = shown.min(post.media.len() - 1);
    let mut first = 0;
    while first < shown && !fits(first, shown) {
        first += 1;
    }
    let mut last = first;
    while last + 1 < post.media.len() && fits(first, last + 1) {
        last += 1;
    }
    MediaRow {
        items: (first..=last).map(|i| (i, cols[i])).collect(),
        rows: rows.round() as u16,
        hidden: post.media.len() - 1 - last,
    }
}

/// `♥ 554  ▬ 37  ↻ 1  ➤ 18  ▶ 0:15     2/4 ●●○○` with a Nerd Font (a
/// count of 0 is its icon alone, as Threads shows it), `554 likes · 37
/// replies · 1 repost · 18 shares` without.
fn counts(
    post: &Post,
    shown: usize,
    playing: Option<&str>,
    width: usize,
    colors: &Colors,
) -> Vec<Span<'static>> {
    let icons = colors.pills;
    let said = |n: Option<i64>, icon: &str, one: &str, many: &str| match (icons, n) {
        (true, Some(0)) => Some(icon.to_string()),
        (true, Some(n)) => Some(format!("{icon} {}", count(n))),
        (true, None) => None,
        (false, Some(n)) if n > 0 => {
            Some(format!("{} {}", count(n), if n == 1 { one } else { many }))
        }
        (false, _) => None,
    };
    let mut parts: Vec<String> = [
        said(post.likes, HEART, "like", "likes"),
        said(post.comments, COMMENT, "reply", "replies"),
        said(post.reposts, REPOST, "repost", "reposts"),
        said(post.shares, SHARE, "share", "shares"),
    ]
    .into_iter()
    .flatten()
    .collect();
    let gap = if icons { "  " } else { " · " };
    let video = post.media.get(shown).filter(|it| it.video).map(|item| {
        let what = match playing {
            Some(label) => label.to_string(),
            None => duration(item.duration),
        };
        match icons {
            true => format!("{PLAY} {what}"),
            false => format!("video {what}"),
        }
    });
    let mut right = match post.media.len() {
        0 | 1 => String::new(),
        n => {
            let at = shown.min(n - 1);
            let dots: String = (0..n.min(10))
                .map(|i| if i == at { '●' } else { '○' })
                .collect();
            format!("{}/{n} {dots}", at + 1)
        }
    };
    // In a narrow pane (beside the replies) the line drops what matters
    // least until it fits: the carousel's dots, then shares, reposts and
    // replies. The video's length stays.
    let (left, video_text) = loop {
        let left = parts.join(gap);
        let video_text = match (&video, left.is_empty()) {
            (Some(v), true) => v.clone(),
            (Some(v), false) => format!("{gap}{v}"),
            (None, _) => String::new(),
        };
        let fits = left.width() + video_text.width() + right.width() <= width;
        if fits || (right.is_empty() && parts.len() <= 1) {
            break (left, video_text);
        }
        if !right.is_empty() {
            right.clear();
        } else {
            parts.pop();
        }
    };
    let pad = width.saturating_sub(left.width() + video_text.width() + right.width());
    vec![
        Span::from(left).fg(colors.subtle),
        Span::from(video_text).fg(colors.accent),
        Span::from(" ".repeat(pad)),
        Span::from(right).fg(colors.muted),
    ]
}

/// `text` wrapped to `width`, line breaks kept, at most `max` lines, the
/// last ending with `…` if there's more.
fn wrapped(text: &str, width: usize, max: usize) -> Vec<String> {
    if text.is_empty() || width < 2 || max == 0 {
        return Vec::new();
    }
    let options = textwrap::Options::new(width).break_words(true);
    let mut lines: Vec<String> = Vec::new();
    let mut more = false;
    'out: for line in text.lines() {
        let pieces = textwrap::wrap(line, &options);
        // A blank line between paragraphs stays.
        if pieces.is_empty() {
            if lines.len() == max {
                more = true;
                break;
            }
            lines.push(String::new());
        }
        for piece in pieces {
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

/// The author's picture at `at` in `area`, or their first letter on a badge
/// until it's ready. Under a popup, sixel and iTerm2 pictures wait.
fn draw_avatar(
    frame: &mut Frame,
    area: Rect,
    still: &Preview,
    at: (u16, u16),
    images: &mut Images,
    covered: bool,
) {
    images.want(still, AVATAR_COLS, AVATAR_ROWS);
    if covered && images.paints_over() {
        return;
    }
    if let Some(image) = images.get(still, AVATAR_COLS, AVATAR_ROWS, covered) {
        let position = SignedPosition::from((at.0 as i16, at.1 as i16));
        frame.render_widget(SlicedImage::new(image, position), area);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::meta::Network;

    fn post(id: i64, fields: serde_json::Value) -> Post {
        let mut base =
            serde_json::json!({"url": "https://www.threads.com/@alice.example/post/ABC"});
        if let (Some(base), Some(fields)) = (base.as_object_mut(), fields.as_object()) {
            for (k, v) in fields {
                base.insert(k.clone(), v.clone());
            }
        }
        Post::new(&crate::feed::tests::post(id, base), Network::Threads)
    }

    fn shape(width: u16, max_rows: u16) -> Shape {
        Shape {
            width,
            max_rows,
            font: FontSize::new(10, 20),
            now: 1_759_900_000 + 3600,
        }
    }

    fn text(spans: &[Span]) -> String {
        spans.iter().map(|s| s.content.as_ref()).collect()
    }

    #[test]
    fn the_counts_are_threads_four_with_icons_and_in_words_without() {
        let p = post(
            1,
            serde_json::json!({"likes": 554, "comments": 37, "reposts": 1, "shares": 0}),
        );
        let colors = |pills| Colors {
            pills,
            ..Colors::default()
        };
        assert_eq!(
            text(&counts(&p, 0, None, 60, &colors(true))).trim_end(),
            format!("{HEART} 554  {COMMENT} 37  {REPOST} 1  {SHARE}")
        );
        assert_eq!(
            text(&counts(&p, 0, None, 60, &colors(false))).trim_end(),
            "554 likes · 37 replies · 1 repost"
        );
        for icon in [REPOST, SHARE] {
            assert_eq!(icon.width(), 1);
        }
        // Too narrow for all of it: what matters least goes first.
        let carousel = post(
            2,
            serde_json::json!({"likes": 189, "comments": 14, "reposts": 26, "shares": 5,
                "media": [{"kind": "photo", "file_id": 1}, {"kind": "photo", "file_id": 2}]}),
        );
        let narrow = |width| text(&counts(&carousel, 0, None, width, &colors(false)));
        assert!(narrow(80).ends_with("1/2 ●○"));
        assert_eq!(narrow(40).trim_end(), "189 likes · 14 replies · 26 reposts");
        assert_eq!(narrow(30).trim_end(), "189 likes · 14 replies");
        assert!(narrow(30).width() <= 30);
    }

    #[test]
    fn a_carousel_shows_whole_pictures_side_by_side_and_counts_the_rest() {
        let photo = |n: i32| {
            serde_json::json!({"kind": "photo", "file_id": n,
            "thumbnail": {"file_id": n, "width": 1080, "height": 1350}})
        };
        let p = post(
            1,
            serde_json::json!({"media": [photo(1), photo(2), photo(3), photo(4)]}),
        );
        // 10 rows of 20 px are 200 px tall, 160 px wide at 4:5: 16 columns.
        let row = media_row(&p, 0, &shape(40, 10));
        assert_eq!(row.rows, 10);
        assert_eq!(row.items, vec![(0, 16), (1, 16)]);
        assert_eq!(row.hidden, 2);
        // Moving on keeps the one under the cursor whole, at the right.
        let row = media_row(&p, 3, &shape(40, 10));
        assert_eq!(row.items, vec![(2, 16), (3, 16)]);
        assert_eq!(row.hidden, 0);
        // One photo alone is as wide as the column lets it, no taller than
        // it may be.
        let one = post(2, serde_json::json!({"media": [photo(1)]}));
        let row = media_row(&one, 0, &shape(40, 16));
        assert_eq!((row.items, row.rows), (vec![(0, 26)], 16));
        let text_only = post(3, serde_json::json!({"media": []}));
        assert!(media_row(&text_only, 0, &shape(40, 16)).items.is_empty());
    }

    #[test]
    fn a_thread_has_a_line_down_from_its_picture_and_other_posts_a_line_across() {
        let p = post(
            1,
            serde_json::json!({"media": [], "caption": "one\ntwo\nthree"}),
        );
        let colors = Colors::default();
        let rows = |next_continues: bool| -> Vec<String> {
            layout(&p, 0, false, next_continues, None, &shape(30, 10), &colors)
                .lines
                .iter()
                .map(|l| text(&l.spans))
                .collect()
        };
        let across = rows(false);
        assert!(across.last().unwrap().starts_with("────"), "{across:?}");
        assert!(across[0].contains("alice.example"));
        assert_eq!(across[1].trim(), "one");
        let down = rows(true);
        assert_eq!(down.len(), across.len());
        // From the picture's second row on, its column has the line.
        assert!(down[2].starts_with("   │"), "{down:?}");
        assert!(down.last().unwrap().ends_with('│'));
    }

    #[test]
    fn a_quote_and_a_link_card_are_boxes_naming_the_author_and_the_host() {
        let p = post(
            1,
            serde_json::json!({"media": [], "caption": "look",
                "quoted": {"id": 0, "author": {"username": "sam"}, "date": 1_759_900_000,
                           "caption": "the quoted words", "media": [], "url": ""},
                "link_preview": {"url": "https://example.com/a/b", "title": "A page"}}),
        );
        let colors = Colors::default();
        let lines: Vec<String> = layout(&p, 0, false, false, None, &shape(40, 10), &colors)
            .lines
            .iter()
            .map(|l| text(&l.spans))
            .collect();
        let joined = lines.join("\n");
        assert!(joined.contains("│ sam 1h"), "{joined}");
        assert!(joined.contains("│ the quoted words"), "{joined}");
        assert!(joined.contains("│ A page"), "{joined}");
        assert!(joined.contains("│ example.com"), "{joined}");
        // Every line of a box is as wide as the box.
        let widths: Vec<usize> = lines
            .iter()
            .filter(|l| l.contains('│'))
            .map(|l| l.trim_end().width())
            .collect();
        assert!(widths.windows(2).all(|w| w[0] == w[1]), "{widths:?}");
    }

    #[test]
    fn the_feed_is_a_centered_column_of_posts_with_pictures_on_the_left_and_lines_between() {
        use ratatui::Terminal;
        use ratatui::backend::TestBackend;
        use ratatui_image::picker::Picker;

        let mut feed = Feed::new(Network::Threads);
        let load = feed.start_load(false).unwrap();
        let photo = |n: i32| {
            serde_json::json!({"kind": "photo", "file_id": n,
            "thumbnail": {"file_id": n, "width": 1080, "height": 1350}})
        };
        let thread = |id: i64, fields: serde_json::Value| {
            let mut all = serde_json::json!({"media": [], "likes": 554, "comments": 37,
                "reposts": 1, "shares": 18, "date": 1_759_900_000,
                "url": "https://www.threads.com/@alice.example/post/ABC",
                "author": {"username": "alice.example",
                           "photo": {"file_id": 99, "width": 150, "height": 150}}});
            if let (Some(all), Some(fields)) = (all.as_object_mut(), fields.as_object()) {
                for (k, v) in fields {
                    all.insert(k.clone(), v.clone());
                }
            }
            crate::feed::tests::post(id, all)
        };
        let posts = vec![
            thread(
                1,
                serde_json::json!({"caption": "Morning coffee",
                "media": [photo(1), photo(2), photo(3)]}),
            ),
            thread(2, serde_json::json!({"caption": "first of a thread"})),
            thread(
                3,
                serde_json::json!({"caption": "and the second", "continues": true}),
            ),
        ];
        feed.on_page(load, Ok((posts, false)));
        let mut images = Images::new(
            Picker::halfblocks(),
            tokio::sync::mpsc::unbounded_channel().0,
        );
        let (cols, rows) = (120, 60);
        let mut terminal = Terminal::new(TestBackend::new(cols, rows)).unwrap();
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
        let row = |y: u16| -> String { (0..cols).map(|x| buf[(x, y)].symbol()).collect() };
        let all: Vec<String> = (0..rows).map(row).collect();
        assert!(all[0].contains("Threads · For you"), "{all:#?}");
        // The column is centered: (118 - 72) / 2 columns in, then the bar.
        let header = &all[1];
        let bar = header.find('▌').expect("the selected post's bar");
        assert_eq!(header[..bar].chars().count(), 1 + 23, "{header}");
        // The username right of the picture's columns, then when.
        let at = header.find("alice.example").unwrap();
        assert_eq!(header[bar..at].chars().count(), 2 + 5, "{header}");
        assert!(all[2].contains("Morning coffee"), "{all:#?}");
        let counts = all
            .iter()
            .position(|r| r.contains("554 likes · 37 replies · 1 repost · 18 shares"))
            .expect("the counts");
        assert!(all[counts].contains("1/3 ●○○"), "{}", all[counts]);
        assert!(
            all[counts + 1].contains("──────"),
            "a line before the next post"
        );
        // The second post goes on into the third: a line down its picture's
        // column instead of one across.
        let second = all
            .iter()
            .position(|r| r.contains("first of a thread"))
            .unwrap();
        let third = all
            .iter()
            .position(|r| r.contains("and the second"))
            .unwrap();
        let thread_col = 1 + 23 + 3;
        for r in &all[second + 1..third - 1] {
            assert_eq!(r.chars().nth(thread_col), Some('│'), "{all:#?}");
        }
    }
}
