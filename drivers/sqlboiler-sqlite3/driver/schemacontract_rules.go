package driver

import (
	"github.com/aarondl/sqlboiler/v4/drivers"
	"github.com/aarondl/sqlboiler/v4/schemacontract"
)

func init() {
	schemacontract.RegisterTypeRules(schemacontract.TypeRulesFromDriverSet(sqliteTypeRuleSet()))
}

// TypeRules implements drivers.TypeRuleProvider so the binary driver protocol
// exposes the same explicit rules as the side-effect imported driver.
func (SQLiteDriver) TypeRules() (*drivers.TypeRuleSet, error) {
	return sqliteTypeRuleSet(), nil
}

// sqliteTypeRuleSet builds the explicit sqlite3 compatibility table from the
// same type families used by SQLiteDriver.TranslateColumnType. SQLite has no
// user-defined domains; columns carrying domain information fall through to
// VerdictUnknown via the shared FamilyTable implementation, and DB types
// absent from this table (custom application types that sqlite's dynamic
// typing silently turns into strings) also report unknown instead of being
// assumed compatible.
func sqliteTypeRuleSet() *drivers.TypeRuleSet {
	const (
		famInteger = "integer"
		famText    = "text"
		famBlob    = "blob"
		famFloat   = "float"
		famDecimal = "decimal"
		famBool    = "boolean"
		famTime    = "time"
		famJSON    = "json"
	)

	families := map[string]drivers.TypeFamilyRule{}

	// Integer family: rank follows the generated Go type width. A live type
	// with a larger rank than the contract is a widening (read risk), a
	// smaller rank a narrowing (write risk).
	for _, t := range []string{"TINYINT", "INT8"} {
		families[t] = drivers.TypeFamilyRule{Family: famInteger, Rank: 1}
	}
	for _, t := range []string{"SMALLINT", "INT2"} {
		families[t] = drivers.TypeFamilyRule{Family: famInteger, Rank: 2}
	}
	families["MEDIUMINT"] = drivers.TypeFamilyRule{Family: famInteger, Rank: 3}
	for _, t := range []string{"INT", "INTEGER", "BIGINT"} {
		families[t] = drivers.TypeFamilyRule{Family: famInteger, Rank: 4}
	}
	families["UNSIGNED BIG INT"] = drivers.TypeFamilyRule{Family: famInteger, Rank: 5}

	for _, t := range []string{
		"CHARACTER", "VARCHAR", "VARYING CHARACTER", "NCHAR",
		"NATIVE CHARACTER", "NVARCHAR", "TEXT", "CLOB",
	} {
		families[t] = drivers.TypeFamilyRule{Family: famText, Rank: 1}
	}

	families["BLOB"] = drivers.TypeFamilyRule{Family: famBlob, Rank: 1}

	// FLOAT maps to float32, REAL/DOUBLE family to float64.
	families["FLOAT"] = drivers.TypeFamilyRule{Family: famFloat, Rank: 1}
	for _, t := range []string{"REAL", "DOUBLE", "DOUBLE PRECISION"} {
		families[t] = drivers.TypeFamilyRule{Family: famFloat, Rank: 2}
	}

	for _, t := range []string{"NUMERIC", "DECIMAL"} {
		families[t] = drivers.TypeFamilyRule{Family: famDecimal, Rank: 1}
	}
	families["BOOLEAN"] = drivers.TypeFamilyRule{Family: famBool, Rank: 1}
	for _, t := range []string{"DATE", "DATETIME", "TIMESTAMP"} {
		families[t] = drivers.TypeFamilyRule{Family: famTime, Rank: 1}
	}
	families["JSON"] = drivers.TypeFamilyRule{Family: famJSON, Rank: 1}

	return &drivers.TypeRuleSet{Driver: "sqlite3", Families: families}
}
