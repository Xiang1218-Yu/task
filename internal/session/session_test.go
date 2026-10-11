package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testFingerprint() *Fingerprint {
	return &Fingerprint{
		Taskfile: "/project/Taskfile.yml",
		Task:     "build",
		Dir:      "/project",
		Run:      "always",
		Vars:     map[string]string{"FOO": "bar"},
		Cmds: []CmdFingerprint{
			{Cmd: "echo one"},
			{Cmd: "echo two"},
		},
		Deps:            []DepFingerprint{{Task: "deps"}},
		Sources:         []GlobFingerprint{{Glob: "src/*.go"}},
		SourcesChecksum: "abc123",
	}
}

func TestTaskKeyStable(t *testing.T) {
	t.Parallel()

	fp := testFingerprint()
	assert.Equal(t, TaskKey(fp), TaskKey(testFingerprint()))
	assert.True(t, hasPrefix(t, TaskKey(fp), "task:"))
}

func TestCmdKeyUniquePerIndex(t *testing.T) {
	t.Parallel()

	fp := testFingerprint()
	k0 := CmdKey(fp, 0)
	k1 := CmdKey(fp, 1)
	assert.NotEqual(t, k0, k1)
	assert.True(t, hasPrefix(t, k0, TaskKey(fp)))
}

func TestFingerprintChanges(t *testing.T) {
	t.Parallel()

	base := TaskKey(testFingerprint())

	cases := []struct {
		name string
		mut  func(fp *Fingerprint)
	}{
		{"taskfile path", func(fp *Fingerprint) { fp.Taskfile = "/other/Taskfile.yml" }},
		{"task name", func(fp *Fingerprint) { fp.Task = "build2" }},
		{"input var value", func(fp *Fingerprint) { fp.Vars["FOO"] = "baz" }},
		{"input var added", func(fp *Fingerprint) { fp.Vars["NEW"] = "x" }},
		{"input var removed", func(fp *Fingerprint) { delete(fp.Vars, "FOO") }},
		{"sources checksum", func(fp *Fingerprint) { fp.SourcesChecksum = "def456" }},
		{"sources glob", func(fp *Fingerprint) { fp.Sources[0].Glob = "src/**/*.go" }},
		{"cmd text", func(fp *Fingerprint) { fp.Cmds[0].Cmd = "echo ONE" }},
		{"cmd count", func(fp *Fingerprint) { fp.Cmds = append(fp.Cmds, CmdFingerprint{Cmd: "echo three"}) }},
		{"dep", func(fp *Fingerprint) { fp.Deps[0].Task = "other-deps" }},
		{"dir", func(fp *Fingerprint) { fp.Dir = "/project/sub" }},
		{"preconditions", func(fp *Fingerprint) {
			fp.Preconditions = []PreconditionFingerprint{{Sh: "test -f go.mod"}}
		}},
		{"status", func(fp *Fingerprint) { fp.Status = []string{"test -f bin"} }},
		{"if predicate", func(fp *Fingerprint) { fp.If = "test -f x" }},
		{"defer", func(fp *Fingerprint) { fp.HasDefer = true }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fp := testFingerprint()
			tc.mut(fp)
			assert.NotEqual(t, base, TaskKey(fp))
		})
	}
}

func TestSessionIDStableAndBoundToInvocation(t *testing.T) {
	t.Parallel()

	calls := []Call{{Task: "build"}}
	id1 := SessionID("/project/Taskfile.yml", calls)
	id2 := SessionID("/project/Taskfile.yml", []Call{{Task: "build"}})
	assert.Equal(t, id1, id2)

	// The order of entry tasks does not matter.
	assert.Equal(t,
		SessionID("/project/Taskfile.yml", []Call{{Task: "build"}, {Task: "test"}}),
		SessionID("/project/Taskfile.yml", []Call{{Task: "test"}, {Task: "build"}}),
	)

	assert.NotEqual(t, id1, SessionID("/other/Taskfile.yml", calls))
	assert.NotEqual(t, id1, SessionID("/project/Taskfile.yml", []Call{{Task: "test"}}))
	assert.NotEqual(t, id1, SessionID("/project/Taskfile.yml", []Call{{Task: "build"}, {Task: "test"}}))
}

func TestStoreRoundtrip(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "session")
	calls := []Call{{Task: "build"}}
	entrypoint := "/project/Taskfile.yml"

	s, err := Open(dir, entrypoint, calls)
	require.NoError(t, err)
	assert.False(t, s.Corrupt())

	key := TaskKey(testFingerprint())
	assert.False(t, s.IsComplete(key))
	require.NoError(t, s.Complete(key))
	assert.True(t, s.IsComplete(key))

	// Reopening reads what was persisted.
	s2, err := Open(dir, entrypoint, calls)
	require.NoError(t, err)
	assert.True(t, s2.IsComplete(key))
	assert.False(t, s2.Corrupt())
}

func TestStoreCorruptRecordIsDiscarded(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "session")
	calls := []Call{{Task: "build"}}
	entrypoint := "/project/Taskfile.yml"

	s, err := Open(dir, entrypoint, calls)
	require.NoError(t, err)
	require.NoError(t, s.Complete(TaskKey(testFingerprint())))

	require.NoError(t, os.WriteFile(filepath.Join(dir, StateFileName), []byte("{not json"), 0o644))

	s2, err := Open(dir, entrypoint, calls)
	require.NoError(t, err)
	assert.True(t, s2.Corrupt(), "corrupt record must be reported and discarded")
	assert.False(t, s2.IsComplete(TaskKey(testFingerprint())), "steps from a corrupt record must never be trusted")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "only the quarantined file remains before the next save")
	assert.Contains(t, entries[0].Name(), "state.corrupt-")
}

func TestStoreRejectsDifferentInvocation(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "session")
	entrypoint := "/project/Taskfile.yml"

	s, err := Open(dir, entrypoint, []Call{{Task: "build"}})
	require.NoError(t, err)
	require.NoError(t, s.Complete("task:k"))

	// Same directory, but a different invocation: the record cannot be trusted
	// for this call and is discarded.
	s2, err := Open(dir, entrypoint, []Call{{Task: "deploy"}})
	require.NoError(t, err)
	assert.True(t, s2.Corrupt())
	assert.False(t, s2.IsComplete("task:k"))
}

func TestStoreRejectsDifferentVersion(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "session")
	calls := []Call{{Task: "build"}}
	entrypoint := "/project/Taskfile.yml"

	// A well-formed state from a hypothetical future version.
	state := State{
		Version:    Version + 1,
		Entrypoint: entrypoint,
		Calls:      calls,
		Steps:      map[string]Step{"task:k": {}},
	}
	s := newState(entrypoint, calls)
	s.Version = Version + 1
	s.Steps = state.Steps
	require.NoError(t, os.MkdirAll(dir, 0o755))
	data, err := json.Marshal(s)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, StateFileName), data, 0o644))

	s2, err := Open(dir, entrypoint, calls)
	require.NoError(t, err)
	assert.True(t, s2.Corrupt())
	assert.False(t, s2.IsComplete("task:k"))
}

func TestConsumeRemovesState(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	dir := filepath.Join(base, DirName, "id")
	calls := []Call{{Task: "build"}}

	s, err := Open(dir, base, calls)
	require.NoError(t, err)
	require.NoError(t, s.Complete("task:k"))
	require.NoError(t, s.Consume())

	_, statErr := os.Stat(filepath.Join(dir, StateFileName))
	assert.True(t, os.IsNotExist(statErr))

	// Consuming again is not an error (idempotent cleanup).
	assert.NoError(t, s.Consume())
}

func TestClearRemovesAllSessions(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(tempDir, DirName, "a"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(tempDir, DirName, "b"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tempDir, DirName, "a", StateFileName), []byte("{}"), 0o644))
	// Unrelated Task state (checksums) must survive.
	require.NoError(t, os.MkdirAll(filepath.Join(tempDir, "checksum"), 0o755))

	require.NoError(t, Clear(tempDir))

	_, err := os.Stat(filepath.Join(tempDir, DirName))
	assert.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(tempDir, "checksum"))
	assert.NoError(t, err)

	// Clearing a missing directory is fine.
	assert.NoError(t, Clear(tempDir))
}

func hasPrefix(t *testing.T, s, prefix string) bool {
	t.Helper()
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
