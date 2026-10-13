package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompatibleExternalPropertiesRequireLoadedParity(t *testing.T) {
	raw := []byte("[Unit]\nWants=network-online.target\nWants=network-online.target\n[Service]\nExecStart=/usr/bin/true x\nKillSignal=SIGINT\nNoNewPrivileges=true\nPrivateTmp=false\nProtectHome=read-only\nProtectSystem=strict\n")
	u, err := parseExternalLaunchSources([][]byte{raw}, "comfyui")
	if err != nil {
		t.Fatal(err)
	}
	observed := map[string]string{"Wants": "network-online.target", "KillSignal": "2", "NoNewPrivileges": "yes", "PrivateTmp": "no", "ProtectHome": "read-only", "ProtectSystem": "strict"}
	if err := CheckLoadedLaunchProperties(observed, u.loadedProperties); err != nil {
		t.Fatal(err)
	}
	for key := range observed {
		old := observed[key]
		observed[key] = "private-secret"
		err := CheckLoadedLaunchProperties(observed, u.loadedProperties)
		if !errors.Is(err, ErrLaunchChanged) || !strings.Contains(err.Error(), key) || strings.Contains(err.Error(), "private-secret") {
			t.Fatal(err)
		}
		observed[key] = old
	}
	for _, directive := range []string{"[Unit]\nWants=other-runtime.service\nWants=network-online.target", "[Unit]\nWants=other-runtime.service\nWants=\n", "[Unit]\nRequires=other-runtime.service", "[Service]\nRestart=on-failure", "[Service]\nKillSignal=SIGKILL", "[Service]\nNoNewPrivileges=false", "[Service]\nPrivateTmp=disconnected", "[Service]\nProtectHome=false", "[Service]\nProtectSystem=false"} {
		if _, err := parseExternalLaunchSources([][]byte{append(append([]byte{}, raw...), []byte(directive+"\n")...)}, "comfyui"); !errors.Is(err, ErrLaunchUnsupported) {
			t.Fatalf("unsupported directive accepted: %v", err)
		}
	}
	u, err = parseExternalLaunchSources([][]byte{raw, []byte("[Unit]\nWants=\n")}, "comfyui")
	if err != nil || u.loadedProperties["Wants"] != "network-online.target" {
		t.Fatal("empty dependency assignment changed accumulated dependencies", err)
	}
}

func TestWorkingDirectoryLiteralAndHomeUseSameTrust(t *testing.T) {
	home := t.TempDir()
	good := filepath.Join(home, "good")
	bad := filepath.Join(home, "bad")
	link := filepath.Join(home, "link")
	for _, p := range []string{good, bad} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(bad, 0775); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	for _, spec := range []bool{false, true} {
		for _, name := range []string{"good", "bad", "link", "missing"} {
			value := filepath.Join(home, name)
			if spec {
				value = "%h/" + name
			}
			u, err := parseExternalLaunchSourcesWithAccountHome([][]byte{[]byte("[Service]\nExecStart=/usr/bin/true x\nWorkingDirectory=" + value + "\n")}, "comfyui", false, func() (string, error) { return home, nil })
			if (err == nil) != (name == "good") {
				t.Fatalf("path qualification differs: specifier=%v case=%s: %v", spec, name, err)
			}
			if err == nil {
				if u.workingDirectory != good || CheckLoadedLaunchProperties(map[string]string{"WorkingDirectory": good}, u.loadedProperties) != nil {
					t.Fatal("exact directory evidence missing")
				}
				if CheckLoadedLaunchProperties(map[string]string{"WorkingDirectory": home}, u.loadedProperties) == nil {
					t.Fatal("directory drift accepted")
				}
			}
		}
	}
}

func TestComfyCompatibleDirectoriesPreserveCommandAndRejectUntrustedPaths(t *testing.T) {
	script := comfyScriptFixture(t)
	dir := t.TempDir()
	for _, flag := range []string{"--output-directory", "--temp-directory", "--user-directory"} {
		command := "/usr/bin/python3 " + script + " " + flag + " " + dir + " --disable-auto-launch"
		found, err := inspectAutomaticLaunch([]byte("[Service]\nExecStart="+command+"\n"), "comfyui", fixtureExecutableValidator)
		if err != nil || found.Command != command || found.Endpoint != "http://127.0.0.1:8188" {
			t.Fatal("compatible directory or original command lost", err)
		}
		if err := os.Chmod(dir, 0775); err != nil {
			t.Fatal(err)
		}
		_, err = inspectAutomaticLaunch([]byte("[Service]\nExecStart="+command+"\n"), "comfyui", fixtureExecutableValidator)
		if !errors.Is(err, ErrLaunchUnsupported) || !strings.Contains(err.Error(), flag) || strings.Contains(err.Error(), dir) {
			t.Fatal("untrusted directory accepted or disclosed", err)
		}
		os.Chmod(dir, 0700)
	}
	_, _, err := comfyLaunchAddress([]string{"--private-secret=value"})
	if err == nil || strings.Contains(err.Error(), "private-secret") {
		t.Fatal("unknown option exposed", err)
	}
}

func TestComfyConfigurationReferencesAndManagerPreserveArguments(t *testing.T) {
	script := comfyScriptFixture(t)
	dir := t.TempDir()
	config := filepath.Join(dir, "paths.yaml")
	if err := os.WriteFile(config, []byte("fixture:\n  checkpoints: /synthetic/model/path\n"), 0600); err != nil {
		t.Fatal(err)
	}
	command := "/usr/bin/python3 " + script + " --enable-manager --extra-model-paths-config " + config + " " + config + " --extra-model-paths-config " + config + " --disable-auto-launch"
	raw := []byte("[Service]\nExecStart=" + command + "\n")
	found, err := inspectAutomaticLaunch(raw, "comfyui", fixtureExecutableValidator)
	if err != nil || found.Command != command {
		t.Fatal("configuration or manager arguments changed", err)
	}
	for _, mode := range []string{"missing", "writable", "link", "directory"} {
		target := filepath.Join(dir, mode)
		switch mode {
		case "writable":
			os.WriteFile(target, []byte("fixture"), 0666)
			os.Chmod(target, 0666)
		case "link":
			os.Symlink(config, target)
		case "directory":
			os.Mkdir(target, 0700)
		}
		_, _, err := comfyLaunchAddress([]string{"--extra-model-paths-config", target})
		if !errors.Is(err, ErrLaunchUnsupported) || !strings.Contains(err.Error(), "--extra-model-paths-config") || strings.Contains(err.Error(), target) {
			t.Fatal("untrusted configuration accepted or disclosed", err)
		}
	}
	if _, _, err := comfyLaunchAddress([]string{"--extra-model-paths-config", "--enable-manager"}); err == nil {
		t.Fatal("empty configuration list accepted")
	}
}

func TestComfyIndependentPathFailuresAreAggregatedAndCleanupSkipsAvailability(t *testing.T) {
	script := comfyScriptFixture(t)
	base := t.TempDir()
	unit := filepath.Join(base, "comfy.service")
	command := "/usr/bin/python3 " + script + " --output-directory " + base + "/missing-output --temp-directory " + base + "/missing-temp --user-directory " + base + "/missing-user --extra-model-paths-config " + base + "/missing-config"
	if err := os.WriteFile(unit, []byte("[Service]\nExecStart="+command+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	found, err := inspectAutomaticLaunchSourcesMode(unit, "comfyui", nil, fixtureExecutableValidator, false)
	if err == nil || found.Endpoint != "" {
		t.Fatal("bad paths produced usable binding")
	}
	for _, flag := range []string{"--output-directory", "--temp-directory", "--user-directory", "--extra-model-paths-config"} {
		if !strings.Contains(err.Error(), flag) {
			t.Fatal("independent path failure hidden", err)
		}
	}
	if strings.Contains(err.Error(), base) {
		t.Fatal("path disclosed", err)
	}
	found, err = inspectAutomaticLaunchSourcesMode(unit, "comfyui", nil, fixtureExecutableValidator, true)
	if err != nil || found.Command != command {
		t.Fatal("cleanup stranded by unavailable data paths", err)
	}
	if _, _, err := comfyLaunchAddressMode([]string{"--output-directory", "relative"}, true); err == nil {
		t.Fatal("cleanup broadened path syntax")
	}
	if _, _, err := comfyLaunchAddress(append([]string{"--extra-model-paths-config"}, make([]string, 33)...)); err == nil || !strings.Contains(err.Error(), "count") {
		t.Fatal("unbounded configuration inspection", err)
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
