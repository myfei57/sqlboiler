package schemacontract

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/aarondl/sqlboiler/v4/drivers"
)

func TestFamilyTableTypeVerdicts(t *testing.T) {
	rules := FamilyTable{
		Driver: "t",
		Families: map[string]TypeFamily{
			"INT":      {Name: "integer", Rank: 3, Known: true},
			"BIGINT":   {Name: "integer", Rank: 4, Known: true},
			"SMALLINT": {Name: "integer", Rank: 2, Known: true},
			"VARCHAR":  {Name: "text", Rank: 1, Known: true},
		},
	}

	col := func(dbType string) drivers.Column {
		return drivers.Column{DBType: dbType, FullDBType: dbType}
	}

	require.Equal(t, VerdictCompatible, rules.CheckColumnType(col("BIGINT"), col("BIGINT")))
	// Parenthesised size declarations must not defeat the lookup.
	require.Equal(t, VerdictCompatible, rules.CheckColumnType(col("VARCHAR(50)"), col("VARCHAR(100)")))
	require.Equal(t, VerdictWidened, rules.CheckColumnType(col("INT"), col("BIGINT")))
	require.Equal(t, VerdictNarrowed, rules.CheckColumnType(col("BIGINT"), col("SMALLINT")))
	require.Equal(t, VerdictIncompatible, rules.CheckColumnType(col("BIGINT"), col("VARCHAR")))
	require.Equal(t, VerdictUnknown, rules.CheckColumnType(col("BIGINT"), col("CUSTOM_TYPE")))
	require.Equal(t, VerdictUnknown, rules.CheckColumnType(col("CUSTOM_TYPE"), col("BIGINT")))

	domain := "my_domain"
	require.Equal(t, VerdictUnknown, rules.CheckColumnType(
		drivers.Column{DBType: "BIGINT", DomainName: &domain}, col("BIGINT")))
	require.Equal(t, VerdictUnknown, rules.CheckColumnType(
		col("BIGINT"), drivers.Column{DBType: "BIGINT", DomainName: &domain}))
}

func TestTypeRulesRegistry(t *testing.T) {
	rules, ok := TypeRulesFor(fixtureDriverName)
	require.True(t, ok)
	require.Equal(t, fixtureDriverName, rules.DriverName())

	_, ok = TypeRulesFor("never-registered-driver")
	require.False(t, ok)

	require.Panics(t, func() {
		RegisterTypeRules(FamilyTable{Driver: fixtureDriverName, Families: map[string]TypeFamily{}})
	})
	require.Panics(t, func() {
		RegisterTypeRules(nil)
	})
}

func TestBaseDBType(t *testing.T) {
	require.Equal(t, "VARCHAR", baseDBType("varchar(1000)"))
	require.Equal(t, "DOUBLE PRECISION", baseDBType("double precision"))
	require.Equal(t, "INT", baseDBType(" int(11) "))
}
