package setup

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestManagerCgroupInvokesOnlyResolvedTrustedExecutable(t *testing.T) {
	t.Setenv("PATH", "/bin")
	for _, selected := range []string{"/usr/bin/true", "/bin/true", "true"} {
		t.Run(selected, func(t *testing.T) {
			resolved, err := filepath.EvalSymlinks("/bin/true")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			run := func(gotContext context.Context, path string, args ...string) ([]byte, error) {
				calls++
				if gotContext != ctx || path != resolved || !reflect.DeepEqual(args, []string{"--user", "show", "--property=ControlGroup", "--", "-.slice"}) {
					t.Fatalf("unexpected query: context %v, path %q, args %v", gotContext, path, args)
				}
				return []byte("ControlGroup=/user.slice/user-1000.slice/user@1000.service\n"), nil
			}
			got, err := managerCgroup(ctx, run, selected)
			if err != nil || calls != 1 || got != "/user.slice/user-1000.slice/user@1000.service" {
				t.Fatalf("trusted query failed: %q, calls %d, error %v", got, calls, err)
			}
		})
	}
}

func TestManagerCgroupRejectsUnsafeExecutableBeforeRunner(t *testing.T) {
	root := t.TempDir()
	// Keep the refusal deterministic even when TMPDIR has trusted ancestors.
	if err := os.Chmod(root, 0777); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	t.Setenv("PATH", root)
	unsafe := filepath.Join(root, "systemctl")
	if err := os.WriteFile(unsafe, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	writable := filepath.Join(root, "writable")
	if err := os.WriteFile(writable, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(writable, 0777); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "trusted-target")
	if err := os.Symlink("/usr/bin/true", alias); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		path, reason string
	}{
		{"./systemctl", "absolute and clean"},
		{root + "/./systemctl", "absolute and clean"},
		{unsafe, "systemctl executable trust"},
		{"systemctl", "systemctl executable trust"},
		{writable, "world-writable"},
		{alias, "world-writable"},
		{root, "resolve systemctl executable"},
		{filepath.Join(root, "missing"), "resolve systemctl executable"},
	} {
		t.Run(tt.path, func(t *testing.T) {
			run := func(context.Context, string, ...string) ([]byte, error) {
				t.Fatal("untrusted path reached the process runner")
				return nil, nil
			}
			if _, err := managerCgroup(context.Background(), run, tt.path); err == nil || !strings.Contains(err.Error(), tt.reason) {
				t.Fatalf("missing trust refusal %q: %v", tt.reason, err)
			}
		})
	}
}
