package schemacontract

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/aarondl/sqlboiler/v4/drivers"
)

func TestExportRedactsSecrets(t *testing.T) {
	m := baselineManifest(t)

	encoded, err := EncodeManifest(m)
	require.NoError(t, err)
	raw := string(encoded)

	require.NotContains(t, raw, "super-secret")
	require.NotContains(t, raw, "hunter2")
	require.Contains(t, raw, RedactedValue)
	require.Contains(t, raw, `"dbname": "app"`)
	require.Contains(t, raw, `"host": "localhost"`)
}

func TestExportDigestsAreStableAndPresent(t *testing.T) {
	first := baselineManifest(t)
	second := baselineManifest(t)

	require.NotEmpty(t, first.ConfigDigest)
	require.NotEmpty(t, first.AliasesDigest)
	require.Len(t, first.ConfigDigest, 64)
	require.Len(t, first.AliasesDigest, 64)
	require.Equal(t, first.ConfigDigest, second.ConfigDigest)
	require.Equal(t, first.AliasesDigest, second.AliasesDigest)

	// Changing the aliases changes the alias digest only.
	other, err := Export(ExportOptions{
		DriverName: fixtureDriverName,
		Tables:     baselineTables(),
		Aliases:    map[string]any{"tables": map[string]any{"users": map[string]any{"up_singular": "Different"}}},
		DriverConfig: drivers.Config{
			"dbname": "app",
			"pass":   "super-secret",
		},
	})
	require.NoError(t, err)
	require.NotEqual(t, first.AliasesDigest, other.AliasesDigest)
}

func TestExportSortsTablesButNotColumns(t *testing.T) {
	m := baselineManifest(t)

	require.Equal(t,
		[]string{"accounts", "user_view", "users", "s1.users", "s2.users"},
		[]string{
			m.Tables[0].Name,
			m.Tables[1].Name,
			m.Tables[2].Name,
			tableKey(m.Tables[3].SchemaName, m.Tables[3].Name),
			tableKey(m.Tables[4].SchemaName, m.Tables[4].Name),
		})
}

func TestExportViewCapabilities(t *testing.T) {
	m := baselineManifest(t)
	view := findTableContract(t, m, "", "user_view")
	require.True(t, view.IsView)
	require.False(t, view.ViewCapabilities.CanInsert)
	require.False(t, view.ViewCapabilities.CanUpsert)
	require.Nil(t, view.PrimaryKey)
}

func TestExportTypeReplaceMarking(t *testing.T) {
	tables := []drivers.Table{{
		Name: "widgets",
		Columns: []drivers.Column{
			{Name: "id", Type: "int64", DBType: "BIGINT", Nullable: false},
			{Name: "payload", Type: "myapp.JSONBox", DBType: "JSON", Nullable: false},
		},
		PKey: &drivers.PrimaryKey{Columns: []string{"id"}},
	}}

	input := TypeReplaceInput{
		Tables:  []string{"widgets"},
		Match:   drivers.Column{DBType: "JSON"},
		Replace: drivers.Column{Type: "myapp.JSONBox"},
	}
	digest, err := ComputeTypeReplaceDigest(input)
	require.NoError(t, err)

	m, err := Export(ExportOptions{
		DriverName:   fixtureDriverName,
		Tables:       tables,
		TypeReplaces: []TypeReplaceInput{input},
		ReplacedColumns: map[string]string{
			ReplacedColumnKey("", "widgets", "payload"): digest,
		},
	})
	require.NoError(t, err)

	widgets := findTableContract(t, m, "", "widgets")
	var payload *ColumnContract
	for i := range widgets.Columns {
		if widgets.Columns[i].Name == "payload" {
			payload = &widgets.Columns[i]
		}
	}
	require.NotNil(t, payload)
	require.True(t, payload.TypeReplaced)
	require.Equal(t, digest, payload.TypeReplaceDigest)
	require.Len(t, m.TypeReplaces, 1)
	require.Equal(t, digest, m.TypeReplaces[0].Digest)
}

func TestExportRejectsInvalidInputs(t *testing.T) {
	_, err := Export(ExportOptions{Tables: baselineTables()})
	require.Error(t, err)

	_, err = Export(ExportOptions{DriverName: fixtureDriverName})
	require.Error(t, err)
}

func TestRedactConfigCoversCredentialKeys(t *testing.T) {
	cfg := drivers.Config{
		"pass":     "a",
		"password": "b",
		"secret":   "c",
		"dsn":      "d",
		"token":    "e",
		"dbname":   "kept",
		"schema":   "public",
	}
	redacted := redactConfig(cfg)
	for _, key := range []string{"pass", "password", "secret", "dsn", "token"} {
		require.Equal(t, RedactedValue, redacted[key], key)
	}
	require.Equal(t, "kept", redacted["dbname"])
	require.Equal(t, "public", redacted["schema"])
}

func TestManifestJSONHasNoGoOnlyFields(t *testing.T) {
	encoded, err := EncodeManifest(baselineManifest(t))
	require.NoError(t, err)

	var generic map[string]any
	require.NoError(t, json.Unmarshal(encoded, &generic))
	require.NotContains(t, generic, "schemaContractReplaced")
	// Tables is an array of objects, never a map.
	tables, ok := generic["tables"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, tables)
	require.True(t, strings.HasPrefix(string(encoded), "{"))
}
