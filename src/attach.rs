//! Files waiting in the composer to be sent, and the ways they get there:
//! a typed path (`a`), the clipboard (`p`), or a paste of file paths, which
//! is what dropping files on a terminal window types.

use std::ops::Not;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicI32, Ordering};

use crate::messages::Preview;
use crate::meta::FileToSend;
use crate::text;

/// Telegram's limits for photos. Bigger ones go as files, uncompressed.
const PHOTO_MAX_BYTES: u64 = 10 * 1024 * 1024;
/// Width plus height.
const PHOTO_MAX_SIDES: u32 = 10_000;
/// Long side over short side.
const PHOTO_MAX_RATIO: u32 = 20;
/// Image types that go as photos; anything else (GIFs, HEIC, RAW…) as a file.
const PHOTO_TYPES: &[&str] = &["jpg", "jpeg", "png", "webp"];

/// How a file is sent.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Kind {
    /// Compressed and shown in the chat, like Telegram's "Send as photo".
    Photo { width: u32, height: u32 },
    /// As it is, with its name.
    File,
}

/// A file to go with the next message sent.
#[derive(Clone, Debug)]
pub struct Attachment {
    /// Absolute, as TDLib wants it.
    pub path: PathBuf,
    /// What the composer calls it: the file name, without control
    /// characters, which any file name can hold.
    pub name: String,
    pub size: u64,
    pub kind: Kind,
    /// Which file it was when listed, to tell if it was swapped since.
    pub identity: Identity,
    /// Numbers its picture in the composer the way TDLib numbers files:
    /// negative, so never one of TDLib's, and new for each attachment, so a
    /// file changed and attached again doesn't show as it was.
    pub image_id: i32,
}

/// The next [`Attachment::image_id`].
static NEXT_IMAGE_ID: AtomicI32 = AtomicI32::new(-1);

/// What tells one file from another that took its place: on Unix its device,
/// inode and when its inode last changed, everywhere its size and when its
/// contents last changed. Times are nanoseconds since 1970, as the helper
/// reads them (PROTOCOL.md, `send_files`).
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct Identity {
    device: u64,
    inode: u64,
    size: u64,
    modified_ns: i64,
    /// Any write, and putting the modification time back after one, moves
    /// it on, so a file rewritten in place to look untouched is noticed.
    changed_ns: i64,
}

impl Identity {
    fn of(meta: &std::fs::Metadata) -> Self {
        #[cfg(unix)]
        let (device, inode, modified_ns, changed_ns) = {
            use std::os::unix::fs::MetadataExt;
            (
                meta.dev(),
                meta.ino(),
                nanos(meta.mtime(), meta.mtime_nsec()),
                nanos(meta.ctime(), meta.ctime_nsec()),
            )
        };
        #[cfg(not(unix))]
        let (device, inode, modified_ns, changed_ns) = {
            let modified_ns =
                meta.modified()
                    .map_or(0, |at| match at.duration_since(std::time::UNIX_EPOCH) {
                        Ok(after) => after.as_nanos() as i64,
                        Err(before) => -(before.duration().as_nanos() as i64),
                    });
            (0, 0, modified_ns, 0)
        };
        Self {
            device,
            inode,
            size: meta.len(),
            modified_ns,
            changed_ns,
        }
    }
}

/// Seconds and nanoseconds as nanoseconds, wrapping past the year 2262 as
/// Go's `UnixNano` does, so the helper's number for a file is this one.
#[cfg(unix)]
fn nanos(seconds: i64, nanoseconds: i64) -> i64 {
    seconds
        .wrapping_mul(1_000_000_000)
        .wrapping_add(nanoseconds)
}

impl Attachment {
    /// Checks that `path` is a file Telegram can take, and works out whether
    /// it goes as a photo.
    pub fn new(path: &Path) -> Result<Self, String> {
        let name = path
            .file_name()
            .map(|n| text::clean(&n.to_string_lossy()))
            .unwrap_or_else(|| text::clean(&path.display().to_string()));
        let meta = std::fs::metadata(path).map_err(|e| match e.kind() {
            std::io::ErrorKind::NotFound => format!("No such file: {name}"),
            _ => format!("Can't read {name}: {e}"),
        })?;
        if meta.is_dir() {
            return Err(format!("{name} is a folder"));
        }
        if meta.len() == 0 {
            return Err(format!("{name} is empty"));
        }
        let given = path;
        let path = std::fs::canonicalize(path).map_err(|e| format!("Can't read {name}: {e}"))?;
        // TDLib takes paths as JSON text.
        if path.to_str().is_none() {
            return Err(format!("Can't send {name}: its path isn't valid UTF-8"));
        }
        // The name of what goes out: for a link, that's what it points to,
        // which a link's own name could pass off as something else. A link
        // to a file in another folder shows where that is: `report.pdf` in
        // a shared folder can lead to your own one.
        let name = match path.file_name() {
            Some(_) if leaves_its_folder(given, &path) => text::clean(&from_home(&path)),
            Some(file_name) => text::clean(&file_name.to_string_lossy()),
            None => name,
        };
        let kind = match photo_size(&path, meta.len()) {
            Some((width, height)) => Kind::Photo { width, height },
            None => Kind::File,
        };
        let identity = std::fs::symlink_metadata(&path)
            .map(|meta| Identity::of(&meta))
            .map_err(|e| format!("Can't read {name}: {e}"))?;
        Ok(Self {
            path,
            name,
            // The size of the file whose identity goes to the helper.
            size: identity.size,
            kind,
            identity,
            image_id: NEXT_IMAGE_ID.fetch_sub(1, Ordering::Relaxed),
        })
    }

    /// The picture the composer shows of a photo, read from its file.
    pub fn preview(&self) -> Option<Preview> {
        let Kind::Photo { width, height } = self.kind else {
            return None;
        };
        Some(Preview {
            file_id: self.image_id,
            width,
            height,
            thumbnail: None,
            sticker: false,
        })
    }

    /// The file listed is no longer the one there: it was changed, or
    /// something else took its place, such as a link to another file. In a
    /// folder others can write to, that could swap in a file of yours you
    /// never chose to send.
    pub fn swapped(&self) -> bool {
        std::fs::symlink_metadata(&self.path)
            .is_ok_and(|meta| meta.is_file() && Identity::of(&meta) == self.identity)
            .not()
    }

    /// The file as the helper is to send it: where it is, and which file
    /// it was when listed, so the helper sends nothing if what it opens is
    /// another (one swapped in after [`swapped`](Self::swapped) looked).
    pub fn to_send(&self) -> FileToSend {
        let id = &self.identity;
        FileToSend {
            path: self.path.to_string_lossy().into_owned(),
            dev: id.device,
            ino: id.inode,
            size: id.size,
            mtime_ns: id.modified_ns,
            ctime_ns: id.changed_ns,
        }
    }
}

/// Whether `canonical`, where `given` really is, is in another folder than
/// `given` seems to be, as a link to a file elsewhere is.
fn leaves_its_folder(given: &Path, canonical: &Path) -> bool {
    let folder = match given.parent() {
        Some(folder) if folder.as_os_str().is_empty() => Path::new("."),
        Some(folder) => folder,
        None => return false,
    };
    std::fs::canonicalize(folder).ok().as_deref() != canonical.parent()
}

/// `path`, from `~` when it's in the home folder.
fn from_home(path: &Path) -> String {
    match dirs::home_dir().and_then(|home| path.strip_prefix(home).ok().map(Path::to_owned)) {
        Some(rest) => format!("~{}{}", std::path::MAIN_SEPARATOR, rest.display()),
        None => path.display().to_string(),
    }
}

/// The size of an image that Telegram takes as a photo, read from its header.
fn photo_size(path: &Path, bytes: u64) -> Option<(u32, u32)> {
    let extension = path.extension()?.to_str()?.to_ascii_lowercase();
    if !PHOTO_TYPES.contains(&extension.as_str()) || bytes > PHOTO_MAX_BYTES {
        return None;
    }
    // Opened without waiting, and read only if it's a plain file: one
    // swapped for a pipe since it was looked at would otherwise hold the
    // whole app until something wrote to it.
    let file = open_without_waiting(path).ok()?;
    if !file.metadata().ok()?.is_file() {
        return None;
    }
    let format = image::ImageFormat::from_extension(&extension)?;
    let (width, height) = image::ImageReader::with_format(std::io::BufReader::new(file), format)
        .into_dimensions()
        .ok()?;
    // In u64: a crafted header's sides can add up past u32.
    let (long, short) = (u64::from(width.max(height)), u64::from(width.min(height)));
    let fits = short > 0
        && long + short <= u64::from(PHOTO_MAX_SIDES)
        && long <= short * u64::from(PHOTO_MAX_RATIO);
    fits.then_some((width, height))
}

/// Opens a file to read without waiting for a writer, which opening a
/// pipe would.
fn open_without_waiting(path: &Path) -> std::io::Result<std::fs::File> {
    let mut options = std::fs::OpenOptions::new();
    options.read(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.custom_flags(libc::O_NONBLOCK);
    }
    options.open(path)
}

/// A paste that became attachments, which Ctrl-z turns back into the text.
pub struct Dropped {
    pub text: String,
    /// How many attachments it added, at the end of the list.
    pub count: usize,
}

impl Kind {
    pub fn is_photo(self) -> bool {
        matches!(self, Kind::Photo { .. })
    }
}

/// The files a paste names, if it names nothing else. Dropping files on a
/// terminal types their paths, quoted or with `\` before spaces, or as
/// `file://` URLs, depending on the terminal. Only absolute paths count, as
/// written (no `~`), so pasting a word that happens to be a file's name, or
/// a path quoted in a message, stays text.
pub fn pasted_paths(text: &str) -> Option<Vec<PathBuf>> {
    let text = text.trim();
    if text.is_empty() {
        return None;
    }
    // One path with spaces in it, pasted as it is.
    if let Some(path) = existing_file(text) {
        return Some(vec![path]);
    }
    words(text)?.iter().map(|w| existing_file(w)).collect()
}

fn existing_file(word: &str) -> Option<PathBuf> {
    let path = match word.strip_prefix("file://") {
        Some(url) => from_file_url(url)?,
        None => PathBuf::from(word),
    };
    (path.is_absolute() && !on_another_machine(&path) && path.is_file()).then_some(path)
}

/// Whether `path` names a file on another machine: `\\server\share\…` (or
/// `//server/share/…`) on Windows. Even looking at whether such a file
/// exists connects to that server and hands it the user's Windows login
/// hash, so a paste never does. Elsewhere, no paste starts with two slashes.
pub fn on_another_machine(path: &Path) -> bool {
    matches!(
        path.as_os_str().as_encoded_bytes(),
        [b'/' | b'\\', b'/' | b'\\', ..]
    )
}

/// Splits `text` into words the way a shell does, which is how terminals
/// quote dropped paths. `None` if a quote is left open.
fn words(text: &str) -> Option<Vec<String>> {
    // Backslashes separate folders on Windows, where paths with spaces are
    // put in double quotes instead.
    let escapes = !cfg!(windows);
    let mut words = Vec::new();
    let mut word = String::new();
    let mut in_word = false;
    let mut chars = text.chars();
    while let Some(c) = chars.next() {
        match c {
            '\'' => {
                in_word = true;
                loop {
                    match chars.next()? {
                        '\'' => break,
                        c => word.push(c),
                    }
                }
            }
            '"' => {
                in_word = true;
                loop {
                    match chars.next()? {
                        '"' => break,
                        '\\' if escapes => word.push(chars.next()?),
                        c => word.push(c),
                    }
                }
            }
            '\\' if escapes => {
                in_word = true;
                word.push(chars.next()?);
            }
            c if c.is_whitespace() => {
                if in_word {
                    words.push(std::mem::take(&mut word));
                    in_word = false;
                }
            }
            c => {
                in_word = true;
                word.push(c);
            }
        }
    }
    if in_word {
        words.push(word);
    }
    Some(words)
}

/// The path in a `file://` URL (after the `file://`), percent-decoded.
fn from_file_url(url: &str) -> Option<PathBuf> {
    let path = url.strip_prefix("localhost").unwrap_or(url);
    let bytes = path.as_bytes();
    let mut decoded = Vec::with_capacity(bytes.len());
    let mut i = 0;
    while i < bytes.len() {
        if bytes[i] == b'%' {
            let hex = std::str::from_utf8(bytes.get(i + 1..i + 3)?).ok()?;
            decoded.push(u8::from_str_radix(hex, 16).ok()?);
            i += 3;
        } else {
            decoded.push(bytes[i]);
            i += 1;
        }
    }
    let path = String::from_utf8(decoded).ok()?;
    if cfg!(windows)
        && let Some(drive) = on_a_drive(&path)
    {
        return Some(PathBuf::from(drive));
    }
    Some(PathBuf::from(path))
}

/// `C:/Users/…` from a URL's `/C:/Users/…`, as Windows writes it.
fn on_a_drive(path: &str) -> Option<&str> {
    let rest = path.strip_prefix('/')?;
    match rest.as_bytes() {
        [letter, b':', ..] if letter.is_ascii_alphabetic() => Some(rest),
        _ => None,
    }
}

/// `~` and `~/…` mean the home folder, as in a shell.
pub fn expand_home(path: &str) -> PathBuf {
    let rest = match path.strip_prefix('~') {
        Some("") => "",
        Some(rest) if rest.starts_with(['/', std::path::MAIN_SEPARATOR]) => &rest[1..],
        _ => return PathBuf::from(path),
    };
    match dirs::home_dir() {
        Some(home) => home.join(rest),
        None => PathBuf::from(path),
    }
}

/// What Tab does to a path typed in the attach prompt.
#[derive(Debug, PartialEq, Eq)]
pub struct Completion {
    /// The path, with its last part completed as far as the matching names
    /// agree, and a `/` after a lone folder.
    pub text: String,
    /// Every name that matches, folders with a `/`, when there's more than one.
    pub matches: Vec<String>,
}

/// Completes the last part of `typed` from the names in its folder, like a
/// shell's Tab. Hidden files only match once a `.` is typed.
pub fn complete(typed: &str) -> Completion {
    let split = typed
        .rfind(['/', std::path::MAIN_SEPARATOR])
        .map_or(0, |i| i + 1);
    let (folder, start) = typed.split_at(split);
    let dir = if folder.is_empty() {
        PathBuf::from(".")
    } else {
        expand_home(folder)
    };
    let unchanged = || Completion {
        text: typed.to_string(),
        matches: Vec::new(),
    };
    // Listing a folder on another machine hands it the Windows login hash.
    if on_another_machine(&dir) {
        return unchanged();
    }
    let mut names: Vec<(String, bool)> = std::fs::read_dir(&dir)
        .into_iter()
        .flatten()
        .flatten()
        .filter_map(|entry| {
            let name = entry.file_name().into_string().ok()?;
            let wanted =
                name.starts_with(start) && (start.starts_with('.') || !name.starts_with('.'));
            // Follows links, so a link to a folder completes like one.
            wanted.then(|| (name, entry.path().is_dir()))
        })
        .collect();
    names.sort();
    match names.as_slice() {
        [] => unchanged(),
        [(name, is_dir)] => Completion {
            text: format!("{folder}{name}{}", if *is_dir { "/" } else { "" }),
            matches: Vec::new(),
        },
        [(first, _), rest @ ..] => {
            let common = rest.iter().fold(first.as_str(), |common, (name, _)| {
                let same = common
                    .char_indices()
                    .zip(name.chars())
                    .find(|((_, a), b)| a != b)
                    .map_or(common.len().min(name.len()), |((i, _), _)| i);
                &common[..same]
            });
            Completion {
                text: format!("{folder}{common}"),
                matches: names
                    .iter()
                    .map(|(name, is_dir)| format!("{name}{}", if *is_dir { "/" } else { "" }))
                    .collect(),
            }
        }
    }
}

/// A file size the way Telegram shows one: "340 KB", "2.1 MB".
pub fn size_label(bytes: u64) -> String {
    const UNITS: [&str; 4] = ["KB", "MB", "GB", "TB"];
    if bytes < 1024 {
        return format!("{bytes} B");
    }
    let mut size = bytes as f64 / 1024.0;
    let mut unit = 0;
    while size >= 1024.0 && unit < UNITS.len() - 1 {
        size /= 1024.0;
        unit += 1;
    }
    if size < 10.0 {
        format!("{size:.1} {}", UNITS[unit])
    } else {
        format!("{size:.0} {}", UNITS[unit])
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A folder of its own under the system temp folder, removed when dropped.
    struct TempDir(PathBuf);

    impl TempDir {
        fn new(name: &str) -> Self {
            let dir =
                std::env::temp_dir().join(format!("tuigram-test-{name}-{}", std::process::id()));
            let _ = std::fs::remove_dir_all(&dir);
            std::fs::create_dir_all(&dir).unwrap();
            // Canonical, so it compares equal to what `Attachment::new` gives
            // (on macOS the temp folder is behind a link).
            Self(std::fs::canonicalize(&dir).unwrap())
        }

        fn file(&self, name: &str, contents: &[u8]) -> PathBuf {
            let path = self.0.join(name);
            std::fs::write(&path, contents).unwrap();
            path
        }
    }

    impl Drop for TempDir {
        fn drop(&mut self) {
            let _ = std::fs::remove_dir_all(&self.0);
        }
    }

    fn png(width: u32, height: u32) -> Vec<u8> {
        let mut bytes = std::io::Cursor::new(Vec::new());
        image::RgbImage::new(width, height)
            .write_to(&mut bytes, image::ImageFormat::Png)
            .unwrap();
        bytes.into_inner()
    }

    #[test]
    fn images_go_as_photos_unless_telegram_would_refuse_them() {
        let dir = TempDir::new("photos");
        let kind =
            |name: &str, contents: &[u8]| Attachment::new(&dir.file(name, contents)).unwrap().kind;
        assert_eq!(
            kind("cat.png", &png(40, 30)),
            Kind::Photo {
                width: 40,
                height: 30
            }
        );
        assert_eq!(
            kind("strip.png", &png(420, 20)),
            Kind::File,
            "too long and thin"
        );
        assert_eq!(kind("cat.gif", &png(40, 30)), Kind::File, "GIFs are files");
        assert_eq!(kind("notes.txt", b"hello"), Kind::File);
        assert_eq!(
            kind("fake.jpg", b"not a jpeg"),
            Kind::File,
            "unreadable images too"
        );
    }

    #[test]
    fn folders_empty_files_and_missing_ones_cant_be_attached() {
        let dir = TempDir::new("refused");
        assert_eq!(
            Attachment::new(&dir.0).unwrap_err(),
            format!(
                "{} is a folder",
                dir.0.file_name().unwrap().to_string_lossy()
            )
        );
        assert_eq!(
            Attachment::new(&dir.file("empty.txt", b"")).unwrap_err(),
            "empty.txt is empty"
        );
        assert_eq!(
            Attachment::new(&dir.0.join("gone.txt")).unwrap_err(),
            "No such file: gone.txt"
        );
    }

    #[cfg(unix)]
    #[test]
    fn a_link_is_shown_by_the_name_of_what_it_sends() {
        let dir = TempDir::new("link");
        let secret = dir.file("id_rsa", b"key");
        let link = dir.0.join("beach.jpg");
        std::os::unix::fs::symlink(&secret, &link).unwrap();
        let attachment = Attachment::new(&link).unwrap();
        assert_eq!(attachment.name, "id_rsa");
        assert_eq!(attachment.path, std::fs::canonicalize(&secret).unwrap());
    }

    #[cfg(unix)]
    #[test]
    fn a_link_to_a_file_in_another_folder_shows_that_folder() {
        let shared = TempDir::new("link-shared");
        let mine = TempDir::new("link-mine");
        let secret = mine.file("report.pdf", b"private");
        let link = shared.0.join("report.pdf");
        std::os::unix::fs::symlink(&secret, &link).unwrap();
        let attachment = Attachment::new(&link).unwrap();
        assert_eq!(attachment.path, secret);
        assert!(
            attachment.name.contains("link-mine") && attachment.name.ends_with("report.pdf"),
            "{}",
            attachment.name
        );
        // A file reached through a linked folder is where it seems.
        let folder = shared.0.join("mine");
        std::os::unix::fs::symlink(&mine.0, &folder).unwrap();
        assert_eq!(
            Attachment::new(&folder.join("report.pdf")).unwrap().name,
            "report.pdf"
        );
    }

    #[cfg(unix)]
    #[test]
    fn a_pipe_named_like_a_photo_is_not_waited_on() {
        let dir = TempDir::new("pipe");
        let pipe = dir.0.join("cat.png");
        let c_path = std::ffi::CString::new(pipe.to_str().unwrap()).unwrap();
        // SAFETY: a valid C string; mkfifo touches nothing else.
        assert_eq!(unsafe { libc::mkfifo(c_path.as_ptr(), 0o600) }, 0);
        let (tx, rx) = std::sync::mpsc::channel();
        std::thread::spawn(move || tx.send(photo_size(&pipe, 100)));
        let size = rx
            .recv_timeout(std::time::Duration::from_secs(5))
            .expect("waited for a writer");
        assert_eq!(size, None);
    }

    #[test]
    fn file_names_are_shown_without_control_characters() {
        let dir = TempDir::new("names");
        let attachment = Attachment::new(&dir.file("a\u{1b}[2Jb.txt", b"x")).unwrap();
        assert_eq!(attachment.name, "a[2Jb.txt");
    }

    #[test]
    fn dropped_paths_are_read_however_the_terminal_quotes_them() {
        let dir = TempDir::new("dropped");
        let plain = dir.file("plain.txt", b"x");
        let spaced = dir.file("my photo.png", b"x");
        let (p, s) = (plain.display(), spaced.display());
        let escaped = s.to_string().replace(' ', "\\ ");
        let both = vec![plain.clone(), spaced.clone()];
        // macOS Terminal, iTerm2 and Ghostty escape spaces and add a space.
        assert_eq!(
            pasted_paths(&format!("{escaped} ")),
            Some(vec![spaced.clone()])
        );
        // GNOME Terminal quotes.
        assert_eq!(pasted_paths(&format!("'{p}' '{s}' ")), Some(both.clone()));
        assert_eq!(
            pasted_paths(&format!("\"{s}\"\n{p}")),
            Some(vec![spaced.clone(), plain.clone()])
        );
        // A path pasted as it is, spaces and all.
        assert_eq!(pasted_paths(&s.to_string()), Some(vec![spaced.clone()]));
        let url = format!("file://{}", s.to_string().replace(' ', "%20"));
        assert_eq!(pasted_paths(&url), Some(vec![spaced]));
    }

    #[test]
    fn a_paste_with_anything_but_existing_files_stays_text() {
        let dir = TempDir::new("text");
        let file = dir.file("notes.txt", b"x");
        let f = file.display();
        assert_eq!(pasted_paths(&format!("see {f}")), None);
        assert_eq!(pasted_paths(&format!("{f} {f}.missing")), None);
        assert_eq!(pasted_paths(&dir.0.display().to_string()), None, "a folder");
        assert_eq!(pasted_paths("notes.txt"), None, "relative paths are words");
        assert_eq!(pasted_paths("~/.ssh/id_rsa"), None, "so is ~");
        assert_eq!(pasted_paths(&format!("'{f}")), None, "an open quote");
        assert_eq!(pasted_paths("  \n"), None);
    }

    #[test]
    fn a_paste_never_looks_at_another_machines_files() {
        for path in [
            r"\\evil.example\s\a.png",
            "//evil.example/s/a.png",
            r"\\?\UNC\evil.example\s\a.png",
            r"\\.\pipe\a",
        ] {
            assert!(on_another_machine(Path::new(path)), "{path}");
            assert_eq!(pasted_paths(path), None, "{path}");
        }
        assert_eq!(pasted_paths("file:////evil.example/s/a.png"), None);
        assert!(!on_another_machine(Path::new("/Users/sam/a.png")));
        assert!(!on_another_machine(Path::new(r"C:\Users\sam\a.png")));
    }

    #[test]
    fn drive_letters_in_file_urls_are_read_without_cutting_a_character() {
        assert_eq!(on_a_drive("/C:/Users/a.png"), Some("C:/Users/a.png"));
        assert_eq!(on_a_drive("/é:"), None);
        assert_eq!(on_a_drive("é:"), None);
        assert_eq!(on_a_drive("/home/a.png"), None);
    }

    #[test]
    fn tab_completes_as_far_as_the_names_agree() {
        let dir = TempDir::new("complete");
        dir.file("report-2025.pdf", b"x");
        dir.file("report-2026.pdf", b"x");
        dir.file(".hidden", b"x");
        std::fs::create_dir(dir.0.join("photos")).unwrap();
        let d = format!("{}/", dir.0.display());
        assert_eq!(
            complete(&format!("{d}rep")),
            Completion {
                text: format!("{d}report-202"),
                matches: vec!["report-2025.pdf".into(), "report-2026.pdf".into()],
            }
        );
        assert_eq!(
            complete(&format!("{d}report-2026")).text,
            format!("{d}report-2026.pdf")
        );
        assert_eq!(
            complete(&format!("{d}ph")).text,
            format!("{d}photos/"),
            "a folder gets a /"
        );
        assert_eq!(complete(&format!("{d}.h")).text, format!("{d}.hidden"));
        let everything = complete(&d);
        assert!(
            !everything.matches.contains(&".hidden".to_string()),
            "until a . is typed"
        );
        assert_eq!(complete(&format!("{d}nothing")).text, format!("{d}nothing"));
    }

    #[test]
    fn tab_never_lists_a_folder_on_another_machine() {
        for typed in [r"\\evil.example\s\", "//", "//evil.example/s/"] {
            assert_eq!(
                complete(typed),
                Completion {
                    text: typed.to_string(),
                    matches: Vec::new(),
                },
                "{typed}"
            );
        }
    }

    #[test]
    fn a_file_swapped_after_it_was_listed_is_noticed() {
        let dir = std::env::temp_dir().join(format!("tuigram-swap-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("notes.txt");
        std::fs::write(&path, "mine to send").unwrap();
        let listed = Attachment::new(&path).unwrap();
        assert!(!listed.swapped());

        // Moved away and replaced by another file under the same name.
        std::fs::rename(&path, dir.join("moved.txt")).unwrap();
        std::fs::write(&path, "something else").unwrap();
        assert!(listed.swapped());

        #[cfg(unix)]
        {
            std::fs::remove_file(&path).unwrap();
            std::os::unix::fs::symlink(dir.join("moved.txt"), &path).unwrap();
            assert!(listed.swapped(), "a link isn't the file listed");
        }
        std::fs::remove_dir_all(&dir).unwrap();
    }

    #[cfg(unix)]
    #[test]
    fn a_file_rewritten_in_place_with_its_old_time_put_back_is_noticed() {
        use std::os::unix::fs::MetadataExt;
        let dir = TempDir::new("rewritten");
        let path = dir.file("notes.txt", b"mine to send");
        let listed = Attachment::new(&path).unwrap();
        let modified = std::fs::metadata(&path).unwrap().modified().unwrap();
        // Same inode, same size, the same modification time: only the
        // inode's change time tells. Its clock can be coarse, so the write
        // is repeated until it has moved on.
        for _ in 0..200 {
            std::thread::sleep(std::time::Duration::from_millis(10));
            let file = std::fs::OpenOptions::new().write(true).open(&path).unwrap();
            std::io::Write::write_all(&mut &file, b"not yours!!!").unwrap();
            file.set_modified(modified).unwrap();
            let meta = std::fs::metadata(&path).unwrap();
            if nanos(meta.ctime(), meta.ctime_nsec()) != listed.identity.changed_ns {
                break;
            }
        }
        let now = Identity::of(&std::fs::symlink_metadata(&path).unwrap());
        assert_eq!(
            (now.device, now.inode, now.size, now.modified_ns),
            (
                listed.identity.device,
                listed.identity.inode,
                listed.identity.size,
                listed.identity.modified_ns
            ),
            "looks untouched"
        );
        assert!(listed.swapped());
    }

    #[test]
    fn what_goes_to_the_helper_names_the_file_listed() {
        let dir = TempDir::new("to-send");
        let path = dir.file("notes.txt", b"mine to send");
        let listed = Attachment::new(&path).unwrap();
        let sent = serde_json::to_value(listed.to_send()).unwrap();
        let mut keys: Vec<&str> = sent
            .as_object()
            .unwrap()
            .keys()
            .map(String::as_str)
            .collect();
        keys.sort_unstable();
        assert_eq!(
            keys,
            ["ctime_ns", "dev", "ino", "mtime_ns", "path", "size"],
            "as PROTOCOL.md has them"
        );
        assert_eq!(sent["path"], path.to_string_lossy().as_ref());
        assert_eq!(sent["size"], 12);
        let meta = std::fs::symlink_metadata(&path).unwrap();
        let modified = meta
            .modified()
            .unwrap()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_nanos() as i64;
        assert_eq!(sent["mtime_ns"], modified);
        #[cfg(unix)]
        {
            use std::os::unix::fs::MetadataExt;
            assert_eq!(sent["dev"], meta.dev());
            assert_eq!(sent["ino"], meta.ino());
            assert_eq!(sent["ctime_ns"], nanos(meta.ctime(), meta.ctime_nsec()));
            assert_ne!(sent["ctime_ns"], 0);
        }
    }

    #[test]
    fn home_is_expanded_only_at_the_start() {
        let home = dirs::home_dir().unwrap();
        assert_eq!(expand_home("~/a.txt"), home.join("a.txt"));
        assert_eq!(expand_home("~"), home);
        assert_eq!(expand_home("~bob/a.txt"), PathBuf::from("~bob/a.txt"));
        assert_eq!(expand_home("/tmp/~"), PathBuf::from("/tmp/~"));
    }

    #[test]
    fn sizes_read_like_telegram_shows_them() {
        assert_eq!(size_label(512), "512 B");
        assert_eq!(size_label(340 * 1024), "340 KB");
        assert_eq!(size_label(2_200_000), "2.1 MB");
        assert_eq!(size_label(3 * 1024 * 1024 * 1024), "3.0 GB");
    }
}
