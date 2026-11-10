package schemacontract

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	m := baselineManifest(t)

	encoded, err := EncodeManifest(m)
	require.NoError(t, err)
	require.True(t, bytes.HasSuffix(encoded, []byte("\n")))

	decoded, err := DecodeManifest(bytes.NewReader(encoded))
	require.NoError(t, err)
	require.Equal(t, ManifestVersion, decoded.ManifestVersion)
	require.Equal(t, ManifestFormat, decoded.Format)
	require.Equal(t, fixtureDriverName, decoded.DriverName)
	require.Equal(t, Disclaimer, decoded.Disclaimer)
	require.Len(t, decoded.Tables, len(m.Tables))
}

func TestDecodeRejectsCorruptAndUnsupported(t *testing.T) {
	valid, err := EncodeManifest(baselineManifest(t))
	require.NoError(t, err)

	tests := []struct {
		name    string
		input   string
		wantErr error
	}{
		{name: "not json", input: "{not-json", wantErr: ErrManifestCorrupt},
		{name: "unknown field", input: strings.Replace(string(valid), `"format"`, `"unexpected": 1, "format"`, 1), wantErr: ErrManifestCorrupt},
		{name: "missing version", input: `{"format":"sqlboiler-schema-contract","driver_name":"x","tables":[]}`, wantErr: ErrUnsupportedVersion},
		{name: "future version", input: strings.Replace(string(valid), `"manifest_version": 1`, `"manifest_version": 999`, 1), wantErr: ErrUnsupportedVersion},
		{name: "wrong format", input: strings.Replace(string(valid), ManifestFormat, "something-else", 1), wantErr: ErrManifestCorrupt},
		{name: "trailing data", input: string(valid) + `{"extra":true}`, wantErr: ErrManifestCorrupt},
		{name: "duplicate table", input: duplicateTableManifest(t), wantErr: ErrManifestCorrupt},
		{name: "broken ordinal", input: brokenOrdinalManifest(t), wantErr: ErrManifestCorrupt},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeManifest(strings.NewReader(tt.input))
			require.Error(t, err)
			require.ErrorIs(t, err, tt.wantErr)
		})
	}

	t.Run("nil reader", func(t *testing.T) {
		_, err := DecodeManifest(nil)
		require.ErrorIs(t, err, ErrManifestCorrupt)
	})
}

func TestDecodeRejectsBeforeDriverAccess(t *testing.T) {
	driver := &fakeDriver{err: nil, info: baselineDBInfo()}
	// A corrupt manifest must fail without a single Assemble call.
	_, err := CheckSchemaCompatibility(CheckOptions{
		Reader: strings.NewReader("{corrupt"),
		Driver: driver,
	})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrManifestCorrupt)
	require.Equal(t, 0, driver.assembles)
}

func TestValidateRejectsUnknownDriverAndEmptyTables(t *testing.T) {
	m := baselineManifest(t)
	m.DriverName = ""
	_, err := EncodeManifest(m)
	require.ErrorIs(t, err, ErrManifestCorrupt)

	m = baselineManifest(t)
	m.Tables = nil
	_, err = EncodeManifest(m)
	require.Error(t, err)
}

func duplicateTableManifest(t *testing.T) string {
	t.Helper()
	raw, err := EncodeManifest(baselineManifest(t))
	require.NoError(t, err)
	// EncodeManifest rejects duplicates, so splice a second table object into
	// an otherwise valid document.
	snippet := tableSnippet(t, raw)
	require.NotEmpty(t, snippet)
	return strings.Replace(string(raw), `"tables": [`, `"tables": [`+snippet, 1)
}

func brokenOrdinalManifest(t *testing.T) string {
	raw, err := EncodeManifest(baselineManifest(t))
	require.NoError(t, err)
	return strings.Replace(string(raw), `"ordinal": 1`, `"ordinal": 7`, 1)
}

// tableSnippet extracts the first table object text so duplicateTableManifest
// can construct an invalid document despite EncodeManifest validation.
func tableSnippet(t *testing.T, raw []byte) string {
	t.Helper()
	s := string(raw)
	marker := `"tables": [`
	idx := strings.Index(s, marker)
	require.True(t, idx >= 0)
	start := strings.Index(s[idx:], "{") + idx
	depth := 0
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start:i+1] + ","
			}
		}
	}
	return ""
}

func TestManifestIsDeterministic(t *testing.T) {
	first, err := EncodeManifest(baselineManifest(t))
	require.NoError(t, err)
	second, err := EncodeManifest(baselineManifest(t))
	require.NoError(t, err)
	require.Equal(t, string(first), string(second))

	// No timestamps or process-local fields are embedded.
	for _, banned := range []string{"generated_at", "timestamp\":", "pid"} {
		require.NotContains(t, string(first), banned)
	}
}

func TestManifestPreservesMeaningfulOrder(t *testing.T) {
	m := baselineManifest(t)
	users := findTableContract(t, m, "", "users")

	// Composite primary key column order must survive export.
	require.Equal(t, []string{"tenant_id", "id"}, users.PrimaryKey.Columns)

	// Columns keep database ordinal order and the ordinal values match.
	names := make([]string, len(users.Columns))
	for i, c := range users.Columns {
		require.Equal(t, i, c.Ordinal)
		names[i] = c.Name
	}
	require.Equal(t, []string{"id", "tenant_id", "name", "nickname", "score", "created_at", "label"}, names)

	// Default expressions are preserved verbatim.
	created := users.Columns[5]
	require.Equal(t, "current_timestamp", created.Default)
}

func TestManifestKeepsSameNameTablesIndependent(t *testing.T) {
	m := baselineManifest(t)
	s1 := findTableContract(t, m, "s1", "users")
	s2 := findTableContract(t, m, "s2", "users")
	require.NotNil(t, s1)
	require.NotNil(t, s2)
	require.Equal(t, []string{"id", "scope"}, columnNames(s1.Columns))
	require.Equal(t, []string{"id", "scope"}, columnNames(s2.Columns))
}

func columnNames(cols []ColumnContract) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.Name
	}
	return out
}
