package boilingcore

// cloneGraphCustomType is a support package written into the generated test
// module. Widget deliberately keeps its mutable state in private fields, so
// the generator cannot copy it safely on its own: an explicit CloneGraph
// adapter is required.
const cloneGraphCustomType = `
package customtype

// Widget is an external model column type with private mutable state.
type Widget struct {
	Label string
	tags  []string
	meta  map[string]int
}

// NewWidget builds a Widget, exposing its private state to the fixture.
func NewWidget(label string, tags []string, meta map[string]int) Widget {
	return Widget{Label: label, tags: tags, meta: meta}
}

// Tags returns the private tags slice.
func (w Widget) Tags() []string {
	return w.tags
}

// Meta returns the private meta map.
func (w Widget) Meta() map[string]int {
	return w.meta
}

// AddTag mutates the private tags slice and is used to prove clone
// independence.
func (w *Widget) AddTag(tag string) {
	w.tags = append(w.tags, tag)
}

// SetMeta mutates the private meta map and is used to prove clone
// independence.
func (w *Widget) SetMeta(key string, value int) {
	if w.meta == nil {
		w.meta = make(map[string]int)
	}
	w.meta[key] = value
}
`

// cloneGraphExoticFixture exercises every generated copy strategy through
// type replacements: decimals (including nil *Big), hstore maps, JSON bytes,
// pgeo paths, byte/decimal arrays and an external type behind an adapter.
const cloneGraphExoticFixture = `
package models

import (
	"errors"
	"testing"

	"github.com/aarondl/null/v8"
	"github.com/aarondl/sqlboiler-test/customtype"
	"github.com/aarondl/sqlboiler/v4/boil"
	"github.com/aarondl/sqlboiler/v4/types"
	"github.com/aarondl/sqlboiler/v4/types/pgeo"

	"github.com/DATA-DOG/go-sqlmock"
)

func init() {
	RegisterCloneGraphAdapter(func(w customtype.Widget) (customtype.Widget, error) {
		var tags []string
		if w.Tags() != nil {
			tags = append([]string(nil), w.Tags()...)
		}
		var meta map[string]int
		if w.Meta() != nil {
			meta = make(map[string]int, len(w.Meta()))
			for k, v := range w.Meta() {
				meta[k] = v
			}
		}
		return customtype.NewWidget(w.Label, tags, meta), nil
	})
}

func TestCloneGraphExoticTypes(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	boil.SetDB(db)

	// Build decimals through their SQL scanner so the fixture stays free of
	// direct dependencies on the underlying arbitrary precision package.
	var shared types.Decimal
	if err := shared.Scan("42.50"); err != nil {
		t.Fatalf("shared decimal scan: %s", err)
	}
	var other types.NullDecimal
	if err := other.Scan("7.25"); err != nil {
		t.Fatalf("other decimal scan: %s", err)
	}

	airport1 := &Airport{ID: 1, Size: types.BytesArray{[]byte("a"), nil, []byte("ccc")}}
	airport2 := &Airport{ID: 2} // nil BytesArray

	jet1 := &Jet{
		ID: 1, AirportID: 1, Name: "jet-1",
		Cargo:      shared,
		Manifest:   other,
		Color:      types.HStore{"k": null.NewString("v", true)},
		UUID:       types.JSON("{\"n\":1}"),
		Identifier: pgeo.Path{Points: []pgeo.Point{{X: 1, Y: 2}}, Closed: true},
	}
	jet2 := &Jet{
		ID: 2, AirportID: 1, Name: "jet-2",
		// Same *decimal.Big instance as jet1.Cargo to test pointer sharing.
		Cargo:      shared,
		Manifest:   types.NullDecimal{}, // nil *Big must stay nil
		Identifier: pgeo.Path{},         // nil Points must stay nil
	}

	lang1 := &Language{ID: 1, Language: types.DecimalArray{
		{Big: other.Big},
		types.Decimal{}, // nil *Big element must stay nil
	}}
	lang2 := &Language{ID: 2} // nil DecimalArray

	pilot1 := &Pilot{
		ID:   1,
		Name: customtype.NewWidget("amelia", []string{"a", "b"}, map[string]int{"k": 1}),
		R:    &pilotR{Jet: jet1, Languages: LanguageSlice{lang1, lang2}},
	}
	pilot2 := &Pilot{ID: 2, Name: customtype.NewWidget("bedelia", nil, nil)}
	jet1.R = &jetR{Pilot: pilot1, Airport: airport1}
	airport1.R = &airportR{Jets: JetSlice{jet1, jet2}}

	session := NewCloneGraphSession(CloneGraphBudget{})
	cp, err := session.ClonePilot(pilot1)
	if err != nil {
		t.Fatalf("ClonePilot: %s", err)
	}
	cp2, err := session.ClonePilot(pilot2)
	if err != nil {
		t.Fatalf("ClonePilot pilot2: %s", err)
	}
	cAirport2, err := session.CloneAirport(airport2)
	if err != nil {
		t.Fatalf("CloneAirport airport2: %s", err)
	}

	cj1 := cp.R.Jet
	cAirport := cj1.R.Airport
	cj2 := cAirport.R.Jets[1]

	// Decimal: independent backing, equal value, shared source pointer maps
	// to one shared cloned pointer.
	if cj1.Cargo.Big == shared.Big || cj2.Cargo.Big == shared.Big {
		t.Fatal("decimal.Big pointer was shallow-copied")
	}
	if cj1.Cargo.Big != cj2.Cargo.Big {
		t.Fatal("shared *decimal.Big did not resolve to one clone")
	}
	if cj1.Cargo.String() != "42.50" {
		t.Fatalf("decimal value lost: %s", cj1.Cargo.String())
	}
	if err := cj1.Cargo.Scan("999"); err != nil {
		t.Fatalf("could not mutate cloned decimal: %s", err)
	}
	if shared.Big.String() != "42.50" {
		t.Fatalf("source decimal mutated through clone: %s", shared.Big.String())
	}

	// NullDecimal: set value copied independently, nil *Big preserved.
	if cj1.Manifest.Big == other.Big || cj1.Manifest.String() != "7.25" {
		t.Fatal("NullDecimal value not copied independently")
	}
	if cj2.Manifest.Big != nil {
		t.Fatal("nil NullDecimal.Big was not preserved")
	}

	// HStore: populated map independent, nil map preserved.
	if cj1.Color == nil || cj1.Color["k"].String != "v" {
		t.Fatal("hstore content not copied")
	}
	cj1.Color["new"] = null.NewString("x", true)
	delete(cj1.Color, "k")
	if jet1.Color["k"].String != "v" || len(jet1.Color) != 1 {
		t.Fatal("source hstore mutated through clone")
	}
	if cj2.Color != nil {
		t.Fatal("nil hstore was not preserved")
	}

	// JSON bytes: independent and nil preserved.
	if string(cj1.UUID) != "{\"n\":1}" {
		t.Fatal("json content not copied")
	}
	cj1.UUID[0] = ' '
	if jet1.UUID[0] != '{' {
		t.Fatal("source json bytes mutated through clone")
	}
	if cj2.UUID != nil {
		t.Fatal("nil JSON was not preserved")
	}

	// pgeo.Path: Points slice independent, nil Points and Closed preserved.
	if !cj1.Identifier.Closed || len(cj1.Identifier.Points) != 1 {
		t.Fatal("path content not copied")
	}
	cj1.Identifier.Points = append(cj1.Identifier.Points, pgeo.Point{X: 9, Y: 9})
	if len(jet1.Identifier.Points) != 1 {
		t.Fatal("source path points mutated through clone")
	}
	if cj2.Identifier.Points != nil || cj2.Identifier.Closed {
		t.Fatal("zero-value path gained state during clone")
	}

	// BytesArray: outer and inner arrays independent, nil inner preserved.
	cSize := cAirport.Size
	if len(cSize) != 3 || cSize[1] != nil || string(cSize[0]) != "a" || string(cSize[2]) != "ccc" {
		t.Fatalf("bytes array content wrong: %#v", cSize)
	}
	cSize[0][0] = 'Z'
	if airport1.Size[0][0] != 'a' {
		t.Fatal("source inner []byte mutated through clone")
	}
	if cAirport2.Size != nil {
		t.Fatal("nil BytesArray was not preserved")
	}

	// DecimalArray: nil array and nil *Big element preserved, values
	// independent.
	cLang1 := cp.R.Languages[0]
	if cp.R.Languages[1].Language != nil {
		t.Fatal("nil DecimalArray was not preserved")
	}
	if len(cLang1.Language) != 2 || cLang1.Language[1].Big != nil {
		t.Fatal("DecimalArray nil element state was not preserved")
	}
	if cLang1.Language[0].Big == other.Big {
		t.Fatal("decimal array element was shallow-copied")
	}
	if err := cLang1.Language[0].Scan("1"); err != nil {
		t.Fatalf("could not mutate cloned decimal array element: %s", err)
	}
	if other.Big.String() != "7.25" {
		t.Fatal("source decimal array element mutated through clone")
	}

	// External type behind an adapter: private state fully independent.
	if cp.Name.Label != "amelia" || len(cp.Name.Tags()) != 2 || cp.Name.Meta()["k"] != 1 {
		t.Fatal("adapter did not copy the widget state")
	}
	cp.Name.AddTag("c")
	cp.Name.SetMeta("k", 99)
	if len(pilot1.Name.Tags()) != 2 || pilot1.Name.Meta()["k"] != 1 {
		t.Fatal("source widget private state mutated through the adapter clone")
	}
	if cp2.Name.Tags() != nil || cp2.Name.Meta() != nil {
		t.Fatal("nil widget slices/maps were not preserved by the adapter")
	}

	// Byte budget still applies with the heavier column types.
	failed := NewCloneGraphSession(CloneGraphBudget{MaxBytes: 1})
	if got, err := failed.ClonePilot(pilot1); err == nil || got != nil {
		t.Fatalf("MaxBytes budget not enforced: %v %v", got, err)
	} else if !errors.Is(err, ErrCloneGraphBudgetExceeded) {
		t.Fatalf("budget error not wrapped correctly: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the executor received unexpected calls: %s", err)
	}
}
`
