package deleteimpact_test

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/aarondl/sqlboiler/v4/deleteimpact"
	"github.com/aarondl/sqlboiler/v4/drivers"
)

// TestAnalyze_CascadeChainAndSetNull compares the analyzer report against a
// real, rolled back DELETE for a successful CASCADE + SET NULL scenario, and
// verifies trigger boundaries plus zero-write behavior.
func TestAnalyze_CascadeChainAndSetNull(t *testing.T) {
	env := newTestEnv(t)

	report := env.analyzeInSnapshot(t, []deleteimpact.Root{
		{Table: "site", PK: []any{1}},
	}, nil)

	require.False(t, report.Incomplete, "notes: %v", report.Notes)
	require.Empty(t, report.Blockers)
	require.Empty(t, report.Unknowns)

	expectedDeleted := map[string]map[string]bool{
		"site":   {"site|id=1": true},
		"device": {"device|id=1": true, "device|id=2": true},
	}
	requireSameDeleted(t, expectedDeleted, deletedKeySet(t, env, report.Deleted))

	require.Len(t, report.NulledColumns, 1)
	nulled := report.NulledColumns[0]
	require.Equal(t, "asset", nulled.Row.Table)
	require.Equal(t, []any{int64(1)}, nulled.Row.PK)
	require.Equal(t, []string{"device_id"}, nulled.Columns)

	// The already-NULL asset row must not be reported.
	for _, entry := range report.NulledColumns {
		require.NotEqual(t, int64(2), entry.Row.PK[0])
	}

	// Reference execution: the database really performs the same delete in an
	// independent, rolled back transaction.
	diff, err := env.runReferenceDelete(t, nil, `DELETE FROM site WHERE id = 1`)
	require.NoError(t, err)
	requireSameDeleted(t, expectedDeleted, diff.deleted)
	require.Equal(t, map[string]map[string][]string{
		"asset": {"asset|id=1": {"device_id"}},
	}, diff.updated)

	// The trigger really fires on the reference delete but the analyzer only
	// reports it as an evidence boundary instead of predicting its effect.
	require.Equal(t, 2, diff.inserted["device_audit"])
	boundaries := boundaryDetails(report)
	require.Contains(t, boundaries, "trigger \"trg_device_audit\" on table \"device\" is not simulated; its side effects on delete are outside this analysis")
	require.True(t, hasBoundaryKind(report, deleteimpact.BoundaryAdapter))
}

// TestAnalyze_RestrictBlockerAndShortestPath covers RESTRICT conflicts, the
// shortest blocking path, NULL foreign keys (MATCH SIMPLE) and a full
// blocker-removal proof against real database enforcement.
func TestAnalyze_RestrictBlockerAndShortestPath(t *testing.T) {
	env := newTestEnv(t)

	report := env.analyzeInSnapshot(t, []deleteimpact.Root{
		{Table: "region", PK: []any{1}},
	}, nil)

	require.False(t, report.Incomplete, "notes: %v", report.Notes)
	require.Len(t, report.Blockers, 1)
	blocker := report.Blockers[0]
	require.Equal(t, "contract", blocker.Row.Table)
	require.Equal(t, []any{int64(1)}, blocker.Row.PK)
	require.Equal(t, drivers.RefActionRestrict, blocker.Action)
	require.Len(t, blocker.Path, 1)
	require.Equal(t, "region", blocker.Path[0].To.Table)
	require.Equal(t, "contract", blocker.Path[0].From.Table)

	// The full hypothetical closure is still enumerated despite the blocker.
	expectedDeleted := map[string]map[string]bool{
		"region": {"region|id=1": true},
		"site":   {"site|id=1": true, "site|id=2": true},
		"device": {"device|id=1": true, "device|id=2": true, "device|id=3": true},
	}
	requireSameDeleted(t, expectedDeleted, deletedKeySet(t, env, report.Deleted))
	require.Len(t, report.NulledColumns, 1)

	// A plain real delete must fail with a foreign key error.
	_, err := env.runReferenceDelete(t, nil, `DELETE FROM region WHERE id = 1`)
	require.Error(t, err)
	require.Contains(t, strings.ToLower(err.Error()), "foreign key")

	// Removing exactly the RESTRICT descendants (invoice 1, contract 1) makes
	// the real delete succeed; the remaining cascade/set-null effects must then
	// match the analyzer closure one for one.
	force := func(tx *sql.Tx) error {
		return env.forceRemoveRestrictDescendants(t, tx, "region", []any{1})
	}
	diff, err := env.runReferenceDelete(t, force, `DELETE FROM region WHERE id = 1`)
	require.NoError(t, err)

	forceRemoved := map[string]map[string]bool{
		"invoice":  {"invoice|id=1": true},
		"contract": {"contract|id=1": true},
	}
	actualClosure := subtractSets(diff.deleted, forceRemoved)
	requireSameDeleted(t, expectedDeleted, actualClosure)
	require.Equal(t, map[string]map[string][]string{
		"asset": {"asset|id=1": {"device_id"}},
	}, diff.updated)

	// Rows unrelated to the root are untouched by the reference transaction.
	require.NotContains(t, diff.deleted, "contract 2 rows must survive")
	require.NotContains(t, diff.deleted["contract"], "contract|id=2")
}

// TestAnalyze_NullForeignKeyDoesNotBlock proves MATCH SIMPLE behavior: a
// referencing row whose foreign key is NULL is invisible to RESTRICT.
func TestAnalyze_NullForeignKeyDoesNotBlock(t *testing.T) {
	env := newTestEnv(t)

	report := env.analyzeInSnapshot(t, []deleteimpact.Root{
		{Table: "contract", PK: []any{2}},
	}, nil)

	require.Empty(t, report.Blockers)
	require.Equal(t, map[string]map[string]bool{
		"contract": {"contract|id=2": true},
	}, deletedKeySet(t, env, report.Deleted))

	diff, err := env.runReferenceDelete(t, nil, `DELETE FROM contract WHERE id = 2`)
	require.NoError(t, err)
	require.Equal(t, map[string]map[string]bool{
		"contract": {"contract|id=2": true},
	}, diff.deleted)
	require.Empty(t, diff.updated)
}

// TestAnalyze_SelfReferenceCycleAndNoAction covers self loops, CASCADE cycles,
// NO ACTION blockers and the closure rule where a SET NULL on an already
// cascaded row is suppressed.
func TestAnalyze_SelfReferenceCycleAndNoAction(t *testing.T) {
	env := newTestEnv(t)

	report := env.analyzeInSnapshot(t, []deleteimpact.Root{
		{Table: "employee", PK: []any{1}},
	}, nil)

	require.False(t, report.Incomplete, "notes: %v", report.Notes)
	require.Len(t, report.Blockers, 1)
	require.Equal(t, "post", report.Blockers[0].Row.Table)
	require.Equal(t, drivers.RefActionNoAction, report.Blockers[0].Action)
	require.Len(t, report.Blockers[0].Path, 1)

	expectedDeleted := map[string]map[string]bool{
		"employee": {
			"employee|id=1": true,
			"employee|id=2": true,
			"employee|id=3": true,
		},
		"document": {"document|id=1": true},
	}
	requireSameDeleted(t, expectedDeleted, deletedKeySet(t, env, report.Deleted))

	// doc1 is deleted via the reviewer CASCADE, so its owner SET NULL is
	// suppressed; doc2 survives and gets owner_id nulled instead.
	var nulledTables []string
	for _, entry := range report.NulledColumns {
		nulledTables = append(nulledTables, entry.Row.Table)
		require.NotEqual(t, int64(1), entry.Row.PK[0], "cascaded document must not be nulled")
	}
	require.Equal(t, []string{"document"}, nulledTables)
	require.Equal(t, []string{"owner_id"}, report.NulledColumns[0].Columns)
	require.Equal(t, []any{int64(2)}, report.NulledColumns[0].Row.PK)

	// Real delete fails on the NO ACTION post.
	_, err := env.runReferenceDelete(t, nil, `DELETE FROM employee WHERE id = 1`)
	require.Error(t, err)

	force := func(tx *sql.Tx) error {
		return env.forceRemoveRestrictDescendants(t, tx, "employee", []any{1})
	}
	diff, err := env.runReferenceDelete(t, force, `DELETE FROM employee WHERE id = 1`)
	require.NoError(t, err)

	forceRemoved := map[string]map[string]bool{
		"post": {"post|id=1": true},
	}
	requireSameDeleted(t, expectedDeleted, subtractSets(diff.deleted, forceRemoved))
	require.Equal(t, map[string]map[string][]string{
		"document": {"document|id=2": {"owner_id"}},
	}, diff.updated)

	// The standalone CASCADE cycle e4/e5 deletes both rows in either order.
	cycleReport := env.analyzeInSnapshot(t, []deleteimpact.Root{
		{Table: "employee", PK: []any{4}},
	}, nil)
	require.Empty(t, cycleReport.Blockers)
	require.Equal(t, map[string]map[string]bool{
		"employee": {
			"employee|id=4": true,
			"employee|id=5": true,
		},
	}, deletedKeySet(t, env, cycleReport.Deleted))

	cycleDiff, err := env.runReferenceDelete(t, nil, `DELETE FROM employee WHERE id = 4`)
	require.NoError(t, err)
	require.Equal(t, map[string]map[string]bool{
		"employee": {
			"employee|id=4": true,
			"employee|id=5": true,
		},
	}, cycleDiff.deleted)
}

// TestAnalyze_ManyToMany checks the join table with a composite primary key
// and two CASCADE foreign keys from both sides.
func TestAnalyze_ManyToMany(t *testing.T) {
	env := newTestEnv(t)

	courseReport := env.analyzeInSnapshot(t, []deleteimpact.Root{
		{Table: "course", PK: []any{1}},
	}, nil)
	require.Empty(t, courseReport.Blockers)
	require.Equal(t, map[string]map[string]bool{
		"course":     {"course|id=1": true},
		"enrollment": {
			"enrollment|course_id=1|student_id=1": true,
			"enrollment|course_id=1|student_id=2": true,
		},
	}, deletedKeySet(t, env, courseReport.Deleted))

	courseDiff, err := env.runReferenceDelete(t, nil, `DELETE FROM course WHERE id = 1`)
	require.NoError(t, err)
	require.Equal(t, courseReport.Deleted != nil, true)
	requireSameDeleted(t, deletedKeySet(t, env, courseReport.Deleted), courseDiff.deleted)

	studentReport := env.analyzeInSnapshot(t, []deleteimpact.Root{
		{Table: "student", PK: []any{1}},
	}, nil)
	require.Equal(t, map[string]map[string]bool{
		"student":    {"student|id=1": true},
		"enrollment": {
			"enrollment|course_id=1|student_id=1": true,
			"enrollment|course_id=2|student_id=1": true,
		},
	}, deletedKeySet(t, env, studentReport.Deleted))

	// Composite primary keys are carried as ordered typed values.
	for _, ref := range studentReport.Deleted {
		if ref.Table == "enrollment" {
			require.Len(t, ref.PK, 2)
			require.Equal(t, "int64", fmt.Sprintf("%T", ref.PK[0]))
		}
	}

	studentDiff, err := env.runReferenceDelete(t, nil, `DELETE FROM student WHERE id = 1`)
	require.NoError(t, err)
	requireSameDeleted(t, deletedKeySet(t, env, studentReport.Deleted), studentDiff.deleted)
}

// TestAnalyze_CompositeForeignKey covers composite primary/foreign keys and
// the rule that a SET NULL is suppressed when the row is cascade deleted.
func TestAnalyze_CompositeForeignKey(t *testing.T) {
	env := newTestEnv(t)

	partReport := env.analyzeInSnapshot(t, []deleteimpact.Root{
		{Table: "part", PK: []any{int64(1), int64(1)}},
	}, nil)
	require.Empty(t, partReport.Blockers)
	require.Empty(t, partReport.Unknowns)
	require.Equal(t, map[string]map[string]bool{
		"part":      {"part|tenant_id=1|id=1": true},
		"inventory": {"inventory|tenant_id=1|part_id=1": true},
	}, deletedKeySet(t, env, partReport.Deleted))
	require.Empty(t, partReport.NulledColumns, "cascade deleted rows must not be SET NULL")

	partDiff, err := env.runReferenceDelete(t, nil,
		`DELETE FROM part WHERE tenant_id = 1 AND id = 1`)
	require.NoError(t, err)
	requireSameDeleted(t, deletedKeySet(t, env, partReport.Deleted), partDiff.deleted)
	require.Empty(t, partDiff.updated, "bin_id is never nulled because the row disappears")

	tenantReport := env.analyzeInSnapshot(t, []deleteimpact.Root{
		{Table: "tenant", PK: []any{1}},
	}, nil)
	require.Len(t, tenantReport.Deleted, 5) // tenant + 2 parts + 2 inventories
	require.Empty(t, tenantReport.NulledColumns)

	tenantDiff, err := env.runReferenceDelete(t, nil, `DELETE FROM tenant WHERE id = 1`)
	require.NoError(t, err)
	requireSameDeleted(t, deletedKeySet(t, env, tenantReport.Deleted), tenantDiff.deleted)
}

// TestAnalyze_UnknownMetadata ensures unsupported adapters, incomplete
// composite keys and unsupported actions are reported as unknown instead of
// being guessed from single column metadata.
func TestAnalyze_UnknownMetadata(t *testing.T) {
	env := newTestEnv(t)

	// Adapter without constraint level metadata: only the root is known and
	// every relationship becomes unknown rather than guessed.
	legacyTables := cloneTables(env.info.Tables, func(table *drivers.Table) {
		table.FKeyConstraints = nil
	})
	analyzer := deleteimpact.New(deleteimpact.DialectSQLite, "", legacyTables)
	tx, err := env.db.Begin()
	require.NoError(t, err)
	report, err := analyzer.Analyze(t.Context(), tx, []deleteimpact.Root{
		{Table: "site", PK: []any{1}},
	})
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())

	require.Equal(t, map[string]map[string]bool{
		"site": {"site|id=1": true},
	}, deletedKeySet(t, env, report.Deleted))
	require.NotEmpty(t, report.Unknowns)
	for _, unknown := range report.Unknowns {
		require.Contains(t, unknown.Reason, "constraint level foreign key metadata")
	}
	require.Empty(t, report.Edges, "no relationship may be traversed without constraint metadata")

	// Incomplete composite key: explicit unknown, no guessing.
	incompleteTables := cloneTables(env.info.Tables, func(table *drivers.Table) {
		if table.Name != "inventory" {
			return
		}
		for i := range table.FKeyConstraints {
			if len(table.FKeyConstraints[i].Columns) > 1 {
				table.FKeyConstraints[i].Complete = false
			}
		}
	})
	incompleteAnalyzer := deleteimpact.New(deleteimpact.DialectSQLite, "", incompleteTables)
	tx2, err := env.db.Begin()
	require.NoError(t, err)
	incompleteReport, err := incompleteAnalyzer.Analyze(t.Context(), tx2, []deleteimpact.Root{
		{Table: "part", PK: []any{int64(1), int64(1)}},
	})
	require.NoError(t, err)
	require.NoError(t, tx2.Rollback())
	require.True(t, hasUnknown(incompleteReport, "incomplete composite foreign key"))
	require.Equal(t, map[string]map[string]bool{
		"part": {"part|tenant_id=1|id=1": true},
	}, deletedKeySet(t, env, incompleteReport.Deleted))

	// SET DEFAULT is an explicitly unsupported action.
	setDefaultTables := cloneTables(env.info.Tables, func(table *drivers.Table) {
		if table.Name != "asset" {
			return
		}
		for i := range table.FKeyConstraints {
			table.FKeyConstraints[i].OnDelete = drivers.RefActionSetDefault
		}
	})
	setDefaultAnalyzer := deleteimpact.New(deleteimpact.DialectSQLite, "", setDefaultTables)
	tx3, err := env.db.Begin()
	require.NoError(t, err)
	setDefaultReport, err := setDefaultAnalyzer.Analyze(t.Context(), tx3, []deleteimpact.Root{
		{Table: "device", PK: []any{1}},
	})
	require.NoError(t, err)
	require.NoError(t, tx3.Rollback())
	require.True(t, hasUnknown(setDefaultReport, "SET DEFAULT"))
}

// TestAnalyze_LimitsFlagIncomplete verifies every breached limit produces an
// explicit incomplete report instead of silently truncated results.
func TestAnalyze_LimitsFlagIncomplete(t *testing.T) {
	env := newTestEnv(t)

	limits := deleteimpact.DefaultLimits()
	limits.MaxNodes = 1
	report := env.analyzeInSnapshot(t, []deleteimpact.Root{
		{Table: "region", PK: []any{1}},
	}, &limits)

	require.True(t, report.Incomplete)
	require.Contains(t, strings.Join(report.Notes, "\n"), "node limit of 1 reached")
	require.Len(t, report.Deleted, 1)
}

// TestAnalyze_NotFoundRoot reports missing roots without inventing rows.
func TestAnalyze_NotFoundRoot(t *testing.T) {
	env := newTestEnv(t)

	report := env.analyzeInSnapshot(t, []deleteimpact.Root{
		{Table: "region", PK: []any{999}},
	}, nil)

	require.Len(t, report.NotFoundRoots, 1)
	require.Equal(t, "region", report.NotFoundRoots[0].Table)
	require.Empty(t, report.Deleted)
	require.Empty(t, report.Edges)
}

// TestAnalyze_RootValidation rejects malformed caller input up front.
func TestAnalyze_RootValidation(t *testing.T) {
	env := newTestEnv(t)
	analyzer := env.analyzer()
	tx, err := env.db.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	_, err = analyzer.Analyze(t.Context(), tx, []deleteimpact.Root{
		{Table: "part", PK: []any{1}}, // composite PK needs two values
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "primary key values")

	_, err = analyzer.Analyze(t.Context(), tx, []deleteimpact.Root{
		{Table: "does_not_exist", PK: []any{1}},
	})
	require.Error(t, err)
}

func boundaryDetails(report *deleteimpact.Report) []string {
	details := make([]string, 0, len(report.Boundaries))
	for _, boundary := range report.Boundaries {
		details = append(details, boundary.Detail)
	}
	return details
}

func hasBoundaryKind(report *deleteimpact.Report, kind string) bool {
	for _, boundary := range report.Boundaries {
		if boundary.Kind == kind {
			return true
		}
	}
	return false
}

func hasUnknown(report *deleteimpact.Report, fragment string) bool {
	for _, unknown := range report.Unknowns {
		if strings.Contains(unknown.Reason, fragment) {
			return true
		}
	}
	return false
}

func subtractSets(actual, removed map[string]map[string]bool) map[string]map[string]bool {
	result := make(map[string]map[string]bool)
	for table, keys := range actual {
		for key := range keys {
			if removeTable, ok := removed[table]; ok && removeTable[key] {
				continue
			}
			if result[table] == nil {
				result[table] = make(map[string]bool)
			}
			result[table][key] = true
		}
	}
	return result
}

func cloneTables(tables []drivers.Table, mutate func(*drivers.Table)) []drivers.Table {
	clone := make([]drivers.Table, len(tables))
	for i, table := range tables {
		duplicate := table
		duplicate.FKeyConstraints = append([]drivers.ForeignKeyConstraint(nil), table.FKeyConstraints...)
		for j := range duplicate.FKeyConstraints {
			duplicate.FKeyConstraints[j].Columns =
				append([]drivers.ForeignKeyColumnPair(nil), table.FKeyConstraints[j].Columns...)
		}
		duplicate.FKeys = append([]drivers.ForeignKey(nil), table.FKeys...)
		clone[i] = duplicate
	}
	for i := range clone {
		mutate(&clone[i])
	}
	return clone
}

// forceRemoveRestrictDescendants deletes every RESTRICT/NO ACTION descendant
// of the given root row (recursively) but never the root itself, so the
// subsequent real root DELETE still natively exercises CASCADE and SET NULL.
func (env *testEnv) forceRemoveRestrictDescendants(t *testing.T, tx *sql.Tx, table string, pk []any) error {
	return env.forceRemove(t, tx, table, pk, map[string]bool{}, false)
}

// forceRemove deletes the given row and every RESTRICT/NO ACTION descendant
// that would otherwise block its deletion, using real DELETE statements under
// live foreign key enforcement. CASCADE and SET NULL descendants are left
// untouched so the subsequent root DELETE still exercises them natively. When
// deleteSelf is false only descendants are removed.
func (env *testEnv) forceRemove(t *testing.T, tx *sql.Tx, table string, pk []any, visited map[string]bool, deleteSelf bool) error {
	meta, ok := env.tables[table]
	if !ok {
		return fmt.Errorf("unknown table %s", table)
	}
	visitKey := table + "|" + joinPK(meta, pk)
	if visited[visitKey] {
		return nil
	}
	visited[visitKey] = true

	for _, child := range env.info.Tables {
		for _, constraint := range child.FKeyConstraints {
			if constraint.ForeignTable != table {
				continue
			}
			if constraint.OnDelete != drivers.RefActionRestrict &&
				constraint.OnDelete != drivers.RefActionNoAction {
				continue
			}

			fkColumns := constraint.LocalColumns()
			where := make([]string, len(fkColumns))
			args := make([]any, len(fkColumns))
			for i, col := range fkColumns {
				where[i] = fmt.Sprintf("%s = ?", quote(col))
				args[i] = pk[i]
			}
			query := fmt.Sprintf(`SELECT %s FROM %s WHERE %s`,
				strings.Join(quoteAll(child.PKey.Columns), ","),
				quote(child.Name),
				strings.Join(where, " AND "))

			rows, err := tx.Query(query, args...)
			if err != nil {
				return err
			}
			var childPKs [][]any
			for rows.Next() {
				dest := make([]any, len(child.PKey.Columns))
				for i := range dest {
					dest[i] = new(any)
				}
				if err := rows.Scan(dest...); err != nil {
					rows.Close()
					return err
				}
				values := make([]any, len(dest))
				for i := range dest {
					values[i] = normalize(*dest[i].(*any))
				}
				childPKs = append(childPKs, values)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return err
			}
			rows.Close()

			for _, childPK := range childPKs {
				if err := env.forceRemove(t, tx, child.Name, childPK, visited, true); err != nil {
					return err
				}
			}
		}
	}

	if !deleteSelf {
		return nil
	}

	where := make([]string, len(meta.PKey.Columns))
	args := make([]any, len(pk))
	for i, col := range meta.PKey.Columns {
		where[i] = fmt.Sprintf("%s = ?", quote(col))
		args[i] = pk[i]
	}
	_, err := tx.Exec(fmt.Sprintf(`DELETE FROM %s WHERE %s`, quote(table), strings.Join(where, " AND ")), args...)
	return err
}

func joinPK(table drivers.Table, pk []any) string {
	parts := make([]string, len(pk))
	for i := range pk {
		parts[i] = fmt.Sprint(pk[i])
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func quote(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func quoteAll(names []string) []string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = quote(name)
	}
	return quoted
}
