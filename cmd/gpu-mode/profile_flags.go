package main

import (
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strconv"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/operator"
)

const verifyHostCommand = "verify-host"

// profileFlagNames are the runtime flags that setup stores in the operator
// profile, in a fixed order for stable error messages.
var profileFlagNames = []string{"state", "systemctl", "nvidia-smi", "gpu-index", "capacity-headroom-mib"}

func profileFlagValues(p operator.Profile) map[string]string {
	return map[string]string{
		"state": p.StatePath, "systemctl": p.SystemctlPath, "nvidia-smi": p.NvidiaSMIPath,
		"gpu-index": strconv.Itoa(p.GPUIndex), "capacity-headroom-mib": strconv.FormatUint(p.CapacityHeadroomMiB, 10),
	}
}

// usesRuntime tells whether a command builds the runtime and so needs the
// trusted executables. The other commands use only -state.
func usesRuntime(command string) bool {
	switch command {
	case restoreStateCommand, pruneAuditCommand, "configure":
		return false
	}
	return true
}

// applyProfileDefaults gives each runtime flag that the command line omits
// the value of the activated operator profile. The profile applies only to
// its own state path: with -state naming another database, every flag
// comes from the command line, as without a profile. An explicit flag that
// disagrees with the profile fails, so that manual commands and automation
// never act on one state with two configurations. verify-host is the
// exception: it opens no state and qualifies candidate flags, so its
// explicit flags win. The runtime still checks every resulting path and
// executable.
func applyProfileDefaults(flags *flag.FlagSet, command string, load func() (operator.Profile, error)) error {
	explicit := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	profile, err := load()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return brokenProfile(explicit, command, err)
	}
	if explicit["state"] {
		same, err := sameStatePath(flags.Lookup("state").Value.String(), profile.StatePath)
		if err != nil || !same {
			return err
		}
	}
	return mergeProfileFlags(flags, command, explicit, profileFlagValues(profile))
}

func mergeProfileFlags(flags *flag.FlagSet, command string, explicit map[string]bool, values map[string]string) error {
	for _, name := range profileFlagNames {
		if !explicit[name] {
			if err := flags.Set(name, values[name]); err != nil {
				return fmt.Errorf("operator profile value of -%s: %w", name, err)
			}
			continue
		}
		// An explicit -state already resolved to the profile state path.
		given := flags.Lookup(name).Value.String()
		if name != "state" && command != verifyHostCommand && given != values[name] {
			return fmt.Errorf("-%s %q disagrees with the activated operator profile value %q; omit -%s to use the profile, or run setup to change the profile",
				name, given, values[name], name)
		}
	}
	return nil
}

// brokenProfile allows a profile that cannot be read only when the command
// line already names the state and, for a runtime command, systemctl. The
// limit checks still fail with the profile error, as before.
func brokenProfile(explicit map[string]bool, command string, cause error) error {
	if explicit["state"] && (explicit["systemctl"] || !usesRuntime(command)) {
		return nil
	}
	return fmt.Errorf("%s; or give -state and the runtime flags on the command line: %w", profileHint, cause)
}

// sameStatePath resolves a relative flag as deployment.Check resolves it.
func sameStatePath(flagPath, profilePath string) (bool, error) {
	path, err := filepath.Abs(flagPath)
	if err != nil {
		return false, fmt.Errorf("state path: %w", err)
	}
	return path == filepath.Clean(profilePath), nil
}

// errProfileChanged reports a profile change between reading the flags and
// taking the state lock.
var errProfileChanged = errors.New("the operator profile changed while this command waited for the state lock; run the command again")

// profilePin records the profile values that applied to the state path when
// the flags were read, or the error that made the profile unreadable.
type profilePin struct {
	values map[string]string
	err    error
}

// pinProfile returns the profile values that apply to statePath, or nil when
// there is no profile or it belongs to another state path.
func pinProfile(statePath string, load func() (operator.Profile, error)) profilePin {
	profile, err := load()
	if errors.Is(err, os.ErrNotExist) {
		return profilePin{}
	}
	if err != nil {
		return profilePin{err: err}
	}
	same, err := sameStatePath(statePath, profile.StatePath)
	if err != nil || !same {
		return profilePin{err: err}
	}
	return profilePin{values: profileFlagValues(profile)}
}

// check runs under the state lock. Setup activates under the same lock, so a
// profile that changed since the flags were read belongs to a newer
// activation. The command must not combine its old flag values with the
// newly accepted catalog.
func (p profilePin) check(statePath string, load func() (operator.Profile, error)) error {
	current := pinProfile(statePath, load)
	if p.err != nil && current.err != nil {
		// brokenProfile already required explicit flags for this command.
		return nil
	}
	if current.err != nil {
		return fmt.Errorf("%s: %w", profileHint, current.err)
	}
	if p.err != nil || !maps.Equal(p.values, current.values) {
		return errProfileChanged
	}
	return nil
}
