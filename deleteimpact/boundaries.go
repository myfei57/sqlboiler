package deleteimpact

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// triggerRef names a trigger discovered in the database catalog for evidence
// reporting. Triggers are never simulated.
type triggerRef struct {
	table string
	name  string
}

// addAdapterBoundaries records evidence boundaries that hold regardless of the
// traversal outcome. The analysis never claims to predict trigger side
// effects, and some adapters cannot expose deferred constraint state at all.
func (a *Analyzer) addAdapterBoundaries(add func(kind, detail string)) {
	add(BoundaryTrigger,
		"the analysis models only declared foreign key referential actions visible in the caller snapshot; triggers, rules and application side effects are not simulated")

	if a.dialect == DialectSQLite {
		add(BoundaryAdapter,
			"SQLite does not expose DEFERRABLE/INITIALLY DEFERRED state through PRAGMA foreign_key_list; deferred enforcement timing cannot be observed")
	}
}

// addTriggerBoundaries performs a best-effort inventory of triggers attached
// to tables touched by the deletion closure. The inventory is evidence only:
// triggers are never simulated. A failure to read the inventory degrades to a
// generic boundary instead of failing the whole analysis.
func (a *Analyzer) addTriggerBoundaries(
	ctx context.Context,
	q Queryer,
	report *Report,
	add func(kind, detail string),
	closure []RowRef,
) error {
	affected := make(map[string]bool)
	for _, ref := range closure {
		affected[ref.Table] = true
	}
	if len(affected) == 0 {
		return nil
	}

	tables := make([]string, 0, len(affected))
	for name := range affected {
		tables = append(tables, name)
	}
	sort.Strings(tables)

	if report.Stats.SQLStatements >= a.limits.MaxSQLStatements {
		add(BoundaryTrigger, "trigger inventory skipped because the SQL statement limit was reached; trigger side effects remain unobserved")
		return nil
	}

	var triggers []triggerRef
	var queryErr error

	switch a.dialect {
	case DialectSQLite:
		triggers, queryErr = a.sqliteTriggers(ctx, q, report, tables)
	case DialectPostgres:
		triggers, queryErr = a.postgresTriggers(ctx, q, report, tables)
	}

	if queryErr != nil {
		add(BoundaryTrigger, "trigger inventory could not be read from the snapshot: "+queryErr.Error())
		return nil
	}

	sort.Slice(triggers, func(i, j int) bool {
		if triggers[i].table != triggers[j].table {
			return triggers[i].table < triggers[j].table
		}
		return triggers[i].name < triggers[j].name
	})
	for _, trigger := range triggers {
		add(BoundaryTrigger, fmt.Sprintf(
			"trigger %q on table %q is not simulated; its side effects on delete are outside this analysis",
			trigger.name, trigger.table))
	}

	return nil
}

func (a *Analyzer) sqliteTriggers(ctx context.Context, q Queryer, report *Report, tables []string) ([]triggerRef, error) {
	// One catalog query is enough: sqlite_master carries every trigger and
	// filters in Go keep the statement count bounded.
	rows, err := q.QueryContext(ctx,
		`SELECT name, tbl_name FROM sqlite_master WHERE type = 'trigger'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	report.Stats.SQLStatements++

	wanted := make(map[string]bool, len(tables))
	for _, name := range tables {
		wanted[name] = true
	}

	var triggers []triggerRef
	for rows.Next() {
		var name, table string
		if err := rows.Scan(&name, &table); err != nil {
			return nil, err
		}
		if wanted[table] {
			triggers = append(triggers, triggerRef{table: table, name: name})
		}
	}
	return triggers, rows.Err()
}

func (a *Analyzer) postgresTriggers(ctx context.Context, q Queryer, report *Report, tables []string) ([]triggerRef, error) {
	var triggers []triggerRef

	for start := 0; start < len(tables); start += a.limits.MaxTuplesPerStatement {
		end := start + a.limits.MaxTuplesPerStatement
		if end > len(tables) {
			end = len(tables)
		}
		chunk := tables[start:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)+1)
		for i := range chunk {
			placeholders[i] = a.placeholder(i + 2)
			args = append(args, chunk[i])
		}
		query := strings.Join([]string{
			`SELECT c.relname, t.tgname`,
			`FROM pg_trigger t`,
			`INNER JOIN pg_class c ON t.tgrelid = c.oid`,
			`INNER JOIN pg_namespace n ON c.relnamespace = n.oid`,
			`WHERE NOT t.tgisinternal AND n.nspname = $1 AND c.relname IN (` + strings.Join(placeholders, ",") + `)`,
		}, " ")
		finalArgs := append([]any{a.schema}, args...)

		report.Stats.SQLStatements++
		rows, err := q.QueryContext(ctx, query, finalArgs...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var name, table string
			if err := rows.Scan(&table, &name); err != nil {
				rows.Close()
				return nil, err
			}
			triggers = append(triggers, triggerRef{table: table, name: name})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}

	return triggers, nil
}
