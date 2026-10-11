package task

import (
	"fmt"
	"path/filepath"

	"github.com/go-task/task/v3/internal/fingerprint"
	"github.com/go-task/task/v3/internal/logger"
	"github.com/go-task/task/v3/internal/session"
	"github.com/go-task/task/v3/taskfile/ast"
)

// initSession opens (or creates) the resumable session bound to this
// invocation's root Taskfile and entry calls. It is only called when resume is
// enabled and the run is not a dry/watch run.
func (e *Executor) initSession(calls ...*Call) error {
	entrypoint, err := filepath.Abs(e.Entrypoint)
	if err != nil {
		return err
	}

	sessionCalls := make([]session.Call, len(calls))
	for i, c := range calls {
		sessionCalls[i] = session.Call{Task: c.Task}
	}

	id := session.SessionID(entrypoint, sessionCalls)
	dir := filepath.Join(e.TempDir.Fingerprint, session.DirName, id)

	store, err := session.Open(dir, entrypoint, sessionCalls)
	if err != nil {
		return err
	}
	e.session = store

	if store.Corrupt() {
		e.Logger.VerboseErrf(logger.Yellow,
			"task: discarded unusable resume progress for this invocation; running all tasks\n")
	} else {
		e.Logger.VerboseErrf(logger.Magenta,
			"task: resume enabled for this invocation (%s)\n", id)
	}
	return nil
}

// consumeSession deletes the session's persisted state after the whole
// invocation finished successfully. A failed or interrupted run keeps it so
// the next invocation can resume.
func (e *Executor) consumeSession() {
	if e.session == nil {
		return
	}
	if err := e.session.Consume(); err != nil {
		e.Logger.VerboseErrf(logger.Yellow, "task: unable to remove resume progress: %v\n", err)
	}
}

// resumeFingerprint builds the content identity of one compiled task
// invocation. The key derived from it only matches progress recorded for the
// same Taskfile definition, the same resolved input variables and the same
// source-file contents. ok is false when the inputs cannot be determined
// reliably (e.g. a sources glob fails); such a task is simply executed
// normally and contributes no resume records.
func (e *Executor) resumeFingerprint(call *Call, t *ast.Task) (fp *session.Fingerprint, ok bool) {
	origTask, err := e.GetTask(call)
	if err != nil {
		return nil, false
	}

	fp = &session.Fingerprint{
		Taskfile:    t.Location.Taskfile,
		Task:        t.FullName,
		Dir:         t.Dir,
		Run:         t.Run,
		Vars:        map[string]string{},
		Env:         map[string]string{},
		Method:      t.Method,
		If:          t.If,
		Set:         append([]string(nil), t.Set...),
		Shopt:       append([]string(nil), t.Shopt...),
		Platforms:   platformFingerprints(t.Platforms),
		Failfast:    t.Failfast,
		IgnoreError: t.IgnoreError,
		Interactive: t.Interactive,
		Status:      append([]string(nil), t.Status...),
	}

	// Declared input variables. Only explicitly declared layers take part, so
	// volatile ambient environment variables cannot invalidate the record.
	varLayers := []*ast.Vars{
		e.Taskfile.Vars,
		origTask.IncludeVars,
		origTask.IncludedTaskfileVars,
		origTask.Vars,
		call.Vars,
	}
	for k := range declaredKeys(varLayers...) {
		if v, found := t.Vars.Get(k); found {
			fp.Vars[k] = sessionVarValue(v)
		}
	}

	// Explicitly declared environment variables, resolved to their final values.
	envLayers := []*ast.Vars{e.Taskfile.Env, origTask.Env}
	for k := range declaredKeys(envLayers...) {
		if v, found := t.Env.Get(k); found {
			fp.Env[k] = sessionVarValue(v)
		} else if v, found := t.Vars.Get(k); found {
			fp.Env[k] = sessionVarValue(v)
		}
	}

	for _, c := range t.Cmds {
		if c.Defer {
			fp.HasDefer = true
		}
		fp.Cmds = append(fp.Cmds, session.CmdFingerprint{
			Cmd:         c.Cmd,
			Task:        c.Task,
			If:          c.If,
			Vars:        sessionVars(c.Vars),
			Set:         append([]string(nil), c.Set...),
			Shopt:       append([]string(nil), c.Shopt...),
			Platforms:   platformFingerprints(c.Platforms),
			IgnoreError: c.IgnoreError,
			Defer:       c.Defer,
			TimeoutMS:   c.Timeout.Milliseconds(),
		})
	}

	for _, d := range t.Deps {
		fp.Deps = append(fp.Deps, session.DepFingerprint{
			Task:      d.Task,
			Vars:      sessionVars(d.Vars),
			TimeoutMS: d.Timeout.Milliseconds(),
		})
	}

	for _, s := range t.Sources {
		fp.Sources = append(fp.Sources, session.GlobFingerprint{Glob: s.Glob, Negate: s.Negate})
	}
	for _, g := range t.Generates {
		fp.Generates = append(fp.Generates, session.GlobFingerprint{Glob: g.Glob, Negate: g.Negate})
	}

	for _, p := range t.Preconditions {
		fp.Preconditions = append(fp.Preconditions, session.PreconditionFingerprint{
			Sh:  p.Sh,
			Msg: p.Msg,
		})
	}

	// Bind the actual source-file contents (not just the globs) so a changed
	// source invalidates the record even when the task does not use the
	// checksum fingerprinting method.
	if len(t.Sources) > 0 {
		checksum, err := fingerprint.NewChecksumChecker(e.TempDir.Fingerprint, e.Dry).Value(t)
		if err != nil {
			return nil, false
		}
		fp.SourcesChecksum, _ = checksum.(string)
	}

	return fp, true
}

// resumeCmdIsRecordable reports whether a command that just returned without
// error genuinely ran and may therefore be marked complete. Commands skipped
// because their `if` predicate was false or their platform did not match must
// be re-evaluated on resume and are not recorded.
func resumeCmdIsRecordable(cmd *ast.Cmd) bool {
	if cmd.If != "" {
		return false
	}
	if cmd.Cmd != "" && !shouldRunOnCurrentPlatform(cmd.Platforms) {
		return false
	}
	return true
}

// markSessionComplete persists a completed step, logging rather than failing
// the task when the record cannot be written: the actual work already
// succeeded; a missing record only causes the step to run again next time.
func (e *Executor) markSessionComplete(key string) {
	if e.session == nil {
		return
	}
	if err := e.session.Complete(key); err != nil {
		e.Logger.VerboseErrf(logger.Yellow, "task: unable to save resume progress: %v\n", err)
	}
}

func declaredKeys(layers ...*ast.Vars) map[string]struct{} {
	keys := map[string]struct{}{}
	for _, layer := range layers {
		for k := range layer.Keys() {
			keys[k] = struct{}{}
		}
	}
	return keys
}

func platformFingerprints(platforms []*ast.Platform) []session.PlatformFingerprint {
	if len(platforms) == 0 {
		return nil
	}
	out := make([]session.PlatformFingerprint, 0, len(platforms))
	for _, p := range platforms {
		out = append(out, session.PlatformFingerprint{OS: p.OS, Arch: p.Arch})
	}
	return out
}

func sessionVars(vars *ast.Vars) map[string]string {
	if vars == nil {
		return nil
	}
	m := map[string]string{}
	for k, v := range vars.All() {
		m[k] = sessionVarValue(v)
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

func sessionVarValue(v ast.Var) string {
	switch {
	case v.Live != nil:
		return fmt.Sprint(v.Live)
	case v.Value != nil:
		return fmt.Sprint(v.Value)
	case v.Sh != nil:
		return *v.Sh
	default:
		return ""
	}
}
