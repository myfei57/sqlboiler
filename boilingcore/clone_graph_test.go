package boilingcore

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aarondl/sqlboiler/v4/drivers"
	"github.com/aarondl/sqlboiler/v4/importers"
)

// cloneGraphModule generates a models package from the mock driver into a
// temporary standalone Go module and returns its directory. The generated
// package can be compiled and exercised by fixture test files written into
// the same directory. extra mutates the config before generation.
func cloneGraphModule(t *testing.T, extra func(*Config)) string {
	t.Helper()

	if testing.Short() {
		t.SkipNow()
	}

	out, err := os.MkdirTemp("", "boil_clonegraph")
	if err != nil {
		t.Fatalf("unable to create tempdir: %s", err)
	}
	t.Cleanup(func() {
		if t.Failed() || os.Getenv("CLONEGRAPH_KEEP") == "1" {
			t.Logf("keeping module output at: %s", out)
			return
		}
		os.RemoveAll(out)
	})

	config := &Config{
		DriverName: "mock",
		PkgName:    "models",
		OutFolder:  out,
		NoTests:    true,
		DriverConfig: map[string]any{
			drivers.ConfigSchema:    "schema",
			drivers.ConfigBlacklist: []string{"hangars"},
		},
		Imports:    importers.NewDefaultImports(),
		CloneGraph: true,
	}
	if extra != nil {
		extra(config)
	}

	state, err := New(config)
	if err != nil {
		t.Fatalf("unable to create State using config: %s", err)
	}
	if err := state.Run(); err != nil {
		t.Fatalf("unable to execute State.Run: %s", err)
	}

	return out
}

// initCloneGraphModule turns out into a standalone module replacing the
// sqlboiler dependency with the repository currently under test, then runs
// go mod tidy so the generated package can be compiled. The module is built
// fully offline (GOPROXY=off) using the module cache and the root go.sum, so
// fixture runs never depend on network access.
func initCloneGraphModule(t *testing.T, out string) {
	t.Helper()

	buf := &bytes.Buffer{}

	goModFilePath, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatalf("go env GOMOD failed: %s", err)
	}
	rootGoSum := filepath.Join(filepath.Dir(string(goModFilePath)), "go.sum")

	run := func(name string, args ...string) {
		cmd := exec.Command(name, args...)
		cmd.Dir = out
		cmd.Env = append(os.Environ(), "GOPROXY=off")
		cmd.Stderr = buf
		if err := cmd.Run(); err != nil {
			t.Fatalf("%s %v failed: %s\n%s", name, args, err, buf.String())
		}
	}

	run("go", "mod", "init", "github.com/aarondl/sqlboiler-test")
	run("go", "mod", "edit", fmt.Sprintf("-replace=github.com/aarondl/sqlboiler/v4=%s", filepath.Dir(string(goModFilePath))))

	// Seed the hashes of every transitive dependency from the root module so
	// the offline tidy never has to reach the checksum database.
	if rootSum, err := os.ReadFile(rootGoSum); err == nil {
		if err := os.WriteFile(filepath.Join(out, "go.sum"), rootSum, 0o644); err != nil {
			t.Fatalf("seed go.sum: %s", err)
		}
	}
}

// writeModuleFile writes a file (fixture code or support package) inside the
// generated module.
func writeModuleFile(t *testing.T, out, rel, content string) {
	t.Helper()

	path := filepath.Join(out, rel)
	if dir := filepath.Dir(path); dir != out {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %s", dir, err)
		}
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %s", path, err)
	}
}

// runModuleCommand runs a go command inside the generated module.
func runModuleCommand(t *testing.T, out string, args ...string) {
	t.Helper()

	buf := &bytes.Buffer{}
	cmd := exec.Command("go", args...)
	cmd.Dir = out
	cmd.Env = append(os.Environ(), "GOPROXY=off")
	cmd.Stdout = buf
	cmd.Stderr = buf
	if err := cmd.Run(); err != nil {
		t.Fatalf("go %s failed: %s\n%s", strings.Join(args, " "), err, buf.String())
	}
}

func TestCloneGraphSmokeGenerate(t *testing.T) {
	out := cloneGraphModule(t, nil)
	initCloneGraphModule(t, out)
	runModuleCommand(t, out, "mod", "tidy")
	runModuleCommand(t, out, "build", "./...")
}

// TestCloneGraphRuntimeFixtures exercises the generated API against the mock
// schema: cycles, many-to-many, shared leaves, nil/empty states, byte
// independence, budgets and the no-executor guarantee.
func TestCloneGraphRuntimeFixtures(t *testing.T) {
	out := cloneGraphModule(t, nil)
	initCloneGraphModule(t, out)
	writeModuleFile(t, out, "clonegraph_fixture_test.go", cloneGraphBaseFixture)
	runModuleCommand(t, out, "mod", "tidy")
	runModuleCommand(t, out, "test", "-run", "TestCloneGraph", "./...")
}

// TestCloneGraphExoticTypesAndAdapter drives every non-trivial column
// strategy through type replacements (decimal, hstore, json, pgeo, arrays)
// plus a custom external type copied through a registered adapter.
func TestCloneGraphExoticTypesAndAdapter(t *testing.T) {
	out := cloneGraphModule(t, func(c *Config) {
		c.CloneGraphAdapters = []string{"customtype.Widget"}
		c.TypeReplaces = []TypeReplace{
			{
				Tables: []string{"jets"},
				Match:  drivers.Column{Name: "cargo"},
				Replace: drivers.Column{
					Type:   "types.Decimal",
					DBType: "numeric",
				},
				Imports: importers.Set{ThirdParty: importers.List{`"github.com/aarondl/sqlboiler/v4/types"`}},
			},
			{
				Tables: []string{"jets"},
				Match:  drivers.Column{Name: "manifest", Nullable: true},
				Replace: drivers.Column{
					Type:     "types.NullDecimal",
					DBType:   "numeric",
					Nullable: true,
				},
				Imports: importers.Set{ThirdParty: importers.List{`"github.com/aarondl/sqlboiler/v4/types"`}},
			},
			{
				Tables: []string{"jets"},
				Match:  drivers.Column{Name: "color", Nullable: true},
				Replace: drivers.Column{
					Type:     "types.HStore",
					DBType:   "hstore",
					Nullable: true,
				},
				Imports: importers.Set{ThirdParty: importers.List{`"github.com/aarondl/sqlboiler/v4/types"`}},
			},
			{
				Tables: []string{"jets"},
				Match:  drivers.Column{Name: "uuid", Nullable: true},
				Replace: drivers.Column{
					Type:     "types.JSON",
					DBType:   "jsonb",
					Nullable: true,
				},
				Imports: importers.Set{ThirdParty: importers.List{`"github.com/aarondl/sqlboiler/v4/types"`}},
			},
			{
				Tables: []string{"jets"},
				Match:  drivers.Column{Name: "identifier"},
				Replace: drivers.Column{
					Type:   "pgeo.Path",
					DBType: "path",
				},
				Imports: importers.Set{ThirdParty: importers.List{`"github.com/aarondl/sqlboiler/v4/types/pgeo"`}},
			},
			{
				Tables: []string{"airports"},
				Match:  drivers.Column{Name: "size", Nullable: true},
				Replace: drivers.Column{
					Type:     "types.BytesArray",
					DBType:   "bytea[]",
					Nullable: true,
				},
				Imports: importers.Set{ThirdParty: importers.List{`"github.com/aarondl/sqlboiler/v4/types"`}},
			},
			{
				Tables: []string{"languages"},
				Match:  drivers.Column{Name: "language"},
				Replace: drivers.Column{
					Type:   "types.DecimalArray",
					DBType: "numeric[]",
				},
				Imports: importers.Set{ThirdParty: importers.List{`"github.com/aarondl/sqlboiler/v4/types"`}},
			},
			{
				Tables: []string{"pilots"},
				Match:  drivers.Column{Name: "name"},
				Replace: drivers.Column{
					Type:   "customtype.Widget",
					DBType: "widget",
				},
				Imports: importers.Set{ThirdParty: importers.List{`"github.com/aarondl/sqlboiler-test/customtype"`}},
			},
		}
	})
	initCloneGraphModule(t, out)
	writeModuleFile(t, out, filepath.Join("customtype", "widget.go"), cloneGraphCustomType)
	writeModuleFile(t, out, "clonegraph_exotic_test.go", cloneGraphExoticFixture)
	runModuleCommand(t, out, "mod", "tidy")
	runModuleCommand(t, out, "test", "-run", "TestCloneGraph", "./...")
}

// TestCloneGraphRejectsUnknownType makes sure a type-replaced column without
// a known strategy and without a registered adapter aborts generation instead
// of producing unsafe shallow copies.
func TestCloneGraphRejectsUnknownType(t *testing.T) {
	out, err := os.MkdirTemp("", "boil_clonegraph_reject")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(out) })

	config := &Config{
		DriverName: "mock",
		PkgName:    "models",
		OutFolder:  out,
		NoTests:    true,
		DriverConfig: map[string]any{
			drivers.ConfigSchema:    "schema",
			drivers.ConfigBlacklist: []string{"hangars"},
		},
		Imports:    importers.NewDefaultImports(),
		CloneGraph: true,
		TypeReplaces: []TypeReplace{
			{
				Tables: []string{"pilots"},
				Match:  drivers.Column{Name: "name"},
				Replace: drivers.Column{
					Type:   "customtype.Secret",
					DBType: "secret",
				},
				Imports: importers.Set{ThirdParty: importers.List{`"github.com/aarondl/sqlboiler-test/customtype"`}},
			},
		},
	}

	state, err := New(config)
	if err != nil {
		t.Fatalf("State creation failed: %s", err)
	}
	if err := state.Run(); err == nil {
		t.Fatal("expected generation to be rejected for unknown type without adapter, got nil error")
	} else if !strings.Contains(err.Error(), "clone_graph") {
		t.Fatalf("expected a clone_graph rejection error, got: %s", err)
	}
}
