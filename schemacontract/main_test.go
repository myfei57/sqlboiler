package schemacontract

import (
	"os"
	"testing"
)

// TestMain registers explicit type rules for the fixture driver name. The
// real sqlite3 rules are registered by the driver package's own init in
// compatibility_sqlite_test.go via a blank import.
func TestMain(m *testing.M) {
	RegisterTypeRules(FamilyTable{
		Driver: fixtureDriverName,
		Families: map[string]TypeFamily{
			"INT":       {Name: "integer", Rank: 3, Known: true},
			"BIGINT":    {Name: "integer", Rank: 4, Known: true},
			"SMALLINT":  {Name: "integer", Rank: 2, Known: true},
			"VARCHAR":   {Name: "text", Rank: 1, Known: true},
			"TEXT":      {Name: "text", Rank: 1, Known: true},
			"TIMESTAMP": {Name: "time", Rank: 1, Known: true},
		},
	})

	os.Exit(m.Run())
}
