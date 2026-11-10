package types

import "github.com/ericlagergren/decimal"

// This file holds the deep-copy primitives used by the generated CloneGraph
// API. The decimal types wrap a *decimal.Big pointer, so a struct assignment
// would share mutable arbitrary-precision state between the source graph and
// its clone; these helpers allocate independent backing values instead.

// CloneDecimal returns an independently mutable copy of d. A nil embedded
// *decimal.Big stays nil, so the unloaded/null state is preserved rather than
// being replaced by a zero-but-non-nil decimal.
func CloneDecimal(d Decimal) Decimal {
	if d.Big == nil {
		return Decimal{}
	}

	return Decimal{Big: new(decimal.Big).Copy(d.Big)}
}

// CloneNullDecimal returns an independently mutable copy of d. A nil embedded
// *decimal.Big stays nil, so the null state is preserved.
func CloneNullDecimal(d NullDecimal) NullDecimal {
	if d.Big == nil {
		return NullDecimal{}
	}

	return NullDecimal{Big: new(decimal.Big).Copy(d.Big)}
}

// CloneDecimalArray returns an independently mutable copy of a, including a
// private *decimal.Big allocation for every element. A nil slice stays nil
// and an empty non-nil slice stays empty and non-nil.
func CloneDecimalArray(a DecimalArray) DecimalArray {
	if a == nil {
		return nil
	}

	cp := make(DecimalArray, len(a))
	for i := range a {
		cp[i] = CloneDecimal(a[i])
	}

	return cp
}
