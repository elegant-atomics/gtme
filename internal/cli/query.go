package cli

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/elegant-atomics/gtme/internal/ledger"
)

// cmdQuery runs read-only SQL against the ledger, and saves or replays named
// segments (SPEC §8). Rows go to stdout as NDJSON by default, because stdout is
// data; --format table is for reading with your eyes.
func cmdQuery(ctx context.Context, env Env, args []string) error {
	fs := flag.NewFlagSet("query", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	save := fs.String("save", "", "save this SQL as a named segment")
	name := fs.String("name", "", "run a saved segment by name")
	list := fs.Bool("list", false, "list saved segments")
	format := fs.String("format", "ndjson", "output format: ndjson|table|csv")
	limit := fs.Int("limit", 0, "stop after N rows (0 = all)")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}

	l, err := openLedger(ctx)
	if err != nil {
		return err
	}
	defer l.Close()

	if *list {
		saved, err := l.SavedQueries(ctx)
		if err != nil {
			return fail(ExitOther, "%v", err)
		}
		tw := tabwriter.NewWriter(env.Stderr, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "name\tsql")
		for _, q := range saved {
			fmt.Fprintf(tw, "%s\t%s\n", q.Name, oneLine(q.SQL))
		}
		tw.Flush()
		if len(saved) == 0 {
			fmt.Fprintln(env.Stderr, "no saved segments yet — save one with `gtme query --save NAME \"SQL\"`")
		}
		return nil
	}

	query := strings.Join(positional, " ")
	switch {
	case *name != "" && query != "":
		return fail(ExitValidation, "pass either --name or SQL, not both")
	case *name != "":
		saved, err := l.SavedQuery(ctx, *name)
		if err != nil {
			if errors.Is(err, ledger.ErrNotFound) {
				return fail(ExitValidation, "no saved segment named %q", *name)
			}
			return fail(ExitOther, "%v", err)
		}
		query = saved.SQL
	case query == "":
		return fail(ExitValidation, `usage: gtme query "SQL" | gtme query --name NAME | gtme query --list`)
	}

	if err := ledger.ReadOnlyStatement(query); err != nil {
		return fail(ExitValidation, "%v", err)
	}

	if *save != "" {
		if err := l.SaveQuery(ctx, *save, query); err != nil {
			return fail(ExitOther, "%v", err)
		}
		fmt.Fprintf(env.Stderr, "saved segment %q\n", *save)
	}

	db, err := ledger.OpenReadOnly(ctx, l.Path())
	if err != nil {
		return fail(ExitOther, "%v", err)
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return fail(ExitValidation, "query failed: %v", err)
	}
	defer rows.Close()

	n, err := writeRows(env, rows, *format, *limit)
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stderr, "%d rows\n", n)
	return nil
}

// writeRows renders a result set in the requested format and returns the row
// count.
func writeRows(env Env, rows *sql.Rows, format string, limit int) (int, error) {
	cols, err := rows.Columns()
	if err != nil {
		return 0, fail(ExitOther, "reading columns: %v", err)
	}

	var (
		enc   *json.Encoder
		cw    *csv.Writer
		tw    *tabwriter.Writer
		count int
	)
	switch format {
	case "ndjson":
		enc = json.NewEncoder(env.Stdout)
	case "csv":
		cw = csv.NewWriter(env.Stdout)
		if err := cw.Write(cols); err != nil {
			return 0, fail(ExitOther, "writing csv: %v", err)
		}
	case "table":
		tw = tabwriter.NewWriter(env.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, strings.Join(cols, "\t"))
	default:
		return 0, fail(ExitValidation, "unknown --format %q (want ndjson, table or csv)", format)
	}

	for rows.Next() {
		if limit > 0 && count >= limit {
			break
		}
		cells := make([]any, len(cols))
		holders := make([]any, len(cols))
		for i := range cells {
			holders[i] = &cells[i]
		}
		if err := rows.Scan(holders...); err != nil {
			return count, fail(ExitOther, "reading row: %v", err)
		}

		switch {
		case enc != nil:
			obj := make(map[string]any, len(cols))
			for i, col := range cols {
				obj[col] = normalize(cells[i])
			}
			if err := enc.Encode(obj); err != nil {
				return count, fail(ExitOther, "writing json: %v", err)
			}
		case cw != nil:
			record := make([]string, len(cols))
			for i := range cols {
				record[i] = display(cells[i])
			}
			if err := cw.Write(record); err != nil {
				return count, fail(ExitOther, "writing csv: %v", err)
			}
		case tw != nil:
			record := make([]string, len(cols))
			for i := range cols {
				record[i] = display(cells[i])
			}
			fmt.Fprintln(tw, strings.Join(record, "\t"))
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return count, fail(ExitOther, "reading rows: %v", err)
	}

	if cw != nil {
		cw.Flush()
		if err := cw.Error(); err != nil {
			return count, fail(ExitOther, "writing csv: %v", err)
		}
	}
	if tw != nil {
		tw.Flush()
	}
	return count, nil
}

// normalize turns driver values into something JSON-friendly, and decodes stored
// field values so `SELECT value FROM field_values` reads naturally.
func normalize(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case []byte:
		return string(t)
	default:
		return v
	}
}

func display(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case []byte:
		return string(t)
	case string:
		return t
	default:
		return fmt.Sprint(t)
	}
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
