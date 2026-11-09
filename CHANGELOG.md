# Changelog

Older versions' changes are in the git log.

## 0.6.0 (2026-11-09)

### Added

- **Smooth scrolling in Instagram's and Threads' feeds.** Moving to another post with `j`, `k`, `Ctrl-d` or `Ctrl-u` now jumps most of the way and eases the last few rows, instead of landing all at once. `gg` and `G` still jump, and so does every move on terminals that draw pictures with sixel or iTerm2's protocol. It has its own row in `?`, **Smooth scrolling** (`smooth_scrolling = false` in `settings.toml`), apart from **Animations**; if you had turned Animations off, it starts off too. What Threads is told about the posts you view doesn't change.

## 0.5.0 (2026-11-07)

> [!CAUTION]
> **Privacy change: tuimeta now tells Threads which posts you look at.** In Threads' For you feed it reports each post you scroll past and how long it stayed on screen, the same report threads.com sends. Until now tuimeta sent none. It can't be turned off. If you don't want Threads to have this, stay on 0.4.5 or don't open For you.

### Changed

- **Threads' For you reports what you view, as the website does.** Without these reports Threads sends far fewer posts a page, then empty pages, and restarting the feed brings mostly posts you've already had: that's the "only 1 more post" you may have seen. In a test on 2026-11-07, pages stayed at about 7 rows with reports and fell to 2 or 3 without them, while nothing else about the request made a difference. A post is reported once it leaves the screen, and only while tuimeta's window has focus and you pressed a key in the last few minutes. Following sends no reports (the website's doesn't either), and neither do replies or a post opened by its link. Nothing changes for Instagram, Messenger or WhatsApp.
- **Threads' feed asks as the website asks today:** its own query for the first page, the current query versions and feature flags, 20 threads named as already given instead of 10, and the same session headers. The helper's protocol is now version 9, so the app and the helper must come from the same release.

### Fixed

- **Threads: no more lines joining unrelated posts.** When Threads sent a post again with a new reply under it, the reply got a thread line to whatever was above it. A post now keeps its line only under a post of its own thread.
