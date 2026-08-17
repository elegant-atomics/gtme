package conformance

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/elegant-atomics/gtme/internal/ledger"

	_ "modernc.org/sqlite"
)

// implementationOnlyObjects are tables the migrations add that SPEC §3 does not
// name. Each is recorded as a Decision and is spec-invisible under ADR-010's
// litmus ("would a second clean-room implementation need this to interoperate?"),
// so their presence is not a divergence. Anything outside this list is.
var implementationOnlyObjects = map[string]string{
	"schema_migrations": "migration bookkeeping (SPEC §3 mandates numbered migration files but names no ledger table for them)",
	"identity_aliases":  "DECISIONS.md 2026-08-12: keeps every key an identity has been known by, so an identity upgrade (SPEC §4) cannot produce a duplicate",
	"saved_queries":     "DECISIONS.md: storage for `gtme query --save NAME` (SPEC §8 names the feature, not its storage)",
}

// schemaObjects lists the tables and views in a database, by name.
func schemaObjects(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT type, name FROM sqlite_master
	                       WHERE type IN ('table','view') AND name NOT LIKE 'sqlite_%'
	                       ORDER BY name`)
	if err != nil {
		t.Fatalf("reading sqlite_master: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var kind, name string
		if err := rows.Scan(&kind, &name); err != nil {
			t.Fatalf("scanning sqlite_master: %v", err)
		}
		out[name] = kind
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading sqlite_master: %v", err)
	}
	return out
}

// columnsOf lists a table or view's column names, in declaration order.
func columnsOf(t *testing.T, db *sql.DB, object string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, object)
	if err != nil {
		t.Fatalf("reading columns of %s: %v", object, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scanning columns of %s: %v", object, err)
		}
		out = append(out, name)
	}
	return out
}

// openCanonical applies spec/ledger.sql to a fresh database. If this fails, the
// canonical DDL is not standalone-runnable, which is a bug in spec/ledger.sql.
func openCanonical(t *testing.T) *sql.DB {
	t.Helper()
	path := specPath("ledger.sql")
	ddl, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	file := filepath.Join(t.TempDir(), "canonical.db")
	db, err := sql.Open("sqlite", "file:"+file+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("opening canonical db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatalf("spec/ledger.sql does not apply to an empty SQLite database: %v", err)
	}
	return db
}

// openMigrated opens a ledger through the real migration path.
func openMigrated(t *testing.T) *sql.DB {
	t.Helper()
	l, err := ledger.Open(context.Background(), filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("opening ledger through internal/ledger: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l.DB()
}

// TestLedgerSchemaMatchesSpec is the ADR-010 schema-conformance test: the
// canonical DDL in spec/ledger.sql and the schema the migrations actually build
// must describe the same ledger.
func TestLedgerSchemaMatchesSpec(t *testing.T) {
	canonical := schemaObjects(t, openCanonical(t))
	migratedDB := openMigrated(t)
	migrated := schemaObjects(t, migratedDB)

	t.Run("every spec object exists in the migrated schema", func(t *testing.T) {
		var missing []string
		for name, kind := range canonical {
			got, ok := migrated[name]
			if !ok {
				missing = append(missing, kind+" "+name)
				continue
			}
			if got != kind {
				t.Errorf("%s: spec/ledger.sql defines it as a %s, the migrations create a %s", name, kind, got)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("spec/ledger.sql defines objects the migrations never create: %s\n"+
				"  SPEC.md §3 is DECIDED; an object it defines must exist after internal/ledger.Open.\n"+
				"  Fix by adding a migration under internal/ledger/migrations/, not by editing spec/ledger.sql.",
				strings.Join(missing, ", "))
		}
	})

	t.Run("extra objects are recorded implementation additions", func(t *testing.T) {
		var undocumented []string
		for name := range migrated {
			if _, inSpec := canonical[name]; inSpec {
				continue
			}
			why, allowed := implementationOnlyObjects[name]
			if !allowed {
				undocumented = append(undocumented, name)
				continue
			}
			t.Logf("implementation-only object %q is permitted: %s", name, why)
		}
		sort.Strings(undocumented)
		if len(undocumented) > 0 {
			t.Errorf("the migrations create objects that are neither in spec/ledger.sql nor recorded as "+
				"spec-invisible additions: %s\n"+
				"  Either add a Decision (SPEC §12) and list it in implementationOnlyObjects, or amend SPEC.md §3.",
				strings.Join(undocumented, ", "))
		}
	})

	t.Run("shared objects have the same columns", func(t *testing.T) {
		canonicalDB := openCanonical(t)
		names := make([]string, 0, len(canonical))
		for name := range canonical {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if _, ok := migrated[name]; !ok {
				continue // already reported as missing above
			}
			want := columnsOf(t, canonicalDB, name)
			got := columnsOf(t, migratedDB, name)
			if strings.Join(want, ",") != strings.Join(got, ",") {
				t.Errorf("%s columns differ:\n  spec/ledger.sql: %s\n  migrations:      %s",
					name, strings.Join(want, ", "), strings.Join(got, ", "))
			}
		}
	})
}
