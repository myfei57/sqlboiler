package boil

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestCheckVersionIncrement(t *testing.T) {
	t.Parallel()

	okValues := []any{
		int(0), int8(-1), int16(12), int32(math.MaxInt32 - 1), int64(-100),
		uint(5), uint8(200), uint16(30000), uint32(4000000000), uint64(1 << 40),
	}
	for _, v := range okValues {
		if err := CheckVersionIncrement(v); err != nil {
			t.Errorf("expected %#v (%T) to be incrementable, got %v", v, v, err)
		}
	}

	maxValues := []any{
		int(math.MaxInt64), int8(math.MaxInt8), int16(math.MaxInt16), int32(math.MaxInt32), int64(math.MaxInt64),
		uint(math.MaxUint64), uint8(math.MaxUint8), uint16(math.MaxUint16),
		uint32(math.MaxUint32), uint64(math.MaxUint64), uintptr(math.MaxUint64),
	}
	for _, v := range maxValues {
		err := CheckVersionIncrement(v)
		if !errors.Is(err, ErrUpdateBatchCASVersionOverflow) {
			t.Errorf("expected overflow for %#v (%T), got %v", v, v, err)
		}
	}

	if err := CheckVersionIncrement(nil); !errors.Is(err, ErrUpdateBatchCASInvalid) {
		t.Errorf("expected invalid for nil, got %v", err)
	}
	if err := CheckVersionIncrement("12"); !errors.Is(err, ErrUpdateBatchCASInvalid) {
		t.Errorf("expected invalid for string, got %v", err)
	}
	if err := CheckVersionIncrement(struct{}{}); !errors.Is(err, ErrUpdateBatchCASInvalid) {
		t.Errorf("expected invalid for struct, got %v", err)
	}
}

// recordingExecutor keeps every SQL statement it receives.
type recordingExecutor struct {
	statements []string
	beginErr   error
}

func (r *recordingExecutor) Exec(query string, args ...any) (sql.Result, error) {
	r.statements = append(r.statements, query)
	return driverResultFake{}, nil
}
func (r *recordingExecutor) Query(query string, args ...any) (*sql.Rows, error) {
	r.statements = append(r.statements, query)
	return nil, nil
}
func (r *recordingExecutor) QueryRow(query string, args ...any) *sql.Row {
	r.statements = append(r.statements, query)
	return nil
}

// recordingTransactor is a caller-owned transaction: Commit must never be
// invoked by the batch guard.
type recordingTransactor struct {
	recordingExecutor
	commits   int
	rollbacks int
}

func (r *recordingTransactor) Commit() error {
	r.commits++
	return nil
}
func (r *recordingTransactor) Rollback() error {
	r.rollbacks++
	return nil
}

type recordingBeginner struct {
	recordingExecutor
	tx       *recordingTransactor
	beginCnt int
}

func (r *recordingBeginner) Begin() (*sql.Tx, error) {
	r.beginCnt++
	return nil, errors.New("Begin is not used by the context guard")
}

type ctxRecordingTransactor struct {
	recordingTransactor
}

func (c *ctxRecordingTransactor) ExecContext(_ context.Context, query string, args ...any) (sql.Result, error) {
	return c.recordingTransactor.Exec(query, args...)
}
func (c *ctxRecordingTransactor) QueryContext(_ context.Context, query string, args ...any) (*sql.Rows, error) {
	return c.recordingTransactor.Query(query, args...)
}
func (c *ctxRecordingTransactor) QueryRowContext(_ context.Context, query string, args ...any) *sql.Row {
	return c.recordingTransactor.QueryRow(query, args...)
}

type ctxRecordingBeginner struct {
	recordingBeginner
	ctxTx      *ctxRecordingTransactor
	beginTxCnt int
}

func (c *ctxRecordingBeginner) ExecContext(_ context.Context, query string, args ...any) (sql.Result, error) {
	return c.recordingExecutor.Exec(query, args...)
}
func (c *ctxRecordingBeginner) QueryContext(_ context.Context, query string, args ...any) (*sql.Rows, error) {
	return c.recordingExecutor.Query(query, args...)
}
func (c *ctxRecordingBeginner) QueryRowContext(_ context.Context, query string, args ...any) *sql.Row {
	return c.recordingExecutor.QueryRow(query, args...)
}

func (c *ctxRecordingBeginner) BeginTx(_ context.Context, _ *sql.TxOptions) (*sql.Tx, error) {
	c.beginTxCnt++
	return nil, errors.New("real BeginTx must not be called on the fake")
}

type driverResultFake struct{}

func (driverResultFake) LastInsertId() (int64, error) { return 0, nil }
func (driverResultFake) RowsAffected() (int64, error) { return 0, nil }

func TestBeginBatchCAS_SavepointProtocol(t *testing.T) {
	t.Parallel()

	tx := &recordingTransactor{}
	guard, err := BeginBatchCAS(tx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if guard.OwnsTransaction() {
		t.Fatal("savepoint guard must not own the transaction")
	}
	if len(tx.statements) != 1 || tx.statements[0][:9] != "SAVEPOINT" {
		t.Fatalf("expected a SAVEPOINT statement, got %#v", tx.statements)
	}
	savepoint := tx.statements[0][10:]

	if err := guard.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if tx.commits != 0 {
		t.Fatal("caller transaction must never be committed by the guard")
	}
	if len(tx.statements) != 2 || tx.statements[1] != "RELEASE SAVEPOINT "+savepoint {
		t.Fatalf("expected release of %q, got %#v", savepoint, tx.statements)
	}
}

func TestBeginBatchCAS_RollbackToSavepoint(t *testing.T) {
	t.Parallel()

	tx := &recordingTransactor{}
	guard, err := BeginBatchCAS(tx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	savepoint := tx.statements[0][10:]

	if err := guard.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if tx.rollbacks != 0 {
		t.Fatal("caller transaction must never be rolled back by the guard")
	}
	want := []string{
		"SAVEPOINT " + savepoint,
		"ROLLBACK TO SAVEPOINT " + savepoint,
		"RELEASE SAVEPOINT " + savepoint,
	}
	if len(tx.statements) != len(want) {
		t.Fatalf("expected %#v, got %#v", want, tx.statements)
	}
	for i := range want {
		if tx.statements[i] != want[i] {
			t.Fatalf("statement %d: want %q got %q", i, want[i], tx.statements[i])
		}
	}
}

func TestBeginBatchCAS_OwnedTransaction(t *testing.T) {
	t.Parallel()

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectRollback()
	guard, err := BeginBatchCAS(db)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !guard.OwnsTransaction() {
		t.Fatal("guard should own the freshly begun transaction")
	}
	if err := guard.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}

	mock.ExpectBegin()
	mock.ExpectCommit()
	guard, err = BeginBatchCAS(db)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := guard.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestBeginContextBatchCAS_OwnedTransaction(t *testing.T) {
	t.Parallel()

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	mock.ExpectBegin()
	mock.ExpectCommit()
	guard, err := BeginContextBatchCAS(ctx, db)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !guard.OwnsTransaction() {
		t.Fatal("guard should own the freshly begun transaction")
	}
	if err := guard.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestBeginBatchCAS_UnsupportedAndNil(t *testing.T) {
	t.Parallel()

	if _, err := BeginBatchCAS(nil); !errors.Is(err, ErrUpdateBatchCASInvalid) {
		t.Fatalf("expected invalid on nil, got %v", err)
	}
	if _, err := BeginBatchCAS(&recordingExecutor{}); !errors.Is(err, ErrUpdateBatchCASUnsupported) {
		t.Fatalf("expected unsupported on plain executor, got %v", err)
	}
}

func TestBeginContextBatchCAS_SavepointAndUnsupported(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tx := &ctxRecordingTransactor{}
	guard, err := BeginContextBatchCAS(ctx, tx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if guard.OwnsTransaction() {
		t.Fatal("savepoint guard must not own the transaction")
	}
	if err := guard.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if tx.commits != 0 {
		t.Fatal("caller tx must not be committed by the guard")
	}

	bad := &ctxRecordingBeginner{}
	if _, err := BeginContextBatchCAS(ctx, bad); err == nil {
		t.Fatal("expected BeginTx failure to propagate")
	}
	if bad.beginTxCnt != 1 {
		t.Fatalf("expected one BeginTx attempt, got %d", bad.beginTxCnt)
	}

	if _, err := BeginContextBatchCAS(ctx, nil); !errors.Is(err, ErrUpdateBatchCASInvalid) {
		t.Fatalf("expected invalid on nil, got %v", err)
	}
}

func TestBatchCASTx_DoubleClose(t *testing.T) {
	t.Parallel()

	tx := &recordingTransactor{}
	guard, err := BeginBatchCAS(tx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := guard.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if err := guard.Rollback(); err != nil {
		t.Fatalf("second rollback must be a no-op, got %v", err)
	}
	if err := guard.Commit(); !errors.Is(err, ErrUpdateBatchCASInvalid) {
		t.Fatalf("commit after close should be invalid, got %v", err)
	}
}
