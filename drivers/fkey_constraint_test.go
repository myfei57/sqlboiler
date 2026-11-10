package drivers

import "testing"

func TestParseReferentialAction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw  string
		want ReferentialAction
	}{
		{"CASCADE", RefActionCascade},
		{" cascade ", RefActionCascade},
		{"set null", RefActionSetNull},
		{"SET DEFAULT", RefActionSetDefault},
		{"RESTRICT", RefActionRestrict},
		{"NO ACTION", RefActionNoAction},
		{"", RefActionUnknown},
		{"MAGIC", RefActionUnknown},
	}

	for _, tt := range tests {
		if got := ParseReferentialAction(tt.raw); got != tt.want {
			t.Errorf("ParseReferentialAction(%q) = %q, want %q", tt.raw, got, tt.want)
		}
	}
}

func TestParsePostgresActionCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		code byte
		want ReferentialAction
	}{
		{'a', RefActionNoAction},
		{'r', RefActionRestrict},
		{'c', RefActionCascade},
		{'n', RefActionSetNull},
		{'d', RefActionSetDefault},
		{'x', RefActionUnknown},
	}

	for _, tt := range tests {
		if got := ParsePostgresActionCode(tt.code); got != tt.want {
			t.Errorf("ParsePostgresActionCode(%q) = %q, want %q", tt.code, got, tt.want)
		}
	}
}

func TestParsePostgresMatchCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		code byte
		want ForeignKeyMatchType
	}{
		{'s', FKeyMatchSimple},
		{'f', FKeyMatchFull},
		{'p', FKeyMatchPartial},
		{'?', FKeyMatchUnknown},
	}

	for _, tt := range tests {
		if got := ParsePostgresMatchCode(tt.code); got != tt.want {
			t.Errorf("ParsePostgresMatchCode(%q) = %q, want %q", tt.code, got, tt.want)
		}
	}
}

func TestParseSQLiteMatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw  string
		want ForeignKeyMatchType
	}{
		{"", FKeyMatchSimple},
		{"NONE", FKeyMatchSimple},
		{"simple", FKeyMatchSimple},
		{"FULL", FKeyMatchFull},
		{"PARTIAL", FKeyMatchPartial},
		{"WEIRD", FKeyMatchUnknown},
	}

	for _, tt := range tests {
		if got := ParseSQLiteMatch(tt.raw); got != tt.want {
			t.Errorf("ParseSQLiteMatch(%q) = %q, want %q", tt.raw, got, tt.want)
		}
	}
}

func TestForeignKeyConstraintColumns(t *testing.T) {
	t.Parallel()

	constraint := ForeignKeyConstraint{
		Columns: []ForeignKeyColumnPair{
			{Column: "a", ForeignColumn: "xa"},
			{Column: "b", ForeignColumn: "xb"},
		},
	}

	if got, want := constraint.LocalColumns(), []string{"a", "b"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("LocalColumns = %v, want %v", got, want)
	}
	if got, want := constraint.ForeignColumns(), []string{"xa", "xb"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("ForeignColumns = %v, want %v", got, want)
	}
}

func TestFilterForeignKeyConstraints(t *testing.T) {
	t.Parallel()

	table := &Table{
		Name: "child",
		FKeyConstraints: []ForeignKeyConstraint{
			{
				Name:     "complete_fk",
				Table:    "child",
				Complete: true,
				Columns: []ForeignKeyColumnPair{
					{Column: "parent_id", ForeignColumn: "id"},
				},
			},
			{
				Name:     "composite_fk",
				Table:    "child",
				Complete: true,
				Columns: []ForeignKeyColumnPair{
					{Column: "tenant_id", ForeignColumn: "tenant_id"},
					{Column: "parent_id", ForeignColumn: "id"},
				},
			},
		},
	}

	// Blacklisting one column of the composite key must flag it incomplete
	// instead of dropping or silently truncating it.
	filterForeignKeyConstraints(table, nil, []string{"child.tenant_id"})

	if !table.FKeyConstraints[0].Complete {
		t.Error("single column constraint should stay complete")
	}
	if table.FKeyConstraints[1].Complete {
		t.Error("composite constraint with a filtered column must be flagged incomplete")
	}
	if len(table.FKeyConstraints[1].Columns) != 2 {
		t.Error("incomplete composite constraint must retain its full column correspondence")
	}
}
