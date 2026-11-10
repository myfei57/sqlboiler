package boilingcore

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/aarondl/sqlboiler/v4/drivers"
	"github.com/aarondl/sqlboiler/v4/importers"
	"github.com/aarondl/sqlboiler/v4/schemacontract"
)

func TestSchemaContractManifest(t *testing.T) {
	domain := "a_domain"
	s := &State{
		Config: &Config{
			DriverName:   "mock",
			PkgName:      "models",
			Version:      "test",
			DriverConfig: drivers.Config{"dbname": "app", "pass": "secret"},
			Imports:      importers.NewDefaultImports(),
			TypeReplaces: []TypeReplace{
				{
					Match:   drivers.Column{DBType: "serial"},
					Replace: drivers.Column{Type: "excellent.Type"},
				},
			},
			Aliases: Aliases{Tables: map[string]TableAlias{
				"widgets": {UpSingular: "Widget"},
			}},
		},
		Tables: []drivers.Table{
			{
				Name: "widgets",
				Columns: []drivers.Column{
					{Name: "id", Type: "int", DBType: "serial", Nullable: false},
					{Name: "domain_col", Type: "int", DBType: "numeric", DomainName: &domain, Nullable: false},
				},
				PKey: &drivers.PrimaryKey{Name: "widgets_pkey", Columns: []string{"id"}},
			},
		},
	}

	require.NoError(t, s.processTypeReplacements())

	manifest, err := s.SchemaContractManifest()
	require.NoError(t, err)
	require.Equal(t, schemacontract.ManifestVersion, manifest.ManifestVersion)
	require.Equal(t, "mock", manifest.DriverName)
	require.NotEmpty(t, manifest.ConfigDigest)
	require.NotEmpty(t, manifest.AliasesDigest)

	// The serial column was replaced and carries the replacement digest.
	var idContract *schemacontract.ColumnContract
	for i := range manifest.Tables[0].Columns {
		if manifest.Tables[0].Columns[i].Name == "id" {
			idContract = &manifest.Tables[0].Columns[i]
		}
	}
	require.NotNil(t, idContract)
	require.True(t, idContract.TypeReplaced)
	require.NotEmpty(t, idContract.TypeReplaceDigest)
	require.Len(t, manifest.TypeReplaces, 1)
	require.Equal(t, idContract.TypeReplaceDigest, manifest.TypeReplaces[0].Digest)

	// The domain column was not replaced and keeps its domain descriptor.
	var domainContract *schemacontract.ColumnContract
	for i := range manifest.Tables[0].Columns {
		if manifest.Tables[0].Columns[i].Name == "domain_col" {
			domainContract = &manifest.Tables[0].Columns[i]
		}
	}
	require.NotNil(t, domainContract)
	require.False(t, domainContract.TypeReplaced)
	require.NotNil(t, domainContract.DomainName)
	require.Equal(t, "a_domain", *domainContract.DomainName)

	// Credentials are redacted but non-secret driver config survives.
	require.Equal(t, schemacontract.RedactedValue, manifest.Config.DriverConfig["pass"])
	require.Equal(t, "app", manifest.Config.DriverConfig["dbname"])

	// Encoding is deterministic across repeated calls.
	first, err := schemacontract.EncodeManifest(manifest)
	require.NoError(t, err)
	second, err := schemacontract.EncodeManifest(manifest)
	require.NoError(t, err)
	require.Equal(t, string(first), string(second))
}

func TestSchemaContractManifestUninitialized(t *testing.T) {
	var s *State
	_, err := s.SchemaContractManifest()
	require.Error(t, err)
}
