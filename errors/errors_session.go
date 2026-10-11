package errors

import "fmt"

// Session-related exit codes.
const (
	CodeSessionIncompatible int = iota + 210
	CodeSessionNotFound
	CodeSessionInvalid
)

// SessionIncompatibleError is returned when an existing session cannot be
// resumed because the current invocation no longer matches the one that
// created it: the Taskfile content, the requested tasks, the variables or the
// command line arguments changed.
type SessionIncompatibleError struct {
	SessionID string
	Reason    string
	Detail    string
}

func (err *SessionIncompatibleError) Error() string {
	msg := fmt.Sprintf(
		"task: cannot resume session %q: %s",
		err.SessionID,
		err.Reason,
	)
	if err.Detail != "" {
		msg += " (" + err.Detail + ")"
	}
	return msg
}

func (err *SessionIncompatibleError) Code() int {
	return CodeSessionIncompatible
}

// SessionNotFoundError is returned when a session management action references
// a session that does not exist.
type SessionNotFoundError struct {
	SessionID string
}

func (err *SessionNotFoundError) Error() string {
	return fmt.Sprintf("task: session %q not found", err.SessionID)
}

func (err *SessionNotFoundError) Code() int {
	return CodeSessionNotFound
}

// SessionInvalidError is returned when a session identifier or its persisted
// state is invalid.
type SessionInvalidError struct {
	SessionID string
	Reason    string
}

func (err *SessionInvalidError) Error() string {
	return fmt.Sprintf("task: invalid session %q: %s", err.SessionID, err.Reason)
}

func (err *SessionInvalidError) Code() int {
	return CodeSessionInvalid
}
