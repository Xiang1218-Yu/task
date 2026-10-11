package task

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-task/task/v3/internal/filepathext"
)

// An interrupted run must not be treated as successful: stale files are not
// pruned and the output manifest keeps describing the latest *successful*
// run, even if the shell commands themselves returned without an error after
// a signal was received.
func TestDynamicOutputsInterruptedRunDoesNotCommit(t *testing.T) { // nolint:paralleltest // mutates process env vars and shared testdata
	const dir = "testdata/dynamic_outputs"

	for _, p := range []string{"out", ".task"} {
		require.NoError(t, os.RemoveAll(filepathext.SmartJoin(dir, p)))
	}

	newExecutor := func() *Executor {
		t.Helper()
		var buff bytes.Buffer
		tempDir := TempDir{
			Remote:      filepathext.SmartJoin(dir, ".task"),
			Fingerprint: filepathext.SmartJoin(dir, ".task"),
		}
		e := NewExecutor(
			WithDir(dir),
			WithStdout(&buff),
			WithStderr(&buff),
			WithTempDir(tempDir),
		)
		require.NoError(t, e.Setup())
		return e
	}

	// First run succeeds and registers out/a.txt and out/b.txt.
	t.Setenv("OUTPUTS", "a.txt b.txt")
	e := newExecutor()
	require.NoError(t, e.Run(t.Context(), &Call{Task: "build"}))

	manifest := filepath.Join(dir, ".task/outputs/build")
	first, err := os.ReadFile(manifest)
	require.NoError(t, err)
	info1, err := os.Stat(manifest)
	require.NoError(t, err)

	// A signal arrives during the next run that would otherwise prune b.txt.
	t.Setenv("OUTPUTS", "a.txt c.txt")
	e = newExecutor()
	e.signalReceived.Store(true)
	require.NoError(t, e.Run(t.Context(), &Call{Task: "build"}))

	require.FileExists(t, filepathext.SmartJoin(dir, "out/b.txt"),
		"interrupted run must not prune stale outputs")
	second, err := os.ReadFile(manifest)
	require.NoError(t, err)
	assert.Equal(t, first, second, "interrupted run must not rewrite the manifest")
	info2, err := os.Stat(manifest)
	require.NoError(t, err)
	assert.Equal(t, info1.ModTime(), info2.ModTime())

	// The next healthy run commits normally and prunes b.txt.
	e = newExecutor()
	require.NoError(t, e.Run(t.Context(), &Call{Task: "build"}))
	_, err = os.Stat(filepathext.SmartJoin(dir, "out/b.txt"))
	require.Error(t, err, "b.txt should be pruned after a healthy run")
	require.FileExists(t, filepathext.SmartJoin(dir, "out/a.txt"))
	require.FileExists(t, filepathext.SmartJoin(dir, "out/c.txt"))
}
