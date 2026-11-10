package driver

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"

	"github.com/aarondl/sqlboiler/v4/drivers"
)

// TestForeignKeyConstraintInfoSQLite exercises PRAGMA foreign_key_list grouping
// against a real (pure Go, in-process) SQLite database so no external sqlite3
// CLI is required. It covers composite keys, every supported action and the
// effective MATCH SIMPLE mapping.
func TestForeignKeyConstraintInfoSQLite(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "fkeys.sqlite") + "?_loc=UTC"
	db, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	defer db.Close()

	ddl := []string{
		`CREATE TABLE tenant (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE part (
			tenant_id INTEGER NOT NULL,
			id INTEGER NOT NULL,
			PRIMARY KEY (tenant_id, id),
			FOREIGN KEY (tenant_id) REFERENCES tenant(id) ON DELETE CASCADE
		)`,
		`CREATE TABLE bin (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE inventory (
			tenant_id INTEGER NOT NULL,
			part_id INTEGER NOT NULL,
			bin_id INTEGER,
			PRIMARY KEY (tenant_id, part_id),
			FOREIGN KEY (tenant_id, part_id) REFERENCES part(tenant_id, id) ON DELETE RESTRICT,
			FOREIGN KEY (bin_id) REFERENCES bin(id) ON DELETE SET NULL ON UPDATE CASCADE
		)`,
		`CREATE TABLE node (
			id INTEGER PRIMARY KEY,
			parent_id INTEGER REFERENCES node(id) ON DELETE SET DEFAULT
		)`,
	}
	for _, stmt := range ddl {
		_, err := db.Exec(stmt)
		require.NoError(t, err)
	}

	driver := SQLiteDriver{dbConn: db}

	inventory, err := driver.ForeignKeyConstraintInfo("", "inventory")
	require.NoError(t, err)
	require.Len(t, inventory, 2)

	// PRAGMA id ordering is adapter specific, so locate constraints by their
	// referenced table instead of assuming slice positions.
	restrict := findConstraint(t, inventory, "part")
	require.Equal(t, "inventory", restrict.Table)
	require.Equal(t, drivers.RefActionRestrict, restrict.OnDelete)
	require.Equal(t, drivers.RefActionNoAction, restrict.OnUpdate)
	require.Equal(t, drivers.FKeyMatchSimple, restrict.Match)
	require.True(t, restrict.Complete)
	require.False(t, restrict.Deferrable)
	require.Equal(t, []drivers.ForeignKeyColumnPair{
		{Column: "tenant_id", ForeignColumn: "tenant_id"},
		{Column: "part_id", ForeignColumn: "id"},
	}, restrict.Columns, "composite column correspondence must stay ordered and complete")

	setNull := findConstraint(t, inventory, "bin")
	require.Equal(t, drivers.RefActionSetNull, setNull.OnDelete)
	require.Equal(t, drivers.RefActionCascade, setNull.OnUpdate)
	require.Equal(t, []drivers.ForeignKeyColumnPair{
		{Column: "bin_id", ForeignColumn: "id"},
	}, setNull.Columns)

	node, err := driver.ForeignKeyConstraintInfo("", "node")
	require.NoError(t, err)
	require.Len(t, node, 1)
	require.Equal(t, drivers.RefActionSetDefault, node[0].OnDelete)

	// A table without foreign keys returns an empty result without error.
	tenant, err := driver.ForeignKeyConstraintInfo("", "tenant")
	require.NoError(t, err)
	require.Empty(t, tenant)
}

func findConstraint(t *testing.T, constraints []drivers.ForeignKeyConstraint, foreignTable string) drivers.ForeignKeyConstraint {
	t.Helper()
	for _, constraint := range constraints {
		if constraint.ForeignTable == foreignTable {
			return constraint
		}
	}
	t.Fatalf("constraint referencing %q not found in %+v", foreignTable, constraints)
	return drivers.ForeignKeyConstraint{}
}
