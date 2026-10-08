# Security

## Reporting a vulnerability

Report it privately at <https://github.com/erictran308/tuimeta/security/advisories/new> (Security → Report a vulnerability), not in a public issue: tuimeta holds people's Messenger and Instagram sessions, and a public report tells everyone how to reach them before a fix is out.

Say what someone has to send or do, what happens, your tuimeta version (`tuimeta --version`), OS and terminal.

## What counts

Anything another Messenger or Instagram user, or a file, link or folder tuimeta is pointed at, can do to you through tuimeta or its helper. For example:

- a message, name, link, file or image that runs code, opens something without asking, or garbles the terminal;
- messages marked read, or typing shown, while you're away;
- your session cookies or encrypted-chat keys read, logged, replaced or left where another account can reach them;
- files sent that you didn't pick;
- the helper fetching a URL from a message, or sending anything the app didn't ask for.

Bugs in mautrix-meta or whatsmeow themselves belong at <https://github.com/mautrix/meta> and <https://github.com/tulir/whatsmeow>.

## What tuimeta can't protect you from

tuimeta isn't made or allowed by Meta. Meta can tell an unofficial client apart, and may lock or ban an account that uses one. Instagram's direct messages aren't end-to-end encrypted, so Meta can read them whatever app you use; Messenger's encrypted chats are encrypted between the devices in them, tuimeta's helper among them.
