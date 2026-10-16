//! pepper-play, playing a feed's video over its post inside Pepper (see
//! [`crate::pepper`]). It takes mpv's options and mpv's JSON control
//! socket: tuimeta passes it one end of a socket pair as file descriptor 3
//! and sends it lines of JSON, first the token its box is placed with, then
//! the video to load. pepper-play quits when that socket closes.
//!
//! What it plays is a file the helper downloaded, by its absolute path,
//! never an address: nothing about what's watched goes to Instagram, and
//! mpv's own fetching (yt-dlp, its protocols) is never asked to run.

use std::path::Path;
#[cfg(unix)]
use std::process::Stdio;
#[cfg(unix)]
use std::time::Duration;

use anyhow::Result;
#[cfg(not(unix))]
use anyhow::bail;
use tokio::sync::mpsc::UnboundedSender;
#[cfg(unix)]
use tokio::sync::oneshot;

use crate::pepper::Pepper;

/// How long pepper-play has to quit on its own once its socket closes,
/// before it's killed.
#[cfg(unix)]
const QUIT_GRACE: Duration = Duration::from_secs(2);

/// The environment pepper-play keeps from tuimeta's, besides
/// `PEPPER_PORT`: what finding its library and the user's folders needs.
#[cfg(unix)]
const KEPT: [&str; 7] = [
    "HOME",
    "USER",
    "LOGNAME",
    "TMPDIR",
    "LANG",
    "LC_ALL",
    "PEPPER_LIBMPV",
];

/// Something about playback `play`.
#[derive(Debug, PartialEq)]
pub enum PlayerEvent {
    /// The video couldn't be played: why, in mpv's words.
    Failed { play: u64, why: String },
    /// Where it is, in seconds: every [`POSITION_EVERY`] while it plays,
    /// and at once after a seek or when it starts over.
    Position { play: u64, seconds: f64 },
    /// How long it is, in seconds.
    Duration { play: u64, seconds: f64 },
    /// pepper-play has quit.
    Exited { play: u64 },
}

/// The numbers the properties tuimeta follows are told by.
const POSITION_ID: u64 = 1;
const DURATION_ID: u64 = 2;
/// mpv tells the position at every frame; the seek bar needs it this often,
/// in seconds, and each one costs a frame of tuimeta's.
const POSITION_EVERY: f64 = 0.25;
/// How far `,` and `.` seek, in seconds.
pub const SEEK_STEP: i32 = 5;

/// One video playing. Dropping it stops the video.
pub struct Player {
    pub play: u64,
    #[cfg(unix)]
    control: Option<tokio::sync::mpsc::UnboundedSender<String>>,
    #[cfg(unix)]
    stop: Option<oneshot::Sender<()>>,
    /// What was sent, for tests (`Player::pretend`).
    #[cfg(test)]
    pub sent: std::sync::Arc<std::sync::Mutex<Vec<String>>>,
}

/// The options pepper-play starts with: no config or scripts of mpv's, no
/// terminal output, nothing fetched, the media keys and Now Playing left to
/// whatever else plays, and the clip over and over, as Instagram plays it.
#[cfg_attr(not(unix), allow(dead_code))]
fn args() -> Vec<String> {
    let mut args: Vec<String> = [
        "--input-ipc-client=fd://3",
        "--no-config",
        "--load-scripts=no",
        "--terminal=no",
        "--ytdl=no",
        "--keep-open=no",
        "--loop-file=inf",
        "--save-position-on-quit=no",
        "--resume-playback=no",
        "--input-media-keys=no",
    ]
    .map(String::from)
    .into();
    // On macOS, mpv's CoreAudio output doesn't open for 48 kHz sound in
    // planar float, what AAC decodes to, and mpv falls back to AVFoundation,
    // which holds 2 s of sound. The same float, interleaved, opens it.
    if cfg!(target_os = "macos") {
        args.push("--audio-format=float".into());
    }
    args
}

/// One command for the control socket, as a line of JSON: strings are JSON
/// strings, so a path can't break out of its place.
fn line(command: serde_json::Value) -> String {
    format!("{}\n", serde_json::json!({ "command": command }))
}

/// What's sent to play `file`: the token first, so the first frame has a
/// box to go to, then the properties the seek bar follows, then its sound
/// when that's a file of its own, added to the list of sound files so it
/// plays with the picture, then the picture.
fn commands(token: &str, file: &Path, audio: Option<&Path>) -> Vec<String> {
    let mut commands = vec![
        line(serde_json::json!([
            "script-message-to",
            "main",
            "pepper-token",
            token
        ])),
        line(serde_json::json!([
            "observe_property",
            POSITION_ID,
            "time-pos"
        ])),
        line(serde_json::json!([
            "observe_property",
            DURATION_ID,
            "duration"
        ])),
    ];
    if let Some(audio) = audio {
        commands.push(line(serde_json::json!([
            "change-list",
            "audio-files",
            "append",
            audio.to_string_lossy()
        ])));
    }
    commands.push(line(serde_json::json!([
        "loadfile",
        file.to_string_lossy(),
        "replace"
    ])));
    commands
}

impl Player {
    /// Starts pepper-play on `file`, a video the helper downloaded, with
    /// `audio`, its sound when that's a file of its own; its box goes where
    /// `token` is placed.
    #[cfg(unix)]
    pub fn start(
        pepper: &Pepper,
        token: &str,
        file: &Path,
        audio: Option<&Path>,
        play: u64,
        tx: UnboundedSender<PlayerEvent>,
    ) -> Result<Self> {
        use anyhow::Context;
        use std::os::fd::AsRawFd;
        // An absolute path is a file to mpv, never one of its protocols.
        anyhow::ensure!(
            file.is_absolute() && audio.is_none_or(Path::is_absolute),
            "the video isn't a file here"
        );
        let mut command = tokio::process::Command::new(&pepper.play);
        command
            .args(args())
            .env_clear()
            .envs(KEPT.iter().filter_map(|&k| Some((k, std::env::var_os(k)?))))
            .env("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")
            .env("PEPPER_PORT", &pepper.port)
            // Nothing of it on tuimeta's screen.
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .kill_on_drop(true);
        if let Some(home) = dirs::home_dir() {
            command.current_dir(home);
        }
        let (ours, theirs) = std::os::unix::net::UnixStream::pair()?;
        let fd = theirs.as_raw_fd();
        // SAFETY: only dup2 or fcntl runs between fork and exec, both
        // async-signal-safe. dup2 also clears close-on-exec on fd 3.
        unsafe {
            command.pre_exec(move || {
                let ok = if fd == 3 {
                    libc::fcntl(3, libc::F_SETFD, 0) != -1
                } else {
                    libc::dup2(fd, 3) != -1
                };
                if !ok {
                    return Err(std::io::Error::last_os_error());
                }
                Ok(())
            });
        }
        let child = command.spawn().context("can't start pepper-play")?;
        drop(theirs);
        ours.set_nonblocking(true)?;
        let stream = tokio::net::UnixStream::from_std(ours)?;
        let (control, lines) = tokio::sync::mpsc::unbounded_channel();
        spawn_ipc(play, stream, lines, tx.clone());
        for line in commands(token, file, audio) {
            let _ = control.send(line);
        }
        let (stop, stopped) = oneshot::channel();
        tokio::spawn(own(play, child, stopped, tx));
        Ok(Self {
            play,
            control: Some(control),
            stop: Some(stop),
            #[cfg(test)]
            sent: Default::default(),
        })
    }

    #[cfg(not(unix))]
    pub fn start(
        _pepper: &Pepper,
        _token: &str,
        _file: &Path,
        _audio: Option<&Path>,
        _play: u64,
        _tx: UnboundedSender<PlayerEvent>,
    ) -> Result<Self> {
        bail!("videos play inside the terminal only on macOS")
    }

    /// A player with no pepper-play behind it, which keeps what it's sent,
    /// for tests.
    #[cfg(test)]
    pub fn pretend(token: &str, file: &Path, audio: Option<&Path>, play: u64) -> Self {
        let player = Self {
            play,
            #[cfg(unix)]
            control: None,
            #[cfg(unix)]
            stop: None,
            sent: Default::default(),
        };
        player
            .sent
            .lock()
            .unwrap()
            .extend(commands(token, file, audio));
        player
    }

    fn send(&self, command: serde_json::Value) {
        let line = line(command);
        #[cfg(test)]
        self.sent.lock().unwrap().push(line.clone());
        #[cfg(unix)]
        if let Some(control) = &self.control {
            let _ = control.send(line);
        }
        #[cfg(not(unix))]
        let _ = line;
    }

    pub fn toggle_pause(&self) {
        self.send(serde_json::json!(["cycle", "pause"]));
    }

    pub fn toggle_mute(&self) {
        self.send(serde_json::json!(["cycle", "mute"]));
    }

    /// Jumps `seconds` on (back, when negative), to the frame rather than
    /// the keyframe before: clips are short, and a seek of 5 s shouldn't
    /// land 2 s off.
    pub fn seek(&self, seconds: i32) {
        self.send(serde_json::json!(["seek", seconds, "relative+exact"]));
    }
}

impl Drop for Player {
    fn drop(&mut self) {
        #[cfg(unix)]
        {
            // Closing the socket makes pepper-play quit.
            self.control = None;
            if let Some(stop) = self.stop.take() {
                let _ = stop.send(());
            }
        }
    }
}

/// Waits for pepper-play to end, or, once told to stop, gives it
/// [`QUIT_GRACE`] to quit before killing it.
#[cfg(unix)]
async fn own(
    play: u64,
    mut child: tokio::process::Child,
    stop: oneshot::Receiver<()>,
    tx: UnboundedSender<PlayerEvent>,
) {
    tokio::select! {
        _ = child.wait() => {}
        _ = stop => {
            if tokio::time::timeout(QUIT_GRACE, child.wait()).await.is_err() {
                let _ = child.start_kill();
                let _ = child.wait().await;
            }
        }
    }
    let _ = tx.send(PlayerEvent::Exited { play });
}

/// Writes the commands to the socket, and reads what mpv says on it for
/// a file it couldn't play.
#[cfg(unix)]
fn spawn_ipc(
    play: u64,
    stream: tokio::net::UnixStream,
    mut lines: tokio::sync::mpsc::UnboundedReceiver<String>,
    tx: UnboundedSender<PlayerEvent>,
) {
    use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
    let (read, mut write) = stream.into_split();
    tokio::spawn(async move {
        while let Some(line) = lines.recv().await {
            if write.write_all(line.as_bytes()).await.is_err() {
                break;
            }
        }
        // Dropping the writer closes our end: pepper-play quits.
    });
    tokio::spawn(async move {
        let mut lines = BufReader::new(read).lines();
        let mut told: Option<f64> = None;
        while let Ok(Some(line)) = lines.next_line().await {
            if line.len() > 64 * 1024 {
                continue;
            }
            let event = match heard(&line) {
                Some(Heard::Failed(why)) => PlayerEvent::Failed { play, why },
                Some(Heard::Position(seconds)) if worth_telling(told, seconds) => {
                    told = Some(seconds);
                    PlayerEvent::Position { play, seconds }
                }
                Some(Heard::Duration(seconds)) => PlayerEvent::Duration { play, seconds },
                _ => continue,
            };
            if tx.send(event).is_err() {
                break;
            }
        }
    });
}

/// What a line from mpv says, of what tuimeta follows.
#[derive(Debug, PartialEq)]
enum Heard {
    /// The file couldn't be played: why, on one line.
    Failed(String),
    Position(f64),
    Duration(f64),
}

/// What `line` from mpv says: a file it couldn't play, or a followed
/// property's new value (a number of seconds, finite and not negative).
#[cfg_attr(not(unix), allow(dead_code))]
fn heard(line: &str) -> Option<Heard> {
    #[derive(serde::Deserialize)]
    struct Event {
        event: Option<String>,
        reason: Option<String>,
        file_error: Option<String>,
        id: Option<u64>,
        data: Option<serde_json::Value>,
    }
    let event: Event = serde_json::from_str(line).ok()?;
    match event.event.as_deref()? {
        "end-file" if event.reason.as_deref() == Some("error") => {
            let why = event.file_error.unwrap_or_else(|| "it didn't load".into());
            Some(Heard::Failed(crate::messages::one_line(&why)))
        }
        "property-change" => {
            let seconds = event
                .data?
                .as_f64()
                .filter(|s| s.is_finite() && *s >= 0.0)?;
            match event.id? {
                POSITION_ID => Some(Heard::Position(seconds)),
                DURATION_ID => Some(Heard::Duration(seconds)),
                _ => None,
            }
        }
        _ => None,
    }
}

/// A position worth passing on, after `told` was the last: one
/// [`POSITION_EVERY`] on, or back (a seek, or the clip starting over).
#[cfg_attr(not(unix), allow(dead_code))]
fn worth_telling(told: Option<f64>, now: f64) -> bool {
    told.is_none_or(|told| now < told || now - told >= POSITION_EVERY)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_token_goes_before_the_file_and_nothing_is_fetched() {
        let sent = commands("abc123", Path::new("/data/helper/files/7.mp4"), None);
        assert!(sent[0].contains("pepper-token") && sent[0].contains("abc123"));
        assert!(sent[1].contains(r#"["observe_property",1,"time-pos"]"#));
        assert!(sent[2].contains(r#"["observe_property",2,"duration"]"#));
        assert!(sent[3].contains(r#""loadfile","/data/helper/files/7.mp4""#));
        // A sound of its own goes in before the picture loads, so it plays
        // with it.
        let apart = commands(
            "abc123",
            Path::new("/data/7.mp4"),
            Some(Path::new("/data/8.m4a")),
        );
        assert!(apart[3].contains(r#"["change-list","audio-files","append","/data/8.m4a"]"#));
        assert!(apart[4].contains("loadfile"));
        let args = args();
        for option in [
            "--ytdl=no",
            "--no-config",
            "--load-scripts=no",
            "--terminal=no",
        ] {
            assert!(args.iter().any(|a| a == option), "{option}");
        }
        // The token never goes on the command line.
        assert!(!args.iter().any(|a| a.contains("abc123")));
    }

    #[test]
    fn a_file_mpv_could_not_play_says_why_on_one_line() {
        let line =
            r#"{"event":"end-file","reason":"error","file_error":"unrecognized\nfile format"}"#;
        assert_eq!(
            heard(line),
            Some(Heard::Failed("unrecognized file format".into()))
        );
        assert_eq!(heard(r#"{"event":"end-file","reason":"eof"}"#), None);
        assert_eq!(heard(r#"{"event":"playback-restart"}"#), None);
        assert_eq!(heard("not json"), None);
    }

    #[test]
    fn the_position_and_length_are_followed_a_few_times_a_second() {
        let change = |id: u64, data: &str| {
            heard(&format!(
                r#"{{"event":"property-change","id":{id},"name":"x","data":{data}}}"#
            ))
        };
        assert_eq!(change(1, "12.5"), Some(Heard::Position(12.5)));
        assert_eq!(change(2, "30.03"), Some(Heard::Duration(30.03)));
        // Nothing yet, nonsense, or a property tuimeta didn't ask for.
        for (id, data) in [(1, "null"), (1, "-1"), (2, r#""30""#), (9, "1")] {
            assert_eq!(change(id, data), None, "{id} {data}");
        }
        assert!(worth_telling(None, 0.0));
        assert!(!worth_telling(Some(1.0), 1.1), "a frame on");
        assert!(worth_telling(Some(1.0), 1.25));
        assert!(worth_telling(Some(29.9), 0.1), "started over");
        assert!(worth_telling(Some(10.0), 9.95), "a seek back");
        let player = Player::pretend("t", Path::new("/data/7.mp4"), None, 1);
        player.seek(-SEEK_STEP);
        let sent = player.sent.lock().unwrap();
        assert!(
            sent.last()
                .unwrap()
                .contains(r#"["seek",-5,"relative+exact"]"#)
        );
    }

    /// pepper-play, as Pepper names it to its shells, takes tuimeta's
    /// options and plays a downloaded file until told to stop, then quits.
    /// Its box is placed nowhere, so nothing shows. Inside Pepper (outside
    /// tmux): `cargo test -- --ignored pepper_play`.
    #[tokio::test]
    #[ignore]
    async fn pepper_play_takes_our_options_and_plays_a_file_until_stopped() {
        use image::codecs::gif::GifEncoder;
        let pepper = crate::pepper::find().expect("run this inside Pepper, outside tmux");
        let dir = std::env::temp_dir().join(format!("tuimeta-play-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("clip.gif");
        let frames = (0..10u8).map(|i| {
            let picture = image::RgbaImage::from_pixel(64, 80, image::Rgba([i * 25, 80, 160, 255]));
            image::Frame::from_parts(picture, 0, 0, image::Delay::from_numer_denom_ms(100, 1))
        });
        GifEncoder::new(std::fs::File::create(&path).unwrap())
            .encode_frames(frames)
            .unwrap();
        // Its sound apart, as Instagram's DASH videos have it: silence, so
        // the test plays nothing out loud.
        let sound = dir.join("sound.wav");
        let (rate, samples) = (8000u32, 16000u32);
        let mut wav = Vec::new();
        wav.extend(b"RIFF");
        wav.extend((36 + samples * 2).to_le_bytes());
        wav.extend(b"WAVEfmt ");
        wav.extend(16u32.to_le_bytes());
        wav.extend(1u16.to_le_bytes()); // PCM
        wav.extend(1u16.to_le_bytes()); // mono
        wav.extend(rate.to_le_bytes());
        wav.extend((rate * 2).to_le_bytes());
        wav.extend(2u16.to_le_bytes());
        wav.extend(16u16.to_le_bytes());
        wav.extend(b"data");
        wav.extend((samples * 2).to_le_bytes());
        wav.resize(wav.len() + samples as usize * 2, 0);
        std::fs::write(&sound, wav).unwrap();
        let (tx, mut rx) = tokio::sync::mpsc::unbounded_channel();
        let token = crate::pepper::new_token().unwrap();
        let player = Player::start(&pepper, &token, &path, Some(&sound), 1, tx).unwrap();
        // Playing, over and over: neither a failure nor an exit, and its
        // length and position told.
        let mut heard = Vec::new();
        let until = tokio::time::Instant::now() + Duration::from_secs(3);
        while let Ok(Some(event)) = tokio::time::timeout_at(until, rx.recv()).await {
            heard.push(event);
        }
        assert!(
            heard.iter().all(|e| matches!(
                e,
                PlayerEvent::Position { .. } | PlayerEvent::Duration { .. }
            )),
            "pepper-play said {heard:?}"
        );
        assert!(
            heard
                .iter()
                .any(|e| matches!(e, PlayerEvent::Duration { seconds, .. } if *seconds > 0.5))
        );
        let positions = heard
            .iter()
            .filter(|e| matches!(e, PlayerEvent::Position { .. }));
        assert!(positions.count() >= 3, "{heard:?}");
        drop(player);
        // It quits, perhaps after telling a last position.
        let until = tokio::time::Instant::now() + Duration::from_secs(5);
        let gone = loop {
            match tokio::time::timeout_at(until, rx.recv()).await {
                Ok(Some(PlayerEvent::Position { .. } | PlayerEvent::Duration { .. })) => continue,
                other => break other.ok().flatten(),
            }
        };
        assert_eq!(gone, Some(PlayerEvent::Exited { play: 1 }));
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[cfg(unix)]
    #[test]
    fn only_a_file_on_this_machine_is_played() {
        let pepper = Pepper {
            port: "x.1".into(),
            play: "/nonexistent/pepper-play".into(),
        };
        let (tx, _rx) = tokio::sync::mpsc::unbounded_channel();
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap();
        let _guard = runtime.enter();
        for (file, audio) in [
            ("video.mp4", None),
            ("/data/7.mp4", Some(Path::new("8.m4a"))),
        ] {
            let err = Player::start(&pepper, "t", Path::new(file), audio, 1, tx.clone())
                .err()
                .unwrap();
            assert!(err.to_string().contains("isn't a file"), "{err}");
        }
    }
}
