package task

import (
	"fmt"
	"slices"

	"github.com/go-task/task/v3/errors"
	"github.com/go-task/task/v3/taskfile/ast"
)

// setupProfile validates the selected profile and applies it to the loaded
// Taskfile. Profile variables are layered between the Taskfile defaults and
// the command line variables: CLI variables are merged into
// e.Taskfile.Vars after Setup and therefore take precedence.
func (e *Executor) setupProfile() error {
	if e.Profile == "" {
		return nil
	}

	effective, err := e.resolveProfile(e.Profile, nil, make(map[string]*ast.Profile))
	if err != nil {
		return err
	}
	e.activeProfile = effective

	merged := ast.NewVars()
	merged.Merge(e.Taskfile.Vars, nil)
	merged.Merge(effective.Vars, nil)
	e.Taskfile.Vars = merged

	return nil
}

// resolveProfile resolves a profile and all the profiles it extends into a
// single effective profile. Parents are merged in declaration order (later
// parents win), followed by the profile itself, so a child always overrides
// the profiles it extends. chain records the active extension path for cycle
// and dangling-reference errors.
func (e *Executor) resolveProfile(
	name string,
	chain []string,
	resolved map[string]*ast.Profile,
) (*ast.Profile, error) {
	if effective, ok := resolved[name]; ok {
		return effective, nil
	}

	for i, ancestor := range chain {
		if ancestor == name {
			return nil, &errors.ProfileCycleError{Chain: append(append([]string{}, chain[i:]...), name)}
		}
	}

	profile, ok := e.Taskfile.Profiles.Get(name)
	if !ok {
		if len(chain) > 0 {
			return nil, &errors.ProfileInvalidError{
				Name:    chain[0],
				Message: fmt.Sprintf("extends unknown profile %q", name),
			}
		}
		return nil, &errors.ProfileNotFoundError{
			Name:      name,
			Available: slices.Collect(e.Taskfile.Profiles.Keys()),
		}
	}

	effective := &ast.Profile{
		Desc:   profile.Desc,
		Vars:   ast.NewVars(),
		Env:    ast.NewVars(),
		Dotenv: []string{},
	}

	currentChain := append(append([]string{}, chain...), name)

	// Parent dotenv files are collected separately: within an effective
	// dotenv list the first file wins, so the profile's own files must come
	// before those of its parents for child values to take precedence.
	var parentDotenv []string
	for _, parentName := range profile.Extends {
		parent, err := e.resolveProfile(parentName, currentChain, resolved)
		if err != nil {
			return nil, err
		}
		effective.Vars.Merge(parent.Vars, nil)
		effective.Env.Merge(parent.Env, nil)
		parentDotenv = append(parentDotenv, parent.Dotenv...)
	}

	effective.Vars.Merge(profile.Vars, nil)
	effective.Env.Merge(profile.Env, nil)
	effective.Dotenv = append(append([]string{}, profile.Dotenv...), parentDotenv...)

	resolved[name] = effective
	return effective, nil
}
