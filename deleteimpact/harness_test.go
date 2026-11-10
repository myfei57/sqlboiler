package deleteimpact_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"

	"github.com/aarondl/sqlboiler/v4/deleteimpact"
	"github.com/aarondl/sqlboiler/v4/drivers"
	sqlitedriver "github.com/aarondl/sqlboiler/v4/drivers/sqlboiler-sqlite3/driver"
)

// fixtureDDL is a multi-table schema exercising every behavior the analyzer
// has to model: CASCADE chains, SET NULL, RESTRICT/NO ACTION, self loops,
// cycles, many-to-many join tables, composite primary/foreign keys, nullable
// foreign keys (MATCH SIMPLE) and a trigger whose side effects cannot be
// simulated.
var fixtureDDL = []string{
	`CREATE TABLE region (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`,
	`CREATE TABLE site (
		id INTEGER PRIMARY KEY,
		region_id INTEGER NOT NULL REFERENCES region(id) ON DELETE CASCADE
	)`,
	`CREATE TABLE device (
		id INTEGER PRIMARY KEY,
		site_id INTEGER NOT NULL REFERENCES site(id) ON DELETE CASCADE,
		code TEXT
	)`,
	`CREATE TABLE asset (
		id INTEGER PRIMARY KEY,
		device_id INTEGER REFERENCES device(id) ON DELETE SET NULL
	)`,
	`CREATE TABLE contract (
		id INTEGER PRIMARY KEY,
		region_id INTEGER NOT NULL REFERENCES region(id) ON DELETE RESTRICT
	)`,
	`CREATE TABLE invoice (
		id INTEGER PRIMARY KEY,
		contract_id INTEGER REFERENCES contract(id) ON DELETE RESTRICT
	)`,
	`CREATE TABLE employee (
		id INTEGER PRIMARY KEY,
		manager_id INTEGER REFERENCES employee(id) ON DELETE CASCADE
	)`,
	`CREATE TABLE post (
		id INTEGER PRIMARY KEY,
		author_id INTEGER NOT NULL REFERENCES employee(id) ON DELETE NO ACTION
	)`,
	`CREATE TABLE course (id INTEGER PRIMARY KEY)`,
	`CREATE TABLE student (id INTEGER PRIMARY KEY)`,
	`CREATE TABLE enrollment (
		course_id INTEGER NOT NULL REFERENCES course(id) ON DELETE CASCADE,
		student_id INTEGER NOT NULL REFERENCES student(id) ON DELETE CASCADE,
		PRIMARY KEY (course_id, student_id)
	)`,
	`CREATE TABLE tenant (id INTEGER PRIMARY KEY)`,
	`CREATE TABLE part (
		tenant_id INTEGER NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
		id INTEGER NOT NULL,
		name TEXT,
		PRIMARY KEY (tenant_id, id)
	)`,
	`CREATE TABLE bin (id INTEGER PRIMARY KEY)`,
	`CREATE TABLE inventory (
		tenant_id INTEGER NOT NULL,
		part_id INTEGER NOT NULL,
		bin_id INTEGER REFERENCES bin(id) ON DELETE SET NULL,
		PRIMARY KEY (tenant_id, part_id),
		FOREIGN KEY (tenant_id, part_id) REFERENCES part(tenant_id, id) ON DELETE CASCADE
	)`,
	`CREATE TABLE document (
		id INTEGER PRIMARY KEY,
		owner_id INTEGER REFERENCES employee(id) ON DELETE SET NULL,
		reviewer_id INTEGER REFERENCES employee(id) ON DELETE CASCADE
	)`,
	`CREATE TABLE device_audit (old_id INTEGER, note TEXT)`,
	`CREATE TRIGGER trg_device_audit AFTER DELETE ON device
	BEGIN
		INSERT INTO device_audit(old_id, note) VALUES (OLD.id, 'deleted');
	END`,
}

var fixtureData = []string{
	`INSERT INTO region (id, name) VALUES (1, 'r1'), (2, 'r2')`,
	`INSERT INTO site (id, region_id) VALUES (1, 1), (2, 1), (3, 2)`,
	`INSERT INTO device (id, site_id, code) VALUES (1, 1, 'd1'), (2, 1, 'd2'), (3, 2, 'd3')`,
	// a1 references d1 (will be SET NULL), a2 already NULL and must be ignored.
	`INSERT INTO asset (id, device_id) VALUES (1, 1), (2, NULL)`,
	`INSERT INTO contract (id, region_id) VALUES (1, 1), (2, 2)`,
	// i1 blocks contract 1; i2 has a NULL contract id and must never block.
	`INSERT INTO invoice (id, contract_id) VALUES (1, 1), (2, NULL)`,
	// e1 <- e2 <- e3 management chain; e4/e5 form a CASCADE cycle.
	`INSERT INTO employee (id, manager_id) VALUES (1, NULL), (2, 1), (3, 2), (4, 5), (5, 4)`,
	`INSERT INTO post (id, author_id) VALUES (1, 1)`,
	`INSERT INTO course (id) VALUES (1), (2)`,
	`INSERT INTO student (id) VALUES (1), (2)`,
	`INSERT INTO enrollment (course_id, student_id) VALUES (1, 1), (1, 2), (2, 1)`,
	`INSERT INTO tenant (id) VALUES (1)`,
	`INSERT INTO part (tenant_id, id, name) VALUES (1, 1, 'p1'), (1, 2, 'p2')`,
	`INSERT INTO bin (id) VALUES (1), (2)`,
	// inventory 1 points at bin 1; its row is CASCADE deleted so the SET NULL
	// is suppressed; inventory 2 already has a NULL bin id.
	`INSERT INTO inventory (tenant_id, part_id, bin_id) VALUES (1, 1, 1), (1, 2, NULL)`,
	// doc1 is both SET NULL (owner) and CASCADE (reviewer) reachable from e1;
	// doc2 is only SET NULL reachable from e1.
	`INSERT INTO document (id, owner_id, reviewer_id) VALUES (1, 2, 1), (2, 1, NULL)`,
}

type testEnv struct {
	file  string
	db    *sql.DB
	info  *drivers.DBInfo
	tables map[string]drivers.Table
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()

	dir := t.TempDir()
	file := filepath.Join(dir, "deleteimpact.sqlite")
	// foreign_keys enforcement is attached per connection through the DSN.
	dsn := "file:" + file + "?_pragma=foreign_keys(1)&_loc=UTC"
	db, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	for _, stmt := range fixtureDDL {
		_, err := db.Exec(stmt)
		require.NoError(t, err, "ddl: %s", stmt)
	}
	for _, stmt := range fixtureData {
		_, err := db.Exec(stmt)
		require.NoError(t, err, "data: %s", stmt)
	}

	// The driver opens its own read-only connection and assembles full
	// constraint level metadata, exactly like production usage. Concurrency is
	// pinned to one so metadata reads stay on a single serialized connection.
	info, err := sqlitedriver.Assemble(drivers.Config{
		"dbname":      file,
		"concurrency": 1,
	})
	require.NoError(t, err)

	tables := make(map[string]drivers.Table, len(info.Tables))
	for _, table := range info.Tables {
		tables[table.Name] = table
	}

	return &testEnv{file: file, db: db, info: info, tables: tables}
}

func (env *testEnv) analyzer() *deleteimpact.Analyzer {
	return deleteimpact.New(deleteimpact.DialectSQLite, "", env.info.Tables)
}

// readOnlyQueryer wraps the caller transaction and fails the test if anything
// other than a read statement is ever issued, proving the analyzer never
// writes and always uses the single supplied snapshot handle.
type readOnlyQueryer struct {
	t     *testing.T
	inner deleteimpact.Queryer
	seen  []string
}

var readStatement = regexp.MustCompile(`(?is)^\s*(select|pragma)\b`)

func (r *readOnlyQueryer) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if !readStatement.MatchString(query) {
		r.t.Fatalf("deleteimpact issued a non-read statement: %s", query)
	}
	r.seen = append(r.seen, query)
	return r.inner.QueryContext(ctx, query, args...)
}

// snapshotRow is one full table row keyed by column name.
type snapshotRow map[string]any

func snapshotDB(t *testing.T, db *sql.DB) map[string]map[string]snapshotRow {
	t.Helper()
	result := make(map[string]map[string]snapshotRow)

	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	require.NoError(t, err)
	var tables []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		tables = append(tables, name)
	}
	require.NoError(t, rows.Err())
	rows.Close()

	for _, table := range tables {
		rows, err := db.Query(fmt.Sprintf(`SELECT * FROM "%s"`, table))
		require.NoError(t, err)
		columns, err := rows.Columns()
		require.NoError(t, err)

		tableRows := make(map[string]snapshotRow)
		for rows.Next() {
			dest := make([]any, len(columns))
			for i := range dest {
				dest[i] = new(any)
			}
			require.NoError(t, rows.Scan(dest...))
			row := snapshotRow{}
			for i, col := range columns {
				row[col] = normalize(*dest[i].(*any))
			}
			tableRows[fullKey(table, row)] = row
		}
		require.NoError(t, rows.Err())
		rows.Close()
		result[table] = tableRows
	}

	return result
}

func normalize(value any) any {
	if bytes, ok := value.([]byte); ok {
		return string(bytes)
	}
	return value
}

// fullKey builds a deterministic row key from every column value.
func fullKey(table string, row snapshotRow) string {
	columns := make([]string, 0, len(row))
	for col := range row {
		columns = append(columns, col)
	}
	sort.Strings(columns)
	parts := []string{table}
	for _, col := range columns {
		parts = append(parts, fmt.Sprintf("%s=%v", col, row[col]))
	}
	return strings.Join(parts, "|")
}

type referenceDiff struct {
	deleted  map[string]map[string]bool
	updated  map[string]map[string][]string
	inserted map[string]int
}

// pkOnlyKey renders a snapshot row using the table's primary key columns so
// reference diffs and analyzer RowRefs share one key space.
func pkOnlyKey(t *testing.T, tables map[string]drivers.Table, table string, row snapshotRow) string {
	meta, ok := tables[table]
	if !ok || meta.PKey == nil || len(meta.PKey.Columns) == 0 {
		t.Fatalf("table %s has no primary key for reference diffing", table)
	}
	parts := []string{table}
	for _, col := range meta.PKey.Columns {
		parts = append(parts, fmt.Sprintf("%s=%v", col, row[col]))
	}
	return strings.Join(parts, "|")
}

func diffSnapshots(t *testing.T, tables map[string]drivers.Table, before, after map[string]map[string]snapshotRow) referenceDiff {
	t.Helper()
	diff := referenceDiff{
		deleted:  make(map[string]map[string]bool),
		updated:  make(map[string]map[string][]string),
		inserted: make(map[string]int),
	}

	for table, beforeRows := range before {
		afterRows := after[table]
		meta, hasPK := tables[table]
		hasPrimaryKey := hasPK && meta.PKey != nil && len(meta.PKey.Columns) > 0

		// Re-key rows by primary key when available so that an updated row is
		// not mistaken for a delete plus insert.
		beforeByPK := make(map[string]snapshotRow, len(beforeRows))
		afterByPK := make(map[string]snapshotRow, len(afterRows))
		if hasPrimaryKey {
			for _, row := range beforeRows {
				beforeByPK[pkOnlyKey(t, tables, table, row)] = row
			}
			for _, row := range afterRows {
				afterByPK[pkOnlyKey(t, tables, table, row)] = row
			}
		}

		if hasPrimaryKey {
			for key, beforeRow := range beforeByPK {
				afterRow, ok := afterByPK[key]
				if !ok {
					if diff.deleted[table] == nil {
						diff.deleted[table] = make(map[string]bool)
					}
					diff.deleted[table][key] = true
					continue
				}
				var changed []string
				for col, value := range beforeRow {
					if fmt.Sprint(value) != fmt.Sprint(afterRow[col]) {
						changed = append(changed, col)
					}
				}
				if len(changed) > 0 {
					sort.Strings(changed)
					if diff.updated[table] == nil {
						diff.updated[table] = make(map[string][]string)
					}
					diff.updated[table][key] = changed
				}
			}
			for key := range afterByPK {
				if _, ok := beforeByPK[key]; !ok {
					diff.inserted[table]++
				}
			}
			continue
		}

		// Tables without a primary key can only be compared by full row key.
		for key := range beforeRows {
			if _, ok := afterRows[key]; !ok {
				if diff.deleted[table] == nil {
					diff.deleted[table] = make(map[string]bool)
				}
				diff.deleted[table][key] = true
			}
		}
		for key := range afterRows {
			if _, ok := beforeRows[key]; !ok {
				diff.inserted[table]++
			}
		}
	}

	return diff
}

// pkKey renders the analyzer RowRef primary key the same way full snapshot
// rows are keyed, using declared primary key column order.
func refKey(table drivers.Table, pk []any) string {
	parts := []string{table.Name}
	for i, col := range table.PKey.Columns {
		parts = append(parts, fmt.Sprintf("%s=%v", col, pk[i]))
	}
	return strings.Join(parts, "|")
}

// runReferenceDelete executes the same DELETE the analyzer predicts, inside a
// transaction that is always rolled back. forceRemoval runs first and is used
// to prove RESTRICT blockers: only RESTRICT/NO ACTION descendants are removed
// manually, everything else is left for real database cascades and SET NULL.
func (env *testEnv) runReferenceDelete(t *testing.T, forceRemoval func(*sql.Tx) error, statement string, args ...any) (referenceDiff, error) {
	tx, err := env.db.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	if forceRemoval != nil {
		require.NoError(t, forceRemoval(tx))
	}

	before := snapshotInTx(t, tx)
	_, err = tx.Exec(statement, args...)
	if err != nil {
		require.NoError(t, tx.Rollback())
		return referenceDiff{}, err
	}
	after := snapshotInTx(t, tx)
	require.NoError(t, tx.Rollback())

	return diffSnapshots(t, env.tables, before, after), nil
}

func snapshotInTx(t *testing.T, tx *sql.Tx) map[string]map[string]snapshotRow {
	t.Helper()
	rows, err := tx.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	require.NoError(t, err)
	var names []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		names = append(names, name)
	}
	require.NoError(t, rows.Err())
	rows.Close()

	result := make(map[string]map[string]snapshotRow)
	for _, table := range names {
		rows, err := tx.Query(fmt.Sprintf(`SELECT * FROM "%s"`, table))
		require.NoError(t, err)
		columns, err := rows.Columns()
		require.NoError(t, err)
		tableRows := make(map[string]snapshotRow)
		for rows.Next() {
			dest := make([]any, len(columns))
			for i := range dest {
				dest[i] = new(any)
			}
			require.NoError(t, rows.Scan(dest...))
			row := snapshotRow{}
			for i, col := range columns {
				row[col] = normalize(*dest[i].(*any))
			}
			tableRows[fullKey(table, row)] = row
		}
		require.NoError(t, rows.Err())
		rows.Close()
		result[table] = tableRows
	}
	return result
}

// analyzeInSnapshot runs the analyzer inside a rolled back transaction and
// asserts the analyzer performs no writes: only SELECT/PRAGMA statements are
// allowed and a full before/after snapshot is identical after rollback.
func (env *testEnv) analyzeInSnapshot(t *testing.T, roots []deleteimpact.Root, limits *deleteimpact.Limits) *deleteimpact.Report {
	t.Helper()

	ctx := context.Background()
	before := snapshotDB(t, env.db)

	tx, err := env.db.Begin()
	require.NoError(t, err)
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	analyzer := env.analyzer()
	if limits != nil {
		analyzer = analyzer.WithLimits(*limits)
	}
	guard := &readOnlyQueryer{t: t, inner: tx}
	report, err := analyzer.Analyze(ctx, guard, roots)
	require.NoError(t, err)
	require.NotEmpty(t, guard.seen, "analyzer must query the snapshot")

	require.NoError(t, tx.Rollback())
	committed = true

	after := snapshotDB(t, env.db)
	require.Equal(t, before, after, "rollback snapshot must be byte identical")

	return report
}

func deletedKeySet(t *testing.T, env *testEnv, refs []deleteimpact.RowRef) map[string]map[string]bool {
	t.Helper()
	set := make(map[string]map[string]bool)
	for _, ref := range refs {
		table, ok := env.tables[ref.Table]
		require.True(t, ok, "unknown table %s", ref.Table)
		if set[ref.Table] == nil {
			set[ref.Table] = make(map[string]bool)
		}
		set[ref.Table][refKey(table, ref.PK)] = true
	}
	return set
}

func requireSameDeleted(t *testing.T, expected, actual map[string]map[string]bool) {
	t.Helper()
	require.Equal(t, expected, actual)
}
