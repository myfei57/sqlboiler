package drivers

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTypeRuleSetJSONRoundTrip(t *testing.T) {
	want := &TypeRuleSet{
		Driver: "sqlite3",
		Families: map[string]TypeFamilyRule{
			"INTEGER": {Family: "integer", Rank: 4},
			"TEXT":    {Family: "text", Rank: 1},
		},
	}

	raw, err := json.Marshal(want)
	require.NoError(t, err)

	var got TypeRuleSet
	require.NoError(t, json.Unmarshal(raw, &got))
	require.Equal(t, want.Driver, got.Driver)
	require.Len(t, got.Families, 2)
	require.Equal(t, 4, got.Families["INTEGER"].Rank)
	require.Equal(t, "text", got.Families["TEXT"].Family)
}
