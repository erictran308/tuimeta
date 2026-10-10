// SPDX-License-Identifier: AGPL-3.0-or-later

package messenger

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	waTypes "go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/erictran308/tuimeta/helper/internal/fsutil"

	// A SQLite written in Go, so the helper still builds with CGO_ENABLED=0.
	// It registers the "sqlite" driver; mautrix's dbutil reads any driver
	// name starting with "sqlite" as the SQLite dialect.
	_ "modernc.org/sqlite"
)

// storeFile is the encrypted chats' database in the network's session
// folder: whatsmeow's device (keys, sessions, sender keys) and the encrypted
// messages that have arrived since this device was linked.
const storeFile = "e2ee.db"

// e2eeStore is the open database.
type e2eeStore struct {
	db        *sql.DB
	container *sqlstore.Container
	path      string
}

// openStore opens the database at path, made 0600 before SQLite ever opens
// it: SQLite gives its -wal and -shm files the database file's mode, so they
// are private too. It must be this user's own plain file: a link planted in
// its place is refused, not followed.
func openStore(ctx context.Context, path string) (*e2eeStore, error) {
	if strings.ContainsRune(path, '?') {
		return nil, errors.New("store path contains '?'")
	}
	f, err := fsutil.OpenPrivate(path, os.O_RDWR|os.O_CREATE)
	if err != nil {
		return nil, err
	}
	f.Close()
	// Foreign keys are on, as whatsmeow requires; secure_delete overwrites
	// deleted rows (old keys, read messages) instead of leaving them in free
	// pages.
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
	s := &e2eeStore{db: db, container: container, path: path}
	if err := s.upgrade(ctx); err != nil {
		db.Close()
		return nil, err
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Chmod(path+suffix, 0o600)
	}
	return s, nil
}

// upgrade makes the helper's own tables, and adds the columns a store made
// by an older build lacks.
func (s *e2eeStore) upgrade(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS tuimeta_e2ee_message (
			chat    TEXT    NOT NULL,
			sender  TEXT    NOT NULL,
			id      TEXT    NOT NULL,
			ts      INTEGER NOT NULL,
			from_me INTEGER NOT NULL,
			app     BLOB    NOT NULL,
			PRIMARY KEY (chat, sender, id)
		);
		CREATE INDEX IF NOT EXISTS tuimeta_e2ee_message_ts ON tuimeta_e2ee_message (chat, ts);
	`); err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM pragma_table_info('tuimeta_e2ee_message')`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		have[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// What a row is (see rowMessage), the message an edit or a reaction
	// changes, and when a disappearing message goes (unix ms, 0: never).
	for _, col := range []struct{ name, def string }{
		{"kind", "TEXT NOT NULL DEFAULT ''"},
		{"target_sender", "TEXT NOT NULL DEFAULT ''"},
		{"target_id", "TEXT NOT NULL DEFAULT ''"},
		{"expires", "INTEGER NOT NULL DEFAULT 0"},
	} {
		if have[col.name] {
			continue
		}
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE tuimeta_e2ee_message ADD COLUMN `+col.name+` `+col.def); err != nil {
			return err
		}
	}
	_, err = s.db.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS tuimeta_e2ee_message_kind ON tuimeta_e2ee_message (chat, kind, ts);
		CREATE INDEX IF NOT EXISTS tuimeta_e2ee_message_target ON tuimeta_e2ee_message (chat, target_sender, target_id);
		CREATE INDEX IF NOT EXISTS tuimeta_e2ee_message_expires ON tuimeta_e2ee_message (expires);
	`)
	return err
}

func (s *e2eeStore) Close() error { return s.db.Close() }

// clear forgets every kept message (another account's, or none's).
func (s *e2eeStore) clear(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM tuimeta_e2ee_message`)
	return err
}

// dropDevices deletes every device in the store, before a new one is
// registered: the store only ever holds this login's.
func (s *e2eeStore) dropDevices(ctx context.Context) error {
	devs, err := s.container.GetAllDevices(ctx)
	if err != nil {
		return err
	}
	for _, d := range devs {
		if d.ID == nil {
			continue
		}
		if err := d.Delete(ctx); err != nil {
			return err
		}
	}
	return nil
}

// device is the stored device with jid, or a new one when jid is empty or
// gone.
func (s *e2eeStore) device(ctx context.Context, jid string) (dev *store.Device, isNew bool, err error) {
	if jid != "" {
		parsed, perr := waTypes.ParseJID(jid)
		if perr == nil {
			dev, err = s.container.GetDevice(ctx, parsed)
			if err != nil {
				return nil, false, err
			}
			if dev != nil {
				return dev, false, nil
			}
		}
	}
	return s.container.NewDevice(), true, nil
}

// What a kept row is.
const (
	rowMessage  = ""         // a message
	rowEdit     = "edit"     // the newest edit of a kept message
	rowReaction = "reaction" // someone's reaction to a kept message
	rowSetting  = "setting"  // the chat's disappearing-messages setting
)

// storedMessage is an encrypted message as kept: who sent it where, when,
// and its decrypted application payload, from which it is read again at the
// next start. An edit or a reaction is kept too, with the message it
// changes (TargetSender and TargetID are that message's Sender and ID), and
// goes with it.
type storedMessage struct {
	Chat, Sender, ID string
	TS               time.Time
	FromMe           bool
	App              []byte
	Kind             string
	TargetSender     string
	TargetID         string
	// Expires is when a disappearing message goes, in unix ms (0: never).
	Expires int64
}

// MaxStoredPerChat bounds how many encrypted messages are kept per chat.
// Edits and reactions don't count: what's kept of them is one per message
// (its newest edit) or per person and message (their reaction).
const MaxStoredPerChat = 3000

// put keeps m. An edit replaces its message's earlier edit, a reaction the
// same person's earlier reaction to it, and a setting the chat's earlier
// one, so what's kept of a message's changes stays bounded by the message.
func (s *e2eeStore) put(ctx context.Context, m storedMessage) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	switch m.Kind {
	case rowEdit:
		_, err = tx.ExecContext(ctx, `
			DELETE FROM tuimeta_e2ee_message WHERE chat=? AND kind=? AND target_sender=? AND target_id=? AND NOT (sender=? AND id=?)`,
			m.Chat, rowEdit, m.TargetSender, m.TargetID, m.Sender, m.ID)
	case rowReaction:
		_, err = tx.ExecContext(ctx, `
			DELETE FROM tuimeta_e2ee_message WHERE chat=? AND kind=? AND target_sender=? AND target_id=? AND sender=? AND id<>?`,
			m.Chat, rowReaction, m.TargetSender, m.TargetID, m.Sender, m.ID)
	case rowSetting:
		_, err = tx.ExecContext(ctx, `
			DELETE FROM tuimeta_e2ee_message WHERE chat=? AND kind=? AND NOT (sender=? AND id=?)`,
			m.Chat, rowSetting, m.Sender, m.ID)
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tuimeta_e2ee_message (chat, sender, id, ts, from_me, app, kind, target_sender, target_id, expires)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (chat, sender, id) DO UPDATE SET ts=excluded.ts, from_me=excluded.from_me, app=excluded.app,
			kind=excluded.kind, target_sender=excluded.target_sender, target_id=excluded.target_id, expires=excluded.expires`,
		m.Chat, m.Sender, m.ID, m.TS.UnixMilli(), m.FromMe, m.App, m.Kind, m.TargetSender, m.TargetID, m.Expires); err != nil {
		return err
	}
	return tx.Commit()
}

// update rewrites a kept row's payload, kind, target and expiry.
func (s *e2eeStore) update(ctx context.Context, m storedMessage) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE tuimeta_e2ee_message SET app=?, kind=?, target_sender=?, target_id=?, expires=? WHERE chat=? AND sender=? AND id=?`,
		m.App, m.Kind, m.TargetSender, m.TargetID, m.Expires, m.Chat, m.Sender, m.ID)
	return err
}

// drop deletes one row.
func (s *e2eeStore) drop(ctx context.Context, chat, sender, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM tuimeta_e2ee_message WHERE chat=? AND sender=? AND id=?`, chat, sender, id)
	return err
}

// remove forgets a message (unsent) with its edit and reactions, and empties
// the write-ahead log so the unsent text doesn't linger there.
func (s *e2eeStore) remove(ctx context.Context, chat, sender, id string) error {
	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM tuimeta_e2ee_message WHERE chat=? AND ((sender=? AND id=?) OR (kind IN (?, ?) AND target_sender=? AND target_id=?))`,
		chat, sender, id, rowEdit, rowReaction, sender, id); err != nil {
		return err
	}
	s.checkpoint(ctx)
	return nil
}

// setExpires records when a kept message disappears (unix ms).
func (s *e2eeStore) setExpires(ctx context.Context, chat, sender, id string, ms int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tuimeta_e2ee_message SET expires=? WHERE chat=? AND sender=? AND id=?`, ms, chat, sender, id)
	return err
}

// deleteExpired deletes the messages that disappear by ms, with their edits
// and reactions, and empties the write-ahead log if any went.
func (s *e2eeStore) deleteExpired(ctx context.Context, ms int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM tuimeta_e2ee_message WHERE expires > 0 AND expires <= ?`, ms)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM tuimeta_e2ee_message WHERE `+orphans); err != nil {
		return err
	}
	s.checkpoint(ctx)
	return nil
}

// forgetChat deletes everything kept of the chat whose JID's user is user,
// on any server a chat's JID can be on, and empties the write-ahead log so
// it doesn't linger there.
func (s *e2eeStore) forgetChat(ctx context.Context, user string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM tuimeta_e2ee_message WHERE chat IN (?, ?, ?)`,
		user+"@"+waTypes.MessengerServer, user+"@"+waTypes.DefaultUserServer, user+"@"+waTypes.GroupServer); err != nil {
		return err
	}
	s.checkpoint(ctx)
	return nil
}

// checkpoint writes the write-ahead log into the database and empties it,
// so rows deleted (and overwritten there, secure_delete) aren't left in it.
func (s *e2eeStore) checkpoint(ctx context.Context) {
	_, _ = s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
}

// orphans matches the edits and reactions whose message isn't kept.
const orphans = `kind IN ('edit', 'reaction') AND NOT EXISTS (
	SELECT 1 FROM tuimeta_e2ee_message AS m WHERE m.chat = tuimeta_e2ee_message.chat AND m.kind = ''
		AND m.sender = tuimeta_e2ee_message.target_sender AND m.id = tuimeta_e2ee_message.target_id)`

// prune keeps the newest MaxStoredPerChat messages of each chat, and the
// edits and reactions of those only. It runs once at startup to bound a
// store grown over-cap by an older build.
func (s *e2eeStore) prune(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM tuimeta_e2ee_message WHERE rowid IN (
			SELECT rowid FROM (
				SELECT rowid, ROW_NUMBER() OVER (PARTITION BY chat ORDER BY ts DESC) AS n FROM tuimeta_e2ee_message WHERE kind = ''
			) WHERE n > ?
		)`, MaxStoredPerChat); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM tuimeta_e2ee_message WHERE `+orphans)
	return err
}

// pruneChat keeps the newest MaxStoredPerChat messages of one chat, and the
// edits and reactions of those only. It runs after each message is kept, so
// a chat's decrypted messages never pile up past the cap on disk (the store
// isn't encrypted at rest).
func (s *e2eeStore) pruneChat(ctx context.Context, chat string) error {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM tuimeta_e2ee_message WHERE chat=? AND kind='' AND rowid NOT IN (
			SELECT rowid FROM tuimeta_e2ee_message WHERE chat=? AND kind='' ORDER BY ts DESC LIMIT ?
		)`, chat, chat, MaxStoredPerChat)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM tuimeta_e2ee_message WHERE chat=? AND `+orphans, chat)
	return err
}

// all is every stored row, oldest first.
func (s *e2eeStore) all(ctx context.Context) ([]storedMessage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT chat, sender, id, ts, from_me, app, kind, target_sender, target_id, expires
		FROM tuimeta_e2ee_message ORDER BY ts, rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storedMessage
	for rows.Next() {
		var m storedMessage
		var ts int64
		if err := rows.Scan(&m.Chat, &m.Sender, &m.ID, &ts, &m.FromMe, &m.App, &m.Kind, &m.TargetSender, &m.TargetID, &m.Expires); err != nil {
			return nil, err
		}
		m.TS = time.UnixMilli(ts)
		out = append(out, m)
	}
	return out, rows.Err()
}
