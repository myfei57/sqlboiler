package boilingcore

// cloneGraphBaseFixture is written into a generated mock-schema module and
// run there. It covers cycles (pilot<->jet, pilot<->license,
// pilot<->language through the join table), many-to-many, shared leaves,
// nil/empty relationship states, byte independence, budgets, multi-root
// sessions and the absence of any executor/database call.
const cloneGraphBaseFixture = `
package models

import (
	"errors"
	"reflect"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/aarondl/null/v8"
	"github.com/aarondl/sqlboiler/v4/boil"
)

// verifyCloneTopology walks the R relationship graph of src and dst in
// parallel and asserts edge by edge that:
//   - every non-nil source edge has a non-nil clone edge of the same shape,
//   - every nil pointer and nil/empty slice state is preserved exactly,
//   - a source node reached twice resolves to the same clone (shared nodes
//     and cycles keep their topology),
//   - no pointer of the source graph appears anywhere in the clone graph.
func verifyCloneTopology(t *testing.T, src, dst any) {
	t.Helper()

	srcPointers := map[uintptr]bool{}
	dstPointers := map[uintptr]bool{}
	pairs := map[uintptr]uintptr{}

	var walk func(s, d reflect.Value)
	walk = func(s, d reflect.Value) {
		if s.Kind() == reflect.Ptr && s.IsNil() {
			if !d.IsNil() {
				t.Fatalf("nil source edge was cloned as non-nil")
			}
			return
		}

		sp, dp := s.Pointer(), d.Pointer()
		if srcPointers[sp] {
			if pairs[sp] != dp {
				t.Fatalf("shared source node %#x maps to two different clones: %#x and %#x", sp, pairs[sp], dp)
			}
			return
		}

		srcPointers[sp] = true
		dstPointers[dp] = true
		pairs[sp] = dp

		rField := s.Elem().FieldByName("R")
		if !rField.IsValid() {
			return
		}
		if rField.IsNil() {
			if !d.Elem().FieldByName("R").IsNil() {
				t.Fatalf("nil R (unloaded relationships) was not preserved")
			}
			return
		}
		if d.Elem().FieldByName("R").IsNil() {
			t.Fatalf("loaded R was cloned as nil")
		}

		sr, dr := rField.Elem(), d.Elem().FieldByName("R").Elem()
		for i := 0; i < sr.NumField(); i++ {
			name := sr.Type().Field(i).Name
			sf, df := sr.Field(i), dr.Field(i)
			switch sf.Kind() {
			case reflect.Ptr:
				walk(sf, df)
			case reflect.Slice:
				if sf.IsNil() {
					if !df.IsNil() {
						t.Fatalf("nil slice R.%s was not preserved", name)
					}
					continue
				}
				if df.IsNil() {
					t.Fatalf("non-nil slice R.%s was cloned as nil", name)
				}
				if sf.Len() != df.Len() {
					t.Fatalf("slice R.%s length changed: %d -> %d", name, sf.Len(), df.Len())
				}
				for j := 0; j < sf.Len(); j++ {
					walk(sf.Index(j), df.Index(j))
				}
			default:
				t.Fatalf("unexpected relationship field kind: %s", sf.Kind())
			}
		}
	}

	walk(reflect.ValueOf(src), reflect.ValueOf(dst))

	for dp := range dstPointers {
		if srcPointers[dp] {
			t.Fatalf("clone graph contains a source pointer: %#x", dp)
		}
	}
}

func TestCloneGraphFixtures(t *testing.T) {
	// An executor exists and is set globally: none of the clone calls may
	// ever touch it.
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	boil.SetDB(db)

	sharedCargo := []byte("shared-cargo")
	otherCargo := []byte("shared-cargo") // equal contents, distinct instance

	airport1 := &Airport{ID: 1, Size: null.Int{Int: 10, Valid: true}}
	jet1 := &Jet{
		ID: 1, PilotID: null.Int{Int: 1, Valid: true}, AirportID: 1,
		Name:       "skylane",
		Color:      null.String{String: "red", Valid: true},
		UUID:       null.String{String: "uuid-1", Valid: true},
		Identifier: "identifier-1",
		Cargo:      sharedCargo,
		Manifest:   null.Bytes{Bytes: []byte("manifest"), Valid: true},
	}
	jet2 := &Jet{ID: 2, AirportID: 1, Name: "skylark", Cargo: sharedCargo}
	jet3 := &Jet{ID: 3, AirportID: 1, Name: "empty-relationships"}       // R stays nil
	jet4 := &Jet{ID: 4, AirportID: 1, Name: "loaded-empty-r", R: &jetR{}}
	jet5 := &Jet{ID: 5, AirportID: 1, Name: "distinct-bytes", Cargo: otherCargo}

	pilot1 := &Pilot{ID: 1, Name: "amelia"}
	pilot2 := &Pilot{ID: 2, Name: "bedelia", R: &pilotR{Languages: LanguageSlice{}}}
	license1 := &License{ID: 1, PilotID: 1}
	lang1 := &Language{ID: 1, Language: "en"}
	lang2 := &Language{ID: 2, Language: "fr"}

	// Cyclic graph: pilot -> jet -> pilot, pilot -> license -> pilot and
	// pilot -> languages (many-to-many) -> pilot. jet2 shares its cargo
	// backing array with jet1.
	pilot1.R = &pilotR{
		Jet:       jet1,
		Licenses:  LicenseSlice{license1},
		Languages: LanguageSlice{lang1, lang2},
	}
	jet1.R = &jetR{Pilot: pilot1, Airport: airport1}
	airport1.R = &airportR{Jets: JetSlice{jet1, jet2}}
	license1.R = &licenseR{Pilot: pilot1}
	lang1.R = &languageR{Pilots: PilotSlice{pilot1}}

	session := NewCloneGraphSession(CloneGraphBudget{})

	cp1, err := session.ClonePilot(pilot1)
	if err != nil {
		t.Fatalf("ClonePilot: %s", err)
	}
	if cp1 == nil || cp1 == pilot1 {
		t.Fatal("pilot clone missing or aliased to source")
	}

	// Second root in the same session keeps the shared nodes.
	cAirport, err := session.CloneAirport(airport1)
	if err != nil {
		t.Fatalf("CloneAirport: %s", err)
	}
	cj3, err := session.CloneJet(jet3)
	if err != nil {
		t.Fatalf("CloneJet jet3: %s", err)
	}
	cj4, err := session.CloneJet(jet4)
	if err != nil {
		t.Fatalf("CloneJet jet4: %s", err)
	}
	cp2, err := session.ClonePilot(pilot2)
	if err != nil {
		t.Fatalf("ClonePilot pilot2: %s", err)
	}
	cj5, err := session.CloneJet(jet5)
	if err != nil {
		t.Fatalf("CloneJet jet5: %s", err)
	}

	// One-shot helper and nil roots.
	oneShot, err := ClonePilotGraph(pilot1)
	if err != nil || oneShot == nil {
		t.Fatalf("ClonePilotGraph: %v %v", oneShot, err)
	}
	nilClone, err := CloneJetGraph(nil)
	if err != nil || nilClone != nil {
		t.Fatalf("CloneJetGraph(nil) = %v, %v", nilClone, err)
	}

	// Primary key values are copied verbatim.
	if cp1.ID != pilot1.ID || cp1.Name != pilot1.Name {
		t.Fatal("scalar column values were not copied")
	}
	if cAirport.ID != 1 || !cAirport.Size.Valid || cAirport.Size.Int != 10 {
		t.Fatal("nullable scalar wrapper was not copied")
	}

	// Cycles resolve back to the cloned root, never to the source.
	if cp1.R.Jet.R.Pilot != cp1 {
		t.Fatal("pilot->jet->pilot cycle was not preserved")
	}
	if cp1.R.Licenses[0].R.Pilot != cp1 {
		t.Fatal("pilot->license->pilot cycle was not preserved")
	}
	if cp1.R.Languages[0].R.Pilots[0] != cp1 {
		t.Fatal("many-to-many back edge cycle was not preserved")
	}

	// Shared model node: the airport reached through the jet is the same
	// clone as the one produced by the second root.
	if cp1.R.Jet.R.Airport != cAirport {
		t.Fatal("shared airport node was cloned twice")
	}
	if cAirport.R.Jets[0] != cp1.R.Jet {
		t.Fatal("shared jet node topology diverged between roots")
	}

	// Shared mutable leaf: jet1 and jet2 deliberately point at the same cargo
	// backing array, so their clones must share one cloned backing array.
	cj1 := cp1.R.Jet
	cj2 := cAirport.R.Jets[1]
	if &cj1.Cargo[0] != &cj2.Cargo[0] {
		t.Fatal("shared []byte backing array was not preserved in clone")
	}

	// Equal values in distinct instances must get independent memory.
	if &cj1.Cargo[0] == &cj5.Cargo[0] {
		t.Fatal("distinct []byte instances were aliased in the clone")
	}
	cj5Cargo0 := cj5.Cargo[0]
	cj1.Cargo[0] = 'Z'
	if cj5.Cargo[0] != cj5Cargo0 {
		t.Fatal("mutating one clone leaked into an equal-but-distinct instance")
	}
	if jet5.Cargo[0] != 's' {
		t.Fatal("source graph was mutated through the clone")
	}

	// Nullable byte wrapper: Valid flag and independent bytes.
	if !cj1.Manifest.Valid || string(cj1.Manifest.Bytes) != "manifest" {
		t.Fatal("null.Bytes content/Valid was not copied")
	}
	cj1.Manifest.Bytes[0] = 'X'
	if jet1.Manifest.Bytes[0] != 'm' {
		t.Fatal("null.Bytes source bytes were mutated through the clone")
	}

	// nil R, empty R, nil vs empty slices keep their exact state.
	if cj3.R != nil {
		t.Fatal("nil R (unloaded) was turned into a non-nil relationship struct")
	}
	if cj4.R == nil || cj4.R.Pilot != nil || cj4.R.Airport != nil {
		t.Fatal("loaded but empty R did not keep its nil edges")
	}
	if cp2.R == nil || cp2.R.Jet != nil || cp2.R.Licenses != nil {
		t.Fatal("nil relationship edges were not preserved")
	}
	if cp2.R.Languages == nil || len(cp2.R.Languages) != 0 {
		t.Fatal("empty non-nil relationship slice was not preserved as empty")
	}
	if cj3.Cargo != nil {
		t.Fatal("nil []byte column was not preserved as nil")
	}
	if cj3.Manifest.Bytes != nil || cj3.Manifest.Valid {
		t.Fatal("zero null.Bytes gained bytes or validity")
	}

	// Slice roots: nil, empty and nil elements.
	nilSlice, err := session.CloneJetSlice(nil)
	if err != nil || nilSlice != nil {
		t.Fatalf("nil slice root = %v, %v", nilSlice, err)
	}
	emptySlice, err := session.CloneJetSlice(JetSlice{})
	if err != nil || emptySlice == nil || len(emptySlice) != 0 {
		t.Fatalf("empty non-nil slice root = %v, %v", emptySlice, err)
	}
	mixed, err := session.CloneJetSlice(JetSlice{jet1, nil, jet3})
	if err != nil || len(mixed) != 3 || mixed[1] != nil || mixed[0] != cj1 {
		t.Fatalf("mixed slice root mis-cloned: %#v err=%v", mixed, err)
	}

	// Full edge-by-edge topology verification for every root.
	verifyCloneTopology(t, pilot1, cp1)
	verifyCloneTopology(t, airport1, cAirport)
	verifyCloneTopology(t, pilot2, cp2)
	verifyCloneTopology(t, jet4, cj4)

	// Mutate the clones aggressively; the source graph must stay untouched.
	cp1.Name = "changed"
	cp1.R.Jet = nil
	cp1.R.Licenses = nil
	cp1.R.Languages[0].Language = "xx"
	cAirport.R.Jets = append(cAirport.R.Jets, cj3)
	cj2.Cargo = append(cj2.Cargo, '!')
	if pilot1.Name != "amelia" || pilot1.R.Jet == nil || pilot1.R.Licenses == nil {
		t.Fatal("source pilot changed after mutating the clone")
	}
	if lang1.Language != "en" {
		t.Fatal("source language changed after mutating the clone")
	}
	if len(airport1.R.Jets) != 2 || string(jet2.Cargo) != "shared-cargo" {
		t.Fatal("source airport graph changed after mutating the clone")
	}
	if jet1.Name != "skylane" || jet1.R.Pilot != pilot1 {
		t.Fatal("source jet changed after mutating the clone")
	}

	// Stats reflect the traversal without any SQL involvement.
	stats := session.Stats()
	if stats.Nodes < 6 || stats.Edges < 1 || stats.MaxDepth < 2 || stats.Bytes < len(sharedCargo)+len(jet1.Manifest.Bytes) {
		t.Fatalf("unexpected clone stats: %+v", stats)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the executor received unexpected calls: %s", err)
	}

	// Budgets: every dimension fails the session as a whole with a nil result
	// and the typed budget error.
	budgetCases := map[string]CloneGraphBudget{
		"nodes": {MaxNodes: 1},
		"edges": {MaxEdges: 1},
		"depth": {MaxDepth: 1},
		"bytes": {MaxBytes: 1},
	}
	for name, budget := range budgetCases {
		failed := NewCloneGraphSession(budget)
		cp, err := failed.ClonePilot(pilot1)
		if err == nil || cp != nil {
			t.Fatalf("budget %s: expected error and nil clone, got %v and %v", name, cp, err)
		}
		if !errors.Is(err, ErrCloneGraphBudgetExceeded) {
			t.Fatalf("budget %s: error does not wrap ErrCloneGraphBudgetExceeded: %v", name, err)
		}
		var graphErr *CloneGraphError
		if !errors.As(err, &graphErr) {
			t.Fatalf("budget %s: error is not *CloneGraphError: %v", name, err)
		}
		// A failed session refuses reuse instead of returning a half graph.
		if _, err := failed.ClonePilot(pilot1); !errors.Is(err, ErrCloneGraphSessionFailed) {
			t.Fatalf("budget %s: failed session reuse returned %v", name, err)
		}
	}
}

// TestCloneGraphDeepChain builds a long alternating pilot/jet relationship
// chain and clones it through the iterative work queue. A recursive copier
// would overflow the goroutine stack on a chain of this length.
func TestCloneGraphDeepChain(t *testing.T) {
	const links = 2000

	root := &Pilot{ID: 1, Name: "p0"}
	cur := root
	for i := 0; i < links; i++ {
		jet := &Jet{ID: i + 1, AirportID: 1, Name: "j"}
		next := &Pilot{ID: i + 2, Name: "p"}
		cur.R = &pilotR{Jet: jet}
		jet.R = &jetR{Pilot: next}
		cur = next
	}

	cp, err := NewCloneGraphSession(CloneGraphBudget{}).ClonePilot(root)
	if err != nil {
		t.Fatalf("deep chain clone: %s", err)
	}

	// Walk the cloned chain and count the hops; every back edge must point
	// at a clone, never at a source object.
	depth := 0
	srcCur, dstCur := root, cp
	for dstCur.R != nil && dstCur.R.Jet != nil {
		if dstCur.R.Jet.R == nil || dstCur.R.Jet.R.Pilot == nil {
			t.Fatalf("chain broken at depth %d", depth)
		}
		if dstCur.R.Jet == srcCur.R.Jet || dstCur.R.Jet.R.Pilot == srcCur.R.Jet.R.Pilot {
			t.Fatalf("source pointer leaked into clone at depth %d", depth)
		}
		srcCur = srcCur.R.Jet.R.Pilot
		dstCur = dstCur.R.Jet.R.Pilot
		depth++
	}
	if depth != links {
		t.Fatalf("cloned chain depth = %d, want %d", depth, links)
	}
}
`
