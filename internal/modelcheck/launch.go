package modelcheck

import (
	"errors"
	"fmt"
)

// ErrLaunchRefused marks a launch refused by a stored model check record.
var ErrLaunchRefused = errors.New("the model check refuses this launch")

// AllowLaunch verifies that r passed all checks for root and matches the pinned chat template SHA-256.
func (r Record) AllowLaunch(root, templateSHA256 string) error {
	if err := r.Validate(); err != nil {
		return fmt.Errorf("%w: the stored result is not valid", ErrLaunchRefused)
	}
	if root != "" && root != r.Root {
		return fmt.Errorf("%w: the result is for another model directory; check the model again", ErrLaunchRefused)
	}
	for i, name := range checkOrder {
		if check := r.Checks[i]; check.Name != name || check.Outcome != OutcomePassed {
			return fmt.Errorf("%w: status %s, check %s gave %s %s", ErrLaunchRefused, r.Status, check.Name, check.Outcome, check.Reason)
		}
	}
	switch {
	case templateSHA256 == "":
		return fmt.Errorf("%w: the launch pins no chat template", ErrLaunchRefused)
	case r.ChatTemplateSHA256 != "" && templateSHA256 != r.ChatTemplateSHA256:
		return fmt.Errorf("%w: the chat template changed after the model check; check the model again", ErrLaunchRefused)
	}
	return nil
}
