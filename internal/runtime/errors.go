package runtime

import (
	"context"
	"errors"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

// SafeError preserves inspectable causes without rendering untrusted diagnostics.
func SafeError(message string, causes ...error) error { return &safeError{message, causes} }

type safeError struct {
	message string
	causes  []error
}

func (e *safeError) Error() string   { return e.message }
func (e *safeError) Unwrap() []error { return e.causes }

// errBindingCommand marks a binding check whose command failed: the
// systemctl call of the launch binding or the nvidia-smi query of the GPU
// identity. It proves no change.
var errBindingCommand = errors.New("a binding check command failed")

// errBindingRead marks a binding check whose file read failed with an I/O
// error other than ENOENT, ELOOP or ENOTDIR. It proves no change.
var errBindingRead = errors.New("a binding check read failed")

func bindingReadFailure(err error) bool {
	var errno unix.Errno
	return errors.As(err, &errno) && !definiteFileErrno(err)
}

func definiteFileErrno(err error) bool {
	return errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR)
}

func markBindingRead(failure, cause error) error {
	if errors.Is(cause, errBindingRead) || bindingReadFailure(cause) {
		return SafeError(failure.Error(), failure, errBindingRead)
	}
	return failure
}

// DefiniteBindingFailure reports a launch binding that is proven changed or
// unsupported. A failed command, read or context proves nothing.
func DefiniteBindingFailure(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var group *launchDiagnosticError
	if errors.As(err, &group) {
		return slices.ContainsFunc(group.items, DefiniteBindingFailure)
	}
	return !errors.Is(err, errBindingCommand) && !errors.Is(err, errBindingRead) && !bindingReadFailure(err)
}

// dropInPaths gives the listed drop-ins of systemctl show output.
func dropInPaths(values map[string][]string) []string {
	var paths []string
	for _, value := range values["DropInPaths"] {
		paths = append(paths, strings.Fields(value)...)
	}
	return paths
}

func single(values map[string][]string, name string) (string, bool) {
	items := values[name]
	if len(items) != 1 {
		return "", false
	}
	return items[0], true
}
