package setup

import (
	"os"
	"path/filepath"
	"testing"
)

// testModelRoot holds model folders with a chat template, which owned
// vLLM profiles pin.
var testModelRoot string

func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "setup-models-")
	if err != nil {
		panic(err)
	}
	testModelRoot = root
	code := m.Run()
	_ = os.RemoveAll(root)
	os.Exit(code)
}

// testModelPath returns a model folder with a chat template file.
func testModelPath(name string) string {
	dir := filepath.Join(testModelRoot, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		panic(err)
	}
	template := filepath.Join(dir, ownedChatTemplateName)
	if _, err := os.Stat(template); err != nil {
		if err := os.WriteFile(template, []byte("{{ messages }}"), 0o600); err != nil {
			panic(err)
		}
	}
	return dir
}
