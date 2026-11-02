package contract

import (
	"fmt"
	"sort"
)

// Detail describes one specific difference. Breaking differences change
// the generated query-layer API; non-breaking differences are additive.
type Detail struct {
	// Breaking reports whether the difference breaks existing callers.
	Breaking bool
	// Message is a human-readable description of the difference.
	Message string
}

// QueryDiff describes the difference between two versions of one named
// query.
type QueryDiff struct {
	// Name is the query name.
	Name string
	// Breaking reports whether any detail is breaking.
	Breaking bool
	// Details describes every difference found.
	Details []Detail
}

// PackageDiff describes the difference between two snapshots of one SQL
// package.
type PackageDiff struct {
	// Package is the SQL package name.
	Package string

	// EngineChanged reports that the SQL engine changed. An engine change
	// is always breaking.
	EngineChanged bool
	// EngineDetail describes the engine change.
	EngineDetail Detail

	// Added lists query names present in the current snapshot but absent
	// from the baseline. Adding a query is backwards compatible.
	Added []string
	// Removed lists query names present in the baseline but absent from
	// the current snapshot. Removing a query is breaking.
	Removed []string
	// Changed contains the differences for queries present in both.
	Changed []QueryDiff
}

// Empty reports whether the snapshots are identical.
func (d *PackageDiff) Empty() bool {
	return !d.EngineChanged && len(d.Added) == 0 && len(d.Removed) == 0 && len(d.Changed) == 0
}

// Breaking reports whether the difference contains breaking changes.
func (d *PackageDiff) Breaking() bool {
	if d.EngineChanged {
		return true
	}
	if len(d.Removed) > 0 {
		return true
	}
	for _, c := range d.Changed {
		if c.Breaking {
			return true
		}
	}
	return false
}

// Diff compares a baseline snapshot with the current snapshot. Both
// snapshots must describe the same package. Queries are matched by name,
// parameters by position and result columns by name.
func Diff(baseline, current *Snapshot) *PackageDiff {
	d := &PackageDiff{Package: current.Package}

	if baseline.Engine != current.Engine {
		d.EngineChanged = true
		d.EngineDetail = Detail{
			Breaking: true,
			Message:  fmt.Sprintf("engine changed: %s -> %s", baseline.Engine, current.Engine),
		}
	}

	base := make(map[string]QueryContract, len(baseline.Queries))
	for _, q := range baseline.Queries {
		base[q.Name] = q
	}
	cur := make(map[string]QueryContract, len(current.Queries))
	for _, q := range current.Queries {
		cur[q.Name] = q
	}

	for name := range cur {
		if _, ok := base[name]; !ok {
			d.Added = append(d.Added, name)
		}
	}
	for name := range base {
		if _, ok := cur[name]; !ok {
			d.Removed = append(d.Removed, name)
		}
	}
	sort.Strings(d.Added)
	sort.Strings(d.Removed)

	names := make([]string, 0, len(cur))
	for name := range cur {
		if _, ok := base[name]; ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if qd := diffQuery(base[name], cur[name]); len(qd.Details) > 0 {
			d.Changed = append(d.Changed, qd)
		}
	}
	return d
}

func diffQuery(baseline, current QueryContract) QueryDiff {
	qd := QueryDiff{Name: current.Name}

	if baseline.Cmd != current.Cmd {
		qd.Details = append(qd.Details, Detail{
			Breaking: true,
			Message:  fmt.Sprintf("query command changed: %s -> %s", baseline.Cmd, current.Cmd),
		})
	}

	qd.Details = append(qd.Details, diffParams(baseline.Params, current.Params)...)
	qd.Details = append(qd.Details, diffColumns(baseline.Columns, current.Columns)...)

	for _, detail := range qd.Details {
		if detail.Breaking {
			qd.Breaking = true
			break
		}
	}
	return qd
}

func diffParams(baseline, current []ParamContract) []Detail {
	if len(baseline) != len(current) {
		return []Detail{{
			Breaking: true,
			Message:  fmt.Sprintf("parameter count changed: %d -> %d", len(baseline), len(current)),
		}}
	}
	var out []Detail
	for i := range baseline {
		b, c := baseline[i], current[i]
		label := fmt.Sprintf("parameter %d", b.Number)
		if b.Field.Name != "" {
			label = fmt.Sprintf("parameter %d (%s)", b.Number, b.Field.Name)
		}
		if b.Number != c.Number {
			out = append(out, Detail{true, fmt.Sprintf("%s number changed: %d -> %d", label, b.Number, c.Number)})
			continue
		}
		if b.Field.Name != c.Field.Name {
			out = append(out, Detail{true, fmt.Sprintf("%s renamed: %q -> %q", label, b.Field.Name, c.Field.Name)})
		}
		out = append(out, diffField(label, b.Field, c.Field)...)
	}
	return out
}

func diffColumns(baseline, current []FieldContract) []Detail {
	base := make(map[string]FieldContract, len(baseline))
	for _, f := range baseline {
		base[f.Name] = f
	}
	cur := make(map[string]FieldContract, len(current))
	for _, f := range current {
		cur[f.Name] = f
	}

	var removed, added, common []string
	for name := range base {
		if _, ok := cur[name]; !ok {
			removed = append(removed, name)
		} else {
			common = append(common, name)
		}
	}
	for name := range cur {
		if _, ok := base[name]; !ok {
			added = append(added, name)
		}
	}
	sort.Strings(removed)
	sort.Strings(added)
	sort.Strings(common)

	var out []Detail
	for _, name := range removed {
		out = append(out, Detail{true, fmt.Sprintf("result column removed: %q", name)})
	}
	for _, name := range added {
		out = append(out, Detail{false, fmt.Sprintf("result column added: %q", name)})
	}
	for _, name := range common {
		out = append(out, diffField(fmt.Sprintf("result column %q", name), base[name], cur[name])...)
	}
	return out
}

// diffField compares the type identity of two fields. Every difference is
// breaking: generated field types or shapes change for callers.
func diffField(label string, baseline, current FieldContract) []Detail {
	var out []Detail
	if baseline.Type != current.Type {
		out = append(out, Detail{true, fmt.Sprintf("%s type changed: %s -> %s", label, baseline.Type, current.Type)})
	}
	if baseline.NotNull != current.NotNull {
		out = append(out, Detail{true, fmt.Sprintf("%s nullability changed: not_null=%t -> %t", label, baseline.NotNull, current.NotNull)})
	}
	if baseline.Unsigned != current.Unsigned {
		out = append(out, Detail{true, fmt.Sprintf("%s unsigned changed: %t -> %t", label, baseline.Unsigned, current.Unsigned)})
	}
	if baseline.IsArray != current.IsArray || baseline.ArrayDims != current.ArrayDims {
		out = append(out, Detail{true, fmt.Sprintf("%s array shape changed: array=%t(dims=%d) -> array=%t(dims=%d)",
			label, baseline.IsArray, baseline.ArrayDims, current.IsArray, current.ArrayDims)})
	}
	return out
}
