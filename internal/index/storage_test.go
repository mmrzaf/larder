package index

import (
	"strings"
	"testing"
)

func TestCandidateQueriesUseDeclaredIndexes(t *testing.T) {
	db := openIndex(t)
	cases := []struct {
		name  string
		query string
		want  string
		args  []any
	}{
		{
			name: "eviction",
			query: `SELECT o.id FROM objects o JOIN blobs b ON b.sha256 = o.sha256
				WHERE o.current = 1 AND o.class = ?
				ORDER BY o.last_access ASC, o.id ASC LIMIT 100`,
			want: "objects_evict",
			args: []any{"immutable"},
		},
		{
			name: "history",
			query: `SELECT o.id FROM objects o
				WHERE o.class = 'mutable' AND o.current = 0 AND o.repo = ?
				AND (SELECT COUNT(*) FROM objects n
				     WHERE n.repo = o.repo AND n.key = o.key AND n.generation > o.generation) >= ?
				ORDER BY o.repo, o.key, o.generation`,
			want: "objects_gen",
			args: []any{"repo", 2},
		},
		{
			name:  "current key",
			query: `SELECT id FROM objects WHERE repo = ? AND key = ? AND current = 1`,
			want:  "objects_current",
			args:  []any{"repo", "/key"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := db.sql.Query(`EXPLAIN QUERY PLAN `+tc.query, tc.args...)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = rows.Close() }()
			var details []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				details = append(details, detail)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			plan := strings.Join(details, " | ")
			if !strings.Contains(plan, tc.want) {
				t.Fatalf("plan does not use %s: %s", tc.want, plan)
			}
		})
	}
}
