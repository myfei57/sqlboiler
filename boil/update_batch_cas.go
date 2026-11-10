package boil

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sync/atomic"
)

// UpdateBatchCASStatus is the outcome of an UpdateBatchCAS entry point.
type UpdateBatchCASStatus int

const (
	// UpdateBatchCASNoOutcome is the zero value and is never returned on a
	// completed call. It exists so callers cannot accidentally treat an
	// uninitialized result as success.
	UpdateBatchCASNoOutcome UpdateBatchCASStatus = iota
	// UpdateBatchCASCommitted means every input row matched and the whole
	// batch was durably committed (or released from its savepoint).
	UpdateBatchCASCommitted
	// UpdateBatchCASConflict means at least one input row did not exist or
	// its expected version did not match. The whole batch was rolled back
	// and no row changed.
	UpdateBatchCASConflict
	// UpdateBatchCASCommitUnknown means the database work executed, but the
	// commit itself failed so the final state cannot be determined. No
	// automatic retry is performed and the in-memory objects are untouched.
	UpdateBatchCASCommitUnknown
	// UpdateBatchCASUnsupported means the current adapter cannot provide
	// the required compare-and-swap guarantees. No SQL was issued.
	UpdateBatchCASUnsupported
	// UpdateBatchCASRejected means pre-SQL validation failed (duplicate
	// primary key, version overflow, illegal column, budget exceeded, ...).
	// No SQL was issued.
	UpdateBatchCASRejected
)

// UpdateBatchCASResult describes how an UpdateBatchCAS call ended.
type UpdateBatchCASResult struct {
	// Status is the terminal outcome of the call.
	Status UpdateBatchCASStatus
	// Applied is the number of rows published after a confirmed commit.
	// It is zero on every non-committed path.
	Applied int
}

// Sentinel errors returned by UpdateBatchCAS entry points. They can be
// inspected with errors.Is/errors.As even when wrapped with a cause.
var (
	// ErrUpdateBatchCASConflict wraps an all-or-nothing batch that had to
	// roll back because an input row was missing or version-stale.
	ErrUpdateBatchCASConflict = errors.New("boil: update batch cas conflict, transaction rolled back")
	// ErrUpdateBatchCASUnsupported is returned when an adapter cannot
	// reliably verify row identity without relying on driver-specific
	// RowsAffected semantics.
	ErrUpdateBatchCASUnsupported = errors.New("boil: update batch cas is not supported by this adapter")
	// ErrUpdateBatchCASCommitUnknown wraps a failed commit. The wrapped
	// cause is preserved via errors.Unwrap/errors.Is.
	ErrUpdateBatchCASCommitUnknown = errors.New("boil: update batch cas commit outcome unknown, objects left unchanged and no retry was attempted")
	// ErrUpdateBatchCASVersionOverflow is returned when incrementing an
	// expected version would overflow its declared integer type.
	ErrUpdateBatchCASVersionOverflow = errors.New("boil: update batch cas version would overflow")
	// ErrUpdateBatchCASInvalid is returned for pre-SQL contract violations
	// such as nil rows, duplicate primary keys, illegal columns or an
	// empty batch. The concrete violation is wrapped around this sentinel.
	ErrUpdateBatchCASInvalid = errors.New("boil: update batch cas invalid request")
)

// savepointCounter guarantees unique savepoint names within a process.
var savepointCounter uint64

func newSavepointName() string {
	return fmt.Sprintf("sqlboiler_batch_cas_sp_%d", atomic.AddUint64(&savepointCounter, 1))
}

// CheckVersionIncrement verifies that current+1 stays inside the value range
// of current's concrete integer type. It accepts the closed set of non-null
// Go integer kinds; anything else (including nil and nullable wrappers) is
// rejected so a misconfigured version column can never reach SQL.
func CheckVersionIncrement(current any) error {
	if current == nil {
		return fmt.Errorf("%w: version value is nil", ErrUpdateBatchCASInvalid)
	}

	v := reflect.ValueOf(current)
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if v.Int() == maxSignedInt(v.Kind()) {
			return fmt.Errorf("%w: version %d is at the limit of %s",
				ErrUpdateBatchCASVersionOverflow, v.Int(), v.Kind())
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		if v.Uint() == maxUnsignedInt(v.Kind()) {
			return fmt.Errorf("%w: version %d is at the limit of %s",
				ErrUpdateBatchCASVersionOverflow, v.Uint(), v.Kind())
		}
	default:
		return fmt.Errorf("%w: version column must hold a non-null integer kind, got %T",
			ErrUpdateBatchCASInvalid, current)
	}

	return nil
}

func maxSignedInt(kind reflect.Kind) int64 {
	switch kind {
	case reflect.Int8:
		return math.MaxInt8
	case reflect.Int16:
		return math.MaxInt16
	case reflect.Int32:
		return math.MaxInt32
	default:
		// reflect.Int and reflect.Int64 share the int64 ceiling on every
		// platform sqlboiler supports.
		return math.MaxInt64
	}
}

func maxUnsignedInt(kind reflect.Kind) uint64 {
	switch kind {
	case reflect.Uint8:
		return math.MaxUint8
	case reflect.Uint16:
		return math.MaxUint16
	case reflect.Uint32:
		return math.MaxUint32
	default:
		// reflect.Uint, reflect.Uint64 and reflect.Uintptr share the
		// uint64 ceiling on every platform sqlboiler supports.
		return math.MaxUint64
	}
}

// BatchCASTx is the transaction boundary owned by a context-aware
// UpdateBatchCAS call. When the caller supplied a transaction it only holds
// a named savepoint: Commit releases it and Rollback rolls back to it, so the
// entry point never commits a transaction it does not own.
type BatchCASTx struct {
	exec      ContextExecutor
	owner     bool
	savepoint string
	closed    bool
}

// BeginContextBatchCAS opens the explicit, verifiable boundary used by a
// context-aware UpdateBatchCAS call.
//
//   - A ContextTransactor (e.g. *sql.Tx) gets a uniquely named SAVEPOINT.
//   - A ContextBeginner (e.g. *sql.DB) gets a brand new transaction which
//     the entry point must commit or roll back itself.
//   - Anything else is rejected with ErrUpdateBatchCASUnsupported.
func BeginContextBatchCAS(ctx context.Context, exec ContextExecutor) (*BatchCASTx, error) {
	if exec == nil {
		return nil, fmt.Errorf("%w: nil executor", ErrUpdateBatchCASInvalid)
	}

	if tx, ok := exec.(ContextTransactor); ok {
		name := newSavepointName()
		if _, err := tx.ExecContext(ctx, "SAVEPOINT "+name); err != nil {
			return nil, fmt.Errorf("update batch cas: failed to establish savepoint: %w", err)
		}
		return &BatchCASTx{exec: tx, savepoint: name}, nil
	}

	beginner, ok := exec.(ContextBeginner)
	if !ok {
		return nil, ErrUpdateBatchCASUnsupported
	}

	tx, err := beginner.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("update batch cas: failed to begin transaction: %w", err)
	}
	return &BatchCASTx{exec: tx, owner: true}, nil
}

// Executor returns the executor that statements must run against.
func (b *BatchCASTx) Executor() ContextExecutor {
	return b.exec
}

// OwnsTransaction reports whether the call began (and therefore must
// terminate) its own transaction.
func (b *BatchCASTx) OwnsTransaction() bool {
	return b.owner
}

// Commit either commits the owned transaction or releases the savepoint.
// A failed savepoint release is followed by a best-effort rollback to the
// same savepoint so partial changes cannot silently escape.
func (b *BatchCASTx) Commit(ctx context.Context) error {
	if b.closed {
		return fmt.Errorf("%w: batch transaction boundary already closed", ErrUpdateBatchCASInvalid)
	}
	b.closed = true

	if b.owner {
		if err := b.exec.(ContextTransactor).Commit(); err != nil {
			return err
		}
		return nil
	}

	if _, err := b.exec.ExecContext(ctx, "RELEASE SAVEPOINT "+b.savepoint); err != nil {
		if _, rbErr := b.exec.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+b.savepoint); rbErr == nil {
			_, _ = b.exec.ExecContext(ctx, "RELEASE SAVEPOINT "+b.savepoint)
		}
		return err
	}
	return nil
}

// Rollback either rolls back the owned transaction (tolerating an already
// finished one) or rolls back to the savepoint and releases it.
func (b *BatchCASTx) Rollback(ctx context.Context) error {
	if b.closed {
		return nil
	}
	b.closed = true

	if b.owner {
		err := b.exec.(ContextTransactor).Rollback()
		if errors.Is(err, sql.ErrTxDone) {
			return nil
		}
		return err
	}

	if _, err := b.exec.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+b.savepoint); err != nil {
		return err
	}
	_, err := b.exec.ExecContext(ctx, "RELEASE SAVEPOINT "+b.savepoint)
	return err
}

// BatchCASPlainTx is the non-context counterpart of BatchCASTx. It follows
// the exact same ownership and savepoint protocol.
type BatchCASPlainTx struct {
	exec      Executor
	owner     bool
	savepoint string
	closed    bool
}

// BeginBatchCAS opens the explicit, verifiable boundary used by a
// non-context UpdateBatchCAS call. See BeginContextBatchCAS for semantics.
func BeginBatchCAS(exec Executor) (*BatchCASPlainTx, error) {
	if exec == nil {
		return nil, fmt.Errorf("%w: nil executor", ErrUpdateBatchCASInvalid)
	}

	if tx, ok := exec.(Transactor); ok {
		name := newSavepointName()
		if _, err := tx.Exec("SAVEPOINT " + name); err != nil {
			return nil, fmt.Errorf("update batch cas: failed to establish savepoint: %w", err)
		}
		return &BatchCASPlainTx{exec: tx, savepoint: name}, nil
	}

	beginner, ok := exec.(Beginner)
	if !ok {
		return nil, ErrUpdateBatchCASUnsupported
	}

	tx, err := beginner.Begin()
	if err != nil {
		return nil, fmt.Errorf("update batch cas: failed to begin transaction: %w", err)
	}
	return &BatchCASPlainTx{exec: tx, owner: true}, nil
}

// Executor returns the executor that statements must run against.
func (b *BatchCASPlainTx) Executor() Executor {
	return b.exec
}

// OwnsTransaction reports whether the call began (and therefore must
// terminate) its own transaction.
func (b *BatchCASPlainTx) OwnsTransaction() bool {
	return b.owner
}

// Commit either commits the owned transaction or releases the savepoint.
func (b *BatchCASPlainTx) Commit() error {
	if b.closed {
		return fmt.Errorf("%w: batch transaction boundary already closed", ErrUpdateBatchCASInvalid)
	}
	b.closed = true

	if b.owner {
		return b.exec.(Transactor).Commit()
	}

	if _, err := b.exec.Exec("RELEASE SAVEPOINT " + b.savepoint); err != nil {
		if _, rbErr := b.exec.Exec("ROLLBACK TO SAVEPOINT " + b.savepoint); rbErr == nil {
			_, _ = b.exec.Exec("RELEASE SAVEPOINT " + b.savepoint)
		}
		return err
	}
	return nil
}

// Rollback either rolls back the owned transaction (tolerating an already
// finished one) or rolls back to the savepoint and releases it.
func (b *BatchCASPlainTx) Rollback() error {
	if b.closed {
		return nil
	}
	b.closed = true

	if b.owner {
		err := b.exec.(Transactor).Rollback()
		if errors.Is(err, sql.ErrTxDone) {
			return nil
		}
		return err
	}

	if _, err := b.exec.Exec("ROLLBACK TO SAVEPOINT " + b.savepoint); err != nil {
		return err
	}
	_, err := b.exec.Exec("RELEASE SAVEPOINT " + b.savepoint)
	return err
}
