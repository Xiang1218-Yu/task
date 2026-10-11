// Package session implements cross-process resumable execution sessions for
// task graphs.
//
// A session is an independently persisted record of one task-graph
// invocation: the Taskfile identity it was started from, the requested tasks,
// variables and arguments, and the per-node execution state (status, attempt
// count, input fingerprint and failure information).
//
// The package deliberately does NOT reuse the up-to-date machinery
// (sources/generates/checksum fingerprints or the task cache). Node reuse is
// decided solely from the session record: a node is reused only when it
// succeeded within a compatible session, its own definition fingerprint still
// matches, the fingerprints of every dependency still match and none of the
// dependencies had to be re-executed in the current run. Nodes found running
// (a crashed or interrupted process) or failed, and nodes downstream of those,
// are always re-executed.
package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	taskerrors "github.com/go-task/task/v3/errors"
)

// SchemaVersion is the version of the on-disk session file format understood
// by this package.
const SchemaVersion = 1

// Status is the lifecycle status of a whole session.
type Status string

const (
	// StatusRunning means the session is in progress or was interrupted.
	StatusRunning Status = "running"
	// StatusCompleted means every node of the graph finished successfully.
	StatusCompleted Status = "completed"
	// StatusFailed means the last run stopped with an error; the session can
	// still be resumed.
	StatusFailed Status = "failed"
	// StatusArchived means the session was terminated and moved aside; it can
	// no longer be resumed.
	StatusArchived Status = "archived"
)

// NodeStatus is the execution status of a single graph node.
type NodeStatus string

const (
	// NodeRunning means the node is executing or was left executing when the
	// process died. Such nodes are always re-executed on resume.
	NodeRunning NodeStatus = "running"
	// NodeSucceeded means the node's commands completed successfully.
	NodeSucceeded NodeStatus = "succeeded"
	// NodeFailed means the node's commands failed. Such nodes are re-executed
	// on resume.
	NodeFailed NodeStatus = "failed"
	// NodeSkipped means the node did not execute because Task's own
	// up-to-date fingerprint (sources/generates/status) reported it as
	// up-to-date. Such nodes are neither reused by the session (they are
	// re-evaluated on every resume) nor treated as unfinished work.
	NodeSkipped NodeStatus = "skipped"
)

// Terminal reports whether the status will not change anymore.
func (s Status) Terminal() bool {
	return s == StatusCompleted || s == StatusArchived
}

// Compatibility reasons returned inside SessionIncompatibleError.
const (
	ReasonArchived   = "the session was archived and can no longer be resumed"
	ReasonTaskfile   = "the Taskfile content has changed since the session was started"
	ReasonEntrypoint = "the session belongs to a different Taskfile"
	ReasonCalls      = "the requested tasks differ from the ones stored in the session"
	ReasonVariables  = "the variables passed to the session have changed"
	ReasonCLIArgs    = "the arguments after \"--\" have changed"
	ReasonSchema     = "the session was written by an unsupported version of Task"
)

// Failure records the last error observed for a session or node.
type Failure struct {
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

// Node is the persisted state of a single task-graph node.
type Node struct {
	// Key uniquely identifies a node inside a session: it is derived from the
	// task name and the effective call variables, so the same task invoked
	// with different variables becomes a different node.
	Key string `json:"key"`
	// Name is the human-readable task name (label when set).
	Name string `json:"name"`
	// Task is the fully qualified task name.
	Task string `json:"task"`
	// Deps are the node keys this node depends on. Edges are registered while
	// the graph executes, but a succeeded node always has a complete set
	// because its dependencies run before its own commands.
	Deps []string `json:"deps"`

	Status NodeStatus `json:"status"`
	// LastStatus is the terminal status of the node before the current run
	// marked it "running". It is what ReuseDecision inspects: a node is
	// reusable only when its previous execution ended as succeeded.
	LastStatus NodeStatus `json:"last_status,omitempty"`
	Attempts   int        `json:"attempts"`
	DefHash    string     `json:"def_hash"`
	InputHash  string     `json:"input_hash"`
	Failure    *Failure   `json:"failure,omitempty"`

	StartedAt time.Time `json:"started_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CallSpec identifies a task requested on the command line.
type CallSpec struct {
	Task string `json:"task"`
}

// Identity pins a session to a specific Taskfile.
type Identity struct {
	// Entrypoint is the absolute, slash-normalized location of the root
	// Taskfile.
	Entrypoint string `json:"entrypoint"`
	// ContentHash is a hash of the merged Taskfile definitions and variables.
	ContentHash string `json:"content_hash"`
}

// Session is the persisted record of one execution.
type Session struct {
	SchemaVersion int    `json:"schema_version"`
	ID            string `json:"id"`
	Status        Status `json:"status"`

	Identity Identity   `json:"identity"`
	Calls    []CallSpec `json:"calls"`
	Globals  []VarEntry `json:"globals"`
	CLIArgs  string     `json:"cli_args"`

	Nodes   map[string]*Node `json:"nodes"`
	Failure *Failure         `json:"failure,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// VarEntry is an ordered variable entry stored in a session.
type VarEntry struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// NodeInfo carries the display information about a node when execution
// starts.
type NodeInfo struct {
	Name string
	Task string
}

// CompletionAction tells Finish what to do when the whole run succeeds.
type CompletionAction int

const (
	// CompleteRetain keeps the session on disk, marked completed.
	CompleteRetain CompletionAction = iota
	// CompleteClean deletes the session from disk.
	CompleteClean
	// CompleteArchive moves the session to an archived file.
	CompleteArchive
)

// Config configures OpenOrCreate.
type Config struct {
	ID       string
	Dir      string
	Identity Identity
	Calls    []CallSpec
	Globals  []VarEntry
	CLIArgs  string
	Action   CompletionAction
}

var validID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidateID reports whether id can safely be used as a session file name.
func ValidateID(id string) bool {
	return validID.MatchString(id)
}

// Path returns the session file path for id inside dir.
func Path(dir, id string) string {
	return filepath.Join(dir, id+".json")
}

func archivePath(dir, id string) string {
	return filepath.Join(dir, id+".archived.json")
}

// CanonicalHash returns the SHA-256 hex digest of the deterministic JSON
// encoding of v. Map keys are sorted by encoding/json, so callers can pass
// maps directly.
func CanonicalHash(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// InputHash combines a node definition hash with the current input hashes of
// its dependencies. Because dependency input hashes embed their own
// dependencies' hashes, a change anywhere upstream cascades through the whole
// graph.
func InputHash(defHash string, deps map[string]string) string {
	keys := make([]string, 0, len(deps))
	for k := range deps {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, k+"="+deps[k])
	}
	h := sha256.New()
	h.Write([]byte(defHash))
	h.Write([]byte{0})
	h.Write([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(h.Sum(nil))
}

// Manager coordinates one in-process run against a persisted session. All
// exported methods are safe for concurrent use because task graph nodes can
// run in parallel.
type Manager struct {
	mu      sync.Mutex
	sess    *Session
	dir     string
	path    string
	action  CompletionAction
	reran   map[string]bool
	created bool
}

// OpenOrCreate loads the session identified by cfg.ID, or creates a new one.
// When loading an existing session, the current invocation is checked for
// compatibility and a SessionIncompatibleError is returned on any mismatch,
// before any task is allowed to run.
func OpenOrCreate(cfg Config) (*Manager, error) {
	if !ValidateID(cfg.ID) {
		return nil, &taskerrors.SessionInvalidError{
			SessionID: cfg.ID,
			Reason:    "session id must contain only letters, digits, dots, underscores and dashes, start with a letter or digit and be at most 64 characters long",
		}
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, err
	}

	m := &Manager{
		dir:    cfg.Dir,
		path:   Path(cfg.Dir, cfg.ID),
		action: cfg.Action,
		reran:  map[string]bool{},
	}

	existing, err := load(m.path)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		// An archived copy means a previous session with the same id was
		// terminated explicitly: resuming under the same id is rejected.
		if _, err := os.Stat(archivePath(cfg.Dir, cfg.ID)); err == nil {
			return nil, &taskerrors.SessionIncompatibleError{
				SessionID: cfg.ID,
				Reason:    ReasonArchived,
			}
		}
		now := time.Now().UTC()
		m.sess = &Session{
			SchemaVersion: SchemaVersion,
			ID:            cfg.ID,
			Status:        StatusRunning,
			Identity:      cfg.Identity,
			Calls:         cfg.Calls,
			Globals:       cfg.Globals,
			CLIArgs:       cfg.CLIArgs,
			Nodes:         map[string]*Node{},
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		m.created = true
		if err := m.saveLocked(); err != nil {
			return nil, err
		}
		return m, nil
	}

	if err := checkCompatible(cfg.ID, existing, cfg); err != nil {
		return nil, err
	}
	m.sess = existing
	// A resumed run starts with no knowledge about what executed *this*
	// time; the map is rebuilt as nodes execute.
	return m, nil
}

// Created reports whether a new session was created (as opposed to resumed).
func (m *Manager) Created() bool {
	return m.created
}

// ID returns the session identifier.
func (m *Manager) ID() string {
	return m.sess.ID
}

// Path returns the on-disk path of the session.
func (m *Manager) Path() string {
	return m.path
}

// MarkRunning records that a node started executing. It registers the parent
// dependency edge, increments the attempt counter and persists the running
// state immediately, so a crash leaves an explicit "running" marker rather
// than a missing or succeeded node.
func (m *Manager) MarkRunning(key string, info NodeInfo, parentKey string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if parentKey != "" {
		if parent, ok := m.sess.Nodes[parentKey]; ok {
			if !slices.Contains(parent.Deps, key) {
				parent.Deps = append(parent.Deps, key)
			}
		}
	}

	now := time.Now().UTC()
	n, ok := m.sess.Nodes[key]
	if !ok {
		n = &Node{Key: key, Name: info.Name, Task: info.Task}
		m.sess.Nodes[key] = n
	} else {
		if info.Name != "" {
			n.Name = info.Name
		}
		if info.Task != "" {
			n.Task = info.Task
		}
	}
	// Preserve the previous terminal state: on resume a succeeded node must
	// still be recognizable as succeeded for the reuse decision, even though
	// it is now technically "running" again until the decision is made.
	if n.Status != NodeRunning {
		n.LastStatus = n.Status
	}
	n.Status = NodeRunning
	n.Attempts++
	n.UpdatedAt = now
	if n.StartedAt.IsZero() {
		n.StartedAt = now
	}
	return m.saveLocked()
}

// ReuseDecision answers whether a previously succeeded node can be reused.
// The returned inputHash is the freshly computed input hash and must be passed
// to MarkSucceeded when the node executes anyway.
func (m *Manager) ReuseDecision(key, defHash string) (reuse bool, inputHash string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	n, ok := m.sess.Nodes[key]
	if !ok {
		return false, ""
	}
	inputHash = m.inputHashLocked(n, defHash)

	if n.LastStatus != NodeSucceeded {
		return false, inputHash
	}
	if n.DefHash != defHash {
		return false, inputHash
	}
	if n.InputHash != inputHash {
		return false, inputHash
	}
	// Every dependency must itself be reusable: if one had to run again, its
	// outputs were produced anew and this node must run again as well.
	for _, depKey := range n.Deps {
		if m.reran[depKey] {
			return false, inputHash
		}
		dep, ok := m.sess.Nodes[depKey]
		if !ok || dep.Status != NodeSucceeded {
			return false, inputHash
		}
	}
	return true, inputHash
}

// MarkReused records that a succeeded node was skipped in this run.
func (m *Manager) MarkReused(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	n, ok := m.sess.Nodes[key]
	if !ok {
		return nil
	}
	n.Status = NodeSucceeded
	n.Failure = nil
	n.UpdatedAt = time.Now().UTC()
	return m.saveLocked()
}

// MarkSucceeded records that a node's commands completed successfully in the
// current run.
func (m *Manager) MarkSucceeded(key, defHash, inputHash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	n, ok := m.sess.Nodes[key]
	if !ok {
		n = &Node{Key: key}
		m.sess.Nodes[key] = n
	}
	n.Status = NodeSucceeded
	n.DefHash = defHash
	n.InputHash = inputHash
	n.Failure = nil
	n.UpdatedAt = time.Now().UTC()
	m.reran[key] = true
	return m.saveLocked()
}

// MarkSkipped records that the node was not executed because Task's own
// up-to-date fingerprint reported it as current. It is never marked as a
// session-level success and will be evaluated again on resume.
func (m *Manager) MarkSkipped(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	n, ok := m.sess.Nodes[key]
	if !ok {
		n = &Node{Key: key}
		m.sess.Nodes[key] = n
	}
	n.Status = NodeSkipped
	n.Failure = nil
	n.UpdatedAt = time.Now().UTC()
	return m.saveLocked()
}

// DependencyReran reports whether any dependency of the node was executed
// again in the current run. A node whose dependency re-ran must never be
// reported as up-to-date or reused.
func (m *Manager) DependencyReran(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.sess.Nodes[key]
	if !ok {
		return false
	}
	for _, depKey := range n.Deps {
		if m.reran[depKey] {
			return true
		}
	}
	return false
}

// MarkFailed records that a node's commands failed.
func (m *Manager) MarkFailed(key string, cause error) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	n, ok := m.sess.Nodes[key]
	if !ok {
		n = &Node{Key: key}
		m.sess.Nodes[key] = n
	}
	n.Status = NodeFailed
	if cause != nil {
		n.Failure = &Failure{Message: cause.Error(), At: time.Now().UTC()}
	}
	n.UpdatedAt = time.Now().UTC()
	m.reran[key] = true
	return m.saveLocked()
}

// Finish finalizes the session after a run. A successful run is marked
// completed and retained, cleaned or archived according to the configured
// completion action. A failed run is always kept on disk so it can be
// resumed.
func (m *Manager) Finish(runErr error) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now().UTC()
	m.sess.UpdatedAt = now

	if runErr != nil {
		m.sess.Status = StatusFailed
		m.sess.Failure = &Failure{Message: runErr.Error(), At: now}
		return m.saveLocked()
	}

	// Defensive invariant: a run cannot be reported as successful while
	// nodes never finished. This prevents unfinished work from being mistaken
	// for success. Nodes skipped by the ordinary up-to-date fingerprint are
	// allowed: they did not have any pending work.
	for _, n := range m.sess.Nodes {
		if n.Status != NodeSucceeded && n.Status != NodeSkipped {
			m.sess.Status = StatusFailed
			m.sess.Failure = &Failure{
				Message: "session finished with unfinished node " + n.Name,
				At:      now,
			}
			return m.saveLocked()
		}
	}

	m.sess.Status = StatusCompleted
	m.sess.Failure = nil

	switch m.action {
	case CompleteClean:
		return os.Remove(m.path)
	case CompleteArchive:
		m.sess.Status = StatusArchived
		if err := m.saveLockedAt(archivePath(m.dir, m.sess.ID)); err != nil {
			return err
		}
		return os.Remove(m.path)
	default:
		return m.saveLocked()
	}
}

// NodeStatus returns the persisted status of a node and whether it exists.
// Mainly intended for diagnostics and tests.
func (m *Manager) NodeStatus(key string) (NodeStatus, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.sess.Nodes[key]
	if !ok {
		return "", false
	}
	return n.Status, true
}

// Snapshot returns a detached copy of the session for diagnostics.
func (m *Manager) Snapshot() *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, err := json.Marshal(m.sess)
	if err != nil {
		return nil
	}
	var out Session
	_ = json.Unmarshal(b, &out)
	return &out
}

// Remove deletes the session file (and any archived copy) for id in dir.
func Remove(dir, id string) error {
	path := Path(dir, id)
	err := os.Remove(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	archived := archivePath(dir, id)
	err2 := os.Remove(archived)
	if err2 != nil && !errors.Is(err2, fs.ErrNotExist) {
		return err2
	}
	if errors.Is(err, fs.ErrNotExist) && errors.Is(err2, fs.ErrNotExist) {
		return &taskerrors.SessionNotFoundError{SessionID: id}
	}
	return nil
}

func (m *Manager) inputHashLocked(n *Node, defHash string) string {
	deps := make(map[string]string, len(n.Deps))
	for _, depKey := range n.Deps {
		if dep, ok := m.sess.Nodes[depKey]; ok {
			deps[depKey] = dep.InputHash
		}
	}
	return InputHash(defHash, deps)
}

func checkCompatible(id string, existing *Session, cfg Config) error {
	incompatible := func(reason, detail string) error {
		return &taskerrors.SessionIncompatibleError{
			SessionID: id,
			Reason:    reason,
			Detail:    detail,
		}
	}

	if existing.Status == StatusArchived {
		return incompatible(ReasonArchived, "")
	}
	if existing.SchemaVersion > SchemaVersion {
		return incompatible(ReasonSchema, "session schema version is newer than this Task binary")
	}
	if existing.Identity.Entrypoint != cfg.Identity.Entrypoint {
		return incompatible(
			ReasonEntrypoint,
			"session Taskfile: "+existing.Identity.Entrypoint+", current: "+cfg.Identity.Entrypoint,
		)
	}
	if existing.Identity.ContentHash != cfg.Identity.ContentHash {
		return incompatible(ReasonTaskfile, "re-run with the same Taskfile or remove the session")
	}
	if !sameCalls(existing.Calls, cfg.Calls) {
		return incompatible(
			ReasonCalls,
			"session tasks: "+callsString(existing.Calls)+", current: "+callsString(cfg.Calls),
		)
	}
	if !sameVars(existing.Globals, cfg.Globals) {
		return incompatible(
			ReasonVariables,
			"session variables: "+varsString(existing.Globals)+", current: "+varsString(cfg.Globals),
		)
	}
	if existing.CLIArgs != cfg.CLIArgs {
		return incompatible(
			ReasonCLIArgs,
			"session arguments: "+quoteEmpty(existing.CLIArgs)+", current: "+quoteEmpty(cfg.CLIArgs),
		)
	}
	return nil
}

func sameCalls(a, b []CallSpec) bool {
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

func callsString(calls []CallSpec) string {
	names := make([]string, len(calls))
	for i, c := range calls {
		names[i] = c.Task
	}
	return quoteEmpty(strings.Join(names, " "))
}

func sameVars(a, b []VarEntry) bool {
	if len(a) != len(b) {
		return false
	}
	// Order is part of the recorded invocation, but compare as sets of
	// key/value pairs: variable precedence makes the CLI order irrelevant for
	// the final values.
	counts := map[string]int{}
	values := map[string]string{}
	for _, v := range a {
		counts[v.Key]++
		values[v.Key] = v.Value
	}
	for _, v := range b {
		counts[v.Key]--
		if values[v.Key] != v.Value {
			return false
		}
	}
	for _, c := range counts {
		if c != 0 {
			return false
		}
	}
	return true
}

func varsString(vars []VarEntry) string {
	parts := make([]string, len(vars))
	for i, v := range vars {
		parts[i] = v.Key + "=" + v.Value
	}
	return quoteEmpty(strings.Join(parts, " "))
}

func quoteEmpty(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func load(path string) (*Session, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	if s.Nodes == nil {
		s.Nodes = map[string]*Node{}
	}
	return &s, nil
}

// saveLocked writes the session atomically (temp file + rename). The caller
// must hold m.mu.
func (m *Manager) saveLocked() error {
	return m.saveLockedAt(m.path)
}

func (m *Manager) saveLockedAt(path string) error {
	m.sess.UpdatedAt = time.Now().UTC()
	b, err := json.MarshalIndent(m.sess, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
