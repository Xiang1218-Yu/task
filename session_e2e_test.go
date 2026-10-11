//go:build !windows

// Cross-process recovery tests. They build the real task binary, run it in a
// separate process with --session, kill it (simulating a crash or an
// interrupt signal) and start it again with the same session identifier.

package task_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-task/task/v3/internal/session"
)

var e2eBinary = sync.OnceValues(func() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	bin := filepath.Join(cwd, "bin", "task-session-e2e")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/task")
	cmd.Dir = cwd
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", &buildError{err: err, out: string(out)}
	}
	return bin, nil
})

type buildError struct {
	err error
	out string
}

func (e *buildError) Error() string { return e.err.Error() + ": " + e.out }

// Each node appends to its own log file so parallel tasks cannot interleave
// lines inside one file.
const e2eTaskfile = `version: '3'

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
      - echo start >> c.log
      - while [ ! -f gate ]; do sleep 0.1; done
      - echo done >> c.log
`

// e2eSimpleTaskfile converges without external coordination, so it is safe
// for tests that never kill the process.
const e2eSimpleTaskfile = `version: '3'

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
`

func e2eSetup(t *testing.T, taskfile string) (dir, bin string) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping cross-process e2e test in -short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("signal-based e2e tests are Unix-only")
	}
	var err error
	bin, err = e2eBinary()
	require.NoError(t, err)
	dir = t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Taskfile.yml"), []byte(taskfile), 0o644))
	return dir, bin
}

func e2eArgs(dir, sessionID string, extra ...string) []string {
	args := []string{"--session", sessionID, "--taskfile", filepath.Join(dir, "Taskfile.yml")}
	return append(args, extra...)
}

func e2eEnv() []string {
	env := os.Environ()
	env = append(env, "NO_COLOR=1")
	return env
}

func startTask(t *testing.T, bin, dir string, args ...string) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = e2eEnv()
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	// Own process group so signaling the child does not hit the test runner.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: 0}
	require.NoError(t, cmd.Start())
	return cmd, &buf
}

func waitForMarker(t *testing.T, dir, file, marker string) {
	t.Helper()
	require.Eventually(t, func() bool {
		b, err := os.ReadFile(filepath.Join(dir, file))
		return err == nil && bytes.Contains(b, []byte(marker))
	}, 15*time.Second, 20*time.Millisecond, "waiting for %q in %s", marker, file)
}

func e2eLineCount(t *testing.T, dir, file, name string) int {
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

func e2eLoadSession(t *testing.T, dir, id string) *session.Session {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, ".task", "sessions", id+".json"))
	require.NoError(t, err)
	var s session.Session
	require.NoError(t, json.Unmarshal(b, &s))
	return &s
}

func e2eNode(s *session.Session, taskName string) *session.Node {
	for _, n := range s.Nodes {
		if n.Task == taskName {
			return n
		}
	}
	return nil
}

// A process killed mid-run leaves the executing node persisted as "running".
// On resume successful nodes are reused and the interrupted part of the graph
// runs again until the whole session converges.
func TestSessionE2ECrashRecovery(t *testing.T) {
	dir, bin := e2eSetup(t, e2eTaskfile)
	const id = "e2e-crash"

	// Run 1: start, wait until "a" is done and "c" is waiting on the gate.
	cmd, _ := startTask(t, bin, dir, e2eArgs(dir, id)...)
	waitForMarker(t, dir, "c.log", "start")
	waitForMarker(t, dir, "a.log", "run")

	// Hard crash: no chance to finalize anything.
	require.NoError(t, syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL))
	_ = cmd.Wait()

	s := e2eLoadSession(t, dir, id)
	assert.Equal(t, session.StatusRunning, s.Status)
	cNode := e2eNode(s, "c")
	require.NotNil(t, cNode)
	assert.Equal(t, session.NodeRunning, cNode.Status,
		"a killed node must remain recorded as running")
	assert.Equal(t, session.NodeSucceeded, e2eNode(s, "a").Status)

	// Resume after opening the gate.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gate"), []byte("ok"), 0o644))
	out, err := runTaskBinary(bin, dir, e2eArgs(dir, id)...)
	require.NoError(t, err, "resumed run output:\n%s", out)
	assert.Contains(t, out, "reusing result")

	assert.Equal(t, 1, e2eLineCount(t, dir, "a.log", "run"), "completed node reused")
	assert.Equal(t, 2, e2eLineCount(t, dir, "c.log", "start"), "interrupted node restarted")
	assert.Equal(t, 1, e2eLineCount(t, dir, "c.log", "done"))
	assert.Equal(t, 1, e2eLineCount(t, dir, "b.log", "run"))
	assert.Equal(t, 1, e2eLineCount(t, dir, "default.log", "run"))

	s = e2eLoadSession(t, dir, id)
	assert.Equal(t, session.StatusCompleted, s.Status)
	assert.Equal(t, 2, e2eNode(s, "c").Attempts)

	// A third run with --session-clean converges and removes the state.
	out, err = runTaskBinary(bin, dir, e2eArgs(dir, id, "--session-clean")...)
	require.NoError(t, err, "cleaning run output:\n%s", out)
	assert.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(dir, ".task", "sessions", id+".json"))
		return os.IsNotExist(err)
	}, time.Second, 10*time.Millisecond)
}

// SIGINT unwinds the run through context cancellation and finalizes the
// session; a subsequent invocation resumes it.
func TestSessionE2EInterruptSignal(t *testing.T) {
	dir, bin := e2eSetup(t, e2eTaskfile)
	const id = "e2e-signal"

	cmd, _ := startTask(t, bin, dir, e2eArgs(dir, id)...)
	waitForMarker(t, dir, "c.log", "start")

	require.NoError(t, syscall.Kill(-cmd.Process.Pid, syscall.SIGINT))
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		require.Error(t, err, "an interrupted run must exit non-zero")
	case <-time.After(15 * time.Second):
		t.Fatal("interrupted process did not exit after SIGINT")
	}

	require.NoError(t, os.WriteFile(filepath.Join(dir, "gate"), []byte("ok"), 0o644))
	out, err := runTaskBinary(bin, dir, e2eArgs(dir, id)...)
	require.NoError(t, err, "resumed run output:\n%s", out)
	assert.Equal(t, 1, e2eLineCount(t, dir, "a.log", "run"))
	assert.Equal(t, 1, e2eLineCount(t, dir, "c.log", "done"))
	assert.Equal(t, session.StatusCompleted, e2eLoadSession(t, dir, id).Status)
}

// Editing the Taskfile between runs makes the session incompatible and Task
// exits with an explanatory error without executing anything.
func TestSessionE2EIncompatibleTaskfile(t *testing.T) {
	dir, bin := e2eSetup(t, e2eSimpleTaskfile)
	const id = "e2e-incompat"

	out, err := runTaskBinary(bin, dir, e2eArgs(dir, id)...)
	require.NoError(t, err, out)
	before := e2eLineCount(t, dir, "a.log", "run")
	require.Equal(t, 1, before)

	edited := strings.Replace(e2eSimpleTaskfile, "echo run >> a.log", "echo changed >> a.log", 1)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Taskfile.yml"), []byte(edited), 0o644))

	out, err = runTaskBinary(bin, dir, e2eArgs(dir, id)...)
	require.Error(t, err)
	assert.Contains(t, out, "cannot resume session")
	assert.Contains(t, out, "Taskfile content has changed")
	assert.Equal(t, before, e2eLineCount(t, dir, "a.log", "run"))
}

// --session-rm deletes the stored session.
func TestSessionE2ESessionRemove(t *testing.T) {
	dir, bin := e2eSetup(t, e2eSimpleTaskfile)
	const id = "e2e-rm"

	_, err := runTaskBinary(bin, dir, e2eArgs(dir, id, "--session-archive")...)
	require.NoError(t, err)
	require.FileExists(t, filepath.Join(dir, ".task", "sessions", id+".archived.json"))

	out, err := runTaskBinary(bin, dir,
		"--session-rm", id, "--taskfile", filepath.Join(dir, "Taskfile.yml"))
	require.NoError(t, err, out)
	assert.Contains(t, out, "removed")
	_, err = os.Stat(filepath.Join(dir, ".task", "sessions", id+".archived.json"))
	assert.True(t, os.IsNotExist(err))
}

func runTaskBinary(bin, dir string, args ...string) (string, error) {
	var buf bytes.Buffer
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = e2eEnv()
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}
