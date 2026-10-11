package task

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	taskerrors "github.com/go-task/task/v3/errors"
	"github.com/go-task/task/v3/internal/session"
	"github.com/go-task/task/v3/taskfile/ast"
)

// Each task appends to its own log file so parallel dependencies cannot
// interleave lines inside one file.
const basicGraphTaskfile = `version: '3'

tasks:
  default:
    deps: [a, b]
    cmds:
      - echo run >> default.log
  a:
    cmds:
      - echo run >> a.log
  b:
    deps: [c]
    cmds:
      - echo run >> b.log
  c:
    cmds:
      - echo run >> c.log
  other:
    cmds:
      - echo run >> other.log
`

func writeSessionProject(t *testing.T, taskfile string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Taskfile.yml"), []byte(taskfile), 0o644))
	return dir
}

type sessionRun struct {
	cfg *SessionConfig
	buf *bytes.Buffer
	e   *Executor
}

func newSessionRun(t *testing.T, dir, id string, action ...func(*SessionConfig)) *sessionRun {
	t.Helper()
	cfg := &SessionConfig{ID: id}
	for _, fn := range action {
		fn(cfg)
	}
	var buf bytes.Buffer
	e := NewExecutor(
		WithDir(dir),
		WithSessionConfig(cfg),
		WithStdout(&buf),
		WithStderr(&buf),
		WithAssumeTerm(true),
	)
	require.NoError(t, e.Setup())
	return &sessionRun{cfg: cfg, buf: &buf, e: e}
}

func (r *sessionRun) withGlobals(globals map[string]string) *sessionRun {
	r.cfg.Globals = globals
	vars := ast.NewVars()
	for k, v := range globals {
		vars.Set(k, ast.Var{Value: v})
	}
	r.e.Taskfile.Vars.Merge(vars, nil)
	return r
}

func (r *sessionRun) run(ctx context.Context, taskNames ...string) error {
	// Match the CLI: with no task arguments the "default" task runs.
	if len(taskNames) == 0 {
		taskNames = []string{"default"}
	}
	calls := make([]*Call, 0, len(taskNames))
	for _, name := range taskNames {
		calls = append(calls, &Call{Task: name, Vars: ast.NewVars()})
	}
	return r.e.Run(ctx, calls...)
}

func (r *sessionRun) output() string { return r.buf.String() }

func loadSessionFile(t *testing.T, dir, id string) *session.Session {
	t.Helper()
	path := filepath.Join(dir, ".task", "sessions", id+".json")
	b, err := os.ReadFile(path)
	require.NoError(t, err, "session file not found: %s", path)
	var s session.Session
	require.NoError(t, json.Unmarshal(b, &s))
	return &s
}

func sessionFileExists(dir, id string) bool {
	_, err := os.Stat(filepath.Join(dir, ".task", "sessions", id+".json"))
	return err == nil
}

func archivedSessionFileExists(dir, id string) bool {
	_, err := os.Stat(filepath.Join(dir, ".task", "sessions", id+".archived.json"))
	return err == nil
}

func findNode(s *session.Session, taskName string) *session.Node {
	for _, n := range s.Nodes {
		if n.Task == taskName {
			return n
		}
	}
	return nil
}

func countLogLines(t *testing.T, dir, file, name string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, file))
	if os.IsNotExist(err) {
		return 0
	}
	require.NoError(t, err)
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.TrimSpace(line) == name {
			count++
		}
	}
	return count
}

// A successful first run completes every node; a second run with the same
// session reuses all of them and executes no commands.
func TestSessionRecoveryNodeSkip(t *testing.T) {
	t.Parallel()
	dir := writeSessionProject(t, basicGraphTaskfile)

	r1 := newSessionRun(t, dir, "skip")
	require.NoError(t, r1.run(context.Background()))
	for _, name := range []string{"a", "b", "c", "default"} {
		require.Equal(t, 1, countLogLines(t, dir, name+".log", "run"), "node %s should run once", name)
	}
	s1 := loadSessionFile(t, dir, "skip")
	assert.Equal(t, session.StatusCompleted, s1.Status)
	for _, name := range []string{"a", "b", "c", "default"} {
		assert.Equal(t, session.NodeSucceeded, findNode(s1, name).Status)
	}

	r2 := newSessionRun(t, dir, "skip")
	require.NoError(t, r2.run(context.Background()))
	out := r2.output()
	assert.Contains(t, out, "reusing result")
	for _, name := range []string{"a", "b", "c", "default"} {
		assert.Equal(t, 1, countLogLines(t, dir, name+".log", "run"), "node %s must not run again when reused", name)
	}
	s2 := loadSessionFile(t, dir, "skip")
	assert.Equal(t, session.StatusCompleted, s2.Status)
}

// A failed node runs again on resume while its already successful dependency
// is reused. Downstream nodes are re-scheduled and the session converges.
func TestSessionRecoveryFailureRerun(t *testing.T) {
	t.Parallel()
	taskfile := `version: '3'

tasks:
  default:
    deps: [a, flaky]
    cmds:
      - echo run >> default.log
  a:
    cmds:
      - echo run >> a.log
  flaky:
    cmds:
      - test -f gate
      - echo run >> flaky.log
`
	dir := writeSessionProject(t, taskfile)

	r1 := newSessionRun(t, dir, "fail")
	err := r1.run(context.Background())
	require.Error(t, err)
	assert.Equal(t, 1, countLogLines(t, dir, "a.log", "run"))
	assert.Equal(t, 0, countLogLines(t, dir, "flaky.log", "run"))
	assert.Equal(t, 0, countLogLines(t, dir, "default.log", "run"))
	s1 := loadSessionFile(t, dir, "fail")
	assert.Equal(t, session.StatusFailed, s1.Status)
	assert.Equal(t, session.NodeFailed, findNode(s1, "flaky").Status)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "gate"), []byte("ok"), 0o644))

	r2 := newSessionRun(t, dir, "fail")
	require.NoError(t, r2.run(context.Background()))
	assert.Contains(t, r2.output(), "reusing result")
	assert.Equal(t, 1, countLogLines(t, dir, "a.log", "run"), "successful dependency must be reused")
	assert.Equal(t, 1, countLogLines(t, dir, "flaky.log", "run"), "failed node must run again")
	assert.Equal(t, 1, countLogLines(t, dir, "default.log", "run"), "downstream node must be re-scheduled")
	s2 := loadSessionFile(t, dir, "fail")
	assert.Equal(t, session.StatusCompleted, s2.Status)
	assert.Equal(t, 2, findNode(s2, "flaky").Attempts)
}

// A node left running when the process is interrupted (context canceled) is
// never mistaken for success and runs again on resume.
func TestSessionRecoveryInterruptedNode(t *testing.T) {
	t.Parallel()
	taskfile := `version: '3'

tasks:
  default:
    deps: [a, slow]
    cmds:
      - echo run >> default.log
  a:
    cmds:
      - echo run >> a.log
  slow:
    cmds:
      - echo start >> slow.log
      - while [ ! -f gate ]; do sleep 0.05; done
      - echo done >> slow.log
`
	dir := writeSessionProject(t, taskfile)

	ctx, cancel := context.WithCancel(context.Background())
	r1 := newSessionRun(t, dir, "interrupt")
	done := make(chan error, 1)
	go func() { done <- r1.run(ctx) }()

	require.Eventually(t, func() bool {
		return countLogLines(t, dir, "a.log", "run") == 1 &&
			countLogLines(t, dir, "slow.log", "start") == 1
	}, 5*time.Second, 10*time.Millisecond, "interrupted run never reached the waiting node")
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("interrupted run did not stop after cancellation")
	}

	s1 := loadSessionFile(t, dir, "interrupt")
	assert.Equal(t, session.StatusFailed, s1.Status)
	slow := findNode(s1, "slow")
	require.NotNil(t, slow)
	assert.NotEqual(t, session.NodeSucceeded, slow.Status,
		"an interrupted node must not be recorded as succeeded")
	assert.Equal(t, 0, countLogLines(t, dir, "slow.log", "done"))
	assert.Equal(t, 0, countLogLines(t, dir, "default.log", "run"))

	require.NoError(t, os.WriteFile(filepath.Join(dir, "gate"), []byte("ok"), 0o644))

	r2 := newSessionRun(t, dir, "interrupt")
	require.NoError(t, r2.run(context.Background()))
	assert.Equal(t, 1, countLogLines(t, dir, "a.log", "run"), "node finished before the interruption is reused")
	assert.Equal(t, 1, countLogLines(t, dir, "slow.log", "done"), "interrupted node must run again and complete")
	assert.Equal(t, 2, countLogLines(t, dir, "slow.log", "start"), "the interrupted node must restart")
	assert.Equal(t, 1, countLogLines(t, dir, "default.log", "run"))
	assert.Equal(t, session.StatusCompleted, loadSessionFile(t, dir, "interrupt").Status)
}

// Resuming with an incompatible invocation is rejected before any command
// runs, with a precise reason.
func TestSessionRecoveryIncompatible(t *testing.T) {
	t.Parallel()

	t.Run("taskfile changed", func(t *testing.T) {
		t.Parallel()
		dir := writeSessionProject(t, basicGraphTaskfile)
		r1 := newSessionRun(t, dir, "incompat-tf")
		require.NoError(t, r1.run(context.Background()))

		// Edit a command definition.
		edited := strings.Replace(basicGraphTaskfile, "echo run >> a.log", "echo changed >> a.log", 1)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "Taskfile.yml"), []byte(edited), 0o644))

		r2 := newSessionRun(t, dir, "incompat-tf")
		err := r2.run(context.Background())
		var ince *taskerrors.SessionIncompatibleError
		require.ErrorAs(t, err, &ince)
		assert.Equal(t, session.ReasonTaskfile, ince.Reason)
		assert.Equal(t, 1, countLogLines(t, dir, "a.log", "run"), "no command may run after a compatibility failure")
	})

	t.Run("variables changed", func(t *testing.T) {
		t.Parallel()
		taskfile := `version: '3'

tasks:
  default:
    cmds:
      - echo {{.GREETING}} >> greet.log
`
		dir := writeSessionProject(t, taskfile)
		r1 := newSessionRun(t, dir, "incompat-vars").
			withGlobals(map[string]string{"GREETING": "hi"})
		require.NoError(t, r1.run(context.Background()))

		r2 := newSessionRun(t, dir, "incompat-vars").
			withGlobals(map[string]string{"GREETING": "hey"})
		err := r2.run(context.Background())
		var ince *taskerrors.SessionIncompatibleError
		require.ErrorAs(t, err, &ince)
		assert.Equal(t, session.ReasonVariables, ince.Reason)
		assert.Equal(t, 0, countLogLines(t, dir, "greet.log", "hey"))
	})

	t.Run("requested tasks changed", func(t *testing.T) {
		t.Parallel()
		dir := writeSessionProject(t, basicGraphTaskfile)
		r1 := newSessionRun(t, dir, "incompat-calls")
		require.NoError(t, r1.run(context.Background(), "default"))

		r2 := newSessionRun(t, dir, "incompat-calls")
		err := r2.run(context.Background(), "other")
		var ince *taskerrors.SessionIncompatibleError
		require.ErrorAs(t, err, &ince)
		assert.Equal(t, session.ReasonCalls, ince.Reason)
		assert.Equal(t, 0, countLogLines(t, dir, "other.log", "run"))
	})
}

// Completion actions: retain marks the session completed, clean removes the
// file and archive moves it aside and blocks further resumes.
func TestSessionCompletionActions(t *testing.T) {
	t.Parallel()

	t.Run("retain then resume and remove", func(t *testing.T) {
		t.Parallel()
		dir := writeSessionProject(t, basicGraphTaskfile)
		r1 := newSessionRun(t, dir, "lifecycle")
		require.NoError(t, r1.run(context.Background()))
		assert.True(t, sessionFileExists(dir, "lifecycle"))
		assert.Equal(t, session.StatusCompleted, loadSessionFile(t, dir, "lifecycle").Status)

		// Resuming a completed session converges immediately.
		r2 := newSessionRun(t, dir, "lifecycle")
		require.NoError(t, r2.run(context.Background()))
		assert.True(t, sessionFileExists(dir, "lifecycle"))

		require.NoError(t, r2.e.RemoveSession("lifecycle"))
		assert.False(t, sessionFileExists(dir, "lifecycle"))
		err := r2.e.RemoveSession("lifecycle")
		var notFound *taskerrors.SessionNotFoundError
		require.ErrorAs(t, err, &notFound)
	})

	t.Run("clean", func(t *testing.T) {
		t.Parallel()
		dir := writeSessionProject(t, basicGraphTaskfile)
		r := newSessionRun(t, dir, "clean", func(cfg *SessionConfig) { cfg.Clean = true })
		require.NoError(t, r.run(context.Background()))
		assert.False(t, sessionFileExists(dir, "clean"))
	})

	t.Run("archive", func(t *testing.T) {
		t.Parallel()
		dir := writeSessionProject(t, basicGraphTaskfile)
		r1 := newSessionRun(t, dir, "archived", func(cfg *SessionConfig) { cfg.Archive = true })
		require.NoError(t, r1.run(context.Background()))
		assert.False(t, sessionFileExists(dir, "archived"))
		assert.True(t, archivedSessionFileExists(dir, "archived"))

		r2 := newSessionRun(t, dir, "archived", func(cfg *SessionConfig) { cfg.Archive = true })
		err := r2.run(context.Background())
		var ince *taskerrors.SessionIncompatibleError
		require.ErrorAs(t, err, &ince)
		assert.Equal(t, session.ReasonArchived, ince.Reason)
	})
}

// Without the session entry point no session state is created and Task keeps
// its original behavior (commands run again).
func TestNoSessionMeansOriginalBehavior(t *testing.T) {
	t.Parallel()
	dir := writeSessionProject(t, basicGraphTaskfile)

	runOnce := func() {
		var buf bytes.Buffer
		e := NewExecutor(WithDir(dir), WithStdout(&buf), WithStderr(&buf))
		require.NoError(t, e.Setup())
		require.NoError(t, e.Run(context.Background(), &Call{Task: "default", Vars: ast.NewVars()}))
	}
	runOnce()
	runOnce()
	for _, name := range []string{"a", "b", "c", "default"} {
		assert.Equal(t, 2, countLogLines(t, dir, name+".log", "run"))
	}
	_, err := os.Stat(filepath.Join(dir, ".task", "sessions"))
	assert.True(t, os.IsNotExist(err), "no sessions directory must be created")
}

// CLI control flags such as --silent must not be part of node identity: a
// session resumed with different flags reuses the same nodes.
func TestSessionReuseAcrossCLIFlags(t *testing.T) {
	t.Parallel()
	dir := writeSessionProject(t, basicGraphTaskfile)

	r1 := newSessionRun(t, dir, "flags")
	r1.e.Silent = true
	require.NoError(t, r1.run(context.Background()))
	assert.Equal(t, 1, countLogLines(t, dir, "a.log", "run"))

	r2 := newSessionRun(t, dir, "flags")
	r2.e.Verbose = true
	require.NoError(t, r2.run(context.Background()))
	assert.Contains(t, r2.output(), "reusing result")
	assert.Equal(t, 1, countLogLines(t, dir, "a.log", "run"))
}

// --force must bypass session reuse while still recording node state.
func TestSessionForceRerunsNodes(t *testing.T) {
	t.Parallel()
	dir := writeSessionProject(t, basicGraphTaskfile)
	r1 := newSessionRun(t, dir, "force")
	require.NoError(t, r1.run(context.Background()))

	var buf bytes.Buffer
	e := NewExecutor(
		WithDir(dir),
		WithSessionConfig(&SessionConfig{ID: "force"}),
		WithForceAll(true),
		WithStdout(&buf),
		WithStderr(&buf),
	)
	require.NoError(t, e.Setup())
	require.NoError(t, e.Run(context.Background(), &Call{Task: "default", Vars: ast.NewVars()}))
	for _, name := range []string{"a", "b", "c", "default"} {
		assert.Equal(t, 2, countLogLines(t, dir, name+".log", "run"))
	}
	assert.Equal(t, session.StatusCompleted, loadSessionFile(t, dir, "force").Status)
}
