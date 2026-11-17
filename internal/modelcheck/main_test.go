package modelcheck

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain removes any hub login of the machine, so no test depends on it.
// Tests that need a token set it themselves.
func TestMain(m *testing.M) {
	cache, err := os.MkdirTemp("", "modelcheck-cache-")
	if err != nil {
		panic(err)
	}
	if resolved, evalErr := filepath.EvalSymlinks(cache); evalErr == nil {
		cache = resolved
	}
	for _, key := range []string{"HF_TOKEN", "HF_TOKEN_PATH", "HF_HOME"} {
		_ = os.Unsetenv(key)
	}
	_ = os.Setenv("XDG_CACHE_HOME", cache)
	code := m.Run()
	_ = os.RemoveAll(cache)
	os.Exit(code)
}
