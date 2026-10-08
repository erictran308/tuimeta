mod app;
mod attach;
mod chats;
mod clipboard;
mod complete;
mod config;
mod demo;
mod images;
mod messages;
mod meta;
mod notify;
mod picker;
mod reactions;
mod search;
mod service;
mod settings;
mod text;
mod theme;
mod tmux;
mod ui;
mod viewer;

use std::io::{Write, stdout};

use anyhow::Result;
use crossterm::event::{
    DisableBracketedPaste, DisableFocusChange, EnableBracketedPaste, EnableFocusChange,
    KeyboardEnhancementFlags, PopKeyboardEnhancementFlags, PushKeyboardEnhancementFlags,
};
use crossterm::execute;
use ratatui_image::picker::Picker;

#[tokio::main]
async fn main() -> Result<()> {
    // A missing .env is fine: the variables can also come from the shell.
    config::load_dotenv();
    let fake = match std::env::args().nth(1).as_deref() {
        None => false,
        Some("-h" | "--help") => return print(&help()?),
        Some("-V" | "--version") => {
            return print(&format!("tuimeta {}\n", env!("CARGO_PKG_VERSION")));
        }
        Some("--demo") => return demo::run().await,
        Some("--fake") => true,
        Some(other) => anyhow::bail!("unknown argument {other:?}; see tuimeta --help"),
    };

    // Load config and start the helper before taking over the terminal, so
    // setup errors print as normal text.
    let config = config::Config::load()?;
    let settings_path = settings::path(&config.data_dir);
    let settings = settings::Settings::load(&settings_path)?;
    let (tx, rx) = tokio::sync::mpsc::unbounded_channel();
    let outbox = config.data_dir.join("outbox");
    clipboard::clean_outbox(&outbox);
    // Made-up accounts keep their own folder, so trying them leaves your
    // sessions alone.
    let helper_dir = config
        .data_dir
        .join(if fake { "helper-fake" } else { "helper" });
    // The helper takes only an absolute folder, so where it works can't
    // depend on where it was started.
    let helper_dir = std::path::absolute(&helper_dir)?;
    let meta = meta::Meta::start(&meta::helper_path()?, &helper_dir, fake, tx).await?;

    let mut terminal = ratatui::init();
    // The title shows unread chats while tuimeta runs, then goes back.
    notify::send(notify::SAVE_TITLE);
    notify::send(&notify::title(0));
    // A panic, on any thread but an image decoder's (caught there), ends
    // the app: the terminal is put back, then
    // the message is printed without control characters, since it can quote
    // text from a message. Carrying on after a background thread died would
    // leave the screen restored under a running app.
    std::panic::set_hook(Box::new(|info| {
        if images::panic_is_contained() {
            return;
        }
        let _ = execute!(
            stdout(),
            PopKeyboardEnhancementFlags,
            DisableBracketedPaste,
            DisableFocusChange
        );
        notify::send(notify::RESTORE_TITLE);
        ratatui::restore();
        tmux::restore();
        // The helper sees its stdin close as the process ends, and stops
        // without telling anyone anything.
        eprintln!("tuimeta crashed: {}", text::clean(&info.to_string()));
        std::process::exit(101);
    }));
    // Ask the terminal which image protocol it speaks (Kitty on Ghostty) and its
    // cell size in pixels. Must happen before key reading starts.
    tmux::save();
    let picker = Picker::from_query_stdio().unwrap_or_else(|_| Picker::halfblocks());
    // Pasted text arrives as one event instead of keystrokes, so a pasted line
    // break can't send a half-written message.
    execute!(stdout(), EnableBracketedPaste)?;
    // Read receipts wait while the terminal window is in the background, on
    // terminals that report focus changes.
    execute!(stdout(), EnableFocusChange)?;
    // Terminals with the kitty keyboard protocol (kitty, Ghostty, WezTerm…)
    // can then report Shift-Enter separately from Enter.
    let enhanced = crossterm::terminal::supports_keyboard_enhancement().unwrap_or(false);
    if enhanced {
        execute!(
            stdout(),
            PushKeyboardEnhancementFlags(KeyboardEnhancementFlags::DISAMBIGUATE_ESCAPE_CODES)
        )?;
    }

    let (image_tx, image_rx) = tokio::sync::mpsc::unbounded_channel();
    let images = images::Images::new(picker, image_tx);
    let (clipboard_tx, clipboard_rx) = tokio::sync::mpsc::unbounded_channel();
    let clipboard = clipboard::Clipboard::new(clipboard_tx, outbox);
    let mut app = app::App::new(meta, images, clipboard, settings, settings_path);
    app.browser = config::var("TM_BROWSER");
    let result = app.run(&mut terminal, rx, image_rx, clipboard_rx).await;

    if enhanced {
        let _ = execute!(stdout(), PopKeyboardEnhancementFlags);
    }
    let _ = execute!(stdout(), DisableBracketedPaste, DisableFocusChange);
    notify::send(notify::RESTORE_TITLE);
    ratatui::restore();
    tmux::restore();
    result
}

/// Writes to stdout. A reader that stops early (`tuimeta --help | head -1`,
/// `grep -q`) is fine, where `println!` would panic on the closed pipe.
fn print(text: &str) -> Result<()> {
    match stdout().lock().write_all(text.as_bytes()) {
        Err(e) if e.kind() == std::io::ErrorKind::BrokenPipe => Ok(()),
        result => Ok(result?),
    }
}

fn help() -> Result<String> {
    Ok(format!(
        "tuimeta {version}
Messenger and Instagram in your terminal, with vim-style keys.

Usage: tuimeta [-h | --help] [-V | --version] [--demo | --fake]

Inside the app, the status bar lists the keys for wherever you are;
press ? for settings and q to quit.

You log in by pasting your browser's cookies for facebook.com or
instagram.com. tuimeta isn't made or allowed by Meta: using it is against
Meta's terms, and an account can be locked or banned for it.

--demo shows made-up chats without starting anything:
1-9 or Tab switch scenes, t changes the theme, q quits.
--fake runs the whole app with made-up accounts and no network: log in
with any text that names the cookies, e.g. c_user=1; xs=2; datr=3.

Your login sessions, downloaded files and settings are kept in this
folder, and your own color themes in its themes folder:
  {data}

The helper, tuimeta-helper, must be next to tuimeta: it's the separate
program (under the AGPL) that speaks Meta's protocols.

Environment:
  TM_DATA_DIR    keep them somewhere else
  TM_BROWSER     the Chrome you copy cookies from, as chrome://version names
                 it (\"Chrome 150.0.7712.45\"): logins then say tuimeta is
                 that Chrome on this computer, for as long as they last
",
        version = env!("CARGO_PKG_VERSION"),
        data = config::shown(&config::data_dir()?),
    ))
}
