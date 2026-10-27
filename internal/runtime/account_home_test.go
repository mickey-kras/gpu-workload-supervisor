package runtime

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestAccountHomeRequiresUniqueValidLocalRecord(t *testing.T) {
	good := "# account fixture\n\nother:x:1001:1001::/home/other:/bin/sh\nfixture:x:1000:1000::/home/fixture:/bin/sh\n"
	home, err := accountHomeFromPasswd([]byte(good), 1000)
	if err != nil || home != "/home/fixture" {
		t.Fatalf("valid local record unavailable: %v", err)
	}
	for _, raw := range []string{
		"other:x:1001:1001::/home/other:/bin/sh\n",
		good + "duplicate:x:1000:1000::/private-duplicate:/bin/sh\n",
		"fixture:x:1000:1000::/home/fixture\n",
		":x:1000:1000::/home/fixture:/bin/sh\n",
		"fixture:x:invalid:1000::/home/fixture:/bin/sh\n",
		"fixture:x:1000:invalid::/home/fixture:/bin/sh\n",
		strings.Repeat("x", 65537),
		strings.Repeat("x", (1<<20)+1),
	} {
		home, err := accountHomeFromPasswd([]byte(raw), 1000)
		if !errors.Is(err, ErrLaunchUnsupported) || home != "" || strings.Contains(err.Error(), "private-duplicate") {
			t.Fatalf("absent, malformed or ambiguous record acquired identity: %v", err)
		}
	}
}

// This test also runs in the CGO_ENABLED=0 release build. The child inherits
// hostile HOME/USER through its process launch, leaving the parent unchanged.
func TestReleaseAccountHomeDoesNotFallBackToEnvironment(t *testing.T) {
	if os.Getenv("SUPERVISOR_ACCOUNT_HOME_REGRESSION") == "child" {
		raw := []byte("fixture:x:1000:1000::/home/fixture:/bin/sh\n")
		home, err := accountHomeFromPasswd(raw, 1000)
		if err != nil {
			t.Fatal(err)
		}
		expanded, err := expandAccountHomePath("%h/models", home)
		if err != nil || expanded != "/home/fixture/models" {
			t.Fatal("present local account did not supply expansion")
		}
		if home, err := accountHomeFromPasswd(raw, 1001); err == nil || home != "" {
			t.Fatal("missing local account acquired environment identity")
		}
		return
	}
	child := exec.Command(os.Args[0], "-test.run=^TestReleaseAccountHomeDoesNotFallBackToEnvironment$")
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "HOME=") && !strings.HasPrefix(entry, "USER=") && !strings.HasPrefix(entry, "SUPERVISOR_ACCOUNT_HOME_REGRESSION=") {
			child.Env = append(child.Env, entry)
		}
	}
	child.Env = append(child.Env, "HOME=/private-untrusted-home", "USER=private-untrusted-account", "SUPERVISOR_ACCOUNT_HOME_REGRESSION=child")
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("account provenance regression failed: %v %s", err, output)
	}
}

func TestAccountHomeExpansionRejectsInvalidLocalHome(t *testing.T) {
	for _, home := range []string{"", "relative", "/home/../fixture", "/home/private secret", "/home/$PRIVATE"} {
		if _, err := expandAccountHomePath("%h/models", home); !errors.Is(err, ErrLaunchUnsupported) || strings.Contains(err.Error(), "PRIVATE") {
			t.Fatal("unqualified account home accepted or disclosed", err)
		}
	}
}

func TestExternalInspectionUsesOneLazyAccountHomeSnapshot(t *testing.T) {
	home := t.TempDir()
	var sources [][]byte
	for i := 0; i < 33; i++ {
		sources = append(sources, []byte("[Service]\nEnvironment=HOME=%h\nWorkingDirectory=%h\nExecStart=\nExecStart=%h/bin/vllm serve /models/example --host 127.0.0.1 --port 8080\n"))
	}
	reads := 0
	unit, err := parseExternalLaunchSourcesWithAccountHome(sources, func() (string, error) { reads++; return home, nil })
	if err != nil || reads != 1 || unit.environmentValues["HOME"] != home || unit.homeWorkingDirectory != home || !strings.HasPrefix(unit.execStart, home+"/bin/") {
		t.Fatalf("inspection did not share one home snapshot: reads=%d %v", reads, err)
	}
	reads = 0
	_, err = parseExternalLaunchSourcesWithAccountHome(sources, func() (string, error) { reads++; return "", accountHomeError() })
	if !errors.Is(err, ErrLaunchUnsupported) || reads != 1 {
		t.Fatalf("failed account reads were repeated: reads=%d %v", reads, err)
	}
	reads = 0
	_, err = parseExternalLaunchSourcesWithAccountHome([][]byte{[]byte("[Service]\nExecStart=/usr/bin/vllm serve /models/example --host 127.0.0.1 --port 8080\n")}, func() (string, error) { reads++; return "", accountHomeError() })
	if err != nil || reads != 0 {
		t.Fatalf("literal launch unnecessarily read account database: reads=%d %v", reads, err)
	}
}

func TestAccountDatabaseReadRejectsWritableAncestors(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatal(err)
	}
	path := dir + "/passwd"
	if err := os.WriteFile(path, []byte("fixture:x:1000:1000::/home/fixture:/bin/sh\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readTrustedLaunchSource(path, true); err == nil {
		t.Fatal("root-only account read accepted writable temporary ancestor")
	}
}
