package contract

import (
	"testing"

	"github.com/sqlc-dev/sqlc/internal/compiler"
)

func snapshotForQueries(queries ...*compiler.Query) *Snapshot {
	return Build("db", "postgresql", &compiler.Result{Queries: queries})
}

func TestDiffIdentical(t *testing.T) {
	s := snapshotForQueries(
		query("GetAuthor", ":one", []compiler.Parameter{
			param(1, col("id", "int8", true)),
		}, []*compiler.Column{
			col("id", "int8", true),
			col("name", "text", true),
		}),
	)
	diff := Diff(s, snapshotForQueries(
		query("GetAuthor", ":one", []compiler.Parameter{
			param(1, col("id", "int8", true)),
		}, []*compiler.Column{
			col("id", "int8", true),
			col("name", "text", true),
		}),
	))
	if !diff.Empty() {
		t.Fatalf("expected empty diff, got %#v", diff)
	}
	if diff.Breaking() {
		t.Fatal("identical snapshots must not be breaking")
	}
}

func TestDiffNewQueryIsCompatible(t *testing.T) {
	baseline := snapshotForQueries(
		query("GetAuthor", ":one", nil, []*compiler.Column{col("id", "int8", true)}),
	)
	current := snapshotForQueries(
		query("GetAuthor", ":one", nil, []*compiler.Column{col("id", "int8", true)}),
		query("CreateAuthor", ":exec", []compiler.Parameter{
			param(1, col("name", "text", true)),
		}, nil),
	)
	diff := Diff(baseline, current)
	if diff.Empty() || len(diff.Added) != 1 || diff.Added[0] != "CreateAuthor" {
		t.Fatalf("expected CreateAuthor added, got %#v", diff)
	}
	if diff.Breaking() {
		t.Fatal("adding a query must be compatible")
	}
}

func TestDiffRemovedQueryIsBreaking(t *testing.T) {
	baseline := snapshotForQueries(
		query("GetAuthor", ":one", nil, nil),
		query("DeleteAuthor", ":exec", []compiler.Parameter{
			param(1, col("id", "int8", true)),
		}, nil),
	)
	current := snapshotForQueries(
		query("GetAuthor", ":one", nil, nil),
	)
	diff := Diff(baseline, current)
	if len(diff.Removed) != 1 || diff.Removed[0] != "DeleteAuthor" {
		t.Fatalf("expected DeleteAuthor removed, got %#v", diff)
	}
	if !diff.Breaking() {
		t.Fatal("removing a query must be breaking")
	}
}

func TestDiffCmdChangeIsBreaking(t *testing.T) {
	baseline := snapshotForQueries(query("ListAuthors", ":many", nil, []*compiler.Column{col("id", "int8", true)}))
	current := snapshotForQueries(query("ListAuthors", ":one", nil, []*compiler.Column{col("id", "int8", true)}))
	diff := Diff(baseline, current)
	if len(diff.Changed) != 1 || !diff.Changed[0].Breaking {
		t.Fatalf("cmd change must be breaking, got %#v", diff)
	}
}

func TestDiffParamChanges(t *testing.T) {
	baseline := snapshotForQueries(query("GetAuthor", ":one", []compiler.Parameter{
		param(1, col("id", "int8", true)),
	}, nil))

	cases := []struct {
		name   string
		params []compiler.Parameter
	}{
		{"count", []compiler.Parameter{
			param(1, col("id", "int8", true)),
			param(2, col("name", "text", true)),
		}},
		{"type", []compiler.Parameter{
			param(1, col("id", "text", true)),
		}},
		{"name", []compiler.Parameter{
			param(1, col("author_id", "int8", true)),
		}},
		{"nullability", []compiler.Parameter{
			param(1, col("id", "int8", false)),
		}},
		{"array", []compiler.Parameter{
			{Number: 1, Column: &compiler.Column{Name: "id", DataType: "int8", NotNull: true, IsArray: true, ArrayDims: 1}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			current := snapshotForQueries(query("GetAuthor", ":one", tc.params, nil))
			diff := Diff(baseline, current)
			if len(diff.Changed) != 1 || !diff.Changed[0].Breaking {
				t.Fatalf("parameter %s change must be breaking, got %#v", tc.name, diff)
			}
		})
	}
}

func TestDiffParamNumberReorderIsBreaking(t *testing.T) {
	baseline := snapshotForQueries(query("Find", ":one", []compiler.Parameter{
		param(1, col("a", "text", true)),
		param(2, col("b", "text", true)),
	}, nil))
	// Sorted-by-number normalization means equal positions are compared; a
	// swap of the numbers assigned to names is detected.
	current := snapshotForQueries(query("Find", ":one", []compiler.Parameter{
		param(1, col("b", "text", true)),
		param(2, col("a", "text", true)),
	}, nil))
	diff := Diff(baseline, current)
	if !diff.Breaking() {
		t.Fatalf("swapping parameter names must be breaking, got %#v", diff)
	}
}

func TestDiffAddedResultColumnIsCompatible(t *testing.T) {
	baseline := snapshotForQueries(query("GetAuthor", ":one", nil, []*compiler.Column{
		col("id", "int8", true),
	}))
	current := snapshotForQueries(query("GetAuthor", ":one", nil, []*compiler.Column{
		col("id", "int8", true),
		col("name", "text", true),
	}))
	diff := Diff(baseline, current)
	if len(diff.Changed) != 1 {
		t.Fatalf("expected one changed query, got %#v", diff)
	}
	qd := diff.Changed[0]
	if qd.Breaking {
		t.Fatalf("adding a result column must be compatible, got %#v", qd)
	}
	if len(qd.Details) != 1 || qd.Details[0].Breaking {
		t.Fatalf("expected one compatible detail, got %#v", qd.Details)
	}
}

func TestDiffRemovedResultColumnIsBreaking(t *testing.T) {
	baseline := snapshotForQueries(query("GetAuthor", ":one", nil, []*compiler.Column{
		col("id", "int8", true),
		col("name", "text", true),
	}))
	current := snapshotForQueries(query("GetAuthor", ":one", nil, []*compiler.Column{
		col("id", "int8", true),
	}))
	diff := Diff(baseline, current)
	if !diff.Breaking() || len(diff.Changed) != 1 {
		t.Fatalf("removing a result column must be breaking, got %#v", diff)
	}
}

func TestDiffResultColumnTypeChangeIsBreaking(t *testing.T) {
	baseline := snapshotForQueries(query("GetAuthor", ":one", nil, []*compiler.Column{
		col("id", "int8", true),
	}))
	cases := []struct {
		name   string
		column *compiler.Column
	}{
		{"type", col("id", "text", true)},
		{"nullability", col("id", "int8", false)},
		{"unsigned", &compiler.Column{Name: "id", DataType: "int8", NotNull: true, Unsigned: true}},
		{"array", &compiler.Column{Name: "id", DataType: "int8", NotNull: true, IsArray: true, ArrayDims: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			current := snapshotForQueries(query("GetAuthor", ":one", nil, []*compiler.Column{tc.column}))
			diff := Diff(baseline, current)
			if !diff.Breaking() {
				t.Fatalf("result column %s change must be breaking, got %#v", tc.name, diff)
			}
		})
	}
}

func TestDiffEngineChangeIsBreaking(t *testing.T) {
	baseline := Build("db", "postgresql", &compiler.Result{})
	current := Build("db", "sqlite", &compiler.Result{})
	diff := Diff(baseline, current)
	if !diff.EngineChanged || !diff.Breaking() {
		t.Fatalf("engine change must be breaking, got %#v", diff)
	}
}
