package boilingcore

import (
	"fmt"

	"github.com/aarondl/sqlboiler/v4/drivers"
)

// Clone graph column copy strategies emitted by the CloneGraph templates.
//
// The strategy names are part of the template contract: the per-table
// CloneGraph template switches on them to render the exact copy expression
// for every column. They describe how a value is copied, never whether it is
// loaded: a nil pointer or a nil slice keeps its nil identity and an empty
// non-nil slice stays empty and non-nil.
const (
	// cloneStrategyValue is a plain struct/value assignment. The type is fully
	// composed of scalar, safely copyable fields (primitives, null.* scalar
	// wrappers, time.Time, fixed-array/value geometry types, generated enums).
	cloneStrategyValue = "value"
	// cloneStrategyBytes deep-copies a []byte while preserving nil.
	cloneStrategyBytes = "bytes"
	// cloneStrategyJSON deep-copies a types.JSON (a defined []byte).
	cloneStrategyJSON = "json"
	// cloneStrategyNullBytes copies a null.Bytes wrapper and deep-copies its
	// inner byte slice, keeping Valid and the nil/empty distinction intact.
	cloneStrategyNullBytes = "nullbytes"
	// cloneStrategyNullJSON copies a null.JSON wrapper and deep-copies its
	// inner byte slice, keeping Valid and the nil/empty distinction intact.
	cloneStrategyNullJSON = "nulljson"
	// cloneStrategyHStore deep-copies a types.HStore map.
	cloneStrategyHStore = "hstore"
	// cloneStrategySlice deep-copies a slice of scalar values
	// (types.Int64Array, types.BoolArray, types.Float64Array,
	// types.StringArray).
	cloneStrategySlice = "slice"
	// cloneStrategyBytesArray deep-copies a types.BytesArray including every
	// inner []byte.
	cloneStrategyBytesArray = "bytesarray"
	// cloneStrategyDecimal deep-copies a types.Decimal and its *decimal.Big.
	cloneStrategyDecimal = "decimal"
	// cloneStrategyNullDecimal deep-copies a types.NullDecimal and its
	// *decimal.Big.
	cloneStrategyNullDecimal = "nulldecimal"
	// cloneStrategyDecimalArray deep-copies a types.DecimalArray and every
	// decimal element it holds.
	cloneStrategyDecimalArray = "decimalarray"
	// cloneStrategyPath deep-copies a pgeo.Path and its Points slice.
	cloneStrategyPath = "path"
	// cloneStrategyNullPath deep-copies a pgeo.NullPath and its Points slice.
	cloneStrategyNullPath = "nullpath"
	// cloneStrategyPolygon deep-copies a pgeo.Polygon point slice.
	cloneStrategyPolygon = "polygon"
	// cloneStrategyNullPolygon deep-copies a pgeo.NullPolygon point slice.
	cloneStrategyNullPolygon = "nullpolygon"
	// cloneStrategyAdapter means the column type is unknown to the generator
	// and the generated code must call a user-registered CloneGraph adapter.
	cloneStrategyAdapter = "adapter"
)

// cloneGraphValueTypes need nothing more than a struct assignment: every
// field they contain is a scalar, a safely copyable value (time.Time, fixed
// size arrays, value structs of scalars) or an immutable shared object (the
// *time.Location inside time.Time, which is globally treated as read-only).
var cloneGraphValueTypes = map[string]struct{}{
	// Go primitives
	"int": {}, "int8": {}, "int16": {}, "int32": {}, "int64": {},
	"uint": {}, "uint8": {}, "uint16": {}, "uint32": {}, "uint64": {},
	"float32": {}, "float64": {},
	"bool": {}, "string": {}, "byte": {}, "rune": {},

	// sqlboiler scalar wrapper
	"types.Byte": {},

	// null/v8 wrappers made only of scalar fields (including null.Time,
	// which wraps a value time.Time plus a Valid bool)
	"null.Int": {}, "null.Int8": {}, "null.Int16": {}, "null.Int32": {}, "null.Int64": {},
	"null.Uint": {}, "null.Uint8": {}, "null.Uint16": {}, "null.Uint32": {}, "null.Uint64": {},
	"null.Float32": {}, "null.Float64": {},
	"null.Bool": {}, "null.String": {}, "null.Byte": {}, "null.Time": {},

	// value time
	"time.Time": {},

	// pgeo value types: fixed arrays ([2]Point) or structs made exclusively
	// of float64/value Point fields. Path and Polygon hold slices and are
	// handled separately.
	"pgeo.Point": {}, "pgeo.NullPoint": {},
	"pgeo.Line": {}, "pgeo.NullLine": {},
	"pgeo.Lseg": {}, "pgeo.NullLseg": {},
	"pgeo.Box": {}, "pgeo.NullBox": {},
	"pgeo.Circle": {}, "pgeo.NullCircle": {},

	// mssql unique identifier is a fixed [16]byte array
	"mssql.UniqueIdentifier": {},
}

// cloneGraphStrategy maps a single model column to its copy strategy.
//
// Enum columns always resolve to value copies: generated enum types are
// string-based (or a struct of a string-based value and a Valid bool for the
// nullable variants), so they carry no mutable state.
//
// A type that is not recognized and has no registered adapter is rejected at
// generation time instead of being shallow-copied with a false promise of
// independence: it may hide private mutable state, as is the case for
// math/big.Int or similar external types.
func cloneGraphStrategy(c drivers.Column, adapters map[string]struct{}) (string, error) {
	if drivers.IsEnumDBType(c.DBType) {
		return cloneStrategyValue, nil
	}

	if _, ok := cloneGraphValueTypes[c.Type]; ok {
		return cloneStrategyValue, nil
	}

	switch c.Type {
	case "[]byte":
		return cloneStrategyBytes, nil
	case "types.JSON":
		return cloneStrategyJSON, nil
	case "null.Bytes":
		return cloneStrategyNullBytes, nil
	case "null.JSON":
		return cloneStrategyNullJSON, nil
	case "types.HStore":
		return cloneStrategyHStore, nil
	case "types.Int64Array", "types.BoolArray", "types.Float64Array", "types.StringArray":
		return cloneStrategySlice, nil
	case "types.BytesArray":
		return cloneStrategyBytesArray, nil
	case "types.Decimal":
		return cloneStrategyDecimal, nil
	case "types.NullDecimal":
		return cloneStrategyNullDecimal, nil
	case "types.DecimalArray":
		return cloneStrategyDecimalArray, nil
	case "pgeo.Path":
		return cloneStrategyPath, nil
	case "pgeo.NullPath":
		return cloneStrategyNullPath, nil
	case "pgeo.Polygon":
		return cloneStrategyPolygon, nil
	case "pgeo.NullPolygon":
		return cloneStrategyNullPolygon, nil
	}

	if _, ok := adapters[c.Type]; ok {
		return cloneStrategyAdapter, nil
	}

	return "", fmt.Errorf(
		"clone_graph: column %q of type %q has no known deep-copy strategy; "+
			"register a CloneGraph adapter for the type (clone_graph_adapters) "+
			"or remove the column from generation",
		c.Name, c.Type,
	)
}

// validateCloneGraph makes sure every column of every generated table can be
// safely deep-copied by the generated CloneGraph API. It runs before any file
// is emitted, so an unsupported type aborts generation instead of producing
// code that silently shares mutable memory.
func validateCloneGraph(tables []drivers.Table, adapters map[string]struct{}) error {
	for _, t := range tables {
		if t.IsJoinTable {
			continue
		}
		for _, c := range t.Columns {
			if _, err := cloneGraphStrategy(c, adapters); err != nil {
				return fmt.Errorf("table %q: %w", t.Name, err)
			}
		}
	}

	return nil
}

// CloneGraphStrategy is exposed to templates through templateData so the
// per-table CloneGraph template can render the correct copy expression for
// each column.
func (t templateData) CloneGraphStrategy(c drivers.Column) (string, error) {
	return cloneGraphStrategy(c, t.CloneGraphAdapters)
}
