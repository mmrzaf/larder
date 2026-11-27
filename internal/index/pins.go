package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// CreatePinSet creates a named pin set.
func (db *DB) CreatePinSet(ctx context.Context, set PinSet) error {
	if set.Name == "" {
		return fmt.Errorf("create pin set: name is required")
	}
	if set.CreatedAt.IsZero() {
		return fmt.Errorf("create pin set %s: created_at is required", set.Name)
	}
	if _, err := db.execWrite(ctx,
		`INSERT INTO pin_sets (name, created_at, note) VALUES (?, ?, ?)`,
		set.Name, unixTime(set.CreatedAt), set.Note,
	); err != nil {
		return fmt.Errorf("create pin set %s: %w", set.Name, constraintError(err))
	}
	return nil
}

// DeletePinSet deletes a set and its members.
func (db *DB) DeletePinSet(ctx context.Context, name string) error {
	res, err := db.execWrite(ctx, `DELETE FROM pin_sets WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("delete pin set %s: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete pin set rows %s: %w", name, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListPinSets lists every pin set with member counts.
func (db *DB) ListPinSets(ctx context.Context) ([]PinSet, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT s.name, s.created_at, s.note,
		       (SELECT COUNT(*) FROM pin_items i WHERE i.set_name = s.name),
		       (SELECT COUNT(*) FROM pin_rules r WHERE r.set_name = s.name)
		FROM pin_sets s ORDER BY s.name`)
	if err != nil {
		return nil, fmt.Errorf("list pin sets: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []PinSet
	for rows.Next() {
		var set PinSet
		var created int64
		if err := rows.Scan(&set.Name, &created, &set.Note, &set.Items, &set.Rules); err != nil {
			return nil, fmt.Errorf("scan pin set: %w", err)
		}
		set.CreatedAt = fromUnix(created)
		out = append(out, set)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pin sets: %w", err)
	}
	return out, nil
}

// AddPinItem pins one exact key.
func (db *DB) AddPinItem(ctx context.Context, item PinItem) error {
	if item.SetName == "" || item.Repo == "" || item.Key == "" {
		return fmt.Errorf("add pin item: set, repo and key are required")
	}
	if _, err := db.execWrite(ctx,
		`INSERT INTO pin_items (set_name, repo, key) VALUES (?, ?, ?)`,
		item.SetName, item.Repo, item.Key,
	); err != nil {
		return fmt.Errorf("add pin item %s/%s/%s: %w", item.SetName, item.Repo, item.Key, constraintError(err))
	}
	return nil
}

// RemovePinItem unpins one exact key.
func (db *DB) RemovePinItem(ctx context.Context, item PinItem) error {
	res, err := db.execWrite(ctx,
		`DELETE FROM pin_items WHERE set_name = ? AND repo = ? AND key = ?`,
		item.SetName, item.Repo, item.Key)
	if err != nil {
		return fmt.Errorf("remove pin item %s/%s/%s: %w", item.SetName, item.Repo, item.Key, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("remove pin item rows: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// AddPinRule pins every key matching a glob in one repo.
func (db *DB) AddPinRule(ctx context.Context, rule PinRule) error {
	if rule.SetName == "" || rule.Repo == "" || rule.Pattern == "" {
		return fmt.Errorf("add pin rule: set, repo and pattern are required")
	}
	if _, err := db.execWrite(ctx,
		`INSERT INTO pin_rules (set_name, repo, pattern) VALUES (?, ?, ?)`,
		rule.SetName, rule.Repo, rule.Pattern,
	); err != nil {
		return fmt.Errorf("add pin rule %s/%s/%s: %w", rule.SetName, rule.Repo, rule.Pattern, constraintError(err))
	}
	return nil
}

// RemovePinRule removes one glob rule.
func (db *DB) RemovePinRule(ctx context.Context, rule PinRule) error {
	res, err := db.execWrite(ctx,
		`DELETE FROM pin_rules WHERE set_name = ? AND repo = ? AND pattern = ?`,
		rule.SetName, rule.Repo, rule.Pattern)
	if err != nil {
		return fmt.Errorf("remove pin rule %s/%s/%s: %w", rule.SetName, rule.Repo, rule.Pattern, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("remove pin rule rows: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// PinSetMembers returns the exact keys and glob rules of a set.
func (db *DB) PinSetMembers(ctx context.Context, name string) ([]PinItem, []PinRule, error) {
	var exists int
	if err := db.sql.QueryRowContext(ctx, `SELECT 1 FROM pin_sets WHERE name = ?`, name).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, fmt.Errorf("read pin set %s: %w", name, err)
	}

	itemRows, err := db.sql.QueryContext(ctx,
		`SELECT set_name, repo, key FROM pin_items WHERE set_name = ? ORDER BY repo, key`, name)
	if err != nil {
		return nil, nil, fmt.Errorf("list pin items %s: %w", name, err)
	}
	var items []PinItem
	for itemRows.Next() {
		var item PinItem
		if err := itemRows.Scan(&item.SetName, &item.Repo, &item.Key); err != nil {
			_ = itemRows.Close()
			return nil, nil, fmt.Errorf("scan pin item %s: %w", name, err)
		}
		items = append(items, item)
	}
	if err := itemRows.Err(); err != nil {
		_ = itemRows.Close()
		return nil, nil, fmt.Errorf("list pin items %s: %w", name, err)
	}
	if err := itemRows.Close(); err != nil {
		return nil, nil, fmt.Errorf("close pin items %s: %w", name, err)
	}

	ruleRows, err := db.sql.QueryContext(ctx,
		`SELECT set_name, repo, pattern FROM pin_rules WHERE set_name = ? ORDER BY repo, pattern`, name)
	if err != nil {
		return nil, nil, fmt.Errorf("list pin rules %s: %w", name, err)
	}
	defer func() { _ = ruleRows.Close() }()
	var rules []PinRule
	for ruleRows.Next() {
		var rule PinRule
		if err := ruleRows.Scan(&rule.SetName, &rule.Repo, &rule.Pattern); err != nil {
			return nil, nil, fmt.Errorf("scan pin rule %s: %w", name, err)
		}
		rules = append(rules, rule)
	}
	if err := ruleRows.Err(); err != nil {
		return nil, nil, fmt.Errorf("list pin rules %s: %w", name, err)
	}
	return items, rules, nil
}

// Pinned reports whether a key matches an exact pin item or a glob rule.
func (db *DB) Pinned(ctx context.Context, repo, key string) (bool, error) {
	var count int
	if err := db.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pin_items WHERE repo = ? AND key = ?`, repo, key,
	).Scan(&count); err != nil {
		return false, fmt.Errorf("check pin items %s/%s: %w", repo, key, err)
	}
	if count > 0 {
		return true, nil
	}

	rows, err := db.sql.QueryContext(ctx, `SELECT pattern FROM pin_rules WHERE repo = ?`, repo)
	if err != nil {
		return false, fmt.Errorf("check pin rules %s/%s: %w", repo, key, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var pattern string
		if err := rows.Scan(&pattern); err != nil {
			return false, fmt.Errorf("scan pin rule %s/%s: %w", repo, key, err)
		}
		if matchGlob(pattern, key) {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("check pin rules %s/%s: %w", repo, key, err)
	}
	return false, nil
}

func constraintError(err error) error {
	switch {
	case err == nil:
		return nil
	case strings.Contains(err.Error(), "FOREIGN KEY"):
		return ErrNotFound
	case strings.Contains(err.Error(), "UNIQUE constraint"), strings.Contains(err.Error(), "PRIMARY KEY"):
		return ErrExists
	default:
		return err
	}
}

// PinItems returns every exact pin item.
func (db *DB) PinItems(ctx context.Context) ([]PinItem, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT set_name, repo, key FROM pin_items ORDER BY set_name, repo, key`)
	if err != nil {
		return nil, fmt.Errorf("list pin items: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []PinItem
	for rows.Next() {
		var item PinItem
		if err := rows.Scan(&item.SetName, &item.Repo, &item.Key); err != nil {
			return nil, fmt.Errorf("scan pin item: %w", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pin items: %w", err)
	}
	return out, nil
}

// PinRules returns every glob pin rule.
func (db *DB) PinRules(ctx context.Context) ([]PinRule, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT set_name, repo, pattern FROM pin_rules ORDER BY set_name, repo, pattern`)
	if err != nil {
		return nil, fmt.Errorf("list pin rules: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []PinRule
	for rows.Next() {
		var rule PinRule
		if err := rows.Scan(&rule.SetName, &rule.Repo, &rule.Pattern); err != nil {
			return nil, fmt.Errorf("scan pin rule: %w", err)
		}
		out = append(out, rule)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pin rules: %w", err)
	}
	return out, nil
}
