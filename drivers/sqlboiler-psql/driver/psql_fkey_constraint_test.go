package driver

import (
	"fmt"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"

	"github.com/aarondl/sqlboiler/v4/drivers"
)

// TestForeignKeyConstraintInfo verifies that pg_constraint rows are grouped
// back into constraints preserving the ordered composite column
// correspondence, action codes, MATCH type and deferrability flags.
func TestForeignKeyConstraintInfo(t *testing.T) {
	t.Parallel()

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	columns := []string{
		"conname", "table_schema", "table_name", "foreign_schema", "foreign_table",
		"column_name", "foreign_column_name",
		"confdeltype", "confupdtype", "confmatchtype",
		"condeferrable", "condeferred", "ordinality",
	}
	rows := sqlmock.NewRows(columns).
		AddRow("fk_composite", "public", "inventory", "public", "part",
			"tenant_id", "tenant_id", "c", "a", "s", true, false, int64(1)).
		AddRow("fk_composite", "public", "inventory", "public", "part",
			"part_id", "id", "c", "a", "s", true, false, int64(2)).
		AddRow("fk_setnull", "public", "asset", "public", "device",
			"device_id", "id", "n", "c", "f", false, false, int64(1))
	mock.ExpectQuery(`select`).WillReturnRows(rows)

	driver := &PostgresDriver{conn: db}
	constraints, err := driver.ForeignKeyConstraintInfo("public", "inventory")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
	require.Len(t, constraints, 2)

	composite := constraints[0]
	require.Equal(t, "fk_composite", composite.Name)
	require.Equal(t, "inventory", composite.Table)
	require.Equal(t, "public", composite.Schema)
	require.Equal(t, "part", composite.ForeignTable)
	require.Equal(t, "public", composite.ForeignSchema)
	require.Equal(t, drivers.RefActionCascade, composite.OnDelete)
	require.Equal(t, drivers.RefActionNoAction, composite.OnUpdate)
	require.Equal(t, drivers.FKeyMatchSimple, composite.Match)
	require.True(t, composite.Deferrable)
	require.False(t, composite.InitiallyDeferred)
	require.True(t, composite.Complete)
	require.Equal(t, []drivers.ForeignKeyColumnPair{
		{Column: "tenant_id", ForeignColumn: "tenant_id"},
		{Column: "part_id", ForeignColumn: "id"},
	}, composite.Columns)

	setNull := constraints[1]
	require.Equal(t, drivers.RefActionSetNull, setNull.OnDelete)
	require.Equal(t, drivers.RefActionCascade, setNull.OnUpdate)
	require.Equal(t, drivers.FKeyMatchFull, setNull.Match)
	require.False(t, setNull.Deferrable)
	require.Len(t, setNull.Columns, 1)
}

// TestForeignKeyConstraintInfo_RowsErr mirrors the rows.Err propagation
// guarantees of the other driver methods.
func TestForeignKeyConstraintInfo_RowsErr(t *testing.T) {
	t.Parallel()

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	rows := sqlmock.NewRows([]string{
		"conname", "table_schema", "table_name", "foreign_schema", "foreign_table",
		"column_name", "foreign_column_name",
		"confdeltype", "confupdtype", "confmatchtype",
		"condeferrable", "condeferred", "ordinality",
	}).
		AddRow("fk", "public", "child", "public", "parent",
			"parent_id", "id", "r", "a", "s", false, false, int64(1)).
		RowError(0, fmt.Errorf("connection reset by peer"))
	mock.ExpectQuery(`select`).WillReturnRows(rows)

	driver := &PostgresDriver{conn: db}
	_, err = driver.ForeignKeyConstraintInfo("public", "child")
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}
