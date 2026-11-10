package schemacontract

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/friendsofgo/errors"

	"github.com/aarondl/sqlboiler/v4/drivers"
)

// Severity classifies a difference for CLI/report consumers.
type Severity string

const (
	// SeverityError describes a difference that breaks generated operations.
	SeverityError Severity = "error"
	// SeverityWarning describes a risky but not necessarily failing difference.
	SeverityWarning Severity = "warning"
	// SeverityInfo describes a difference with no impact on generated code.
	SeverityInfo Severity = "info"
	// SeverityUnknown marks a difference that requires explicit human or
	// driver-rule adaptation.
	SeverityUnknown Severity = "unknown"
)

// Impact describes the effect of a difference on a class of operations.
type Impact string

const (
	// ImpactBreaks means the generated operation is structurally guaranteed to
	// fail against the live schema.
	ImpactBreaks Impact = "breaks"
	// ImpactRisk means the generated operation can fail depending on the data
	// or usage.
	ImpactRisk Impact = "risk"
	// ImpactUnknown means no driver rule can prove the operation safe.
	ImpactUnknown Impact = "unknown"
	// ImpactSafe means the difference does not affect the operation.
	ImpactSafe Impact = "safe"
)

// Stable difference kind identifiers used by tests and consumers.
const (
	KindMissingTable           = "missing_table"
	KindExtraTable             = "extra_table"
	KindTableViewMismatch      = "table_view_mismatch"
	KindMissingColumn          = "missing_column"
	KindExtraUnusedColumn      = "extra_unused_column"
	KindRequiredColumnAdded    = "required_column_added"
	KindColumnTypeWidened      = "column_type_widened"
	KindColumnTypeNarrowed     = "column_type_narrowed"
	KindColumnTypeIncompatible = "column_type_incompatible"
	KindColumnTypeUnknown      = "column_type_unknown"
	KindNullableTightened      = "nullable_tightened"
	KindNullableRelaxed        = "nullable_relaxed"
	KindDefaultChanged         = "default_changed"
	KindGeneratedChanged       = "generated_changed"
	KindUniqueChanged          = "unique_changed"
	KindColumnOrderChanged     = "column_order_changed"
	KindPrimaryKeyMissing      = "primary_key_missing"
	KindPrimaryKeyChanged      = "primary_key_changed"
	KindPrimaryKeyOrderChanged = "primary_key_order_changed"
	KindForeignKeyMissing      = "foreign_key_missing"
	KindForeignKeyRetargeted   = "foreign_key_retargeted"
	KindForeignKeyChanged      = "foreign_key_changed"
	KindExtraForeignKey        = "extra_foreign_key"
	KindRelationshipMissing    = "relationship_missing"
	KindRelationshipRetargeted = "relationship_retargeted"
	KindViewInsertCapability   = "view_insert_capability_changed"
	KindViewUpsertCapability   = "view_upsert_capability_changed"
	KindTypeReplaceTargetDrift = "type_replace_target_drift"
)

// Difference is a single manifest-versus-live discrepancy with a precise
// schema path, the read/write operations it affects and the evidence that
// establishes it.
type Difference struct {
	// Path is a deterministic schema path, for example
	// tables[public.users].columns.email.
	Path string `json:"path"`
	// Kind is one of the Kind* constants.
	Kind string `json:"kind"`
	// Severity is one of the Severity* constants.
	Severity Severity `json:"severity"`
	// ReadImpact and WriteImpact classify the effect on generated Select/Find
	// and Insert/Update/Delete operations respectively.
	ReadImpact  Impact `json:"read_impact"`
	WriteImpact Impact `json:"write_impact"`
	// Message is a human-readable explanation.
	Message string `json:"message"`
	// Evidence carries the expected and actual values verbatim.
	Evidence Evidence `json:"evidence"`
}

// Evidence records both sides of a comparison without normalization.
type Evidence struct {
	Expected string `json:"expected,omitempty"`
	Actual   string `json:"actual,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// Report is the result of CheckSchemaCompatibility.
type Report struct {
	Format          string `json:"format"`
	ManifestVersion int    `json:"manifest_version"`
	DriverName      string `json:"driver_name"`
	Schema          string `json:"schema"`
	// ReadCompatible is true only when no difference breaks, risks or leaves
	// unknown the generated read operations.
	ReadCompatible bool `json:"read_compatible"`
	// WriteCompatible is true only when no difference breaks, risks or leaves
	// unknown the generated write operations.
	WriteCompatible bool `json:"write_compatible"`
	// HasUnknowns reports differences that explicit rules could not decide.
	HasUnknowns bool `json:"has_unknowns"`
	// Differences is sorted by path and kind for deterministic output.
	Differences []Difference `json:"differences"`
	// Disclaimer is repeated on every report.
	Disclaimer string `json:"disclaimer"`
}

// EncodeReport deterministically encodes a compatibility report with HTML
// escaping disabled, matching EncodeManifest's output style.
func EncodeReport(report *Report) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		return nil, errors.Wrap(err, "failed to encode compatibility report")
	}
	return buf.Bytes(), nil
}

// CheckOptions configures CheckSchemaCompatibility. Either Reader or Manifest
// must be supplied; Reader is decoded and validated first so corrupt or
// unsupported manifests are rejected without any database access.
type CheckOptions struct {
	// Context cancels the comparison. The driver introspection call owns its
	// own Rows and closes them on return; a cancelled context prevents further
	// table processing in this engine.
	Context context.Context
	// Reader holds a serialized manifest.
	Reader io.Reader
	// Manifest holds an already decoded manifest. Ignored when Reader is set.
	Manifest *Manifest
	// Driver performs the fresh, read-only introspection. The check never
	// registers, replaces or closes a global database handle.
	Driver drivers.Interface
	// DriverConfig is passed verbatim to Driver.Assemble and should carry
	// current credentials; nothing secret is read from the manifest.
	DriverConfig drivers.Config
	// Rules optionally overrides the driver-name based rule lookup.
	Rules TypeRules
}

// CheckSchemaCompatibility decodes and validates the manifest, introspects the
// live database through the supplied driver using read-only driver behavior,
// and returns a structured diff with per-operation impact classification.
//
// It never executes DDL, never mutates generated files and never touches the
// global boil database handle. A compatible report is a structural statement
// only and does not prove row-level data conversion succeeds.
func CheckSchemaCompatibility(opts CheckOptions) (*Report, error) {
	if opts.Driver == nil {
		return nil, errors.New("schemacontract: a driver is required for schema compatibility checks")
	}

	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Wrap(err, "schemacontract: check cancelled before introspection")
	}

	// Reject corrupt or unsupported manifests before touching the database.
	manifest := opts.Manifest
	if opts.Reader != nil {
		decoded, err := DecodeManifest(opts.Reader)
		if err != nil {
			return nil, err
		}
		manifest = decoded
	}
	if manifest == nil {
		return nil, errors.Wrap(ErrManifestCorrupt, "no manifest supplied")
	}
	if err := validateManifest(manifest); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Wrap(err, "schemacontract: check cancelled before introspection")
	}

	// Fresh introspection. Drivers open and close their own *sql.DB and close
	// Rows on error; this engine holds no Rows itself.
	dbInfo, err := opts.Driver.Assemble(opts.DriverConfig)
	if err != nil {
		return nil, errors.Wrap(err, "schemacontract: live database introspection failed")
	}
	if dbInfo == nil {
		return nil, errors.New("schemacontract: driver returned no database information")
	}

	rules := opts.Rules
	if rules == nil {
		rules, _ = TypeRulesFor(manifest.DriverName)
	}
	if rules == nil {
		// Binary drivers expose explicit rules through the driver protocol.
		// A provider that errors or describes another driver is ignored: the
		// safe fallback is to report unknown, never to assume compatibility.
		if provider, ok := opts.Driver.(drivers.TypeRuleProvider); ok {
			if set, err := provider.TypeRules(); err == nil && set != nil && set.Driver == manifest.DriverName {
				rules = TypeRulesFromDriverSet(set)
			}
		}
	}

	engine := &checkEngine{
		ctx:      ctx,
		manifest: manifest,
		live:     dbInfo,
		rules:    rules,
	}
	differences, err := engine.diff()
	if err != nil {
		return nil, err
	}

	sort.SliceStable(differences, func(i, j int) bool {
		if differences[i].Path != differences[j].Path {
			return differences[i].Path < differences[j].Path
		}
		return differences[i].Kind < differences[j].Kind
	})

	report := &Report{
		Format:          ManifestFormat,
		ManifestVersion: manifest.ManifestVersion,
		DriverName:      manifest.DriverName,
		Schema:          manifest.Schema,
		Differences:     differences,
		Disclaimer:      Disclaimer,
	}

	report.ReadCompatible, report.WriteCompatible, report.HasUnknowns = summarize(differences)
	return report, nil
}

type checkEngine struct {
	ctx      context.Context
	manifest *Manifest
	live     *drivers.DBInfo
	rules    TypeRules
}

func (e *checkEngine) diff() ([]Difference, error) {
	liveTables := make(map[string]drivers.Table, len(e.live.Tables))
	for _, t := range e.live.Tables {
		liveTables[tableKey(t.SchemaName, t.Name)] = t
	}

	var differences []Difference
	for i := range e.manifest.Tables {
		if err := e.ctx.Err(); err != nil {
			return nil, errors.Wrap(err, "schemacontract: check cancelled during comparison")
		}

		expected := e.manifest.Tables[i]
		key := tableKey(expected.SchemaName, expected.Name)
		path := tablePath(expected.SchemaName, expected.Name)

		actual, ok := liveTables[key]
		if !ok {
			differences = append(differences, Difference{
				Path:        path,
				Kind:        KindMissingTable,
				Severity:    SeverityError,
				ReadImpact:  ImpactBreaks,
				WriteImpact: ImpactBreaks,
				Message:     fmt.Sprintf("table or view %s used by the generated models no longer exists", key),
				Evidence:    Evidence{Expected: tableEvidence(expected), Actual: "<absent>"},
			})
			continue
		}

		differences = append(differences, e.diffTable(path, expected, actual)...)
	}

	// Tables present in the database but absent from the manifest are not
	// modeled, so they cannot break generated code by themselves.
	manifestTables := make(map[string]struct{}, len(e.manifest.Tables))
	for _, t := range e.manifest.Tables {
		manifestTables[tableKey(t.SchemaName, t.Name)] = struct{}{}
	}
	liveKeys := make([]string, 0, len(liveTables))
	for key := range liveTables {
		liveKeys = append(liveKeys, key)
	}
	sort.Strings(liveKeys)
	for _, key := range liveKeys {
		if _, ok := manifestTables[key]; ok {
			continue
		}
		actual := liveTables[key]
		differences = append(differences, Difference{
			Path:        tablePath(actual.SchemaName, actual.Name),
			Kind:        KindExtraTable,
			Severity:    SeverityInfo,
			ReadImpact:  ImpactSafe,
			WriteImpact: ImpactSafe,
			Message:     fmt.Sprintf("unmodeled table or view %s exists in the live database and is ignored", key),
			Evidence:    Evidence{Expected: "<absent>", Actual: liveTableEvidence(actual)},
		})
	}

	return differences, nil
}

func (e *checkEngine) diffTable(path string, expected TableContract, actual drivers.Table) []Difference {
	var differences []Difference

	if expected.IsView != actual.IsView {
		differences = append(differences, Difference{
			Path:        path,
			Kind:        KindTableViewMismatch,
			Severity:    SeverityError,
			ReadImpact:  ImpactBreaks,
			WriteImpact: ImpactBreaks,
			Message:     "the relation kind changed between table and view",
			Evidence:    Evidence{Expected: kindEvidence(expected.IsView), Actual: kindEvidence(actual.IsView)},
		})
	}

	differences = append(differences, e.diffColumns(path, expected, actual)...)
	differences = append(differences, e.diffPrimaryKey(path, expected, actual)...)
	differences = append(differences, e.diffForeignKeys(path, expected, actual)...)
	differences = append(differences, e.diffRelationships(path, expected, actual)...)
	differences = append(differences, e.diffViewCapabilities(path, expected, actual)...)

	return differences
}

func (e *checkEngine) diffColumns(path string, expected TableContract, actual drivers.Table) []Difference {
	var differences []Difference

	actualColumns := make(map[string]drivers.Column, len(actual.Columns))
	for _, c := range actual.Columns {
		actualColumns[c.Name] = c
	}

	expectedNames := make([]string, 0, len(expected.Columns))
	actualNames := make([]string, 0, len(actual.Columns))
	for _, c := range expected.Columns {
		expectedNames = append(expectedNames, c.Name)
	}
	for _, c := range actual.Columns {
		actualNames = append(actualNames, c.Name)
	}

	for _, expCol := range expected.Columns {
		colPath := path + ".columns." + expCol.Name
		actCol, ok := actualColumns[expCol.Name]
		if !ok {
			// Generated queries explicitly name this column in insert/update,
			// reload and explicit select lists.
			differences = append(differences, Difference{
				Path:        colPath,
				Kind:        KindMissingColumn,
				Severity:    SeverityError,
				ReadImpact:  ImpactBreaks,
				WriteImpact: ImpactBreaks,
				Message:     fmt.Sprintf("column %s referenced by the generated models was dropped", expCol.Name),
				Evidence:    Evidence{Expected: columnSummary(expCol), Actual: "<absent>"},
			})
			continue
		}

		differences = append(differences, e.diffColumn(colPath, expCol, actCol)...)
	}

	// Extra live columns are tolerated for reads (generated selects name their
	// own columns / scan by name), but a new required column breaks inserts.
	for _, actCol := range actual.Columns {
		if _, ok := columnContractByName(expected.Columns, actCol.Name); ok {
			continue
		}
		colPath := path + ".columns." + actCol.Name
		required := !actCol.Nullable && !actCol.AutoGenerated && len(actCol.Default) == 0
		if required && !actual.IsView {
			differences = append(differences, Difference{
				Path:        colPath,
				Kind:        KindRequiredColumnAdded,
				Severity:    SeverityError,
				ReadImpact:  ImpactSafe,
				WriteImpact: ImpactBreaks,
				Message: fmt.Sprintf(
					"new NOT NULL column %s has no default and is not generated: generated INSERT statements omit it and will be rejected",
					actCol.Name),
				Evidence: Evidence{Expected: "<absent>", Actual: liveColumnSummary(actCol), Detail: "insert column lists only contain modeled columns"},
			})
		} else {
			differences = append(differences, Difference{
				Path:        colPath,
				Kind:        KindExtraUnusedColumn,
				Severity:    SeverityInfo,
				ReadImpact:  ImpactSafe,
				WriteImpact: ImpactSafe,
				Message:     fmt.Sprintf("unmodeled column %s is unused by the generated models", actCol.Name),
				Evidence:    Evidence{Expected: "<absent>", Actual: liveColumnSummary(actCol)},
			})
		}
	}

	// Ordinal order is meaningful metadata (struct field order, generated
	// column lists) even though set equality holds.
	if len(expectedNames) == len(actualNames) && !sameStringSlice(expectedNames, actualNames) &&
		sameStringSet(expectedNames, actualNames) {
		differences = append(differences, Difference{
			Path:        path + ".columns",
			Kind:        KindColumnOrderChanged,
			Severity:    SeverityInfo,
			ReadImpact:  ImpactSafe,
			WriteImpact: ImpactSafe,
			Message:     "column ordinal order changed while the column set stayed the same",
			Evidence:    Evidence{Expected: strings.Join(expectedNames, ", "), Actual: strings.Join(actualNames, ", ")},
		})
	}

	return differences
}

func (e *checkEngine) diffColumn(path string, expected ColumnContract, actual drivers.Column) []Difference {
	var differences []Difference

	// Custom TypeReplace columns are never auto-compatible: the recorded
	// replacement target must still match the live column.
	if expected.TypeReplaced {
		if !e.typeReplaceStillApplies(expected, actual) {
			differences = append(differences, Difference{
				Path:        path,
				Kind:        KindTypeReplaceTargetDrift,
				Severity:    SeverityUnknown,
				ReadImpact:  ImpactUnknown,
				WriteImpact: ImpactUnknown,
				Message:     "custom type replacement target changed; explicit adaptation is required",
				Evidence: Evidence{
					Expected: columnSummary(expected),
					Actual:   liveColumnSummary(actual),
					Detail:   "type_replace_digest=" + expected.TypeReplaceDigest,
				},
			})
		}
		// Even when the replacement still applies, structural constraints below
		// must still be checked.
	} else {
		differences = append(differences, e.diffColumnType(path, expected, actual)...)
	}

	// Nullability cannot be reduced to Go type string equality: the generated
	// Go type's null wrapper depends on it.
	if expected.Nullable && !actual.Nullable {
		differences = append(differences, Difference{
			Path:        path,
			Kind:        KindNullableTightened,
			Severity:    SeverityWarning,
			ReadImpact:  ImpactSafe,
			WriteImpact: ImpactRisk,
			Message:     "the live column became NOT NULL; generated inserts may still send NULL and be rejected",
			Evidence:    Evidence{Expected: "nullable", Actual: "not null"},
		})
	} else if !expected.Nullable && actual.Nullable {
		differences = append(differences, Difference{
			Path:        path,
			Kind:        KindNullableRelaxed,
			Severity:    SeverityWarning,
			ReadImpact:  ImpactRisk,
			WriteImpact: ImpactSafe,
			Message:     "the live column became nullable; NULL rows cannot scan into the generated non-nullable Go type",
			Evidence:    Evidence{Expected: "not null", Actual: "nullable"},
		})
	}

	if expected.Default != actual.Default {
		diff := Difference{
			Path:        path,
			Kind:        KindDefaultChanged,
			Severity:    SeverityWarning,
			ReadImpact:  ImpactSafe,
			WriteImpact: ImpactSafe,
			Message:     "default expression changed",
			Evidence:    Evidence{Expected: defaultEvidence(expected.Default), Actual: defaultEvidence(actual.Default)},
		}
		// The generated insert omits columns it believes have a default. If the
		// live default is gone on a required column, that insert now fails.
		if expected.Default != "" && actual.Default == "" && !actual.Nullable && !actual.AutoGenerated {
			diff.Severity = SeverityError
			diff.WriteImpact = ImpactBreaks
			diff.Message = "default was removed from a NOT NULL column; generated inserts omit it and will be rejected"
		}
		differences = append(differences, diff)
	}

	if expected.AutoGenerated != actual.AutoGenerated {
		diff := Difference{
			Path:        path,
			Kind:        KindGeneratedChanged,
			Severity:    SeverityWarning,
			ReadImpact:  ImpactSafe,
			WriteImpact: ImpactRisk,
			Message:     "generated-column status changed",
			Evidence:    Evidence{Expected: generatedEvidence(expected.AutoGenerated), Actual: generatedEvidence(actual.AutoGenerated)},
		}
		switch {
		case expected.AutoGenerated && !actual.AutoGenerated && !actual.Nullable && actual.Default == "":
			// Generated inserts exclude this column; now it is required.
			diff.Severity = SeverityError
			diff.WriteImpact = ImpactBreaks
			diff.Message = "column is no longer generated and has no default; generated inserts omit it and will be rejected"
		case !expected.AutoGenerated && actual.AutoGenerated:
			// Generated inserts/updates still name this column; live databases
			// reject writes to generated columns.
			diff.Severity = SeverityError
			diff.WriteImpact = ImpactBreaks
			diff.Message = "column became generated; generated inserts and updates naming it will be rejected"
		}
		differences = append(differences, diff)
	}

	if expected.Unique != actual.Unique {
		differences = append(differences, Difference{
			Path:        path,
			Kind:        KindUniqueChanged,
			Severity:    SeverityWarning,
			ReadImpact:  ImpactSafe,
			WriteImpact: ImpactRisk,
			Message:     "uniqueness constraint changed; duplicate-value assumptions in generated code may no longer hold",
			Evidence:    Evidence{Expected: uniqueEvidence(expected.Unique), Actual: uniqueEvidence(actual.Unique)},
		})
	}

	return differences
}

func (e *checkEngine) diffColumnType(path string, expected ColumnContract, actual drivers.Column) []Difference {
	verdict := VerdictUnknown
	detail := "no type rules registered for driver " + e.manifest.DriverName
	if e.rules != nil {
		verdict = e.rules.CheckColumnType(
			drivers.Column{DBType: expected.DBType, FullDBType: expected.FullDBType, UDTName: expected.UDTName, ArrType: expected.ArrType, DomainName: expected.DomainName, Nullable: expected.Nullable},
			drivers.Column{DBType: actual.DBType, FullDBType: actual.FullDBType, UDTName: actual.UDTName, ArrType: actual.ArrType, DomainName: actual.DomainName, Nullable: actual.Nullable},
		)
		detail = "rules=" + e.rules.DriverName()
	}

	evidence := Evidence{Expected: expected.DBType, Actual: actual.DBType, Detail: detail}
	switch verdict {
	case VerdictCompatible:
		return nil
	case VerdictWidened:
		return []Difference{{
			Path: path, Kind: KindColumnTypeWidened, Severity: SeverityWarning,
			ReadImpact: ImpactRisk, WriteImpact: ImpactSafe,
			Message:  "live type is wider than the generated type; reads may overflow the generated Go field while writes stay valid",
			Evidence: evidence,
		}}
	case VerdictNarrowed:
		return []Difference{{
			Path: path, Kind: KindColumnTypeNarrowed, Severity: SeverityWarning,
			ReadImpact: ImpactSafe, WriteImpact: ImpactRisk,
			Message:  "live type is narrower than the generated type; writes may be rejected or truncated while reads stay valid",
			Evidence: evidence,
		}}
	case VerdictIncompatible:
		return []Difference{{
			Path: path, Kind: KindColumnTypeIncompatible, Severity: SeverityError,
			ReadImpact: ImpactBreaks, WriteImpact: ImpactBreaks,
			Message:  "live type family does not match the generated type",
			Evidence: evidence,
		}}
	default:
		return []Difference{{
			Path: path, Kind: KindColumnTypeUnknown, Severity: SeverityUnknown,
			ReadImpact: ImpactUnknown, WriteImpact: ImpactUnknown,
			Message:  "no explicit driver rule covers this type (unknown domain, custom type or unregistered driver); manual adaptation is required",
			Evidence: evidence,
		}}
	}
}

func (e *checkEngine) typeReplaceStillApplies(expected ColumnContract, actual drivers.Column) bool {
	var match *TypeReplaceContract
	for i := range e.manifest.TypeReplaces {
		if e.manifest.TypeReplaces[i].Digest == expected.TypeReplaceDigest {
			match = &e.manifest.TypeReplaces[i]
			break
		}
	}
	if match == nil {
		return false
	}
	m := match.Match
	if m.DBType != "" && m.DBType != actual.DBType {
		return false
	}
	if m.FullDBType != "" && m.FullDBType != actual.FullDBType {
		return false
	}
	if m.UDTName != "" && m.UDTName != actual.UDTName {
		return false
	}
	if m.ArrType != nil && (actual.ArrType == nil || *m.ArrType != *actual.ArrType) {
		return false
	}
	if m.DomainName != nil && (actual.DomainName == nil || *m.DomainName != *actual.DomainName) {
		return false
	}
	return true
}

func (e *checkEngine) diffPrimaryKey(path string, expected TableContract, actual drivers.Table) []Difference {
	pkPath := path + ".primary_key"

	switch {
	case expected.PrimaryKey == nil && actual.PKey == nil:
		return nil
	case expected.PrimaryKey != nil && actual.PKey == nil:
		return []Difference{{
			Path: pkPath, Kind: KindPrimaryKeyMissing, Severity: SeverityError,
			ReadImpact: ImpactBreaks, WriteImpact: ImpactBreaks,
			Message:  "primary key used by Find/Update/Delete was dropped",
			Evidence: Evidence{Expected: strings.Join(expected.PrimaryKey.Columns, ", "), Actual: "<absent>"},
		}}
	case expected.PrimaryKey == nil && actual.PKey != nil:
		return []Difference{{
			Path: pkPath, Kind: KindPrimaryKeyChanged, Severity: SeverityWarning,
			ReadImpact: ImpactRisk, WriteImpact: ImpactRisk,
			Message:  "a primary key appeared on a relation that was generated without one",
			Evidence: Evidence{Expected: "<absent>", Actual: strings.Join(actual.PKey.Columns, ", ")},
		}}
	}

	expCols := expected.PrimaryKey.Columns
	actCols := actual.PKey.Columns
	if sameStringSlice(expCols, actCols) {
		return nil
	}
	if sameStringSet(expCols, actCols) {
		return []Difference{{
			Path: pkPath, Kind: KindPrimaryKeyOrderChanged, Severity: SeverityWarning,
			ReadImpact: ImpactRisk, WriteImpact: ImpactRisk,
			Message:  "composite primary key column order changed; key order is meaningful metadata and must not be normalized away",
			Evidence: Evidence{Expected: strings.Join(expCols, ", "), Actual: strings.Join(actCols, ", ")},
		}}
	}
	return []Difference{{
		Path: pkPath, Kind: KindPrimaryKeyChanged, Severity: SeverityError,
		ReadImpact: ImpactBreaks, WriteImpact: ImpactBreaks,
		Message:  "primary key column set changed; generated Find/Update/Delete statements rely on the original key",
		Evidence: Evidence{Expected: strings.Join(expCols, ", "), Actual: strings.Join(actCols, ", ")},
	}}
}

func (e *checkEngine) diffForeignKeys(path string, expected TableContract, actual drivers.Table) []Difference {
	var differences []Difference

	actualFKs := make(map[string]drivers.ForeignKey, len(actual.FKeys))
	for _, fk := range actual.FKeys {
		actualFKs[fk.Name] = fk
	}

	for _, expFK := range expected.ForeignKeys {
		fkPath := path + ".foreign_keys." + expFK.Name
		actFK, ok := actualFKs[expFK.Name]
		if !ok {
			differences = append(differences, Difference{
				Path: fkPath, Kind: KindForeignKeyMissing, Severity: SeverityWarning,
				ReadImpact: ImpactRisk, WriteImpact: ImpactRisk,
				Message:  "foreign key backing a generated relationship was dropped",
				Evidence: Evidence{Expected: fkSummary(expFK.Column, expFK.ForeignTable, expFK.ForeignColumn), Actual: "<absent>"},
			})
			continue
		}
		if actFK.ForeignTable != expFK.ForeignTable || actFK.ForeignColumn != expFK.ForeignColumn || actFK.Column != expFK.Column {
			differences = append(differences, Difference{
				Path: fkPath, Kind: KindForeignKeyRetargeted, Severity: SeverityError,
				ReadImpact: ImpactBreaks, WriteImpact: ImpactRisk,
				Message: "foreign key was retargeted; generated relationship joins no longer resolve the same relation",
				Evidence: Evidence{
					Expected: fkSummary(expFK.Column, expFK.ForeignTable, expFK.ForeignColumn),
					Actual:   fkSummary(actFK.Column, actFK.ForeignTable, actFK.ForeignColumn),
				},
			})
			continue
		}
		if actFK.Nullable != expFK.Nullable || actFK.Unique != expFK.Unique {
			differences = append(differences, Difference{
				Path: fkPath, Kind: KindForeignKeyChanged, Severity: SeverityWarning,
				ReadImpact: ImpactSafe, WriteImpact: ImpactRisk,
				Message: "foreign key nullability or uniqueness changed; relationship cardinality assumptions may shift",
				Evidence: Evidence{
					Expected: fmt.Sprintf("nullable=%t unique=%t", expFK.Nullable, expFK.Unique),
					Actual:   fmt.Sprintf("nullable=%t unique=%t", actFK.Nullable, actFK.Unique),
				},
			})
		}
	}

	for _, actFK := range actual.FKeys {
		if _, ok := fkContractByName(expected.ForeignKeys, actFK.Name); ok {
			continue
		}
		differences = append(differences, Difference{
			Path: path + ".foreign_keys." + actFK.Name, Kind: KindExtraForeignKey, Severity: SeverityInfo,
			ReadImpact: ImpactSafe, WriteImpact: ImpactSafe,
			Message:  "additional live foreign key is not used by the generated models",
			Evidence: Evidence{Expected: "<absent>", Actual: fkSummary(actFK.Column, actFK.ForeignTable, actFK.ForeignColumn)},
		})
	}

	return differences
}

func (e *checkEngine) diffRelationships(path string, expected TableContract, actual drivers.Table) []Difference {
	var differences []Difference

	actualToOne := make(map[string]drivers.ToOneRelationship, len(actual.ToOneRelationships))
	for _, r := range actual.ToOneRelationships {
		actualToOne[r.Name] = r
	}
	actualToMany := make(map[string]drivers.ToManyRelationship, len(actual.ToManyRelationships))
	for _, r := range actual.ToManyRelationships {
		actualToMany[r.Name] = r
	}

	for _, expRel := range expected.ToOneRelationships {
		relPath := path + ".to_one_relationships." + expRel.Name
		actRel, ok := actualToOne[expRel.Name]
		if !ok {
			differences = append(differences, relMissing(relPath, expRel.Name))
			continue
		}
		if actRel.ForeignTable != expRel.ForeignTable || actRel.ForeignColumn != expRel.ForeignColumn {
			differences = append(differences, Difference{
				Path: relPath, Kind: KindRelationshipRetargeted, Severity: SeverityError,
				ReadImpact: ImpactBreaks, WriteImpact: ImpactSafe,
				Message: "to-one relationship target changed; generated eager loading joins are stale",
				Evidence: Evidence{
					Expected: relSummary(expRel.ForeignTable, expRel.ForeignColumn),
					Actual:   relSummary(actRel.ForeignTable, actRel.ForeignColumn),
				},
			})
		}
	}

	for _, expRel := range expected.ToManyRelationships {
		relPath := path + ".to_many_relationships." + expRel.Name
		actRel, ok := actualToMany[expRel.Name]
		if !ok {
			differences = append(differences, relMissing(relPath, expRel.Name))
			continue
		}
		if actRel.ForeignTable != expRel.ForeignTable || actRel.ForeignColumn != expRel.ForeignColumn ||
			actRel.JoinTable != expRel.JoinTable {
			differences = append(differences, Difference{
				Path: relPath, Kind: KindRelationshipRetargeted, Severity: SeverityError,
				ReadImpact: ImpactBreaks, WriteImpact: ImpactSafe,
				Message: "to-many relationship target changed; generated eager loading joins are stale",
				Evidence: Evidence{
					Expected: relSummary(expRel.ForeignTable, expRel.ForeignColumn),
					Actual:   relSummary(actRel.ForeignTable, actRel.ForeignColumn),
				},
			})
		}
	}

	return differences
}

func (e *checkEngine) diffViewCapabilities(path string, expected TableContract, actual drivers.Table) []Difference {
	if !expected.IsView || !actual.IsView {
		return nil
	}
	var differences []Difference
	if expected.ViewCapabilities.CanInsert != actual.ViewCapabilities.CanInsert {
		diff := Difference{
			Path: path + ".view_capabilities.can_insert", Kind: KindViewInsertCapability,
			ReadImpact: ImpactSafe,
			Message:    "view insert capability changed",
			Evidence: Evidence{
				Expected: fmt.Sprintf("can_insert=%t", expected.ViewCapabilities.CanInsert),
				Actual:   fmt.Sprintf("can_insert=%t", actual.ViewCapabilities.CanInsert),
			},
		}
		if expected.ViewCapabilities.CanInsert && !actual.ViewCapabilities.CanInsert {
			// An Insert method was generated; the live view now rejects it.
			diff.Severity = SeverityError
			diff.WriteImpact = ImpactBreaks
			diff.Message = "the view no longer accepts inserts but the generated models contain Insert"
		} else {
			diff.Severity = SeverityInfo
			diff.WriteImpact = ImpactSafe
		}
		differences = append(differences, diff)
	}
	if expected.ViewCapabilities.CanUpsert != actual.ViewCapabilities.CanUpsert {
		diff := Difference{
			Path: path + ".view_capabilities.can_upsert", Kind: KindViewUpsertCapability,
			ReadImpact: ImpactSafe,
			Message:    "view upsert capability changed",
			Evidence: Evidence{
				Expected: fmt.Sprintf("can_upsert=%t", expected.ViewCapabilities.CanUpsert),
				Actual:   fmt.Sprintf("can_upsert=%t", actual.ViewCapabilities.CanUpsert),
			},
		}
		if expected.ViewCapabilities.CanUpsert && !actual.ViewCapabilities.CanUpsert {
			diff.Severity = SeverityError
			diff.WriteImpact = ImpactBreaks
			diff.Message = "the view no longer accepts upserts but the generated models contain Upsert"
		} else {
			diff.Severity = SeverityInfo
			diff.WriteImpact = ImpactSafe
		}
		differences = append(differences, diff)
	}
	return differences
}

func summarize(differences []Difference) (readCompatible, writeCompatible, hasUnknowns bool) {
	readCompatible, writeCompatible = true, true
	for _, d := range differences {
		if d.Severity == SeverityUnknown {
			hasUnknowns = true
		}
		if d.ReadImpact != ImpactSafe {
			readCompatible = false
		}
		if d.WriteImpact != ImpactSafe {
			writeCompatible = false
		}
	}
	return readCompatible, writeCompatible, hasUnknowns
}

func tablePath(schema, name string) string {
	if schema == "" {
		return "tables[" + name + "]"
	}
	return "tables[" + schema + "." + name + "]"
}

func relMissing(path, name string) Difference {
	return Difference{
		Path: path, Kind: KindRelationshipMissing, Severity: SeverityWarning,
		ReadImpact: ImpactRisk, WriteImpact: ImpactSafe,
		Message:  fmt.Sprintf("generated relationship %s no longer exists in the live schema", name),
		Evidence: Evidence{Actual: "<absent>"},
	}
}

func columnContractByName(cols []ColumnContract, name string) (ColumnContract, bool) {
	for _, c := range cols {
		if c.Name == name {
			return c, true
		}
	}
	return ColumnContract{}, false
}

func fkContractByName(fks []ForeignKeyContract, name string) (ForeignKeyContract, bool) {
	for _, f := range fks {
		if f.Name == name {
			return f, true
		}
	}
	return ForeignKeyContract{}, false
}

func sameStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]struct{}, len(a))
	for _, s := range a {
		seen[s] = struct{}{}
	}
	for _, s := range b {
		if _, ok := seen[s]; ok == false {
			return false
		}
	}
	return true
}

func tableEvidence(t TableContract) string {
	kind := "table"
	if t.IsView {
		kind = "view"
	}
	return fmt.Sprintf("%s %s with %d column(s)", kind, tableKey(t.SchemaName, t.Name), len(t.Columns))
}

func liveTableEvidence(t drivers.Table) string {
	kind := "table"
	if t.IsView {
		kind = "view"
	}
	return fmt.Sprintf("%s %s with %d column(s)", kind, tableKey(t.SchemaName, t.Name), len(t.Columns))
}

func kindEvidence(isView bool) string {
	if isView {
		return "view"
	}
	return "table"
}

func columnSummary(c ColumnContract) string {
	return fmt.Sprintf("%s %s nullable=%t default=%q generated=%t", c.Name, c.DBType, c.Nullable, c.Default, c.AutoGenerated)
}

func liveColumnSummary(c drivers.Column) string {
	return fmt.Sprintf("%s %s nullable=%t default=%q generated=%t", c.Name, c.DBType, c.Nullable, c.Default, c.AutoGenerated)
}

func defaultEvidence(def string) string {
	if def == "" {
		return "<none>"
	}
	return def
}

func generatedEvidence(v bool) string {
	if v {
		return "generated"
	}
	return "stored"
}

func uniqueEvidence(v bool) string {
	if v {
		return "unique"
	}
	return "not unique"
}

func fkSummary(column, foreignTable, foreignColumn string) string {
	return fmt.Sprintf("%s -> %s.%s", column, foreignTable, foreignColumn)
}

func relSummary(foreignTable, foreignColumn string) string {
	return fmt.Sprintf("%s.%s", foreignTable, foreignColumn)
}
