//! Photos, shown inline in chat bubbles, and chat photos in the chat list.
//!
//! Every frame, the message view and the chat list ask for the photos they
//! have on screen. After the frame, [`Images::fetch`] starts whatever is
//! missing: a download through the helper, then decode + encode for the terminal on a
//! blocking thread. Until the real photo is ready, the blurry thumbnail that
//! came with the message (or chat) stands in.

use std::cell::Cell;
use std::collections::{HashMap, HashSet};
use std::panic::{AssertUnwindSafe, catch_unwind};
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};

use anyhow::Result;
use image::imageops::FilterType;
use image::{DynamicImage, RgbaImage};
use ratatui::layout::Size;
use ratatui_image::picker::{Picker, ProtocolType};
use ratatui_image::sliced::SlicedProtocol;
use ratatui_image::{FontSize, Resize};
use tokio::sync::mpsc::UnboundedSender;

use crate::chats::ChatPhoto;
use crate::messages::Preview;
use crate::meta::Meta;

/// Scale both ways (`Fit` never enlarges, which would leave the tiny thumbnail
/// tiny). Triangle filtering keeps the enlarged thumbnail soft, not blocky.
const RESIZE: Resize = Resize::Scale(Some(FilterType::Triangle));

/// Chat photos kept encoded; the ones drawn longest ago go first. Enough for
/// several screens of the list.
const MAX_AVATARS: usize = 300;

/// One encoded image: a photo (or its thumbnail) at one size in cells.
#[derive(Clone, Copy, PartialEq, Eq, Hash)]
pub struct Key {
    pub file_id: i32,
    pub cols: u16,
    pub rows: u16,
    /// The blurry thumbnail embedded in the message, not the downloaded photo.
    pub thumbnail: bool,
    /// A chat photo for the chat list, cut to a circle.
    pub avatar: bool,
}

/// The downloaded photo itself at `cols` × `rows` cells.
fn sharp_key(photo: &Preview, cols: u16, rows: u16) -> Key {
    Key {
        file_id: photo.file_id,
        cols,
        rows,
        thumbnail: false,
        avatar: false,
    }
}

/// Caps for decoding images from other people. Meta's networks send photos
/// at most about 2048px a side (WhatsApp's HD ones 4096px) and stickers
/// 512px, so a much bigger one is only a way to run memory out.
fn limits() -> image::Limits {
    sized(4096)
}

/// Caps of `side` pixels a side. Decoders allocate more than `max_alloc`
/// counts (WebP's scratch and frame buffers), so the side is what bounds it.
fn sized(side: u32) -> image::Limits {
    let mut limits = image::Limits::default();
    limits.max_image_width = Some(side);
    limits.max_image_height = Some(side);
    limits.max_alloc = Some(128 * 1024 * 1024);
    limits
}

/// Stickers are 512px a side; this leaves room to spare.
const STICKER_SIDE: u32 = 1024;

/// Like `image::open`, within [`limits`].
pub fn open_image(path: &str) -> image::ImageResult<image::DynamicImage> {
    open_within(path, limits())
}

/// The format comes from the file's first bytes, and only failing that from
/// its extension: Messenger names some PNG screenshots' previews JPEG, so
/// they're saved as `.jpg`.
fn open_within(path: &str, limits: image::Limits) -> image::ImageResult<image::DynamicImage> {
    let mut reader = image::ImageReader::open(path)?.with_guessed_format()?;
    picture_format(reader.format())?;
    reader.limits(limits);
    reader.decode()
}

/// The formats Meta's networks send pictures in, and the ones tuimeta
/// sends; anything else is refused before it's decoded. The `image` crate
/// reads many more, from the file's first bytes, whatever its name, and
/// some decode on threads of their own (OpenEXR), where a panic isn't
/// caught and would end the app.
fn picture_format(format: Option<image::ImageFormat>) -> image::ImageResult<()> {
    use image::ImageFormat::{Gif, Jpeg, Png, WebP};
    use image::error::{ImageError, ImageFormatHint};
    match format {
        Some(Png | Jpeg | Gif | WebP) => Ok(()),
        other => Err(ImageError::Unsupported(
            other
                .map_or(ImageFormatHint::Unknown, ImageFormatHint::from)
                .into(),
        )),
    }
}

/// At most this many images decode at once; the rest wait for a later frame.
/// Each may take a few hundred MB while it decodes.
const MAX_BUILDING: usize = 4;

/// Decoders running, counting the ones given up on after [`BUILD_TIMEOUT`],
/// which can't be stopped: past this many, nothing new starts until some
/// finish, so stuck ones can't pile up threads and memory without end.
const MAX_RUNNING: usize = 2 * MAX_BUILDING;

/// One decoder running, counted in [`Images::running`] until it's dropped,
/// however its thread ends (or if it never starts).
struct Running(Arc<AtomicUsize>);

impl Running {
    fn new(count: &Arc<AtomicUsize>) -> Self {
        count.fetch_add(1, Ordering::SeqCst);
        Running(count.clone())
    }
}

impl Drop for Running {
    fn drop(&mut self) {
        self.0.fetch_sub(1, Ordering::SeqCst);
    }
}

/// Encoded photos kept for the open chat; the least recently drawn go first.
const MAX_READY: usize = 200;

/// More cells than any photo in a bubble (40 × 16): the photo viewer's, and
/// the feed's.
const LARGE_CELLS: u32 = 40 * 16;

/// Of those, only this many pixels' worth are kept, always the newest: each
/// pixel takes four bytes and more (kitty's keeps all it sent). That's the
/// feed's posts in view and the few made ahead, or one or two of the
/// viewer's window-sized photos.
const LARGE_PIXELS: u64 = 10_000_000;

fn large(key: &Key) -> bool {
    u32::from(key.cols) * u32::from(key.rows) > LARGE_CELLS
}

/// The pixels `key`'s image takes, with cells of `font`'s size.
fn pixels(key: &Key, font: FontSize) -> u64 {
    u64::from(key.cols) * u64::from(font.width) * u64::from(key.rows) * u64::from(font.height)
}

thread_local! {
    /// Set while this thread decodes another user's image.
    static DECODING: Cell<bool> = const { Cell::new(false) };
}

/// The panic is in an image decoder, where it's caught: the image shows as
/// broken and the app carries on, rather than every chat holding that image
/// becoming one that can't be opened.
pub fn panic_is_contained() -> bool {
    DECODING.with(Cell::get)
}

/// Runs `decode`, with a panic in it caught and turned into an error. Every
/// decode of another user's image goes through this.
pub fn contained<T>(decode: impl FnOnce() -> Result<T>) -> Result<T> {
    DECODING.with(|d| d.set(true));
    let result = catch_unwind(AssertUnwindSafe(decode))
        .unwrap_or_else(|_| Err(anyhow::anyhow!("the image couldn't be decoded")));
    DECODING.with(|d| d.set(false));
    result
}

/// An image still building after this long counts as failed.
const BUILD_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(30);

/// Like `image::load_from_memory`, within [`limits`].
fn decode_bytes(data: &[u8]) -> image::ImageResult<image::DynamicImage> {
    let mut reader = image::ImageReader::new(std::io::Cursor::new(data)).with_guessed_format()?;
    picture_format(reader.format())?;
    reader.limits(limits());
    reader.decode()
}

/// `photo` cut to a circle in the middle of a `width`×`height` canvas, which
/// is transparent around it, so the row's background shows there. The edge
/// fades out over a pixel to look round rather than jagged.
pub fn circle(photo: &DynamicImage, width: u32, height: u32) -> RgbaImage {
    let mut canvas = RgbaImage::new(width, height);
    let size = width.min(height);
    let side = photo.width().min(photo.height());
    if size == 0 || side == 0 {
        return canvas;
    }
    let square = photo
        .crop_imm(
            (photo.width() - side) / 2,
            (photo.height() - side) / 2,
            side,
            side,
        )
        .resize_exact(size, size, FilterType::Triangle)
        .to_rgba8();
    let (left, top) = ((width - size) / 2, (height - size) / 2);
    let radius = size as f32 / 2.0;
    for (x, y, pixel) in square.enumerate_pixels() {
        let dx = x as f32 + 0.5 - radius;
        let dy = y as f32 + 0.5 - radius;
        let inside = (radius - (dx * dx + dy * dy).sqrt() + 0.5).clamp(0.0, 1.0);
        let mut pixel = *pixel;
        pixel[3] = (f32::from(pixel[3]) * inside).round() as u8;
        canvas.put_pixel(left + x, top + y, pixel);
    }
    canvas
}

/// An image finished encoding (or failed) on a background thread.
pub struct ImageEvent {
    key: Key,
    /// [`Images::generation`] when it was started.
    generation: u64,
    result: Result<SlicedProtocol>,
}

enum FileState {
    /// Asked for before it's drawn ([`Images::prefetch`]): behind the
    /// others, and a failure isn't shown.
    Ahead,
    Downloading,
    Ready(String),
    Failed,
}

/// An encoded chat photo.
struct Avatar {
    image: SlicedProtocol,
    /// When it was last drawn, in calls to [`Images::avatar`].
    used: u64,
    /// It has been drawn. Kitty's protocol sends an image to the terminal
    /// the first time it's drawn, and only then, so that time must not be
    /// under a popup that hides it.
    shown: bool,
}

/// An encoded photo.
struct Encoded {
    image: SlicedProtocol,
    /// The frame it was last drawn in.
    used: u64,
    /// It has been drawn, not under a popup: see [`Avatar::shown`].
    shown: bool,
}

/// How much a cell's measured width or height must change, as a share of
/// what it was, to count as a new cell size rather than rounding: a window
/// whose pixels include its padding measures a little more per cell the
/// fewer rows it has. A screen of another density changes it by a quarter
/// or more, a step of the font's size by about a fourteenth.
const CELL_CHANGE: u32 = 16;

/// The pixel size of a cell as the terminal's window size says: its size
/// in pixels over its columns and rows. None where it doesn't say, as on
/// Windows, so cells keep the size the terminal answered at startup.
pub fn measure_cells() -> Option<FontSize> {
    let size = crossterm::terminal::window_size().ok()?;
    if size.columns == 0 || size.rows == 0 || size.width == 0 || size.height == 0 {
        return None;
    }
    Some(FontSize::new(
        size.width / size.columns,
        size.height / size.rows,
    ))
}

/// The cells' size in pixels, for a terminal that answered `queried` at
/// startup, when its window measured `then`, and measures `now` (see
/// [`measure_cells`]): as answered, unless they measure noticeably bigger
/// or smaller, then grown or shrunk as much. The answer is kept as the
/// truth, since a window's measure may count its padding.
fn cells_now(queried: FontSize, then: FontSize, now: FontSize) -> FontSize {
    let changed =
        |then: u16, now: u16| u32::from(then.abs_diff(now)) * CELL_CHANGE > u32::from(then);
    if !changed(then.width, now.width) && !changed(then.height, now.height) {
        return queried;
    }
    let scale = |queried: u16, then: u16, now: u16| {
        let (queried, then, now) = (u32::from(queried), u32::from(then.max(1)), u32::from(now));
        ((queried * now + then / 2) / then).clamp(1, u32::from(u16::MAX)) as u16
    };
    FontSize::new(
        scale(queried.width, then.width, now.width),
        scale(queried.height, then.height, now.height),
    )
}

pub struct Images {
    picker: Picker,
    /// The cell size the terminal answered at startup.
    queried: FontSize,
    /// What its window measured its cells as then: see [`Images::measured`].
    measured: Option<FontSize>,
    tx: UnboundedSender<ImageEvent>,
    /// Encoded photos.
    ready: HashMap<Key, Encoded>,
    /// Counts frames, for `ready`.
    frame: u64,
    /// Goes up when the files are forgotten, for a new session: an
    /// image still being built for the old one numbers its file the old way,
    /// and is dropped when it's done. Also when cells change size, which
    /// leaves one being built the wrong size.
    generation: u64,
    /// Kept apart from `ready`: they stay when another chat is opened.
    avatars: HashMap<Key, Avatar>,
    /// Counts calls to [`Images::avatar`], to find the least recently drawn.
    avatar_clock: u64,
    /// Images being built, and when each started.
    building: HashMap<Key, std::time::Instant>,
    /// Decoders still running, given up on or not.
    running: Arc<AtomicUsize>,
    /// Keys that failed to decode; never retried.
    failed: HashSet<Key>,
    files: HashMap<i32, FileState>,
    /// Photos the last frame showed, with their size.
    wanted: Vec<(Preview, u16, u16)>,
    /// Chat photos the last frame showed, with their size.
    wanted_avatars: Vec<(ChatPhoto, u16, u16)>,
    /// Photos about to come into view, with the size to make each into an
    /// image at if it's to be made before it's drawn: see
    /// [`Images::prefetch`] and [`Images::prepare`].
    ahead: Vec<(Preview, Option<(u16, u16)>)>,
    /// Images being made before they're drawn ([`Images::prepare`]).
    building_ahead: HashSet<Key>,
    /// What the last frame drew, and what it asked to have made ahead and
    /// there was room for: never pushed out, so nothing is made over and
    /// over, each pushing out the last.
    in_use: HashSet<Key>,
}

impl Images {
    pub fn new(picker: Picker, tx: UnboundedSender<ImageEvent>) -> Self {
        Self {
            queried: picker.font_size(),
            measured: None,
            picker,
            tx,
            ready: HashMap::new(),
            frame: 0,
            generation: 0,
            avatars: HashMap::new(),
            avatar_clock: 0,
            building: HashMap::new(),
            running: Arc::new(AtomicUsize::new(0)),
            failed: HashSet::new(),
            files: HashMap::new(),
            wanted: Vec::new(),
            wanted_avatars: Vec::new(),
            ahead: Vec::new(),
            building_ahead: HashSet::new(),
            in_use: HashSet::new(),
        }
    }

    /// False when the terminal shows no real images, only colored blocks
    /// (halfblocks), which are too coarse for a chat photo in a few cells.
    pub fn draws_photos(&self) -> bool {
        self.picker.protocol_type() != ProtocolType::Halfblocks
    }

    /// The terminal paints each image (or each of its rows) from one cell,
    /// and repaints the whole of it whenever that cell is sent again, over
    /// anything drawn on top since: sixel and iTerm2's protocol.
    pub fn paints_over(&self) -> bool {
        matches!(
            self.picker.protocol_type(),
            ProtocolType::Sixel | ProtocolType::Iterm2
        )
    }

    /// Pixel size of one terminal cell, for sizing photos by aspect ratio.
    pub fn font_size(&self) -> FontSize {
        self.picker.font_size()
    }

    /// The terminal's window measures its cells `now` (see
    /// [`measure_cells`]): first right after the terminal answered at
    /// startup, then whenever its size may have changed. Cells change size
    /// in pixels when the window moves to a screen of another density, or
    /// the font is made bigger or smaller. True if they did: photos and
    /// chat photos are then encoded again at the new size, since sixel and
    /// iTerm2's protocol draw them as many pixels as they were made, too
    /// big or too small for the cells, and kitty's would be blurred or
    /// wasted. The screen must then be drawn afresh.
    pub fn measured(&mut self, now: Option<FontSize>) -> bool {
        let Some(now) = now else {
            return false;
        };
        let Some(then) = self.measured else {
            self.measured = Some(now);
            return false;
        };
        let font = cells_now(self.queried, then, now);
        let old = self.picker.font_size();
        if (font.width, font.height) == (old.width, old.height) {
            return false;
        }
        // A picker takes its cell size only when it's made, from the
        // terminal's answer, which can't be asked for again while keys are
        // read. This one keeps the protocol found then, but not kitty's
        // compression, so kitty is sent its images uncompressed.
        let protocol = self.picker.protocol_type();
        #[allow(deprecated)]
        let mut picker = Picker::from_fontsize(font);
        picker.set_protocol_type(protocol);
        self.picker = picker;
        self.generation += 1;
        self.ready.clear();
        self.avatars.clear();
        self.building.clear();
        self.building_ahead.clear();
        true
    }

    /// The best image ready to draw: the photo, else its blurry thumbnail.
    /// While `covered` (a popup may be over it), only one drawn before: see
    /// [`Avatar::shown`].
    pub fn get(
        &mut self,
        photo: &Preview,
        cols: u16,
        rows: u16,
        covered: bool,
    ) -> Option<&SlicedProtocol> {
        let key = |thumbnail| Key {
            file_id: photo.file_id,
            cols,
            rows,
            thumbnail,
            avatar: false,
        };
        let key = [key(false), key(true)]
            .into_iter()
            .find(|k| self.ready.get(k).is_some_and(|e| e.shown || !covered))?;
        self.drawn(key)
    }

    /// Like [`get`](Self::get), but only the photo itself, not its blurry
    /// thumbnail.
    pub fn sharp(
        &mut self,
        photo: &Preview,
        cols: u16,
        rows: u16,
        covered: bool,
    ) -> Option<&SlicedProtocol> {
        if !self.has_sharp(photo, cols, rows, covered) {
            return None;
        }
        self.drawn(sharp_key(photo, cols, rows))
    }

    /// [`sharp`](Self::sharp) would give the photo. Asking doesn't count it
    /// drawn: with kitty's protocol a photo counted drawn but never sent
    /// would come up blank under a popup.
    pub fn has_sharp(&self, photo: &Preview, cols: u16, rows: u16, covered: bool) -> bool {
        self.ready
            .get(&sharp_key(photo, cols, rows))
            .is_some_and(|e| e.shown || !covered)
    }

    /// The photo `key`, drawn in this frame.
    fn drawn(&mut self, key: Key) -> Option<&SlicedProtocol> {
        let frame = self.frame;
        let encoded = self.ready.get_mut(&key)?;
        encoded.used = frame;
        encoded.shown = true;
        Some(&encoded.image)
    }

    /// The photo's file is downloaded.
    pub fn downloaded(&self, photo: &Preview) -> bool {
        matches!(self.files.get(&photo.file_id), Some(FileState::Ready(_)))
    }

    /// True when the photo can't be shown: its download or decode failed.
    pub fn is_broken(&self, photo: &Preview) -> bool {
        matches!(self.files.get(&photo.file_id), Some(FileState::Failed))
    }

    /// Called while drawing, for each photo on screen.
    pub fn want(&mut self, photo: &Preview, cols: u16, rows: u16) {
        self.wanted.push((photo.clone(), cols, rows));
    }

    /// Called while drawing, for a photo about to come into view, such as
    /// the next posts' in the feed: its file downloads now, behind the ones
    /// on screen, so it's there once it's drawn.
    pub fn prefetch(&mut self, photo: &Preview) {
        self.ahead.push((photo.clone(), None));
    }

    /// Like [`prefetch`](Self::prefetch), for a photo coming into view next:
    /// once downloaded, it's made into an image at `cols` × `rows` while a
    /// decoder is free for it, so it's there as soon as it's drawn.
    pub fn prepare(&mut self, photo: &Preview, cols: u16, rows: u16) {
        self.ahead.push((photo.clone(), Some((cols, rows))));
    }

    /// The best chat photo ready to draw, as for [`get`](Self::get). While
    /// `covered` (a popup may be over it), only one that has been drawn before
    /// is returned; see [`Avatar::shown`].
    pub fn avatar(
        &mut self,
        photo: &ChatPhoto,
        cols: u16,
        rows: u16,
        covered: bool,
    ) -> Option<&SlicedProtocol> {
        let key = |thumbnail| Key {
            file_id: photo.file_id,
            cols,
            rows,
            thumbnail,
            avatar: true,
        };
        // See paints_over: a popup may be over the list.
        if covered && self.paints_over() {
            return None;
        }
        let key = [key(false), key(true)]
            .into_iter()
            .find(|k| self.avatars.get(k).is_some_and(|a| a.shown || !covered))?;
        self.avatar_clock += 1;
        let avatar = self.avatars.get_mut(&key)?;
        avatar.used = self.avatar_clock;
        avatar.shown = true;
        Some(&avatar.image)
    }

    /// Called while drawing, for each chat photo on screen.
    pub fn want_avatar(&mut self, photo: &ChatPhoto, cols: u16, rows: u16) {
        self.wanted_avatars.push((photo.clone(), cols, rows));
    }

    /// Starts downloads and encodes for what the last frame wanted.
    pub fn fetch(&mut self, meta: &Meta) {
        self.frame += 1;
        self.fetch_avatars(meta);
        let mut in_use = HashSet::new();
        for (photo, cols, rows) in std::mem::take(&mut self.wanted) {
            let full = Key {
                file_id: photo.file_id,
                cols,
                rows,
                thumbnail: false,
                avatar: false,
            };
            in_use.insert(full);
            if photo.thumbnail.is_some() {
                in_use.insert(Key {
                    thumbnail: true,
                    ..full
                });
            }
            if self.ready.contains_key(&full) {
                continue;
            }
            match self.files.get(&photo.file_id) {
                Some(FileState::Ready(path)) => {
                    let path = path.clone();
                    self.build_file(full, &photo, path);
                }
                Some(FileState::Downloading | FileState::Failed) => {}
                // One asked for ahead is on screen now: it goes before the
                // others.
                Some(FileState::Ahead) | None => {
                    self.files.insert(photo.file_id, FileState::Downloading);
                    meta.download(photo.file_id);
                }
            }
            if let Some(data) = photo.thumbnail {
                let key = Key {
                    thumbnail: true,
                    ..full
                };
                self.build(key, move || Ok(decode_bytes(&data)?));
            }
        }
        // Made before they're drawn only within the budget for large ones,
        // what's on screen counted first, so what's made stays; nearest
        // first, leaving half the decoders free for what comes on screen.
        let font = self.picker.font_size();
        let mut working: u64 = in_use
            .iter()
            .filter(|k| large(k))
            .map(|k| pixels(k, font))
            .sum();
        for (photo, size) in std::mem::take(&mut self.ahead) {
            self.files.entry(photo.file_id).or_insert_with(|| {
                meta.download_quiet(photo.file_id);
                FileState::Ahead
            });
            let Some((cols, rows)) = size else {
                continue;
            };
            let key = sharp_key(&photo, cols, rows);
            if in_use.contains(&key) || self.failed.contains(&key) {
                continue;
            }
            if large(&key) {
                if working + pixels(&key, font) > LARGE_PIXELS {
                    continue;
                }
                working += pixels(&key, font);
            }
            in_use.insert(key);
            if let Some(FileState::Ready(path)) = self.files.get(&photo.file_id)
                && !self.ready.contains_key(&key)
                && !self.building.contains_key(&key)
                && self.building.len() < MAX_BUILDING / 2
            {
                let path = path.clone();
                self.build_file(key, &photo, path);
                if self.building.contains_key(&key) {
                    self.building_ahead.insert(key);
                }
            }
        }
        self.in_use = in_use;
    }

    /// Makes the downloaded `photo`, at `path`, into an image of `key`'s
    /// size.
    fn build_file(&mut self, key: Key, photo: &Preview, path: String) {
        let limits = if photo.sticker {
            sized(STICKER_SIDE)
        } else {
            limits()
        };
        self.build(key, move || Ok(open_within(&path, limits)?));
    }

    fn fetch_avatars(&mut self, meta: &Meta) {
        let font = self.picker.font_size();
        for (photo, cols, rows) in std::mem::take(&mut self.wanted_avatars) {
            let full = Key {
                file_id: photo.file_id,
                cols,
                rows,
                thumbnail: false,
                avatar: true,
            };
            if self.avatars.contains_key(&full) {
                continue;
            }
            let (width, height) = (
                u32::from(cols) * u32::from(font.width),
                u32::from(rows) * u32::from(font.height),
            );
            let file = self
                .files
                .entry(photo.file_id)
                .or_insert_with(|| match &photo.path {
                    Some(path) => FileState::Ready(path.clone()),
                    None => {
                        meta.download_quiet(photo.file_id);
                        FileState::Downloading
                    }
                });
            if let FileState::Ready(path) = file {
                let path = path.clone();
                self.build(full, move || {
                    Ok(circle(&open_image(&path)?, width, height).into())
                });
            }
            if let Some(data) = photo.thumbnail {
                let key = Key {
                    thumbnail: true,
                    ..full
                };
                self.build(key, move || {
                    Ok(circle(&decode_bytes(&data)?, width, height).into())
                });
            }
        }
    }

    pub fn on_downloaded(&mut self, file_id: i32, path: Option<String>) {
        let state = path.map_or(FileState::Failed, FileState::Ready);
        self.files.insert(file_id, state);
    }

    /// The photo numbered `file_id` is a file on this computer, such as one
    /// about to be sent: it's read from `path`, never downloaded. Called
    /// while drawing, before [`want`](Self::want); a photo that failed to
    /// decode stays broken.
    pub fn local_file(&mut self, file_id: i32, path: &str) {
        self.files
            .entry(file_id)
            .or_insert_with(|| FileState::Ready(path.into()));
    }

    pub fn on_built(&mut self, event: ImageEvent) {
        if event.generation != self.generation {
            return;
        }
        self.building.remove(&event.key);
        let ahead = self.building_ahead.remove(&event.key);
        match event.result {
            Ok(image) if event.key.avatar => self.add_avatar(event.key, image),
            // Made ahead for what's no longer coming, as when the feed was
            // left: it would only take memory.
            Ok(_) if ahead && !self.in_use.contains(&event.key) => {}
            Ok(image) => self.add_ready(event.key, image),
            Err(_) => {
                self.failed.insert(event.key);
                if !event.key.thumbnail {
                    self.files.insert(event.key.file_id, FileState::Failed);
                }
            }
        }
    }

    /// Keeps `image`, making room for it within [`LARGE_PIXELS`] and
    /// [`MAX_READY`] from the least recently drawn, but never from what's in
    /// use: that would be wanted again at once, made again and push out
    /// another, without end. What's in use may go past the budget.
    fn add_ready(&mut self, key: Key, image: SlicedProtocol) {
        let font = self.picker.font_size();
        if large(&key) {
            let mut kept: u64 = self
                .ready
                .keys()
                .filter(|k| large(k))
                .map(|k| pixels(k, font))
                .sum();
            while kept + pixels(&key, font) > LARGE_PIXELS
                && let Some(oldest) = self.oldest(true)
            {
                self.ready.remove(&oldest);
                kept -= pixels(&oldest, font);
            }
        }
        if self.ready.len() >= MAX_READY
            && let Some(oldest) = self.oldest(false)
        {
            self.ready.remove(&oldest);
        }
        let encoded = Encoded {
            image,
            used: self.frame,
            shown: false,
        };
        self.ready.insert(key, encoded);
    }

    /// The least recently drawn image kept that isn't in use; with
    /// `only_large`, of the large ones.
    fn oldest(&self, only_large: bool) -> Option<Key> {
        self.ready
            .iter()
            .filter(|(k, _)| !self.in_use.contains(k) && (!only_large || large(k)))
            .min_by_key(|(_, e)| e.used)
            .map(|(&k, _)| k)
    }

    fn add_avatar(&mut self, key: Key, image: SlicedProtocol) {
        if self.avatars.len() >= MAX_AVATARS
            && let Some(oldest) = self
                .avatars
                .iter()
                .min_by_key(|(_, a)| a.used)
                .map(|(&k, _)| k)
        {
            self.avatars.remove(&oldest);
        }
        let avatar = Avatar {
            image,
            used: self.avatar_clock,
            shown: false,
        };
        self.avatars.insert(key, avatar);
    }

    /// Drops encoded photos, e.g. when switching chats. Chat photos and
    /// downloads stay.
    pub fn clear(&mut self) {
        self.ready.clear();
        // Nothing made ahead for what was left is kept when it's done.
        self.in_use.clear();
    }

    /// Forgets every picture and where each downloaded file is, as when a
    /// network was logged out of and its downloads deleted. Which files were
    /// whose isn't known here, so all go: one still being made is dropped
    /// when done, and the others' files are asked for again, which the
    /// helper answers at once for those it still has.
    pub fn forget(&mut self) {
        self.generation += 1;
        self.ready.clear();
        self.avatars.clear();
        self.building.clear();
        self.building_ahead.clear();
        self.failed.clear();
        self.files.clear();
    }

    /// Decodes and encodes on a blocking thread, once per key.
    fn build(
        &mut self,
        key: Key,
        decode: impl FnOnce() -> Result<image::DynamicImage> + Send + 'static,
    ) {
        let built = if key.avatar {
            self.avatars.contains_key(&key)
        } else {
            self.ready.contains_key(&key)
        };
        // One that never finishes mustn't hold its place for good: it counts
        // as failed, and the others get their turn.
        let (failed, ahead) = (&mut self.failed, &mut self.building_ahead);
        self.building.retain(|&key, started| {
            let alive = started.elapsed() < BUILD_TIMEOUT;
            if !alive {
                failed.insert(key);
                ahead.remove(&key);
            }
            alive
        });
        if built
            || self.building.contains_key(&key)
            || self.failed.contains(&key)
            || self.building.len() >= MAX_BUILDING
            || self.running.load(Ordering::SeqCst) >= MAX_RUNNING
        {
            return;
        }
        self.building.insert(key, std::time::Instant::now());
        let picker = self.picker.clone();
        let tx = self.tx.clone();
        let generation = self.generation;
        let running = Running::new(&self.running);
        tokio::task::spawn_blocking(move || {
            let result = {
                let _running = running;
                contained(|| {
                    decode().and_then(|image| {
                        let size = Size::new(key.cols, key.rows);
                        Ok(SlicedProtocol::new_with_resize(
                            &picker, image, size, RESIZE,
                        )?)
                    })
                })
            };
            let _ = tx.send(ImageEvent {
                key,
                generation,
                result,
            });
        });
    }

    #[cfg(test)]
    pub fn insert_ready(&mut self, key: Key, image: image::DynamicImage) {
        let size = Size::new(key.cols, key.rows);
        let image = SlicedProtocol::new_with_resize(&self.picker, image, size, RESIZE).unwrap();
        if key.avatar {
            self.add_avatar(key, image);
        } else {
            self.add_ready(key, image);
        }
    }
}

/// Shared thumbnail bytes, so cloning a [`Preview`] each frame is cheap.
pub type Thumbnail = Arc<[u8]>;

#[cfg(test)]
mod tests {
    use super::*;

    fn png(width: u32, height: u32) -> Vec<u8> {
        let mut data = std::io::Cursor::new(Vec::new());
        image::RgbImage::new(width, height)
            .write_to(&mut data, image::ImageFormat::Png)
            .unwrap();
        data.into_inner()
    }

    #[test]
    fn chat_photos_are_cut_to_a_circle_with_see_through_corners() {
        let photo = image::RgbaImage::from_pixel(160, 100, image::Rgba([255, 0, 0, 255])).into();
        let round = circle(&photo, 40, 44);
        let alpha = |x, y| round.get_pixel(x, y)[3];
        assert_eq!(alpha(0, 2), 0, "corner");
        assert_eq!(alpha(39, 41), 0, "corner");
        assert_eq!(alpha(20, 22), 255, "middle");
        assert_eq!(alpha(20, 0), 0, "above the circle: the canvas is taller");
        assert_eq!(alpha(1, 22), 255, "the circle reaches the sides");
        let edge = alpha(6, 7);
        assert!(edge > 0 && edge < 255, "a soft edge: {edge}");
        assert_eq!(circle(&photo, 0, 44).dimensions(), (0, 44), "no panic");
    }

    fn key(file_id: i32) -> Key {
        Key {
            file_id,
            cols: 4,
            rows: 2,
            thumbnail: false,
            avatar: false,
        }
    }

    fn preview(file_id: i32) -> Preview {
        Preview {
            file_id,
            width: 8,
            height: 8,
            thumbnail: None,
            sticker: false,
        }
    }

    #[tokio::test]
    async fn a_decoder_panic_marks_the_photo_broken_instead_of_ending_the_app() {
        let (tx, mut rx) = tokio::sync::mpsc::unbounded_channel();
        let mut images = Images::new(Picker::halfblocks(), tx);
        images.build(key(7), || panic!("a decoder bug"));
        images.on_built(rx.recv().await.unwrap());
        assert!(images.is_broken(&preview(7)));
        assert!(!panic_is_contained(), "only while decoding");
    }

    #[tokio::test]
    async fn forgetting_drops_every_picture_and_file_and_what_was_still_being_made() {
        let (tx, mut rx) = tokio::sync::mpsc::unbounded_channel();
        let mut images = Images::new(Picker::halfblocks(), tx);
        images.on_downloaded(5, Some("/data/helper/files/5.jpg".into()));
        images.insert_ready(key(6), DynamicImage::new_rgba8(8, 8));
        images.build(key(7), || Ok(DynamicImage::new_rgba8(8, 8)));
        let late = rx.recv().await.unwrap();

        images.forget();
        assert!(images.files.is_empty() && images.ready.is_empty());
        assert!(images.building.is_empty());
        images.on_built(late);
        assert!(images.ready.is_empty(), "made for what was forgotten");
    }

    fn cells_after(queried: (u16, u16), then: (u16, u16), now: (u16, u16)) -> (u16, u16) {
        let font = |(w, h)| FontSize::new(w, h);
        let size = cells_now(font(queried), font(then), font(now));
        (size.width, size.height)
    }

    #[test]
    fn cells_keep_the_size_the_terminal_answered_unless_they_measure_another() {
        assert_eq!(cells_after((20, 40), (20, 40), (20, 40)), (20, 40));
        // A window that counts its padding measures a pixel more per cell
        // with few rows: not a new size.
        assert_eq!(cells_after((20, 40), (20, 40), (20, 41)), (20, 40));
        // Moved from a Retina screen to one of half its density.
        assert_eq!(cells_after((20, 40), (20, 40), (10, 20)), (10, 20));
        assert_eq!(cells_after((10, 20), (10, 20), (20, 40)), (20, 40));
        // Measured with its padding, the answer is scaled, not replaced.
        assert_eq!(cells_after((20, 40), (21, 42), (10, 21)), (10, 20));
        // A step of the font's size.
        assert_eq!(cells_after((14, 28), (14, 28), (15, 30)), (15, 30));
    }

    #[tokio::test]
    async fn photos_are_made_again_once_cells_change_size() {
        let (tx, mut rx) = tokio::sync::mpsc::unbounded_channel();
        let mut images = Images::new(Picker::halfblocks(), tx);
        let start = images.font_size();
        let measure = |w, h| Some(FontSize::new(w, h));
        assert!(
            !images.measured(measure(10, 20)),
            "the first is the startup's"
        );
        assert!(!images.measured(None), "a terminal that doesn't say");
        assert!(!images.measured(measure(10, 20)));
        images.insert_ready(key(1), DynamicImage::new_rgba8(8, 8));

        // One on its way is the old size.
        let (go, wait) = std::sync::mpsc::channel::<()>();
        images.build(key(7), move || {
            wait.recv().ok();
            Ok(DynamicImage::new_rgba8(8, 8))
        });
        assert!(images.measured(measure(20, 40)));
        let font = images.font_size();
        assert_eq!(
            (font.width, font.height),
            (start.width * 2, start.height * 2)
        );
        assert!(!images.draws_photos(), "the protocol stays");
        assert!(images.get(&preview(1), 4, 2, false).is_none(), "made again");
        go.send(()).unwrap();
        images.on_built(rx.recv().await.unwrap());
        assert!(images.get(&preview(7), 4, 2, false).is_none(), "dropped");
        assert!(!images.is_broken(&preview(7)));
        assert!(!images.measured(measure(20, 40)), "only once");
        images.build(key(7), || Ok(DynamicImage::new_rgba8(8, 8)));
        images.on_built(rx.recv().await.unwrap());
        assert!(
            images.get(&preview(7), 4, 2, false).is_some(),
            "the new size"
        );
    }

    #[test]
    fn a_photo_never_drawn_waits_while_a_popup_may_hide_it() {
        let tx = tokio::sync::mpsc::unbounded_channel().0;
        let mut images = Images::new(Picker::halfblocks(), tx);
        images.insert_ready(key(1), DynamicImage::new_rgba8(8, 8));
        // Kitty's terminal gets an image only with its first drawing: one
        // a popup covers would never come.
        assert!(images.get(&preview(1), 4, 2, true).is_none());
        assert!(images.sharp(&preview(1), 4, 2, true).is_none());
        assert!(images.get(&preview(1), 4, 2, false).is_some());
        // Once drawn, it stays under one, as before.
        assert!(images.get(&preview(1), 4, 2, true).is_some());
        assert!(images.sharp(&preview(1), 4, 2, true).is_some());
        // Its thumbnail drawn before stands in while it waits.
        let thumbnail = Key {
            thumbnail: true,
            ..key(2)
        };
        images.insert_ready(thumbnail, DynamicImage::new_rgba8(8, 8));
        assert!(images.get(&preview(2), 4, 2, false).is_some());
        images.insert_ready(key(2), DynamicImage::new_rgba8(8, 8));
        assert!(
            images.get(&preview(2), 4, 2, true).is_some(),
            "the thumbnail"
        );
        assert!(images.sharp(&preview(2), 4, 2, true).is_none());
    }

    #[test]
    fn asking_whether_a_photo_is_ready_doesnt_count_it_drawn() {
        let tx = tokio::sync::mpsc::unbounded_channel().0;
        let mut images = Images::new(Picker::halfblocks(), tx);
        images.insert_ready(key(1), DynamicImage::new_rgba8(8, 8));
        assert!(images.has_sharp(&preview(1), 4, 2, false));
        // The viewer asks before it knows it has room to draw it: under a
        // popup it must still wait for a drawing of its own.
        assert!(!images.has_sharp(&preview(1), 4, 2, true));
        assert!(images.sharp(&preview(1), 4, 2, true).is_none());
        assert!(images.sharp(&preview(1), 4, 2, false).is_some());
        assert!(images.has_sharp(&preview(1), 4, 2, true));
    }

    #[tokio::test]
    async fn only_a_few_images_decode_at_once() {
        let (tx, _rx) = tokio::sync::mpsc::unbounded_channel();
        let mut images = Images::new(Picker::halfblocks(), tx);
        let (go, wait) = std::sync::mpsc::channel::<()>();
        let wait = std::sync::Arc::new(std::sync::Mutex::new(wait));
        for id in 0..10 {
            let wait = wait.clone();
            images.build(key(id), move || {
                let _ = wait.lock().unwrap().recv();
                Ok(DynamicImage::new_rgba8(8, 8))
            });
        }
        assert_eq!(images.building.len(), MAX_BUILDING);
        drop(go);
    }

    #[tokio::test]
    async fn a_decode_that_never_finishes_gives_up_its_place() {
        let (tx, _rx) = tokio::sync::mpsc::unbounded_channel();
        let mut images = Images::new(Picker::halfblocks(), tx);
        for id in 0..MAX_BUILDING as i32 {
            let long_ago = std::time::Instant::now() - BUILD_TIMEOUT;
            images.building.insert(key(id), long_ago);
        }
        images.build(key(99), || Ok(DynamicImage::new_rgba8(8, 8)));
        assert!(images.building.contains_key(&key(99)), "its turn came");
        assert!(
            images.failed.contains(&key(0)),
            "the stuck one counts as failed"
        );
    }

    #[test]
    fn the_photo_cache_keeps_the_ones_drawn_lately() {
        let (tx, _rx) = tokio::sync::mpsc::unbounded_channel();
        let mut images = Images::new(Picker::halfblocks(), tx);
        images.insert_ready(key(0), DynamicImage::new_rgba8(8, 8));
        for id in 1..MAX_READY as i32 + 10 {
            images.frame += 1;
            assert!(
                images.get(&preview(0), 4, 2, false).is_some(),
                "still drawn"
            );
            images.insert_ready(key(id), DynamicImage::new_rgba8(8, 8));
        }
        assert_eq!(images.ready.len(), MAX_READY);
        assert!(images.get(&preview(0), 4, 2, false).is_some());
        assert!(
            images.get(&preview(1), 4, 2, false).is_none(),
            "the oldest went"
        );
    }

    #[test]
    fn large_photos_are_kept_within_a_budget_of_pixels() {
        let tx = tokio::sync::mpsc::unbounded_channel().0;
        let mut images = Images::new(Picker::halfblocks(), tx);
        let at = |file_id, cols| Key {
            file_id,
            cols,
            rows: 50,
            thumbnail: false,
            avatar: false,
        };
        // Two megapixels each, at halfblocks' 10 × 20 px cells: five fit.
        for file_id in 0..15 {
            images.frame += 1;
            images.insert_ready(at(file_id, 4), image::RgbImage::new(4, 4).into());
            images.insert_ready(at(file_id, 200), image::RgbImage::new(4, 4).into());
        }
        let large = images.ready.keys().filter(|k| k.cols == 200);
        assert_eq!(large.clone().count(), 5);
        assert!(large.clone().all(|k| k.file_id >= 10), "the oldest went");
        let small = images.ready.keys().filter(|k| k.cols == 4).count();
        assert_eq!(small, 15, "bubbles' photos aren't touched");
    }

    #[tokio::test]
    async fn what_is_in_use_is_never_pushed_out_and_what_is_ahead_waits_for_room() {
        let mut images = Images::new(
            Picker::halfblocks(),
            tokio::sync::mpsc::unbounded_channel().0,
        );
        let meta = Meta::detached(tokio::sync::mpsc::unbounded_channel().0);
        let at = |file_id| Key {
            file_id,
            cols: 200,
            rows: 50,
            thumbnail: false,
            avatar: false,
        };
        let photo = |file_id| Preview {
            width: 2000,
            height: 1000,
            ..preview(file_id)
        };
        let made = || image::RgbImage::new(4, 4).into();
        // Six on screen, two megapixels each at halfblocks' 10 × 20 px
        // cells: past the budget, and all kept, since each would be wanted
        // again at once and push out another.
        for id in 0..6 {
            images.want(&photo(id), 200, 50);
        }
        images.fetch(&meta);
        for id in 0..6 {
            images.insert_ready(at(id), made());
        }
        assert_eq!(images.ready.len(), 6);
        // No room: nothing is made ahead, though its file is there.
        images.on_downloaded(9, Some("/data/helper/files/9.jpg".into()));
        for id in 0..6 {
            images.want(&photo(id), 200, 50);
        }
        images.prepare(&photo(9), 200, 50);
        images.fetch(&meta);
        assert!(images.building.is_empty());
        // Once they're off screen, the oldest go for what comes next.
        images.want(&photo(6), 200, 50);
        images.fetch(&meta);
        images.insert_ready(at(6), made());
        assert_eq!(images.ready.len(), 5, "the budget's worth");
        assert!(images.ready.contains_key(&at(6)));
        // With room, the one ahead is made; done after it was left, it
        // isn't kept.
        images.want(&photo(6), 200, 50);
        images.prepare(&photo(9), 200, 50);
        images.fetch(&meta);
        assert!(images.building_ahead.contains(&at(9)));
        images.clear();
        let size = Size::new(200, 50);
        let image = SlicedProtocol::new_with_resize(&images.picker, made(), size, RESIZE);
        images.on_built(ImageEvent {
            key: at(9),
            generation: images.generation,
            result: Ok(image.unwrap()),
        });
        assert!(images.ready.is_empty());
    }

    #[tokio::test]
    async fn a_photo_about_to_be_sent_is_read_from_its_file_not_downloaded() {
        let dir = std::env::temp_dir().join(format!("tuimeta-local-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("cat.png");
        std::fs::write(&path, png(80, 60)).unwrap();
        let (tx, mut rx) = tokio::sync::mpsc::unbounded_channel();
        let mut images = Images::new(Picker::halfblocks(), tx);
        let photo = preview(-1);
        images.local_file(-1, path.to_str().unwrap());
        images.want(&photo, 4, 2);
        // A detached client makes no requests, so a download would never end.
        images.fetch(&Meta::detached(tokio::sync::mpsc::unbounded_channel().0));
        images.on_built(rx.recv().await.unwrap());
        assert!(images.get(&photo, 4, 2, false).is_some());
        std::fs::remove_dir_all(&dir).unwrap();
    }

    #[tokio::test]
    async fn a_photo_coming_up_downloads_behind_the_others_then_is_made_ready() {
        let mut images = Images::new(
            Picker::halfblocks(),
            tokio::sync::mpsc::unbounded_channel().0,
        );
        let meta = Meta::detached(tokio::sync::mpsc::unbounded_channel().0);
        let downloads = || -> Vec<String> {
            meta.sent()
                .into_iter()
                .filter(|(method, _)| method == "download")
                .map(|(_, p)| format!("{} {}", p["file_id"], p["priority"].as_str().unwrap()))
                .collect()
        };
        images.want(&preview(1), 4, 2);
        images.prefetch(&preview(1));
        images.prefetch(&preview(2));
        images.fetch(&meta);
        assert_eq!(downloads(), ["1 high", "2 low"]);
        images.prefetch(&preview(2));
        images.fetch(&meta);
        assert_eq!(downloads().len(), 2, "asked for once");
        images.want(&preview(2), 4, 2);
        images.fetch(&meta);
        assert_eq!(downloads()[2], "2 high", "on screen now: it goes first");
        // Downloaded, one only fetched isn't made into an image until it's
        // drawn; one coming next is, nearest first, leaving half the
        // decoders for what comes on screen.
        images.on_downloaded(7, Some("/data/helper/files/7.jpg".into()));
        images.prefetch(&preview(7));
        for id in 3..=5 {
            images.on_downloaded(id, Some(format!("/data/helper/files/{id}.jpg")));
            images.prepare(&preview(id), 4, 2);
        }
        images.fetch(&meta);
        assert!(!images.building.contains_key(&key(7)));
        assert_eq!(downloads().len(), 3);
        assert!(images.building_ahead.contains(&key(3)));
        assert!(images.building_ahead.contains(&key(4)));
        assert_eq!(images.building.len(), MAX_BUILDING / 2);
        images.on_downloaded(6, Some("/data/helper/files/6.jpg".into()));
        images.want(&preview(6), 4, 2);
        images.fetch(&meta);
        assert!(
            images.building.contains_key(&key(6)),
            "one on screen still starts"
        );
        assert!(!images.building_ahead.contains(&key(6)));
    }

    #[test]
    fn a_png_saved_with_a_jpeg_name_still_opens() {
        let dir = std::env::temp_dir().join(format!("tuimeta-misnamed-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("preview.jpg");
        std::fs::write(&path, png(80, 60)).unwrap();
        let image = open_image(path.to_str().unwrap()).unwrap();
        assert_eq!((image.width(), image.height()), (80, 60));
        std::fs::remove_dir_all(&dir).unwrap();
    }

    fn encoded(image: DynamicImage, format: image::ImageFormat) -> Vec<u8> {
        let mut data = std::io::Cursor::new(Vec::new());
        image.write_to(&mut data, format).unwrap();
        data.into_inner()
    }

    #[test]
    fn only_the_formats_pictures_come_in_are_decoded() {
        let exr = encoded(
            image::Rgba32FImage::new(64, 64).into(),
            image::ImageFormat::OpenExr,
        );
        let bmp = encoded(image::RgbImage::new(8, 8).into(), image::ImageFormat::Bmp);
        let tiff = encoded(image::RgbImage::new(8, 8).into(), image::ImageFormat::Tiff);
        for data in [&exr, &bmp, &tiff] {
            let error = decode_bytes(data).unwrap_err();
            assert!(
                matches!(error, image::ImageError::Unsupported(_)),
                "{error}"
            );
        }
        // Whatever the file is called: OpenEXR decodes on threads of its own.
        let dir = std::env::temp_dir().join(format!("tuimeta-formats-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("photo.jpg");
        std::fs::write(&path, &exr).unwrap();
        assert!(open_image(path.to_str().unwrap()).is_err());
        for format in [
            image::ImageFormat::Png,
            image::ImageFormat::Jpeg,
            image::ImageFormat::Gif,
            image::ImageFormat::WebP,
        ] {
            let data = encoded(image::RgbImage::new(8, 8).into(), format);
            assert!(decode_bytes(&data).is_ok(), "{format:?}");
            std::fs::write(&path, &data).unwrap();
            assert!(open_image(path.to_str().unwrap()).is_ok(), "{format:?}");
        }
        std::fs::remove_dir_all(&dir).unwrap();
    }

    #[tokio::test]
    async fn decoders_given_up_on_still_count_until_they_end() {
        let (tx, mut rx) = tokio::sync::mpsc::unbounded_channel();
        let mut images = Images::new(Picker::halfblocks(), tx);
        let (go, wait) = std::sync::mpsc::channel::<()>();
        let wait = std::sync::Arc::new(std::sync::Mutex::new(wait));
        // Decodes that never finish, each given up on in its time.
        for id in 0..MAX_RUNNING as i32 {
            let wait = wait.clone();
            images.build(key(id), move || {
                let _ = wait.lock().unwrap().recv();
                Ok(DynamicImage::new_rgba8(8, 8))
            });
            assert!(images.building.contains_key(&key(id)));
            for started in images.building.values_mut() {
                *started = std::time::Instant::now() - BUILD_TIMEOUT;
            }
        }
        images.build(key(99), || Ok(DynamicImage::new_rgba8(8, 8)));
        assert!(
            !images.building.contains_key(&key(99)),
            "no more threads while they run"
        );
        drop(go);
        for _ in 0..MAX_RUNNING {
            images.on_built(rx.recv().await.unwrap());
        }
        assert_eq!(images.running.load(Ordering::SeqCst), 0);
        images.build(key(99), || Ok(DynamicImage::new_rgba8(8, 8)));
        assert!(images.building.contains_key(&key(99)), "its turn came");
    }

    #[test]
    fn images_far_bigger_than_the_networks_send_are_refused() {
        assert!(decode_bytes(&png(512, 512)).is_ok());
        let error = decode_bytes(&png(5000, 1)).unwrap_err();
        assert!(matches!(error, image::ImageError::Limits(_)), "{error}");
    }
}
