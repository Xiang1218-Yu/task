package fingerprint

import (
	"fmt"
	"path/filepath"

	"github.com/go-task/task/v3/errors"
)

// ErrInvalidMethod lets callers that only need a fingerprint value tell a bad
// method name apart from a checker failing on the sources themselves.
var ErrInvalidMethod = errors.New("invalid method")

func NewSourcesChecker(method, tempDir string, dry bool, profile string) (SourcesCheckable, error) {
	switch method {
	case "timestamp":
		return NewTimestampChecker(tempDir, dry, profile), nil
	case "checksum":
		return NewChecksumChecker(tempDir, dry, profile), nil
	case "none":
		return NoneChecker{}, nil
	default:
		return nil, fmt.Errorf(`task: %w "%s"`, ErrInvalidMethod, method)
	}
}

// stateDir returns the directory holding persistent fingerprint state of the
// given kind ("checksum" or "timestamp"). When a profile is selected, state
// is namespaced under profiles/<profile>/ so that switching profiles can
// never reuse another profile's completion state. Without a profile, the
// path is unchanged so existing behavior is preserved.
func stateDir(tempDir, kind, profile string) string {
	dir := filepath.Join(tempDir, kind)
	if profile != "" {
		dir = filepath.Join(dir, "profiles", normalizeFilename(profile))
	}
	return dir
}
