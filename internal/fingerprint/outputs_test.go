package fingerprint

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-task/task/v3/taskfile/ast"
)

func newOutputsTestTask(dir string) *ast.Task {
	return &ast.Task{
		Task:      "build",
		FullName:  "build",
		Dir:       dir,
		Clean:     true,
		Generates: []*ast.Glob{{Glob: filepath.Join("out", "*.txt")}},
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func assertFileExists(t *testing.T, path string) {
	t.Helper()
	_, err := os.Stat(path)
	require.NoError(t, err, "expected %s to exist", path)
}

func assertFileAbsent(t *testing.T, path string) {
	t.Helper()
	_, err := os.Stat(path)
	require.True(t, os.IsNotExist(err), "expected %s to be gone, got %v", path, err)
}

func manifestFiles(t *testing.T, tr *OutputsTracker, task *ast.Task) []string {
	t.Helper()
	m, err := tr.load(tr.manifestPath(task))
	require.NoError(t, err)
	return m.Files
}

// A full regeneration cycle: first run records produced files; a second run
// with a different set prunes files that are no longer produced. Files that
// predate tracking are never registered and therefore never removed.
func TestOutputsTrackerPrunesStaleOutputs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	task := newOutputsTestTask(dir)
	tr := NewOutputsTracker(filepath.Join(dir, ".task"), false, nil)

	require.True(t, tr.Enabled(task))

	outDir := filepath.Join(dir, "out")
	preExisting := filepath.Join(outDir, "pre.txt")
	aFile := filepath.Join(outDir, "a.txt")
	bFile := filepath.Join(outDir, "b.txt")
	cFile := filepath.Join(outDir, "c.txt")

	// A file that exists before the feature ever tracks the task must survive
	// every run, even though it matches the generates patterns.
	writeTestFile(t, preExisting, "pre")

	// First successful run produces a.txt and b.txt.
	run, err := tr.Begin(task)
	require.NoError(t, err)
	writeTestFile(t, aFile, "a")
	writeTestFile(t, bFile, "b")
	require.NoError(t, tr.Commit(task, run))

	files := manifestFiles(t, tr, task)
	assert.ElementsMatch(t, []string{filepath.ToSlash(aFile), filepath.ToSlash(bFile)}, files)
	assertFileExists(t, preExisting)

	// Give the filesystem clock time to advance so rewrites get distinct
	// modification times.
	time.Sleep(10 * time.Millisecond)

	// Second successful run produces a.txt and c.txt, leaving b.txt behind.
	run, err = tr.Begin(task)
	require.NoError(t, err)
	writeTestFile(t, aFile, "a v2")
	writeTestFile(t, cFile, "c")
	require.NoError(t, tr.Commit(task, run))

	assertFileExists(t, aFile)
	assertFileExists(t, cFile)
	assertFileAbsent(t, bFile)
	assertFileExists(t, preExisting) // never registered, never removed

	files = manifestFiles(t, tr, task)
	assert.ElementsMatch(t, []string{filepath.ToSlash(aFile), filepath.ToSlash(cFile)}, files)
}

// A corrupt or invalid manifest must never lead to deletion of existing
// files. The successful run simply records a fresh manifest.
func TestOutputsTrackerCorruptManifestDoesNotDelete(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	task := newOutputsTestTask(dir)
	tr := NewOutputsTracker(filepath.Join(dir, ".task"), false, nil)

	outDir := filepath.Join(dir, "out")
	aFile := filepath.Join(outDir, "a.txt")
	bFile := filepath.Join(outDir, "b.txt")
	cFile := filepath.Join(outDir, "c.txt")

	run, err := tr.Begin(task)
	require.NoError(t, err)
	writeTestFile(t, aFile, "a")
	writeTestFile(t, bFile, "b")
	require.NoError(t, tr.Commit(task, run))

	// Corrupt the manifest between runs.
	require.NoError(t, os.WriteFile(tr.manifestPath(task), []byte("{not valid json"), 0o644))

	time.Sleep(10 * time.Millisecond)

	// Successful run producing a.txt and c.txt; b.txt is stale, but because
	// the previous manifest cannot be trusted, it must be kept.
	run, err = tr.Begin(task)
	require.NoError(t, err)
	writeTestFile(t, aFile, "a v2")
	writeTestFile(t, cFile, "c")
	require.NoError(t, tr.Commit(task, run))

	assertFileExists(t, aFile)
	assertFileExists(t, bFile)
	assertFileExists(t, cFile)

	// The manifest has been rebuilt and is valid again.
	files := manifestFiles(t, tr, task)
	assert.ElementsMatch(t, []string{filepath.ToSlash(aFile), filepath.ToSlash(cFile)}, files)

	// And the next run can prune normally: b.txt is not in a valid manifest
	// anymore, so it stays (it was dropped during the corrupt-run rebuild),
	// while c.txt - produced again - is retained.
	time.Sleep(10 * time.Millisecond)
	run, err = tr.Begin(task)
	require.NoError(t, err)
	writeTestFile(t, aFile, "a v3")
	writeTestFile(t, cFile, "c v2")
	require.NoError(t, tr.Commit(task, run))

	assertFileExists(t, bFile) // never re-registered, so untouchable
	assertFileExists(t, aFile)
	assertFileExists(t, cFile)
}

// Even a valid manifest must never cause deletion of paths outside the
// current generates scope, and directories are never removed.
func TestOutputsTrackerRespectsGeneratesScope(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	task := newOutputsTestTask(dir)
	tr := NewOutputsTracker(filepath.Join(dir, ".task"), false, nil)

	outDir := filepath.Join(dir, "out")
	aFile := filepath.Join(outDir, "a.txt")
	outsideFile := filepath.Join(dir, "elsewhere.txt")
	subDir := filepath.Join(outDir, "sub")

	run, err := tr.Begin(task)
	require.NoError(t, err)
	writeTestFile(t, aFile, "a")
	require.NoError(t, tr.Commit(task, run))

	writeTestFile(t, outsideFile, "outside")
	require.NoError(t, os.MkdirAll(subDir, 0o755))

	// Forge a manifest claiming an out-of-scope file and a directory as
	// previous outputs.
	manifest := outputsManifest{
		Version: outputsManifestVersion,
		Files: []string{
			filepath.ToSlash(aFile),
			filepath.ToSlash(outsideFile),
			filepath.ToSlash(subDir),
		},
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(tr.manifestPath(task), data, 0o644))

	time.Sleep(10 * time.Millisecond)

	// New run no longer produces a.txt.
	run, err = tr.Begin(task)
	require.NoError(t, err)
	writeTestFile(t, filepath.Join(outDir, "d.txt"), "d")
	require.NoError(t, tr.Commit(task, run))

	assertFileAbsent(t, aFile)       // registered, in scope, stale: removed
	assertFileExists(t, outsideFile) // outside the generates scope: kept
	assertFileExists(t, subDir)      // directories are never removed
}

// Dry mode must neither prune files nor write manifests.
func TestOutputsTrackerDryMode(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	task := newOutputsTestTask(dir)
	tr := NewOutputsTracker(filepath.Join(dir, ".task"), true, nil)

	aFile := filepath.Join(dir, "out", "a.txt")
	run, err := tr.Begin(task)
	require.NoError(t, err)
	writeTestFile(t, aFile, "a")
	require.NoError(t, tr.Commit(task, run))

	// No manifest was written in dry mode.
	_, err = os.Stat(tr.manifestPath(task))
	require.True(t, os.IsNotExist(err))
}

// recordedOutputsExist drives the up-to-date checks: nothing is checked
// without a manifest, missing recorded outputs fail the check, and a damaged
// record forces a re-run rather than trusting the state.
func TestRecordedOutputsExist(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tempDir := filepath.Join(dir, ".task")
	task := newOutputsTestTask(dir)
	tr := NewOutputsTracker(tempDir, false, nil)

	aFile := filepath.Join(dir, "out", "a.txt")

	// Opt-out: nothing to check.
	disabled := *task
	disabled.Clean = false
	checked, ok := recordedOutputsExist(tempDir, &disabled)
	assert.False(t, checked)
	assert.True(t, ok)

	// No manifest yet: nothing to check.
	checked, ok = recordedOutputsExist(tempDir, task)
	assert.False(t, checked)
	assert.True(t, ok)

	run, err := tr.Begin(task)
	require.NoError(t, err)
	writeTestFile(t, aFile, "a")
	require.NoError(t, tr.Commit(task, run))

	checked, ok = recordedOutputsExist(tempDir, task)
	assert.True(t, checked)
	assert.True(t, ok)

	// Removing a recorded output invalidates up-to-dateness.
	require.NoError(t, os.Remove(aFile))
	checked, ok = recordedOutputsExist(tempDir, task)
	assert.True(t, checked)
	assert.False(t, ok)

	// A corrupt manifest invalidates up-to-dateness as well.
	writeTestFile(t, aFile, "a")
	require.NoError(t, os.WriteFile(tr.manifestPath(task), []byte("junk"), 0o644))
	checked, ok = recordedOutputsExist(tempDir, task)
	assert.True(t, checked)
	assert.False(t, ok)
}

func TestOutputsTrackerEnabled(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tr := NewOutputsTracker(filepath.Join(dir, ".task"), false, nil)

	assert.False(t, tr.Enabled(nil))

	noOptIn := newOutputsTestTask(dir)
	noOptIn.Clean = false
	assert.False(t, tr.Enabled(noOptIn))

	noGenerates := newOutputsTestTask(dir)
	noGenerates.Generates = nil
	assert.False(t, tr.Enabled(noGenerates))

	assert.True(t, tr.Enabled(newOutputsTestTask(dir)))
}
