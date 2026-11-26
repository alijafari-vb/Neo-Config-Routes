package xdns

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type repository struct {
	db  *sql.DB
	key cipher.AEAD
}

func openRepository(dir string) (*repository, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "token.key")
	key, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if _, dbErr := os.Stat(filepath.Join(dir, "routes.db")); dbErr == nil {
			return nil, errors.New("Config Routes encryption key is missing")
		} else if !errors.Is(dbErr, os.ErrNotExist) {
			return nil, dbErr
		}
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		file, openErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if openErr != nil {
			return nil, openErr
		}
		_, writeErr := file.Write(key)
		err = errors.Join(writeErr, file.Sync(), file.Close())
	}
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	path = filepath.Join(dir, "routes.db")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`
		PRAGMA busy_timeout=5000;
		PRAGMA journal_mode=WAL;
		CREATE TABLE IF NOT EXISTS xdns_settings (
			id INTEGER PRIMARY KEY CHECK(id=1), token BLOB, dry_run INTEGER NOT NULL
		);
		INSERT OR IGNORE INTO xdns_settings(id,dry_run) VALUES(1,1);
		CREATE TABLE IF NOT EXISTS xdns_groups (
			id INTEGER PRIMARY KEY AUTOINCREMENT, revision INTEGER NOT NULL DEFAULT 1,
			zone TEXT NOT NULL, record TEXT NOT NULL, body TEXT NOT NULL, UNIQUE(zone,record)
		);
		CREATE TABLE IF NOT EXISTS xdns_history (
			id INTEGER PRIMARY KEY AUTOINCREMENT, group_id INTEGER NOT NULL, record_key TEXT NOT NULL,
			at INTEGER NOT NULL, reason TEXT NOT NULL, status TEXT NOT NULL, body TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS xdns_history_group ON xdns_history(group_id,id DESC);
		CREATE INDEX IF NOT EXISTS xdns_history_switch ON xdns_history(group_id,record_key,status, at DESC);
		CREATE TABLE IF NOT EXISTS xdns_incidents (
			group_id INTEGER NOT NULL, kind TEXT NOT NULL, detail TEXT NOT NULL, PRIMARY KEY(group_id,kind)
		);
		CREATE TABLE IF NOT EXISTS xdns_cloud_health (
			group_id INTEGER NOT NULL, record_key TEXT NOT NULL, operation TEXT NOT NULL, message TEXT NOT NULL,
			failed_at INTEGER NOT NULL, retry_at INTEGER NOT NULL, verified_at INTEGER NOT NULL,
			PRIMARY KEY(group_id, record_key)
		);
		CREATE TABLE IF NOT EXISTS xdns_route_samples (
			group_id INTEGER NOT NULL, record_key TEXT NOT NULL, at INTEGER NOT NULL, target TEXT NOT NULL,
			PRIMARY KEY(group_id, record_key, at)
		) WITHOUT ROWID;
		CREATE TABLE IF NOT EXISTS xdns_delivery (
			event_id INTEGER NOT NULL, channel_id INTEGER NOT NULL,
			next INTEGER NOT NULL DEFAULT 0, attempts INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY(event_id, channel_id)
		);
	`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &repository{db: db, key: aead}, nil
}

func (s *repository) settings(ctx context.Context) (string, bool, error) {
	var secret []byte
	var dry bool
	if err := s.db.QueryRowContext(ctx, "SELECT token,dry_run FROM xdns_settings WHERE id=1").Scan(&secret, &dry); err != nil {
		return "", false, err
	}
	if len(secret) == 0 {
		return "", dry, nil
	}
	size := s.key.NonceSize()
	if len(secret) < size {
		return "", dry, errors.New("invalid encrypted Cloudflare token")
	}
	out, err := s.key.Open(nil, secret[:size], secret[size:], []byte("xdns/cloudflare/v1"))
	if err != nil {
		return "", dry, errors.New("cannot decrypt the Cloudflare token")
	}
	return string(out), dry, nil
}

func (s *repository) setSettings(ctx context.Context, token *string, dry bool) error {
	if token == nil {
		_, err := s.db.ExecContext(ctx, "UPDATE xdns_settings SET dry_run=? WHERE id=1", dry)
		return err
	}
	var secret []byte
	if *token != "" {
		nonce := make([]byte, s.key.NonceSize())
		if _, err := rand.Read(nonce); err != nil {
			return err
		}
		secret = s.key.Seal(nonce, nonce, []byte(*token), []byte("xdns/cloudflare/v1"))
	}
	_, err := s.db.ExecContext(ctx, "UPDATE xdns_settings SET token=?,dry_run=? WHERE id=1", secret, dry)
	return err
}

type scanner interface{ Scan(...any) error }

func readGroup(row scanner) (Group, error) {
	var g Group
	var id, rev int64
	var body []byte
	if err := row.Scan(&id, &rev, &body); err != nil {
		return g, err
	}
	if err := json.Unmarshal(body, &g); err != nil {
		return g, err
	}
	g.ID, g.Revision = id, rev
	return g, nil
}

func (s *repository) groups(ctx context.Context) ([]Group, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id,revision,body FROM xdns_groups ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := []Group{}
	for rows.Next() {
		group, err := readGroup(rows)
		if err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

func (s *repository) group(ctx context.Context, id int64) (Group, error) {
	return readGroup(s.db.QueryRowContext(ctx, "SELECT id,revision,body FROM xdns_groups WHERE id=?", id))
}

var errConflict = errors.New("the group has changed; reload and try again")
var errDuplicate = errors.New("this public record is already managed by another group")

func (s *repository) save(ctx context.Context, g Group) (Group, error) {
	body, err := json.Marshal(g)
	if err != nil {
		return Group{}, err
	}
	if g.ID == 0 {
		res, err := s.db.ExecContext(ctx, `INSERT INTO xdns_groups(zone,record,body) VALUES(?,?,?) ON CONFLICT(zone,record) DO NOTHING`, g.Zone, g.Record(), body)
		if err != nil {
			return Group{}, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return Group{}, errDuplicate
		}
		g.ID, err = res.LastInsertId()
		g.Revision = 1
		return g, err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE xdns_groups SET zone=?,record=?,body=?,revision=revision+1
		WHERE id=? AND revision=? AND NOT EXISTS (SELECT 1 FROM xdns_groups WHERE zone=? AND record=? AND id<>?)`,
		g.Zone, g.Record(), body, g.ID, g.Revision, g.Zone, g.Record(), g.ID)
	if err != nil {
		return Group{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		old, queryErr := s.group(ctx, g.ID)
		if queryErr == nil && old.Revision == g.Revision {
			return Group{}, errDuplicate
		}
		return Group{}, errConflict
	}
	g.Revision++
	return g, nil
}

type Event struct {
	ID          int64  `json:"id"`
	GroupID     int64  `json:"groupId"`
	GroupName   string `json:"groupName"`
	Record      string `json:"record"`
	Kind        string `json:"kind"`
	At          int64  `json:"at"`
	Reason      string `json:"reason"`
	Status      string `json:"status"`
	Before      string `json:"before"`
	After       string `json:"after"`
	FromMonitor string `json:"fromMonitor"`
	ToMonitor   string `json:"toMonitor"`
	Message     string `json:"message"`
	RecordID    string `json:"-"`
}

func recordKey(g Group) string { return strings.Join([]string{g.Zone, g.Record(), g.Kind}, "/") }

func eventBody(e Event) ([]byte, error) {
	return json.Marshal(struct {
		Event
		RecordID string
	}{e, e.RecordID})
}

func readEvent(row scanner) (Event, error) {
	var v struct {
		Event
		RecordID string
	}
	var id, at int64
	var reason, status string
	var body []byte
	if err := row.Scan(&id, &at, &reason, &status, &body); err != nil {
		return Event{}, err
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return Event{}, err
	}
	v.ID, v.At, v.Reason, v.Status, v.Event.RecordID = id, at, reason, status, v.RecordID
	return v.Event, nil
}

func (s *repository) addEvent(ctx context.Context, g Group, e Event, incident string, channels []int64) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if incident != "" {
		result, err := tx.ExecContext(ctx, `INSERT INTO xdns_incidents VALUES(?,?,?)
			ON CONFLICT(group_id,kind) DO UPDATE SET detail=excluded.detail WHERE detail<>excluded.detail`, g.ID, incident, e.Message)
		if err != nil {
			return 0, err
		}
		if n, _ := result.RowsAffected(); n == 0 {
			return 0, nil
		}
	}
	body, err := eventBody(e)
	if err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO xdns_history(group_id,record_key,at,reason,status,body) VALUES(?,?,?,?,?,?)`,
		g.ID, recordKey(g), e.At, e.Reason, e.Status, body)
	if err != nil {
		return 0, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := enqueue(ctx, tx, id, channels); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func (s *repository) clearIncident(ctx context.Context, id int64, kind string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM xdns_incidents WHERE group_id=? AND kind=?`, id, kind)
	return err
}

func enqueue(ctx context.Context, tx *sql.Tx, event int64, channels []int64) error {
	for _, channel := range channels {
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO xdns_delivery(event_id,channel_id) VALUES(?,?)", event, channel); err != nil {
			return err
		}
	}
	return nil
}

func (s *repository) finish(ctx context.Context, e Event, channels []int64) error {
	return s.finishIncident(ctx, e, channels, "")
}

func (s *repository) finishIncident(ctx context.Context, e Event, channels []int64, incident string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	body, err := eventBody(e)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, "UPDATE xdns_history SET status=?,body=?,at=? WHERE id=? AND status='pending'", e.Status, body, e.At, e.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("switch journal entry is no longer pending")
	}
	if incident != "" && e.Status == "failed" {
		result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO xdns_incidents VALUES(?,?,?)`, e.GroupID, incident, e.Message)
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n == 0 {
			channels = nil
		}
	}
	if err := enqueue(ctx, tx, e.ID, channels); err != nil {
		return err
	}
	return tx.Commit()
}

const eventColumns = "id,at,reason,status,body"

func (s *repository) pending(ctx context.Context, g Group) (Event, error) {
	return readEvent(s.db.QueryRowContext(ctx, `SELECT `+eventColumns+` FROM xdns_history WHERE group_id=? AND status='pending' ORDER BY id LIMIT 1`, g.ID))
}

func (s *repository) history(ctx context.Context, id, before int64, limit int) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+eventColumns+` FROM xdns_history
		WHERE group_id=? AND (?=0 OR id<?) ORDER BY id DESC LIMIT ?`, id, before, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []Event{}
	for rows.Next() {
		event, err := readEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *repository) lastSwitch(ctx context.Context, g Group) (int64, error) {
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT coalesce(max(at),0) FROM xdns_history
		WHERE group_id=? AND record_key=? AND status='changed'`, g.ID, recordKey(g)).Scan(&at)
	return at, err
}

func (s *repository) remove(ctx context.Context, id, revision int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "DELETE FROM xdns_groups WHERE id=? AND revision=?", id, revision)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errConflict
	}
	for _, table := range []string{"xdns_incidents", "xdns_history", "xdns_cloud_health", "xdns_route_samples"} {
		if table == "xdns_history" {
			if _, err := tx.ExecContext(ctx, "DELETE FROM xdns_delivery WHERE event_id IN (SELECT id FROM xdns_history WHERE group_id=?)", id); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE group_id=?", table), id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type delivery struct {
	Event    Event
	Channel  int64
	Attempts int
}

func (s *repository) deliveries(ctx context.Context) ([]delivery, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT h.id,h.at,h.reason,h.status,h.body,d.channel_id,d.attempts
		FROM xdns_delivery d JOIN xdns_history h ON h.id=d.event_id WHERE d.next<=? ORDER BY d.event_id,d.channel_id LIMIT 8`, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var batch []delivery
	for rows.Next() {
		var next delivery
		event, err := readEvent(extraScanner{rows, &next})
		if err != nil {
			return nil, err
		}
		next.Event = event
		batch = append(batch, next)
	}
	return batch, rows.Err()
}

type extraScanner struct {
	scanner
	delivery *delivery
}

func (r extraScanner) Scan(dest ...any) error {
	return r.scanner.Scan(append(dest, &r.delivery.Channel, &r.delivery.Attempts)...)
}

func (s *repository) delivered(ctx context.Context, d delivery, retry bool) error {
	if !retry || d.Attempts >= 4 {
		_, err := s.db.ExecContext(ctx, "DELETE FROM xdns_delivery WHERE event_id=? AND channel_id=?", d.Event.ID, d.Channel)
		return err
	}
	_, err := s.db.ExecContext(ctx, "UPDATE xdns_delivery SET attempts=attempts+1,next=? WHERE event_id=? AND channel_id=?",
		time.Now().Add(time.Minute<<d.Attempts).Unix(), d.Event.ID, d.Channel)
	return err
}
