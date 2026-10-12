package runtime

import (
	"errors"
	"strings"
)

const launchDiagnosticLimit = 32

// launchProblems preserves sentinel errors while bounding public diagnostics.
// Only validator-generated messages belong here; never include command values.
type launchProblems struct {
	items      []error
	identities []error
	truncated  bool
}

func (p *launchProblems) add(err error) {
	if err == nil {
		return
	}
	p.captureIdentities(err)
	var group *launchDiagnosticError
	if errors.As(err, &group) {
		for _, item := range group.items {
			p.add(item)
		}
		p.truncated = p.truncated || group.truncated
		return
	}
	for _, item := range p.items {
		if item.Error() == err.Error() {
			return
		}
	}
	if len(p.items) < launchDiagnosticLimit-1 {
		p.items = append(p.items, err)
	} else {
		p.truncated = true
	}
}

func (p launchProblems) err() error {
	if len(p.items) == 0 {
		return nil
	}
	return &launchDiagnosticError{items: p.items, identities: p.identities, truncated: p.truncated}
}

type launchDiagnosticError struct {
	items      []error
	identities []error
	truncated  bool
}

func (e *launchDiagnosticError) Is(target error) bool {
	for _, identity := range e.identities {
		if identity == target {
			return true
		}
	}
	return false
}
func (e *launchDiagnosticError) Error() string   { return strings.Join(e.Diagnostics(), "; ") }
func (e *launchDiagnosticError) Unwrap() []error { return e.items }
func (e *launchDiagnosticError) Diagnostics() []string {
	result := make([]string, len(e.items))
	for i, item := range e.items {
		result[i] = item.Error()
	}
	if e.truncated {
		result = append(result, "Additional launch blockers exceed the diagnostic limit; resolve listed blockers and inspect again.")
	}
	return result
}

// LaunchDiagnostics returns bounded validation messages through wrapped errors.
func LaunchDiagnostics(err error) []string {
	var group *launchDiagnosticError
	if errors.As(err, &group) {
		return group.Diagnostics()
	}
	if err == nil {
		return nil
	}
	return []string{err.Error()}
}

// CombineLaunchErrors collects independent, sanitized validator failures.
func CombineLaunchErrors(errs ...error) error {
	var problems launchProblems
	for _, err := range errs {
		problems.add(err)
	}
	return problems.err()
}

func (p *launchProblems) captureIdentities(err error) {
	for _, identity := range []error{ErrGPUIdentityUnresolved, ErrModelUnavailable, ErrLaunchChanged, ErrLaunchUnsupported} {
		if errors.Is(err, identity) && !containsLaunchIdentity(p.identities, identity) {
			p.identities = append(p.identities, identity)
		}
	}
}
func containsLaunchIdentity(identities []error, target error) bool {
	for _, identity := range identities {
		if identity == target {
			return true
		}
	}
	return false
}
