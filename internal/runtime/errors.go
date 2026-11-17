package runtime

// SafeError preserves inspectable causes without rendering untrusted diagnostics.
func SafeError(message string, causes ...error) error { return &safeError{message, causes} }

type safeError struct {
	message string
	causes  []error
}

func (e *safeError) Error() string   { return e.message }
func (e *safeError) Unwrap() []error { return e.causes }
