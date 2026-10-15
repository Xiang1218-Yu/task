package task_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-task/task/v3"
	"github.com/go-task/task/v3/errors"
	"github.com/go-task/task/v3/internal/filepathext"
	"github.com/go-task/task/v3/taskfile/ast"
)

func runProfileTask(t *testing.T, dir, profile, taskName string, extraOpts ...task.ExecutorOption) (string, error) {
	t.Helper()
	var buff bytes.Buffer
	opts := []task.ExecutorOption{
		task.WithDir(dir),
		task.WithStdout(&buff),
		task.WithStderr(&buff),
	}
	if profile != "" {
		opts = append(opts, task.WithProfile(profile))
	}
	opts = append(opts, extraOpts...)
	e := task.NewExecutor(opts...)
	if err := e.Setup(); err != nil {
		return buff.String(), err
	}
	err := e.Run(t.Context(), &task.Call{Task: taskName})
	return buff.String(), err
}

func TestProfileNoSelectionKeepsDefaultBehavior(t *testing.T) {
	t.Parallel()

	out, err := runProfileTask(t, "testdata/profile", "", "show")
	require.NoError(t, err)
	assert.Contains(t, out, "ENV=default SHARED= PROFILE=")
	// The Taskfile-level dotenv is still loaded as usual.
	assert.Contains(t, out, "BASE_DOTENV=base-file DEV_DOTENV= LOG_LEVEL= FROM_BASE_ENV=")
}

func TestProfileVarsEnvAndDotenv(t *testing.T) {
	t.Parallel()

	out, err := runProfileTask(t, "testdata/profile", "dev", "show")
	require.NoError(t, err)
	// Vars are inherited from "base" and overridden by "dev".
	assert.Contains(t, out, "ENV=dev SHARED=from-base PROFILE=dev")
	// Both dotenv files are loaded and the profile env is applied.
	assert.Contains(t, out, "BASE_DOTENV=base-file DEV_DOTENV=dev-file LOG_LEVEL=debug FROM_BASE_ENV=base-env")

	out, err = runProfileTask(t, "testdata/profile", "test", "show")
	require.NoError(t, err)
	// "test" extends "dev" which extends "base".
	assert.Contains(t, out, "ENV=test SHARED=from-base PROFILE=test")
	assert.Contains(t, out, "DEV_DOTENV=dev-file LOG_LEVEL=debug FROM_BASE_ENV=base-env")
}

func TestProfileAppliesToDependencies(t *testing.T) {
	t.Parallel()

	out, err := runProfileTask(t, "testdata/profile", "dev", "with-dep")
	require.NoError(t, err)
	assert.Contains(t, out, "DEP_ENV=dev")
	assert.Contains(t, out, "TASK_ENV=dev")
}

func TestProfileCLIVarsOverrideDefaults(t *testing.T) {
	t.Parallel()

	var buff bytes.Buffer
	e := task.NewExecutor(
		task.WithDir("testdata/profile"),
		task.WithProfile("dev"),
		task.WithStdout(&buff),
		task.WithStderr(&buff),
	)
	require.NoError(t, e.Setup())

	// This mirrors how the CLI merges global variables after Setup: they must
	// win over profile defaults, everywhere in the dependency graph.
	globals := ast.NewVars()
	globals.Set("ENV", ast.Var{Value: "cli"})
	e.Taskfile.Vars.Merge(globals, nil)

	require.NoError(t, e.Run(t.Context(), &task.Call{Task: "with-dep"}))
	assert.Contains(t, buff.String(), "DEP_ENV=cli")
	assert.Contains(t, buff.String(), "TASK_ENV=cli")
}

func TestProfileUnknown(t *testing.T) {
	t.Parallel()

	_, err := runProfileTask(t, "testdata/profile_unknown", "ghost", "default")
	profileErr, ok := errors.AsType[*errors.ProfileNotFoundError](err)
	require.True(t, ok, "expected ProfileNotFoundError, got %v", err)
	assert.Equal(t, errors.CodeProfileNotFound, profileErr.Code())
	assert.Contains(t, err.Error(), `profile "ghost" is not defined`)
	assert.Contains(t, err.Error(), "dev")
}

func TestProfileCycle(t *testing.T) {
	t.Parallel()

	_, err := runProfileTask(t, "testdata/profile_cycle", "a", "default")
	profileErr, ok := errors.AsType[*errors.ProfileCycleError](err)
	require.True(t, ok, "expected ProfileCycleError, got %v", err)
	assert.Equal(t, errors.CodeProfileCycle, profileErr.Code())
	assert.Contains(t, err.Error(), "a -> b -> a")
}

func TestProfileIncompleteExtends(t *testing.T) {
	t.Parallel()

	_, err := runProfileTask(t, "testdata/profile_incomplete", "dev", "default")
	profileErr, ok := errors.AsType[*errors.ProfileInvalidError](err)
	require.True(t, ok, "expected ProfileInvalidError, got %v", err)
	assert.Equal(t, errors.CodeProfileInvalid, profileErr.Code())
	assert.Contains(t, err.Error(), `extends unknown profile "missing-base"`)
}

func TestProfileIncludedTaskfileRejected(t *testing.T) {
	t.Parallel()

	_, err := runProfileTask(t, "testdata/profile_included", "dev", "default")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Included Taskfiles can't have profile declarations")
}

func TestProfileChecksumStateIsolation(t *testing.T) { //nolint:paralleltest // mutates shared fixture state
	const dir = "testdata/profile_state"
	tempDir := filepathext.SmartJoin(dir, ".task")
	require.NoError(t, os.RemoveAll(tempDir))
	t.Cleanup(func() { _ = os.RemoveAll(tempDir) })

	ran := func(out string) bool { return strings.Contains(out, "ran-checksum") }

	// First run with dev executes and records state in the dev namespace.
	out, err := runProfileTask(t, dir, "dev", "build")
	require.NoError(t, err)
	assert.True(t, ran(out), "dev should run on first invocation")

	// Repeated runs converge to up-to-date.
	out, err = runProfileTask(t, dir, "dev", "build")
	require.NoError(t, err)
	assert.False(t, ran(out), "dev should be up-to-date on second invocation")

	// Switching profile must not reuse dev's completion state.
	out, err = runProfileTask(t, dir, "test", "build")
	require.NoError(t, err)
	assert.True(t, ran(out) && strings.Contains(out, "ran-checksum test"),
		"test must not reuse dev state")

	// Switching back: dev's own state is still valid.
	out, err = runProfileTask(t, dir, "dev", "build")
	require.NoError(t, err)
	assert.False(t, ran(out), "dev should still be up-to-date after switching back")

	// Without a profile the legacy, unnamespaced state is used.
	out, err = runProfileTask(t, dir, "", "build")
	require.NoError(t, err)
	assert.True(t, ran(out), "no profile must use the legacy state namespace")
	out, err = runProfileTask(t, dir, "", "build")
	require.NoError(t, err)
	assert.False(t, ran(out), "legacy namespace converges to up-to-date")

	// All three state namespaces exist independently.
	assert.FileExists(t, filepath.Join(tempDir, "checksum", "build"))
	assert.FileExists(t, filepath.Join(tempDir, "checksum", "profiles", "dev", "build"))
	assert.FileExists(t, filepath.Join(tempDir, "checksum", "profiles", "test", "build"))
}

func TestProfileTimestampStateIsolation(t *testing.T) { //nolint:paralleltest // mutates shared fixture state
	const dir = "testdata/profile_state"
	tempDir := filepathext.SmartJoin(dir, ".task")
	require.NoError(t, os.RemoveAll(tempDir))
	t.Cleanup(func() { _ = os.RemoveAll(tempDir) })

	ran := func(out string) bool { return strings.Contains(out, "ran-timestamp") }

	out, err := runProfileTask(t, dir, "dev", "build-timestamp")
	require.NoError(t, err)
	assert.True(t, ran(out), "dev should run on first invocation")

	out, err = runProfileTask(t, dir, "dev", "build-timestamp")
	require.NoError(t, err)
	assert.False(t, ran(out), "dev should be up-to-date on second invocation")

	out, err = runProfileTask(t, dir, "test", "build-timestamp")
	require.NoError(t, err)
	assert.True(t, ran(out) && strings.Contains(out, "ran-timestamp test"),
		"test must not reuse dev timestamp marker")

	out, err = runProfileTask(t, dir, "dev", "build-timestamp")
	require.NoError(t, err)
	assert.False(t, ran(out), "dev marker must survive switching to another profile")

	assert.FileExists(t, filepath.Join(tempDir, "timestamp", "profiles", "dev", "build-timestamp"))
	assert.FileExists(t, filepath.Join(tempDir, "timestamp", "profiles", "test", "build-timestamp"))
}
