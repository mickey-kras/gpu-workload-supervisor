package runtime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/user"
	"strconv"
	"strings"
	"testing"
)

func TestLaunchDiagnosticsBoundAndPreserveSentinels(t *testing.T) {
	var problems launchProblems
	for i := 0; i < 100; i++ {
		problems.add(fmt.Errorf("%w: blocker %d", ErrLaunchUnsupported, i))
	}
	problems.add(ErrGPUIdentityUnresolved)
	err := problems.err()
	if !errors.Is(err, ErrGPUIdentityUnresolved) || len(LaunchDiagnostics(err)) != launchDiagnosticLimit || !strings.Contains(err.Error(), "diagnostic limit") {
		t.Fatal(err)
	}
	var outer launchProblems
	outer.add(fmt.Errorf("context: %w", err))
	if !errors.Is(outer.err(), ErrGPUIdentityUnresolved) || len(LaunchDiagnostics(outer.err())) != launchDiagnosticLimit {
		t.Fatal(outer.err())
	}
}

func TestAccountHomeExpansionUsesAccountDatabaseAndRejectsOtherSyntax(t *testing.T) {
	account, err := user.LookupId(strconv.Itoa(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", "/private-untrusted-home")
	for _, value := range []string{"%h/application", "HOME=%h", "PATH=%h/bin:/usr/bin", "/usr/bin/python3 %h/ComfyUI/main.py", "/usr/bin/python3\t%h/ComfyUI/main.py\t--disable-auto-launch"} {
		got, err := expandAccountHome(value)
		if err != nil || got != strings.ReplaceAll(value, "%h", account.HomeDir) {
			t.Fatalf("%q: %q %v", value, got, err)
		}
	}
	for _, value := range []string{"%H/path", "%%h/path", "prefix%h/path", "%h-suffix", "HOME=$HOME", "%h/path/%u"} {
		if !strings.Contains(value, "%") {
			continue
		}
		if _, err := expandAccountHome(value); err == nil {
			t.Fatalf("expanded unsupported syntax: %q", value)
		}
	}
	if err := CheckLoadedAccountHomeDirectory("/different", account.HomeDir); err == nil || strings.Contains(err.Error(), "/different") {
		t.Fatal(err)
	}
	if err := CheckLoadedAccountHomeDirectory(account.HomeDir, account.HomeDir); err != nil {
		t.Fatal(err)
	}
}

func TestEffectiveWorkingDirectoryResetClearsSpecifierBinding(t *testing.T) {
	workingDirectory := t.TempDir()
	unit, err := parseExternalLaunchSources([][]byte{[]byte("[Service]\nExecStart=/usr/bin/true x\nWorkingDirectory=%h\n"), []byte("[Service]\nWorkingDirectory=" + workingDirectory + "\n")})
	if err != nil || unit.homeWorkingDirectory != "" {
		t.Fatalf("stale specifier binding: %+v %v", unit, err)
	}
}

func TestInvalidLaunchHasNoUsableBindingAndCollectsCommandBlockers(t *testing.T) {
	path := t.TempDir() + "/external.service"
	raw := []byte("[Service]\nExecStart=/usr/bin/vllm serve /missing-model --host 127.0.0.1 --port 8080\nEnvironment=CUDA_VISIBLE_DEVICES=0\nRestart=on-failure\n")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	found, err := inspectAutomaticLaunchSourcesWithValidator(path, "vllm", nil, func(string) error { return errors.New("executable trust is unavailable") })
	if !errors.Is(err, ErrModelUnavailable) || len(LaunchDiagnostics(err)) < 2 {
		t.Fatal(err)
	}
	if found.Command != "" || found.Endpoint != "" || found.SHA256 != "" || found.Environment != nil || found.DropIns != nil {
		t.Fatalf("partial launch escaped error: %+v", found)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil || string(after) != string(raw) {
		t.Fatal("inspection changed source", readErr)
	}
}

func TestAccountHomeSpecifierEffectiveEnvironmentAndCommand(t *testing.T) {
	account, err := user.LookupId(strconv.Itoa(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", "/private-untrusted-home")
	raw := []byte("[Service]\nExecStart=%h/bin/vllm serve %h/models/example --host 127.0.0.1 --port 8080\nEnvironment=HOME=%h\nWorkingDirectory=%h\n")
	if err := qualifyEnvironmentPath(account.HomeDir, false); err != nil {
		t.Skip("OS account home is not a qualified fixture path")
	}
	unit, err := parseExternalLaunchSources([][]byte{raw})
	if err != nil {
		t.Fatal(err)
	}
	if unit.environmentValues["HOME"] != account.HomeDir || unit.homeWorkingDirectory != account.HomeDir || !strings.HasPrefix(unit.execStart, account.HomeDir+"/bin/") {
		t.Fatal("account home resolution mismatch")
	}
	if _, err := parseLaunchUnit(raw); err == nil {
		t.Fatal("owned grammar unexpectedly expanded specifier")
	}
}

func TestDiagnosticsDoNotEchoUnqualifiedCommandTokens(t *testing.T) {
	_, err := parseServerCommand([]string{"/usr/bin/vllm", "--private-credential-value", "unused"}, "vllm")
	if err == nil || strings.Contains(err.Error(), "private-credential-value") {
		t.Fatal(err)
	}
}

func TestQuotedEnvironmentWhitespaceCannotInventAssignments(t *testing.T) {
	for _, quote := range []string{"\"", "'"} {
		path := t.TempDir() + "/external.service"
		raw := []byte("[Service]\nExecStart=/usr/bin/vllm serve /models/example --host 127.0.0.1 --port 8080\nEnvironment=" + quote + "CUDA_VISIBLE_DEVICES=0 HOME=/tmp" + quote + "\n")
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		found, err := inspectAutomaticLaunchSourcesWithValidator(path, "vllm", nil, func(string) error { return nil })
		if err == nil || found.Endpoint != "" || found.Command != "" || found.Environment != nil || !strings.Contains(err.Error(), "whitespace") {
			t.Fatalf("quoted whitespace acquired usable launch: %+v %v", found, err)
		}
		unit, _ := parseExternalLaunchSources([][]byte{raw})
		if unit.gpuUUID != "" || len(unit.environmentValues) != 0 {
			t.Fatal("quoted assignment invented runtime settings")
		}
	}
}

func TestExternalExpansionUsesOnlyEffectiveCommandsAndScalars(t *testing.T) {
	workingDirectory := t.TempDir()
	sources := [][]byte{
		[]byte("[Service]\nExecStart=%u/unsupported\nWorkingDirectory=%u/unsupported\n"),
		[]byte("[Service]\nExecStart=\nExecStart=/usr/bin/vllm serve /models/example --host 127.0.0.1 --port 8080\nWorkingDirectory=" + workingDirectory + "\n"),
	}
	unit, err := parseExternalLaunchSources(sources)
	if err != nil || unit.execStart != "/usr/bin/vllm serve /models/example --host 127.0.0.1 --port 8080" || unit.homeWorkingDirectory != "" {
		t.Fatalf("obsolete directives blocked effective launch: %+v %v", unit, err)
	}
}

func TestOvercountPreparationSkipsAllExecutableValidation(t *testing.T) {
	path := t.TempDir() + "/external.service"
	raw := "[Service]\nExecStart=/usr/bin/vllm serve /models/example --host 127.0.0.1 --port 8080\n" + strings.Repeat("ExecStartPre=/usr/bin/test -f /private-predicate\n", 33)
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	preparationCalls := 0
	_, err := inspectAutomaticLaunchSourcesWithValidator(path, "vllm", nil, func(path string) error {
		if path == "/usr/bin/test" {
			preparationCalls++
		}
		return nil
	})
	if err == nil || preparationCalls != 0 || strings.Contains(err.Error(), "private-predicate") {
		t.Fatalf("overcount preparation performed unbounded validation: %d %v", preparationCalls, err)
	}
}

func TestInvalidPreparationDoesNotHideIndependentMissingModel(t *testing.T) {
	path := t.TempDir() + "/external.service"
	raw := []byte("[Service]\nExecStart=/usr/bin/vllm serve /nonexistent-supervisor-fixture/example --host 127.0.0.1 --port 8080\nExecStartPre=/usr/bin/true extra\n")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	found, err := inspectAutomaticLaunchSourcesWithValidator(path, "vllm", nil, func(string) error { return nil })
	if !errors.Is(err, ErrModelUnavailable) || !strings.Contains(err.Error(), "ExecStartPre") || found.Command != "" || found.Endpoint != "" {
		t.Fatalf("independent model blocker hidden: %+v %v", found, err)
	}
}

func TestPublicDiagnosticCombinationPreservesIndependentErrors(t *testing.T) {
	if CombineLaunchErrors(nil, nil) != nil || LaunchDiagnostics(nil) != nil {
		t.Fatal("empty diagnostics did not remain empty")
	}
	single := errors.New("trusted validator explanation")
	if got := LaunchDiagnostics(single); len(got) != 1 || got[0] != single.Error() {
		t.Fatal(got)
	}
	combined := CombineLaunchErrors(single, fmt.Errorf("context: %w", ErrGPUIdentityUnresolved), nil, single)
	if got := LaunchDiagnostics(combined); len(got) != 2 || got[0] != single.Error() || !errors.Is(combined, ErrGPUIdentityUnresolved) {
		t.Fatal(got)
	}
}

func TestRuntimeBindingRequiresLoadedAccountHomeWorkingDirectory(t *testing.T) {
	account, err := user.LookupId(strconv.Itoa(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	if qualifyEnvironmentPath(account.HomeDir, false) != nil {
		t.Skip("OS account home is not a qualified fixture path")
	}
	manager, runner, profile, show := nativeFixture(t, "vllm", "http://127.0.0.1:9000")
	raw, err := os.ReadFile(profile.NativeModel.LaunchFile)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte("WorkingDirectory=%h\n")...)
	if err := os.WriteFile(profile.NativeModel.LaunchFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	profile.NativeModel.LaunchSHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
	showWithDirectory := strings.Replace(show, " --property=Environment ", " --property=WorkingDirectory --property=Environment ", 1)
	good := string(runner.outputs[show]) + "WorkingDirectory=" + account.HomeDir + "\n"
	runner.outputs[showWithDirectory] = []byte(good)
	if err := manager.verifyNativeBinding(context.Background(), profile); err != nil {
		t.Fatal(err)
	}
	for _, observed := range []string{"", "/private-different-directory"} {
		runner.outputs[showWithDirectory] = []byte(strings.Replace(good, "WorkingDirectory="+account.HomeDir, "WorkingDirectory="+observed, 1))
		err := manager.verifyNativeBinding(context.Background(), profile)
		if !errors.Is(err, ErrLaunchChanged) || !strings.Contains(err.Error(), "WorkingDirectory") || strings.Contains(err.Error(), "private-different-directory") {
			t.Fatal(err)
		}
	}
}
