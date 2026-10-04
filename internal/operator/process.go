package operator

import (
	"os/signal"
	"syscall"
)

// DetachSession runs before admission. Failure is fail-closed: an operation must
// not begin in the UI's process group. Session/logout SIGKILL policies remain
// outside this guarantee and require interrupted-transition recovery.
func DetachSession() error {
	signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE)
	_, err := syscall.Setsid()
	return err
}
