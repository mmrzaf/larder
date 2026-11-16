// Package index implements the embedded SQLite catalog.
package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	// ErrNotFound means the requested row does not exist.
	ErrNotFound = errors.New("not found")
	// ErrExists means a row with the same primary key already exists.
	ErrExists = errors.New("already exists")
	// ErrSchemaTooNew means the database was written by a newer binary.
	ErrSchemaTooNew = errors.New("database schema is newer than this binary")
)

// DB is the embedded index database.
type DB struct {
	sql  *sql.DB
	path string
}

// Open opens the database, applies migrations and verifies the schema version.
func Open(path string) (*DB, error) {
	if path == "" {
		return nil, fmt.Errorf("index: database path is required")
	}

	dsn := path
	if path != ":memory:" {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolve index path: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
			return nil, fmt.Errorf("create index directory: %w", err)
		}
		u := url.URL{Scheme: "file", Path: abs}
		q := u.Query()
		q.Add("_pragma", "busy_timeout(5000)")
		q.Add("_pragma", "foreign_keys(1)")
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "synchronous(FULL)")
		q.Set("_txlock", "immediate")
		u.RawQuery = q.Encode()
		dsn = u.String()
	} else {
		dsn = "file::memory:?cache=shared&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	}

	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open index: %w", err)
	}
	database.SetMaxOpenConns(8)
	database.SetMaxIdleConns(8)

	db := &DB{sql: database, path: path}
	if err := database.Ping(); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("ping index: %w", err)
	}
	if err := db.migrate(); err != nil {
		_ = database.Close()
		return nil, err
	}
	if path != ":memory:" {
		if err := secureIndexFiles(path); err != nil {
			_ = database.Close()
			return nil, err
		}
	}
	return db, nil
}

// OpenReadOnly opens an existing database without migrations or writes. It is
// used by reporting commands while a server holds the data directory lock.
func OpenReadOnly(path string) (*DB, error) {
	if path == "" {
		return nil, fmt.Errorf("index: database path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve index path: %w", err)
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, fmt.Errorf("open index: %w", err)
	}
	u := url.URL{Scheme: "file", Path: abs}
	q := u.Query()
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	u.RawQuery = q.Encode()

	database, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("open index: %w", err)
	}
	database.SetMaxOpenConns(4)
	database.SetMaxIdleConns(4)
	db := &DB{sql: database, path: path}
	if err := database.Ping(); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("ping index: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.SchemaVersion(ctx); err != nil {
		_ = database.Close()
		return nil, err
	}
	return db, nil
}

func secureIndexFiles(path string) error {
	for _, file := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(file, 0o640); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("set index file mode %s: %w", file, err)
		}
	}
	return nil
}

// Close closes the database.
func (db *DB) Close() error {
	if db == nil || db.sql == nil {
		return nil
	}
	err := db.sql.Close()
	db.sql = nil
	return err
}

// SchemaVersion returns the applied schema version.
func (db *DB) SchemaVersion(ctx context.Context) (int, error) {
	var value string
	if err := db.sql.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&value); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	version, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("parse schema version %q: %w", value, err)
	}
	return version, nil
}

func (db *DB) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// Ping verifies that the database accepts a connection.
func (db *DB) Ping(ctx context.Context) error {
	if err := db.sql.PingContext(ctx); err != nil {
		return fmt.Errorf("ping index: %w", err)
	}
	return nil
}

// QuickCheck runs SQLite's quick integrity check.
func (db *DB) QuickCheck(ctx context.Context) error {
	rows, err := db.sql.QueryContext(ctx, `PRAGMA quick_check`)
	if err != nil {
		return fmt.Errorf("quick check: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var problems []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return fmt.Errorf("quick check scan: %w", err)
		}
		if line != "ok" {
			problems = append(problems, line)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("quick check: %w", err)
	}
	if len(problems) > 0 {
		return fmt.Errorf("index integrity check failed: %s", strings.Join(problems, "; "))
	}
	return nil
}

// Checkpoint folds the write-ahead log into the main database file.
func (db *DB) Checkpoint(ctx context.Context) error {
	if _, err := db.sql.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("checkpoint index: %w", err)
	}
	return nil
}
