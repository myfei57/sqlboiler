package schemacontract

import (
	"sort"
	"strings"

	"github.com/friendsofgo/errors"

	"github.com/aarondl/sqlboiler/v4/drivers"
)

// ExportOptions are the inputs for Export. They describe the exact metadata
// and configuration that were used for a code generation run. Export does not
// open any database connection and does not touch generated files.
type ExportOptions struct {
	DriverName       string
	GeneratorName    string
	GeneratorVersion string
	Schema           string
	Dialect          drivers.Dialect
	// Tables must be the post-whitelist/blacklist, post-type-replacement
	// tables that templates were rendered against.
	Tables []drivers.Table

	// PkgName and Settings describe non-driver generation settings.
	PkgName  string
	Settings map[string]any
	// DriverConfig is redacted before it is stored: any key looking like a
	// credential or DSN string is replaced with RedactedValue.
	DriverConfig drivers.Config

	// Aliases is the fully resolved alias mapping (any JSON-marshalable value
	// works; the boilingcore Aliases type is expected in practice).
	Aliases any

	// TypeReplaces are the configured custom type replacements.
	TypeReplaces []TypeReplaceInput
	// ReplacedColumns maps "schema.table.column" keys for every column a
	// TypeReplace was actually applied to, to the digest of that replacement.
	ReplacedColumns map[string]string
}

// TypeReplaceInput is the driver-agnostic representation of a configured
// type replacement.
type TypeReplaceInput struct {
	Tables  []string
	Views   []string
	Match   drivers.Column
	Replace drivers.Column
}

// Export builds a deterministic, versioned manifest from generation-time
// inputs. Repeated calls with equal inputs produce byte-identical manifests.
func Export(opts ExportOptions) (*Manifest, error) {
	if opts.DriverName == "" {
		return nil, errors.New("schemacontract: driver name is required to export a manifest")
	}
	if len(opts.Tables) == 0 {
		return nil, errors.New("schemacontract: at least one table is required to export a manifest")
	}

	redactedDriverConfig := redactConfig(opts.DriverConfig)
	genConfig := GenerationConfig{
		PkgName:      opts.PkgName,
		Settings:     cloneSettings(opts.Settings),
		DriverConfig: redactedDriverConfig,
	}
	configDigest, err := canonicalDigest("generation config", genConfig)
	if err != nil {
		return nil, err
	}
	aliasesDigest, err := canonicalDigest("alias mapping", opts.Aliases)
	if err != nil {
		return nil, err
	}

	typeReplaces, err := buildTypeReplaceContracts(opts.TypeReplaces)
	if err != nil {
		return nil, err
	}

	tables := buildTableContracts(opts.Tables, opts.ReplacedColumns)

	m := &Manifest{
		Format:          ManifestFormat,
		ManifestVersion: ManifestVersion,
		DriverName:      opts.DriverName,
		Generator: GeneratorInfo{
			Name:    opts.GeneratorName,
			Version: opts.GeneratorVersion,
		},
		Schema:        opts.Schema,
		Dialect:       opts.Dialect,
		Config:        genConfig,
		ConfigDigest:  configDigest,
		AliasesDigest: aliasesDigest,
		TypeReplaces:  typeReplaces,
		Tables:        tables,
		Disclaimer:    Disclaimer,
	}

	if err := validateManifest(m); err != nil {
		return nil, err
	}
	return m, nil
}

// redactConfig returns a copy of cfg where credential-like values are
// redacted. The presence of the key is preserved (its absence versus an empty
// value can be meaningful) but the secret value is never stored.
func redactConfig(cfg drivers.Config) map[string]any {
	if cfg == nil {
		return nil
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		if isSecretKey(k) {
			out[k] = RedactedValue
			continue
		}
		out[k] = v
	}
	return out
}

// secretKeyFragments is intentionally conservative: matching a fragment means
// the value could carry a credential, so the value is never written.
var secretKeyFragments = []string{
	"pass", "password", "secret", "dsn", "token", "credential",
}

func isSecretKey(key string) bool {
	lower := strings.ToLower(key)
	for _, fragment := range secretKeyFragments {
		if strings.Contains(lower, fragment) {
			return true
		}
	}
	return false
}

func cloneSettings(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func buildTypeReplaceInputs(in []TypeReplaceInput) []TypeReplaceInput {
	out := make([]TypeReplaceInput, len(in))
	copy(out, in)
	sort.Slice(out, func(i, j int) bool {
		return typeReplaceIdentity(out[i]) < typeReplaceIdentity(out[j])
	})
	return out
}

func typeReplaceIdentity(r TypeReplaceInput) string {
	return strings.Join(append(append([]string{}, r.Tables...), r.Match.DBType, r.Match.UDTName, r.Match.Type, r.Replace.Type), "\x00")
}

func buildTypeReplaceContracts(in []TypeReplaceInput) ([]TypeReplaceContract, error) {
	if len(in) == 0 {
		return nil, nil
	}
	ordered := buildTypeReplaceInputs(in)
	out := make([]TypeReplaceContract, 0, len(ordered))
	for _, r := range ordered {
		contract, err := typeReplaceContract(r)
		if err != nil {
			return nil, err
		}
		out = append(out, contract)
	}
	return out, nil
}

// ComputeTypeReplaceDigest returns the stable digest of a type replacement
// specification. Callers tracking which columns a replacement was applied to
// use the same digest as ColumnContract.TypeReplaceDigest.
func ComputeTypeReplaceDigest(r TypeReplaceInput) (string, error) {
	contract, err := typeReplaceContract(r)
	if err != nil {
		return "", err
	}
	return contract.Digest, nil
}

func typeReplaceContract(r TypeReplaceInput) (TypeReplaceContract, error) {
	tables := append([]string(nil), r.Tables...)
	views := append([]string(nil), r.Views...)
	sort.Strings(tables)
	sort.Strings(views)

	contract := TypeReplaceContract{
		Tables:  tables,
		Views:   views,
		Match:   columnContractFromDriver(r.Match, 0, false),
		Replace: columnContractFromDriver(r.Replace, 0, false),
	}
	digest, err := canonicalDigest("type replacement", contract)
	if err != nil {
		return TypeReplaceContract{}, err
	}
	contract.Digest = digest
	return contract, nil
}

func buildTableContracts(tables []drivers.Table, replaced map[string]string) []TableContract {
	out := make([]TableContract, 0, len(tables))
	for _, t := range tables {
		contract := TableContract{
			Name:             t.Name,
			SchemaName:       t.SchemaName,
			IsView:           t.IsView,
			IsJoinTable:      t.IsJoinTable,
			ViewCapabilities: ViewCapabilities{CanInsert: t.ViewCapabilities.CanInsert, CanUpsert: t.ViewCapabilities.CanUpsert},
		}

		contract.Columns = make([]ColumnContract, len(t.Columns))
		for i, c := range t.Columns {
			digest, isReplaced := replaced[replacedColumnKey(t.SchemaName, t.Name, c.Name)]
			col := columnContractFromDriver(c, i, isReplaced)
			col.TypeReplaceDigest = digest
			contract.Columns[i] = col
		}

		if t.PKey != nil {
			// Defensive copy: callers must not be able to mutate the manifest
			// by reusing the driver slices.
			columns := append([]string(nil), t.PKey.Columns...)
			contract.PrimaryKey = &PrimaryKeyContract{Name: t.PKey.Name, Columns: columns}
		}

		contract.ForeignKeys = buildForeignKeyContracts(t.FKeys)
		contract.ToOneRelationships = buildToOneRelationshipContracts(t.ToOneRelationships)
		contract.ToManyRelationships = buildToManyRelationshipContracts(t.ToManyRelationships)

		out = append(out, contract)
	}

	// Table order is normalized for determinism, but the ordering *inside* a
	// table (columns, primary key columns) stays untouched.
	sort.Slice(out, func(i, j int) bool {
		if out[i].SchemaName != out[j].SchemaName {
			return out[i].SchemaName < out[j].SchemaName
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// ReplacedColumnKey returns the stable key identifying a column a TypeReplace
// was applied to, usable across package boundaries.
func ReplacedColumnKey(schema, table, column string) string {
	return tableKey(schema, table) + "." + column
}

func replacedColumnKey(schema, table, column string) string {
	return ReplacedColumnKey(schema, table, column)
}

func columnContractFromDriver(c drivers.Column, ordinal int, isReplaced bool) ColumnContract {
	return ColumnContract{
		Name:          c.Name,
		Ordinal:       ordinal,
		Type:          c.Type,
		DBType:        c.DBType,
		FullDBType:    c.FullDBType,
		UDTName:       c.UDTName,
		ArrType:       copyStringPtr(c.ArrType),
		DomainName:    copyStringPtr(c.DomainName),
		Nullable:      c.Nullable,
		Unique:        c.Unique,
		Default:       c.Default,
		AutoGenerated: c.AutoGenerated,
		TypeReplaced:  isReplaced,
	}
}

func copyStringPtr(s *string) *string {
	if s == nil {
		return nil
	}
	v := *s
	return &v
}

func buildForeignKeyContracts(fkeys []drivers.ForeignKey) []ForeignKeyContract {
	if len(fkeys) == 0 {
		return nil
	}
	out := make([]ForeignKeyContract, 0, len(fkeys))
	for _, f := range fkeys {
		out = append(out, ForeignKeyContract{
			Name:                  f.Name,
			Column:                f.Column,
			Nullable:              f.Nullable,
			Unique:                f.Unique,
			ForeignTable:          f.ForeignTable,
			ForeignColumn:         f.ForeignColumn,
			ForeignColumnNullable: f.ForeignColumnNullable,
			ForeignColumnUnique:   f.ForeignColumnUnique,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Column < out[j].Column
	})
	return out
}

func buildToOneRelationshipContracts(rels []drivers.ToOneRelationship) []RelationshipContract {
	if len(rels) == 0 {
		return nil
	}
	out := make([]RelationshipContract, 0, len(rels))
	for _, r := range rels {
		out = append(out, RelationshipContract{
			Name:                  r.Name,
			Table:                 r.Table,
			Column:                r.Column,
			Nullable:              r.Nullable,
			Unique:                r.Unique,
			ForeignTable:          r.ForeignTable,
			ForeignColumn:         r.ForeignColumn,
			ForeignColumnNullable: r.ForeignColumnNullable,
			ForeignColumnUnique:   r.ForeignColumnUnique,
		})
	}
	sortRelationshipContracts(out)
	return out
}

func buildToManyRelationshipContracts(rels []drivers.ToManyRelationship) []RelationshipContract {
	if len(rels) == 0 {
		return nil
	}
	out := make([]RelationshipContract, 0, len(rels))
	for _, r := range rels {
		out = append(out, RelationshipContract{
			Name:                      r.Name,
			Table:                     r.Table,
			Column:                    r.Column,
			Nullable:                  r.Nullable,
			Unique:                    r.Unique,
			ForeignTable:              r.ForeignTable,
			ForeignColumn:             r.ForeignColumn,
			ForeignColumnNullable:     r.ForeignColumnNullable,
			ForeignColumnUnique:       r.ForeignColumnUnique,
			ToJoinTable:               r.ToJoinTable,
			JoinTable:                 r.JoinTable,
			JoinLocalFKeyName:         r.JoinLocalFKeyName,
			JoinLocalColumn:           r.JoinLocalColumn,
			JoinLocalColumnNullable:   r.JoinLocalColumnNullable,
			JoinLocalColumnUnique:     r.JoinLocalColumnUnique,
			JoinForeignFKeyName:       r.JoinForeignFKeyName,
			JoinForeignColumn:         r.JoinForeignColumn,
			JoinForeignColumnNullable: r.JoinForeignColumnNullable,
			JoinForeignColumnUnique:   r.JoinForeignColumnUnique,
		})
	}
	sortRelationshipContracts(out)
	return out
}

func sortRelationshipContracts(rels []RelationshipContract) {
	sort.Slice(rels, func(i, j int) bool {
		if rels[i].Name != rels[j].Name {
			return rels[i].Name < rels[j].Name
		}
		if rels[i].ForeignTable != rels[j].ForeignTable {
			return rels[i].ForeignTable < rels[j].ForeignTable
		}
		return rels[i].ForeignColumn < rels[j].ForeignColumn
	})
}
