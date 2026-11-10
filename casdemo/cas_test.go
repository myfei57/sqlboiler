// Package casdemo_test orchestrates the end-to-end UpdateBatchCAS fixture:
// it creates a real SQLite database, generates an sqlboiler package against
// it and runs the functional test suite (including a real concurrency race)
// inside an isolated nested module.
package casdemo_test

import (
	"bytes"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/aarondl/sqlboiler/v4/boilingcore"
	"github.com/aarondl/sqlboiler/v4/drivers"
	_ "github.com/aarondl/sqlboiler/v4/drivers/sqlboiler-sqlite3/driver"
	"github.com/aarondl/sqlboiler/v4/importers"
)

func TestUpdateBatchCASSuite(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}

	work := t.TempDir()
	dbPath := filepath.Join(work, "fixture.db")

	ddl, err := os.ReadFile(filepath.Join("schema.sql"))
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	modelsDir := filepath.Join(work, "models")
	config := &boilingcore.Config{
		DriverName:   "sqlite3",
		PkgName:      "models",
		OutFolder:    modelsDir,
		NoTests:      true,
		DriverConfig: drivers.Config{"dbname": dbPath},
		Imports:      importers.NewDefaultImports(),
		UpdateBatchCAS: boilingcore.UpdateBatchCASConfig{
			VersionColumn: "version",
		},
	}
	state, err := boilingcore.New(config)
	if err != nil {
		t.Fatalf("boilingcore.New: %v", err)
	}
	if err := state.Run(); err != nil {
		t.Fatalf("boilingcore.Run: %v", err)
	}

	functionalTest, err := os.ReadFile(filepath.Join("testdata", "main_test.go"))
	if err != nil {
		t.Fatalf("read functional test: %v", err)
	}
	if err := os.WriteFile(filepath.Join(modelsDir, "main_test.go"), functionalTest, 0o664); err != nil {
		t.Fatalf("write functional test: %v", err)
	}

	goModBytes, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	repoRoot := filepath.Dir(string(goModBytes))

	run := func(args ...string) {
		buf := &bytes.Buffer{}
		cmd := exec.Command("go", args...)
		cmd.Dir = modelsDir
		cmd.Stdout = buf
		cmd.Stderr = buf
		if err := cmd.Run(); err != nil {
			t.Fatalf("go %v failed: %v\n%s", args, err, buf.String())
		}
	}

	run("mod", "init", "github.com/aarondl/sqlboiler-casdemo")
	run("mod", "edit", "-replace=github.com/aarondl/sqlboiler/v4="+repoRoot)
	// Pin driver/mock versions to the ones validated by the root module,
	// otherwise tidy may select releases that need a newer toolchain.
	run("get",
		"modernc.org/sqlite@v1.18.1",
		"github.com/DATA-DOG/go-sqlmock@v1.4.1",
		"github.com/aarondl/null/v8@v8.1.3",
	)
	run("mod", "tidy")

	buf := &bytes.Buffer{}
	// -race is intentionally omitted: it requires cgo and a C toolchain,
	// which are not available on every supported runner. The concurrency
	// semantics themselves are exercised against a real SQLite database.
	cmd := exec.Command("go", "test", "-count=1", "-v", "./...")
	cmd.Dir = modelsDir
	cmd.Env = append(os.Environ(), "CAS_FIXTURE_DB="+dbPath)
	cmd.Stdout = buf
	cmd.Stderr = buf
	if err := cmd.Run(); err != nil {
		t.Fatalf("functional suite failed: %v\n%s", err, buf.String())
	}
	t.Log(buf.String())
}
