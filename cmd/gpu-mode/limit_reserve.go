package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mickey-kras/llm-workload-supervisor/internal/operator"
	gpuruntime "github.com/mickey-kras/llm-workload-supervisor/internal/runtime"
)

// loadOperatorProfile reads the operator profile of this account. Tests
// replace it.
var loadOperatorProfile = operator.LoadProfile

// profileHint names the operator profile and the fixes of a profile that
// gpu-mode cannot read or that is not valid.
const profileHint = "the operator profile ~/.config/gpu-workload-supervisor/operator.json, or a directory above it, " +
	"cannot be read or is not valid; fix the owner, mode or content of that path, or run setup again"

// limitReserveFor returns the limit reserve that setup stored for statePath
// in this account's operator profile. A missing profile, or one for another
// state path, yields nil and the runtime uses the default. An unreadable
// profile returns an error, and the runtime then fails every limit check
// because the default reserve could be too small. A relative statePath
// resolves as in deployment.Check.
func limitReserveFor(statePath string, load func() (operator.Profile, error)) (*gpuruntime.LimitReserve, error) {
	profile, err := load()
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", profileHint, err)
	}
	path, err := filepath.Abs(statePath)
	if err != nil {
		return nil, fmt.Errorf("state path: %w", err)
	}
	if filepath.Clean(profile.StatePath) != path {
		return nil, nil
	}
	return profile.LimitReserve, nil
}
