package models_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"sync"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/aarondl/null/v8"
	_ "modernc.org/sqlite"

	"github.com/aarondl/sqlboiler/v4/boil"

	// The generated package lives at the root of the nested module and
	// declares package name "models".
	"github.com/aarondl/sqlboiler-casdemo"
)

// ---- helpers ---------------------------------------------------------------

func dbPath() string {
	v := os.Getenv("CAS_FIXTURE_DB")
	if v == "" {
		panic("missing CAS_FIXTURE_DB: the suite must be launched by casdemo/cas_test.go")
	}
	return v
}

func rawOpen(t *testing.T) *sql.DB {
	t.Helper()
	dsn := "file:" + dbPath() + "?_pragma=busy_timeout(10000)&_loc=UTC"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// openFixtureDB hands back a pool over a freshly emptied database so tests
// never observe rows left behind by another test.
func openFixtureDB(t *testing.T) *sql.DB {
	t.Helper()
	db := rawOpen(t)
	for _, table := range []string{"cas_items", "cas_counters", "cas_ticks"} {
		if _, err := db.Exec("DELETE FROM " + table); err != nil {
			t.Fatalf("reset %s: %v", table, err)
		}
	}
	return db
}

func seedItems(t *testing.T, db *sql.DB, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		_, err := db.Exec(
			`INSERT INTO cas_items(tenant_id, item_id, name, note, weight, raw, version) VALUES(?,?,?,?,?,?,0)`,
			"tenant-a", int64(i+1), fmt.Sprintf("name-%d", i),
			fmt.Sprintf("note-%d", i), float64(i)*1.5, []byte{byte(i), byte(i + 1)},
		)
		if err != nil {
			t.Fatalf("seed item %d: %v", i, err)
		}
	}
}

func itemRows(t *testing.T, db *sql.DB) map[int64]struct {
	name    string
	version int64
} {
	t.Helper()
	rows, err := db.Query(`SELECT item_id, name, version FROM cas_items ORDER BY item_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int64]struct {
		name    string
		version int64
	}{}
	for rows.Next() {
		var id, version int64
		var name string
		if err := rows.Scan(&id, &name, &version); err != nil {
			t.Fatal(err)
		}
		out[id] = struct {
			name    string
			version int64
		}{name: name, version: version}
	}
	return out
}

func freshItems(n int) []*models.CasItem {
	objs := make([]*models.CasItem, n)
	for i := range objs {
		objs[i] = &models.CasItem{
			TenantID: "tenant-a",
			ItemID:   int64(i + 1),
			Name:     fmt.Sprintf("new-%d", i),
			Note:     null.String{String: fmt.Sprintf("new-note-%d", i), Valid: true},
			Weight:   null.Float64{Float64: float64(i) + 0.25, Valid: true},
			Raw:      null.Bytes{Bytes: []byte{0xA, byte(i)}, Valid: true},
			Version:  0,
		}
	}
	return objs
}

// countingDB wraps a pool and counts statements. It still supports BeginTx
// so the generated code can own a transaction.
type countingDB struct {
	inner *sql.DB
	mu    sync.Mutex
	calls int
}

func (c *countingDB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.inner.ExecContext(ctx, query, args...)
}
func (c *countingDB) Exec(query string, args ...any) (sql.Result, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.inner.Exec(query, args...)
}
func (c *countingDB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.inner.QueryContext(ctx, query, args...)
}
func (c *countingDB) Query(query string, args ...any) (*sql.Rows, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.inner.Query(query, args...)
}
func (c *countingDB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.inner.QueryRowContext(ctx, query, args...)
}
func (c *countingDB) QueryRow(query string, args ...any) *sql.Row {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.inner.QueryRow(query, args...)
}
func (c *countingDB) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	return c.inner.BeginTx(ctx, opts)
}
func (c *countingDB) Begin() (*sql.Tx, error) { return c.inner.Begin() }
func (c *countingDB) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// nonCommittingTx wraps a caller-owned transaction and fails the test if the
// generated code ever tries to commit it.
type nonCommittingTx struct {
	inner   *sql.Tx
	commits int
}

func (n *nonCommittingTx) Commit() error {
	n.commits++
	return errors.New("caller transaction must not be committed by UpdateBatchCAS")
}
func (n *nonCommittingTx) Rollback() error { return n.inner.Rollback() }
func (n *nonCommittingTx) Exec(q string, args ...any) (sql.Result, error) {
	return n.inner.Exec(q, args...)
}
func (n *nonCommittingTx) Query(q string, args ...any) (*sql.Rows, error) {
	return n.inner.Query(q, args...)
}
func (n *nonCommittingTx) QueryRow(q string, args ...any) *sql.Row {
	return n.inner.QueryRow(q, args...)
}
func (n *nonCommittingTx) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return n.inner.ExecContext(ctx, q, args...)
}
func (n *nonCommittingTx) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return n.inner.QueryContext(ctx, q, args...)
}
func (n *nonCommittingTx) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return n.inner.QueryRowContext(ctx, q, args...)
}

// ---- tests -----------------------------------------------------------------

func TestUpdateBatchCAS_HappyPathCompositeKey(t *testing.T) {
	db := openFixtureDB(t)
	seedItems(t, db, 3)
	objs := freshItems(3)

	res, err := models.UpdateBatchCASCasItem(context.Background(), db, objs,
		boil.Whitelist("name", "note", "weight", "raw"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != boil.UpdateBatchCASCommitted || res.Applied != 3 {
		t.Fatalf("unexpected result: %+v", res)
	}
	for i, o := range objs {
		if o.Version != 1 {
			t.Errorf("obj %d version published too early/wrong: %d", i, o.Version)
		}
	}

	rows := itemRows(t, db)
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(rows))
	}
	for i := int64(1); i <= 3; i++ {
		got := rows[i]
		if got.version != 1 {
			t.Errorf("row %d version: want 1 got %d", i, got.version)
		}
		if got.name != fmt.Sprintf("new-%d", i-1) {
			t.Errorf("row %d name: %q", i, got.name)
		}
	}

	var note sql.NullString
	var weight sql.NullFloat64
	var raw []byte
	err = db.QueryRow(`SELECT note, weight, raw FROM cas_items WHERE item_id = 2`).Scan(&note, &weight, &raw)
	if err != nil {
		t.Fatal(err)
	}
	if !note.Valid || note.String != "new-note-1" {
		t.Errorf("note mismatch: %+v", note)
	}
	if !weight.Valid || weight.Float64 != 1.25 {
		t.Errorf("weight mismatch: %+v", weight)
	}
	if len(raw) != 2 || raw[0] != 0xA || raw[1] != 1 {
		t.Errorf("raw mismatch: %v", raw)
	}
}

func TestUpdateBatchCAS_InferColumns(t *testing.T) {
	db := openFixtureDB(t)
	seedItems(t, db, 2)
	objs := freshItems(2)
	// Leave nullable columns explicitly NULL to exercise null bindings.
	for _, o := range objs {
		o.Note = null.String{}
		o.Weight = null.Float64{}
		o.Raw = null.Bytes{}
	}

	res, err := models.UpdateBatchCASCasItem(context.Background(), db, objs, boil.Infer())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != boil.UpdateBatchCASCommitted {
		t.Fatalf("unexpected status: %+v", res)
	}

	var note sql.NullString
	if err := db.QueryRow(`SELECT note FROM cas_items WHERE item_id = 1`).Scan(&note); err != nil {
		t.Fatal(err)
	}
	if note.Valid {
		t.Errorf("infer update must persist NULL note, got %q", note.String)
	}
}

func TestUpdateBatchCAS_MissingRowRollsBack(t *testing.T) {
	db := openFixtureDB(t)
	seedItems(t, db, 2)
	objs := freshItems(3) // item_id 3 does not exist
	originals := make([]models.CasItem, 3)
	for i, o := range objs {
		originals[i] = *o
	}

	res, err := models.UpdateBatchCASCasItem(context.Background(), db, objs, boil.Whitelist("name"))
	if !errors.Is(err, boil.ErrUpdateBatchCASConflict) {
		t.Fatalf("want conflict sentinel, got %v", err)
	}
	if res.Status != boil.UpdateBatchCASConflict || res.Applied != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
	for i, o := range objs {
		if o.Version != originals[i].Version || o.Name != originals[i].Name {
			t.Errorf("obj %d was not restored: %+v", i, o)
		}
	}
	for id, row := range itemRows(t, db) {
		if row.version != 0 || row.name != fmt.Sprintf("name-%d", id-1) {
			t.Errorf("row %d changed despite rollback: %+v", id, row)
		}
	}
}

func TestUpdateBatchCAS_StaleVersionRollsBack(t *testing.T) {
	db := openFixtureDB(t)
	seedItems(t, db, 1)
	objs := freshItems(1)
	objs[0].Version = 42

	res, err := models.UpdateBatchCASCasItem(context.Background(), db, objs, boil.Whitelist("name"))
	if !errors.Is(err, boil.ErrUpdateBatchCASConflict) {
		t.Fatalf("want conflict sentinel, got %v", err)
	}
	if res.Status != boil.UpdateBatchCASConflict {
		t.Fatalf("unexpected status: %+v", res)
	}
	if objs[0].Version != 42 {
		t.Error("stale object must keep its original version on conflict")
	}
	if itemRows(t, db)[1].version != 0 {
		t.Error("database row changed despite stale version")
	}
}

func TestUpdateBatchCAS_SameValueStillCommits(t *testing.T) {
	db := openFixtureDB(t)
	seedItems(t, db, 1)
	// Bind exactly the values already stored: the row matches but nothing
	// "changes". A RowsAffected-based implementation could report 0 here; the
	// RETURNING identity check must still commit the version bump.
	obj := &models.CasItem{
		TenantID: "tenant-a", ItemID: 1, Name: "name-0",
		Note:    null.String{String: "note-0", Valid: true},
		Version: 0,
	}
	res, err := models.UpdateBatchCASCasItem(context.Background(), db,
		[]*models.CasItem{obj}, boil.Whitelist("name", "note"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != boil.UpdateBatchCASCommitted {
		t.Fatalf("same-value update must commit via RETURNING: %+v", res)
	}
	if itemRows(t, db)[1].version != 1 {
		t.Error("version must be incremented even when values are identical")
	}
}

func TestUpdateBatchCAS_PreSQLRejections(t *testing.T) {
	db := openFixtureDB(t)
	seedItems(t, db, 1)
	counted := &countingDB{inner: db}
	ctx := context.Background()

	t.Run("empty batch", func(t *testing.T) {
		_, err := models.UpdateBatchCASCasItem(ctx, counted, nil, boil.Whitelist("name"))
		if !errors.Is(err, boil.ErrUpdateBatchCASInvalid) {
			t.Fatalf("want invalid, got %v", err)
		}
	})

	t.Run("nil element", func(t *testing.T) {
		_, err := models.UpdateBatchCASCasItem(ctx, counted, []*models.CasItem{nil}, boil.Whitelist("name"))
		if !errors.Is(err, boil.ErrUpdateBatchCASInvalid) {
			t.Fatalf("want invalid, got %v", err)
		}
	})

	t.Run("duplicate full primary key", func(t *testing.T) {
		objs := freshItems(2)
		objs[1].TenantID, objs[1].ItemID = objs[0].TenantID, objs[0].ItemID
		_, err := models.UpdateBatchCASCasItem(ctx, counted, objs, boil.Whitelist("name"))
		if !errors.Is(err, boil.ErrUpdateBatchCASInvalid) {
			t.Fatalf("want invalid, got %v", err)
		}
	})

	t.Run("unknown column", func(t *testing.T) {
		_, err := models.UpdateBatchCASCasItem(ctx, counted, freshItems(1), boil.Whitelist("nope"))
		if !errors.Is(err, boil.ErrUpdateBatchCASInvalid) {
			t.Fatalf("want invalid, got %v", err)
		}
	})

	t.Run("primary key column", func(t *testing.T) {
		_, err := models.UpdateBatchCASCasItem(ctx, counted, freshItems(1), boil.Whitelist("item_id"))
		if !errors.Is(err, boil.ErrUpdateBatchCASInvalid) {
			t.Fatalf("want invalid, got %v", err)
		}
	})

	t.Run("version column", func(t *testing.T) {
		_, err := models.UpdateBatchCASCasItem(ctx, counted, freshItems(1), boil.Whitelist("version"))
		if !errors.Is(err, boil.ErrUpdateBatchCASInvalid) {
			t.Fatalf("want invalid, got %v", err)
		}
	})

	t.Run("generated column", func(t *testing.T) {
		tick := &models.CasTick{Step: 1, Version: 0}
		_, err := models.UpdateBatchCASCasTick(ctx, counted, []*models.CasTick{tick}, boil.Whitelist("id"))
		if !errors.Is(err, boil.ErrUpdateBatchCASInvalid) {
			t.Fatalf("want invalid, got %v", err)
		}
	})

	t.Run("blacklist column set unsupported", func(t *testing.T) {
		_, err := models.UpdateBatchCASCasItem(ctx, counted, freshItems(1), boil.Blacklist("name"))
		if !errors.Is(err, boil.ErrUpdateBatchCASInvalid) {
			t.Fatalf("want invalid, got %v", err)
		}
	})

	t.Run("batch size budget", func(t *testing.T) {
		// Default generated budget is 100 rows per batch.
		objs := make([]*models.CasItem, 101)
		for i := range objs {
			objs[i] = &models.CasItem{TenantID: "t", ItemID: int64(i), Name: "x", Version: 0}
		}
		_, err := models.UpdateBatchCASCasItem(ctx, counted, objs, boil.Whitelist("name"))
		if !errors.Is(err, boil.ErrUpdateBatchCASInvalid) {
			t.Fatalf("want invalid, got %v", err)
		}
	})

	t.Run("int64 version overflow", func(t *testing.T) {
		obj := freshItems(1)[0]
		obj.Version = math.MaxInt64
		_, err := models.UpdateBatchCASCasItem(ctx, counted, []*models.CasItem{obj}, boil.Whitelist("name"))
		if !errors.Is(err, boil.ErrUpdateBatchCASVersionOverflow) {
			t.Fatalf("want overflow, got %v", err)
		}
	})

	t.Run("int16 version overflow", func(t *testing.T) {
		tick := &models.CasTick{Step: 1, Version: math.MaxInt16}
		_, err := models.UpdateBatchCASCasTick(ctx, counted, []*models.CasTick{tick}, boil.Whitelist("step"))
		if !errors.Is(err, boil.ErrUpdateBatchCASVersionOverflow) {
			t.Fatalf("want overflow, got %v", err)
		}
	})

	if counted.count() != 0 {
		t.Fatalf("rejected batches must issue no SQL, saw %d statements", counted.count())
	}
	if rows := itemRows(t, db); len(rows) != 1 || rows[1].version != 0 {
		t.Fatalf("database must be untouched after rejections: %+v", rows)
	}
}

func TestUpdateBatchCAS_Cancellation(t *testing.T) {
	db := openFixtureDB(t)
	seedItems(t, db, 2)
	objs := freshItems(2)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := models.UpdateBatchCASCasItem(ctx, db, objs, boil.Whitelist("name"))
	if err == nil {
		t.Fatalf("cancelled context must produce an error, got %+v", res)
	}
	for i, o := range objs {
		if o.Version != 0 {
			t.Errorf("obj %d mutated despite cancellation: version %d", i, o.Version)
		}
	}
	for id, row := range itemRows(t, db) {
		if row.version != 0 {
			t.Errorf("row %d changed despite cancellation", id)
		}
	}
}

func TestUpdateBatchCAS_CommitError(t *testing.T) {
	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer sqlDB.Close()

	obj := &models.CasItem{TenantID: "tenant-a", ItemID: 7, Name: "n", Version: 0}
	mock.ExpectBegin()
	mock.MatchExpectationsInOrder(true)
	mock.ExpectQuery(regexp.QuoteMeta(`UPDATE "cas_items" SET "name"=`)).
		WithArgs("n", "tenant-a", int64(7), int64(0)).
		WillReturnRows(sqlmock.NewRows([]string{"tenant_id", "item_id"}).AddRow("tenant-a", int64(7)))
	boom := errors.New("synthetic commit failure")
	mock.ExpectCommit().WillReturnError(boom)

	res, err := models.UpdateBatchCASCasItem(context.Background(), sqlDB,
		[]*models.CasItem{obj}, boil.Whitelist("name"))
	if !errors.Is(err, boil.ErrUpdateBatchCASCommitUnknown) {
		t.Fatalf("want commit-unknown sentinel, got %v", err)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("commit cause must be preserved: %v", err)
	}
	if res.Status != boil.UpdateBatchCASCommitUnknown {
		t.Fatalf("unexpected status: %+v", res)
	}
	if obj.Version != 0 || obj.Name != "n" {
		t.Errorf("object must remain untouched on uncertain commit: %+v", obj)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestUpdateBatchCAS_SavepointOnCallerTransaction(t *testing.T) {
	db := openFixtureDB(t)
	seedItems(t, db, 2)

	t.Run("success releases savepoint without committing caller tx", func(t *testing.T) {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		wrapped := &nonCommittingTx{inner: tx}
		objs := freshItems(2)
		res, err := models.UpdateBatchCASCasItem(context.Background(), wrapped, objs, boil.Whitelist("name"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Status != boil.UpdateBatchCASCommitted {
			t.Fatalf("unexpected status: %+v", res)
		}
		if wrapped.commits != 0 {
			t.Fatal("generated code must never commit a transaction it does not own")
		}
		// Changes are still invisible to another connection while the caller
		// transaction stays open.
		other := rawOpen(t)
		if row := itemRows(t, other)[1]; row.version != 0 {
			t.Fatal("changes leaked before caller committed its transaction")
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if row := itemRows(t, db)[1]; row.version != 1 {
			t.Fatal("changes missing after caller commit")
		}
	})

	t.Run("conflict rolls back to savepoint and keeps caller tx usable", func(t *testing.T) {
		// Re-establish the seeded baseline after the previous subtest commit.
		for _, table := range []string{"cas_items", "cas_counters", "cas_ticks"} {
			if _, err := db.Exec("DELETE FROM " + table); err != nil {
				t.Fatal(err)
			}
		}
		seedItems(t, db, 2)

		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		objs := freshItems(2)
		objs[1].Version = 9 // stale -> conflict inside the savepoint
		res, err := models.UpdateBatchCASCasItem(context.Background(), tx, objs, boil.Whitelist("name"))
		if !errors.Is(err, boil.ErrUpdateBatchCASConflict) || res.Status != boil.UpdateBatchCASConflict {
			t.Fatalf("want conflict, got %+v %v", res, err)
		}
		var count int
		if err := tx.QueryRow(`SELECT count(*) FROM cas_items`).Scan(&count); err != nil {
			t.Fatalf("caller transaction must remain usable: %v", err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		for id, row := range itemRows(t, db) {
			if row.version != 0 {
				t.Errorf("row %d changed despite savepoint rollback", id)
			}
		}
	})
}

func TestUpdateBatchCAS_HooksOrderAndTiming(t *testing.T) {
	db := openFixtureDB(t)
	seedItems(t, db, 2)

	var mu sync.Mutex
	var events []string
	models.AddCasItemHook(boil.BeforeUpdateHook, func(_ context.Context, _ boil.ContextExecutor, o *models.CasItem) error {
		mu.Lock()
		events = append(events, "before-"+fmt.Sprint(o.ItemID))
		mu.Unlock()
		return nil
	})
	models.AddCasItemHook(boil.AfterUpdateHook, func(_ context.Context, _ boil.ContextExecutor, o *models.CasItem) error {
		mu.Lock()
		defer mu.Unlock()
		if o.Version != 1 {
			t.Errorf("after hook must observe published version, got %d", o.Version)
		}
		events = append(events, "after-"+fmt.Sprint(o.ItemID))
		return nil
	})

	objs := freshItems(2)
	res, err := models.UpdateBatchCASCasItem(context.Background(), db, objs, boil.Whitelist("name"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != boil.UpdateBatchCASCommitted {
		t.Fatalf("unexpected status: %+v", res)
	}
	want := []string{"before-1", "before-2", "after-1", "after-2"}
	if len(events) < len(want) {
		t.Fatalf("hook events missing: %v", events)
	}
	got := events[len(events)-4:]
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("hook order: want %v got %v (full: %v)", want, got, events)
		}
	}
}

func TestUpdateBatchCAS_FailingHooks(t *testing.T) {
	db := openFixtureDB(t)
	seedItems(t, db, 1)
	counted := &countingDB{inner: db}
	boom := errors.New("before hook boom")
	models.AddCasItemHook(boil.BeforeUpdateHook, func(_ context.Context, _ boil.ContextExecutor, o *models.CasItem) error {
		if o.Name == "hook-boom" {
			return boom
		}
		return nil
	})

	obj := freshItems(1)[0]
	obj.Name = "hook-boom"
	_, err := models.UpdateBatchCASCasItem(context.Background(), counted, []*models.CasItem{obj}, boil.Whitelist("name"))
	if !errors.Is(err, boom) {
		t.Fatalf("hook cause must be preserved, got %v", err)
	}
	if counted.count() != 0 {
		t.Fatalf("failing before hook must prevent all SQL, saw %d statements", counted.count())
	}
	if obj.Version != 0 {
		t.Error("object must be restored after before-hook failure")
	}

	afterBoom := errors.New("after hook boom")
	models.AddCasItemHook(boil.AfterUpdateHook, func(_ context.Context, _ boil.ContextExecutor, o *models.CasItem) error {
		// Scoped to this object so the hook keeps succeeding for every
		// later test in the same generated test binary.
		if o.Name == "after-boom-marker" {
			return afterBoom
		}
		return nil
	})
	good := freshItems(1)[0]
	good.Name = "after-boom-marker"
	res, err := models.UpdateBatchCASCasItem(context.Background(), db, []*models.CasItem{good}, boil.Whitelist("name"))
	if !errors.Is(err, afterBoom) {
		t.Fatalf("after hook cause must be preserved, got %v", err)
	}
	if res.Status != boil.UpdateBatchCASCommitted || res.Applied != 1 {
		t.Fatalf("commit must already be confirmed when after hook fails: %+v", res)
	}
	if itemRows(t, db)[1].version != 1 {
		t.Error("after-hook failure cannot roll back the already durable commit")
	}
}

func TestUpdateBatchCAS_ConcurrentContention(t *testing.T) {
	// Two independent pools over the same SQLite file, each single-connection
	// so transactions stay ordered. busy_timeout makes the loser wait for the
	// winner instead of failing with SQLITE_BUSY.
	open := func() *sql.DB {
		dsn := "file:" + dbPath() + "?_pragma=busy_timeout(10000)&_loc=UTC"
		pool, err := sql.Open("sqlite", dsn)
		if err != nil {
			t.Fatal(err)
		}
		pool.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = pool.Close() })
		return pool
	}

	seedDB := openFixtureDB(t)
	seedItems(t, seedDB, 5)

	poolA, poolB := open(), open()
	start := make(chan struct{})
	var wg sync.WaitGroup
	type outcome struct {
		status boil.UpdateBatchCASStatus
		err    error
		objs   []*models.CasItem
	}
	outcomes := make([]outcome, 2)

	run := func(idx int, pool *sql.DB, prefix string) {
		defer wg.Done()
		objs := make([]*models.CasItem, 5)
		for i := range objs {
			objs[i] = &models.CasItem{
				TenantID: "tenant-a", ItemID: int64(i + 1),
				Name: prefix + "-" + fmt.Sprint(i), Version: 0,
			}
		}
		<-start
		res, err := models.UpdateBatchCASCasItem(context.Background(), pool, objs, boil.Whitelist("name"))
		outcomes[idx] = outcome{status: res.Status, err: err, objs: objs}
	}

	wg.Add(2)
	go run(0, poolA, "A")
	go run(1, poolB, "B")
	close(start)
	wg.Wait()

	var committed, conflict int
	var winner string
	for i, oc := range outcomes {
		switch oc.status {
		case boil.UpdateBatchCASCommitted:
			committed++
			winner = fmt.Sprintf("%c", 'A'+i)
			if oc.err != nil {
				t.Errorf("winner %d returned error: %v", i, oc.err)
			}
		case boil.UpdateBatchCASConflict:
			conflict++
			if !errors.Is(oc.err, boil.ErrUpdateBatchCASConflict) {
				t.Errorf("loser %d must wrap the conflict sentinel: %v", i, oc.err)
			}
			for _, o := range oc.objs {
				if o.Version != 0 {
					t.Errorf("loser object published despite conflict: %+v", o)
				}
			}
		default:
			t.Fatalf("unexpected outcome %d: %+v %v", i, oc.status, oc.err)
		}
	}
	if committed != 1 || conflict != 1 {
		t.Fatalf("exactly one batch must commit, got committed=%d conflict=%d", committed, conflict)
	}

	rows := itemRows(t, seedDB)
	if len(rows) != 5 {
		t.Fatalf("row count changed: %d", len(rows))
	}
	for id, row := range rows {
		if row.version != 1 {
			t.Errorf("row %d version: want exactly 1 got %d", id, row.version)
		}
		if row.name != winner+"-"+fmt.Sprint(id-1) {
			t.Errorf("row %d name must come from winner %s only, got %q", id, winner, row.name)
		}
	}
}
