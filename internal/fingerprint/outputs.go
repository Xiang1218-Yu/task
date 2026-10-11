package fingerprint

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/go-task/task/v3/internal/logger"
	"github.com/go-task/task/v3/taskfile/ast"
)

const (
	outputsManifestVersion = 1
	// outputsDir is the subdirectory of the fingerprint temp directory that
	// holds one manifest per tracked task.
	outputsDir = "outputs"
)

// ErrCorruptOutputsManifest is returned when a manifest cannot be parsed or
// fails validation. Callers must treat it as "nothing trustworthy to prune
// against" and never delete output files in that case.
var ErrCorruptOutputsManifest = errors.New("corrupt outputs manifest")

// outputsManifest records the files a task produced during its latest
// successful run. It is the only source of truth for cleanup: a file that is
// not listed here can never be removed by the tracker.
type outputsManifest struct {
	Version int      `json:"version"`
	Files   []string `json:"files"`
}

// OutputsRun is the state captured around a single task run.
type OutputsRun struct {
	// before maps each output present before the run to its modification time.
	before map[string]time.Time
}

// OutputsTracker remembers the files a task actually produces on each
// successful run and removes files recorded by a previous successful run that
// the current run no longer produces.
//
// It is deliberately conservative:
//   - Tracking only applies to tasks that explicitly opt in (clean: true) and
//     declare at least one "generates" pattern, which scopes all tracked files.
//   - A file is ever deleted when it is listed by a previous, valid manifest
//     and still matches the task's current generates patterns. Files that
//     predate tracking, files outside the generates scope and files from other
//     tasks are therefore untouchable.
//   - Nothing is deleted and no manifest is written when the run fails, is
//     interrupted, or in dry mode. A missing, unreadable or corrupt manifest
//     disables cleanup for the run while still allowing a fresh manifest to be
//     recorded after a successful run.
type OutputsTracker struct {
	tempDir string
	dry     bool
	logger  *logger.Logger
}

// NewOutputsTracker creates an OutputsTracker that stores manifests under
// tempDir (the fingerprint temporary directory).
func NewOutputsTracker(tempDir string, dry bool, log *logger.Logger) *OutputsTracker {
	return &OutputsTracker{
		tempDir: tempDir,
		dry:     dry,
		logger:  log,
	}
}

// Enabled reports whether dynamic output tracking applies to t.
func (tr *OutputsTracker) Enabled(t *ast.Task) bool {
	return t != nil && t.Clean && len(t.Generates) > 0
}

// Begin snapshots the outputs currently present. It must be called right
// before a task's commands run, and the returned state passed to Commit once
// the run has succeeded.
func (tr *OutputsTracker) Begin(t *ast.Task) (*OutputsRun, error) {
	files, err := tr.snapshot(t)
	if err != nil {
		return nil, err
	}
	return &OutputsRun{before: files}, nil
}

// Commit finalizes a successful run: it determines which files the run
// actually produced, removes files recorded by the previous run that are no
// longer produced, and atomically records the new set of produced files.
// Commit must not be called after a failed or interrupted run.
func (tr *OutputsTracker) Commit(t *ast.Task, run *OutputsRun) error {
	if !tr.Enabled(t) || run == nil {
		return nil
	}

	// In dry mode commands do not run, so there is no new result set to
	// record: leave files and manifests untouched.
	if tr.dry {
		return nil
	}

	after, err := tr.snapshot(t)
	if err != nil {
		return err
	}

	// The current result set contains the files this run created or modified.
	// Files that existed beforehand and were left untouched are considered
	// stale by design: this feature targets tasks that regenerate their
	// (potentially dynamic) output set on every run.
	current := make(map[string]bool, len(after))
	for f, modTime := range after {
		prev, existed := run.before[f]
		if !existed || !modTime.Equal(prev) {
			current[f] = true
		}
	}

	manifestPath := tr.manifestPath(t)

	if previous, err := tr.load(manifestPath); err != nil {
		// There is no trustworthy record of what this task produced before, so
		// existing files must be left alone. The fresh manifest written below
		// restores cleanup from the next run onwards.
		if !errors.Is(err, fs.ErrNotExist) {
			tr.verbosef("task: [%s] not cleaning outputs: %v\n", t.Name(), err)
		}
	} else {
		tr.prune(t, previous, current, after)
	}

	return writeOutputsManifest(manifestPath, current)
}

// prune removes files recorded by the previous successful run that the current
// run no longer produces. A file is only removed when it still exists within
// the task's current generates scope, guaranteeing cleanup never escapes the
// outputs registered for this task.
func (tr *OutputsTracker) prune(t *ast.Task, previous *outputsManifest, current map[string]bool, after map[string]time.Time) {
	for _, f := range previous.Files {
		if current[f] {
			continue
		}
		// Either already gone, or no longer covered by the declared generates
		// scope: leave it untouched in both cases.
		if _, ok := after[f]; !ok {
			continue
		}
		if err := os.Remove(filepath.FromSlash(f)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			// Keep the successful run successful: a single undeletable stale
			// output must not fail the task. It simply drops out of the new
			// manifest and is reported verbosely.
			tr.verbosef("task: [%s] unable to remove stale output %q: %v\n", t.Name(), f, err)
			continue
		}
		tr.verbosef("task: [%s] removed stale output: %s\n", t.Name(), f)
	}
}

// snapshot returns the modification times of all regular files currently
// matching the task's generates patterns. Keys use slash-separated paths, as
// produced by Globs.
func (tr *OutputsTracker) snapshot(t *ast.Task) (map[string]time.Time, error) {
	files, err := Globs(t.Dir, t.Generates, t.ShouldUseGitignore())
	if err != nil {
		return nil, err
	}
	result := make(map[string]time.Time, len(files))
	for _, f := range files {
		// Manifests store absolute paths so they stay valid regardless of the
		// caller's working directory (the executor can use a relative Dir).
		abs, err := filepath.Abs(filepath.FromSlash(f))
		if err != nil {
			abs = filepath.FromSlash(f)
		}
		info, err := os.Stat(abs)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		// Track regular files only (os.Stat follows symlinks, so a symlink to
		// a regular file is included; directories and devices never are).
		// This ensures prune never removes a directory.
		if !info.Mode().IsRegular() {
			continue
		}
		result[filepath.ToSlash(abs)] = info.ModTime()
	}
	return result, nil
}

func (tr *OutputsTracker) manifestPath(t *ast.Task) string {
	return filepath.Join(tr.tempDir, outputsDir, normalizeFilename(t.Name()))
}

func (tr *OutputsTracker) verbosef(format string, args ...any) {
	if tr.logger != nil {
		tr.logger.VerboseOutf(logger.Yellow, format, args...)
	}
}

// load reads and validates the manifest at path. It returns
// fs.ErrNotExist when there is no manifest and ErrCorruptOutputsManifest for
// any unreadable or invalid content.
func (tr *OutputsTracker) load(path string) (*outputsManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m outputsManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, ErrCorruptOutputsManifest
	}
	if m.Version != outputsManifestVersion {
		return nil, ErrCorruptOutputsManifest
	}
	for _, f := range m.Files {
		// Entries must be non-empty absolute slash paths; anything else means
		// the manifest has been tampered with or is from an incompatible
		// version.
		if f == "" || !filepath.IsAbs(filepath.FromSlash(f)) {
			return nil, ErrCorruptOutputsManifest
		}
	}
	return &m, nil
}

// writeOutputsManifest writes the manifest atomically (temporary file in the
// same directory followed by a rename), so an interruption can never leave a
// partially written manifest behind.
func writeOutputsManifest(path string, files map[string]bool) error {
	list := make([]string, 0, len(files))
	for f := range files {
		list = append(list, f)
	}
	slices.Sort(list)

	data, err := json.MarshalIndent(&outputsManifest{
		Version: outputsManifestVersion,
		Files:   list,
	}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".outputs-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if the rename below never happens.
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// recordedOutputsExist checks that every file recorded by the task's latest
// successful run still exists. The boolean "checked" is false when there is no
// manifest to consult (the feature is off, or the task never ran
// successfully), in which case callers must keep their usual behavior. A
// corrupt manifest reports checked=true, ok=false: a damaged record cannot
// certify up-to-dateness, so the task is run again and the manifest rebuilt.
func recordedOutputsExist(tempDir string, t *ast.Task) (checked bool, ok bool) {
	if !t.Clean || len(t.Generates) == 0 {
		return false, true
	}
	path := filepath.Join(tempDir, outputsDir, normalizeFilename(t.Name()))
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, true
		}
		return true, false
	}
	var m outputsManifest
	if err := json.Unmarshal(data, &m); err != nil || m.Version != outputsManifestVersion {
		return true, false
	}
	for _, f := range m.Files {
		if _, err := os.Stat(filepath.FromSlash(f)); err != nil {
			return true, false
		}
	}
	return true, true
}
