package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnvironmentSearchPathsRejectUntrustedCodeDirectories(t *testing.T) {
	trusted := t.TempDir()
	// Group write is supported by the single-admin trust policy.
	if err := os.Chmod(trusted, 0775); err != nil {
		t.Fatal(err)
	}
	worldWritable := t.TempDir()
	if err := os.Chmod(worldWritable, 0777); err != nil {
		t.Fatal(err)
	}
	sticky := t.TempDir()
	if err := os.Chmod(sticky, 0777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "sticky-link")
	if err := os.Symlink(sticky, link); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"PATH", "LD_LIBRARY_PATH", "PYTHONPATH"} {
		t.Run(name, func(t *testing.T) {
			for _, value := range []string{trusted, trusted + ":" + trusted} {
				u := parsedLaunchUnit{}
				if err := u.applySafeEnvironment("llama.cpp", name+"="+value); err != nil {
					t.Fatalf("trusted search path rejected: %v", err)
				}
			}
			for _, value := range []string{"", ":", ":" + trusted, trusted + ":", trusted + "::" + trusted, ".", "relative", trusted + ":.", filepath.Join(trusted, "missing"), worldWritable, sticky, link, file} {
				u := parsedLaunchUnit{}
				if err := u.applySafeEnvironment("llama.cpp", name+"="+value); !errors.Is(err, ErrLaunchUnsupported) {
					t.Errorf("unsafe search path %q accepted: %v", value, err)
				}
			}
		})
	}
	// A literal directory containing ';' must not disguise the linker's actual
	// search list. Both supported separators require the same checks.
	for _, value := range []string{";" + trusted, trusted + ";", trusted + ";;" + trusted, trusted + ";" + sticky} {
		u := parsedLaunchUnit{}
		if err := u.applySafeEnvironment("llama.cpp", "LD_LIBRARY_PATH="+value); err == nil {
			t.Errorf("unsafe dynamic linker search path %q accepted", value)
		}
	}
	u := parsedLaunchUnit{}
	if err := u.applySafeEnvironment("llama.cpp", "LD_LIBRARY_PATH="+trusted+";"+trusted); err != nil {
		t.Fatal("trusted semicolon-separated library directories rejected", err)
	}
}

func TestEnvironmentRejectsUnsupportedCodeLoadingOverrides(t *testing.T) {
	for _, name := range []string{"LD_PRELOAD", "LD_AUDIT", "PYTHONHOME", "PYTHONUSERBASE", "PYTHONSTARTUP"} {
		t.Run(name, func(t *testing.T) {
			u := parsedLaunchUnit{}
			err := u.applySafeEnvironment("llama.cpp", name+"=/PRIVATE_CODE_LOCATION")
			if !errors.Is(err, ErrLaunchUnsupported) || !strings.Contains(err.Error(), name) || strings.Contains(err.Error(), "PRIVATE_CODE_LOCATION") {
				t.Fatalf("code-loading override accepted or disclosed: %v", err)
			}
			u = parsedLaunchUnit{}
			if err := u.applySafeEnvironment("llama.cpp", name+"="); err != nil {
				t.Fatal("disabled code-loading override rejected", err)
			}
		})
	}
	u := parsedLaunchUnit{}
	if err := u.applySafeEnvironment("llama.cpp", "CUSTOM_HARMLESS_SETTING=literal"); err != nil || u.environmentValues["CUSTOM_HARMLESS_SETTING"] != "literal" {
		t.Fatal("harmless unknown environment setting rejected", err)
	}
}

func TestEnvironmentSearchPathCleanupDoesNotRequireExistingDirectories(t *testing.T) {
	for _, name := range []string{"PATH", "LD_LIBRARY_PATH", "PYTHONPATH"} {
		u := parsedLaunchUnit{cleanupEnvironment: true}
		if err := u.applySafeEnvironment("llama.cpp", name+"=/removed/after/admission"); err != nil {
			t.Fatalf("%s prevented exact invocation cleanup: %v", name, err)
		}
	}
}
