//! The photo viewer: the photo as big as the window allows, keeping its
//! shape, over everything but the status bar.

use ratatui::Frame;
use ratatui::layout::Rect;
use ratatui::style::Stylize;
use ratatui::text::Line;
use ratatui_image::FontSize;
use ratatui_image::sliced::{SignedPosition, SlicedImage};

use super::{Cover, popup_block};
use crate::images::Images;
use crate::messages::Preview;
use crate::theme::Colors;
use crate::viewer::PhotoView;

pub(super) fn draw(
    frame: &mut Frame,
    area: Rect,
    view: &PhotoView,
    images: &mut Images,
    colors: &Colors,
) {
    let block = popup_block(
        " Photo ",
        " `o` open in its app · `y` copy · `Esc` close ",
        colors,
    );
    let inner = block.inner(area);
    frame.render_widget(Cover, area);
    frame.render_widget(block, area);
    if inner.is_empty() {
        return;
    }
    let (cols, rows) = fit(&view.photo, inner.width, inner.height, images.font_size());
    let at = SignedPosition::from((
        ((inner.width - cols) / 2) as i16,
        ((inner.height - rows) / 2) as i16,
    ));
    // The largest photo once it's ready; the bubble's until then.
    images.want(&view.photo, cols, rows);
    let shown = match &view.stand_in {
        Some(stand_in) if images.get(&view.photo, cols, rows).is_none() => {
            images.want(stand_in, cols, rows);
            stand_in
        }
        _ => &view.photo,
    };
    if let Some(image) = images.get(shown, cols, rows) {
        frame.render_widget(SlicedImage::new(image, at), inner);
        return;
    }
    let broken =
        images.is_broken(&view.photo) && view.stand_in.as_ref().is_none_or(|s| images.is_broken(s));
    let label = if broken {
        "Can't show this photo"
    } else {
        "Loading…"
    };
    let middle = Rect {
        y: inner.y + inner.height / 2,
        height: 1,
        ..inner
    };
    frame.render_widget(Line::from(label).fg(colors.subtle).centered(), middle);
}

/// Cells the photo takes: as many as fit in `cols` × `rows` with its shape
/// kept, since cells are taller than wide, but never more than its own
/// pixels, which would only blur it.
fn fit(photo: &Preview, cols: u16, rows: u16, font: FontSize) -> (u16, u16) {
    let (fw, fh) = (f64::from(font.width.max(1)), f64::from(font.height.max(1)));
    let (pw, ph) = (f64::from(photo.width), f64::from(photo.height));
    let scale = (f64::from(cols) * fw / pw)
        .min(f64::from(rows) * fh / ph)
        .min(1.0);
    let cells = |pixels: f64, cell: f64, most: u16| {
        ((pixels * scale / cell).round() as u16).clamp(1, most.max(1))
    };
    (cells(pw, fw, cols), cells(ph, fh, rows))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn photo(width: u32, height: u32) -> Preview {
        Preview {
            file_id: 1,
            width,
            height,
            thumbnail: None,
            sticker: false,
        }
    }

    /// Cells twice as tall as wide, as most fonts' are.
    const FONT: FontSize = FontSize {
        width: 10,
        height: 20,
    };

    #[test]
    fn a_wide_photo_takes_the_whole_width_and_rows_by_its_shape() {
        // 2000×1000 px in 100 columns: 1000×500 px of cells, 25 rows.
        assert_eq!(fit(&photo(2000, 1000), 100, 40, FONT), (100, 25));
    }

    #[test]
    fn a_tall_photo_takes_the_whole_height_and_columns_by_its_shape() {
        // 1000×2000 px in 40 rows: 400×800 px of cells, 40 columns.
        assert_eq!(fit(&photo(1000, 2000), 100, 40, FONT), (40, 40));
    }

    #[test]
    fn a_small_photo_is_never_enlarged_past_its_pixels() {
        assert_eq!(fit(&photo(300, 200), 100, 40, FONT), (30, 10));
    }

    #[test]
    fn a_tiny_window_still_shows_a_cell_of_it() {
        assert_eq!(fit(&photo(2000, 1000), 1, 1, FONT), (1, 1));
    }
}
