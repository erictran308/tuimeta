//! The photo viewer: Enter on a photo shows it as big as the window allows,
//! over everything but the status bar, instead of handing it to another app.

use crate::messages::{MediaFile, Msg, Preview};

/// A photo shown in the viewer.
pub struct PhotoView {
    pub chat_id: i64,
    /// The message it's in. The viewer closes once that's deleted, or
    /// edited to another photo.
    pub message_id: i64,
    /// The photo at its largest.
    pub photo: Preview,
    /// The bubble's smaller copy, or its blurry thumbnail, shown until the
    /// largest is ready.
    pub stand_in: Option<Preview>,
    /// What `o` opens and `y` copies.
    pub file: MediaFile,
}

impl PhotoView {
    /// The photo of message `message_id`, `msg`, if it has one to show.
    pub fn of(chat_id: i64, message_id: i64, msg: &Msg) -> Option<Self> {
        let photo = msg.photo.clone()?;
        let file = msg.file.clone().filter(|f| f.id == photo.file_id)?;
        Some(Self {
            chat_id,
            message_id,
            stand_in: msg.preview.clone(),
            photo,
            file,
        })
    }

    /// Message `message_id` of chat `chat_id`, now `msg`, no longer has the
    /// photo shown.
    pub fn lost(&self, chat_id: i64, message_id: i64, msg: Option<&Msg>) -> bool {
        chat_id == self.chat_id
            && message_id == self.message_id
            && msg.and_then(|m| m.photo.as_ref()).map(|p| p.file_id) != Some(self.photo.file_id)
    }
}
