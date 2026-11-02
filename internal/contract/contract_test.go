package contract

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/sqlc-dev/sqlc/internal/compiler"
	"github.com/sqlc-dev/sqlc/internal/metadata"
	"github.com/sqlc-dev/sqlc/internal/sql/ast"
)

func col(name, dataType string, notNull bool) *compiler.Column {
	return &compiler.Column{Name: name, DataType: dataType, NotNull: notNull}
}

func typedCol(name, schema, typ string, notNull bool) *compiler.Column {
	return &compiler.Column{
		Name:    name,
		Type:    &ast.TypeName{Schema: schema, Name: typ},
		NotNull: notNull,
	}
}

func query(name, cmd string, params []compiler.Parameter, columns []*compiler.Column) *compiler.Query {
	return &compiler.Query{
		Metadata: metadata.Metadata{Name: name, Cmd: cmd},
		Params:   params,
		Columns:  columns,
	}
}

func param(number int, c *compiler.Column) compiler.Parameter {
	return compiler.Parameter{Number: number, Column: c}
}

func TestBuildNormalizesAndMarshalsStably(t *testing.T) {
	result := &compiler.Result{
		Queries: []*compiler.Query{
			query("Zebra", ":exec", []compiler.Parameter{
				param(2, col("second", "text", true)),
				param(1, col("first", "int8", true)),
			}, nil),
			query("Alpha", ":one", nil, []*compiler.Column{
				typedCol("id", "pg_catalog", "int8", true),
				col("bio", "text", false),
			}),
		},
	}

	snap := Build("db", "postgresql", result)

	if got := snap.Queries[0].Name; got != "Alpha" {
		t.Fatalf("queries not sorted by name, first = %q", got)
	}
	zebra := snap.Queries[1]
	if got := zebra.Params[0].Number; got != 1 {
		t.Fatalf("params not sorted by number, first = %d", got)
	}

	first, err := snap.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	second, err := snap.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(string(first), string(second)); diff != "" {
		t.Fatalf("marshal not deterministic (-first +second):\n%s", diff)
	}

	roundtrip, err := Unmarshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(snap, roundtrip); diff != "" {
		t.Fatalf("roundtrip mismatch (-want +got):\n%s", diff)
	}

	alpha := snap.Queries[0]
	if got := alpha.Columns[0].Type; got != "pg_catalog.int8" {
		t.Fatalf("qualified type = %q, want pg_catalog.int8", got)
	}
	if got := alpha.Columns[1].Type; got != "text" {
		t.Fatalf("data type = %q, want text", got)
	}
}

func TestUnmarshalRejectsUnknownFields(t *testing.T) {
	blob := []byte(`{
  "version": 1,
  "package": "db",
  "engine": "postgresql",
  "queries": [],
  "bogus": true
}`)
	if _, err := Unmarshal(blob); err == nil {
		t.Fatal("expected error for unknown field, got nil")
	}
}

func TestEmptySlicesSerializeAsArrays(t *testing.T) {
	snap := Build("db", "postgresql", &compiler.Result{})
	blob, err := snap.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "version": 1,
  "package": "db",
  "engine": "postgresql",
  "queries": []
}
`
	if diff := cmp.Diff(want, string(blob)); diff != "" {
		t.Fatalf("snapshot mismatch (-want +got):\n%s", diff)
	}
}
