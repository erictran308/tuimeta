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
// are private too.
func openStore(ctx context.Context, path string) (*waStore, error) {
	if strings.ContainsRune(path, '?') {
		return nil, errors.New("store path contains '?'")
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	_ = f.Chmod(0o600)
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
	`)
	return err
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

// deleteChat forgets a chat and its messages.
func (s *waStore) deleteChat(ctx context.Context, jid string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM tuimeta_message WHERE chat=?`, jid); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM tuimeta_chat WHERE jid=?`, jid)
		return err
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
		if _, err := tx.ExecContext(ctx, `UPDATE OR IGNORE tuimeta_chat SET jid=? WHERE jid=?`, to, from); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM tuimeta_chat WHERE jid=?`, from)
		return err
	})
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

// putMessages keeps messages of a chat, replacing ones with the same id,
// then trims the chat to MaxStoredPerChat.
func (s *waStore) putMessages(ctx context.Context, chat string, ms ...*message) error {
	if len(ms) == 0 {
		return nil
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, m := range ms {
			data, err := json.Marshal(m)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO tuimeta_message (chat, id, ts, expires, data) VALUES (?, ?, ?, ?, ?)
				ON CONFLICT (chat, id) DO UPDATE SET ts=excluded.ts, expires=excluded.expires, data=excluded.data`,
				chat, m.ID, m.MS, m.Expires, string(data)); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `
			DELETE FROM tuimeta_message WHERE chat=? AND rowid NOT IN (
				SELECT rowid FROM tuimeta_message WHERE chat=? ORDER BY ts DESC LIMIT ?
			)`, chat, chat, MaxStoredPerChat)
		return err
	})
}

func (s *waStore) deleteMessage(ctx context.Context, chat, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM tuimeta_message WHERE chat=? AND id=?`, chat, id)
	return err
}

// expiry is a kept message whose time ran out.
type expiry struct{ chat, id string }

// expired lists messages that disappear by ms.
func (s *waStore) expired(ctx context.Context, ms int64) ([]expiry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT chat, id FROM tuimeta_message WHERE expires > 0 AND expires <= ?`, ms)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []expiry
	for rows.Next() {
		var e expiry
		if err := rows.Scan(&e.chat, &e.id); err != nil {
			return nil, err
		}
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
// startup, so what's read back stays bounded whatever an older run kept.
func (s *waStore) prune(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM tuimeta_message WHERE rowid IN (
			SELECT rowid FROM (
				SELECT rowid, ROW_NUMBER() OVER (PARTITION BY chat ORDER BY ts DESC) AS n FROM tuimeta_message
			) WHERE n > ?
		)`, MaxStoredPerChat)
	return err
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
