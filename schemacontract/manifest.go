// Package schemacontract implements a versioned, deterministic schema
// contract manifest that records the database metadata sqlboiler actually
// used when generating models, and a read-only compatibility check that
// compares the manifest against a live database introspection.
//
// The package never executes DDL, never opens or mutates the global
// boil database handle and never writes generated model files. Secrets
// such as DSN passwords are redacted from the manifest before it is
// encoded.
package schemacontract

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"

	"github.com/friendsofgo/errors"

	"github.com/aarondl/sqlboiler/v4/drivers"
)

const (
	// ManifestFormat identifies the artifact type produced by Export.
	ManifestFormat = "sqlboiler-schema-contract"
	// ManifestVersion is the only manifest schema version understood by
	// this implementation. Bumping this value requires an explicit
	// migration path; older or newer versions are rejected up front.
	ManifestVersion = 1

	// Disclaimer is attached to every manifest and compatibility report so
	// that consumers cannot mistake structural metadata compatibility for a
	// guarantee that row-level data conversion or application queries succeed.
	Disclaimer = "Structural metadata comparison only. No DDL is executed and " +
		"the global database handle and generated files are not modified. A " +
		"compatible result does not prove that existing row values can be " +
		"converted, nor that application queries or data constraints remain valid."
)

// Sentinel errors returned before any database introspection happens, so that
// callers can distinguish a rejected artifact from a live database failure.
var (
	// ErrManifestCorrupt is returned when the manifest cannot be decoded or
	// fails structural validation.
	ErrManifestCorrupt = errors.New("schema contract manifest is corrupt")
	// ErrUnsupportedVersion is returned when the manifest version is missing
	// or not supported by this binary.
	ErrUnsupportedVersion = errors.New("schema contract manifest version is not supported")
)

// Manifest is the deterministic, versioned artifact describing the schema that
// was used during code generation.
type Manifest struct {
	// Format and ManifestVersion allow strict rejection of foreign or future
	// artifacts before any other field is trusted.
	Format          string `json:"format"`
	ManifestVersion int    `json:"manifest_version"`

	// DriverName selects the registered driver and its type rules.
	DriverName string `json:"driver_name"`
	// Generator records which binary produced the artifact.
	Generator GeneratorInfo `json:"generator"`
	// Schema is the database schema (namespace) used during generation. It is
	// kept separate from table names so same-name tables in different schemas
	// remain independent.
	Schema  string          `json:"schema"`
	Dialect drivers.Dialect `json:"dialect"`

	// Config is a redacted generation configuration snapshot and ConfigDigest
	// is the sha256 digest of its canonical JSON encoding.
	Config       GenerationConfig `json:"config"`
	ConfigDigest string           `json:"config_digest"`
	// AliasesDigest is the sha256 digest of the fully resolved alias mapping.
	// The full alias mapping is intentionally not duplicated: the digest lets
	// consumers detect alias drift without embedding generated Go names.
	AliasesDigest string `json:"aliases_digest"`

	// TypeReplaces records every custom type replacement that was applied so
	// that the check can verify the replacement target still matches instead of
	// silently assuming custom types are compatible.
	TypeReplaces []TypeReplaceContract `json:"type_replaces"`

	// Tables is sorted by (schema_name, name). Column and primary key ordering
	// inside a table is preserved verbatim and must not be normalized away.
	Tables []TableContract `json:"tables"`

	// Disclaimer is embedded in every artifact on purpose.
	Disclaimer string `json:"disclaimer"`
}

// GeneratorInfo identifies the producing binary.
type GeneratorInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// GenerationConfig captures the generation-affecting settings without any
// credentials. DriverConfig is redacted during export.
type GenerationConfig struct {
	PkgName  string         `json:"pkg_name,omitempty"`
	Settings map[string]any `json:"settings,omitempty"`
	// DriverConfig holds non-secret driver settings such as schema, whitelist
	// or blacklist. Secret values are replaced with RedactedValue.
	DriverConfig map[string]any `json:"driver_config,omitempty"`
}

// RedactedValue replaces credential material in the redacted config.
const RedactedValue = "<redacted>"

// TypeReplaceContract records a configured custom type replacement.
type TypeReplaceContract struct {
	// Digest uniquely identifies the replacement specification.
	Digest  string         `json:"digest"`
	Tables  []string       `json:"tables,omitempty"`
	Views   []string       `json:"views,omitempty"`
	Match   ColumnContract `json:"match"`
	Replace ColumnContract `json:"replace"`
}

// TableContract is the generation-time contract for a single table or view.
type TableContract struct {
	Name       string `json:"name"`
	SchemaName string `json:"schema_name,omitempty"`

	IsView      bool `json:"is_view"`
	IsJoinTable bool `json:"is_join_table"`

	ViewCapabilities ViewCapabilities `json:"view_capabilities"`

	// Columns stay in database ordinal order.
	Columns []ColumnContract `json:"columns"`
	// PrimaryKey column order is meaningful and is preserved verbatim.
	PrimaryKey *PrimaryKeyContract `json:"primary_key,omitempty"`

	ForeignKeys         []ForeignKeyContract   `json:"foreign_keys"`
	ToOneRelationships  []RelationshipContract `json:"to_one_relationships"`
	ToManyRelationships []RelationshipContract `json:"to_many_relationships"`
}

// ViewCapabilities mirrors drivers.ViewCapabilities.
type ViewCapabilities struct {
	CanInsert bool `json:"can_insert"`
	CanUpsert bool `json:"can_upsert"`
}

// ColumnContract is the generation-time contract for a single column. Ordinal
// records the database ordinal position (zero based); the slice order in
// TableContract.Columns matches it.
type ColumnContract struct {
	Name    string `json:"name"`
	Ordinal int    `json:"ordinal"`

	// Type is the Go type that was actually emitted into the generated model.
	Type string `json:"type"`
	// DBType/FullDBType/UDTName/ArrType/DomainName are the raw database-side
	// type descriptors used by the driver-specific compatibility rules.
	DBType     string  `json:"db_type"`
	FullDBType string  `json:"full_db_type,omitempty"`
	UDTName    string  `json:"udt_name,omitempty"`
	ArrType    *string `json:"arr_type,omitempty"`
	DomainName *string `json:"domain_name,omitempty"`

	// Nullable and Unique are structural constraints.
	Nullable bool `json:"nullable"`
	Unique   bool `json:"unique"`

	// Default is the default expression verbatim, including expression
	// punctuation. An empty string means no default exists.
	Default string `json:"default"`
	// AutoGenerated marks generated/identity/rowid columns. Generated columns
	// are excluded from generated INSERT column lists.
	AutoGenerated bool `json:"auto_generated"`

	// TypeReplaced marks columns whose Go type was produced by a configured
	// TypeReplace. Such columns can never be auto-compatible: the recorded
	// replacement must still match at check time or the result is unknown.
	TypeReplaced bool `json:"type_replaced,omitempty"`
	// TypeReplaceDigest references the TypeReplaceContract that produced the
	// Go type when TypeReplaced is true.
	TypeReplaceDigest string `json:"type_replace_digest,omitempty"`
}

// PrimaryKeyContract preserves the ordered primary key column list.
type PrimaryKeyContract struct {
	Name    string   `json:"name,omitempty"`
	Columns []string `json:"columns"`
}

// ForeignKeyContract is the generation-time contract for a foreign key.
type ForeignKeyContract struct {
	Name   string `json:"name"`
	Column string `json:"column"`

	Nullable bool `json:"nullable"`
	Unique   bool `json:"unique"`

	ForeignTable          string `json:"foreign_table"`
	ForeignColumn         string `json:"foreign_column"`
	ForeignColumnNullable bool   `json:"foreign_column_nullable"`
	ForeignColumnUnique   bool   `json:"foreign_column_unique"`
}

// RelationshipContract records the target of a generated relationship so that
// retargeted or dropped relationships are detectable.
type RelationshipContract struct {
	Name   string `json:"name"`
	Table  string `json:"table"`
	Column string `json:"column"`

	Nullable bool `json:"nullable"`
	Unique   bool `json:"unique"`

	ForeignTable          string `json:"foreign_table"`
	ForeignColumn         string `json:"foreign_column"`
	ForeignColumnNullable bool   `json:"foreign_column_nullable"`
	ForeignColumnUnique   bool   `json:"foreign_column_unique"`

	ToJoinTable bool   `json:"to_join_table,omitempty"`
	JoinTable   string `json:"join_table,omitempty"`

	JoinLocalFKeyName       string `json:"join_local_fkey_name,omitempty"`
	JoinLocalColumn         string `json:"join_local_column,omitempty"`
	JoinLocalColumnNullable bool   `json:"join_local_column_nullable,omitempty"`
	JoinLocalColumnUnique   bool   `json:"join_local_column_unique,omitempty"`

	JoinForeignFKeyName       string `json:"join_foreign_fkey_name,omitempty"`
	JoinForeignColumn         string `json:"join_foreign_column,omitempty"`
	JoinForeignColumnNullable bool   `json:"join_foreign_column_nullable,omitempty"`
	JoinForeignColumnUnique   bool   `json:"join_foreign_column_unique,omitempty"`
}

// EncodeManifest validates and deterministically encodes the manifest. The
// output is stable across repeated calls for the same input: map keys are
// emitted sorted by encoding/json, slices are emitted in their preserved
// order and no timestamps or process-local data are included.
func EncodeManifest(m *Manifest) ([]byte, error) {
	if err := validateManifest(m); err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return nil, errors.Wrap(ErrManifestCorrupt, err.Error())
	}
	return buf.Bytes(), nil
}

// DecodeManifest strictly decodes a manifest and rejects corrupt artifacts and
// unsupported versions before returning, so callers never introspect a database
// based on an untrusted manifest.
func DecodeManifest(r io.Reader) (*Manifest, error) {
	if r == nil {
		return nil, errors.Wrap(ErrManifestCorrupt, "manifest reader is nil")
	}

	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()

	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, errors.Wrap(ErrManifestCorrupt, err.Error())
	}
	if dec.More() {
		return nil, errors.Wrap(ErrManifestCorrupt, "trailing data after manifest document")
	}

	if m.Format != ManifestFormat {
		return nil, errors.Wrapf(ErrManifestCorrupt, "unexpected manifest format %q", m.Format)
	}
	if m.ManifestVersion == 0 {
		return nil, errors.Wrap(ErrUnsupportedVersion, "manifest_version is missing")
	}
	if m.ManifestVersion != ManifestVersion {
		return nil, errors.Wrapf(ErrUnsupportedVersion, "manifest_version %d, supported version %d", m.ManifestVersion, ManifestVersion)
	}
	if err := validateManifest(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

// validateManifest enforces the invariants the diff engine relies on: unique
// table identity (schema + name), unique column names within a table and
// preserved primary key ordering.
func validateManifest(m *Manifest) error {
	if m == nil {
		return errors.Wrap(ErrManifestCorrupt, "manifest is nil")
	}
	if m.ManifestVersion != ManifestVersion {
		return errors.Wrapf(ErrUnsupportedVersion, "manifest_version %d, supported version %d", m.ManifestVersion, ManifestVersion)
	}
	if m.DriverName == "" {
		return errors.Wrap(ErrManifestCorrupt, "driver_name is required")
	}
	if len(m.Tables) == 0 {
		return errors.Wrap(ErrManifestCorrupt, "manifest contains no tables")
	}

	seenTables := make(map[string]struct{}, len(m.Tables))
	for i := range m.Tables {
		t := &m.Tables[i]
		if t.Name == "" {
			return errors.Wrap(ErrManifestCorrupt, "table name is required")
		}
		key := tableKey(t.SchemaName, t.Name)
		if _, ok := seenTables[key]; ok {
			return errors.Wrapf(ErrManifestCorrupt, "duplicate table in manifest: %s", key)
		}
		seenTables[key] = struct{}{}

		seenCols := make(map[string]struct{}, len(t.Columns))
		for j := range t.Columns {
			c := &t.Columns[j]
			if c.Name == "" {
				return errors.Wrapf(ErrManifestCorrupt, "column name is required in table %s", key)
			}
			if c.Ordinal != j {
				return errors.Wrapf(ErrManifestCorrupt, "column %s.%s ordinal %d does not match position %d", key, c.Name, c.Ordinal, j)
			}
			if _, ok := seenCols[c.Name]; ok {
				return errors.Wrapf(ErrManifestCorrupt, "duplicate column %s in table %s", c.Name, key)
			}
			seenCols[c.Name] = struct{}{}
		}

		if t.PrimaryKey != nil {
			if len(t.PrimaryKey.Columns) == 0 {
				return errors.Wrapf(ErrManifestCorrupt, "empty primary key for table %s", key)
			}
			for _, name := range t.PrimaryKey.Columns {
				if _, ok := seenCols[name]; !ok {
					return errors.Wrapf(ErrManifestCorrupt, "primary key column %s not found in table %s", name, key)
				}
			}
		}
	}
	return nil
}

// canonicalDigest returns the sha256 hex digest of the canonical JSON encoding
// of v. encoding/json emits map keys in sorted order, so structurally equal
// values always produce the same digest regardless of Go map iteration.
func canonicalDigest(prefix string, v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", errors.Wrapf(err, "failed to encode %s for digest", prefix)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// tableKey is the identity of a table or view. Same-name tables in different
// schemas never collide.
func tableKey(schema, name string) string {
	return fmt.Sprintf("%s.%s", schema, name)
}
