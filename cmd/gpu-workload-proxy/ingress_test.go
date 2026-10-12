package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	workloadproxy "github.com/mickey-kras/gpu-workload-supervisor/internal/proxy"
)

// Deliberately nonrandom fixtures: valid wire format, not production secrets.
const testExecutionCredential = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const testCompletionCredential = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func privateCredential(t *testing.T, value string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestIngressCredentialFiles(t *testing.T) {
	for _, value := range []string{testExecutionCredential, testExecutionCredential + "\n"} {
		got, err := readIngressCredential(privateCredential(t, value))
		if err != nil || got != testExecutionCredential {
			t.Fatalf("valid credential: %v", err)
		}
	}
	for _, value := range []string{"", "short", strings.Repeat("g", 64), strings.ToUpper(testExecutionCredential), testExecutionCredential + "\n\n", testExecutionCredential + " ", strings.Repeat("a", 10000)} {
		if _, err := readIngressCredential(privateCredential(t, value)); err == nil {
			t.Fatal("invalid credential accepted")
		}
	}
	path := privateCredential(t, testExecutionCredential)
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	if _, err := readIngressCredential(path); err == nil {
		t.Fatal("readable-by-group credential accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "symlink")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readIngressCredential(link); err == nil {
		t.Fatal("symlink credential accepted")
	}
	parentLink := filepath.Join(t.TempDir(), "parent")
	if err := os.Symlink(filepath.Dir(path), parentLink); err != nil {
		t.Fatal(err)
	}
	if _, err := readIngressCredential(filepath.Join(parentLink, "credential")); err == nil {
		t.Fatal("parent symlink accepted")
	}
	if _, err := readIngressCredential(filepath.Dir(path)); err == nil {
		t.Fatal("directory credential accepted")
	}
	if _, err := readIngressCredential("relative"); err == nil {
		t.Fatal("relative credential path accepted")
	}
	if _, err := readIngressCredential(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing credential accepted")
	}
}

func TestConfigureIngressFailsClosed(t *testing.T) {
	execution := privateCredential(t, testExecutionCredential)
	completion := privateCredential(t, testCompletionCredential)
	var config workloadproxy.Config
	if err := configureIngress(&config, workloadproxy.IngressTrusted, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := configureIngress(&config, workloadproxy.IngressAdapter, execution, completion); err != nil {
		t.Fatal(err)
	}
	if config.ExecutionCredential != testExecutionCredential || config.CompletionCredential != testCompletionCredential {
		t.Fatal("credentials not loaded")
	}
	for _, test := range []struct{ mode, execution, completion string }{
		{"", "", ""}, {"invalid", "", ""}, {workloadproxy.IngressTrusted, execution, ""},
		{workloadproxy.IngressAdapter, "", completion}, {workloadproxy.IngressAdapter, execution, ""},
		{workloadproxy.IngressAdapter, execution, execution}, {workloadproxy.IngressAdapter, "relative", completion},
		{workloadproxy.IngressAdapter, execution, "relative"},
	} {
		var config workloadproxy.Config
		if err := configureIngress(&config, test.mode, test.execution, test.completion); err == nil {
			t.Fatalf("unsafe mode %q accepted", test.mode)
		}
	}
}

func TestAdapterFlagsRequireCredentialsBeforeStateOpen(t *testing.T) {
	before := os.Args
	t.Cleanup(func() { os.Args = before })
	statePath := filepath.Join(t.TempDir(), "absent.db")
	for _, args := range [][]string{{"-ingress-mode", "adapter"}, {"-ingress-mode", "invalid"}, {"-execution-credential-file", "relative"}} {
		os.Args = append([]string{"gpu-workload-proxy", "-state", statePath, "-upstream", "http://127.0.0.1:1", "-workload", "media", "-execute-route", "POST:/execute"}, args...)
		if err := run(); err == nil {
			t.Fatal("unsafe startup accepted")
		}
		if _, err := os.Stat(statePath); !os.IsNotExist(err) {
			t.Fatal("invalid credentials opened state")
		}
	}
}
