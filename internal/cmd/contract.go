package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/trace"
	"sort"

	"github.com/spf13/cobra"

	"github.com/sqlc-dev/sqlc/internal/config"
	"github.com/sqlc-dev/sqlc/internal/contract"
	"github.com/sqlc-dev/sqlc/internal/debug"
	"github.com/sqlc-dev/sqlc/internal/opts"
)

// ErrContractCheckFailed is returned when a contract check finds breaking
// changes, missing baselines or corrupted snapshots.
var ErrContractCheckFailed = errors.New("contract check failed")

// ErrContractDisabled is returned when the contract flow is not enabled in
// the configuration.
var ErrContractDisabled = errors.New("contract checking is not enabled")

// Action selects what the contract command does.
type Action int

const (
	// Check compares the current contracts against the stored baselines
	// without modifying them.
	Check Action = iota
	// Update replaces the stored baselines with the current contracts.
	Update
)

func NewCmdContract() *cobra.Command {
	check := &cobra.Command{
		Use:   "check",
		Short: "Check current queries against contract snapshots",
		RunE: func(cmd *cobra.Command, args []string) error {
			defer trace.StartRegion(cmd.Context(), "contract-check").End()
			return runContract(cmd, false)
		},
	}
	update := &cobra.Command{
		Use:   "update",
		Short: "Replace contract snapshots with the current query contracts",
		RunE: func(cmd *cobra.Command, args []string) error {
			defer trace.StartRegion(cmd.Context(), "contract-update").End()
			return runContract(cmd, true)
		},
	}
	c := &cobra.Command{
		Use:   "contract",
		Short: "Check or update the query contract snapshots",
		// Running `sqlc contract` without a subcommand performs a check.
		RunE: func(cmd *cobra.Command, args []string) error {
			defer trace.StartRegion(cmd.Context(), "contract-check").End()
			return runContract(cmd, false)
		},
	}
	c.AddCommand(check, update)
	return c
}

func runContract(cmd *cobra.Command, update bool) error {
	stderr := cmd.ErrOrStderr()
	dir, name := getConfigPath(stderr, cmd.Flag("file"))
	action := Check
	if update {
		action = Update
	}
	if err := Contract(cmd.Context(), dir, name, &Options{
		Env:    ParseEnv(cmd),
		Stderr: stderr,
	}, action); err != nil {
		if !errors.Is(err, ErrContractCheckFailed) && !errors.Is(err, ErrContractDisabled) {
			fmt.Fprintf(stderr, "%s\n", err)
		}
		os.Exit(1)
	}
	return nil
}

// Contract runs the opt-in contract flow. In Check mode it never modifies
// the snapshots and returns ErrContractCheckFailed when breaking changes,
// missing baselines or unreadable baselines are found. In Update mode it
// atomically replaces the snapshots after every package has been parsed
// successfully.
func Contract(ctx context.Context, dir, filename string, o *Options, action Action) error {
	stderr := o.Stderr

	configPath, conf, err := o.ReadConfig(dir, filename)
	if err != nil {
		return err
	}

	base := filepath.Base(configPath)
	if err := config.Validate(conf); err != nil {
		fmt.Fprintf(stderr, "error validating %s: %s\n", base, err)
		return err
	}
	if err := o.Env.Validate(conf); err != nil {
		fmt.Fprintf(stderr, "error validating %s: %s\n", base, err)
		return err
	}

	if !conf.Contract.Enabled {
		fmt.Fprintln(stderr, "error: contract checking is not enabled; add a top-level \"contract: { enabled: true }\" section to your sqlc configuration")
		return ErrContractDisabled
	}

	// Validate every package name before doing any work so that neither
	// checks nor updates run with an unstable on-disk package identity.
	for _, pkg := range conf.SQL {
		if err := contract.ValidatePackageName(pkg.Name); err != nil {
			fmt.Fprintf(stderr, "error: %s\n", err)
			return err
		}
	}

	// Parse every package and build its snapshot up front. Any parse or
	// contract generation failure aborts the run before the snapshot store
	// is touched, preserving the last valid state.
	current := make([]*contract.Snapshot, 0, len(conf.SQL))
	for _, pkg := range conf.SQL {
		sql := pkg
		schema := make([]string, 0, len(sql.Schema))
		for _, s := range sql.Schema {
			schema = append(schema, filepath.Join(dir, s))
		}
		sql.Schema = schema
		queries := make([]string, 0, len(sql.Queries))
		for _, q := range sql.Queries {
			queries = append(queries, filepath.Join(dir, q))
		}
		sql.Queries = queries

		combo := config.Combine(*conf, sql)
		parseOpts := opts.Parser{
			Debug: debug.Debug,
		}
		result, failed := parse(ctx, sql.Name, dir, sql, combo, parseOpts, stderr)
		if failed {
			return ErrFailedChecks
		}
		current = append(current, contract.Build(sql.Name, string(sql.Engine), result))
	}

	store := contract.NewStore(dir, conf.Contract)

	if action == Update {
		return updateContracts(stderr, store, current)
	}
	return checkContracts(ctx, stderr, store, current)
}

func checkContracts(ctx context.Context, stderr io.Writer, store *contract.Store, current []*contract.Snapshot) error {
	defer trace.StartRegion(ctx, "contract-diff").End()

	keep := make(map[string]struct{}, len(current))
	for _, snap := range current {
		keep[snap.Package] = struct{}{}
	}

	failed := false

	// Package order follows the configuration file and is deterministic.
	for _, snap := range current {
		baseline, err := store.Load(snap.Package)
		switch {
		case errors.Is(err, contract.ErrNotFound):
			failed = true
			fmt.Fprintf(stderr, "FAIL\t%s\n", snap.Package)
			fmt.Fprintf(stderr, "  no baseline snapshot; run \"sqlc contract update\" to create it\n")
			continue
		case err != nil:
			// Unreadable or corrupt baselines are reported but never
			// modified by a check.
			failed = true
			fmt.Fprintf(stderr, "FAIL\t%s\n", snap.Package)
			fmt.Fprintf(stderr, "  %s\n", err)
			continue
		}

		diff := contract.Diff(baseline, snap)
		if diff.Empty() {
			fmt.Fprintf(stderr, "ok\t%s\n", snap.Package)
			continue
		}

		printPackageDiff(stderr, diff)
		if diff.Breaking() {
			failed = true
			fmt.Fprintf(stderr, "FAIL\t%s\n", snap.Package)
		} else {
			fmt.Fprintf(stderr, "ok\t%s\n", snap.Package)
		}
	}

	// Packages that disappeared from the configuration are breaking.
	existing, err := store.List()
	if err != nil {
		fmt.Fprintf(stderr, "error: %s\n", err)
		return err
	}
	for _, name := range existing {
		if _, ok := keep[name]; ok {
			continue
		}
		failed = true
		fmt.Fprintf(stderr, "FAIL\t%s\n", name)
		fmt.Fprintf(stderr, "  package removed from configuration\n")
	}

	if failed {
		return ErrContractCheckFailed
	}
	return nil
}

func printPackageDiff(stderr io.Writer, diff *contract.PackageDiff) {
	fmt.Fprintf(stderr, "# package %s\n", diff.Package)
	if diff.EngineChanged {
		printDetail(stderr, diff.EngineDetail)
	}
	for _, name := range diff.Added {
		fmt.Fprintf(stderr, "  added query: %s\n", name)
	}
	for _, name := range diff.Removed {
		fmt.Fprintf(stderr, "  [breaking] removed query: %s\n", name)
	}
	for _, q := range diff.Changed {
		severity := "compatible"
		if q.Breaking {
			severity = "breaking"
		}
		fmt.Fprintf(stderr, "  changed query: %s [%s]\n", q.Name, severity)
		for _, d := range q.Details {
			printDetail(stderr, d)
		}
	}
}

func printDetail(stderr io.Writer, d contract.Detail) {
	if d.Breaking {
		fmt.Fprintf(stderr, "  [breaking] %s\n", d.Message)
	} else {
		fmt.Fprintf(stderr, "  [compatible] %s\n", d.Message)
	}
}

func updateContracts(stderr io.Writer, store *contract.Store, current []*contract.Snapshot) error {
	if err := store.SaveAll(current); err != nil {
		fmt.Fprintf(stderr, "error: %s\n", err)
		return err
	}
	keep := make(map[string]struct{}, len(current))
	names := make([]string, 0, len(current))
	for _, snap := range current {
		keep[snap.Package] = struct{}{}
		names = append(names, snap.Package)
	}
	pruned, err := store.Prune(keep)
	if err != nil {
		fmt.Fprintf(stderr, "error: %s\n", err)
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(stderr, "updated\t%s\n", name)
	}
	for _, name := range pruned {
		fmt.Fprintf(stderr, "pruned\t%s\n", name)
	}
	return nil
}
