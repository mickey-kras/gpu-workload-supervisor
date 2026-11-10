package main

import (
	"errors"
	"flag"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/operator"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

func activatedProfile(statePath string) operator.Profile {
	return operator.Profile{
		Version: 1, StatePath: statePath, ActivatedRelease: "1.0.0",
		SystemctlPath: "/usr/bin/systemctl", NvidiaSMIPath: "/usr/bin/nvidia-smi", GPUIndex: 1, CapacityHeadroomMiB: 512,
	}
}

func profileFlagSet(t *testing.T, args ...string) *flag.FlagSet {
	t.Helper()
	flags := flag.NewFlagSet("gpu-mode", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.String("state", "/default/state.db", "")
	flags.String("systemctl", "", "")
	flags.String("nvidia-smi", "", "")
	flags.Int("gpu-index", 0, "")
	flags.Uint64("capacity-headroom-mib", 0, "")
	if err := flags.Parse(args); err != nil {
		t.Fatal(err)
	}
	return flags
}

func flagValue(flags *flag.FlagSet, name string) string {
	return flags.Lookup(name).Value.String()
}

// Without flags, gpu-mode takes the runtime configuration of the activated
// profile. Equal explicit flags are accepted, also a relative -state.
func TestProfileDefaultsFillOmittedFlags(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	profile := activatedProfile(filepath.Join(dir, "state.db"))
	for _, args := range [][]string{
		nil,
		{"-state", "state.db", "-systemctl", "/usr/bin/systemctl", "-gpu-index", "1"},
	} {
		flags := profileFlagSet(t, append(args, "reconcile")...)
		if err := applyProfileDefaults(flags, "reconcile", profileLoader(profile, nil)); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if flagValue(flags, "systemctl") != "/usr/bin/systemctl" || flagValue(flags, "nvidia-smi") != "/usr/bin/nvidia-smi" ||
			flagValue(flags, "gpu-index") != "1" || flagValue(flags, "capacity-headroom-mib") != "512" {
			t.Fatalf("%v: profile defaults not applied", args)
		}
		if args == nil && flagValue(flags, "state") != profile.StatePath {
			t.Fatalf("state = %s", flagValue(flags, "state"))
		}
	}
}

// A state-changing command fails on an explicit flag that disagrees with
// the profile. verify-host keeps its explicit flags.
func TestProfileDefaultsRejectDisagreement(t *testing.T) {
	profile := activatedProfile("/state/state.db")
	for _, args := range [][]string{
		{"-systemctl", "/opt/bin/systemctl"},
		{"-nvidia-smi", "/opt/bin/nvidia-smi"},
		{"-gpu-index", "0"},
		{"-capacity-headroom-mib", "0"},
	} {
		flags := profileFlagSet(t, append(args, "switch")...)
		err := applyProfileDefaults(flags, "switch", profileLoader(profile, nil))
		if err == nil || !strings.Contains(err.Error(), "disagrees with the activated operator profile") {
			t.Fatalf("%v: %v", args, err)
		}
		flags = profileFlagSet(t, append(args, verifyHostCommand)...)
		if err := applyProfileDefaults(flags, verifyHostCommand, profileLoader(profile, nil)); err != nil {
			t.Fatalf("verify-host %v: %v", args, err)
		}
		if flagValue(flags, strings.TrimPrefix(args[0], "-")) != args[1] {
			t.Fatalf("verify-host lost the explicit %v", args)
		}
	}
}

// The profile applies only to its own state, and a missing profile keeps
// the command-line flags.
func TestProfileDefaultsKeepCommandLineFlags(t *testing.T) {
	profile := activatedProfile("/state/state.db")
	flags := profileFlagSet(t, "-state", "/other/state.db", "-systemctl", "/opt/bin/systemctl", "switch")
	if err := applyProfileDefaults(flags, "switch", profileLoader(profile, nil)); err != nil || flagValue(flags, "nvidia-smi") != "" {
		t.Fatalf("other state path: %v", err)
	}
	flags = profileFlagSet(t, "switch")
	if err := applyProfileDefaults(flags, "switch", profileLoader(operator.Profile{}, fs.ErrNotExist)); err != nil || flagValue(flags, "state") != "/default/state.db" {
		t.Fatalf("no profile: %v", err)
	}
}

// A profile that cannot be read stops a command that would need its values.
func TestProfileDefaultsWithAnUnreadableProfile(t *testing.T) {
	broken := profileLoader(operator.Profile{}, errors.New("untrusted profile"))
	for _, tc := range []struct {
		command string
		args    []string
		ok      bool
	}{
		{"switch", nil, false},
		{"switch", []string{"-state", "/state/state.db"}, false},
		{"switch", []string{"-state", "/state/state.db", "-systemctl", "/usr/bin/systemctl"}, true},
		{restoreStateCommand, []string{"-state", "/state/state.db"}, true},
		{restoreStateCommand, nil, false},
	} {
		flags := profileFlagSet(t, append(tc.args, tc.command)...)
		err := applyProfileDefaults(flags, tc.command, broken)
		if (err == nil) != tc.ok || (err != nil && !strings.Contains(err.Error(), "operator.json")) {
			t.Fatalf("%s %v: %v", tc.command, tc.args, err)
		}
	}
}

// The command reaches the runtime with the profile configuration.
func TestCommandUsesTheProfileConfiguration(t *testing.T) {
	oldArgs, oldLoad, oldStdout := os.Args, loadOperatorProfile, os.Stdout
	t.Cleanup(func() { os.Args, loadOperatorProfile, os.Stdout = oldArgs, oldLoad, oldStdout })
	statePath := filepath.Join(t.TempDir(), "state.db")
	seedCatalog(t, statePath)
	profile := activatedProfile(statePath)
	profile.ActivatedRelease = ""
	loadOperatorProfile = profileLoader(profile, nil)
	output, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	os.Stdout = output
	var got gpuruntime.SystemdConfig
	os.Args = []string{"gpu-mode", "reconcile"}
	err = runWithRuntimeFactory(func(cfg gpuruntime.SystemdConfig) (gpuruntime.Manager, error) {
		got = cfg
		return &stoppedRecoveryRuntime{}, nil
	})
	if got.SystemctlPath != profile.SystemctlPath || got.NvidiaSMIPath != profile.NvidiaSMIPath || got.GPUIndex != 1 || got.CapacityHeadroomMiB != 512 {
		t.Fatalf("runtime configuration = %+v (%v)", got, err)
	}
	os.Args = []string{"gpu-mode", "-systemctl", "/opt/bin/systemctl", "reconcile"}
	if err := runWithRuntimeFactory(nil); err == nil || !strings.Contains(err.Error(), "-systemctl") {
		t.Fatalf("disagreeing flag = %v", err)
	}
}
