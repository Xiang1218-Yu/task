package session

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	taskerrors "github.com/go-task/task/v3/errors"
)

func testConfig(t *testing.T, id string) Config {
	t.Helper()
	return Config{
		ID:       id,
		Dir:      t.TempDir(),
		Identity: Identity{Entrypoint: "/project/Taskfile.yml", ContentHash: "abc"},
		Calls:    []CallSpec{{Task: "build"}},
		Globals:  []VarEntry{{Key: "FOO", Value: "bar"}},
		CLIArgs:  "",
	}
}

func incompatible(t *testing.T, err error) *taskerrors.SessionIncompatibleError {
	t.Helper()
	var ince *taskerrors.SessionIncompatibleError
	require.True(t, errors.As(err, &ince), "expected SessionIncompatibleError, got %v", err)
	return ince
}

func TestOpenOrCreateAndResume(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t, "s1")

	m, err := OpenOrCreate(cfg)
	require.NoError(t, err)
	assert.True(t, m.Created())

	require.NoError(t, m.MarkRunning("a", NodeInfo{Name: "a", Task: "a"}, ""))
	require.NoError(t, m.MarkSucceeded("a", "defa", InputHash("defa", nil)))
	require.NoError(t, m.Finish(nil))

	m2, err := OpenOrCreate(cfg)
	require.NoError(t, err)
	assert.False(t, m2.Created())
	status, ok := m2.NodeStatus("a")
	require.True(t, ok)
	assert.Equal(t, NodeSucceeded, status)

	b, err := os.ReadFile(Path(cfg.Dir, "s1"))
	require.NoError(t, err)
	assert.Contains(t, string(b), `"status": "completed"`)
}

func TestInvalidID(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t, "../escape")
	_, err := OpenOrCreate(cfg)
	var invalid *taskerrors.SessionInvalidError
	require.ErrorAs(t, err, &invalid)
}

func TestCompatibilityMismatches(t *testing.T) {
	t.Parallel()

	cases := []struct {
		id     string
		name   string
		mutate func(*Config)
		reason string
	}{
		{
			id:     "m-taskfile",
			name:   "taskfile content",
			mutate: func(c *Config) { c.Identity.ContentHash = "different" },
			reason: ReasonTaskfile,
		},
		{
			id:     "m-entrypoint",
			name:   "entrypoint",
			mutate: func(c *Config) { c.Identity.Entrypoint = "/other/Taskfile.yml" },
			reason: ReasonEntrypoint,
		},
		{
			id:     "m-calls",
			name:   "calls",
			mutate: func(c *Config) { c.Calls = []CallSpec{{Task: "test"}} },
			reason: ReasonCalls,
		},
		{
			id:     "m-vars",
			name:   "variables",
			mutate: func(c *Config) { c.Globals = []VarEntry{{Key: "FOO", Value: "baz"}} },
			reason: ReasonVariables,
		},
		{
			id:     "m-cliargs",
			name:   "cli args",
			mutate: func(c *Config) { c.CLIArgs = "--extra" },
			reason: ReasonCLIArgs,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig(t, tc.id)
			m, err := OpenOrCreate(cfg)
			require.NoError(t, err)
			require.NoError(t, m.Finish(nil))

			resume := testConfig(t, tc.id)
			resume.Dir = cfg.Dir
			tc.mutate(&resume)
			_, err = OpenOrCreate(resume)
			ince := incompatible(t, err)
			assert.Equal(t, tc.reason, ince.Reason)
		})
	}
}

func TestRunningNodeIsNeverReused(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t, "running")

	// First run: the node starts and never finishes (simulating a crash).
	m, err := OpenOrCreate(cfg)
	require.NoError(t, err)
	require.NoError(t, m.MarkRunning("c", NodeInfo{Name: "c", Task: "c"}, ""))
	require.NoError(t, m.Finish(errors.New("killed")))

	// Resume: the node is still "running" and must execute again.
	m2, err := OpenOrCreate(cfg)
	require.NoError(t, err)
	require.NoError(t, m2.MarkRunning("c", NodeInfo{Name: "c", Task: "c"}, ""))
	reuse, _ := m2.ReuseDecision("c", "defc")
	assert.False(t, reuse, "an interrupted node must not be reported as reusable")

	// Attempts accumulate across runs.
	snap := m2.Snapshot()
	assert.Equal(t, 2, snap.Nodes["c"].Attempts)
}

func TestFailedNodeRerunsAndSucceeds(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t, "failed")

	m, err := OpenOrCreate(cfg)
	require.NoError(t, err)
	require.NoError(t, m.MarkRunning("f", NodeInfo{Name: "f", Task: "f"}, ""))
	require.NoError(t, m.MarkFailed("f", errors.New("exit status 1")))
	require.NoError(t, m.Finish(errors.New("run failed")))

	m2, err := OpenOrCreate(cfg)
	require.NoError(t, err)
	require.NoError(t, m2.MarkRunning("f", NodeInfo{Name: "f", Task: "f"}, ""))
	reuse, input := m2.ReuseDecision("f", "deff")
	assert.False(t, reuse)

	require.NoError(t, m2.MarkSucceeded("f", "deff", input))
	require.NoError(t, m2.Finish(nil))

	m3, err := OpenOrCreate(cfg)
	require.NoError(t, err)
	require.NoError(t, m3.MarkRunning("f", NodeInfo{Name: "f", Task: "f"}, ""))
	reuse, _ = m3.ReuseDecision("f", "deff")
	assert.True(t, reuse, "a node that succeeded on retry must be reusable afterwards")
}

func TestUpstreamChangeCascades(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t, "cascade")

	// Graph: a -> b
	m, err := OpenOrCreate(cfg)
	require.NoError(t, err)
	require.NoError(t, m.MarkRunning("a", NodeInfo{Name: "a", Task: "a"}, ""))
	ha := InputHash("defa", nil)
	require.NoError(t, m.MarkSucceeded("a", "defa", ha))
	require.NoError(t, m.MarkRunning("b", NodeInfo{Name: "b", Task: "b"}, ""))
	require.NoError(t, m.MarkRunning("a", NodeInfo{Name: "a", Task: "a"}, "b"))
	hb := InputHash("defb", map[string]string{"a": ha})
	require.NoError(t, m.MarkSucceeded("b", "defb", hb))
	require.NoError(t, m.Finish(nil))

	// Resume with an unchanged graph: both nodes reuse.
	m2, err := OpenOrCreate(cfg)
	require.NoError(t, err)
	require.NoError(t, m2.MarkRunning("a", NodeInfo{}, ""))
	reuseA, _ := m2.ReuseDecision("a", "defa")
	assert.True(t, reuseA)
	require.NoError(t, m2.MarkReused("a"))
	require.NoError(t, m2.MarkRunning("b", NodeInfo{}, ""))
	reuseB, _ := m2.ReuseDecision("b", "defb")
	assert.True(t, reuseB, "unchanged upstream must allow reuse downstream")
	require.NoError(t, m2.MarkReused("b"))
	require.NoError(t, m2.Finish(nil))

	// On another resume, the definition of "a" changes: its input hash
	// changes and b must no longer be reusable.
	m3, err := OpenOrCreate(cfg)
	require.NoError(t, err)
	require.NoError(t, m3.MarkRunning("a", NodeInfo{}, ""))
	newHA := InputHash("defa2", nil)
	reuseA, _ = m3.ReuseDecision("a", "defa2")
	assert.False(t, reuseA)
	require.NoError(t, m3.MarkSucceeded("a", "defa2", newHA))

	require.NoError(t, m3.MarkRunning("b", NodeInfo{}, ""))
	reuseB, newHB := m3.ReuseDecision("b", "defb")
	assert.False(t, reuseB, "a changed upstream input must invalidate its dependents")
	assert.NotEqual(t, hb, newHB)
}

func TestRerunDependencyForcesParentRerun(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t, "depfail")

	// Run 1: p depends on a; a fails while p nevertheless completes (e.g.
	// because the task call ignores errors).
	m, err := OpenOrCreate(cfg)
	require.NoError(t, err)
	require.NoError(t, m.MarkRunning("a", NodeInfo{}, ""))
	require.NoError(t, m.MarkFailed("a", errors.New("boom")))
	ha := InputHash("defa", nil)
	require.NoError(t, m.MarkRunning("p", NodeInfo{}, ""))
	require.NoError(t, m.MarkRunning("a", NodeInfo{}, "p"))
	require.NoError(t, m.MarkSucceeded("p", "defp", InputHash("defp", map[string]string{"a": ha})))
	require.NoError(t, m.Finish(errors.New("run failed")))

	// Resume: a must run again (it was failed); its input hash itself does
	// not change, so only the rerun guard can invalidate p.
	m2, err := OpenOrCreate(cfg)
	require.NoError(t, err)
	require.NoError(t, m2.MarkRunning("a", NodeInfo{}, ""))
	reuseA, ha2 := m2.ReuseDecision("a", "defa")
	assert.False(t, reuseA)
	require.NoError(t, m2.MarkSucceeded("a", "defa", ha2))
	assert.Equal(t, ha, ha2, "the definition of a is unchanged")

	require.NoError(t, m2.MarkRunning("p", NodeInfo{}, ""))
	assert.True(t, m2.DependencyReran("p"))
	reuseP, _ := m2.ReuseDecision("p", "defp")
	assert.False(t, reuseP, "a reran, so its dependent p must run again")
}

func TestFinishActions(t *testing.T) {
	t.Parallel()

	t.Run("retain marks completed", func(t *testing.T) {
		t.Parallel()
		cfg := testConfig(t, "retain")
		m, err := OpenOrCreate(cfg)
		require.NoError(t, err)
		require.NoError(t, m.MarkRunning("a", NodeInfo{}, ""))
		require.NoError(t, m.MarkSucceeded("a", "d", InputHash("d", nil)))
		require.NoError(t, m.Finish(nil))
		assert.Equal(t, StatusCompleted, m.Snapshot().Status)
		assert.FileExists(t, Path(cfg.Dir, "retain"))
	})

	t.Run("clean removes the file", func(t *testing.T) {
		t.Parallel()
		cfg := testConfig(t, "clean")
		cfg.Action = CompleteClean
		m, err := OpenOrCreate(cfg)
		require.NoError(t, err)
		require.NoError(t, m.Finish(nil))
		_, err = os.Stat(Path(cfg.Dir, "clean"))
		assert.True(t, os.IsNotExist(err))
	})

	t.Run("archive moves the file and blocks resume", func(t *testing.T) {
		t.Parallel()
		cfg := testConfig(t, "archive")
		cfg.Action = CompleteArchive
		m, err := OpenOrCreate(cfg)
		require.NoError(t, err)
		require.NoError(t, m.Finish(nil))
		assert.NoFileExists(t, Path(cfg.Dir, "archive"))
		assert.FileExists(t, filepath.Join(cfg.Dir, "archive.archived.json"))

		_, err = OpenOrCreate(cfg)
		assert.Equal(t, ReasonArchived, incompatible(t, err).Reason)
	})

	t.Run("failure is always retained", func(t *testing.T) {
		t.Parallel()
		cfg := testConfig(t, "keepfailed")
		cfg.Action = CompleteClean
		m, err := OpenOrCreate(cfg)
		require.NoError(t, err)
		require.NoError(t, m.Finish(errors.New("nope")))
		assert.FileExists(t, Path(cfg.Dir, "keepfailed"))
		assert.Equal(t, StatusFailed, m.Snapshot().Status)
	})

	t.Run("unfinished node cannot be reported completed", func(t *testing.T) {
		t.Parallel()
		cfg := testConfig(t, "unfinished")
		m, err := OpenOrCreate(cfg)
		require.NoError(t, err)
		require.NoError(t, m.MarkRunning("a", NodeInfo{}, ""))
		// Node left running, but the caller wrongly reports success.
		require.NoError(t, m.Finish(nil))
		assert.Equal(t, StatusFailed, m.Snapshot().Status,
			"a successful run with a running node must be marked failed")
	})
}

func TestRemoveSession(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t, "rm")
	m, err := OpenOrCreate(cfg)
	require.NoError(t, err)
	require.NoError(t, m.Finish(nil))

	require.NoError(t, Remove(cfg.Dir, "rm"))
	err = Remove(cfg.Dir, "rm")
	var notFound *taskerrors.SessionNotFoundError
	require.ErrorAs(t, err, &notFound)
}

func TestPersistedFileIsJSON(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t, "json")
	m, err := OpenOrCreate(cfg)
	require.NoError(t, err)
	require.NoError(t, m.MarkRunning("a", NodeInfo{Name: "A", Task: "ns:a"}, ""))

	snap := m.Snapshot()
	assert.Equal(t, SchemaVersion, snap.SchemaVersion)
	assert.Equal(t, "json", snap.ID)
	assert.Equal(t, StatusRunning, snap.Status)
	assert.Equal(t, NodeRunning, snap.Nodes["a"].Status)
	assert.Equal(t, 1, snap.Nodes["a"].Attempts)
}
