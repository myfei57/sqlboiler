package deleteimpact

import (
	"fmt"
	"strings"
)

func (a *Analyzer) quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// nonEmpty returns value when it is non-empty, otherwise fallback.
func nonEmpty(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func (a *Analyzer) quoteTable(schema, table string) string {
	if schema == "" {
		return a.quoteIdent(table)
	}
	return a.quoteIdent(schema) + "." + a.quoteIdent(table)
}

// placeholder renders the positional placeholder for the 1-based parameter
// position. PostgreSQL uses $n while SQLite uses ?.
func (a *Analyzer) placeholder(position int) string {
	if a.dialect == DialectPostgres {
		return fmt.Sprintf("$%d", position)
	}
	return "?"
}

// renderInTuple renders an IN expression matching the foreign key columns of a
// constraint against one or more referenced primary key tuples. It returns the
// expression together with the flattened args and the next parameter position.
//
// Single column keys render as a plain IN list. Composite keys use row value
// syntax: PostgreSQL tuple IN, SQLite VALUES IN. NULL elements never match in
// either dialect, which already implements the MATCH SIMPLE / MATCH FULL
// exclusion of partially or fully NULL foreign keys at the SQL level.
func (a *Analyzer) renderInTuple(fkColumns []string, tuples [][]any, startParam int) (string, []any, int) {
	param := startParam
	args := make([]any, 0, len(tuples)*len(fkColumns))

	placeholders := make([]string, len(tuples))
	for i, tuple := range tuples {
		tuplePlaceholders := make([]string, len(tuple))
		for j := range tuple {
			tuplePlaceholders[j] = a.placeholder(param)
			args = append(args, tuple[j])
			param++
		}
		placeholders[i] = "(" + strings.Join(tuplePlaceholders, ",") + ")"
	}

	quotedCols := make([]string, len(fkColumns))
	for i, col := range fkColumns {
		quotedCols[i] = a.quoteIdent(col)
	}
	colList := "(" + strings.Join(quotedCols, ",") + ")"

	var expr string
	if len(fkColumns) == 1 {
		// Plain IN list, without the row value wrapper.
		values := make([]string, len(placeholders))
		for i, ph := range placeholders {
			values[i] = strings.TrimPrefix(ph, "(")
			values[i] = strings.TrimSuffix(values[i], ")")
		}
		expr = quotedCols[0] + " IN (" + strings.Join(values, ",") + ")"
	} else if a.dialect == DialectPostgres {
		expr = colList + " IN (" + strings.Join(placeholders, ",") + ")"
	} else {
		expr = colList + " IN (VALUES " + strings.Join(placeholders, ",") + ")"
	}

	return expr, args, param
}

// buildSelectQuery renders a SELECT of the given columns filtered by a tuple IN
// expression. selectColumns must already be ordered as expected by the scanner.
func (a *Analyzer) buildSelectQuery(schema, table string, selectColumns, fkColumns []string, tuples [][]any) (string, []any) {
	expr, args, _ := a.renderInTuple(fkColumns, tuples, 1)

	quotedSelect := make([]string, len(selectColumns))
	for i, col := range selectColumns {
		quotedSelect[i] = a.quoteIdent(col)
	}

	return fmt.Sprintf("SELECT %s FROM %s WHERE %s",
		strings.Join(quotedSelect, ","),
		a.quoteTable(schema, table),
		expr,
	), args
}

// chunkTuples splits parent key tuples into query sized chunks.
func (a *Analyzer) chunkTuples(tuples [][]any) [][][]any {
	size := a.limits.MaxTuplesPerStatement
	if size <= 0 {
		size = DefaultLimits().MaxTuplesPerStatement
	}
	if len(tuples) <= size {
		return [][][]any{tuples}
	}

	chunks := make([][][]any, 0, (len(tuples)+size-1)/size)
	for start := 0; start < len(tuples); start += size {
		end := start + size
		if end > len(tuples) {
			end = len(tuples)
		}
		chunks = append(chunks, tuples[start:end])
	}
	return chunks
}
