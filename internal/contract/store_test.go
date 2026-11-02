package contract

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sqlc-dev/sqlc/internal/compiler"
	"github.com/sqlc-dev/sqlc/internal/config"
)

func newTestStore(t *testing.T, dir string) *Store {
	t.Helper()
	return NewStore(t.TempDir(), config.ContractSettings{Enabled: true, Dir: dir})
}

func TestValidatePackageName(t *testing.T) {
	cases := []struct {
		name    string
		wantErr bool
	}{
		{"", true},
		{"..", true},
		{".", true},
		{"a/b", true},
		{"a..b", false},
		{"db-1_v2", false},
		{"-leading", true},
		{"ok.Name", false},
	}
	for _, tc := range cases {
		err := ValidatePackageName(tc.name)
		if (err != nil) != tc.wantErr {
			t.Errorf("ValidatePackageName(%q) err = %v, wantErr = %t", tc.name, err, tc.wantErr)
		}
	}
}

func TestStoreSaveLoadRoundtrip(t *testing.T) {
	store := newTestStore(t, "contracts")
	want := snapshotForQueries(query("GetAuthor", ":one", nil, []*compiler.Column{
		col("id", "int8", true),
	}))

	if err := store.SaveAll([]*Snapshot{want}); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load("db")
	if err != nil {
		t.Fatal(err)
	}
	if got.Package != "db" || got.Version != SnapshotVersion {
		t.Fatalf("unexpected snapshot header: %#v", got)
	}
	if len(got.Queries) != 1 || got.Queries[0].Name != "GetAuthor" {
		t.Fatalf("unexpected snapshot contents: %#v", got)
	}
}

func TestStoreLoadMissing(t *testing.T) {
	store := newTestStore(t, "contracts")
	if _, err := store.Load("db"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestStoreSaveIsIdempotent(t *testing.T) {
	store := newTestStore(t, "contracts")
	snap := snapshotForQueries(query("GetAuthor", ":one", nil, nil))

	if err := store.SaveAll([]*Snapshot{snap}); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(filepath.Join(store.Dir(), "db.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAll([]*Snapshot{snap}); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(filepath.Join(store.Dir(), "db.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("repeated updates produced different snapshot bytes")
	}

	entries, err := os.ReadDir(store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}
}

func TestStoreSaveFailurePreservesBaseline(t *testing.T) {
	store := newTestStore(t, "contracts")
	baseline := snapshotForQueries(query("V1", ":one", nil, nil))
	if err := store.SaveAll([]*Snapshot{baseline}); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(filepath.Join(store.Dir(), "db.json"))
	if err != nil {
		t.Fatal(err)
	}

	// An invalid package name must abort before touching the filesystem.
	next := snapshotForQueries(query("V2", ":one", nil, nil))
	next.Package = "bad/name"
	if err := store.SaveAll([]*Snapshot{next}); err == nil {
		t.Fatal("expected error for invalid package name")
	}
	preserved, err := os.ReadFile(filepath.Join(store.Dir(), "db.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(original) != string(preserved) {
		t.Fatal("existing baseline was modified despite the failed update")
	}
}

func TestStoreSaveUnwritableDirectoryPreservesBaseline(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions are bypassed when running as root")
	}
	store := newTestStore(t, "contracts")
	baseline := Build("db", "postgresql", &compiler.Result{
		Queries: []*compiler.Query{query("V1", ":one", nil, nil)},
	})
	if err := store.SaveAll([]*Snapshot{baseline}); err != nil {
		t.Fatal(err)
	}
	snapshotPath := filepath.Join(store.Dir(), "db.json")
	original, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}

	// Make the snapshot directory read-only so temp-file creation fails.
	if err := os.Chmod(store.Dir(), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(store.Dir(), 0o755) })

	next := Build("db", "postgresql", &compiler.Result{
		Queries: []*compiler.Query{query("V2", ":one", nil, nil)},
	})
	if err := store.SaveAll([]*Snapshot{next}); err == nil {
		t.Fatal("expected write failure")
	}
	if err := os.Chmod(store.Dir(), 0o755); err != nil {
		t.Fatal(err)
	}

	preserved, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(original) != string(preserved) {
		t.Fatal("existing baseline was modified despite the failed update")
	}

	// A rerun after the failure converges and installs the new snapshot.
	if err := store.SaveAll([]*Snapshot{next}); err != nil {
		t.Fatalf("rerun after failed update must succeed: %v", err)
	}
	reloaded, err := store.Load("db")
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Queries) != 1 || reloaded.Queries[0].Name != "V2" {
		t.Fatalf("unexpected snapshot after rerun: %#v", reloaded)
	}
}

func TestStoreCorruptBaselineIsNotModified(t *testing.T) {
	store := newTestStore(t, "contracts")
	if err := os.MkdirAll(store.Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Dir(), "db.json")
	corrupt := []byte("{not json")
	if err := os.WriteFile(path, corrupt, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load("db"); err == nil {
		t.Fatal("expected parse error for corrupt snapshot")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(corrupt) {
		t.Fatal("check/load must not modify corrupt snapshots")
	}
}

func TestStoreRejectsUnsupportedVersion(t *testing.T) {
	store := newTestStore(t, "contracts")
	if err := os.MkdirAll(store.Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Dir(), "db.json")
	blob := []byte(`{"version":999,"package":"db","engine":"postgresql","queries":[]}`)
	if err := os.WriteFile(path, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load("db"); err == nil {
		t.Fatal("expected unsupported version error")
	}
}

func TestStorePruneAndList(t *testing.T) {
	store := newTestStore(t, "contracts")
	a := Build("a", "postgresql", &compiler.Result{})
	b := Build("b", "postgresql", &compiler.Result{})
	c := Build("c", "postgresql", &compiler.Result{})
	if err := store.SaveAll([]*Snapshot{a, b, c}); err != nil {
		t.Fatal(err)
	}

	names, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 3 || names[0] != "a" || names[2] != "c" {
		t.Fatalf("unexpected list: %#v", names)
	}

	removed, err := store.Prune(map[string]struct{}{"a": {}, "b": {}})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "c" {
		t.Fatalf("expected c pruned, got %#v", removed)
	}
	if _, err := store.Load("c"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected c to be gone, got %v", err)
	}
}

func TestStoreRerunAfterInterruptedRenameConverges(t *testing.T) {
	store := newTestStore(t, "contracts")
	first := Build("db", "postgresql", &compiler.Result{
		Queries: []*compiler.Query{query("V1", ":one", nil, nil)},
	})
	second := Build("db", "postgresql", &compiler.Result{
		Queries: []*compiler.Query{query("V2", ":many", nil, nil)},
	})

	if err := store.SaveAll([]*Snapshot{first}); err != nil {
		t.Fatal(err)
	}
	// Interruption is modeled by rerunning the update with the new,
	// deterministic content. Any partial state from a previous run is
	// replaced by identical target bytes.
	if err := store.SaveAll([]*Snapshot{second}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAll([]*Snapshot{second}); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load("db")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Queries) != 1 || got.Queries[0].Name != "V2" || got.Queries[0].Cmd != ":many" {
		t.Fatalf("snapshot did not converge: %#v", got)
	}
}

func TestStoreDefaultDirectory(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root, config.ContractSettings{})
	want := filepath.Join(root, DefaultDir)
	if store.Dir() != filepath.Clean(want) {
		t.Fatalf("default dir = %q, want %q", store.Dir(), want)
	}
}
