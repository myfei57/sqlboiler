package boilingcore

import (
	"testing"

	"github.com/aarondl/sqlboiler/v4/drivers"
)

func TestCloneGraphStrategy(t *testing.T) {
	t.Parallel()

	adapters := map[string]struct{}{"custom.Widget": {}}

	tests := []struct {
		name    string
		column  drivers.Column
		want    string
		wantErr bool
	}{
		{name: "primitive", column: drivers.Column{Name: "id", Type: "int"}, want: cloneStrategyValue},
		{name: "nullable scalar wrapper", column: drivers.Column{Name: "n", Type: "null.String", Nullable: true}, want: cloneStrategyValue},
		{name: "null time", column: drivers.Column{Name: "at", Type: "null.Time"}, want: cloneStrategyValue},
		{name: "time", column: drivers.Column{Name: "at", Type: "time.Time"}, want: cloneStrategyValue},
		{name: "pgeo value", column: drivers.Column{Name: "p", Type: "pgeo.Point"}, want: cloneStrategyValue},
		{name: "pgeo null value", column: drivers.Column{Name: "p", Type: "pgeo.NullCircle"}, want: cloneStrategyValue},
		{name: "mssql uuid", column: drivers.Column{Name: "u", Type: "mssql.UniqueIdentifier"}, want: cloneStrategyValue},
		{name: "enum", column: drivers.Column{Name: "e", Type: "MyEnum", DBType: "enum('a','b')"}, want: cloneStrategyValue},
		{name: "bytes", column: drivers.Column{Name: "b", Type: "[]byte"}, want: cloneStrategyBytes},
		{name: "json", column: drivers.Column{Name: "j", Type: "types.JSON"}, want: cloneStrategyJSON},
		{name: "null bytes", column: drivers.Column{Name: "b", Type: "null.Bytes"}, want: cloneStrategyNullBytes},
		{name: "null json", column: drivers.Column{Name: "j", Type: "null.JSON"}, want: cloneStrategyNullJSON},
		{name: "hstore", column: drivers.Column{Name: "h", Type: "types.HStore"}, want: cloneStrategyHStore},
		{name: "int64 array", column: drivers.Column{Name: "a", Type: "types.Int64Array"}, want: cloneStrategySlice},
		{name: "string array", column: drivers.Column{Name: "a", Type: "types.StringArray"}, want: cloneStrategySlice},
		{name: "bytes array", column: drivers.Column{Name: "a", Type: "types.BytesArray"}, want: cloneStrategyBytesArray},
		{name: "decimal", column: drivers.Column{Name: "d", Type: "types.Decimal"}, want: cloneStrategyDecimal},
		{name: "null decimal", column: drivers.Column{Name: "d", Type: "types.NullDecimal"}, want: cloneStrategyNullDecimal},
		{name: "decimal array", column: drivers.Column{Name: "d", Type: "types.DecimalArray"}, want: cloneStrategyDecimalArray},
		{name: "path", column: drivers.Column{Name: "p", Type: "pgeo.Path"}, want: cloneStrategyPath},
		{name: "null path", column: drivers.Column{Name: "p", Type: "pgeo.NullPath"}, want: cloneStrategyNullPath},
		{name: "polygon", column: drivers.Column{Name: "p", Type: "pgeo.Polygon"}, want: cloneStrategyPolygon},
		{name: "null polygon", column: drivers.Column{Name: "p", Type: "pgeo.NullPolygon"}, want: cloneStrategyNullPolygon},
		{name: "registered adapter", column: drivers.Column{Name: "w", Type: "custom.Widget"}, want: cloneStrategyAdapter},
		{name: "unknown external type", column: drivers.Column{Name: "w", Type: "big.Int"}, wantErr: true},
		{name: "unknown replaced type", column: drivers.Column{Name: "w", Type: "custom.Secret"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := cloneGraphStrategy(tt.column, adapters)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %s, got strategy %q", tt.column.Type, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %s", err)
			}
			if got != tt.want {
				t.Fatalf("strategy = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateCloneGraph(t *testing.T) {
	t.Parallel()

	tables := []drivers.Table{
		{
			Name: "ok",
			Columns: []drivers.Column{
				{Name: "id", Type: "int"},
				{Name: "doc", Type: "types.JSON"},
			},
		},
		{
			Name: "join_table",
			// Join tables have no generated model struct and are skipped.
			IsJoinTable: true,
			Columns:     []drivers.Column{{Name: "id", Type: "mystery.Type"}},
		},
		{
			Name: "bad",
			Columns: []drivers.Column{
				{Name: "id", Type: "int"},
				{Name: "widget", Type: "external.Widget"},
			},
		},
	}

	if err := validateCloneGraph(tables, map[string]struct{}{}); err == nil {
		t.Fatal("expected validation to reject the unknown column type")
	}

	if err := validateCloneGraph(tables, map[string]struct{}{"external.Widget": {}}); err != nil {
		t.Fatalf("validation should pass with a registered adapter: %s", err)
	}
}
