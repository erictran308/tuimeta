use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::OnceLock;

use anyhow::{Context, Result, bail};

use crate::{attach, text};

/// Settings read from the environment, or in development builds from the
/// repository's `.env` ([`load_dotenv`]).
pub struct Config {
    /// Where the helper keeps your login sessions, downloaded files and its
    /// log (in `helper/`), next to the app's own `settings.toml`.
    pub data_dir: PathBuf,
}

impl Config {
    pub fn load() -> Result<Self> {
        let data_dir = data_dir()?;
        let shown = shown(&data_dir);
        // The default folder is in your own home; one set elsewhere must be
        // somewhere nobody else can swap it out.
        let elsewhere = var("TM_DATA_DIR").is_some();
        // Not even made through someone else's link, which would have it
        // made (and then made private) wherever they chose.
        #[cfg(unix)]
        if elsewhere {
            links_are_yours(&data_dir)?;
        }
        std::fs::create_dir_all(&data_dir).with_context(|| format!("cannot create {shown}"))?;
        // Checked before anything is changed. The helper then gets the path
        // that was checked, with any links in it resolved.
        #[cfg(unix)]
        let data_dir = if elsewhere {
            private_place(&data_dir)?
        } else {
            data_dir
        };
        // It holds the login sessions and downloaded files: only for this user,
        // whatever the umask or the folder it's in allow.
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            std::fs::set_permissions(&data_dir, std::fs::Permissions::from_mode(0o700))
                .with_context(|| format!("cannot protect {shown}"))?;
            if elsewhere {
                kept_private(&data_dir)?;
            }
        }
        // Inside your user folder by its name, and also once any junction on
        // the way is followed.
        #[cfg(windows)]
        if elsewhere {
            let real = |path: &Path| {
                std::fs::canonicalize(path)
                    .with_context(|| format!("cannot read {}", self::shown(path)))
            };
            let home = profile().context("no user folder found (USERPROFILE)")?;
            if !within(&real(&data_dir)?, &real(&home)?) {
                bail!(outside_profile(&home));
            }
        }

        Ok(Self { data_dir })
    }
}

/// `TM_DATA_DIR`, else the platform's place for app data: `~/Library/Application
/// Support/tuimeta` on macOS, `~/.local/share/tuimeta` on Linux, `%LOCALAPPDATA%\tuimeta`
/// on Windows. The same wherever the command is run from.
pub fn data_dir() -> Result<PathBuf> {
    data_dir_from(var("TM_DATA_DIR"))
}

fn data_dir_from(set: Option<String>) -> Result<PathBuf> {
    if let Some(dir) = set {
        let dir = PathBuf::from(dir);
        // On Windows even creating a folder there hands that server your
        // login hash, and the session would live on it. A single leading
        // slash can also name one there (`\??\UNC\…`); `C:\` does the rest.
        // A relative path on Windows would sit under the working directory,
        // whose permissions aren't checked (the private_place guard below is
        // Unix-only), so it's refused: the error promises a drive letter.
        let elsewhere = attach::on_another_machine(&dir)
            || (cfg!(windows) && matches!(dir.as_os_str().as_encoded_bytes(), [b'/' | b'\\', ..]))
            || (cfg!(windows) && !dir.is_absolute());
        if elsewhere {
            bail!("TM_DATA_DIR must be a folder on this computer, with a drive letter on Windows");
        }
        // On Windows a folder takes the access of the folder it's made in,
        // and tuimeta sets none of its own: in your user folder that's you
        // alone, elsewhere (`C:\tuimeta`, another drive) it may let other
        // accounts on the computer read your sessions.
        if cfg!(windows) && !profile().is_some_and(|profile| within(&dir, &profile)) {
            let named = profile().unwrap_or_else(|| PathBuf::from("%USERPROFILE%"));
            bail!(outside_profile(&named));
        }
        return Ok(dir);
    }
    let base = dirs::data_local_dir().context("no home directory found; set TM_DATA_DIR")?;
    Ok(base.join("tuimeta"))
}

/// Your user folder on Windows, `%USERPROFILE%`.
fn profile() -> Option<PathBuf> {
    std::env::var_os("USERPROFILE")
        .map(PathBuf::from)
        .filter(|path| path.is_absolute())
}

fn outside_profile(profile: &Path) -> String {
    format!(
        "TM_DATA_DIR must be in your user folder ({}) on Windows: elsewhere, \
         other accounts on this computer may be able to read your sessions",
        shown(profile)
    )
}

/// `dir` is `base` or a folder in it, whatever the letter case (Windows
/// paths ignore it). Never with a `..`, which could lead back out.
fn within(dir: &Path, base: &Path) -> bool {
    use std::path::Component;
    let parts = |path: &Path| -> Vec<String> {
        path.components()
            .map(|part| part.as_os_str().to_string_lossy().to_lowercase())
            .collect()
    };
    let (dir_parts, base_parts) = (parts(dir), parts(base));
    !base_parts.is_empty()
        && !dir.components().any(|part| part == Component::ParentDir)
        && dir_parts.starts_with(&base_parts)
}

/// A path as it can be printed: it may come from the environment.
pub fn shown(path: &Path) -> String {
    text::clean(&path.display().to_string())
}

#[cfg(unix)]
fn unsafe_place(what: &Path) -> anyhow::Error {
    anyhow::anyhow!(
        "{} can be changed by other users, so it's no place for your session; \
         set TM_DATA_DIR somewhere in your home folder",
        shown(what)
    )
}

/// Checks that only you or the system can change the folders above `dir`:
/// otherwise another account could rename the data folder away and put its
/// own in its place, after this check and before the helper writes a session
/// into it. Every link on the way, the folder itself among them, must be
/// yours or the system's too. Folders anyone may write to, like `/tmp`, are
/// fine when only an entry's owner can move it (the sticky bit). Returns the
/// folder with its links resolved: the path that was checked, and the one
/// to use.
#[cfg(unix)]
fn private_place(dir: &Path) -> Result<PathBuf> {
    use std::os::unix::fs::MetadataExt;
    // SAFETY: geteuid can't fail and touches no memory.
    let uid = unsafe { libc::geteuid() };
    links_are_yours(dir)?;
    // Resolved once: links changed after this don't change what's used.
    let real = std::fs::canonicalize(dir)?;
    // It's made private next, which only its owner can.
    if std::fs::metadata(&real)?.uid() != uid {
        return Err(unsafe_place(&real));
    }
    let own_group = private_group();
    for folder in real.ancestors().skip(1) {
        let meta = std::fs::metadata(folder)?;
        let mode = meta.mode();
        let group_is_others = own_group != Some(meta.gid());
        let others_write = mode & 0o002 != 0 || (mode & 0o020 != 0 && group_is_others);
        let sticky = mode & 0o1000 != 0;
        let owner_ok = meta.uid() == uid || meta.uid() == 0;
        if !owner_ok || (others_write && !(sticky && meta.uid() == 0)) {
            return Err(unsafe_place(folder));
        }
    }
    Ok(real)
}

/// Every link on the way to `dir` (as far as it exists) is yours or the
/// system's: one someone else made could lead it anywhere, now or later.
#[cfg(unix)]
fn links_are_yours(dir: &Path) -> Result<()> {
    use std::os::unix::fs::MetadataExt;
    // SAFETY: geteuid can't fail and touches no memory.
    let uid = unsafe { libc::geteuid() };
    let mut at = PathBuf::new();
    for part in dir.components() {
        at.push(part);
        if let Ok(meta) = std::fs::symlink_metadata(&at)
            && meta.file_type().is_symlink()
            && meta.uid() != uid
            && meta.uid() != 0
        {
            return Err(unsafe_place(&at));
        }
    }
    Ok(())
}

/// The folder, made private, really is: on some drives a chmod succeeds and
/// changes nothing (FAT and exFAT, some network and FUSE mounts, a Mac's
/// external drive set to ignore ownership).
#[cfg(unix)]
fn kept_private(dir: &Path) -> Result<()> {
    use std::os::unix::fs::MetadataExt;
    let meta = std::fs::metadata(dir)?;
    // SAFETY: geteuid can't fail and touches no memory.
    let uid = unsafe { libc::geteuid() };
    if meta.uid() != uid || meta.mode() & 0o077 != 0 || ignores_ownership(dir) {
        bail!(
            "{} can't be kept for you alone: its drive ignores who owns what; \
             set TM_DATA_DIR somewhere in your home folder",
            shown(dir)
        );
    }
    Ok(())
}

/// The volume `dir` is on shows every file as yours, whoever made it.
#[cfg(target_os = "macos")]
fn ignores_ownership(dir: &Path) -> bool {
    use std::os::unix::ffi::OsStrExt;
    let Ok(path) = std::ffi::CString::new(dir.as_os_str().as_bytes()) else {
        return true;
    };
    let mut volume = std::mem::MaybeUninit::<libc::statfs>::zeroed();
    // SAFETY: `path` is a C string, and `volume` has room for what statfs
    // writes; it's read only if statfs says it filled it in.
    unsafe {
        libc::statfs(path.as_ptr(), volume.as_mut_ptr()) != 0
            || volume.assume_init().f_flags & libc::MNT_IGNORE_OWNERSHIP as u32 != 0
    }
}

#[cfg(all(unix, not(target_os = "macos")))]
fn ignores_ownership(_: &Path) -> bool {
    false
}

/// Your primary group, when it's yours alone: named like you, with no one
/// else in it, as most Linux systems and the BSDs make one for each account.
/// Then a folder it may write to is as good as yours. A group others are in
/// may be shared (`users`, or Active Directory's `Domain Users`), and on
/// macOS every account is in `staff`.
#[cfg(unix)]
fn private_group() -> Option<libc::gid_t> {
    if cfg!(target_os = "macos") {
        return None;
    }
    // SAFETY: neither can fail, and they touch no memory.
    let (uid, gid) = unsafe { (libc::geteuid(), libc::getegid()) };
    let user = user_name(uid)?;
    let (group, members) = group_of(gid)?;
    alone_in(&user, &group, &members).then_some(gid)
}

/// The group is `user`'s own: named after them, with no one else listed.
#[cfg(unix)]
fn alone_in(user: &str, group: &str, members: &[String]) -> bool {
    group == user && members.iter().all(|member| member == user)
}

/// Lookups that may need a bigger buffer give up past this.
#[cfg(unix)]
const MAX_LOOKUP: usize = 16 << 20;

/// The name of the account `uid`.
#[cfg(unix)]
fn user_name(uid: libc::uid_t) -> Option<String> {
    let mut buf = vec![0 as libc::c_char; 1024];
    loop {
        let mut entry = std::mem::MaybeUninit::<libc::passwd>::zeroed();
        let mut found = std::ptr::null_mut();
        // SAFETY: every pointer is to a local that outlives the call, and
        // `buf.len()` is the room `buf` has.
        let error = unsafe {
            libc::getpwuid_r(
                uid,
                entry.as_mut_ptr(),
                buf.as_mut_ptr(),
                buf.len(),
                &mut found,
            )
        };
        if error == libc::ERANGE && buf.len() < MAX_LOOKUP {
            buf.resize(buf.len() * 2, 0);
            continue;
        }
        if error != 0 || found.is_null() {
            return None;
        }
        // SAFETY: once found, `found` points at `entry`, whose name is a C string
        // in `buf`.
        let name = unsafe { std::ffi::CStr::from_ptr((*found).pw_name) };
        return Some(name.to_string_lossy().into_owned());
    }
}

/// The name of group `gid` and the accounts listed in it.
#[cfg(unix)]
fn group_of(gid: libc::gid_t) -> Option<(String, Vec<String>)> {
    let mut buf = vec![0 as libc::c_char; 4096];
    loop {
        let mut entry = std::mem::MaybeUninit::<libc::group>::zeroed();
        let mut found = std::ptr::null_mut();
        // SAFETY: as in `user_name`.
        let error = unsafe {
            libc::getgrgid_r(
                gid,
                entry.as_mut_ptr(),
                buf.as_mut_ptr(),
                buf.len(),
                &mut found,
            )
        };
        if error == libc::ERANGE && buf.len() < MAX_LOOKUP {
            buf.resize(buf.len() * 2, 0);
            continue;
        }
        if error != 0 || found.is_null() {
            return None;
        }
        // SAFETY: once found, `found` points at `entry`: its name is a C string,
        // and its members a list of them ending in a null pointer, all in
        // `buf`.
        unsafe {
            let group = &*found;
            let name = std::ffi::CStr::from_ptr(group.gr_name);
            let mut members = Vec::new();
            let mut at = group.gr_mem;
            while !at.is_null() && !(*at).is_null() {
                members.push(std::ffi::CStr::from_ptr(*at).to_string_lossy().into_owned());
                at = at.add(1);
            }
            return Some((name.to_string_lossy().into_owned(), members));
        }
    }
}

/// `TM_*` settings from the repository's `.env`, for development.
static DOTENV: OnceLock<HashMap<String, String>> = OnceLock::new();

/// Reads `.env` from the repository this build was made from, not from
/// wherever tuimeta is started, and keeps only its `TM_*` keys, without
/// touching the process environment. So a `.env` in some untrusted folder
/// can't pick the helper that runs with your sessions, or set `LD_PRELOAD`
/// or `BROWSER` for the programs tuimeta starts. Only development builds
/// read it: in an installed tuimeta, a `.env` could pick the folder your
/// session is kept in, or hand you one prepared by whoever wrote it.
pub fn load_dotenv() {
    if !cfg!(debug_assertions) {
        let _ = DOTENV.set(HashMap::new());
        return;
    }
    let vars = dotenvy::from_path_iter(concat!(env!("CARGO_MANIFEST_DIR"), "/.env"))
        .into_iter()
        .flatten()
        .filter_map(Result::ok)
        .filter(|(key, _)| key.starts_with("TM_"))
        .collect();
    let _ = DOTENV.set(vars);
}

/// There's a `.env` here that this build doesn't read: worth saying, since a
/// developer may expect it to pick a separate session.
pub fn dotenv_ignored() -> bool {
    !cfg!(debug_assertions) && Path::new(".env").is_file()
}

/// A variable's trimmed value from the environment, else from `.env`, or
/// `None` if it's unset or blank.
pub fn var(name: &str) -> Option<String> {
    let value = std::env::var(name)
        .ok()
        .or_else(|| DOTENV.get()?.get(name).cloned())?;
    let value = value.trim();
    (!value.is_empty()).then(|| value.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_data_folder_on_another_machine_is_refused() {
        for dir in ["//evil.example/s/tg", "\\\\evil.example\\s\\tg"] {
            assert!(data_dir_from(Some(dir.into())).is_err(), "{dir}");
        }
        let local = data_dir_from(Some("./.tuimeta".into())).unwrap();
        assert_eq!(local, PathBuf::from("./.tuimeta"));
    }

    #[cfg(unix)]
    #[test]
    fn a_data_folder_others_could_swap_out_is_refused() {
        use std::os::unix::fs::PermissionsExt;
        // As root, a folder you make is the system's, which is fine.
        // SAFETY: geteuid can't fail and touches no memory.
        if unsafe { libc::geteuid() } == 0 {
            return;
        }
        let parent = std::env::temp_dir().join(format!("tuimeta-place-{}", std::process::id()));
        let dir = parent.join("tg");
        std::fs::create_dir_all(&dir).unwrap();
        let mode = |path: &Path, mode| {
            std::fs::set_permissions(path, std::fs::Permissions::from_mode(mode)).unwrap()
        };
        mode(&parent, 0o777);
        assert!(private_place(&dir).is_err(), "anyone could rename it");
        mode(&parent, 0o1777);
        assert!(private_place(&dir).is_err(), "sticky, but not the system's");
        // Its group may write to it: fine only if the group is yours alone,
        // never on macOS, where every account is in `staff`.
        mode(&parent, 0o775);
        let group = std::os::unix::fs::MetadataExt::gid(&std::fs::metadata(&parent).unwrap());
        assert_eq!(
            private_place(&dir).is_ok(),
            private_group() == Some(group),
            "a group others are in is others"
        );
        mode(&parent, 0o755);
        private_place(&dir).unwrap();
        std::fs::remove_dir_all(&parent).unwrap();
    }

    #[cfg(unix)]
    #[test]
    fn the_folder_checked_is_the_one_used_with_its_links_resolved() {
        // SAFETY: geteuid can't fail and touches no memory.
        if unsafe { libc::geteuid() } == 0 {
            return;
        }
        let parent = std::env::temp_dir().join(format!("tuimeta-link-{}", std::process::id()));
        let real = parent.join("real");
        std::fs::create_dir_all(&real).unwrap();
        let link = parent.join("link");
        std::os::unix::fs::symlink(&real, &link).unwrap();
        assert_eq!(
            private_place(&link).unwrap(),
            std::fs::canonicalize(&real).unwrap(),
            "your own link"
        );
        links_are_yours(&link.join("not yet made")).unwrap();
        std::fs::remove_dir_all(&parent).unwrap();
    }

    #[cfg(unix)]
    #[test]
    fn a_data_folder_its_drive_wont_keep_private_is_refused() {
        use std::os::unix::fs::PermissionsExt;
        let dir = std::env::temp_dir().join(format!("tuimeta-kept-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::set_permissions(&dir, std::fs::Permissions::from_mode(0o700)).unwrap();
        kept_private(&dir).unwrap();
        // What a drive that ignores modes shows after the chmod.
        std::fs::set_permissions(&dir, std::fs::Permissions::from_mode(0o755)).unwrap();
        assert!(kept_private(&dir).is_err());
        std::fs::remove_dir_all(&dir).unwrap();
    }

    #[cfg(unix)]
    #[test]
    fn a_group_is_yours_alone_only_named_after_you_with_no_one_else() {
        let none: [String; 0] = [];
        assert!(alone_in("eric", "eric", &none));
        assert!(alone_in("eric", "eric", &["eric".to_string()]));
        assert!(!alone_in("eric", "users", &none));
        assert!(!alone_in("eric", "eric", &["bob".to_string()]));
        // The lookups work, on whatever system the tests run.
        // SAFETY: neither can fail, and they touch no memory.
        let (uid, gid) = unsafe { (libc::geteuid(), libc::getegid()) };
        assert!(user_name(uid).is_some_and(|name| !name.is_empty()));
        assert!(group_of(gid).is_some_and(|(name, _)| !name.is_empty()));
    }

    #[test]
    fn a_folder_is_within_another_by_its_parts_whatever_their_case() {
        let within = |dir: &str, base: &str| within(Path::new(dir), Path::new(base));
        assert!(within("/home/eric/tm", "/home/eric"));
        assert!(within("/home/eric", "/home/eric/"));
        assert!(
            within("/HOME/Eric/tm", "/home/eric"),
            "Windows ignores case"
        );
        assert!(!within("/home/ericx/tm", "/home/eric"));
        assert!(
            !within("/home/eric/../bob/tm", "/home/eric"),
            "leads back out"
        );
        assert!(!within("/srv/tm", "/home/eric"));
        assert!(!within("/srv/tm", ""));
    }

    #[test]
    fn paths_are_printed_without_control_characters() {
        assert_eq!(shown(Path::new("/tmp/a\u{1b}]0;x\u{7}b")), "/tmp/a]0;xb");
    }
}
