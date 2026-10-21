package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestComfyScriptRejectsMalformedPathsAndPreservesTrustedInterpreterAlias(t *testing.T) {
	script := comfyScriptFixture(t)
	for _, path := range []string{"relative/main.py", filepath.Dir(script) + "/../ComfyUI/main.py", script + "\x00"} {
		if err := validateComfyScript(path); err == nil {
			t.Fatal(path)
		}
	}
	// The application need not be executable or private; ordinary readable code
	// owned by the desktop principal is supported, with no other writers.
	if err := os.Chmod(script, 0644); err != nil {
		t.Fatal(err)
	}
	if err := validateComfyScript(script); err != nil {
		t.Fatal(err)
	}
	if err := validateComfyExecutable("/usr/bin/python3"); err != nil {
		t.Fatal("trusted python3 interpreter alias rejected", err)
	}
}
