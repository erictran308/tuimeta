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
// are private too.
func openStore(ctx context.Context, path string) (*e2eeStore, error) {
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

// upgrade makes the helper's own tables.
func (s *e2eeStore) upgrade(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
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

// storedMessage is an encrypted message as kept: who sent it where, when,
// and its decrypted application payload, from which it is read again at the
// next start.
type storedMessage struct {
	Chat, Sender, ID string
	TS               time.Time
	FromMe           bool
	App              []byte
}

// MaxStoredPerChat bounds how many encrypted messages are kept per chat.
const MaxStoredPerChat = 3000

func (s *e2eeStore) put(ctx context.Context, m storedMessage) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO tuimeta_e2ee_message (chat, sender, id, ts, from_me, app) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (chat, sender, id) DO UPDATE SET ts=excluded.ts, from_me=excluded.from_me, app=excluded.app`,
		m.Chat, m.Sender, m.ID, m.TS.UnixMilli(), m.FromMe, m.App)
	return err
}

// remove forgets a message (unsent).
func (s *e2eeStore) remove(ctx context.Context, chat, sender, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM tuimeta_e2ee_message WHERE chat=? AND sender=? AND id=?`, chat, sender, id)
	return err
}

// prune keeps the newest MaxStoredPerChat messages of each chat. It runs
// once at startup to bound a store grown over-cap by an older build.
func (s *e2eeStore) prune(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM tuimeta_e2ee_message WHERE rowid IN (
			SELECT rowid FROM (
				SELECT rowid, ROW_NUMBER() OVER (PARTITION BY chat ORDER BY ts DESC) AS n FROM tuimeta_e2ee_message
			) WHERE n > ?
		)`, MaxStoredPerChat)
	return err
}

// pruneChat keeps the newest MaxStoredPerChat messages of one chat. It runs
// after each insert, so a chat's decrypted messages never pile up past the
// cap on disk (the store isn't encrypted at rest).
func (s *e2eeStore) pruneChat(ctx context.Context, chat string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM tuimeta_e2ee_message WHERE chat=? AND rowid NOT IN (
			SELECT rowid FROM tuimeta_e2ee_message WHERE chat=? ORDER BY ts DESC LIMIT ?
		)`, chat, chat, MaxStoredPerChat)
	return err
}

// all is every stored message, oldest first.
func (s *e2eeStore) all(ctx context.Context) ([]storedMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT chat, sender, id, ts, from_me, app FROM tuimeta_e2ee_message ORDER BY ts, rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storedMessage
	for rows.Next() {
		var m storedMessage
		var ts int64
		if err := rows.Scan(&m.Chat, &m.Sender, &m.ID, &ts, &m.FromMe, &m.App); err != nil {
			return nil, err
		}
		m.TS = time.UnixMilli(ts)
		out = append(out, m)
	}
	return out, rows.Err()
}
