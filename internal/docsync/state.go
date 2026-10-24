// Package docsync is the engine behind `jiku docs sync`: it mirrors the documents a person may
// read into a local folder and sends local edits back.
//
// It is internal on purpose. What is promised is the command, its flags and the on-disk layout
// (`.jiku-docs/state.db`, `.jiku-docs/sin-acceso/`, `<name>.remoto-v<n>.md`); the Go API is
// free to change.
package docsync

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	// A pure-Go SQLite: the release binaries are built with CGO_ENABLED=0 for five platforms,
	// and a cgo driver would end that.
	_ "modernc.org/sqlite"
)

// StateDir is the daemon's own folder inside the synced one. Everything under it is ignored by
// the sync itself.
const StateDir = ".jiku-docs"

// NoAccessDir is where files go when their document is no longer readable. They are moved, never
// deleted: the person may still need what they had.
const NoAccessDir = "sin-acceso"

// State is the daemon's memory: which document each file is, at which version, and how far the
// change feed has been read.
type State struct {
	db *sql.DB
}

// DocRecord is what the daemon knows about one synced document.
type DocRecord struct {
	ID string
	// Path is the module's path, `/infra/a.md`.
	Path     string
	FolderID string
	// Version and Checksum are of the remote content the local file was last made equal to:
	// the base of any local edit.
	Version  int
	Checksum string
	ReadOnly bool
	// Conflict is the module path of a `.remoto-v<n>.md` copy waiting for the person. While it
	// exists the local file is not uploaded; deleting it says "merged".
	Conflict string
}

// LogEntry is one line of the daemon's recent history, for `sync status`.
type LogEntry struct {
	At      time.Time
	Level   string
	Path    string
	Message string
}

const schema = `
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS docs (
  id        TEXT PRIMARY KEY,
  path      TEXT NOT NULL UNIQUE,
  folder_id TEXT NOT NULL DEFAULT '',
  version   INTEGER NOT NULL,
  checksum  TEXT NOT NULL,
  read_only INTEGER NOT NULL DEFAULT 0,
  conflict  TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS pending_new (path TEXT PRIMARY KEY, id TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS log (
  id      INTEGER PRIMARY KEY AUTOINCREMENT,
  at      TEXT NOT NULL,
  level   TEXT NOT NULL,
  path    TEXT NOT NULL DEFAULT '',
  message TEXT NOT NULL
);`

// logKeep is how many log lines the state keeps. It is for `sync status`, not an audit trail.
const logKeep = 500

// OpenState opens, or creates, the state database at path.
func OpenState(path string) (*State, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("docsync: opening %s: %w", path, err)
	}
	// One writer at a time is all SQLite offers anyway; a single connection keeps a status
	// reader in another process from ever seeing a half-applied change.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("docsync: preparing %s: %w", path, err)
	}
	return &State{db: db}, nil
}

// Close closes the database.
func (s *State) Close() error { return s.db.Close() }

// Meta keys.
const (
	MetaIdentity    = "identity"
	MetaLastSeq     = "lastSeq"
	MetaLastFull    = "lastFull"
	MetaServiceName = "serviceName"
)

// Meta reads a value; "" when absent.
func (s *State) Meta(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetMeta writes a value.
func (s *State) SetMeta(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO meta(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// LastSeq is how far the change feed has been read.
func (s *State) LastSeq() (int64, error) {
	v, err := s.Meta(MetaLastSeq)
	if err != nil || v == "" {
		return 0, err
	}
	return strconv.ParseInt(v, 10, 64)
}

// SetLastSeq records how far the change feed has been read.
func (s *State) SetLastSeq(seq int64) error {
	return s.SetMeta(MetaLastSeq, strconv.FormatInt(seq, 10))
}

// LastFull is when the last full reconciliation finished; zero if never.
func (s *State) LastFull() (time.Time, error) {
	v, err := s.Meta(MetaLastFull)
	if err != nil || v == "" {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339Nano, v)
}

// SetLastFull records a full reconciliation.
func (s *State) SetLastFull(t time.Time) error {
	return s.SetMeta(MetaLastFull, t.UTC().Format(time.RFC3339Nano))
}

func scanDoc(row interface{ Scan(...any) error }) (*DocRecord, error) {
	var d DocRecord
	var ro int
	err := row.Scan(&d.ID, &d.Path, &d.FolderID, &d.Version, &d.Checksum, &ro, &d.Conflict)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d.ReadOnly = ro != 0
	return &d, nil
}

const docColumns = `id, path, folder_id, version, checksum, read_only, conflict`

// Doc returns a document's record, nil when unknown.
func (s *State) Doc(id string) (*DocRecord, error) {
	return scanDoc(s.db.QueryRow(`SELECT `+docColumns+` FROM docs WHERE id = ?`, id))
}

// DocByPath returns the record of the document at a module path, nil when unknown.
func (s *State) DocByPath(path string) (*DocRecord, error) {
	return scanDoc(s.db.QueryRow(`SELECT `+docColumns+` FROM docs WHERE path = ?`, path))
}

// Docs returns every record.
func (s *State) Docs() ([]DocRecord, error) {
	rows, err := s.db.Query(`SELECT ` + docColumns + ` FROM docs ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DocRecord
	for rows.Next() {
		d, err := scanDoc(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// PutDoc stores a record, replacing the one with its id — and any stale one still claiming its
// path, which a move in the module can leave behind.
func (s *State) PutDoc(d DocRecord) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM docs WHERE path = ? AND id <> ?`, d.Path, d.ID); err != nil {
		return err
	}
	ro := 0
	if d.ReadOnly {
		ro = 1
	}
	if _, err := tx.Exec(`INSERT INTO docs(`+docColumns+`) VALUES(?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET path = excluded.path, folder_id = excluded.folder_id,
		version = excluded.version, checksum = excluded.checksum, read_only = excluded.read_only,
		conflict = excluded.conflict`,
		d.ID, d.Path, d.FolderID, d.Version, d.Checksum, ro, d.Conflict); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteDoc forgets a document.
func (s *State) DeleteDoc(id string) error {
	_, err := s.db.Exec(`DELETE FROM docs WHERE id = ?`, id)
	return err
}

// PendingID returns the id reserved for a new local file, reserving newID() if there is none.
// The id outlives a failed attempt, so the retry is the same creation and not a second document.
func (s *State) PendingID(path string, newID func() string) (string, error) {
	var id string
	err := s.db.QueryRow(`SELECT id FROM pending_new WHERE path = ?`, path).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	id = newID()
	_, err = s.db.Exec(`INSERT INTO pending_new(path, id) VALUES(?, ?)`, path, id)
	return id, err
}

// ClearPending forgets the id reserved for a path.
func (s *State) ClearPending(path string) error {
	_, err := s.db.Exec(`DELETE FROM pending_new WHERE path = ?`, path)
	return err
}

// Log records a line for `sync status`, trimming the oldest beyond logKeep.
func (s *State) Log(at time.Time, level, path, message string) error {
	if _, err := s.db.Exec(`INSERT INTO log(at, level, path, message) VALUES(?, ?, ?, ?)`,
		at.UTC().Format(time.RFC3339Nano), level, path, message); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM log WHERE id <= (SELECT MAX(id) FROM log) - ?`, logKeep)
	return err
}

// RecentLog returns the last n lines, oldest first.
func (s *State) RecentLog(n int) ([]LogEntry, error) {
	rows, err := s.db.Query(`SELECT at, level, path, message FROM
		(SELECT id, at, level, path, message FROM log ORDER BY id DESC LIMIT ?) ORDER BY id`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LogEntry
	for rows.Next() {
		var e LogEntry
		var at string
		if err := rows.Scan(&at, &e.Level, &e.Path, &e.Message); err != nil {
			return nil, err
		}
		e.At, _ = time.Parse(time.RFC3339Nano, at)
		out = append(out, e)
	}
	return out, rows.Err()
}
