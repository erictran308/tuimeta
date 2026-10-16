//! Pepper: a Ghostty build that shows video inside the terminal, straight
//! from the graphics chip (`../pepper`). There, a feed's video plays over
//! its post through Pepper's player, pepper-play (inside Pepper.app, its
//! path in `PEPPER_PLAY`: libmpv drawing into surfaces that Pepper shows
//! over a box of cells). Anywhere else, the feed shows a video's cover
//! picture, and Enter opens the video in the system's player.
//!
//! The two meet by a token, fresh for each playback: tuimeta gives it to
//! pepper-play over its control socket (never on a command line or in its
//! environment), and places the box with it by an escape sequence,
//! `ESC ] 777 ; pepper ; place ; TOKEN ; COL ; ROW ; COLS ; ROWS ESC \`,
//! which only a program writing to this tab can send. Pepper shows
//! pepper-play's frames only where that token was placed, and tells it the
//! box's size in pixels. Other terminals ignore the sequences.

use std::ffi::OsString;
use std::path::{Path, PathBuf};

use ratatui::layout::Rect;

/// The token's random bytes (32 hex digits).
#[cfg(unix)]
const TOKEN_BYTES: usize = 16;

/// What a terminal multiplexer running tuimeta sets: tmux, GNU screen,
/// zellij. Each would need every escape sequence wrapped to pass it on, and
/// screen may log them (`screen -L`).
const MULTIPLEXERS: [&str; 3] = ["TMUX", "STY", "ZELLIJ"];

/// What videos play with inside Pepper.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Pepper {
    /// Pepper's port, from `PEPPER_PORT`: pepper-play gets it in its
    /// environment.
    pub port: OsString,
    /// pepper-play, from `PEPPER_PLAY`.
    pub play: PathBuf,
}

/// Pepper and its player, when tuimeta runs in Pepper (not through a
/// multiplexer) and pepper-play is there to run.
pub fn find() -> Option<Pepper> {
    find_from(|name| std::env::var_os(name), usable)
}

fn find_from(
    var: impl Fn(&str) -> Option<OsString>,
    usable: impl Fn(&Path) -> bool,
) -> Option<Pepper> {
    if MULTIPLEXERS.iter().any(|name| var(name).is_some()) {
        return None;
    }
    let port = var("PEPPER_PORT").filter(|port| !port.is_empty())?;
    let play = var("PEPPER_PLAY")
        .map(PathBuf::from)
        .filter(|p| usable(p))?;
    Some(Pepper { port, play })
}

/// An absolute path to a file this user can run.
fn usable(path: &Path) -> bool {
    if !path.is_absolute() {
        return false;
    }
    let Ok(meta) = std::fs::metadata(path) else {
        return false;
    };
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        meta.is_file() && meta.permissions().mode() & 0o111 != 0
    }
    #[cfg(not(unix))]
    meta.is_file()
}

/// A token for one playback: random hex digits, as Pepper takes them
/// (1–64 of `[A-Za-z0-9_-]`). `None` if the system's randomness can't be
/// read: then the video opens in the system's player instead.
pub fn new_token() -> Option<String> {
    #[cfg(unix)]
    {
        use std::io::Read;
        let mut bytes = [0u8; TOKEN_BYTES];
        std::fs::File::open("/dev/urandom")
            .and_then(|mut f| f.read_exact(&mut bytes))
            .ok()?;
        Some(bytes.iter().map(|b| format!("{b:02x}")).collect())
    }
    #[cfg(not(unix))]
    None
}

/// Puts the picture of `token`'s playback over `area`.
pub fn place(token: &str, area: Rect) -> String {
    format!(
        "\x1b]777;pepper;place;{token};{};{};{};{}\x1b\\",
        area.x, area.y, area.width, area.height
    )
}

/// Takes the picture of `token`'s playback away.
pub fn remove(token: &str) -> String {
    format!("\x1b]777;pepper;remove;{token}\x1b\\")
}

/// What Pepper was last told.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Placed {
    pub token: String,
    pub area: Rect,
}

/// What to tell Pepper so that `wanted` shows, when `placed` is what it
/// was told last: nothing if they're the same, else the old box taken away
/// (unless the same playback only moves) and the new one placed.
pub fn tell(placed: Option<&Placed>, wanted: Option<&Placed>) -> String {
    if placed == wanted {
        return String::new();
    }
    let mut out = String::new();
    if let Some(old) = placed
        && wanted.is_none_or(|new| new.token != old.token)
    {
        out.push_str(&remove(&old.token));
    }
    if let Some(new) = wanted {
        out.push_str(&place(&new.token, new.area));
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    fn env(vars: Vec<(&'static str, &'static str)>) -> impl Fn(&str) -> Option<OsString> {
        move |name: &str| {
            vars.iter()
                .find(|(k, _)| *k == name)
                .map(|(_, v)| OsString::from(v))
        }
    }

    #[test]
    fn videos_play_inside_only_in_pepper_with_its_player_and_outside_multiplexers() {
        let play = "/Applications/Pepper.app/Contents/Helpers/pepper-play";
        let yes = |_: &Path| true;
        assert_eq!(
            find_from(
                env(vec![("PEPPER_PORT", "x.1"), ("PEPPER_PLAY", play)]),
                yes
            ),
            Some(Pepper {
                port: "x.1".into(),
                play: play.into()
            })
        );
        assert_eq!(find_from(env(vec![("PEPPER_PLAY", play)]), yes), None);
        assert_eq!(find_from(env(vec![("PEPPER_PORT", "x.1")]), yes), None);
        assert_eq!(
            find_from(env(vec![("PEPPER_PORT", ""), ("PEPPER_PLAY", play)]), yes),
            None
        );
        for multiplexer in MULTIPLEXERS {
            let vars = |name: &str| match name {
                "PEPPER_PORT" => Some("x.1".into()),
                "PEPPER_PLAY" => Some(play.into()),
                _ if name == multiplexer => Some("1".into()),
                _ => None,
            };
            assert_eq!(find_from(vars, yes), None, "{multiplexer}");
        }
        // A player that isn't there, or a relative path, isn't run.
        assert_eq!(
            find_from(
                env(vec![("PEPPER_PORT", "x.1"), ("PEPPER_PLAY", play)]),
                |_| false
            ),
            None
        );
        assert!(!usable(Path::new("pepper-play")));
    }

    #[cfg(unix)]
    #[test]
    fn tokens_are_fresh_hex() {
        let (a, b) = (new_token().unwrap(), new_token().unwrap());
        assert_eq!(a.len(), 2 * TOKEN_BYTES);
        assert!(a.bytes().all(|c| c.is_ascii_hexdigit()));
        assert_ne!(a, b);
    }

    #[test]
    fn pepper_is_told_only_what_changed() {
        let area = Rect::new(2, 3, 40, 11);
        assert_eq!(
            place("ab", area),
            "\x1b]777;pepper;place;ab;2;3;40;11\x1b\\"
        );
        assert_eq!(remove("ab"), "\x1b]777;pepper;remove;ab\x1b\\");
        let at = |token: &str, area| Placed {
            token: token.into(),
            area,
        };
        let here = at("ab", area);
        assert_eq!(tell(None, None), "");
        assert_eq!(tell(None, Some(&here)), place("ab", area));
        assert_eq!(tell(Some(&here), Some(&here)), "");
        // The same playback moves without being taken away first.
        let moved = at("ab", Rect::new(2, 9, 40, 11));
        assert_eq!(tell(Some(&here), Some(&moved)), place("ab", moved.area));
        assert_eq!(tell(Some(&here), None), remove("ab"));
        let next = at("cd", area);
        assert_eq!(
            tell(Some(&here), Some(&next)),
            format!("{}{}", remove("ab"), place("cd", area))
        );
    }
}
