package drivers

import "strings"

// ReferentialAction mirrors the ON DELETE / ON UPDATE referential action
// declared by a foreign key constraint.
type ReferentialAction string

const (
	// RefActionUnknown is returned when an adapter cannot determine the
	// declared action, for example an unsupported dialect specific encoding.
	RefActionUnknown ReferentialAction = "UNKNOWN"
	// RefActionCascade is ON DELETE/UPDATE CASCADE.
	RefActionCascade ReferentialAction = "CASCADE"
	// RefActionSetNull is ON DELETE/UPDATE SET NULL.
	RefActionSetNull ReferentialAction = "SET NULL"
	// RefActionSetDefault is ON DELETE/UPDATE SET DEFAULT.
	RefActionSetDefault ReferentialAction = "SET DEFAULT"
	// RefActionRestrict is ON DELETE/UPDATE RESTRICT.
	RefActionRestrict ReferentialAction = "RESTRICT"
	// RefActionNoAction is ON DELETE/UPDATE NO ACTION.
	RefActionNoAction ReferentialAction = "NO ACTION"
)

// ForeignKeyMatchType mirrors the MATCH clause of a foreign key constraint.
type ForeignKeyMatchType string

const (
	// FKeyMatchUnknown means the adapter could not resolve the MATCH clause.
	FKeyMatchUnknown ForeignKeyMatchType = "UNKNOWN"
	// FKeyMatchSimple is MATCH SIMPLE (the SQL default): a referencing row is
	// not checked as soon as any of its foreign key columns is NULL.
	FKeyMatchSimple ForeignKeyMatchType = "SIMPLE"
	// FKeyMatchFull is MATCH FULL: either all foreign key columns are NULL or
	// all of them must match a referenced row.
	FKeyMatchFull ForeignKeyMatchType = "FULL"
	// FKeyMatchPartial is MATCH PARTIAL: the non-NULL foreign key columns must
	// match. Most databases do not implement it.
	FKeyMatchPartial ForeignKeyMatchType = "PARTIAL"
)

// ForeignKeyColumnPair maps one local column to the referenced column at the
// same ordinal position of a foreign key constraint.
type ForeignKeyColumnPair struct {
	Column        string `json:"column"`
	ForeignColumn string `json:"foreign_column"`
}

// ForeignKeyConstraint retains constraint level foreign key metadata that the
// single column ForeignKey type cannot represent: the complete ordered column
// correspondence (including composite keys), the referential actions, the
// MATCH clause and deferrability.
//
// Consumers must never reconstruct composite relationships or actions from
// ForeignKey values: when Complete is false, or the adapter did not populate
// this metadata at all, the constraint has to be treated as unknown.
type ForeignKeyConstraint struct {
	Name   string `json:"name"`
	Table  string `json:"table"`
	Schema string `json:"schema,omitempty"`

	ForeignTable  string `json:"foreign_table"`
	ForeignSchema string `json:"foreign_schema,omitempty"`

	// Columns holds the complete ordered correspondence between local and
	// referenced columns, sorted by ordinal position inside the constraint.
	Columns []ForeignKeyColumnPair `json:"columns"`

	OnDelete ReferentialAction   `json:"on_delete"`
	OnUpdate ReferentialAction   `json:"on_update"`
	Match    ForeignKeyMatchType `json:"match"`

	// Deferrable reports whether the constraint is DEFERRABLE.
	Deferrable bool `json:"deferrable"`
	// InitiallyDeferred reports whether the constraint is INITIALLY DEFERRED.
	InitiallyDeferred bool `json:"initially_deferred"`

	// Complete is false when the full column correspondence could not be
	// resolved (for example because whitelist/blacklist filtering removed one
	// of the columns). Such constraints must be reported as unknown instead of
	// being guessed from single column metadata.
	Complete bool `json:"complete"`
}

// LocalColumns returns the ordered local column names of the constraint.
func (f ForeignKeyConstraint) LocalColumns() []string {
	cols := make([]string, len(f.Columns))
	for i, pair := range f.Columns {
		cols[i] = pair.Column
	}
	return cols
}

// ForeignColumns returns the ordered referenced column names of the constraint.
func (f ForeignKeyConstraint) ForeignColumns() []string {
	cols := make([]string, len(f.Columns))
	for i, pair := range f.Columns {
		cols[i] = pair.ForeignColumn
	}
	return cols
}

// ParseReferentialAction normalizes a textual referential action as reported
// by a database (for example "CASCADE", "SET NULL", "NO ACTION"). Empty or
// unrecognized values resolve to RefActionUnknown.
func ParseReferentialAction(raw string) ReferentialAction {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "CASCADE":
		return RefActionCascade
	case "SET NULL":
		return RefActionSetNull
	case "SET DEFAULT":
		return RefActionSetDefault
	case "RESTRICT":
		return RefActionRestrict
	case "NO ACTION":
		return RefActionNoAction
	default:
		return RefActionUnknown
	}
}

// ParsePostgresActionCode converts the one character action codes stored in
// pg_constraint.confdeltype / confupdtype:
//
//	a = NO ACTION, r = RESTRICT, c = CASCADE, n = SET NULL, d = SET DEFAULT.
func ParsePostgresActionCode(code byte) ReferentialAction {
	switch code {
	case 'a':
		return RefActionNoAction
	case 'r':
		return RefActionRestrict
	case 'c':
		return RefActionCascade
	case 'n':
		return RefActionSetNull
	case 'd':
		return RefActionSetDefault
	default:
		return RefActionUnknown
	}
}

// ParsePostgresMatchCode converts the pg_constraint.confmatchtype code:
//
//	s = SIMPLE, f = FULL, p = PARTIAL.
func ParsePostgresMatchCode(code byte) ForeignKeyMatchType {
	switch code {
	case 's':
		return FKeyMatchSimple
	case 'f':
		return FKeyMatchFull
	case 'p':
		return FKeyMatchPartial
	default:
		return FKeyMatchUnknown
	}
}

// ParseSQLiteMatch normalizes the MATCH value returned by
// PRAGMA foreign_key_list. SQLite parses the MATCH clause but does not enforce
// it, so the empty string and the conventional "NONE" value map to the
// effective MATCH SIMPLE behavior.
func ParseSQLiteMatch(raw string) ForeignKeyMatchType {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "", "NONE":
		return FKeyMatchSimple
	case "SIMPLE":
		return FKeyMatchSimple
	case "FULL":
		return FKeyMatchFull
	case "PARTIAL":
		return FKeyMatchPartial
	default:
		return FKeyMatchUnknown
	}
}

// ForeignKeyConstraintConstructor is implemented by drivers that are able to
// provide complete constraint level foreign key metadata.
type ForeignKeyConstraintConstructor interface {
	ForeignKeyConstraintInfo(schema, tableName string) ([]ForeignKeyConstraint, error)
}

// filterForeignKeyConstraints applies whitelist/blacklist semantics to
// constraint level foreign keys. Instead of dropping a partially filtered
// constraint, it is kept but flagged incomplete so consumers report it as
// unknown rather than silently ignoring part of a composite key.
func filterForeignKeyConstraints(t *Table, whitelist, blacklist []string) {
	if len(t.FKeyConstraints) == 0 {
		return
	}

	filtered := make([]ForeignKeyConstraint, 0, len(t.FKeyConstraints))
	for _, constraint := range t.FKeyConstraints {
		complete := len(constraint.Columns) > 0
		for _, pair := range constraint.Columns {
			if !knownColumn(constraint.Table, pair.Column, whitelist, blacklist) ||
				!knownColumn(constraint.ForeignTable, pair.ForeignColumn, whitelist, blacklist) {
				complete = false
			}
		}
		constraint.Complete = constraint.Complete && complete
		filtered = append(filtered, constraint)
	}
	t.FKeyConstraints = filtered
}
