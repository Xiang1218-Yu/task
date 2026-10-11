package task_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-task/task/v3"
	"github.com/go-task/task/v3/internal/session"
	"github.com/go-task/task/v3/taskfile/ast"
)

// newResumeExecutor builds an executor whose .task state and output live in
// the fixed testdata directory, exactly like a CLI invocation would.
func newResumeExecutor(t *testing.T, dir string, resume, forceAll bool) *task.Executor {
	t.Helper()
	opts := []task.ExecutorOption{
		task.WithDir(dir),
		task.WithSilent(true),
		task.WithOutputStyle(ast.Output{Name: "group"}),
		task.WithStdout(&bytes.Buffer{}),
		task.WithStderr(&bytes.Buffer{}),
	}
	if resume {
		opts = append(opts, task.WithResume(true))
	}
	if forceAll {
		opts = append(opts, task.WithForceAll(true))
	}
	e := task.NewExecutor(opts...)
	require.NoError(t, e.Setup())
	return e
}

func runResumeCall(t *testing.T, e *task.Executor, name string, vars map[string]any) error {
	t.Helper()
	v := ast.NewVars()
	for key, value := range vars {
		v.Set(key, ast.Var{Value: value})
	}
	return e.Run(context.Background(), &task.Call{Task: name, Vars: v})
}

func resumeMarks(t *testing.T, dir, file string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, file))
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

func resetResumeDir(t *testing.T, dir string, files ...string) {
	t.Helper()
	all := append([]string{".task"}, files...)
	for _, name := range all {
		_ = os.RemoveAll(filepath.Join(dir, name))
	}
	t.Cleanup(func() {
		for _, name := range all {
			_ = os.RemoveAll(filepath.Join(dir, name))
		}
	})
}

func createSentinel(t *testing.T, dir, name string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644))
}

func removeSentinel(t *testing.T, dir, name string) {
	t.Helper()
	require.NoError(t, os.Remove(filepath.Join(dir, name)))
}

// findSessionStateFile locates the persisted state under .task/session.
func findSessionStateFile(t *testing.T, dir string) string {
	t.Helper()
	var found []string
	require.NoError(t, filepath.Walk(filepath.Join(dir, ".task", "session"), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && info.Name() == session.StateFileName {
			found = append(found, path)
		}
		return nil
	}))
	require.Len(t, found, 1)
	return found[0]
}

func sessionDirExists(t *testing.T, dir string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, ".task", "session"))
	if os.IsNotExist(err) {
		return false
	}
	require.NoError(t, err)
	return true
}

func TestResumeInterruptedRun(t *testing.T) {
	t.Parallel()

	const dir = "testdata/resume"

	// All subtests share the fixed testdata directory, so they run sequentially.

	t.Run("skips completed deps and reruns half-finished command", func(t *testing.T) {
		resetResumeDir(t, dir, "marks-a.txt", "marks-b.txt", "marks-root.txt", "fail-root2")

		// Run 1: interrupted (failed) while root2 is running - root2 already
		// left a side effect behind.
		createSentinel(t, dir, "fail-root2")
		e1 := newResumeExecutor(t, dir, true, false)
		require.Error(t, runResumeCall(t, e1, "default", nil))

		assert.Equal(t, []string{"a1", "a2"}, resumeMarks(t, dir, "marks-a.txt"))
		assert.Equal(t, []string{"b1"}, resumeMarks(t, dir, "marks-b.txt"))
		assert.Equal(t, []string{"root1", "root2"}, resumeMarks(t, dir, "marks-root.txt"))

		// Run 2: resume with the failing step fixed.
		removeSentinel(t, dir, "fail-root2")
		e2 := newResumeExecutor(t, dir, true, false)
		require.NoError(t, runResumeCall(t, e2, "default", nil))

		// Dependencies were already complete and must not run again.
		assert.Equal(t, []string{"a1", "a2"}, resumeMarks(t, dir, "marks-a.txt"))
		assert.Equal(t, []string{"b1"}, resumeMarks(t, dir, "marks-b.txt"))
		// root1 skipped; root2 (no completion record despite its side effect)
		// and root3 ran.
		assert.Equal(t, []string{"root1", "root2", "root2", "root3"}, resumeMarks(t, dir, "marks-root.txt"))

		// The successful invocation consumed its recovery state.
		assert.False(t, sessionDirExists(t, dir))

		// Run 3: nothing to resume anymore - the whole chain executes normally.
		e3 := newResumeExecutor(t, dir, true, false)
		require.NoError(t, runResumeCall(t, e3, "default", nil))

		assert.Equal(t, []string{"a1", "a2", "a1", "a2"}, resumeMarks(t, dir, "marks-a.txt"))
		assert.Equal(t, []string{"b1", "b1"}, resumeMarks(t, dir, "marks-b.txt"))
		assert.Equal(t,
			[]string{"root1", "root2", "root2", "root3", "root1", "root2", "root3"},
			resumeMarks(t, dir, "marks-root.txt"))
	})

	t.Run("disabled without the resume flag", func(t *testing.T) {
		resetResumeDir(t, dir, "marks-a.txt", "marks-b.txt", "marks-root.txt", "fail-root2")

		createSentinel(t, dir, "fail-root2")
		e1 := newResumeExecutor(t, dir, false, false)
		require.Error(t, runResumeCall(t, e1, "default", nil))

		removeSentinel(t, dir, "fail-root2")
		e2 := newResumeExecutor(t, dir, false, false)
		require.NoError(t, runResumeCall(t, e2, "default", nil))

		// Existing behavior: deps and commands run again from the beginning.
		assert.Equal(t, []string{"a1", "a2", "a1", "a2"}, resumeMarks(t, dir, "marks-a.txt"))
		assert.Equal(t, []string{"b1", "b1"}, resumeMarks(t, dir, "marks-b.txt"))
		assert.Equal(t,
			[]string{"root1", "root2", "root1", "root2", "root3"},
			resumeMarks(t, dir, "marks-root.txt"))
		assert.False(t, sessionDirExists(t, dir))
	})

	t.Run("changed input variable invalidates the task record", func(t *testing.T) {
		resetResumeDir(t, dir, "marks-a.txt", "marks-b.txt", "marks-root.txt", "fail-root2")

		createSentinel(t, dir, "fail-root2")
		e1 := newResumeExecutor(t, dir, true, false)
		require.Error(t, runResumeCall(t, e1, "default", map[string]any{"FOO": "one"}))

		// Same failing step, but FOO changed: the root task must re-execute.
		// The dependency tasks do not depend on FOO, so they stay completed.
		e2 := newResumeExecutor(t, dir, true, false)
		require.Error(t, runResumeCall(t, e2, "default", map[string]any{"FOO": "two"}))

		assert.Equal(t, []string{"root1", "root2", "root1", "root2"}, resumeMarks(t, dir, "marks-root.txt"))
		assert.Equal(t, []string{"a1", "a2"}, resumeMarks(t, dir, "marks-a.txt"))
		assert.Equal(t, []string{"b1"}, resumeMarks(t, dir, "marks-b.txt"))
	})

	t.Run("force rebuild ignores saved progress", func(t *testing.T) {
		resetResumeDir(t, dir, "marks-a.txt", "marks-b.txt", "marks-root.txt", "fail-root2")

		createSentinel(t, dir, "fail-root2")
		e1 := newResumeExecutor(t, dir, true, false)
		require.Error(t, runResumeCall(t, e1, "default", nil))

		removeSentinel(t, dir, "fail-root2")
		e2 := newResumeExecutor(t, dir, true, true)
		require.NoError(t, runResumeCall(t, e2, "default", nil))

		assert.Equal(t, []string{"a1", "a2", "a1", "a2"}, resumeMarks(t, dir, "marks-a.txt"))
		assert.Equal(t, []string{"b1", "b1"}, resumeMarks(t, dir, "marks-b.txt"))
		assert.Equal(t,
			[]string{"root1", "root2", "root1", "root2", "root3"},
			resumeMarks(t, dir, "marks-root.txt"))
	})

	t.Run("corrupt progress record is discarded", func(t *testing.T) {
		resetResumeDir(t, dir, "marks-a.txt", "marks-b.txt", "marks-root.txt", "fail-root2")

		createSentinel(t, dir, "fail-root2")
		e1 := newResumeExecutor(t, dir, true, false)
		require.Error(t, runResumeCall(t, e1, "default", nil))

		stateFile := findSessionStateFile(t, dir)
		require.NoError(t, os.WriteFile(stateFile, []byte("{this is not json"), 0o644))

		removeSentinel(t, dir, "fail-root2")
		e2 := newResumeExecutor(t, dir, true, false)
		require.NoError(t, runResumeCall(t, e2, "default", nil))

		// Nothing from the corrupt record was trusted.
		assert.Equal(t, []string{"a1", "a2", "a1", "a2"}, resumeMarks(t, dir, "marks-a.txt"))
		assert.Equal(t, []string{"b1", "b1"}, resumeMarks(t, dir, "marks-b.txt"))
		assert.Equal(t,
			[]string{"root1", "root2", "root1", "root2", "root3"},
			resumeMarks(t, dir, "marks-root.txt"))

		// The unusable file was quarantined rather than silently overwritten.
		var quarantined []string
		require.NoError(t, filepath.Walk(filepath.Join(dir, ".task", "session"), func(path string, info os.FileInfo, err error) error {
			if err == nil && strings.HasPrefix(info.Name(), "state.corrupt-") {
				quarantined = append(quarantined, path)
			}
			return nil
		}))
		assert.Len(t, quarantined, 1)
	})

	t.Run("cleared progress forces a full run", func(t *testing.T) {
		resetResumeDir(t, dir, "marks-a.txt", "marks-b.txt", "marks-root.txt", "fail-root2")

		createSentinel(t, dir, "fail-root2")
		e1 := newResumeExecutor(t, dir, true, false)
		require.Error(t, runResumeCall(t, e1, "default", nil))

		require.NoError(t, session.Clear(filepath.Join(dir, ".task")))
		assert.False(t, sessionDirExists(t, dir))

		removeSentinel(t, dir, "fail-root2")
		e2 := newResumeExecutor(t, dir, true, false)
		require.NoError(t, runResumeCall(t, e2, "default", nil))

		assert.Equal(t, []string{"a1", "a2", "a1", "a2"}, resumeMarks(t, dir, "marks-a.txt"))
		assert.Equal(t,
			[]string{"root1", "root2", "root1", "root2", "root3"},
			resumeMarks(t, dir, "marks-root.txt"))
	})
}

func TestResumeSources(t *testing.T) {
	t.Parallel()

	const dir = "testdata/resume_sources"

	writeInput := func(t *testing.T, content string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "input.txt"), []byte(content), 0o644))
	}
	readOut := func(t *testing.T) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, "out.txt"))
		if os.IsNotExist(err) {
			return ""
		}
		require.NoError(t, err)
		return string(data)
	}

	// Subtests share the fixed testdata directory and run sequentially.

	t.Run("unchanged sources skip the completed command", func(t *testing.T) {
		resetResumeDir(t, dir, "input.txt", "out.txt", "fail-after-copy")
		writeInput(t, "version-1\n")

		createSentinel(t, dir, "fail-after-copy")
		e1 := newResumeExecutor(t, dir, true, false)
		require.Error(t, runResumeCall(t, e1, "default", nil))
		assert.Equal(t, "version-1\n", readOut(t))

		// Remove the output: if the copy is resumed (skipped), the file stays
		// absent; if the command ran again, it would be recreated.
		require.NoError(t, os.Remove(filepath.Join(dir, "out.txt")))

		removeSentinel(t, dir, "fail-after-copy")
		e2 := newResumeExecutor(t, dir, true, false)
		require.NoError(t, runResumeCall(t, e2, "default", nil))
		assert.Empty(t, readOut(t), "the copy command should have been skipped")
	})

	t.Run("changed sources rerun the command", func(t *testing.T) {
		resetResumeDir(t, dir, "input.txt", "out.txt", "fail-after-copy")
		writeInput(t, "version-1\n")

		createSentinel(t, dir, "fail-after-copy")
		e1 := newResumeExecutor(t, dir, true, false)
		require.Error(t, runResumeCall(t, e1, "default", nil))
		assert.Equal(t, "version-1\n", readOut(t))

		// Source contents change after the interrupted run: the old record no
		// longer matches and the copy must execute again.
		writeInput(t, "version-2\n")

		removeSentinel(t, dir, "fail-after-copy")
		e2 := newResumeExecutor(t, dir, true, false)
		require.NoError(t, runResumeCall(t, e2, "default", nil))
		assert.Equal(t, "version-2\n", readOut(t))
	})
}
