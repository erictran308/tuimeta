//! The photo viewer: Enter on a photo shows it as big as the window allows,
//! over everything but the status bar, instead of handing it to another app.
//! `h` / `l` go to the photo before or after it, and `j` / `k` zoom in and
//! out, never past the size that fills the window, so all of it shows.

use crate::messages::{MediaFile, Msg, Preview};

/// The sizes `j` and `k` go through, as shares of the size that fills the
/// window.
pub const SIZES: [f64; 4] = [0.25, 0.5, 0.75, 1.0];

/// A photo shown in the viewer.
pub struct PhotoView {
    pub chat_id: i64,
    /// The message it's in. The viewer closes once that's deleted, or
    /// edited to another photo.
    pub message_id: i64,
    /// The photo at its largest.
    pub photo: Preview,
    /// The bubble's smaller copy, shown until the largest is downloaded.
    /// Without its blurry thumbnail, which would be one more window-sized
    /// picture to make and keep.
    pub stand_in: Option<Preview>,
    /// What `o` opens and `y` copies.
    pub file: MediaFile,
    /// The size `j` or `k` picked, in [`SIZES`]. `None` until then: as big
    /// as fills the window, but not past its own pixels.
    pub zoom: Option<usize>,
    /// The share of the size that fills the window it opens at, from the
    /// last frame: less than all for a photo smaller than the window.
    pub natural: f64,
}

impl PhotoView {
    /// The photo of message `message_id`, `msg`, if it has one to show.
    /// A view-once photo never has one: it's only a label here.
    pub fn of(chat_id: i64, message_id: i64, msg: &Msg) -> Option<Self> {
        let photo = msg.photo.clone()?;
        let file = msg.file.clone().filter(|f| f.id == photo.file_id)?;
        Some(Self {
            chat_id,
            message_id,
            stand_in: msg.preview.clone().map(|p| Preview {
                thumbnail: None,
                ..p
            }),
            photo,
            file,
            zoom: None,
            natural: 1.0,
        })
    }

    /// How big it is, as a share of the size that fills the window.
    pub fn share(&self) -> f64 {
        self.zoom.map_or(self.natural, |i| SIZES[i])
    }

    /// The next size up, up to filling the window.
    pub fn zoom_in(&mut self) {
        let now = self.share();
        if let Some(i) = SIZES.iter().position(|&s| s > now + 0.001) {
            self.zoom = Some(i);
        }
    }

    /// The next size down.
    pub fn zoom_out(&mut self) {
        let now = self.share();
        if let Some(i) = SIZES.iter().rposition(|&s| s < now - 0.001) {
            self.zoom = Some(i);
        }
    }

    /// Message `message_id` of chat `chat_id`, now `msg`, no longer has the
    /// photo shown.
    pub fn lost(&self, chat_id: i64, message_id: i64, msg: Option<&Msg>) -> bool {
        chat_id == self.chat_id
            && message_id == self.message_id
            && msg.and_then(|m| m.photo.as_ref()).map(|p| p.file_id) != Some(self.photo.file_id)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn view(natural: f64) -> PhotoView {
        let photo = Preview {
            file_id: 1,
            width: 300,
            height: 200,
            thumbnail: None,
            sticker: false,
        };
        PhotoView {
            chat_id: 1,
            message_id: 1,
            photo,
            stand_in: None,
            file: MediaFile {
                id: 1,
                label: "Photo".into(),
                photo: true,
            },
            zoom: None,
            natural,
        }
    }

    #[test]
    fn a_photo_filling_the_window_only_zooms_out_and_back() {
        let mut view = view(1.0);
        view.zoom_in();
        assert_eq!(view.zoom, None, "it can't get bigger");
        view.zoom_out();
        assert_eq!(view.share(), 0.75);
        for _ in 0..5 {
            view.zoom_out();
        }
        assert_eq!(view.share(), 0.25);
        for _ in 0..5 {
            view.zoom_in();
        }
        assert_eq!(view.share(), 1.0, "filling the window at most");
    }

    #[test]
    fn a_small_photo_zooms_in_from_its_own_size_up_to_filling_the_window() {
        let mut view = view(0.3);
        view.zoom_in();
        assert_eq!(view.share(), 0.5);
        let mut view = self::view(0.3);
        view.zoom_out();
        assert_eq!(view.share(), 0.25);
    }
}
