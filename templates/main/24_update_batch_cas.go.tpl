{{- $vcolName := .UpdateBatchCAS.VersionColumnName .Table.Name -}}
{{- if and $vcolName (not .Table.IsView) (not .Table.IsJoinTable) -}}
{{- $alias := .Aliases.Table .Table.Name -}}
{{- $schemaTable := .Table.Name | .SchemaTable -}}
{{- $vcol := .Table.GetColumn $vcolName -}}
{{- $vcolAlias := $alias.Column $vcolName -}}
{{- $supported := or (eq .DriverName "psql") (eq .DriverName "sqlite3") -}}

// {{$alias.DownSingular}}UpdateBatchCASMaxRows bounds the number of rows
// accepted by one {{$alias.UpSingular}} UpdateBatchCAS call.
const {{$alias.DownSingular}}UpdateBatchCASMaxRows = {{.UpdateBatchCAS.MaxBatchSize}}

// {{$alias.DownSingular}}UpdateBatchCASMaxSQLArgs bounds the total number of
// bound SQL parameters produced by one {{$alias.UpSingular}} UpdateBatchCAS call.
const {{$alias.DownSingular}}UpdateBatchCASMaxSQLArgs = {{.UpdateBatchCAS.MaxSQLArgs}}

{{if not $supported -}}
{{if .NoContext -}}
// UpdateBatchCAS is not implemented for this adapter. It never issues SQL
// and always reports boil.UpdateBatchCASUnsupported because the adapter
// cannot reliably verify row identity without driver-specific
// RowsAffected semantics.
func UpdateBatchCAS{{$alias.UpSingular}}(exec boil.Executor, objs []*{{$alias.UpSingular}}, columns boil.Columns) (boil.UpdateBatchCASResult, error) {
	_ = exec
	_ = objs
	_ = columns
	return boil.UpdateBatchCASResult{Status: boil.UpdateBatchCASUnsupported}, boil.ErrUpdateBatchCASUnsupported
}
{{- else -}}
// UpdateBatchCAS is not implemented for this adapter. It never issues SQL
// and always reports boil.UpdateBatchCASUnsupported because the adapter
// cannot reliably verify row identity without driver-specific
// RowsAffected semantics.
func UpdateBatchCAS{{$alias.UpSingular}}(ctx context.Context, exec boil.ContextExecutor, objs []*{{$alias.UpSingular}}, columns boil.Columns) (boil.UpdateBatchCASResult, error) {
	_ = ctx
	_ = exec
	_ = objs
	_ = columns
	return boil.UpdateBatchCASResult{Status: boil.UpdateBatchCASUnsupported}, boil.ErrUpdateBatchCASUnsupported
}
{{- end}}
{{- else -}}
{{if .NoContext -}}
// UpdateBatchCAS performs an all-or-nothing compare-and-swap batch update.
//
// Every input object carries its full primary key, the expected version and
// the new values for the fixed writable column set. Each matched row has its
// version incremented by exactly one. Real conditional UPDATE ... RETURNING
// statements verify every input identity: if any row is missing or carries a
// stale version the whole batch rolls back and the in-memory objects are
// restored. There is no SELECT-then-update window and RowsAffected is never
// used to decide a conflict.
//
// A passed *sql.Tx is never committed: a named savepoint is established and
// either released or rolled back. Otherwise the call owns its transaction.
func UpdateBatchCAS{{$alias.UpSingular}}(exec boil.Executor, objs []*{{$alias.UpSingular}}, columns boil.Columns) (boil.UpdateBatchCASResult, error) {
	result := boil.UpdateBatchCASResult{}

	reject := func(format string, args ...any) (boil.UpdateBatchCASResult, error) {
		result.Status = boil.UpdateBatchCASRejected
		return result, fmt.Errorf("%w: %s", boil.ErrUpdateBatchCASInvalid, fmt.Sprintf(format, args...))
	}

	if len(objs) == 0 {
		return reject("{{.PkgName}}: empty {{.Table.Name}} update batch cas input")
	}
	if len(objs) > {{$alias.DownSingular}}UpdateBatchCASMaxRows {
		return reject("{{.PkgName}}: {{.Table.Name}} update batch cas size %d exceeds limit %d", len(objs), {{$alias.DownSingular}}UpdateBatchCASMaxRows)
	}

	// Resolve the fixed writable column set once for the whole batch.
	var wl []string
	switch {
	case columns.IsInfer():
		wl = strmangle.SetComplement({{$alias.DownSingular}}AllColumns, {{$alias.DownSingular}}PrimaryKeyColumns)
		wl = strmangle.SetComplement(wl, {{$alias.DownSingular}}GeneratedColumns)
		wl = strmangle.SetComplement(wl, []string{ {{quoteWrap $vcolName}} })
	case columns.IsWhitelist():
		wl = append(wl, columns.Cols...)
	default:
		return reject("{{.PkgName}}: {{.Table.Name}} update batch cas only supports boil.Infer() or boil.Whitelist(...) column sets")
	}
	for _, col := range wl {
		if !strmangle.SetInclude(col, {{$alias.DownSingular}}AllColumns) {
			return reject("{{.PkgName}}: {{.Table.Name}} update batch cas references unknown column %q", col)
		}
		if strmangle.SetInclude(col, {{$alias.DownSingular}}PrimaryKeyColumns) {
			return reject("{{.PkgName}}: {{.Table.Name}} update batch cas cannot write primary key column %q", col)
		}
		if col == {{quoteWrap $vcolName}} {
			return reject("{{.PkgName}}: {{.Table.Name}} update batch cas manages version column %q itself", col)
		}
		if strmangle.SetInclude(col, {{$alias.DownSingular}}GeneratedColumns) {
			return reject("{{.PkgName}}: {{.Table.Name}} update batch cas cannot write auto-generated column %q", col)
		}
	}
	wl = strmangle.SortByKeys({{$alias.DownSingular}}AllColumns, strmangle.RemoveDuplicates(wl))

	argsPerRow := len(wl) + len({{$alias.DownSingular}}PrimaryKeyColumns) + 1
	if len(objs)*argsPerRow > {{$alias.DownSingular}}UpdateBatchCASMaxSQLArgs {
		return reject("{{.PkgName}}: {{.Table.Name}} update batch cas would bind %d args, exceeding limit %d", len(objs)*argsPerRow, {{$alias.DownSingular}}UpdateBatchCASMaxSQLArgs)
	}

	// Per-row, pre-SQL validation: nil elements, duplicate full primary
	// keys (compared with type fidelity, never via fmt) and version overflow.
	pkValues := make([][]any, len(objs))
	expectedVersions := make([]{{$vcol.Type}}, len(objs))
	for i, o := range objs {
		if o == nil {
			return reject("{{.PkgName}}: {{.Table.Name}} update batch cas element at index %d is nil", i)
		}
		pkValues[i] = queries.ValuesFromMapping(reflect.Indirect(reflect.ValueOf(o)), {{$alias.DownSingular}}PrimaryKeyMapping)
		if err := boil.CheckVersionIncrement(o.{{$vcolAlias}}); err != nil {
			result.Status = boil.UpdateBatchCASRejected
			return result, err
		}
		expectedVersions[i] = o.{{$vcolAlias}}
		for j := 0; j < i; j++ {
			duplicate := true
			for k, pv := range pkValues[i] {
				if !queries.Equal(pv, pkValues[j][k]) {
					duplicate = false
					break
				}
			}
			if duplicate {
				return reject("{{.PkgName}}: {{.Table.Name}} update batch cas duplicate full primary key at index %d", i)
			}
		}
	}

	// Snapshot every object so non-commit paths can restore the originals.
	originals := make([]{{$alias.UpSingular}}, len(objs))
	for i, o := range objs {
		originals[i] = *o
	}
	restore := func() {
		for i, o := range objs {
			*o = originals[i]
		}
	}

	{{if not .NoHooks -}}
	for _, o := range objs {
		if err := o.doBeforeUpdateHooks(exec); err != nil {
			restore()
			return result, errors.Wrap(err, "{{.PkgName}}: {{.Table.Name}} update batch cas before update hook failed")
		}
	}
	{{end -}}

	batchTx, err := boil.BeginBatchCAS(exec)
	if err != nil {
		restore()
		if errors.Is(err, boil.ErrUpdateBatchCASUnsupported) {
			result.Status = boil.UpdateBatchCASUnsupported
		} else {
			result.Status = boil.UpdateBatchCASRejected
		}
		return result, err
	}
	fail := func(cause error, format string, args ...any) (boil.UpdateBatchCASResult, error) {
		if rbErr := batchTx.Rollback(); rbErr != nil {
			cause = fmt.Errorf("%w: rollback failed: %w", cause, rbErr)
		}
		restore()
		return result, errors.Wrapf(cause, format, args...)
	}

	wlMapping, err := queries.BindMapping({{$alias.DownSingular}}Type, {{$alias.DownSingular}}Mapping, wl)
	if err != nil {
		return fail(err, "{{.PkgName}}: {{.Table.Name}} update batch cas whitelist mapping")
	}

	whereStart := {{if .Dialect.UseIndexPlaceholders}}1{{else}}0{{end}}
	versionSet := "{{.LQ}}{{$vcolName}}{{.RQ}}={{.LQ}}{{$vcolName}}{{.RQ}}+1"
	setList := versionSet
	if len(wl) > 0 {
		setList = strmangle.SetParamNames("{{.LQ}}", "{{.RQ}}", whereStart, wl) + ", " + versionSet
	}
	whereParts := strmangle.WhereClause("{{.LQ}}", "{{.RQ}}", {{if .Dialect.UseIndexPlaceholders}}whereStart+len(wl){{else}}whereStart{{end}}, {{$alias.DownSingular}}PrimaryKeyColumns)
	versionPlaceholder := strmangle.Placeholders({{.Dialect.UseIndexPlaceholders}}, 1, {{if .Dialect.UseIndexPlaceholders}}whereStart+len(wl)+len({{$alias.DownSingular}}PrimaryKeyColumns){{else}}1{{end}}, 1)
	whereParts += " AND {{.LQ}}{{$vcolName}}{{.RQ}}=" + versionPlaceholder
	returningClause := strings.Join(strmangle.StringMap(func(c string) string {
		return "{{.LQ}}" + c + "{{.RQ}}"
	}, {{$alias.DownSingular}}PrimaryKeyColumns), ",")
	query := fmt.Sprintf("UPDATE {{$schemaTable}} SET %s WHERE %s RETURNING %s", setList, whereParts, returningClause)

	matched := make([]bool, len(objs))
	batchExec := batchTx.Executor()
	for i, o := range objs {
		args := queries.ValuesFromMapping(reflect.Indirect(reflect.ValueOf(o)), wlMapping)
		args = append(args, pkValues[i]...)
		args = append(args, expectedVersions[i])

		if boil.DebugMode {
			fmt.Fprintln(boil.DebugWriter, query)
			fmt.Fprintln(boil.DebugWriter, args)
		}

		var (
		{{range $pkName := .Table.PKey.Columns -}}
		{{- $pkCol := $.Table.GetColumn $pkName -}}
		{{- $pkAlias := $alias.Column $pkName -}}
			ret{{$pkAlias}} {{$pkCol.Type}}
		{{end -}}
		)
		err = batchExec.QueryRow(query, args...).Scan(
		{{range $pkName := .Table.PKey.Columns -}}
		{{- $pkAlias := $alias.Column $pkName -}}
			&ret{{$pkAlias}},
		{{end -}}
		)
		if errors.Is(err, sql.ErrNoRows) {
			result.Status = boil.UpdateBatchCASConflict
			if rbErr := batchTx.Rollback(); rbErr != nil {
				restore()
				return result, errors.Wrapf(boil.ErrUpdateBatchCASConflict, "{{.PkgName}}: {{.Table.Name}} update batch cas conflict at index %d and rollback failed: %v", i, rbErr)
			}
			restore()
			return result, errors.Wrapf(boil.ErrUpdateBatchCASConflict, "{{.PkgName}}: {{.Table.Name}} update batch cas conflict at index %d: row missing or version does not match", i)
		}
		if err != nil {
			return fail(err, "{{.PkgName}}: {{.Table.Name}} update batch cas update failed at index %d", i)
		}

		gotValues := []any{
		{{range $pkName := .Table.PKey.Columns -}}
		{{- $pkAlias := $alias.Column $pkName -}}
			ret{{$pkAlias}},
		{{end -}}
		}
		for k, want := range pkValues[i] {
			if !queries.Equal(want, gotValues[k]) {
				return fail(boil.ErrUpdateBatchCASInvalid, "{{.PkgName}}: {{.Table.Name}} update batch cas returned identity mismatch at index %d", i)
			}
		}
		matched[i] = true
	}
	for i, ok := range matched {
		if !ok {
			return fail(boil.ErrUpdateBatchCASConflict, "{{.PkgName}}: {{.Table.Name}} update batch cas unverified row at index %d", i)
		}
	}

	if err := batchTx.Commit(); err != nil {
		restore()
		result.Status = boil.UpdateBatchCASCommitUnknown
		return result, fmt.Errorf("%w: {{.PkgName}}: {{.Table.Name}} update batch cas commit failed: %w", boil.ErrUpdateBatchCASCommitUnknown, err)
	}

	// Commit is confirmed: only now publish the new in-memory state.
	for i, o := range objs {
		o.{{$vcolAlias}} = expectedVersions[i] + 1
	}
	{{if not .NoHooks -}}
	for _, o := range objs {
		if err := o.doAfterUpdateHooks(exec); err != nil {
			result.Status = boil.UpdateBatchCASCommitted
			result.Applied = len(objs)
			return result, errors.Wrap(err, "{{.PkgName}}: {{.Table.Name}} update batch cas after update hook failed; database commit is already durable and external hook side effects cannot be rolled back")
		}
	}
	{{end -}}

	result.Status = boil.UpdateBatchCASCommitted
	result.Applied = len(objs)
	return result, nil
}
{{- else -}}
// UpdateBatchCAS performs an all-or-nothing compare-and-swap batch update.
//
// Every input object carries its full primary key, the expected version and
// the new values for the fixed writable column set. Each matched row has its
// version incremented by exactly one. Real conditional UPDATE ... RETURNING
// statements verify every input identity: if any row is missing or carries a
// stale version the whole batch rolls back and the in-memory objects are
// restored. There is no SELECT-then-update window and RowsAffected is never
// used to decide a conflict.
//
// A passed *sql.Tx is never committed: a named savepoint is established and
// either released or rolled back. Otherwise the call owns its transaction.
func UpdateBatchCAS{{$alias.UpSingular}}(ctx context.Context, exec boil.ContextExecutor, objs []*{{$alias.UpSingular}}, columns boil.Columns) (boil.UpdateBatchCASResult, error) {
	result := boil.UpdateBatchCASResult{}

	reject := func(format string, args ...any) (boil.UpdateBatchCASResult, error) {
		result.Status = boil.UpdateBatchCASRejected
		return result, fmt.Errorf("%w: %s", boil.ErrUpdateBatchCASInvalid, fmt.Sprintf(format, args...))
	}

	if len(objs) == 0 {
		return reject("{{.PkgName}}: empty {{.Table.Name}} update batch cas input")
	}
	if len(objs) > {{$alias.DownSingular}}UpdateBatchCASMaxRows {
		return reject("{{.PkgName}}: {{.Table.Name}} update batch cas size %d exceeds limit %d", len(objs), {{$alias.DownSingular}}UpdateBatchCASMaxRows)
	}

	// Resolve the fixed writable column set once for the whole batch.
	var wl []string
	switch {
	case columns.IsInfer():
		wl = strmangle.SetComplement({{$alias.DownSingular}}AllColumns, {{$alias.DownSingular}}PrimaryKeyColumns)
		wl = strmangle.SetComplement(wl, {{$alias.DownSingular}}GeneratedColumns)
		wl = strmangle.SetComplement(wl, []string{ {{quoteWrap $vcolName}} })
	case columns.IsWhitelist():
		wl = append(wl, columns.Cols...)
	default:
		return reject("{{.PkgName}}: {{.Table.Name}} update batch cas only supports boil.Infer() or boil.Whitelist(...) column sets")
	}
	for _, col := range wl {
		if !strmangle.SetInclude(col, {{$alias.DownSingular}}AllColumns) {
			return reject("{{.PkgName}}: {{.Table.Name}} update batch cas references unknown column %q", col)
		}
		if strmangle.SetInclude(col, {{$alias.DownSingular}}PrimaryKeyColumns) {
			return reject("{{.PkgName}}: {{.Table.Name}} update batch cas cannot write primary key column %q", col)
		}
		if col == {{quoteWrap $vcolName}} {
			return reject("{{.PkgName}}: {{.Table.Name}} update batch cas manages version column %q itself", col)
		}
		if strmangle.SetInclude(col, {{$alias.DownSingular}}GeneratedColumns) {
			return reject("{{.PkgName}}: {{.Table.Name}} update batch cas cannot write auto-generated column %q", col)
		}
	}
	wl = strmangle.SortByKeys({{$alias.DownSingular}}AllColumns, strmangle.RemoveDuplicates(wl))

	argsPerRow := len(wl) + len({{$alias.DownSingular}}PrimaryKeyColumns) + 1
	if len(objs)*argsPerRow > {{$alias.DownSingular}}UpdateBatchCASMaxSQLArgs {
		return reject("{{.PkgName}}: {{.Table.Name}} update batch cas would bind %d args, exceeding limit %d", len(objs)*argsPerRow, {{$alias.DownSingular}}UpdateBatchCASMaxSQLArgs)
	}

	// Per-row, pre-SQL validation: nil elements, duplicate full primary
	// keys (compared with type fidelity, never via fmt) and version overflow.
	pkValues := make([][]any, len(objs))
	expectedVersions := make([]{{$vcol.Type}}, len(objs))
	for i, o := range objs {
		if o == nil {
			return reject("{{.PkgName}}: {{.Table.Name}} update batch cas element at index %d is nil", i)
		}
		pkValues[i] = queries.ValuesFromMapping(reflect.Indirect(reflect.ValueOf(o)), {{$alias.DownSingular}}PrimaryKeyMapping)
		if err := boil.CheckVersionIncrement(o.{{$vcolAlias}}); err != nil {
			result.Status = boil.UpdateBatchCASRejected
			return result, err
		}
		expectedVersions[i] = o.{{$vcolAlias}}
		for j := 0; j < i; j++ {
			duplicate := true
			for k, pv := range pkValues[i] {
				if !queries.Equal(pv, pkValues[j][k]) {
					duplicate = false
					break
				}
			}
			if duplicate {
				return reject("{{.PkgName}}: {{.Table.Name}} update batch cas duplicate full primary key at index %d", i)
			}
		}
	}

	// Snapshot every object so non-commit paths can restore the originals.
	originals := make([]{{$alias.UpSingular}}, len(objs))
	for i, o := range objs {
		originals[i] = *o
	}
	restore := func() {
		for i, o := range objs {
			*o = originals[i]
		}
	}

	{{if not .NoHooks -}}
	for _, o := range objs {
		if err := o.doBeforeUpdateHooks(ctx, exec); err != nil {
			restore()
			return result, errors.Wrap(err, "{{.PkgName}}: {{.Table.Name}} update batch cas before update hook failed")
		}
	}
	{{end -}}

	batchTx, err := boil.BeginContextBatchCAS(ctx, exec)
	if err != nil {
		restore()
		if errors.Is(err, boil.ErrUpdateBatchCASUnsupported) {
			result.Status = boil.UpdateBatchCASUnsupported
		} else {
			result.Status = boil.UpdateBatchCASRejected
		}
		return result, err
	}
	fail := func(cause error, format string, args ...any) (boil.UpdateBatchCASResult, error) {
		if rbErr := batchTx.Rollback(ctx); rbErr != nil {
			cause = fmt.Errorf("%v: rollback failed: %w", cause, rbErr)
		}
		restore()
		return result, errors.Wrapf(cause, format, args...)
	}

	wlMapping, err := queries.BindMapping({{$alias.DownSingular}}Type, {{$alias.DownSingular}}Mapping, wl)
	if err != nil {
		return fail(err, "{{.PkgName}}: {{.Table.Name}} update batch cas whitelist mapping")
	}

	whereStart := {{if .Dialect.UseIndexPlaceholders}}1{{else}}0{{end}}
	versionSet := "{{.LQ}}{{$vcolName}}{{.RQ}}={{.LQ}}{{$vcolName}}{{.RQ}}+1"
	setList := versionSet
	if len(wl) > 0 {
		setList = strmangle.SetParamNames("{{.LQ}}", "{{.RQ}}", whereStart, wl) + ", " + versionSet
	}
	whereParts := strmangle.WhereClause("{{.LQ}}", "{{.RQ}}", {{if .Dialect.UseIndexPlaceholders}}whereStart+len(wl){{else}}whereStart{{end}}, {{$alias.DownSingular}}PrimaryKeyColumns)
	versionPlaceholder := strmangle.Placeholders({{.Dialect.UseIndexPlaceholders}}, 1, {{if .Dialect.UseIndexPlaceholders}}whereStart+len(wl)+len({{$alias.DownSingular}}PrimaryKeyColumns){{else}}1{{end}}, 1)
	whereParts += " AND {{.LQ}}{{$vcolName}}{{.RQ}}=" + versionPlaceholder
	returningClause := strings.Join(strmangle.StringMap(func(c string) string {
		return "{{.LQ}}" + c + "{{.RQ}}"
	}, {{$alias.DownSingular}}PrimaryKeyColumns), ",")
	query := fmt.Sprintf("UPDATE {{$schemaTable}} SET %s WHERE %s RETURNING %s", setList, whereParts, returningClause)

	matched := make([]bool, len(objs))
	batchExec := batchTx.Executor()
	for i, o := range objs {
		args := queries.ValuesFromMapping(reflect.Indirect(reflect.ValueOf(o)), wlMapping)
		args = append(args, pkValues[i]...)
		args = append(args, expectedVersions[i])

		if boil.IsDebug(ctx) {
			writer := boil.DebugWriterFrom(ctx)
			fmt.Fprintln(writer, query)
			fmt.Fprintln(writer, args)
		}

		var (
		{{range $pkName := .Table.PKey.Columns -}}
		{{- $pkCol := $.Table.GetColumn $pkName -}}
		{{- $pkAlias := $alias.Column $pkName -}}
			ret{{$pkAlias}} {{$pkCol.Type}}
		{{end -}}
		)
		err = batchExec.QueryRowContext(ctx, query, args...).Scan(
		{{range $pkName := .Table.PKey.Columns -}}
		{{- $pkAlias := $alias.Column $pkName -}}
			&ret{{$pkAlias}},
		{{end -}}
		)
		if errors.Is(err, sql.ErrNoRows) {
			result.Status = boil.UpdateBatchCASConflict
			if rbErr := batchTx.Rollback(ctx); rbErr != nil {
				restore()
				return result, errors.Wrapf(boil.ErrUpdateBatchCASConflict, "{{.PkgName}}: {{.Table.Name}} update batch cas conflict at index %d and rollback failed: %v", i, rbErr)
			}
			restore()
			return result, errors.Wrapf(boil.ErrUpdateBatchCASConflict, "{{.PkgName}}: {{.Table.Name}} update batch cas conflict at index %d: row missing or version does not match", i)
		}
		if err != nil {
			return fail(err, "{{.PkgName}}: {{.Table.Name}} update batch cas update failed at index %d", i)
		}

		gotValues := []any{
		{{range $pkName := .Table.PKey.Columns -}}
		{{- $pkAlias := $alias.Column $pkName -}}
			ret{{$pkAlias}},
		{{end -}}
		}
		for k, want := range pkValues[i] {
			if !queries.Equal(want, gotValues[k]) {
				return fail(boil.ErrUpdateBatchCASInvalid, "{{.PkgName}}: {{.Table.Name}} update batch cas returned identity mismatch at index %d", i)
			}
		}
		matched[i] = true
	}
	for i, ok := range matched {
		if !ok {
			return fail(boil.ErrUpdateBatchCASConflict, "{{.PkgName}}: {{.Table.Name}} update batch cas unverified row at index %d", i)
		}
	}

	if err := batchTx.Commit(ctx); err != nil {
		restore()
		result.Status = boil.UpdateBatchCASCommitUnknown
		return result, fmt.Errorf("%w: {{.PkgName}}: {{.Table.Name}} update batch cas commit failed: %w", boil.ErrUpdateBatchCASCommitUnknown, err)
	}

	// Commit is confirmed: only now publish the new in-memory state.
	for i, o := range objs {
		o.{{$vcolAlias}} = expectedVersions[i] + 1
	}
	{{if not .NoHooks -}}
	for _, o := range objs {
		if err := o.doAfterUpdateHooks(ctx, exec); err != nil {
			result.Status = boil.UpdateBatchCASCommitted
			result.Applied = len(objs)
			return result, errors.Wrap(err, "{{.PkgName}}: {{.Table.Name}} update batch cas after update hook failed; database commit is already durable and external hook side effects cannot be rolled back")
		}
	}
	{{end -}}

	result.Status = boil.UpdateBatchCASCommitted
	result.Applied = len(objs)
	return result, nil
}
{{- end}}
{{- end -}}
{{- end -}}
