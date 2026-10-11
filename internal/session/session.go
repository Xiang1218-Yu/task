// Package session persists progress for a single resumable Task invocation.
//
// When a Task run is interrupted (for example, a CI job is terminated while a
// dependency chain is still executing), the progress that was already written
// to disk lets the next invocation skip only the dependencies and commands
// that genuinely completed. Failed, unfinished, stale (inputs changed) or
// corrupt records are never skipped: they simply do not match on resume and
// are therefore executed again.
//
// A step is only recorded after it completed successfully, so a process killed
// in the middle of a command never leaves a record for that command.
package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/zeebo/xxh3"
)

const (
	// DirName is the directory (under the Task temporary directory) that holds
	// all resumable session state.
	DirName = "session"
	// StateFileName is the name of the file that stores a session's progress.
	StateFileName = "state.json"
	// Version is the schema version of the persisted state. A state written by
	// a different version is discarded rather than trusted.
	Version = 1
)

// Call identifies one of the entry tasks a Task invocation was started with.
// Only the task name takes part in the session identity: whether a specific
// step may be skipped is decided by its content fingerprint, which already
// binds the resolved input variables and source files. This lets completed
// dependencies shared between invocations with different entry variables be
// reused as long as the dependency itself is identical.
type Call struct {
	Task string `json:"task"`
}

// Step records the outcome of a single completed unit of work.
type Step struct {
	CompletedAt time.Time `json:"completed_at"`
}

// State is the persisted progress of one Task invocation.
type State struct {
	// Version is the schema version, used to reject records this binary cannot
	// interpret.
	Version int `json:"version"`
	// Entrypoint is the absolute path to the root Taskfile the session belongs
	// to. It is validated when the state is loaded.
	Entrypoint string `json:"entrypoint"`
	// Calls are the entry tasks (and their variables) the session was started
	// for. A different invocation gets a different session, so completed work
	// cannot leak across unrelated Task calls.
	Calls []Call `json:"calls"`
	// CreatedAt and UpdatedAt are informational; validity never depends on
	// timestamps.
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
	Steps     map[string]Step `json:"steps"`
}

// PlatformFingerprint identifies an OS/arch restriction.
type PlatformFingerprint struct {
	OS   string `json:"os,omitempty"`
	Arch string `json:"arch,omitempty"`
}

// CmdFingerprint is the compiled identity of a single task command.
type CmdFingerprint struct {
	Cmd         string                `json:"cmd,omitempty"`
	Task        string                `json:"task,omitempty"`
	If          string                `json:"if,omitempty"`
	Vars        map[string]string     `json:"vars,omitempty"`
	Set         []string              `json:"set,omitempty"`
	Shopt       []string              `json:"shopt,omitempty"`
	Platforms   []PlatformFingerprint `json:"platforms,omitempty"`
	IgnoreError bool                  `json:"ignore_error,omitempty"`
	Defer       bool                  `json:"defer,omitempty"`
	TimeoutMS   int64                 `json:"timeout_ms,omitempty"`
}

// DepFingerprint is the compiled identity of a single task dependency.
type DepFingerprint struct {
	Task      string            `json:"task"`
	Vars      map[string]string `json:"vars,omitempty"`
	TimeoutMS int64             `json:"timeout_ms,omitempty"`
}

// PreconditionFingerprint is the compiled identity of a precondition.
type PreconditionFingerprint struct {
	Sh  string `json:"sh"`
	Msg string `json:"msg,omitempty"`
}

// GlobFingerprint identifies one resolved sources/generates pattern.
type GlobFingerprint struct {
	Glob   string `json:"glob"`
	Negate bool   `json:"negate,omitempty"`
}

// Fingerprint is the curated content that uniquely identifies one execution of
// a task with its resolved input variables and source-file contents. Every
// field is derived from the Taskfile, declared input variables or source
// files, so a change to any of them produces a different key and the stale
// progress is no longer matched.
type Fingerprint struct {
	// Taskfile is the location of the file defining the task.
	Taskfile string `json:"taskfile"`
	// Task is the full (namespaced, wildcard-expanded) task name.
	Task string `json:"task"`
	// Dir is the working directory the task runs in.
	Dir string `json:"dir"`
	Run string `json:"run,omitempty"`

	// Vars are the resolved values of the variables declared by the Taskfile,
	// the task, includes or the call. Process environment variables that were
	// not explicitly declared are deliberately excluded so volatile values
	// (PWD, SHLVL, ...) do not invalidate the record.
	Vars map[string]string `json:"vars,omitempty"`
	// Env are the resolved values of explicitly declared environment variables.
	Env map[string]string `json:"env,omitempty"`

	If          string                `json:"if,omitempty"`
	Method      string                `json:"method,omitempty"`
	Set         []string              `json:"set,omitempty"`
	Shopt       []string              `json:"shopt,omitempty"`
	Platforms   []PlatformFingerprint `json:"platforms,omitempty"`
	Failfast    bool                  `json:"failfast,omitempty"`
	IgnoreError bool                  `json:"ignore_error,omitempty"`
	Interactive bool                  `json:"interactive,omitempty"`

	Cmds []CmdFingerprint `json:"cmds,omitempty"`
	Deps []DepFingerprint `json:"deps,omitempty"`

	Sources         []GlobFingerprint `json:"sources,omitempty"`
	SourcesChecksum string            `json:"sources_checksum,omitempty"`
	Generates       []GlobFingerprint `json:"generates,omitempty"`

	Status        []string                  `json:"status,omitempty"`
	Preconditions []PreconditionFingerprint `json:"preconditions,omitempty"`

	// HasDefer is set when the task owns a deferred command. Such tasks are
	// only resumed as a whole, because deferred commands run as the task is
	// exiting and cannot be retried independently.
	HasDefer bool `json:"has_defer,omitempty"`
}

// Store owns the persisted state of one session. All operations are safe for
// concurrent use, as dependency chains run tasks in parallel.
type Store struct {
	mu      sync.Mutex
	dir     string
	path    string
	state   *State
	corrupt bool
}

// Open loads the session stored in dir, or starts a fresh one when none exists.
// A corrupt, unreadable or incompatible record is moved aside and replaced by
// a fresh session; Store.Corrupt reports whether that happened so callers can
// log that stale progress was discarded.
func Open(dir, entrypoint string, calls []Call) (*Store, error) {
	s := &Store{
		dir:  dir,
		path: filepath.Join(dir, StateFileName),
	}

	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		s.state = newState(entrypoint, calls)
		return s, nil
	}
	if err != nil {
		// Unreadable (permissions, I/O error): quarantine the file rather than
		// risk trusting a partial read.
		s.state = newState(entrypoint, calls)
		s.corrupt = true
		_ = s.quarantine()
		return s, nil
	}

	var st State
	if err := json.Unmarshal(data, &st); err != nil ||
		st.Version != Version ||
		st.Entrypoint != entrypoint ||
		!callsEqual(st.Calls, calls) {
		s.state = newState(entrypoint, calls)
		s.corrupt = true
		if err := s.quarantine(); err != nil {
			return nil, err
		}
		return s, nil
	}

	if st.Steps == nil {
		st.Steps = map[string]Step{}
	}
	s.state = &st
	return s, nil
}

// Corrupt reports whether a previous state file was discarded because it was
// corrupt or incompatible.
func (s *Store) Corrupt() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.corrupt
}

// IsComplete reports whether the step identified by key completed in a
// previous run with matching inputs.
func (s *Store) IsComplete(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.state.Steps[key]
	return ok
}

// Complete records that the step identified by key finished successfully and
// persists the state immediately, so the record survives the process being
// killed at any later point.
func (s *Store) Complete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.state.Steps[key]; ok {
		return nil
	}
	now := time.Now().UTC()
	s.state.Steps[key] = Step{CompletedAt: now}
	s.state.UpdatedAt = now
	return s.saveLocked()
}

// Consume deletes the session state once the whole invocation finished
// successfully. The recovery record is not carried into later, healthy runs.
// Empty parent directories (the session id directory and the session root) are
// removed as well; non-empty or unrelated directories are left untouched.
func (s *Store) Consume() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	// os.Remove only succeeds on empty directories, so this is safe even when
	// other sessions still exist.
	_ = os.Remove(s.dir)
	_ = os.Remove(filepath.Dir(s.dir))
	return nil
}

// Clear removes every resumable session under the Task temporary directory.
func Clear(tempDir string) error {
	err := os.RemoveAll(filepath.Join(tempDir, DirName))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// saveLocked atomically replaces the state file. The caller must hold s.mu.
func (s *Store) saveLocked() error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}

	// A unique temporary file keeps concurrent (serialized) saves safe on
	// platforms where rename cannot replace an existing destination.
	tmp, err := os.CreateTemp(s.dir, ".state-*.json.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path)
}

// quarantine moves an unusable state file aside instead of deleting it,
// preserving it for inspection.
func (s *Store) quarantine() error {
	if _, err := os.Stat(s.path); err != nil {
		return nil
	}
	target := filepath.Join(s.dir, fmt.Sprintf("state.corrupt-%d.json", time.Now().UnixNano()))
	return os.Rename(s.path, target)
}

func newState(entrypoint string, calls []Call) *State {
	now := time.Now().UTC()
	return &State{
		Version:    Version,
		Entrypoint: entrypoint,
		Calls:      cloneCalls(calls),
		CreatedAt:  now,
		UpdatedAt:  now,
		Steps:      map[string]Step{},
	}
}

// SessionID derives the directory name for an invocation. The identity binds
// the root Taskfile and the (order-independent) set of entry task names.
func SessionID(entrypoint string, calls []Call) string {
	names := make([]string, len(calls))
	for i, c := range calls {
		names[i] = c.Task
	}
	sort.Strings(names)
	data, _ := json.Marshal(struct {
		Entrypoint string   `json:"entrypoint"`
		Tasks      []string `json:"tasks"`
	}{Entrypoint: entrypoint, Tasks: names})
	return hashBytes(data)
}

// TaskKey returns the key identifying a whole task invocation.
func TaskKey(fp *Fingerprint) string {
	return "task:" + fingerprintHash(fp)
}

// CmdKey returns the key identifying a single command within a task. The
// full task fingerprint is part of the key, so changing any input, source or
// the command list invalidates every command-level record at once.
func CmdKey(fp *Fingerprint, index int) string {
	return fmt.Sprintf("%s:cmd:%d", TaskKey(fp), index)
}

func fingerprintHash(fp *Fingerprint) string {
	data, err := json.Marshal(fp)
	if err != nil {
		// Fingerprint only contains JSON-safe values; a marshal error here would
		// indicate a programming mistake. Fall back to a distinct, unstable key
		// that simply never matches an existing record.
		return "unhashable"
	}
	return hashBytes(data)
}

func hashBytes(data []byte) string {
	h := xxh3.Hash128(data)
	return fmt.Sprintf("%x%x", h.Hi, h.Lo)
}

func cloneCalls(calls []Call) []Call {
	out := make([]Call, len(calls))
	copy(out, calls)
	return out
}

func callsEqual(a, b []Call) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Task != b[i].Task {
			return false
		}
	}
	return true
}

func cloneStringMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// SortedStringMap copies m into a new map; JSON already serializes map keys in
// sorted order, but this helper documents intent and drops nil maps.
func SortedStringMap(m map[string]string) map[string]string {
	return cloneStringMap(m)
}

// SortedKeys returns the sorted keys of m, useful for deterministic logging.
func SortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
