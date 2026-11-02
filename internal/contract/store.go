package contract

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/sqlc-dev/sqlc/internal/config"
)

// DefaultDir is the default directory, relative to the configuration file,
// in which contract snapshots are stored.
const DefaultDir = ".sqlc" + string(filepath.Separator) + "contracts"

const snapshotSuffix = ".json"

// ErrNotFound is returned when no baseline snapshot exists for a package.
var ErrNotFound = errors.New("contract snapshot not found")

var validPackageName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidatePackageName reports whether name can safely be used to identify a
// package snapshot on disk.
func ValidatePackageName(name string) error {
	if name == "" {
		return errors.New("SQL package is missing a name; set \"name\" for every package to use contract checking")
	}
	if name == "." || name == ".." || !validPackageName.MatchString(name) {
		return fmt.Errorf("invalid SQL package name for contract snapshot: %q", name)
	}
	return nil
}

// Store persists per-package snapshots under a single directory.
type Store struct {
	dir string
}

// NewStore creates a store rooted at the project directory (the directory
// containing the sqlc configuration file).
func NewStore(root string, settings config.ContractSettings) *Store {
	dir := settings.Dir
	if dir == "" {
		dir = DefaultDir
	}
	return &Store{dir: filepath.Clean(filepath.Join(root, dir))}
}

// Dir returns the absolute snapshot directory.
func (s *Store) Dir() string {
	return s.dir
}

func (s *Store) path(pkg string) (string, error) {
	if err := ValidatePackageName(pkg); err != nil {
		return "", err
	}
	return filepath.Join(s.dir, pkg+snapshotSuffix), nil
}

// Load reads and parses the baseline snapshot for a package. ErrNotFound is
// returned when the baseline does not exist; a non-nil error is returned for
// read or parse failures without modifying the on-disk state.
func (s *Store) Load(pkg string) (*Snapshot, error) {
	path, err := s.path(pkg)
	if err != nil {
		return nil, err
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read contract snapshot %s: %w", path, err)
	}
	snap, err := Unmarshal(blob)
	if err != nil {
		return nil, fmt.Errorf("parse contract snapshot %s: %w", path, err)
	}
	if snap.Version != SnapshotVersion {
		return nil, fmt.Errorf("unsupported contract snapshot version in %s: got %d, want %d",
			path, snap.Version, SnapshotVersion)
	}
	return snap, nil
}

// List returns the names of packages that have a baseline snapshot, sorted
// alphabetically. A missing snapshot directory is not an error.
func (s *Store) List() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read contract snapshot directory %s: %w", s.dir, err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, snapshotSuffix) {
			continue
		}
		names = append(names, strings.TrimSuffix(name, snapshotSuffix))
	}
	sort.Strings(names)
	return names, nil
}

// SaveAll atomically replaces the snapshots for the given packages.
//
// All snapshots are serialized before any file is touched, and every file is
// first written to a temporary file in the same directory and fsynced before
// being renamed into place. If serialization, temp-file creation, writing or
// syncing fails for any snapshot, the temporary files are removed and every
// previously existing baseline is left untouched. Re-running SaveAll after
// an interruption between renames converges to the same on-disk state, since
// snapshot content is deterministic for identical inputs.
func (s *Store) SaveAll(snapshots []*Snapshot) error {
	type pending struct {
		path string
		blob []byte
	}

	pendingFiles := make([]pending, 0, len(snapshots))
	for _, snap := range snapshots {
		if err := ValidatePackageName(snap.Package); err != nil {
			return err
		}
		blob, err := snap.Marshal()
		if err != nil {
			return fmt.Errorf("marshal contract snapshot for %q: %w", snap.Package, err)
		}
		pendingFiles = append(pendingFiles, pending{
			path: filepath.Join(s.dir, snap.Package+snapshotSuffix),
			blob: blob,
		})
	}

	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("create contract snapshot directory %s: %w", s.dir, err)
	}

	var temps []string
	cleanup := func() {
		for _, tmp := range temps {
			_ = os.Remove(tmp)
		}
	}

	for _, p := range pendingFiles {
		tmp, err := os.CreateTemp(s.dir, ".contract-*.tmp")
		if err != nil {
			cleanup()
			return fmt.Errorf("create temporary contract snapshot: %w", err)
		}
		tmpName := tmp.Name()
		temps = append(temps, tmpName)
		if _, err := tmp.Write(p.blob); err != nil {
			_ = tmp.Close()
			cleanup()
			return fmt.Errorf("write temporary contract snapshot: %w", err)
		}
		if err := tmp.Sync(); err != nil {
			_ = tmp.Close()
			cleanup()
			return fmt.Errorf("sync temporary contract snapshot: %w", err)
		}
		if err := tmp.Close(); err != nil {
			cleanup()
			return fmt.Errorf("close temporary contract snapshot: %w", err)
		}
	}

	for i, p := range pendingFiles {
		if err := os.Rename(temps[i], p.path); err != nil {
			// Remove the temp files that have not been renamed yet.
			// Already renamed snapshots contain identical content to
			// what a rerun produces, so the on-disk state converges.
			for j := i; j < len(temps); j++ {
				_ = os.Remove(temps[j])
			}
			return fmt.Errorf("replace contract snapshot %s: %w", p.path, err)
		}
	}

	// Best effort: persist the directory entry changes.
	if dir, err := os.Open(s.dir); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

// Prune removes baseline snapshots for packages that are no longer present in
// keep. It returns the names of the removed packages. A missing snapshot
// directory is not an error.
func (s *Store) Prune(keep map[string]struct{}) ([]string, error) {
	existing, err := s.List()
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, name := range existing {
		if _, ok := keep[name]; ok {
			continue
		}
		path, err := s.path(name)
		if err != nil {
			return removed, err
		}
		if err := os.Remove(path); err != nil {
			return removed, fmt.Errorf("remove stale contract snapshot %s: %w", path, err)
		}
		removed = append(removed, name)
	}
	return removed, nil
}
