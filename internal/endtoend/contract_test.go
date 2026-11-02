package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sqlc-dev/sqlc/internal/cmd"
)

const contractSchema = `CREATE TABLE authors (
  id   INTEGER PRIMARY KEY,
  name TEXT NOT NULL,
  bio  TEXT
);
`

const contractQueriesV1 = `-- name: GetAuthor :one
SELECT id, name, bio FROM authors WHERE id = ?;

-- name: CreateAuthor :exec
INSERT INTO authors (name, bio) VALUES (?, ?);

-- name: ListAuthors :many
SELECT id, name, bio FROM authors ORDER BY name;
`

const contractQueriesWithDelete = contractQueriesV1 + `
-- name: DeleteAuthor :exec
DELETE FROM authors WHERE id = ?;
`

// contractQueriesMissingList removes ListAuthors, a breaking change.
const contractQueriesMissingList = `-- name: GetAuthor :one
SELECT id, name, bio FROM authors WHERE id = ?;

-- name: CreateAuthor :exec
INSERT INTO authors (name, bio) VALUES (?, ?);
`

const contractQueriesInvalid = `-- name: Broken :one
SELECT FROM;
`

func writeContractProject(t *testing.T, dir, configYaml, queries string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "sqlc.yaml"), []byte(configYaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "schema.sql"), []byte(contractSchema), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "queries.sql"), []byte(queries), 0o644); err != nil {
		t.Fatal(err)
	}
}

func contractConfig(enabled bool, name, genOut string) string {
	var b strings.Builder
	b.WriteString("version: \"2\"\n")
	if enabled {
		b.WriteString("contract:\n  enabled: true\n")
	}
	b.WriteString("sql:\n")
	b.WriteString("  - name: " + name + "\n")
	b.WriteString("    engine: sqlite\n")
	b.WriteString("    schema: schema.sql\n")
	b.WriteString("    queries: queries.sql\n")
	if genOut != "" {
		b.WriteString("    gen:\n      go:\n        package: db\n        out: " + genOut + "\n")
	}
	return b.String()
}

func runContract(t *testing.T, dir string, action cmd.Action) (string, error) {
	t.Helper()
	var stderr bytes.Buffer
	err := cmd.Contract(context.Background(), dir, "", &cmd.Options{
		Env:    cmd.Env{},
		Stderr: &stderr,
	}, action)
	return stderr.String(), err
}

func snapshotPath(dir, name string) string {
	return filepath.Join(dir, ".sqlc", "contracts", name+".json")
}

func TestContractFlow(t *testing.T) {
	dir := t.TempDir()
	writeContractProject(t, dir, contractConfig(true, "db", ""), contractQueriesV1)

	// A check without any baseline fails and tells the user to update.
	out, err := runContract(t, dir, cmd.Check)
	if !errors.Is(err, cmd.ErrContractCheckFailed) {
		t.Fatalf("check without baseline: expected ErrContractCheckFailed, got %v (%s)", err, out)
	}
	if !strings.Contains(out, "no baseline snapshot") {
		t.Fatalf("missing baseline message: %s", out)
	}

	// Update creates the baseline.
	out, err = runContract(t, dir, cmd.Update)
	if err != nil {
		t.Fatalf("update failed: %v: %s", err, out)
	}
	baseline, err := os.ReadFile(snapshotPath(dir, "db"))
	if err != nil {
		t.Fatalf("snapshot not created: %v", err)
	}

	// Check now passes.
	out, err = runContract(t, dir, cmd.Check)
	if err != nil {
		t.Fatalf("check after update failed: %v: %s", err, out)
	}
	if !strings.Contains(out, "ok\tdb") {
		t.Fatalf("expected ok line, got: %s", out)
	}

	// Updates are idempotent: rerunning produces identical bytes.
	if _, err := runContract(t, dir, cmd.Update); err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(snapshotPath(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(baseline) {
		t.Fatal("repeated update changed snapshot bytes")
	}

	// Adding a query is a compatible change: check passes and reports it.
	writeContractProject(t, dir, contractConfig(true, "db", ""), contractQueriesWithDelete)
	out, err = runContract(t, dir, cmd.Check)
	if err != nil {
		t.Fatalf("compatible add must pass, got %v: %s", err, out)
	}
	if !strings.Contains(out, "added query: DeleteAuthor") {
		t.Fatalf("expected added query report, got: %s", out)
	}

	// Removing a query is breaking: check fails and keeps the baseline.
	writeContractProject(t, dir, contractConfig(true, "db", ""), contractQueriesMissingList)
	out, err = runContract(t, dir, cmd.Check)
	if !errors.Is(err, cmd.ErrContractCheckFailed) {
		t.Fatalf("removal expected to fail, got %v: %s", err, out)
	}
	if !strings.Contains(out, "[breaking] removed query: ListAuthors") {
		t.Fatalf("expected breaking removal report, got: %s", out)
	}
	preserved, err := os.ReadFile(snapshotPath(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	if string(preserved) != string(baseline) {
		t.Fatal("breaking check must not modify the baseline")
	}

	// Parse failures abort the update before any snapshot is touched.
	writeContractProject(t, dir, contractConfig(true, "db", ""), contractQueriesInvalid)
	out, err = runContract(t, dir, cmd.Update)
	if err == nil {
		t.Fatalf("invalid SQL must fail update, got: %s", out)
	}
	preserved, err = os.ReadFile(snapshotPath(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	if string(preserved) != string(baseline) {
		t.Fatal("failed parse must not modify the baseline")
	}

	// Restoring valid SQL and updating replaces the baseline; check passes.
	writeContractProject(t, dir, contractConfig(true, "db", ""), contractQueriesMissingList)
	if _, err := runContract(t, dir, cmd.Update); err != nil {
		t.Fatalf("update after restore failed: %v", err)
	}
	if _, err := runContract(t, dir, cmd.Check); err != nil {
		t.Fatalf("check after update failed: %v", err)
	}

	// A corrupt baseline is reported by check and never modified.
	corrupt := []byte("{broken")
	if err := os.WriteFile(snapshotPath(dir, "db"), corrupt, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = runContract(t, dir, cmd.Check)
	if !errors.Is(err, cmd.ErrContractCheckFailed) {
		t.Fatalf("corrupt baseline expected to fail, got %v: %s", err, out)
	}
	if !strings.Contains(out, "parse contract snapshot") {
		t.Fatalf("expected corrupt snapshot message, got: %s", out)
	}
	after, err := os.ReadFile(snapshotPath(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(corrupt) {
		t.Fatal("corrupt baseline must be left untouched by check")
	}

	// Renaming the package makes the old baseline stale: check flags it,
	// update prunes it.
	writeContractProject(t, dir, contractConfig(true, "db2", ""), contractQueriesMissingList)
	out, err = runContract(t, dir, cmd.Check)
	if !errors.Is(err, cmd.ErrContractCheckFailed) {
		t.Fatalf("stale package expected to fail check, got %v: %s", err, out)
	}
	if !strings.Contains(out, "package removed from configuration") {
		t.Fatalf("expected removed package report, got: %s", out)
	}
	out, err = runContract(t, dir, cmd.Update)
	if err != nil {
		t.Fatalf("update with renamed package failed: %v: %s", err, out)
	}
	if !strings.Contains(out, "pruned\tdb") {
		t.Fatalf("expected prune report, got: %s", out)
	}
	if _, err := os.Stat(snapshotPath(dir, "db")); !os.IsNotExist(err) {
		t.Fatal("stale snapshot was not pruned")
	}
	if _, err := os.Stat(snapshotPath(dir, "db2")); err != nil {
		t.Fatalf("new snapshot missing: %v", err)
	}
	if _, err := runContract(t, dir, cmd.Check); err != nil {
		t.Fatalf("check after prune failed: %v", err)
	}
}

func TestContractMultiplePackages(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []struct{ name, body string }{
		{"schema_a.sql", contractSchema},
		{"schema_b.sql", contractSchema},
		{"queries_a.sql", contractQueriesV1},
		{"queries_b.sql", contractQueriesV1},
	} {
		if err := os.WriteFile(filepath.Join(dir, f.name), []byte(f.body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	configYaml := `version: "2"
contract:
  enabled: true
sql:
  - name: a
    engine: sqlite
    schema: schema_a.sql
    queries: queries_a.sql
  - name: b
    engine: sqlite
    schema: schema_b.sql
    queries: queries_b.sql
`
	if err := os.WriteFile(filepath.Join(dir, "sqlc.yaml"), []byte(configYaml), 0o644); err != nil {
		t.Fatal(err)
	}

	if out, err := runContract(t, dir, cmd.Update); err != nil {
		t.Fatalf("update failed: %v: %s", err, out)
	}
	for _, name := range []string{"a", "b"} {
		if _, err := os.Stat(snapshotPath(dir, name)); err != nil {
			t.Fatalf("snapshot for package %s missing: %v", name, err)
		}
	}
	out, err := runContract(t, dir, cmd.Check)
	if err != nil {
		t.Fatalf("check failed: %v: %s", err, out)
	}
	if !strings.Contains(out, "ok\ta") || !strings.Contains(out, "ok\tb") {
		t.Fatalf("expected both packages ok, got: %s", out)
	}

	// Breaking change in package a only: a fails, b stays ok.
	if err := os.WriteFile(filepath.Join(dir, "queries_a.sql"), []byte(contractQueriesMissingList), 0o644); err != nil {
		t.Fatal(err)
	}
	goodB, err := os.ReadFile(snapshotPath(dir, "b"))
	if err != nil {
		t.Fatal(err)
	}
	out, err = runContract(t, dir, cmd.Check)
	if !errors.Is(err, cmd.ErrContractCheckFailed) {
		t.Fatalf("expected breaking failure, got %v: %s", err, out)
	}
	if !strings.Contains(out, "FAIL\ta") || !strings.Contains(out, "ok\tb") {
		t.Fatalf("expected FAIL for a and ok for b, got: %s", out)
	}

	// A parse failure in package b aborts the whole update: neither
	// baseline is replaced.
	goodA, err := os.ReadFile(snapshotPath(dir, "a"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "queries_b.sql"), []byte(contractQueriesInvalid), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runContract(t, dir, cmd.Update); err == nil {
		t.Fatal("expected update to fail when a package cannot be parsed")
	}
	afterA, err := os.ReadFile(snapshotPath(dir, "a"))
	if err != nil {
		t.Fatal(err)
	}
	afterB, err := os.ReadFile(snapshotPath(dir, "b"))
	if err != nil {
		t.Fatal(err)
	}
	if string(afterA) != string(goodA) || string(afterB) != string(goodB) {
		t.Fatal("a failed multi-package update must preserve every baseline")
	}
}

func TestContractDisabledRequiresOptIn(t *testing.T) {
	dir := t.TempDir()
	writeContractProject(t, dir, contractConfig(false, "db", ""), contractQueriesV1)

	out, err := runContract(t, dir, cmd.Check)
	if !errors.Is(err, cmd.ErrContractDisabled) {
		t.Fatalf("expected ErrContractDisabled, got %v: %s", err, out)
	}
}

func TestContractRequiresNamedPackage(t *testing.T) {
	dir := t.TempDir()
	writeContractProject(t, dir, contractConfig(true, "", ""), contractQueriesV1)

	out, err := runContract(t, dir, cmd.Update)
	if err == nil || errors.Is(err, cmd.ErrContractDisabled) {
		t.Fatalf("expected configuration error for unnamed package, got %v: %s", err, out)
	}
	if !strings.Contains(out, "missing a name") {
		t.Fatalf("expected missing name message, got: %s", out)
	}
}

func TestContractDoesNotAffectGenerate(t *testing.T) {
	dir := t.TempDir()
	writeContractProject(t, dir, contractConfig(true, "db", "gen"), contractQueriesV1)

	var stderr bytes.Buffer
	output, err := cmd.Generate(context.Background(), dir, "", &cmd.Options{
		Env:    cmd.Env{},
		Stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("generate failed with contract enabled: %v: %s", err, stderr.String())
	}
	if len(output) == 0 {
		t.Fatal("expected generated Go files")
	}
	for filename := range output {
		if strings.Contains(filepath.ToSlash(filename), ".sqlc/contracts") {
			t.Fatalf("generate must not write contract snapshots: %s", filename)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".sqlc")); !os.IsNotExist(err) {
		t.Fatal("generate must not create the contract snapshot directory")
	}

	// vet and verify wiring remain untouched structurally: contract is a
	// separate command, so running it on an unrelated config keeps working.
	out, cerr := runContract(t, dir, cmd.Check)
	if !errors.Is(cerr, cmd.ErrContractCheckFailed) {
		t.Fatalf("fresh project check should miss baseline, got %v: %s", cerr, out)
	}
}
