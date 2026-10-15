package errors

import (
	"fmt"
	"strings"
)

// ProfileNotFoundError is returned when the profile selected from the command
// line is not declared in the Taskfile.
type ProfileNotFoundError struct {
	Name      string
	Available []string
}

func (err ProfileNotFoundError) Error() string {
	if len(err.Available) == 0 {
		return fmt.Sprintf(`task: profile %q is not defined and no profiles are declared in the Taskfile`, err.Name)
	}
	return fmt.Sprintf(
		`task: profile %q is not defined. Available profiles: %s`,
		err.Name, strings.Join(err.Available, ", "),
	)
}

func (err ProfileNotFoundError) Code() int {
	return CodeProfileNotFound
}

// ProfileCycleError is returned when profile "extends" declarations form a
// cycle.
type ProfileCycleError struct {
	Chain []string
}

func (err ProfileCycleError) Error() string {
	return fmt.Sprintf(
		`task: profile extension cycle detected: %s`,
		strings.Join(err.Chain, " -> "),
	)
}

func (err ProfileCycleError) Code() int {
	return CodeProfileCycle
}

// ProfileInvalidError is returned when a profile declaration is incomplete,
// for example when it extends a profile that does not exist.
type ProfileInvalidError struct {
	Name    string
	Message string
}

func (err ProfileInvalidError) Error() string {
	return fmt.Sprintf(`task: invalid profile %q: %s`, err.Name, err.Message)
}

func (err ProfileInvalidError) Code() int {
	return CodeProfileInvalid
}
