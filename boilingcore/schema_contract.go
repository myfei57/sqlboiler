package boilingcore

import (
	"github.com/friendsofgo/errors"

	"github.com/aarondl/sqlboiler/v4/drivers"
	"github.com/aarondl/sqlboiler/v4/schemacontract"
)

// SchemaContractManifest builds a deterministic, versioned schema contract
// from the exact post-introspection, post-type-replacement state that
// templates are rendered against. It neither opens a database connection nor
// writes any generated file; credentials inside DriverConfig are redacted by
// schemacontract.Export and the alias mapping is represented by a digest.
func (s *State) SchemaContractManifest() (*schemacontract.Manifest, error) {
	if s == nil || s.Config == nil {
		return nil, errors.New("boilingcore: state is not initialized")
	}

	typeReplaces := make([]schemacontract.TypeReplaceInput, 0, len(s.Config.TypeReplaces))
	for _, r := range s.Config.TypeReplaces {
		typeReplaces = append(typeReplaces, schemacontract.TypeReplaceInput{
			Tables:  append([]string(nil), r.Tables...),
			Views:   append([]string(nil), r.Views...),
			Match:   r.Match,
			Replace: r.Replace,
		})
	}

	opts := schemacontract.ExportOptions{
		DriverName:       s.Config.DriverName,
		GeneratorName:    "sqlboiler",
		GeneratorVersion: s.Config.Version,
		Schema:           s.Schema,
		Dialect:          s.Dialect,
		Tables:           s.Tables,
		PkgName:          s.Config.PkgName,
		Settings:         generationSettings(s.Config),
		DriverConfig:     s.Config.DriverConfig,
		Aliases:          s.Config.Aliases,
		TypeReplaces:     typeReplaces,
		ReplacedColumns:  s.schemaContractReplaced,
	}

	return schemacontract.Export(opts)
}

// generationSettings captures the generation-affecting flags. Secrets are not
// part of these settings; DriverConfig is redacted separately during export.
func generationSettings(c *Config) map[string]any {
	return map[string]any{
		"add_global":                c.AddGlobal,
		"add_panic":                 c.AddPanic,
		"add_soft_deletes":          c.AddSoftDeletes,
		"add_enum_types":            c.AddEnumTypes,
		"skip_replaced_enum_types":  c.SkipReplacedEnumTypes,
		"enum_null_prefix":          c.EnumNullPrefix,
		"no_context":                c.NoContext,
		"no_tests":                  c.NoTests,
		"no_hooks":                  c.NoHooks,
		"no_auto_timestamps":        c.NoAutoTimestamps,
		"no_rows_affected":          c.NoRowsAffected,
		"no_driver_templates":       c.NoDriverTemplates,
		"no_back_referencing":       c.NoBackReferencing,
		"no_relation_getters":       c.NoRelationGetters,
		"always_wrap_errors":        c.AlwaysWrapErrors,
		"struct_tag_casing":         c.StructTagCasing,
		"relation_tag":              c.RelationTag,
		"tag_ignore":                append([]string(nil), c.TagIgnore...),
		"tags":                      append([]string(nil), c.Tags...),
		"auto_columns":              c.AutoColumns,
		"strict_verify_mod_version": c.StrictVerifyModVersion,
	}
}

// recordSchemaContractReplacement marks a column a TypeReplace was applied to,
// keyed the same way as schemacontract's replaced-column lookup.
func (s *State) recordSchemaContractReplacement(t drivers.Table, c drivers.Column, digest string) {
	if s.schemaContractReplaced == nil {
		s.schemaContractReplaced = make(map[string]string)
	}
	s.schemaContractReplaced[schemacontract.ReplacedColumnKey(t.SchemaName, t.Name, c.Name)] = digest
}
