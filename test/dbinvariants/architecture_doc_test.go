package dbinvariants_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/riyne/fifty-fifty/internal/testutil"
)

// docs/ARCHITECTURE.md describes the schema. This guard keeps it from drifting: every
// table in the database and every migration file must be named in the document, so the
// person who adds a table or a migration is sent to update it. It checks names only; it
// cannot tell whether the prose around a name is still true.
func TestArchitectureDoc_NamesEveryTableAndMigration(t *testing.T) {
	doc, err := os.ReadFile("../../docs/ARCHITECTURE.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(doc)

	// pg_class, not information_schema: the latter shows only tables the role may use.
	rows, err := testutil.Pool(t).Query(context.Background(), `SELECT c.relname FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	tables := 0
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables++
		if !strings.Contains(text, "`"+name+"`") {
			t.Errorf("table %q is not named (in backticks) in docs/ARCHITECTURE.md: describe it in section 2.3", name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if tables < 10 {
		t.Fatalf("found only %d tables: is the test database migrated?", tables)
	}

	files, err := filepath.Glob("../../migrations/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations found: %v", err)
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".sql")
		if !strings.Contains(text, "`"+name+"`") {
			t.Errorf("migration %q is not listed in docs/ARCHITECTURE.md section 2.1", name)
		}
	}
}
