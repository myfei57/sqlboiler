package types

import (
	"testing"

	"github.com/ericlagergren/decimal"
)

func TestCloneDecimal(t *testing.T) {
	t.Parallel()

	big, ok := new(decimal.Big).SetString("123.45")
	if !ok {
		t.Fatal("failed to parse decimal")
	}
	src := Decimal{Big: big}

	cp := CloneDecimal(src)
	if cp.Big == big {
		t.Fatal("CloneDecimal shared the *decimal.Big pointer")
	}
	if cp.String() != "123.45" {
		t.Fatalf("clone value = %q, want 123.45", cp.String())
	}

	if _, ok := cp.Big.SetString("999"); !ok {
		t.Fatal("failed to mutate clone")
	}
	if src.String() != "123.45" {
		t.Fatalf("source decimal mutated through clone: %s", src.String())
	}

	// A nil embedded pointer must stay nil rather than become zero-but-non-nil.
	if got := CloneDecimal(Decimal{}); got.Big != nil {
		t.Fatalf("nil decimal Big was not preserved: %#v", got)
	}
}

func TestCloneNullDecimal(t *testing.T) {
	t.Parallel()

	big, ok := new(decimal.Big).SetString("0.1")
	if !ok {
		t.Fatal("failed to parse decimal")
	}
	src := NullDecimal{Big: big}

	cp := CloneNullDecimal(src)
	if cp.Big == big {
		t.Fatal("CloneNullDecimal shared the *decimal.Big pointer")
	}
	if cp.String() != "0.1" {
		t.Fatalf("clone value = %q, want 0.1", cp.String())
	}

	cp.Big.SetString("7")
	if src.String() != "0.1" {
		t.Fatalf("source decimal mutated through clone: %s", src.String())
	}

	if got := CloneNullDecimal(NullDecimal{}); got.Big != nil {
		t.Fatalf("nil null decimal Big was not preserved: %#v", got)
	}
}

func TestCloneDecimalArray(t *testing.T) {
	t.Parallel()

	b1, _ := new(decimal.Big).SetString("1.5")
	b2, _ := new(decimal.Big).SetString("2.5")
	src := DecimalArray{NewDecimal(b1), {Big: nil}, NewDecimal(b2)}

	cp := CloneDecimalArray(src)
	if len(cp) != 3 {
		t.Fatalf("clone length = %d, want 3", len(cp))
	}
	if cp[0].Big == b1 || cp[2].Big == b2 {
		t.Fatal("decimal array elements were shallow-copied")
	}
	if cp[1].Big != nil {
		t.Fatal("nil element Big was not preserved")
	}

	cp[0].Big.SetString("9")
	if src[0].String() != "1.5" {
		t.Fatalf("source array element mutated through clone: %s", src[0].String())
	}

	if CloneDecimalArray(nil) != nil {
		t.Fatal("nil DecimalArray was not preserved as nil")
	}

	empty := DecimalArray{}
	cpEmpty := CloneDecimalArray(empty)
	if cpEmpty == nil || len(cpEmpty) != 0 {
		t.Fatal("empty non-nil DecimalArray did not stay empty and non-nil")
	}
}
