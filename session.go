package task

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-task/task/v3/errors"
	"github.com/go-task/task/v3/internal/logger"
	"github.com/go-task/task/v3/internal/session"
	"github.com/go-task/task/v3/taskfile/ast"
)

// SessionConfig configures a resumable execution session.
//
// A session persists the state of a whole task-graph run on disk under the
// Task temporary directory. When the same invocation is started again with
// the same session ID, completed nodes whose inputs did not change are
// skipped, while interrupted, failed or upstream-affected nodes are
// re-executed. Sessions are entirely independent of the up-to-date
// fingerprinting machinery (sources/generates/checksum): when no session is
// configured, Task behaves exactly as it would without this feature.
type SessionConfig struct {
	// ID is the unique identifier of the session. It becomes part of a file
	// name, so only letters, digits, dots, underscores and dashes are
	// allowed.
	ID string
	// Clean removes the session file when the whole run completes
	// successfully.
	Clean bool
	// Archive moves the session file aside (marks it archived) when the
	// whole run completes successfully.
	Archive bool
	// Globals are the variables passed on the command line (NAME=value).
	Globals map[string]string
	// CLIArgs are the arguments after "--", quoted as a single string.
	CLIArgs string
}

// sessionConfigOption wires a (possibly nil) SessionConfig into an Executor.
type sessionConfigOption struct {
	cfg *SessionConfig
}

// WithSessionConfig enables resumable execution using the given
// SessionConfig. A nil config or an empty ID leaves the Executor unchanged.
func WithSessionConfig(cfg *SessionConfig) ExecutorOption {
	return &sessionConfigOption{cfg: cfg}
}

func (o *sessionConfigOption) ApplyToExecutor(e *Executor) {
	if o.cfg != nil && o.cfg.ID != "" {
		e.sessionCfg = o.cfg
	}
}

// sessionParentKey carries the node key of the currently executing parent
// through the context, so dependency and task-call edges can be registered
// without changing the RunTask signature.
type sessionParentKey struct{}

// sessionsDir is the directory holding session files for the current
// Taskfile. It is intentionally separate from the checksum/cache directory
// contents.
func (e *Executor) sessionsDir() string {
	return filepath.Join(e.TempDir.Fingerprint, "sessions")
}

// beginSession creates or resumes the configured session, verifying that the
// current invocation is compatible with the stored one.
func (e *Executor) beginSession(calls []*Call) error {
	cfg := e.sessionCfg

	identity, err := e.sessionIdentity()
	if err != nil {
		return err
	}

	callSpecs := make([]session.CallSpec, 0, len(calls))
	for _, c := range calls {
		callSpecs = append(callSpecs, session.CallSpec{Task: c.Task})
	}

	globals := make([]session.VarEntry, 0, len(cfg.Globals))
	for k, v := range cfg.Globals {
		globals = append(globals, session.VarEntry{Key: k, Value: v})
	}
	sort.Slice(globals, func(i, j int) bool { return globals[i].Key < globals[j].Key })

	action := session.CompleteRetain
	switch {
	case cfg.Clean:
		action = session.CompleteClean
	case cfg.Archive:
		action = session.CompleteArchive
	}

	mgr, err := session.OpenOrCreate(session.Config{
		ID:       cfg.ID,
		Dir:      e.sessionsDir(),
		Identity: identity,
		Calls:    callSpecs,
		Globals:  globals,
		CLIArgs:  cfg.CLIArgs,
		Action:   action,
	})
	if err != nil {
		return err
	}
	e.session = mgr

	snap := mgr.Snapshot()
	actionVerb := "Resuming"
	if mgr.Created() {
		actionVerb = "Starting"
	}
	var succeeded, pending int
	for _, n := range snap.Nodes {
		if n.Status == session.NodeSucceeded {
			succeeded++
		} else {
			pending++
		}
	}
	e.Logger.VerboseOutf(
		logger.Magenta,
		"task: %s session %q: %d node(s) already completed, %d pending\n",
		actionVerb, cfg.ID, succeeded, pending,
	)
	return nil
}

// finishSession marks the session as completed or failed and applies the
// configured cleanup action. It is safe to call with a nil manager.
func (e *Executor) finishSession(runErr error) error {
	if e.session == nil {
		return nil
	}
	return e.session.Finish(runErr)
}

// RemoveSession removes a persisted (or archived) session by ID.
func (e *Executor) RemoveSession(id string) error {
	if !session.ValidateID(id) {
		return &errors.SessionInvalidError{
			SessionID: id,
			Reason:    "session id must contain only letters, digits, dots, underscores and dashes and be at most 64 characters long",
		}
	}
	return session.Remove(e.sessionsDir(), id)
}

// sessionNodeIdentity computes the per-node key and the variables and
// definition hashes used to decide whether a node can be reused.
func (e *Executor) sessionNodeIdentity(t *ast.Task, call *Call) (key, defHash string, err error) {
	varsHash, err := e.sessionVarsHash(t, call)
	if err != nil {
		return "", "", err
	}
	defHash, err = e.sessionDefHash(t)
	if err != nil {
		return "", "", err
	}
	key = fmt.Sprintf("%s#%s#%s", t.Location.Taskfile, t.FullName, varsHash[:12])
	return key, defHash, nil
}

// sessionVarsHash hashes the effective variables that originate from the
// Taskfile or the call. Variables inherited from the process environment are
// excluded so that a resume from a different shell does not look like an
// input change, while the volatile TASK_EXE special variable is always
// excluded. Taskfile-declared variables are force-included even when they
// shadow an environment variable.
func (e *Executor) sessionVarsHash(t *ast.Task, call *Call) (string, error) {
	envNames := map[string]bool{}
	for _, entry := range os.Environ() {
		if k, _, ok := strings.Cut(entry, "="); ok {
			envNames[k] = true
		}
	}

	declared := map[string]bool{}
	for k := range e.Taskfile.Vars.Keys() {
		declared[k] = true
	}
	if raw, err := e.GetTask(call); err == nil && raw != nil {
		for k := range raw.Vars.Keys() {
			declared[k] = true
		}
		for k := range raw.IncludeVars.Keys() {
			declared[k] = true
		}
		for k := range raw.IncludedTaskfileVars.Keys() {
			declared[k] = true
		}
	}
	if call.Vars != nil {
		for k := range call.Vars.Keys() {
			declared[k] = true
		}
	}

	values := map[string]any{}
	for k, v := range t.Vars.All() {
		if k == "TASK_EXE" {
			continue
		}
		// CLI control flags (silent/verbose/force/...) and CLI_ARGS describe
		// the invocation rather than the node definition: flags must not
		// change node identity, and the arguments after "--" are compared
		// explicitly at the session compatibility check.
		if sessionSpecialVars[k] {
			continue
		}
		if envNames[k] && !declared[k] {
			continue
		}
		values[k] = v.Value
	}
	return session.CanonicalHash(values)
}

// sessionDefHash hashes the command and dependency definition of a compiled
// task. It does not consult sources, generates or the checksum cache.
func (e *Executor) sessionDefHash(t *ast.Task) (string, error) {
	cmds := make([]any, 0, len(t.Cmds))
	for _, c := range t.Cmds {
		if c == nil {
			continue
		}
		cmds = append(cmds, map[string]any{
			"cmd":          c.Cmd,
			"task":         c.Task,
			"if":           c.If,
			"defer":        c.Defer,
			"silent":       c.Silent,
			"ignore_error": c.IgnoreError,
			"timeout_ns":   int64(c.Timeout),
			"set":          c.Set,
			"shopt":        c.Shopt,
			"vars":         varsValues(c.Vars),
			"platforms":    platformsValues(c.Platforms),
		})
	}
	deps := make([]any, 0, len(t.Deps))
	for _, d := range t.Deps {
		if d == nil {
			continue
		}
		deps = append(deps, map[string]any{
			"task":       d.Task,
			"vars":       varsValues(d.Vars),
			"silent":     d.Silent,
			"timeout_ns": int64(d.Timeout),
		})
	}
	return session.CanonicalHash(map[string]any{
		"task":      t.FullName,
		"dir":       t.Dir,
		"if":        t.If,
		"run":       cmpOr(t.Run, e.Taskfile.Run),
		"set":       t.Set,
		"shopt":     t.Shopt,
		"cmds":      cmds,
		"deps":      deps,
		"platforms": platformsValues(t.Platforms),
	})
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func varsValues(vars *ast.Vars) map[string]any {
	if vars == nil {
		return nil
	}
	out := make(map[string]any, vars.Len())
	for k, v := range vars.All() {
		out[k] = v.Value
	}
	return out
}

func platformsValues(platforms []*ast.Platform) []map[string]string {
	if len(platforms) == 0 {
		return nil
	}
	out := make([]map[string]string, 0, len(platforms))
	for _, p := range platforms {
		if p == nil {
			continue
		}
		out = append(out, map[string]string{"os": p.OS, "arch": p.Arch})
	}
	return out
}

// sessionSpecialVars are injected by the CLI for every invocation. They are
// not part of the Taskfile identity (CLI flags and arguments are compared
// separately by the session) and changing, say, verbosity must not invalidate
// a session.
var sessionSpecialVars = map[string]bool{
	"CLI_ARGS":       true,
	"CLI_ARGS_LIST":  true,
	"CLI_FORCE":      true,
	"CLI_SILENT":     true,
	"CLI_VERBOSE":    true,
	"CLI_OFFLINE":    true,
	"CLI_ASSUME_YES": true,
}

// sessionIdentity hashes the merged Taskfile definition so that any change to
// commands, dependencies or declared variables forces a compatibility error
// on resume. Variables passed on the command line are excluded here because
// they are compared explicitly as part of the session invocation record.
func (e *Executor) sessionIdentity() (session.Identity, error) {
	entrypoint := e.Entrypoint
	if abs, err := filepath.Abs(entrypoint); err == nil {
		entrypoint = filepath.ToSlash(abs)
	}

	tasks := map[string]any{}
	for name, t := range e.Taskfile.Tasks.All(nil) {
		tasks[name] = rawTaskDefinition(t)
	}

	content, err := session.CanonicalHash(map[string]any{
		"version":       e.Taskfile.Version.String(),
		"method":        e.Taskfile.Method,
		"run":           e.Taskfile.Run,
		"interval_ns":   int64(e.Taskfile.Interval),
		"silent":        e.Taskfile.Silent,
		"set":           e.Taskfile.Set,
		"shopt":         e.Taskfile.Shopt,
		"dotenv":        e.Taskfile.Dotenv,
		"use_gitignore": e.Taskfile.UseGitignore,
		"output": []string{
			e.Taskfile.Output.Name,
			e.Taskfile.Output.Group.Begin,
			e.Taskfile.Output.Group.End,
		},
		"vars":  e.identityVarsDefinition(e.Taskfile.Vars),
		"env":   rawVarsDefinition(e.Taskfile.Env),
		"tasks": tasks,
	})
	if err != nil {
		return session.Identity{}, err
	}
	return session.Identity{Entrypoint: entrypoint, ContentHash: content}, nil
}

// identityVarsDefinition snapshots Taskfile variables for the identity hash,
// excluding CLI-provided globals and injected CLI_* variables.
func (e *Executor) identityVarsDefinition(vars *ast.Vars) []map[string]any {
	if vars == nil {
		return nil
	}
	skipped := map[string]bool{}
	for k := range sessionSpecialVars {
		skipped[k] = true
	}
	if e.sessionCfg != nil {
		for k := range e.sessionCfg.Globals {
			skipped[k] = true
		}
	}
	filtered := ast.NewVars()
	for k, v := range vars.All() {
		if !skipped[k] {
			filtered.Set(k, v)
		}
	}
	return rawVarsDefinition(filtered)
}

func rawVarsDefinition(vars *ast.Vars) []map[string]any {
	if vars == nil {
		return nil
	}
	out := make([]map[string]any, 0, vars.Len())
	for k, v := range vars.All() {
		sh := ""
		if v.Sh != nil {
			sh = *v.Sh
		}
		out = append(out, map[string]any{
			"key":   k,
			"value": v.Value,
			"sh":    sh,
			"ref":   v.Ref,
			"dir":   v.Dir,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["key"].(string) < out[j]["key"].(string) })
	return out
}

func rawTaskDefinition(t *ast.Task) map[string]any {
	cmds := make([]any, 0, len(t.Cmds))
	for _, c := range t.Cmds {
		if c == nil {
			continue
		}
		cmds = append(cmds, map[string]any{
			"cmd":          c.Cmd,
			"task":         c.Task,
			"if":           c.If,
			"defer":        c.Defer,
			"silent":       c.Silent,
			"ignore_error": c.IgnoreError,
			"timeout_ns":   int64(c.Timeout),
			"set":          c.Set,
			"shopt":        c.Shopt,
			"vars":         rawVarsDefinition(c.Vars),
			"platforms":    platformsValues(c.Platforms),
			"for":          rawFor(c.For),
		})
	}
	deps := make([]any, 0, len(t.Deps))
	for _, d := range t.Deps {
		if d == nil {
			continue
		}
		deps = append(deps, map[string]any{
			"task":       d.Task,
			"vars":       rawVarsDefinition(d.Vars),
			"silent":     d.Silent,
			"timeout_ns": int64(d.Timeout),
			"for":        rawFor(d.For),
		})
	}
	preconditions := make([]any, 0, len(t.Preconditions))
	for _, p := range t.Preconditions {
		if p == nil {
			continue
		}
		preconditions = append(preconditions, map[string]any{"sh": p.Sh, "msg": p.Msg})
	}
	sources := make([]string, 0, len(t.Sources))
	for _, g := range t.Sources {
		if g != nil {
			sources = append(sources, g.Glob)
		}
	}
	generates := make([]string, 0, len(t.Generates))
	for _, g := range t.Generates {
		if g != nil {
			generates = append(generates, g.Glob)
		}
	}
	return map[string]any{
		"cmds":          cmds,
		"deps":          deps,
		"dir":           t.Dir,
		"if":            t.If,
		"run":           t.Run,
		"method":        t.Method,
		"prefix":        t.Prefix,
		"label":         t.Label,
		"aliases":       t.Aliases,
		"internal":      t.Internal,
		"interactive":   t.Interactive,
		"silent":        t.Silent,
		"ignore_error":  t.IgnoreError,
		"watch":         t.Watch,
		"failfast":      t.Failfast,
		"prompt":        t.Prompt,
		"set":           t.Set,
		"shopt":         t.Shopt,
		"dotenv":        t.Dotenv,
		"status":        t.Status,
		"sources":       sources,
		"generates":     generates,
		"platforms":     platformsValues(t.Platforms),
		"preconditions": preconditions,
		"vars":          rawVarsDefinition(t.Vars),
		"env":           rawVarsDefinition(t.Env),
		"include_vars":  rawVarsDefinition(t.IncludeVars),
		"included_vars": rawVarsDefinition(t.IncludedTaskfileVars),
	}
}

func rawFor(f *ast.For) map[string]any {
	if f == nil {
		return nil
	}
	return map[string]any{
		"from":  f.From,
		"list":  f.List,
		"var":   f.Var,
		"split": f.Split,
		"as":    f.As,
	}
}

// sessionParentFromContext returns the node key whose dependencies are
// currently running.
func sessionParentFromContext(ctx context.Context) string {
	parent, _ := ctx.Value(sessionParentKey{}).(string)
	return parent
}
