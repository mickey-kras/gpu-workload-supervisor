package runtime

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestComfyCompatibleDirectoriesPreserveCommandAndRejectUntrustedPaths(t *testing.T) {
	script := comfyScriptFixture(t)
	dir := t.TempDir()
	for _, flag := range []string{"--output-directory", "--temp-directory", "--user-directory"} {
		command := "/usr/bin/python3 " + script + " " + flag + " " + dir + " --disable-auto-launch"
		found, err := inspectAutomaticLaunch([]byte("[Service]\nExecStart="+command+"\n"), "comfyui", fixtureExecutableValidator)
		if err != nil || found.Command != command || found.Endpoint != "http://127.0.0.1:8188" {
			t.Fatal("compatible directory or original command lost", err)
		}
		// Group write by own groups (umask 002) is normal: still accepted.
		if err := os.Chmod(dir, 0775); err != nil {
			t.Fatal(err)
		}
		if _, err = inspectAutomaticLaunch([]byte("[Service]\nExecStart="+command+"\n"), "comfyui", fixtureExecutableValidator); err != nil {
			t.Fatal("group-writable directory rejected", err)
		}
		// World-write is a real danger: rejected, naming the path.
		if err := os.Chmod(dir, 0777); err != nil {
			t.Fatal(err)
		}
		_, err = inspectAutomaticLaunch([]byte("[Service]\nExecStart="+command+"\n"), "comfyui", fixtureExecutableValidator)
		if !errors.Is(err, ErrLaunchUnsupported) || !strings.Contains(err.Error(), flag) || !strings.Contains(err.Error(), dir) {
			t.Fatal("world-writable directory accepted or not named", err)
		}
		os.Chmod(dir, 0700)
	}
	// Unknown options are skipped, never echoed.
	if _, _, err := comfyLaunchAddress([]string{"--private-secret=value"}); err != nil {
		t.Fatal("unknown option rejected", err)
	}
}

func TestWantsDuplicatesHaveBoundedRepresentationAndUnsafeDependenciesStayRejected(t *testing.T) {
	// Exercise a valid source near the 1MiB limit, then repeated contributing
	// sources. Duplicate dependencies must not grow retained parser state.
	raw := []byte("[Unit]\n" + strings.Repeat("Wants=network-online.target\n", 30000))
	directives := externalLaunchDirectives{scalars: map[string]string{}}
	for source := 0; source < 33; source++ {
		if err := directives.readSource(raw); err != nil {
			t.Fatal(err)
		}
		if len(directives.scalars) != 1 || directives.scalars["[Unit]Wants"] != "network-online.target" {
			t.Fatal("duplicate dependencies grew accumulated state")
		}
	}
	valid := []byte("[Unit]\nWants=\nWants=network-online.target\n[Service]\nExecStart=/usr/bin/true x\n")
	for _, unsafe := range []string{"[Unit]\nWants=other-runtime.service\n", "[Unit]\nWants=network-online.target other-runtime.service\n"} {
		_, err := parseExternalLaunchSources([][]byte{[]byte(unsafe), raw, valid}, "comfyui")
		if !errors.Is(err, ErrLaunchUnsupported) || !strings.Contains(err.Error(), "Wants") || strings.Contains(err.Error(), "other-runtime") {
			t.Fatal("earlier unsafe dependency erased or disclosed", err)
		}
	}
}
