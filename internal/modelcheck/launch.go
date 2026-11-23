package modelcheck

import (
	"errors"
	"fmt"
)

// ErrLaunchRefused marks a launch that a stored model check result refuses.
// The error text names the cause as a fixed code; it never holds a path,
// template text or a token.
var ErrLaunchRefused = errors.New("the model check refuses this launch")

// launchChecks are the checks that must pass before a launch. The
// behavioral scan is not available yet, so it can stay not run.
var launchChecks = checkOrder[:len(checkOrder)-1]

// AllowLaunch returns nil when r is a current passing check for the model in
// root and the launch pins the template that the check passed.
//
// root is the launch's model directory, or empty when the launch does not
// name one; templateSHA256 is the digest of the pinned --chat-template file.
// AllowLaunch does not rehash the model files: it compares the stored file
// identities and rejects any missing, extra, rewritten or replaced file.
func (r Record) AllowLaunch(root, templateSHA256 string) error {
	if err := r.AllowLaunchRecord(root, templateSHA256); err != nil {
		return err
	}
	return r.FilesUnchanged()
}

// FilesUnchanged compares the model directory with the file fingerprint of
// r: no file is missing, extra, written or replaced since the check.
func (r Record) FilesUnchanged() error {
	if err := filesUnchanged(r.Root, r.FileStats); err != nil {
		return fmt.Errorf("%w: %w", ErrLaunchRefused, err)
	}
	return nil
}

// AllowLaunchRecord is AllowLaunch without the read of the model directory.
func (r Record) AllowLaunchRecord(root, templateSHA256 string) error {
	if err := r.Validate(); err != nil {
		return fmt.Errorf("%w: the stored result is not valid", ErrLaunchRefused)
	}
	if root != "" && root != r.Root {
		return fmt.Errorf("%w: the result is for another model directory; check the model again", ErrLaunchRefused)
	}
	for i, name := range launchChecks {
		if check := r.Checks[i]; check.Name != name || check.Outcome != OutcomePassed {
			return fmt.Errorf("%w: status %s, check %s gave %s %s", ErrLaunchRefused, r.Status, check.Name, check.Outcome, check.Reason)
		}
	}
	switch {
	case templateSHA256 == "":
		return fmt.Errorf("%w: the launch pins no chat template", ErrLaunchRefused)
	case templateSHA256 != r.ChatTemplateSHA256:
		return fmt.Errorf("%w: the chat template changed after the model check; check the model again", ErrLaunchRefused)
	case len(r.FileStats) == 0:
		return fmt.Errorf("%w: the result has no file fingerprint; check the model again", ErrLaunchRefused)
	}
	return nil
}
