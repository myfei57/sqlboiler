package deleteimpact

import (
	"testing"

	"github.com/aarondl/sqlboiler/v4/drivers"
)

func newTestAnalyzer(dialect Dialect) *Analyzer {
	return New(dialect, "public", nil)
}

func TestRenderInTuplePostgres(t *testing.T) {
	t.Parallel()

	t.Run("single column", func(t *testing.T) {
		a := newTestAnalyzer(DialectPostgres)
		expr, args, next := a.renderInTuple([]string{"id"}, [][]any{{1}, {2}}, 1)
		want := `"id" IN ($1,$2)`
		if expr != want {
			t.Fatalf("expr = %q, want %q", expr, want)
		}
		if len(args) != 2 || args[0] != 1 || args[1] != 2 {
			t.Fatalf("args = %v", args)
		}
		if next != 3 {
			t.Fatalf("next param = %d, want 3", next)
		}
	})

	t.Run("composite tuple", func(t *testing.T) {
		a := newTestAnalyzer(DialectPostgres)
		expr, args, next := a.renderInTuple(
			[]string{"tenant_id", "id"},
			[][]any{{1, 10}, {2, 20}},
			4,
		)
		want := `("tenant_id","id") IN (($4,$5),($6,$7))`
		if expr != want {
			t.Fatalf("expr = %q, want %q", expr, want)
		}
		if len(args) != 4 || next != 8 {
			t.Fatalf("args = %v next = %d", args, next)
		}
	})

	t.Run("schema qualified select", func(t *testing.T) {
		a := newTestAnalyzer(DialectPostgres)
		query, args := a.buildSelectQuery(
			"public", "site",
			[]string{"id", "region_id"},
			[]string{"region_id"},
			[][]any{{7}},
		)
		want := `SELECT "id","region_id" FROM "public"."site" WHERE "region_id" IN ($1)`
		if query != want {
			t.Fatalf("query = %q, want %q", query, want)
		}
		if len(args) != 1 || args[0] != 7 {
			t.Fatalf("args = %v", args)
		}
	})
}

func TestRenderInTupleSQLite(t *testing.T) {
	t.Parallel()

	t.Run("single column", func(t *testing.T) {
		a := newTestAnalyzer(DialectSQLite)
		expr, args, _ := a.renderInTuple([]string{"id"}, [][]any{{1}, {2}}, 1)
		want := `"id" IN (?,?)`
		if expr != want {
			t.Fatalf("expr = %q, want %q", expr, want)
		}
		if len(args) != 2 {
			t.Fatalf("args = %v", args)
		}
	})

	t.Run("composite tuple uses VALUES", func(t *testing.T) {
		a := newTestAnalyzer(DialectSQLite)
		expr, _, _ := a.renderInTuple(
			[]string{"tenant_id", "part_id"},
			[][]any{{1, 1}, {1, 2}},
			1,
		)
		want := `("tenant_id","part_id") IN (VALUES (?,?),(?,?))`
		if expr != want {
			t.Fatalf("expr = %q, want %q", expr, want)
		}
	})

	t.Run("unqualified table", func(t *testing.T) {
		a := New(DialectSQLite, "", nil)
		query, _ := a.buildSelectQuery("", "site", []string{"id"}, []string{"id"}, [][]any{{1}})
		want := `SELECT "id" FROM "site" WHERE "id" IN (?)`
		if query != want {
			t.Fatalf("query = %q, want %q", query, want)
		}
	})
}

func TestChunkTuples(t *testing.T) {
	t.Parallel()

	a := newTestAnalyzer(DialectSQLite)
	limits := DefaultLimits()
	limits.MaxTuplesPerStatement = 2
	a = a.WithLimits(limits)

	tuples := [][]any{{1}, {2}, {3}, {4}, {5}}
	chunks := a.chunkTuples(tuples)
	if len(chunks) != 3 {
		t.Fatalf("chunks = %d, want 3", len(chunks))
	}
	if len(chunks[0]) != 2 || len(chunks[1]) != 2 || len(chunks[2]) != 1 {
		t.Fatalf("unexpected chunk sizes: %+v", chunks)
	}

	// Zero/negative configured size falls back to the default.
	zero := a.WithLimits(Limits{})
	if got := zero.chunkTuples(tuples); len(got) != 1 {
		t.Fatalf("default chunking failed, got %d chunks", len(got))
	}
}

func TestWithLimitsZeroValueFallsBack(t *testing.T) {
	t.Parallel()

	a := newTestAnalyzer(DialectSQLite).WithLimits(Limits{})
	if a.limits.MaxNodes != DefaultLimits().MaxNodes || a.limits.MaxEdges != DefaultLimits().MaxEdges {
		t.Fatalf("zero limits should fall back to defaults, got %+v", a.limits)
	}
}

func TestMatchesUnderMatchClause(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		match    drivers.ForeignKeyMatchType
		values   []any
		wantMatch bool
	}{
		{"simple all not null", drivers.FKeyMatchSimple, []any{int64(1), "x"}, true},
		{"simple any null skips", drivers.FKeyMatchSimple, []any{nil, "x"}, false},
		{"simple all null skips", drivers.FKeyMatchSimple, []any{nil, nil}, false},
		{"full all not null", drivers.FKeyMatchFull, []any{int64(1)}, true},
		{"full all null skips", drivers.FKeyMatchFull, []any{nil}, false},
		{"full partial null skips", drivers.FKeyMatchFull, []any{int64(1), nil}, false},
		{"partial unsupported", drivers.FKeyMatchPartial, []any{int64(1), nil}, false},
		{"unknown unsupported", drivers.FKeyMatchUnknown, []any{int64(1)}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchesUnderMatchClause(tt.match, tt.values); got != tt.wantMatch {
				t.Fatalf("matchesUnderMatchClause = %v, want %v", got, tt.wantMatch)
			}
		})
	}
}

func TestCanonicalValue(t *testing.T) {
	t.Parallel()

	if canonicalValue(int(7)) != int64(7) {
		t.Error("int should canonicalize to int64")
	}
	if canonicalValue(int32(7)) != int64(7) {
		t.Error("int32 should canonicalize to int64")
	}
	if canonicalValue(uint16(7)) != int64(7) {
		t.Error("uint16 should canonicalize to int64")
	}
	if canonicalValue(float32(1.5)) != float64(1.5) {
		t.Error("float32 should canonicalize to float64")
	}
	if canonicalValue("x") != "x" {
		t.Error("strings should pass through")
	}
	if canonicalValue([]byte("x")) != "x" {
		t.Error("[]byte should canonicalize to string")
	}
}

func TestRowKeyDedupAcrossNumericKinds(t *testing.T) {
	t.Parallel()

	fromCaller := RowRef{Table: "t", PK: []any{int(1)}}
	fromScan := RowRef{Table: "t", PK: []any{int64(1)}}
	if rowKey(fromCaller) != rowKey(fromScan) {
		t.Fatal("int and int64 primary keys must deduplicate to the same row key")
	}
}

func TestUnsupportedReason(t *testing.T) {
	t.Parallel()

	a := newTestAnalyzer(DialectPostgres)

	complete := drivers.ForeignKeyConstraint{
		Complete: true,
		OnDelete: drivers.RefActionCascade,
		Match:    drivers.FKeyMatchSimple,
		Columns:  []drivers.ForeignKeyColumnPair{{Column: "a", ForeignColumn: "id"}},
	}
	if reason := a.unsupportedReason(complete); reason != "" {
		t.Fatalf("supported constraint flagged: %s", reason)
	}

	incomplete := complete
	incomplete.Complete = false
	if reason := a.unsupportedReason(incomplete); reason == "" {
		t.Error("incomplete constraint must be unsupported")
	}

	setDefault := complete
	setDefault.OnDelete = drivers.RefActionSetDefault
	if reason := a.unsupportedReason(setDefault); reason == "" {
		t.Error("SET DEFAULT must be unsupported")
	}

	partial := complete
	partial.Match = drivers.FKeyMatchPartial
	if reason := a.unsupportedReason(partial); reason == "" {
		t.Error("MATCH PARTIAL must be unsupported")
	}
}
