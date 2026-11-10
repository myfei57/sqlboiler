// Package deleteimpact provides an optional, read-only analyzer that predicts
// the database level impact of deleting a finite, explicitly supplied set of
// root rows.
//
// The analyzer reasons only about foreign key constraints declared in the
// database (CASCADE, SET NULL, RESTRICT, NO ACTION) and the rows visible in
// the caller's read-only snapshot transaction. It never executes a DELETE and
// it never treats Go relationship slices as the full set of database
// relationships: every traversal step is backed by a SELECT issued through the
// caller's executor.
//
// Adapters, actions or composite keys that cannot be resolved completely are
// reported as unknown instead of being guessed from single column metadata.
// Deferred constraints and trigger side effects cannot be fully simulated;
// the report therefore carries explicit evidence boundaries rather than
// claiming to predict arbitrary trigger behavior.
package deleteimpact

import (
	"context"
	"database/sql"

	"github.com/aarondl/sqlboiler/v4/drivers"
)

// Dialect identifies a database dialect whose referential semantics the
// analyzer knows how to model.
type Dialect string

const (
	// DialectPostgres models PostgreSQL foreign key semantics.
	DialectPostgres Dialect = "postgres"
	// DialectSQLite models SQLite foreign key semantics.
	DialectSQLite Dialect = "sqlite3"
)

// Queryer is the single read-only snapshot the analyzer is allowed to use.
// In practice it is a *sql.Tx (or another transactional handle) opened by the
// caller. The analyzer never opens a connection of its own, so every read is
// consistent with the caller's snapshot instead of observing the latest
// committed state across multiple connections.
type Queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Root identifies one root row to hypothetically delete. PK holds the full,
// typed primary key value, ordered exactly like the table's primary key
// columns (including every column of a composite key).
type Root struct {
	// Schema is optional. When empty the analyzer default schema is used.
	Schema string
	Table  string
	PK     []any
}

// RowRef identifies a concrete row by schema, table and full typed primary key.
type RowRef struct {
	Schema string `json:"schema"`
	Table  string `json:"table"`
	PK     []any  `json:"pk"`
}

// Edge is one observed foreign key relationship between a referencing row
// (From) and a referenced row that belongs to the candidate deletion closure
// (To).
type Edge struct {
	Constraint string                      `json:"constraint"`
	From       RowRef                      `json:"from"`
	To         RowRef                      `json:"to"`
	Action     drivers.ReferentialAction   `json:"action"`
}

// NulledColumn describes a row whose foreign key columns would be set to NULL
// by a SET NULL action. Only columns that are currently non-NULL are listed.
type NulledColumn struct {
	Row        RowRef   `json:"row"`
	Constraint string   `json:"constraint"`
	Columns    []string `json:"columns"`
}

// Blocker describes a referencing row that prevents the hypothetical delete
// because of a RESTRICT (or immediate NO ACTION) constraint. Path holds the
// shortest chain of edges from one of the caller supplied roots down to the
// blocking row.
type Blocker struct {
	Row        RowRef                    `json:"row"`
	Constraint string                    `json:"constraint"`
	Action     drivers.ReferentialAction `json:"action"`
	Path       []Edge                    `json:"path"`
}

// Unknown records a relationship that could not be analyzed, for example an
// adapter without constraint level metadata, an unsupported action such as
// SET DEFAULT, or an incomplete composite key. The analyzer never guesses
// these from single column ForeignKey metadata.
type Unknown struct {
	Schema     string `json:"schema"`
	Table      string `json:"table"`
	Constraint string `json:"constraint"`
	Reason     string `json:"reason"`
}

// Boundary is an explicit statement about what the analysis cannot prove,
// such as deferred constraint timing or trigger/rule side effects.
type Boundary struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

// Stats counters collected while building the report.
type Stats struct {
	SQLStatements int `json:"sql_statements"`
	RowsScanned   int `json:"rows_scanned"`
	Nodes         int `json:"nodes"`
	Edges         int `json:"edges"`
}

// Limits bounds the work and the report size. When any limit is reached the
// analysis stops cleanly and the report is flagged incomplete.
type Limits struct {
	// MaxSQLStatements bounds the number of queries issued on the snapshot.
	MaxSQLStatements int
	// MaxRowsScanned bounds the total number of rows scanned across queries.
	MaxRowsScanned int
	// MaxNodes bounds distinct rows held in the candidate deletion closure.
	MaxNodes int
	// MaxEdges bounds distinct traversed relationship edges.
	MaxEdges int
	// MaxReportEntries bounds each report slice (deleted rows, edges, ...).
	MaxReportEntries int
	// MaxTuplesPerStatement bounds the number of primary key tuples encoded
	// into a single IN query.
	MaxTuplesPerStatement int
}

// DefaultLimits returns conservative defaults suitable for most schemas.
func DefaultLimits() Limits {
	return Limits{
		MaxSQLStatements:      1000,
		MaxRowsScanned:        100_000,
		MaxNodes:              100_000,
		MaxEdges:              200_000,
		MaxReportEntries:      10_000,
		MaxTuplesPerStatement: 400,
	}
}

// Report is the owned result of an analysis. All values are copied out of the
// database rows before the underlying *sql.Rows are closed.
type Report struct {
	Dialect Dialect `json:"dialect"`
	Schema  string  `json:"schema"`

	// Roots echoes the caller supplied roots that were found in the snapshot.
	Roots []RowRef `json:"roots"`
	// NotFoundRoots lists roots whose row did not exist in the snapshot.
	NotFoundRoots []RowRef `json:"not_found_roots"`

	// Deleted is the candidate deletion closure: every row that would be
	// deleted, including cascaded rows. It is deduplicated and ordered by
	// discovery. When Blockers is non-empty the real delete would fail, but
	// Deleted still describes the full hypothetical closure for planning
	// purposes; nothing is ever deleted by the analyzer.
	Deleted []RowRef `json:"deleted"`
	// NulledColumns lists the SET NULL updates that would occur, excluding
	// rows already belonging to the deletion closure.
	NulledColumns []NulledColumn `json:"nulled_columns"`
	// Blockers lists the RESTRICT / NO ACTION conflicts with shortest paths.
	// A non-empty Blockers slice means the hypothetical delete cannot commit
	// unless the blocking rows (or their constraints) are handled first.
	Blockers []Blocker `json:"blockers"`
	// Edges is the deduplicated relationship evidence traversed.
	Edges []Edge `json:"edges"`
	// Unknowns lists relationships that could not be modeled.
	Unknowns []Unknown `json:"unknowns"`
	// Boundaries lists the evidence boundaries of the analysis.
	Boundaries []Boundary `json:"boundaries"`

	// Incomplete is true when a limit was hit or the analysis had to stop
	// before reaching a fixed point. Notes explains why.
	Incomplete bool     `json:"incomplete"`
	Notes      []string `json:"notes"`

	Stats Stats `json:"stats"`
}

// Analyzer performs delete impact analyses against snapshot transactions.
// It is safe for repeated use; each Analyze call is independent and read-only.
type Analyzer struct {
	dialect Dialect
	schema  string
	tables  []drivers.Table
	limits  Limits
}

// New builds an Analyzer for the given dialect. Schema is the default schema
// used for unqualified table names (use "" for SQLite). tables must carry
// constraint level foreign key metadata populated by a supporting driver;
// tables without it are reported as unknown during analysis.
func New(dialect Dialect, schema string, tables []drivers.Table) *Analyzer {
	return &Analyzer{
		dialect: dialect,
		schema:  schema,
		tables:  tables,
		limits:  DefaultLimits(),
	}
}

// WithLimits returns a copy of the analyzer using the supplied limits. Zero
// valued fields fall back to the corresponding default so a partially filled
// Limits struct cannot accidentally disable traversal.
func (a *Analyzer) WithLimits(limits Limits) *Analyzer {
	copy := *a
	defaults := DefaultLimits()
	if limits.MaxSQLStatements <= 0 {
		limits.MaxSQLStatements = defaults.MaxSQLStatements
	}
	if limits.MaxRowsScanned <= 0 {
		limits.MaxRowsScanned = defaults.MaxRowsScanned
	}
	if limits.MaxNodes <= 0 {
		limits.MaxNodes = defaults.MaxNodes
	}
	if limits.MaxEdges <= 0 {
		limits.MaxEdges = defaults.MaxEdges
	}
	if limits.MaxReportEntries <= 0 {
		limits.MaxReportEntries = defaults.MaxReportEntries
	}
	if limits.MaxTuplesPerStatement <= 0 {
		limits.MaxTuplesPerStatement = defaults.MaxTuplesPerStatement
	}
	copy.limits = limits
	return &copy
}

// Boundary kind constants.
const (
	BoundaryTrigger            = "trigger"
	BoundaryDeferredConstraint = "deferred-constraint"
	BoundaryAdapter            = "adapter"
)
