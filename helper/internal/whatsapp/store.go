// SPDX-License-Identifier: AGPL-3.0-or-later

package whatsapp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strings"

	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/erictran308/tuimeta/helper/internal/fsutil"

	// A SQLite written in Go, so the helper still builds with CGO_ENABLED=0.
	// It registers the "sqlite" driver; mautrix's dbutil reads any driver
	// name starting with "sqlite" as the SQLite dialect.
	_ "modernc.org/sqlite"
)

// storeFile is the account's database in the network's session folder:
// whatsmeow's device (keys, sessions, sender keys, contacts, app state) and
// the chats and messages this device has.
const storeFile = "wa.db"

// MaxStoredPerChat bounds how many messages are kept per chat; older ones
// are asked of the phone again when scrolled to.
const MaxStoredPerChat = 3000

// waStore is the open database.
type waStore struct {
	db        *sql.DB
	container *sqlstore.Container
}

// openStore opens the database at path, made 0600 before SQLite ever opens
// it: SQLite gives its -wal and -shm files the database file's mode, so they
// are private too. It must be this user's own plain file: a link planted in
// its place is refused, not followed.
func openStore(ctx context.Context, path string) (*waStore, error) {
	if strings.ContainsRune(path, '?') {
		return nil, errors.New("store path contains '?'")
	}
	f, err := fsutil.OpenPrivate(path, os.O_RDWR|os.O_CREATE)
	if err != nil {
		return nil, err
	}
	f.Close()
	// Foreign keys are on, as whatsmeow requires; secure_delete overwrites
	// deleted rows (old keys, deleted and disappearing messages) instead of
	// leaving them in free pages.
	dsn := path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=secure_delete(1)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A few connections, as whatsmeow may read outside a transaction while
	// one is open; SQLite serializes the writers (busy_timeout waits).
	db.SetMaxOpenConns(4)
	container := sqlstore.NewWithDB(db, "sqlite", waLog.Noop)
	if err := container.Upgrade(ctx); err != nil {
		db.Close()
		return nil, err
	}
	s := &waStore{db: db, container: container}
	if err := s.upgrade(ctx); err != nil {
		db.Close()
		return nil, err
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Chmod(path+suffix, 0o600)
	}
	return s, nil
}

// upgrade makes the helper's own tables. Rows hold JSON (chatRow, message),
// so what's kept can grow without migrations.
func (s *waStore) upgrade(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS tuimeta_chat (
			jid  TEXT PRIMARY KEY,
			data TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS tuimeta_message (
			chat    TEXT    NOT NULL,
			id      TEXT    NOT NULL,
			ts      INTEGER NOT NULL,
			expires INTEGER NOT NULL DEFAULT 0,
			data    TEXT    NOT NULL,
			PRIMARY KEY (chat, id)
		);
		CREATE INDEX IF NOT EXISTS tuimeta_message_ts ON tuimeta_message (chat, ts);
		CREATE INDEX IF NOT EXISTS tuimeta_message_expires ON tuimeta_message (expires) WHERE expires > 0;
		CREATE TABLE IF NOT EXISTS tuimeta_deleted (
			chat   TEXT    NOT NULL,
			id     TEXT    NOT NULL,
			sender TEXT    NOT NULL,
			sure   INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (chat, id, sender)
		);
	`)
	return err
}

// tombstone says a message was deleted (for everyone, for you, or its time
// up), so another copy of it (the phone's history, a late decryption) never
// brings it back. It names the message's sender: only that sender's message
// with the id is kept out, unless sure, when the id was certainly that
// sender's (the message was kept here, or your phone deleted it) and so is
// nobody else's either.
type tombstone struct {
	id, sender string
	sure       bool
}

// bars reports whether t keeps out a message with id from sender; same
// compares people.
func (t tombstone) bars(id, sender string, same func(a, b string) bool) bool {
	return t.id == id && (t.sure || same(t.sender, sender))
}

func (s *waStore) Close() error { return s.db.Close() }

// chats is every kept chat, by JID.
func (s *waStore) chats(ctx context.Context) (map[string]chatRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT jid, data FROM tuimeta_chat`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]chatRow{}
	for rows.Next() {
		var jid, data string
		if err := rows.Scan(&jid, &data); err != nil {
			return nil, err
		}
		var row chatRow
		if json.Unmarshal([]byte(data), &row) == nil {
			out[jid] = row
		}
	}
	return out, rows.Err()
}

func (s *waStore) putChat(ctx context.Context, jid string, row chatRow) error {
	data, err := json.Marshal(row)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO tuimeta_chat (jid, data) VALUES (?, ?)
		ON CONFLICT (jid) DO UPDATE SET data=excluded.data`, jid, string(data))
	return err
}

// deleteChat forgets a chat, its messages and who had sent its deleted ones.
func (s *waStore) deleteChat(ctx context.Context, jid string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, q := range []string{
			`DELETE FROM tuimeta_message WHERE chat=?`,
			`DELETE FROM tuimeta_deleted WHERE chat=?`,
			`DELETE FROM tuimeta_chat WHERE jid=?`,
		} {
			if _, err := tx.ExecContext(ctx, q, jid); err != nil {
				return err
			}
		}
		return nil
	})
}

// clearChat forgets a chat's messages, keeping the chat.
func (s *waStore) clearChat(ctx context.Context, jid string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM tuimeta_message WHERE chat=?`, jid)
	return err
}

// renameChat moves a chat and its messages to another JID (a chat by phone
// number that turned out to be one by WhatsApp id). Messages already under
// the new JID win.
func (s *waStore) renameChat(ctx context.Context, from, to string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE OR IGNORE tuimeta_message SET chat=? WHERE chat=?`, to, from); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM tuimeta_message WHERE chat=?`, from); err != nil {
			return err
		}
		if err := moveDeleted(ctx, tx, from, to); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE OR IGNORE tuimeta_chat SET jid=? WHERE jid=?`, to, from); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM tuimeta_chat WHERE jid=?`, from)
		return err
	})
}

// renameDeleted moves what's kept of a chat's deleted messages to another
// JID, for a chat that isn't here (yet) whose person turned out to have a
// WhatsApp id.
func (s *waStore) renameDeleted(ctx context.Context, from, to string) error {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM tuimeta_deleted WHERE chat=? LIMIT 1`, from).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // nothing to move, and no write for it
	}
	if err != nil {
		return err
	}
	return s.tx(ctx, func(tx *sql.Tx) error { return moveDeleted(ctx, tx, from, to) })
}

func moveDeleted(ctx context.Context, tx *sql.Tx, from, to string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE OR IGNORE tuimeta_deleted SET chat=? WHERE chat=?`, to, from); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM tuimeta_deleted WHERE chat=?`, from)
	return err
}

// messages is a chat's kept messages, oldest first.
func (s *waStore) messages(ctx context.Context, chat string) ([]*message, error) {
	// Within a second (WhatsApp's timestamps have no finer), the order they
	// were kept in, which is the order they came in.
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM tuimeta_message WHERE chat=? ORDER BY ts, rowid`, chat)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*message
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var m message
		if json.Unmarshal([]byte(data), &m) == nil && m.ID != "" {
			out = append(out, &m)
		}
	}
	return out, rows.Err()
}

// newest is each chat's newest kept message, for the chat list's previews.
func (s *waStore) newest(ctx context.Context) (map[string]*message, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT chat, data FROM (
			SELECT chat, data, ROW_NUMBER() OVER (PARTITION BY chat ORDER BY ts DESC, rowid DESC) AS n FROM tuimeta_message
		) WHERE n = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*message{}
	for rows.Next() {
		var chat, data string
		if err := rows.Scan(&chat, &data); err != nil {
			return nil, err
		}
		var m message
		if json.Unmarshal([]byte(data), &m) == nil && m.ID != "" {
			out[chat] = &m
		}
	}
	return out, rows.Err()
}

// message is one kept message of a chat, nil if there's none.
func (s *waStore) message(ctx context.Context, chat, id string) (*message, error) {
	var data string
	err := s.db.QueryRowContext(ctx, `SELECT data FROM tuimeta_message WHERE chat=? AND id=?`, chat, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decode(data), nil
}

// decode is a message row's data, nil if it can't be read.
func decode(data string) *message {
	var m message
	if json.Unmarshal([]byte(data), &m) != nil || m.ID == "" {
		return nil
	}
	return &m
}

// putMessages keeps messages of a chat, replacing ones with the same id,
// then trims the chat to MaxStoredPerChat; it returns what the trim took. A
// message a tombstone keeps out is never kept, whatever asks.
func (s *waStore) putMessages(ctx context.Context, chat string, ms ...*message) ([]*message, error) {
	if len(ms) == 0 {
		return nil, nil
	}
	var trimmed []*message
	err := s.tx(ctx, func(tx *sql.Tx) error {
		for _, m := range ms {
			data, err := json.Marshal(m)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO tuimeta_message (chat, id, ts, expires, data)
				SELECT ?, ?, ?, ?, ? WHERE NOT EXISTS (
					SELECT 1 FROM tuimeta_deleted WHERE chat=? AND id=? AND (sure=1 OR sender=?)
				)
				ON CONFLICT (chat, id) DO UPDATE SET ts=excluded.ts, expires=excluded.expires, data=excluded.data`,
				chat, m.ID, m.MS, m.Expires, string(data), chat, m.ID, m.Sender); err != nil {
				return err
			}
		}
		const beyond = `chat=? AND rowid NOT IN (SELECT rowid FROM tuimeta_message WHERE chat=? ORDER BY ts DESC LIMIT ?)`
		rows, err := tx.QueryContext(ctx, `SELECT data FROM tuimeta_message WHERE `+beyond, chat, chat, MaxStoredPerChat)
		if err != nil {
			return err
		}
		for rows.Next() {
			var data string
			if err := rows.Scan(&data); err != nil {
				rows.Close()
				return err
			}
			if m := decode(data); m != nil {
				trimmed = append(trimmed, m)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(trimmed) == 0 {
			return nil
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM tuimeta_message WHERE `+beyond, chat, chat, MaxStoredPerChat)
		return err
	})
	if err != nil {
		return nil, err
	}
	return trimmed, nil
}

// deleteMessage deletes a kept message, and keeps gone (if any) so it never
// comes back.
func (s *waStore) deleteMessage(ctx context.Context, chat, id string, gone *tombstone) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM tuimeta_message WHERE chat=? AND id=?`, chat, id); err != nil {
			return err
		}
		if gone == nil {
			return nil
		}
		return putDeleted(ctx, tx, chat, *gone)
	})
}

// MaxDeletedPerChat bounds the deleted messages remembered per chat: those
// certainly their sender's first, then the latest deleted.
const MaxDeletedPerChat = MaxStoredPerChat

// putDeleted keeps a chat's tombstone.
func (s *waStore) putDeleted(ctx context.Context, chat string, t tombstone) error {
	return s.tx(ctx, func(tx *sql.Tx) error { return putDeleted(ctx, tx, chat, t) })
}

func putDeleted(ctx context.Context, tx *sql.Tx, chat string, t tombstone) error {
	sure := 0
	if t.sure {
		sure = 1
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tuimeta_deleted (chat, id, sender, sure) VALUES (?, ?, ?, ?)
		ON CONFLICT (chat, id, sender) DO UPDATE SET sure=MAX(sure, excluded.sure)`,
		chat, t.id, t.sender, sure); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
		DELETE FROM tuimeta_deleted WHERE chat=? AND rowid NOT IN (
			SELECT rowid FROM tuimeta_deleted WHERE chat=? ORDER BY sure DESC, rowid DESC LIMIT ?
		)`, chat, chat, MaxDeletedPerChat)
	return err
}

// deletedIn is a chat's tombstones.
func (s *waStore) deletedIn(ctx context.Context, chat string) ([]tombstone, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, sender, sure FROM tuimeta_deleted WHERE chat=?`, chat)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []tombstone
	for rows.Next() {
		var t tombstone
		if err := rows.Scan(&t.id, &t.sender, &t.sure); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// expiry is a kept message whose time ran out, with what's kept of it.
type expiry struct {
	chat, id string
	msg      *message // nil if it can't be read
}

// expired lists messages that disappear by ms.
func (s *waStore) expired(ctx context.Context, ms int64) ([]expiry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT chat, id, data FROM tuimeta_message WHERE expires > 0 AND expires <= ?`, ms)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []expiry
	for rows.Next() {
		var e expiry
		var data string
		if err := rows.Scan(&e.chat, &e.id, &data); err != nil {
			return nil, err
		}
		e.msg = decode(data)
		out = append(out, e)
	}
	return out, rows.Err()
}

// deleteExpired deletes the messages that disappear by ms.
func (s *waStore) deleteExpired(ctx context.Context, ms int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM tuimeta_message WHERE expires > 0 AND expires <= ?`, ms)
	return err
}

// pruneSecrets deletes the keys whatsmeow keeps for messages (to read later
// reactions and votes on them) of messages that aren't kept here: deleted,
// disappeared, or never shown.
func (s *waStore) pruneSecrets(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM whatsmeow_message_secrets
		WHERE message_id NOT IN (SELECT id FROM tuimeta_message)`)
	return err
}

// checkpoint writes the write-ahead log into the database and empties it,
// so rows deleted (and overwritten there, secure_delete) aren't left in it.
func (s *waStore) checkpoint(ctx context.Context) {
	_, _ = s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
}

// prune keeps the newest MaxStoredPerChat messages of every chat, once at
// startup, so what's read back stays bounded whatever an older run kept. It
// returns what it took.
func (s *waStore) prune(ctx context.Context) ([]*message, error) {
	const beyond = `rowid IN (
		SELECT rowid FROM (
			SELECT rowid, ROW_NUMBER() OVER (PARTITION BY chat ORDER BY ts DESC) AS n FROM tuimeta_message
		) WHERE n > ?
	)`
	var pruned []*message
	err := s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT data FROM tuimeta_message WHERE `+beyond, MaxStoredPerChat)
		if err != nil {
			return err
		}
		for rows.Next() {
			var data string
			if err := rows.Scan(&data); err != nil {
				rows.Close()
				return err
			}
			if m := decode(data); m != nil {
				pruned = append(pruned, m)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM tuimeta_message WHERE `+beyond, MaxStoredPerChat)
		return err
	})
	if err != nil {
		return nil, err
	}
	return pruned, nil
}

func (s *waStore) tx(ctx context.Context, f func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := f(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
