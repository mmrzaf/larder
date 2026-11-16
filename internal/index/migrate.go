package index

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

type migration struct {
	version    int
	statements []string
}

var migrations = []migration{
	{
		version: 1,
		statements: []string{
			`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
			`CREATE TABLE blobs (
				sha256        TEXT PRIMARY KEY,
				raw_size      INTEGER NOT NULL,
				stored_size   INTEGER NOT NULL,
				stored_enc    TEXT NOT NULL DEFAULT 'identity',
				refcount      INTEGER NOT NULL DEFAULT 0,
				created_at    INTEGER NOT NULL
			)`,
			`CREATE TABLE objects (
				id            INTEGER PRIMARY KEY,
				repo          TEXT NOT NULL,
				key           TEXT NOT NULL,
				class         TEXT NOT NULL,
				generation    INTEGER NOT NULL DEFAULT 1,
				current       INTEGER NOT NULL DEFAULT 1,
				sha256        TEXT NOT NULL REFERENCES blobs(sha256),
				content_type  TEXT NOT NULL DEFAULT '',
				etag          TEXT NOT NULL DEFAULT '',
				last_modified TEXT NOT NULL DEFAULT '',
				upstream      TEXT NOT NULL DEFAULT '',
				fetched_at    INTEGER NOT NULL,
				validated_at  INTEGER NOT NULL,
				last_access   INTEGER NOT NULL,
				hits          INTEGER NOT NULL DEFAULT 0,
				verified      INTEGER NOT NULL DEFAULT 0
			)`,
			`CREATE UNIQUE INDEX objects_current ON objects(repo, key) WHERE current = 1`,
			`CREATE INDEX objects_gen ON objects(repo, key, generation)`,
			`CREATE INDEX objects_evict ON objects(class, current, last_access)`,
			`CREATE INDEX objects_repo ON objects(repo, class)`,
			`CREATE INDEX objects_sha ON objects(sha256)`,
			`CREATE TABLE expectations (
				repo    TEXT NOT NULL,
				key     TEXT NOT NULL,
				algo    TEXT NOT NULL,
				digest  TEXT NOT NULL,
				source  TEXT NOT NULL,
				created_at INTEGER NOT NULL,
				PRIMARY KEY (repo, key, algo)
			)`,
			`CREATE TABLE pin_sets (name TEXT PRIMARY KEY, created_at INTEGER NOT NULL, note TEXT NOT NULL DEFAULT '')`,
			`CREATE TABLE pin_items (
				set_name TEXT NOT NULL REFERENCES pin_sets(name) ON DELETE CASCADE,
				repo TEXT NOT NULL,
				key TEXT NOT NULL,
				PRIMARY KEY (set_name, repo, key)
			)`,
			`CREATE TABLE pin_rules (
				set_name TEXT NOT NULL REFERENCES pin_sets(name) ON DELETE CASCADE,
				repo TEXT NOT NULL,
				pattern TEXT NOT NULL,
				PRIMARY KEY (set_name, repo, pattern)
			)`,
		},
	},
}

func (db *DB) migrate() error {
	var exists int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'meta'`).Scan(&exists); err != nil {
		return fmt.Errorf("inspect schema: %w", err)
	}

	version := 0
	if exists > 0 {
		var value string
		err := db.sql.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&value)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			version = 0
		case err != nil:
			return fmt.Errorf("read schema version: %w", err)
		default:
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("parse schema version %q: %w", value, err)
			}
			version = parsed
		}
	}

	latest := 0
	for _, m := range migrations {
		if m.version > latest {
			latest = m.version
		}
	}
	if version > latest {
		return fmt.Errorf("%w: database version %d, binary version %d", ErrSchemaTooNew, version, latest)
	}
	if version == latest {
		return nil
	}

	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	for _, m := range migrations {
		if m.version <= version {
			continue
		}
		for _, statement := range m.statements {
			if _, err := tx.Exec(statement); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("apply migration %d: %w", m.version, err)
			}
		}
		version = m.version
	}
	if _, err := tx.Exec(
		`INSERT INTO meta (key, value) VALUES ('schema_version', ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		strconv.Itoa(version),
	); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("record schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}
	return nil
}
