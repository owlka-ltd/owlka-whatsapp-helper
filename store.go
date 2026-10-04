// owlka-whatsapp-helper. Copyright (C) 2026 Owlka Ltd. GPL-3.0, see LICENSE.

package main

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the local message history: chats, messages, names and the record of
// what was sent. One SQLite file on this computer. Nothing in it is uploaded
// anywhere by this program.
type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS chats (
  account TEXT NOT NULL, jid TEXT NOT NULL, name TEXT NOT NULL DEFAULT '',
  is_group INTEGER NOT NULL DEFAULT 0, unread INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (account, jid));
CREATE TABLE IF NOT EXISTS messages (
  account TEXT NOT NULL, chat TEXT NOT NULL, id TEXT NOT NULL,
  sender TEXT NOT NULL DEFAULT '', sender_name TEXT NOT NULL DEFAULT '',
  from_me INTEGER NOT NULL DEFAULT 0, ts INTEGER NOT NULL,
  kind TEXT NOT NULL DEFAULT 'text', text TEXT NOT NULL DEFAULT '',
  quoted_id TEXT NOT NULL DEFAULT '', media_mime TEXT NOT NULL DEFAULT '',
  media_name TEXT NOT NULL DEFAULT '', raw BLOB,
  PRIMARY KEY (account, chat, id));
CREATE INDEX IF NOT EXISTS messages_by_time ON messages (account, chat, ts);
CREATE TABLE IF NOT EXISTS contacts (
  account TEXT NOT NULL, jid TEXT NOT NULL, name TEXT NOT NULL DEFAULT '',
  push_name TEXT NOT NULL DEFAULT '', PRIMARY KEY (account, jid));
CREATE TABLE IF NOT EXISTS sends (
  account TEXT NOT NULL, ts INTEGER NOT NULL, chat TEXT NOT NULL,
  text_hash TEXT NOT NULL DEFAULT '', new_number INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS sends_by_time ON sends (account, ts);
`

func sqliteDSN(path string) string {
	return "file:" + path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)"
}

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create message store: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

type Chat struct {
	JID         string `json:"chat"`
	Name        string `json:"name"`
	IsGroup     bool   `json:"is_group"`
	Unread      int    `json:"unread"`
	LastTime    string `json:"last_time,omitempty"`
	LastPreview string `json:"last_message,omitempty"`
}

type Message struct {
	Chat       string `json:"chat"`
	ID         string `json:"id"`
	Sender     string `json:"sender"`
	SenderName string `json:"sender_name,omitempty"`
	FromMe     bool   `json:"from_me"`
	Time       string `json:"time"`
	Kind       string `json:"kind"`
	Text       string `json:"text"`
	QuotedID   string `json:"reply_to,omitempty"`
	MediaMime  string `json:"media_type,omitempty"`
	MediaName  string `json:"file_name,omitempty"`
	ChatName   string `json:"chat_name,omitempty"`

	ts  time.Time
	raw []byte
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// EnsureChat makes sure a chat row exists. A non-empty name replaces the
// stored one; an empty name never erases it.
func (s *Store) EnsureChat(account, jid, name string, isGroup bool) error {
	_, err := s.db.Exec(`INSERT INTO chats (account, jid, name, is_group) VALUES (?,?,?,?)
		ON CONFLICT (account, jid) DO UPDATE SET
		  name = CASE WHEN excluded.name != '' THEN excluded.name ELSE chats.name END,
		  is_group = excluded.is_group`, account, jid, name, isGroup)
	return err
}

func (s *Store) SetUnread(account, jid string, n int) error {
	_, err := s.db.Exec(`UPDATE chats SET unread = ? WHERE account = ? AND jid = ?`, n, account, jid)
	return err
}

func (s *Store) AddUnread(account, jid string) error {
	_, err := s.db.Exec(`UPDATE chats SET unread = unread + 1 WHERE account = ? AND jid = ?`, account, jid)
	return err
}

// PutMessage stores one message. Returns false when it was already stored, so
// a message delivered twice is counted once.
func (s *Store) PutMessage(account string, m Message) (bool, error) {
	res, err := s.db.Exec(`INSERT OR IGNORE INTO messages
		(account, chat, id, sender, sender_name, from_me, ts, kind, text, quoted_id, media_mime, media_name, raw)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		account, m.Chat, m.ID, m.Sender, m.SenderName, m.FromMe, m.ts.Unix(), m.Kind, m.Text,
		m.QuotedID, m.MediaMime, m.MediaName, m.raw)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ReplaceText changes a stored message's text (an edit or a deletion).
func (s *Store) ReplaceText(account, chat, id, kind, text string) error {
	_, err := s.db.Exec(`UPDATE messages SET kind = ?, text = ?, raw = NULL, media_mime = '', media_name = ''
		WHERE account = ? AND chat = ? AND id = ?`, kind, text, account, chat, id)
	return err
}

func (s *Store) PutContact(account, jid, name, pushName string) error {
	_, err := s.db.Exec(`INSERT INTO contacts (account, jid, name, push_name) VALUES (?,?,?,?)
		ON CONFLICT (account, jid) DO UPDATE SET
		  name = CASE WHEN excluded.name != '' THEN excluded.name ELSE contacts.name END,
		  push_name = CASE WHEN excluded.push_name != '' THEN excluded.push_name ELSE contacts.push_name END`,
		account, jid, name, pushName)
	return err
}

// displayName is the best name known for a person: the address-book name, then
// the name they chose for themselves, then their number.
const displayNameSQL = `COALESCE(NULLIF(c.name,''), NULLIF(k.name,''), NULLIF(k.push_name,''), '')`

func (s *Store) Chats(account, query string, unreadOnly bool, limit int) ([]Chat, error) {
	q := `SELECT c.jid, ` + displayNameSQL + `, c.is_group, c.unread,
		COALESCE((SELECT MAX(ts) FROM messages m WHERE m.account = c.account AND m.chat = c.jid), 0) AS last_ts,
		COALESCE((SELECT text FROM messages m WHERE m.account = c.account AND m.chat = c.jid ORDER BY ts DESC LIMIT 1), '')
		FROM chats c LEFT JOIN contacts k ON k.account = c.account AND k.jid = c.jid
		WHERE c.account = ?`
	args := []any{account}
	if unreadOnly {
		q += ` AND c.unread > 0`
	}
	if query != "" {
		q += ` AND (` + displayNameSQL + ` LIKE ? ESCAPE '\' OR c.jid LIKE ? ESCAPE '\')`
		like := "%" + escapeLike(query) + "%"
		args = append(args, like, like)
	}
	q += ` ORDER BY last_ts DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Chat
	for rows.Next() {
		var c Chat
		var last int64
		if err := rows.Scan(&c.JID, &c.Name, &c.IsGroup, &c.Unread, &last, &c.LastPreview); err != nil {
			return nil, err
		}
		if last > 0 {
			c.LastTime = stamp(time.Unix(last, 0))
		}
		if len(c.LastPreview) > 160 {
			c.LastPreview = c.LastPreview[:160]
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

const messageCols = `m.chat, m.id, m.sender, m.sender_name, m.from_me, m.ts, m.kind, m.text, m.quoted_id, m.media_mime, m.media_name`

func scanMessages(rows *sql.Rows, withChatName bool) ([]Message, error) {
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		var ts int64
		dest := []any{&m.Chat, &m.ID, &m.Sender, &m.SenderName, &m.FromMe, &ts, &m.Kind, &m.Text,
			&m.QuotedID, &m.MediaMime, &m.MediaName}
		if withChatName {
			dest = append(dest, &m.ChatName)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		m.ts = time.Unix(ts, 0)
		m.Time = stamp(m.ts)
		out = append(out, m)
	}
	return out, rows.Err()
}

// Messages returns a chat's most recent messages, oldest first.
func (s *Store) Messages(account, chat string, limit int, before time.Time) ([]Message, error) {
	q := `SELECT ` + messageCols + ` FROM messages m WHERE m.account = ? AND m.chat = ?`
	args := []any{account, chat}
	if !before.IsZero() {
		q += ` AND m.ts < ?`
		args = append(args, before.Unix())
	}
	q += ` ORDER BY m.ts DESC, m.rowid DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	out, err := scanMessages(rows, false)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, err
}

func (s *Store) Search(account, query, chat string, limit int) ([]Message, error) {
	q := `SELECT ` + messageCols + `, ` + displayNameSQL + ` FROM messages m
		LEFT JOIN chats c ON c.account = m.account AND c.jid = m.chat
		LEFT JOIN contacts k ON k.account = m.account AND k.jid = m.chat
		WHERE m.account = ? AND m.text LIKE ? ESCAPE '\'`
	args := []any{account, "%" + escapeLike(query) + "%"}
	if chat != "" {
		q += ` AND m.chat = ?`
		args = append(args, chat)
	}
	q += ` ORDER BY m.ts DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	return scanMessages(rows, true)
}

// Message returns one stored message with its raw form (needed to fetch media).
func (s *Store) Message(account, chat, id string) (*Message, error) {
	row := s.db.QueryRow(`SELECT `+messageCols+`, m.raw FROM messages m
		WHERE m.account = ? AND m.chat = ? AND m.id = ?`, account, chat, id)
	var m Message
	var ts int64
	err := row.Scan(&m.Chat, &m.ID, &m.Sender, &m.SenderName, &m.FromMe, &ts, &m.Kind, &m.Text,
		&m.QuotedID, &m.MediaMime, &m.MediaName, &m.raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m.ts = time.Unix(ts, 0)
	m.Time = stamp(m.ts)
	return &m, nil
}

// LastIncoming returns the newest messages in a chat that were not sent by
// this account: the ones a "mark read" applies to.
func (s *Store) LastIncoming(account, chat string, limit int) ([]Message, error) {
	rows, err := s.db.Query(`SELECT `+messageCols+` FROM messages m
		WHERE m.account = ? AND m.chat = ? AND m.from_me = 0 ORDER BY m.ts DESC LIMIT ?`, account, chat, limit)
	if err != nil {
		return nil, err
	}
	return scanMessages(rows, false)
}

type Contact struct {
	JID      string `json:"id"`
	Number   string `json:"number,omitempty"`
	Name     string `json:"name,omitempty"`
	PushName string `json:"profile_name,omitempty"`
	HasChat  bool   `json:"has_chat"`
	IsGroup  bool   `json:"is_group"`
}

// People finds chats and contacts by name, number or id. exact means the name
// must match in full (used to choose a recipient); otherwise it is a search.
func (s *Store) People(account, query string, exact bool, limit int) ([]Contact, error) {
	op, arg := `LIKE ? ESCAPE '\'`, "%"+escapeLike(query)+"%"
	if exact {
		op, arg = `= ? COLLATE NOCASE`, strings.TrimSpace(query)
	}
	q := fmt.Sprintf(`
		SELECT jid, MAX(name), MAX(push_name), MAX(has_chat), MAX(is_group) FROM (
		  SELECT c.jid AS jid, c.name AS name, '' AS push_name, 1 AS has_chat, c.is_group AS is_group
		    FROM chats c WHERE c.account = ?1
		  UNION ALL
		  SELECT k.jid, k.name, k.push_name, 0, 0 FROM contacts k WHERE k.account = ?1
		) GROUP BY jid
		HAVING MAX(name) %[1]s OR MAX(push_name) %[1]s OR jid %[1]s
		ORDER BY MAX(has_chat) DESC, MAX(name) LIMIT ?3`, strings.Replace(op, "?", "?2", 1))
	rows, err := s.db.Query(q, account, arg, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Contact
	for rows.Next() {
		var c Contact
		if err := rows.Scan(&c.JID, &c.Name, &c.PushName, &c.HasChat, &c.IsGroup); err != nil {
			return nil, err
		}
		if user, server, ok := strings.Cut(c.JID, "@"); ok && server == "s.whatsapp.net" {
			c.Number = "+" + user
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// HasChat reports whether this account already has a chat with jid.
func (s *Store) HasChat(account, jid string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM chats WHERE account = ? AND jid = ?`, account, jid).Scan(&n)
	return n > 0, err
}

func (s *Store) RecordSend(account string, r SendRecord) error {
	_, err := s.db.Exec(`INSERT INTO sends (account, ts, chat, text_hash, new_number) VALUES (?,?,?,?,?)`,
		account, r.At.Unix(), r.Chat, r.TextHash, r.NewNumber)
	return err
}

// RecentSends returns the last day of sends, and drops older rows.
func (s *Store) RecentSends(account string, now time.Time) ([]SendRecord, error) {
	cutoff := now.Add(-24 * time.Hour).Unix()
	if _, err := s.db.Exec(`DELETE FROM sends WHERE ts < ?`, cutoff); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT ts, chat, text_hash, new_number FROM sends WHERE account = ? ORDER BY ts`, account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SendRecord
	for rows.Next() {
		var r SendRecord
		var ts int64
		if err := rows.Scan(&ts, &r.Chat, &r.TextHash, &r.NewNumber); err != nil {
			return nil, err
		}
		r.At = time.Unix(ts, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteAccount removes everything stored for one linked number.
func (s *Store) DeleteAccount(account string) error {
	for _, t := range []string{"messages", "chats", "contacts", "sends"} {
		if _, err := s.db.Exec(`DELETE FROM `+t+` WHERE account = ?`, account); err != nil {
			return err
		}
	}
	return nil
}
