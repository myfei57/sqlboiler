package deleteimpact

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/aarondl/sqlboiler/v4/drivers"
)

type tableIndexKey struct {
	schema string
	name   string
}

type nodeState struct {
	ref RowRef
	via *Edge
}

type actionCandidate struct {
	edge     Edge
	columns  []string
	fkValues []any
}

// Analyze computes the delete impact of the supplied roots against the
// caller's read-only snapshot queryer. It issues only SELECT/PRAGMA style
// reads through q, never opens another connection and never executes a
// DELETE. The returned report is fully owned by the caller: every scanned row
// is copied into Go values before its *sql.Rows is closed.
func (a *Analyzer) Analyze(ctx context.Context, q Queryer, roots []Root) (*Report, error) {
	if q == nil {
		return nil, fmt.Errorf("deleteimpact: a read-only snapshot queryer is required")
	}
	if len(roots) == 0 {
		return nil, fmt.Errorf("deleteimpact: at least one root primary key is required")
	}
	if a.dialect != DialectPostgres && a.dialect != DialectSQLite {
		return nil, fmt.Errorf("deleteimpact: unsupported dialect %q, expected postgres or sqlite3", a.dialect)
	}

	report := &Report{Dialect: a.dialect, Schema: a.schema}
	tables, legacyIncoming, incoming, err := a.buildIndexes()
	if err != nil {
		return nil, err
	}

	noteSet := make(map[string]bool)
	markIncomplete := func(reason string) {
		report.Incomplete = true
		if !noteSet[reason] {
			noteSet[reason] = true
			report.Notes = append(report.Notes, reason)
		}
	}

	validRoots, err := a.validateRoots(roots, tables)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]*nodeState)
	edgeSet := make(map[string]bool)
	unknownSet := make(map[string]bool)
	boundarySet := make(map[string]bool)

	var (
		queue             []RowRef
		discoveryOrder    []RowRef
		edges             []Edge
		nullCandidates    []actionCandidate
		blockerCandidates []actionCandidate
	)

	addUnknown := func(schema, table, constraint, reason string) {
		key := schema + "\x00" + table + "\x00" + constraint + "\x00" + reason
		if unknownSet[key] {
			return
		}
		if len(report.Unknowns) >= a.limits.MaxReportEntries {
			markIncomplete(fmt.Sprintf("report entry limit of %d reached while collecting unknowns", a.limits.MaxReportEntries))
			return
		}
		unknownSet[key] = true
		report.Unknowns = append(report.Unknowns, Unknown{
			Schema: schema, Table: table, Constraint: constraint, Reason: reason,
		})
	}

	addBoundary := func(kind, detail string) {
		key := kind + "\x00" + detail
		if boundarySet[key] {
			return
		}
		boundarySet[key] = true
		report.Boundaries = append(report.Boundaries, Boundary{Kind: kind, Detail: detail})
	}

	// Verify root existence against the snapshot before traversing anything.
	foundRootKeys := make(map[string]bool)
	for key, refs := range groupRefs(validRoots) {
		table, ok := tables[key]
		if !ok {
			continue
		}
		found, err := a.existingRows(ctx, q, report, table, key.schema, refs)
		if err != nil {
			return nil, err
		}
		for _, ref := range found {
			foundRootKeys[rowKey(ref)] = true
		}
	}

	// Preserve the caller supplied root order in every report slice.
	for _, ref := range validRoots {
		if foundRootKeys[rowKey(ref)] {
			queue = append(queue, ref)
		} else {
			report.NotFoundRoots = append(report.NotFoundRoots, ref)
		}
	}

	// Seed the candidate deletion closure with the existing roots.
	for _, ref := range queue {
		key := rowKey(ref)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = &nodeState{ref: ref}
		discoveryOrder = append(discoveryOrder, ref)
		report.Stats.Nodes++
	}
	report.Roots = append(report.Roots, queue...)

	// Fixed point breadth first traversal. Candidates for SET NULL and
	// RESTRICT are collected during traversal and only decided after the full
	// deletion closure is known, so a row cascaded away through one path is
	// never prematurely judged through another path.
	for len(queue) > 0 && !report.Incomplete {
		batch := queue
		queue = nil
		groups := groupRefs(batch)

		// Deterministic processing order independent of map iteration.
		keys := make([]tableIndexKey, 0, len(groups))
		for key := range groups {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].schema != keys[j].schema {
				return keys[i].schema < keys[j].schema
			}
			return keys[i].name < keys[j].name
		})

		for _, parentKey := range keys {
			parentRefs := groups[parentKey]
			if _, ok := tables[parentKey]; !ok {
				continue
			}

			// Relationships from adapters without constraint level metadata
			// stay unknown: single column ForeignKey data must never be used
			// to guess composite relationships or actions.
			for _, legacy := range legacyIncoming[parentKey] {
				addUnknown(parentKey.schema, legacy.Table, legacy.Name,
					"adapter does not provide constraint level foreign key metadata; referential action and composite column correspondence are unknown")
			}

			for _, constraint := range incoming[parentKey] {
				if reason := a.unsupportedReason(constraint); reason != "" {
					addUnknown(constraint.Schema, constraint.Table, constraint.Name, reason)
					continue
				}
				childKey := tableIndexKey{schema: nonEmpty(constraint.Schema, a.schema), name: constraint.Table}
				childTable, ok := tables[childKey]
				if !ok {
					addUnknown(constraint.Schema, constraint.Table, constraint.Name,
						"referencing table is not present in the supplied schema metadata")
					continue
				}
				if childTable.IsView {
					addUnknown(constraint.Schema, constraint.Table, constraint.Name,
						"referencing relation is a view and cannot participate in delete impact analysis")
					continue
				}
				if constraint.Deferrable {
					detail := "constraint " + constraint.Name + " on " + constraint.Table +
						" is DEFERRABLE"
					if constraint.InitiallyDeferred {
						detail += " INITIALLY DEFERRED"
					}
					detail += "; analysis assumes no further writes before commit time"
					addBoundary(BoundaryDeferredConstraint, detail)
				}

				tuples := make([][]any, len(parentRefs))
				for i, ref := range parentRefs {
					tuples[i] = ref.PK
				}
				for _, chunk := range a.chunkTuples(tuples) {
					if a.atSQLLimit(report) {
						markIncomplete(fmt.Sprintf("sql statement limit of %d reached", a.limits.MaxSQLStatements))
						break
					}
					childRows, err := a.queryReferencingRows(ctx, q, report, childTable, childKey.schema, constraint, chunk)
					if err != nil {
						return nil, err
					}
					if a.atRowsLimit(report) {
						markIncomplete(fmt.Sprintf("rows scanned limit of %d reached", a.limits.MaxRowsScanned))
						break
					}

					for _, childRow := range childRows {
						childRef := childRow.ref
						edge := Edge{
							Constraint: constraint.Name,
							From:       childRef,
							To:         childRow.parent,
							Action:     constraint.OnDelete,
						}
						edgeKey := edgeKey(edge)
						if !edgeSet[edgeKey] {
							if report.Stats.Edges >= a.limits.MaxEdges {
								markIncomplete(fmt.Sprintf("edge limit of %d reached", a.limits.MaxEdges))
								break
							}
							edgeSet[edgeKey] = true
							edges = append(edges, edge)
							report.Stats.Edges++
						}

						switch constraint.OnDelete {
						case drivers.RefActionCascade:
							rk := rowKey(childRef)
							if _, ok := seen[rk]; ok {
								continue
							}
							if report.Stats.Nodes >= a.limits.MaxNodes {
								markIncomplete(fmt.Sprintf("node limit of %d reached", a.limits.MaxNodes))
								break
							}
							discovery := edge
							seen[rk] = &nodeState{ref: childRef, via: &discovery}
							discoveryOrder = append(discoveryOrder, childRef)
							queue = append(queue, childRef)
							report.Stats.Nodes++
						case drivers.RefActionSetNull:
							nullCandidates = appendCandidate(nullCandidates, actionCandidate{
								edge: edge, columns: constraint.LocalColumns(), fkValues: childRow.fkValues,
							})
						case drivers.RefActionRestrict, drivers.RefActionNoAction:
							blockerCandidates = appendCandidate(blockerCandidates, actionCandidate{
								edge: edge, columns: constraint.LocalColumns(), fkValues: childRow.fkValues,
							})
						default:
							addUnknown(constraint.Schema, constraint.Table, constraint.Name,
								"unsupported ON DELETE action "+string(constraint.OnDelete))
						}
					}
				}
			}
		}
	}

	// Decide SET NULL candidates against the final deletion closure.
	candidateSet := make(map[string]bool)
	for _, candidate := range nullCandidates {
		if _, deleted := seen[rowKey(candidate.edge.From)]; deleted {
			// The row is removed by a CASCADE anyway, so the SET NULL is moot.
			continue
		}
		key := "null\x00" + edgeKey(candidate.edge)
		if candidateSet[key] {
			continue
		}
		candidateSet[key] = true
		columns := nonNullColumns(candidate)
		if len(columns) == 0 {
			continue
		}
		if len(report.NulledColumns) >= a.limits.MaxReportEntries {
			markIncomplete(fmt.Sprintf("report entry limit of %d reached while collecting SET NULL effects", a.limits.MaxReportEntries))
			break
		}
		report.NulledColumns = append(report.NulledColumns, NulledColumn{
			Row:        candidate.edge.From,
			Constraint: candidate.edge.Constraint,
			Columns:    columns,
		})
	}

	// Decide RESTRICT / NO ACTION candidates against the final closure and
	// attach the shortest root-to-blocker path.
	blockerSet := make(map[string]bool)
	for _, candidate := range blockerCandidates {
		if _, deleted := seen[rowKey(candidate.edge.From)]; deleted {
			// The blocking row is itself removed by a CASCADE, so it cannot
			// block the delete at commit time.
			continue
		}
		key := "block\x00" + edgeKey(candidate.edge)
		if blockerSet[key] {
			continue
		}
		blockerSet[key] = true
		if len(report.Blockers) >= a.limits.MaxReportEntries {
			markIncomplete(fmt.Sprintf("report entry limit of %d reached while collecting blockers", a.limits.MaxReportEntries))
			break
		}
		report.Blockers = append(report.Blockers, Blocker{
			Row:        candidate.edge.From,
			Constraint: candidate.edge.Constraint,
			Action:     candidate.edge.Action,
			Path:       a.buildPath(candidate.edge, seen),
		})
	}

	// Report slices are capped so the response stays bounded.
	report.Deleted = capRowRefs(discoveryOrder, a.limits.MaxReportEntries, report, "deleted rows")
	report.Edges = capEdges(edges, a.limits.MaxReportEntries, report)

	a.addAdapterBoundaries(addBoundary)
	if err := a.addTriggerBoundaries(ctx, q, report, addBoundary, discoveryOrder); err != nil {
		return nil, err
	}

	return report, nil
}

// referencingRow is a scanned child row together with the evidence needed to
// apply MATCH semantics and SET NULL column reporting.
type referencingRow struct {
	ref      RowRef
	parent   RowRef
	fkValues []any
}

// queryReferencingRows runs one bounded SELECT on the caller snapshot and
// copies every matching row into owned Go values before closing the rows.
func (a *Analyzer) queryReferencingRows(
	ctx context.Context,
	q Queryer,
	report *Report,
	childTable *drivers.Table,
	childSchema string,
	constraint drivers.ForeignKeyConstraint,
	parentTuples [][]any,
) ([]referencingRow, error) {
	if childTable.PKey == nil || len(childTable.PKey.Columns) == 0 {
		return nil, fmt.Errorf("deleteimpact: table %q has no resolvable primary key", childTable.Name)
	}

	pkColumns := childTable.PKey.Columns
	fkColumns := constraint.LocalColumns()
	selectColumns := append(append([]string{}, pkColumns...), fkColumns...)

	query, args := a.buildSelectQuery(childSchema, constraint.Table, selectColumns, fkColumns, parentTuples)
	report.Stats.SQLStatements++

	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("deleteimpact: referencing rows query failed on %s: %w", constraint.Table, err)
	}
	defer rows.Close()

	var result []referencingRow

	for rows.Next() {
		report.Stats.RowsScanned++
		if report.Stats.RowsScanned > a.limits.MaxRowsScanned {
			break
		}

		dest := make([]any, len(selectColumns))
		for i := range dest {
			dest[i] = new(any)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("deleteimpact: scan failed on %s: %w", constraint.Table, err)
		}

		pkValues := make([]any, len(pkColumns))
		for i := range pkColumns {
			pkValues[i] = normalizeValue(*dest[i].(*any))
		}
		fkValues := make([]any, len(fkColumns))
		for i := range fkColumns {
			fkValues[i] = normalizeValue(*dest[len(pkColumns)+i].(*any))
		}

		if !matchesUnderMatchClause(constraint.Match, fkValues) {
			continue
		}

		parentPK := make([]any, len(constraint.Columns))
		for i := range constraint.Columns {
			// The foreign key column values are, by definition, the
			// referenced primary key values for a matching row.
			parentPK[i] = fkValues[i]
		}

		result = append(result, referencingRow{
			ref:      RowRef{Schema: childSchema, Table: constraint.Table, PK: pkValues},
			fkValues: fkValues,
			parent:   RowRef{Schema: nonEmpty(constraint.ForeignSchema, a.schema), Table: constraint.ForeignTable, PK: parentPK},
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("deleteimpact: iteration failed on %s: %w", constraint.Table, err)
	}

	return result, nil
}

// existingRows returns which of the supplied refs currently exist in the
// snapshot, using bounded tuple IN queries.
func (a *Analyzer) existingRows(
	ctx context.Context,
	q Queryer,
	report *Report,
	table *drivers.Table,
	schema string,
	refs []RowRef,
) ([]RowRef, error) {
	if table.PKey == nil || len(table.PKey.Columns) == 0 {
		return nil, fmt.Errorf("deleteimpact: table %q has no resolvable primary key", table.Name)
	}
	pkColumns := table.PKey.Columns

	var found []RowRef
	tuples := make([][]any, len(refs))
	for i, ref := range refs {
		tuples[i] = ref.PK
	}
	for _, chunk := range a.chunkTuples(tuples) {
		if a.atSQLLimit(report) {
			report.Incomplete = true
			report.Notes = append(report.Notes, fmt.Sprintf("sql statement limit of %d reached while verifying roots", a.limits.MaxSQLStatements))
			break
		}
		query, args := a.buildSelectQuery(schema, table.Name, pkColumns, pkColumns, chunk)
		report.Stats.SQLStatements++
		rows, err := q.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("deleteimpact: root existence query failed on %s: %w", table.Name, err)
		}

		for rows.Next() {
			report.Stats.RowsScanned++
			dest := make([]any, len(pkColumns))
			for i := range dest {
				dest[i] = new(any)
			}
			if err := rows.Scan(dest...); err != nil {
				rows.Close()
				return nil, fmt.Errorf("deleteimpact: scan failed on %s: %w", table.Name, err)
			}
			values := make([]any, len(pkColumns))
			for i := range dest {
				values[i] = normalizeValue(*dest[i].(*any))
			}
			found = append(found, RowRef{Schema: schema, Table: table.Name, PK: values})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("deleteimpact: iteration failed on %s: %w", table.Name, err)
		}
		rows.Close()
	}

	return found, nil
}

// unsupportedReason returns a non-empty reason when the constraint cannot be
// modeled by the analyzer.
func (a *Analyzer) unsupportedReason(constraint drivers.ForeignKeyConstraint) string {
	if !constraint.Complete || len(constraint.Columns) == 0 {
		return "incomplete composite foreign key; full ordered column correspondence is unavailable"
	}
	switch constraint.OnDelete {
	case drivers.RefActionCascade, drivers.RefActionSetNull,
		drivers.RefActionRestrict, drivers.RefActionNoAction:
	default:
		return "unsupported or unknown ON DELETE action " + string(constraint.OnDelete)
	}
	switch constraint.Match {
	case drivers.FKeyMatchSimple, drivers.FKeyMatchFull:
	default:
		return "unsupported or unknown MATCH clause " + string(constraint.Match)
	}
	return ""
}

// buildPath reconstructs the shortest root-to-blocker path from BFS parent
// pointers. firstEdge points from the blocking row to its referenced parent.
func (a *Analyzer) buildPath(firstEdge Edge, seen map[string]*nodeState) []Edge {
	path := []Edge{firstEdge}
	current := firstEdge.To
	for {
		state, ok := seen[rowKey(current)]
		if !ok || state.via == nil {
			break
		}
		path = append(path, *state.via)
		current = state.via.To
	}
	for left, right := 0, len(path)-1; left < right; left, right = left+1, right-1 {
		path[left], path[right] = path[right], path[left]
	}
	return path
}

func (a *Analyzer) atSQLLimit(report *Report) bool {
	return report.Stats.SQLStatements >= a.limits.MaxSQLStatements
}

func (a *Analyzer) atRowsLimit(report *Report) bool {
	return report.Stats.RowsScanned >= a.limits.MaxRowsScanned
}

// buildIndexes constructs table and incoming-foreign-key indexes. Constraints
// are indexed by their referenced (parent) table; tables lacking constraint
// level metadata contribute legacy single-column entries solely for unknown
// reporting.
func (a *Analyzer) buildIndexes() (map[tableIndexKey]*drivers.Table, map[tableIndexKey][]drivers.ForeignKey, map[tableIndexKey][]drivers.ForeignKeyConstraint, error) {
	tables := make(map[tableIndexKey]*drivers.Table)
	for i := range a.tables {
		table := &a.tables[i]
		tables[tableIndexKey{schema: a.schema, name: table.Name}] = table
		if a.schema != "" {
			tables[tableIndexKey{schema: "", name: table.Name}] = table
		}
	}

	incoming := make(map[tableIndexKey][]drivers.ForeignKeyConstraint)
	legacyIncoming := make(map[tableIndexKey][]drivers.ForeignKey)
	for i := range a.tables {
		table := &a.tables[i]
		childSchema := nonEmpty(table.SchemaName, a.schema)

		if len(table.FKeyConstraints) > 0 {
			for _, constraint := range table.FKeyConstraints {
				parentSchema := nonEmpty(constraint.ForeignSchema, a.schema)
				key := tableIndexKey{schema: parentSchema, name: constraint.ForeignTable}
				if constraint.Schema == "" {
					constraint.Schema = childSchema
				}
				incoming[key] = append(incoming[key], constraint)
			}
			continue
		}

		// Metadata gap: keep the relationship visible only as an unknown.
		for _, fkey := range table.FKeys {
			key := tableIndexKey{schema: a.schema, name: fkey.ForeignTable}
			legacyIncoming[key] = append(legacyIncoming[key], fkey)
		}
	}

	return tables, legacyIncoming, incoming, nil
}

// validateRoots normalizes and validates the caller supplied roots.
func (a *Analyzer) validateRoots(roots []Root, tables map[tableIndexKey]*drivers.Table) ([]RowRef, error) {
	validated := make([]RowRef, 0, len(roots))
	seen := make(map[string]bool)

	for _, root := range roots {
		schema := nonEmpty(root.Schema, a.schema)
		key := tableIndexKey{schema: schema, name: root.Table}
		table, ok := tables[key]
		if !ok {
			return nil, fmt.Errorf("deleteimpact: root table %q is not present in the supplied schema metadata", root.Table)
		}
		if table.IsView {
			return nil, fmt.Errorf("deleteimpact: root relation %q is a view; views cannot be deleted", root.Table)
		}
		if table.PKey == nil || len(table.PKey.Columns) == 0 {
			return nil, fmt.Errorf("deleteimpact: root table %q has no resolvable primary key", root.Table)
		}
		if len(root.PK) != len(table.PKey.Columns) {
			return nil, fmt.Errorf("deleteimpact: root on %q requires %d primary key values, got %d",
				root.Table, len(table.PKey.Columns), len(root.PK))
		}

		pk := make([]any, len(root.PK))
		for i, value := range root.PK {
			if value == nil {
				return nil, fmt.Errorf("deleteimpact: primary key column %q of %q cannot be NULL", table.PKey.Columns[i], root.Table)
			}
			pk[i] = canonicalValue(value)
		}

		ref := RowRef{Schema: schema, Table: root.Table, PK: pk}
		rk := rowKey(ref)
		if seen[rk] {
			continue
		}
		seen[rk] = true
		validated = append(validated, ref)
	}

	return validated, nil
}

// matchesUnderMatchClause applies the database MATCH semantics to a scanned
// foreign key tuple. A tuple containing NULL never references a parent row
// under MATCH SIMPLE (any NULL) or MATCH FULL (all or nothing); MATCH PARTIAL
// is rejected earlier as unsupported.
func matchesUnderMatchClause(match drivers.ForeignKeyMatchType, fkValues []any) bool {
	nilCount := 0
	for _, value := range fkValues {
		if value == nil {
			nilCount++
		}
	}
	switch match {
	case drivers.FKeyMatchSimple, drivers.FKeyMatchFull:
		return nilCount == 0
	default:
		return false
	}
}

func nonNullColumns(candidate actionCandidate) []string {
	var columns []string
	for i, value := range candidate.fkValues {
		if value != nil {
			columns = append(columns, candidate.columns[i])
		}
	}
	return columns
}

func groupRefs(refs []RowRef) map[tableIndexKey][]RowRef {
	groups := make(map[tableIndexKey][]RowRef)
	for _, ref := range refs {
		key := tableIndexKey{schema: ref.Schema, name: ref.Table}
		groups[key] = append(groups[key], ref)
	}
	return groups
}

func appendCandidate(candidates []actionCandidate, candidate actionCandidate) []actionCandidate {
	key := edgeKey(candidate.edge)
	for _, existing := range candidates {
		if edgeKey(existing.edge) == key {
			return candidates
		}
	}
	return append(candidates, candidate)
}

func edgeKey(edge Edge) string {
	return edge.Constraint + "\x00" + rowKey(edge.From) + "\x00" + rowKey(edge.To)
}

func rowKey(ref RowRef) string {
	var builder strings.Builder
	builder.WriteString(ref.Schema)
	builder.WriteByte(0)
	builder.WriteString(ref.Table)
	for _, value := range ref.PK {
		builder.WriteByte(0)
		switch typed := canonicalValue(value).(type) {
		case []byte:
			builder.WriteString(string(typed))
		default:
			builder.WriteString(fmt.Sprintf("%T:%v", typed, typed))
		}
	}
	return builder.String()
}

// canonicalValue coerces Go numeric families onto the types database drivers
// return (int64/float64) so caller supplied int roots and scanned int64 keys
// deduplicate identically, while the reported RowRef still carries concrete
// typed values.
func canonicalValue(value any) any {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int8:
		return int64(typed)
	case int16:
		return int64(typed)
	case int32:
		return int64(typed)
	case uint:
		return int64(typed)
	case uint8:
		return int64(typed)
	case uint16:
		return int64(typed)
	case uint32:
		return int64(typed)
	case uint64:
		return int64(typed)
	case float32:
		return float64(typed)
	default:
		return normalizeValue(value)
	}
}

func normalizeValue(value any) any {
	if typed, ok := value.([]byte); ok {
		return string(typed)
	}
	return value
}

func capRowRefs(refs []RowRef, limit int, report *Report, label string) []RowRef {
	if len(refs) <= limit {
		return refs
	}
	report.Incomplete = true
	report.Notes = append(report.Notes, fmt.Sprintf("report entry limit of %d reached while collecting %s", limit, label))
	return refs[:limit]
}

func capEdges(edges []Edge, limit int, report *Report) []Edge {
	if len(edges) <= limit {
		return edges
	}
	report.Incomplete = true
	report.Notes = append(report.Notes, fmt.Sprintf("report entry limit of %d reached while collecting edges", limit))
	return edges[:limit]
}
